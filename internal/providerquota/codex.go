package providerquota

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
)

const (
	defaultCodexTimeout   = 15 * time.Second
	defaultCodexMaxLine   = 1 << 20
	defaultCodexMaxOutput = 4 << 20
	defaultCodexMaxStderr = 64 << 10
)

type CodexOptions struct {
	// Command defaults to: codex app-server --listen stdio://. Tests may
	// provide a hermetic fake executable and arguments.
	Command        []string
	Timeout        time.Duration
	MaxLineBytes   int
	MaxOutputBytes int64
	MaxStderrBytes int64
	TempRoot       string
	Now            func() time.Time
}

type CodexClient struct {
	command   []string
	timeout   time.Duration
	maxLine   int
	maxOutput int64
	maxStderr int64
	tempRoot  string
	now       func() time.Time
}

func NewCodexClient(opts CodexOptions) (*CodexClient, error) {
	if len(opts.Command) == 0 {
		opts.Command = []string{"codex", "app-server", "--listen", "stdio://"}
	}
	if strings.TrimSpace(opts.Command[0]) == "" {
		return nil, quotaError(ProviderCodex, CodeConfiguration, false)
	}
	for _, arg := range opts.Command[1:] {
		if strings.IndexByte(arg, 0) >= 0 {
			return nil, quotaError(ProviderCodex, CodeConfiguration, false)
		}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultCodexTimeout
	}
	if opts.MaxLineBytes <= 0 {
		opts.MaxLineBytes = defaultCodexMaxLine
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = defaultCodexMaxOutput
	}
	if opts.MaxStderrBytes <= 0 {
		opts.MaxStderrBytes = defaultCodexMaxStderr
	}
	if opts.MaxLineBytes > 16<<20 || opts.MaxOutputBytes > 64<<20 || opts.MaxStderrBytes > 16<<20 {
		return nil, quotaError(ProviderCodex, CodeConfiguration, false)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &CodexClient{
		command: append([]string(nil), opts.Command...), timeout: opts.Timeout,
		maxLine: opts.MaxLineBytes, maxOutput: opts.MaxOutputBytes,
		maxStderr: opts.MaxStderrBytes, tempRoot: opts.TempRoot, now: opts.Now,
	}, nil
}

// CodexCacheScope derives an account-safe, non-secret discriminator without
// contacting app-server. It intentionally keys on the account rather than the
// rotating access token.
func CodexCacheScope(cred auth.Credential) (string, error) {
	if strings.TrimSpace(cred.AccountID) == "" {
		return "", quotaError(ProviderCodex, CodeCredential, false)
	}
	return credentialScope(ProviderCodex, cred.AccountID)
}

// Read resolves exactly one credential snapshot from src and uses that same
// access-token/account pair for the entire app-server exchange.
func (c *CodexClient) Read(ctx context.Context, src auth.CredentialSource) (Observation, error) {
	if c == nil || src == nil {
		return Observation{}, quotaError(ProviderCodex, CodeConfiguration, false)
	}
	cred, err := src.Get(ctx)
	if err != nil || strings.TrimSpace(cred.AccessToken) == "" || strings.TrimSpace(cred.AccountID) == "" || len(cred.AccessToken) > 256<<10 || len(cred.AccountID) > 4096 {
		return Observation{}, quotaError(ProviderCodex, CodeCredential, true)
	}
	return c.ReadCredential(ctx, src, cred)
}

// ReadCredential performs a read with a credential snapshot already resolved
// by the caller. This lets an integrator derive CodexCacheScope before checking
// a cache, then use the exact same account/token pair on a cache miss. src is
// retained solely so a server refresh request can invalidate that snapshot.
func (c *CodexClient) ReadCredential(parent context.Context, src auth.CredentialSource, cred auth.Credential) (Observation, error) {
	if c == nil || src == nil || strings.TrimSpace(cred.AccessToken) == "" || strings.TrimSpace(cred.AccountID) == "" || len(cred.AccessToken) > 256<<10 || len(cred.AccountID) > 4096 {
		return Observation{}, quotaError(ProviderCodex, CodeCredential, false)
	}
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()

	root, err := os.MkdirTemp(c.tempRoot, "utraque-codex-app-server-")
	if err != nil {
		return Observation{}, quotaError(ProviderCodex, CodeUnavailable, true)
	}
	defer os.RemoveAll(root)
	for _, dir := range []string{"home", "tmp", "work"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			return Observation{}, quotaError(ProviderCodex, CodeUnavailable, true)
		}
	}

	executable, err := exec.LookPath(c.command[0])
	if err != nil {
		return Observation{}, quotaError(ProviderCodex, CodeUnavailable, false)
	}
	cmd := exec.Command(executable, c.command[1:]...)
	cmd.Dir = filepath.Join(root, "work")
	cmd.Env = []string{
		"CODEX_HOME=" + filepath.Join(root, "home"),
		"HOME=" + filepath.Join(root, "home"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"LANG=C", "LC_ALL=C", "NO_COLOR=1", "RUST_BACKTRACE=0",
	}
	if path := os.Getenv("PATH"); path != "" {
		cmd.Env = append(cmd.Env, "PATH="+path)
	}
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Observation{}, quotaError(ProviderCodex, CodeUnavailable, true)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Observation{}, quotaError(ProviderCodex, CodeUnavailable, true)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Observation{}, quotaError(ProviderCodex, CodeUnavailable, true)
	}
	if err := cmd.Start(); err != nil {
		return Observation{}, quotaError(ProviderCodex, CodeUnavailable, true)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	stderrDone := make(chan struct{})
	stderrSink := &boundedDiscard{max: c.maxStderr, overflow: cancel}
	go func() {
		_, _ = io.Copy(stderrSink, stderr)
		close(stderrDone)
	}()
	lines := startJSONLines(ctx, stdout, c.maxLine, c.maxOutput)
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = stdin.Close()
		killProcessGroup(cmd)
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-waitCh
		}
		<-stderrDone
	}
	defer stop()

	rpc := rpcSession{ctx: ctx, stdin: stdin, lines: lines}
	if err := rpc.call(1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "utraque", "title": "utraque", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil); err != nil {
		return Observation{}, err
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return Observation{}, err
	}
	var login struct {
		Type string `json:"type"`
	}
	if err := rpc.call(2, "account/login/start", map[string]any{
		"type": "chatgptAuthTokens", "accessToken": cred.AccessToken, "chatgptAccountId": cred.AccountID,
	}, &login); err != nil {
		if errors.Is(err, errRefreshRequested) {
			src.Invalidate(cred)
			return Observation{}, quotaError(ProviderCodex, CodeCredential, true)
		}
		return Observation{}, err
	}
	if login.Type != "chatgptAuthTokens" {
		return Observation{}, quotaError(ProviderCodex, CodeProtocol, false)
	}
	var account codexAccountResponse
	if err := rpc.call(3, "account/read", map[string]any{"refreshToken": false}, &account); err != nil {
		if errors.Is(err, errRefreshRequested) {
			src.Invalidate(cred)
			return Observation{}, quotaError(ProviderCodex, CodeCredential, true)
		}
		return Observation{}, err
	}
	if account.Account == nil || account.Account.Type != "chatgpt" {
		return Observation{}, quotaError(ProviderCodex, CodeProtocol, false)
	}
	var limits codexRateLimitsResponse
	if err := rpc.call(4, "account/rateLimits/read", nil, &limits); err != nil {
		if errors.Is(err, errRefreshRequested) {
			src.Invalidate(cred)
			return Observation{}, quotaError(ProviderCodex, CodeCredential, true)
		}
		return Observation{}, err
	}
	if limits.AccountID != nil && *limits.AccountID != cred.AccountID {
		return Observation{}, quotaError(ProviderCodex, CodeCredential, true)
	}
	cacheScope, err := CodexCacheScope(cred)
	if err != nil {
		return Observation{}, err
	}
	o := Observation{Source: ProviderCodex, CollectedAt: c.now().UTC(), cacheScope: cacheScope}
	if knownPlan(account.Account.PlanType) {
		o.Plan = &PlanInfo{Type: account.Account.PlanType}
	}
	if err := normalizeCodexLimits(&o, limits, c.now()); err != nil {
		return Observation{}, err
	}
	return o, nil
}

type codexAccountResponse struct {
	Account *struct {
		Type     string `json:"type"`
		PlanType string `json:"planType"`
	} `json:"account"`
}

type codexWindow struct {
	UsedPercent       *float64 `json:"usedPercent"`
	WindowDurationMin *int64   `json:"windowDurationMins"`
	ResetsAt          *int64   `json:"resetsAt"`
}

type codexCredits struct {
	Balance    *string `json:"balance"`
	HasCredits *bool   `json:"hasCredits"`
	Unlimited  *bool   `json:"unlimited"`
}

type codexSpend struct {
	Limit            string   `json:"limit"`
	Used             string   `json:"used"`
	RemainingPercent *float64 `json:"remainingPercent"`
	ResetsAt         *int64   `json:"resetsAt"`
}

type codexSnapshot struct {
	LimitID             *string       `json:"limitId"`
	LimitName           *string       `json:"limitName"`
	PlanType            *string       `json:"planType"`
	Primary             *codexWindow  `json:"primary"`
	Secondary           *codexWindow  `json:"secondary"`
	Credits             *codexCredits `json:"credits"`
	IndividualLimit     *codexSpend   `json:"individualLimit"`
	ReachedType         *string       `json:"rateLimitReachedType"`
	SpendControlReached *bool         `json:"spendControlReached"`
}

type codexRateLimitsResponse struct {
	AccountID      *string                   `json:"accountId"`
	RateLimits     *codexSnapshot            `json:"rateLimits"`
	RateLimitsByID *map[string]codexSnapshot `json:"rateLimitsByLimitId"`
	RateLimitReset *struct {
		AvailableCount *int64 `json:"availableCount"`
	} `json:"rateLimitResetCredits"`
}

func normalizeCodexLimits(o *Observation, response codexRateLimitsResponse, now time.Time) error {
	items := make([]struct {
		key string
		s   codexSnapshot
	}, 0)
	if response.RateLimitsByID != nil {
		keys := make([]string, 0, len(*response.RateLimitsByID))
		for key := range *response.RateLimitsByID {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			items = append(items, struct {
				key string
				s   codexSnapshot
			}{key, (*response.RateLimitsByID)[key]})
		}
	} else if response.RateLimits != nil {
		items = append(items, struct {
			key string
			s   codexSnapshot
		}{"codex", *response.RateLimits})
	} else {
		return quotaError(ProviderCodex, CodeProtocol, false)
	}
	for _, item := range items {
		id := item.key
		if item.s.LimitID != nil && *item.s.LimitID != "" {
			id = *item.s.LimitID
		}
		if !validNonemptyLabel(id) || item.s.LimitName != nil && !validLabel(*item.s.LimitName) || item.s.ReachedType != nil && !validLabel(*item.s.ReachedType) {
			return quotaError(ProviderCodex, CodeInvalidData, false)
		}
		for _, windowItem := range []struct {
			slot   string
			window *codexWindow
		}{{"primary", item.s.Primary}, {"secondary", item.s.Secondary}} {
			slot, window := windowItem.slot, windowItem.window
			if window == nil {
				continue
			}
			if window.UsedPercent == nil || !validPercent(*window.UsedPercent) {
				return quotaError(ProviderCodex, CodeInvalidData, false)
			}
			duration, err := durationSeconds(ProviderCodex, window.WindowDurationMin)
			if err != nil {
				return err
			}
			reset, err := parseUnixReset(ProviderCodex, window.ResetsAt, now)
			if err != nil {
				return err
			}
			q := Quota{ID: id, Slot: slot, UsedPercent: *window.UsedPercent, Unit: PercentUnit, DurationSeconds: duration, ResetsAt: reset}
			if item.s.LimitName != nil {
				q.Name = *item.s.LimitName
			}
			if item.s.PlanType != nil && knownPlan(*item.s.PlanType) {
				q.Plan = &PlanInfo{Type: *item.s.PlanType}
			}
			if item.s.ReachedType != nil {
				q.ReachedType = *item.s.ReachedType
			}
			o.Quotas = append(o.Quotas, q)
		}
		if item.s.Credits != nil {
			if item.s.Credits.HasCredits == nil || item.s.Credits.Unlimited == nil {
				return quotaError(ProviderCodex, CodeInvalidData, false)
			}
			b := Balance{Kind: "workspace_credits", ScopeID: id, AmountUnit: "credits", Available: item.s.Credits.HasCredits, Unlimited: item.s.Credits.Unlimited}
			if item.s.Credits.Balance != nil {
				if !validDecimal(*item.s.Credits.Balance) {
					return quotaError(ProviderCodex, CodeInvalidData, false)
				}
				b.Total = *item.s.Credits.Balance
			}
			o.Balances = append(o.Balances, b)
		}
		if item.s.IndividualLimit != nil {
			s := item.s.IndividualLimit
			if !validDecimal(s.Limit) || !validDecimal(s.Used) || s.RemainingPercent == nil || !validPercent(*s.RemainingPercent) || s.ResetsAt == nil {
				return quotaError(ProviderCodex, CodeInvalidData, false)
			}
			reset, err := parseUnixReset(ProviderCodex, s.ResetsAt, now)
			if err != nil {
				return err
			}
			used := 100 - *s.RemainingPercent
			o.Quotas = append(o.Quotas, Quota{ID: id + ":spend_control", Kind: "spend_control", UsedPercent: used, Unit: PercentUnit, ResetsAt: reset})
			o.Balances = append(o.Balances, Balance{Kind: "spend_control", ScopeID: id, AmountUnit: "provider_units", Total: s.Limit, Components: []BalanceComponent{{Name: "used", Amount: s.Used}}})
		}
	}
	if response.RateLimitReset != nil {
		if response.RateLimitReset.AvailableCount == nil || *response.RateLimitReset.AvailableCount < 0 || *response.RateLimitReset.AvailableCount > 1_000_000 {
			return quotaError(ProviderCodex, CodeInvalidData, false)
		}
		o.ResetCredits = &ResetCredits{AvailableCount: *response.RateLimitReset.AvailableCount}
	}
	return nil
}

func knownPlan(s string) bool {
	switch s {
	case "free", "go", "plus", "pro", "prolite", "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise", "edu", "edu_plus", "edu_pro":
		return true
	default:
		return false
	}
}

var errRefreshRequested = errors.New("codex token refresh requested")

type rpcSession struct {
	ctx   context.Context
	stdin io.Writer
	lines <-chan jsonLine
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (r rpcSession) notify(method string, params any) error {
	return r.write(map[string]any{"method": method, "params": params})
}

func (r rpcSession) call(id int, method string, params any, result any) error {
	request := map[string]any{"method": method, "id": id}
	if params != nil {
		request["params"] = params
	}
	if err := r.write(request); err != nil {
		return err
	}
	for {
		select {
		case <-r.ctx.Done():
			return quotaError(ProviderCodex, CodeTimeout, true)
		case line, ok := <-r.lines:
			if !ok || line.err != nil {
				if line.err != nil {
					return line.err
				}
				return quotaError(ProviderCodex, CodeProtocol, true)
			}
			var envelope rpcEnvelope
			if json.Unmarshal(line.data, &envelope) != nil {
				return quotaError(ProviderCodex, CodeProtocol, false)
			}
			if envelope.Method != "" {
				if envelope.Method == "account/chatgptAuthTokens/refresh" && len(envelope.ID) > 0 {
					return errRefreshRequested
				}
				if len(envelope.ID) > 0 {
					return quotaError(ProviderCodex, CodeProtocol, false)
				}
				continue
			}
			var gotID int
			if json.Unmarshal(envelope.ID, &gotID) != nil || gotID != id {
				continue
			}
			if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
				return quotaError(ProviderCodex, CodeUnavailable, true)
			}
			if len(envelope.Result) == 0 {
				return quotaError(ProviderCodex, CodeProtocol, false)
			}
			if result != nil && json.Unmarshal(envelope.Result, result) != nil {
				return quotaError(ProviderCodex, CodeProtocol, false)
			}
			return nil
		}
	}
}

func (r rpcSession) write(value any) error {
	done := make(chan error, 1)
	go func() { done <- json.NewEncoder(r.stdin).Encode(value) }()
	select {
	case <-r.ctx.Done():
		return quotaError(ProviderCodex, CodeTimeout, true)
	case err := <-done:
		if err != nil {
			return quotaError(ProviderCodex, CodeUnavailable, true)
		}
		return nil
	}
}

type jsonLine struct {
	data []byte
	err  error
}

func startJSONLines(ctx context.Context, r io.Reader, maxLine int, maxOutput int64) <-chan jsonLine {
	ch := make(chan jsonLine)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 4096), maxLine)
		var total int64
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			total += int64(len(line)) + 1
			if total > maxOutput {
				select {
				case ch <- jsonLine{err: quotaError(ProviderCodex, CodeTooLarge, false)}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case ch <- jsonLine{data: line}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case ch <- jsonLine{err: quotaError(ProviderCodex, CodeTooLarge, false)}:
			case <-ctx.Done():
			}
		}
	}()
	return ch
}

type boundedDiscard struct {
	mu       sync.Mutex
	once     sync.Once
	max      int64
	count    int64
	overflow func()
}

func (w *boundedDiscard) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.count += int64(len(p))
	over := w.count > w.max
	if over {
		w.count = w.max + 1
	}
	w.mu.Unlock()
	if over && w.overflow != nil {
		w.once.Do(w.overflow)
	}
	return len(p), nil
}
