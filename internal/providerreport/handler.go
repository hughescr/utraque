package providerreport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/leg"
	"github.com/hughescr/utraque/internal/providerquota"
)

const (
	defaultTTL      = 30 * time.Second
	defaultTimeout  = 90 * time.Second
	maxCacheEntries = 16
)

type Options struct {
	History              HistoryCollector
	Anthropic            AnthropicReader
	DeepSeek             DeepSeekReader
	DeepSeekAPIKey       string
	Codex                CodexReader
	CodexSource          auth.CredentialSource
	ReferencePrices      ReferencePriceReader
	EligiblePriceModels  func(id leg.ID) []string
	NormalizePriceModel  func(id leg.ID, model string) string
	CacheTTL             time.Duration
	Timeout              time.Duration
	ClaudePlan           string
	ClaudePlanMultiplier *float64
	Now                  func() time.Time
}

// providerMemory is the cross-attempt memory schema 2 serves as last_attempt
// and last_success. It lives with the cache entry, so it is scoped like the
// entry (per credential set, history range and plan) and is forgotten when
// the entry is evicted.
type providerMemory struct {
	// lastAttemptAt is the end of the most recent collection attempt.
	lastAttemptAt time.Time
	// lastSuccessAt is the end of the most recent attempt whose Status was
	// not StatusError; a failed attempt leaves it as it was.
	lastSuccessAt time.Time
}

type cacheEntry struct {
	report  Report
	created time.Time
	memory  map[leg.ID]providerMemory
}

// served is what the cache hands ServeHTTP: the schema-1 document plus the
// memory the schema-2 projection needs.
type served struct {
	report Report
	memory map[leg.ID]providerMemory
}

type Handler struct {
	history        HistoryCollector
	anthropic      AnthropicReader
	deepseek       DeepSeekReader
	deepseekKey    string
	codex          CodexReader
	codexSource    auth.CredentialSource
	prices         ReferencePriceReader
	eligiblePrices func(id leg.ID) []string
	normalizePrice func(id leg.ID, model string) string
	ttl, timeout   time.Duration
	planLabel      string
	planMultiplier *float64
	now            func() time.Time
	mu             sync.Mutex
	cache          map[string]*cacheEntry
	group          singleflight.Group
	sem            chan struct{}
}

func New(opts Options) *Handler {
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = defaultTTL
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Handler{history: opts.History,
		anthropic: opts.Anthropic, deepseek: opts.DeepSeek, deepseekKey: opts.DeepSeekAPIKey,
		codex: opts.Codex, codexSource: opts.CodexSource, prices: opts.ReferencePrices,
		eligiblePrices: opts.EligiblePriceModels, normalizePrice: opts.NormalizePriceModel,
		ttl: opts.CacheTTL, timeout: opts.Timeout,
		planLabel: opts.ClaudePlan, planMultiplier: opts.ClaudePlanMultiplier, now: opts.Now,
		cache: make(map[string]*cacheEntry), sem: make(chan struct{}, 4)}
}

// ServeHTTP serves schema 1 (Report), the document the legacy path and
// /utraque/providers/v1 return. V2 serves schema 2 from the same collection.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, func(s served) any { return s.report })
}

// V2 returns the handler for schema 2 (ReportV2). It shares ServeHTTP's
// cache, coalescing and access checks; only the rendering differs.
func (h *Handler) V2() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, func(s served) any { return renderV2(s.report, s.memory) })
	})
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, render func(served) any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"type": "method_not_allowed", "message": "only GET and HEAD are supported"}}, r.Method == http.MethodHead)
		return
	}
	if !isLoopback(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"type": "permission_error", "message": "provider reporting is available only to loopback clients"}}, r.Method == http.MethodHead)
		return
	}
	if h.history == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "configuration_error", "message": "provider reporting is not configured"}}, r.Method == http.MethodHead)
		return
	}
	token := bearer(r.Header.Get("Authorization"))
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	creds := credentials{anthropicToken: token}
	if h.codexSource != nil {
		cred, err := h.codexSource.Get(ctx)
		if err == nil {
			creds.codex = cred
		}
	}
	now := h.now().UTC()
	since := utcDate(now).AddDate(0, 0, -29)
	until := utcDate(now)
	key := h.scopeKey(creds, since, until)
	if rep, ok := h.cached(key, now, false); ok {
		writeJSON(w, http.StatusOK, render(rep), r.Method == http.MethodHead)
		return
	}

	ch := h.group.DoChan(key, func() (any, error) {
		if rep, ok := h.cached(key, h.now().UTC(), false); ok {
			return rep, nil
		}
		work, workCancel := context.WithTimeout(context.Background(), h.timeout)
		defer workCancel()
		select {
		case h.sem <- struct{}{}:
			defer func() { <-h.sem }()
		case <-work.Done():
			return served{}, work.Err()
		}
		attempt := h.collect(work, creds, since, until)
		if h.codexSource != nil && creds.codex.AccountID != "" {
			if current, err := h.codexSource.Get(work); err == nil {
				oldScope, oldErr := providerquota.CodexCacheScope(creds.codex)
				newScope, newErr := providerquota.CodexCacheScope(current)
				if oldErr != nil || newErr != nil {
					markCodexScopeUnverified(&attempt)
				} else if oldScope != newScope {
					markCodexScopeChanged(&attempt)
				}
			} else {
				markCodexScopeUnverified(&attempt)
			}
		}
		return h.storeAttempt(key, attempt), nil
	})
	select {
	case <-ctx.Done():
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": map[string]string{"type": "timeout", "message": "provider report collection timed out"}}, r.Method == http.MethodHead)
	case res := <-ch:
		if res.Err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "unavailable", "message": "provider report unavailable"}}, r.Method == http.MethodHead)
			return
		}
		writeJSON(w, http.StatusOK, render(res.Val.(served)), r.Method == http.MethodHead)
	}
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func markCodexScopeChanged(r *Report) {
	markCodexUnavailable(r, CodeAccountScopeChanged, "Codex account scope changed during collection")
}

func markCodexScopeUnverified(r *Report) {
	markCodexUnavailable(r, CodeAccountScopeUnverified, "Codex account scope could not be verified after collection")
}

func markCodexUnavailable(r *Report, code ErrorCode, message string) {
	for i := range r.Providers {
		p := &r.Providers[i]
		if p.Provider != leg.Codex {
			continue
		}
		p.QuotaBefore, p.Quota, p.Paired = nil, nil, nil
		p.LastComplete = nil
		p.discardPrevious = true
		p.Errors = append(p.Errors, ReportError{Section: SectionQuota, Code: code, Retryable: true, Message: message})
		if p.History != nil {
			p.Status = StatusPartial
		} else {
			p.Status = StatusError
			p.LastSuccess = nil
		}
	}
}

func (h *Handler) scopeKey(creds credentials, since, until time.Time) string {
	parts := []string{"v1", h.planLabel}
	if h.planMultiplier != nil {
		parts = append(parts, strconv.FormatFloat(*h.planMultiplier, 'g', -1, 64))
	}
	if s, err := providerquota.AnthropicCacheScope(creds.anthropicToken); err == nil {
		parts = append(parts, s)
	} else {
		parts = append(parts, "anthropic:none")
	}
	if s, err := providerquota.DeepSeekCacheScope(h.deepseekKey); err == nil {
		parts = append(parts, s)
	} else {
		parts = append(parts, "deepseek:none")
	}
	if s, err := providerquota.CodexCacheScope(creds.codex); err == nil {
		parts = append(parts, s)
	} else {
		parts = append(parts, "codex:none")
	}
	parts = append(parts, since.Format("2006-01-02"), until.Format("2006-01-02"), h.ttl.String(), h.timeout.String())
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) cached(key string, now time.Time, allowExpired bool) (served, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.cache[key]
	if e == nil {
		return served{}, false
	}
	age := now.Sub(e.created)
	if age < 0 {
		age = 0
	}
	if !allowExpired && age >= h.ttl {
		return served{}, false
	}
	r := cloneReport(e.report)
	r.GeneratedAt = now
	markFreshness(&r, true, age >= h.ttl, age)
	return served{report: r, memory: e.memory}, true
}

// storeAttempt records a finished collection under key, carrying forward
// from the previous entry the last complete snapshot (schema 1's
// last_complete_snapshot, for providers this attempt did not fully succeed
// on) and the per-provider memory (schema 2's last_attempt / last_success),
// and returns the attempt as it is to be served.
func (h *Handler) storeAttempt(key string, attempt Report) served {
	now := h.now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.cache[key]
	memory := make(map[leg.ID]providerMemory, len(attempt.Providers))
	for _, p := range attempt.Providers {
		var mem providerMemory
		if old != nil {
			mem = old.memory[p.Provider]
		}
		mem.lastAttemptAt = attempt.CollectionEndedAt
		if p.Status != StatusError {
			mem.lastSuccessAt = attempt.CollectionEndedAt
		}
		memory[p.Provider] = mem
	}
	if old != nil {
		for i := range attempt.Providers {
			if attempt.Providers[i].Status == StatusOK || attempt.Providers[i].discardPrevious {
				continue
			}
			for _, previous := range old.report.Providers {
				if previous.Provider != attempt.Providers[i].Provider || (previous.Status != StatusOK && previous.LastComplete == nil) {
					continue
				}
				if previous.LastComplete != nil {
					attempt.Providers[i].LastComplete = previous.LastComplete
					continue
				}
				age := now.Sub(old.created)
				if age < 0 {
					age = 0
				}
				attempt.Providers[i].LastComplete = &ProviderSnapshot{
					LastSuccess: previous.LastSuccess,
					Freshness:   Freshness{Cached: true, Stale: true, AgeSeconds: age.Seconds()},
					QuotaBefore: previous.QuotaBefore, Quota: previous.Quota,
					Paired: previous.Paired, History: previous.History,
					Calibration: &Calibration{UnavailableReason: ReasonCachedMeasurementExpired},
				}
			}
		}
	}
	if len(h.cache) >= maxCacheEntries {
		var oldestKey string
		var oldest time.Time
		for k, e := range h.cache {
			if oldestKey == "" || e.created.Before(oldest) {
				oldestKey, oldest = k, e.created
			}
		}
		delete(h.cache, oldestKey)
	}
	h.cache[key] = &cacheEntry{report: cloneReport(attempt), created: now, memory: memory}
	markFreshness(&attempt, false, false, 0)
	return served{report: attempt, memory: memory}
}

func markFreshness(r *Report, cached, stale bool, age time.Duration) {
	if age < 0 {
		age = 0
	}
	for i := range r.Providers {
		providerStale := stale || r.Providers[i].SourceFreshness.Stale
		providerAge := age + time.Duration(r.Providers[i].SourceFreshness.AgeSeconds*float64(time.Second))
		r.Providers[i].SourceFreshness = Freshness{Cached: cached || r.Providers[i].SourceFreshness.Cached, Stale: providerStale, AgeSeconds: providerAge.Seconds()}
		if providerStale {
			r.Providers[i].Calibration = &Calibration{UnavailableReason: ReasonCachedMeasurementExpired}
			r.Providers[i].Remaining = nil
		}
		if previous := r.Providers[i].LastComplete; previous != nil {
			previous.Freshness.Cached = true
			previous.Freshness.Stale = true
			previous.Freshness.AgeSeconds = snapshotAge(previous, r.GeneratedAt).Seconds()
			previous.Calibration = &Calibration{UnavailableReason: ReasonCachedMeasurementExpired}
			previous.Remaining = nil
		}
		if observationResetPassed(r.Providers[i].Quota, r.GeneratedAt) {
			r.Providers[i].SourceFreshness.Stale = true
			r.Providers[i].Calibration = &Calibration{UnavailableReason: ReasonQuotaWindowReset}
			r.Providers[i].Remaining = nil
		}
	}
}

func snapshotAge(s *ProviderSnapshot, now time.Time) time.Duration {
	measured := time.Time{}
	if s.LastSuccess != nil {
		measured = *s.LastSuccess
	}
	if measured.IsZero() && s.Paired != nil {
		measured = s.Paired.EndedAt
	}
	if measured.IsZero() && s.History != nil {
		measured = s.History.FinishedAt
	}
	age := now.Sub(measured)
	if measured.IsZero() || age < 0 {
		return 0
	}
	return age
}

func observationResetPassed(obs *providerquota.Observation, now time.Time) bool {
	if obs == nil {
		return false
	}
	for _, q := range obs.Quotas {
		if (q.Active == nil || *q.Active) && q.ResetsAt != nil && !q.ResetsAt.After(now) {
			return true
		}
	}
	return false
}

// cloneReport deep-copies a report so the cache and a response never share
// memory. The copy goes through the schema-1 JSON form, which drops the
// observation fields that form does not carry (Quota.Bucket, SpendLimits);
// the schema-2 projection needs them, so each observation is cloned by
// providerquota instead.
func cloneReport(in Report) Report {
	b, _ := json.Marshal(in)
	var out Report
	_ = json.Unmarshal(b, &out)
	for i := range in.Providers {
		out.Providers[i].Quota = cloneObservation(in.Providers[i].Quota)
		if s := in.Providers[i].LastComplete; s != nil {
			out.Providers[i].LastComplete.Quota = cloneObservation(s.Quota)
		}
	}
	return out
}

func cloneObservation(o *providerquota.Observation) *providerquota.Observation {
	if o == nil {
		return nil
	}
	c := o.Clone()
	return &c
}

func bearer(v string) string {
	p := strings.Fields(v)
	if len(p) == 2 && strings.EqualFold(p[0], "Bearer") {
		return p[1]
	}
	return ""
}
func utcDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
func writeJSON(w http.ResponseWriter, status int, v any, head bool) {
	w.WriteHeader(status)
	if head {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
