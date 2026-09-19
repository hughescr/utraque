package providerquota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
)

type fakeCredentialSource struct {
	cred        auth.Credential
	invalidated []auth.Credential
}

func (s *fakeCredentialSource) Get(context.Context) (auth.Credential, error) { return s.cred, nil }
func (s *fakeCredentialSource) Invalidate(c auth.Credential) {
	s.invalidated = append(s.invalidated, c)
}

func writeFakeAppServer(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-app-server")
	// /usr/bin/env exercises the production PATH allowlist used by common npm
	// and Homebrew Codex launcher scripts.
	if err := os.WriteFile(path, []byte("#!/usr/bin/env sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexReadExternalTokenProtocol(t *testing.T) {
	script := writeFakeAppServer(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      case "$line" in *'"experimentalApi":true'*) ;; *) exit 31;; esac
      printf '%s\n' '{"method":"noise/notification","params":{}}'
      printf '%s\n' '{"id":1,"result":{"userAgent":"fake","platformFamily":"unix","platformOs":"test"}}'
      ;;
    *'"method":"initialized"'*) ;;
    *'"method":"account/login/start"'*)
      case "$line" in *'"accessToken":"codex-access-secret"'*'"chatgptAccountId":"account-private-id"'*'"type":"chatgptAuthTokens"'*) ;; *) exit 32;; esac
      case "${CODEX_HOME}" in *utraque-codex-app-server-*/home) ;; *) exit 33;; esac
      test ! -f "${CODEX_HOME}/auth.json" || exit 34
      printf '%s\n' '{"method":"account/login/completed","params":{"loginId":null,"success":true,"error":null}}'
      printf '%s\n' '{"id":2,"result":{"type":"chatgptAuthTokens"}}'
      ;;
    *'"method":"account/read"'*)
      printf '%s\n' '{"id":3,"result":{"account":{"type":"chatgpt","email":"private@example.com","planType":"plus"},"requiresOpenaiAuth":true}}'
      ;;
    *'"method":"account/rateLimits/read"'*)
      printf '%s\n' '{"id":4,"result":{"accountId":"account-private-id","rateLimits":{"limitId":"legacy"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":"Codex","planType":"plus","primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1789135200},"secondary":{"usedPercent":40,"windowDurationMins":10080,"resetsAt":1789732800},"credits":{"balance":"12.50","hasCredits":true,"unlimited":false}},"codex_other":{"limitId":"codex_other","primary":{"usedPercent":0,"windowDurationMins":60,"resetsAt":1789135200}}},"rateLimitResetCredits":{"availableCount":2,"credits":[{"id":"opaque-private-credit-id"}]}}}'
      ;;
  esac
done
`)
	tempRoot := t.TempDir()
	client, err := NewCodexClient(CodexOptions{Command: []string{script}, TempRoot: tempRoot, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeCredentialSource{cred: auth.Credential{AccessToken: "codex-access-secret", AccountID: "account-private-id"}}
	got, err := client.ReadCredential(context.Background(), source, source.cred)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.invalidated) != 0 || got.Plan == nil || got.Plan.Type != "plus" || len(got.Quotas) != 3 || got.Quotas[2].UsedPercent != 0 {
		t.Fatalf("observation = %+v, invalidated=%d", got, len(source.invalidated))
	}
	if got.Quotas[0].ID != "codex" || got.Quotas[0].Slot != "primary" || *got.Quotas[0].DurationSeconds != 300*60 {
		t.Errorf("primary = %+v", got.Quotas[0])
	}
	if len(got.Balances) != 1 || got.Balances[0].Total != "12.50" || got.ResetCredits == nil || got.ResetCredits.AvailableCount != 2 {
		t.Errorf("balances/reset credits = %+v / %+v", got.Balances, got.ResetCredits)
	}
	wire, _ := json.Marshal(got)
	for _, forbidden := range []string{"codex-access-secret", "account-private-id", "private@example.com", "opaque-private-credit-id", got.CacheScope()} {
		if strings.Contains(string(wire), forbidden) {
			t.Errorf("JSON leaked %q: %s", forbidden, wire)
		}
	}
	wantScope, _ := CodexCacheScope(source.cred)
	if got.CacheScope() != wantScope {
		t.Error("Codex cache scope mismatch")
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil || len(entries) != 0 {
		t.Errorf("isolated app-server directory not cleaned: entries=%v err=%v", entries, err)
	}
}

func TestCodexRefreshRequestInvalidatesWithoutReturningSecrets(t *testing.T) {
	script := writeFakeAppServer(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{"userAgent":"fake"}}' ;;
    *'"method":"account/login/start"'*) printf '%s\n' '{"id":2,"result":{"type":"chatgptAuthTokens"}}' ;;
    *'"method":"account/read"'*) printf '%s\n' '{"id":3,"result":{"account":{"type":"chatgpt","email":null,"planType":"plus"},"requiresOpenaiAuth":true}}' ;;
    *'"method":"account/rateLimits/read"'*)
      printf '%s\n' '{"method":"account/chatgptAuthTokens/refresh","id":88,"params":{"reason":"unauthorized","previousAccountId":"must-not-leak"}}'
      ;;
  esac
done
`)
	client, _ := NewCodexClient(CodexOptions{Command: []string{script}, Timeout: 2 * time.Second})
	source := &fakeCredentialSource{cred: auth.Credential{AccessToken: "must-not-leak-token", AccountID: "must-not-leak-account"}}
	_, err := client.Read(context.Background(), source)
	var qerr *Error
	if !errors.As(err, &qerr) || qerr.Code != CodeCredential || !qerr.Retryable || len(source.invalidated) != 1 {
		t.Fatalf("err=%v invalidated=%d", err, len(source.invalidated))
	}
	for _, secret := range []string{source.cred.AccessToken, source.cred.AccountID, "must-not-leak"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked %q: %v", secret, err)
		}
	}
}

func TestCodexAccountMismatchAndTimeoutAreSanitized(t *testing.T) {
	for _, tt := range []struct {
		name    string
		body    string
		code    ErrorCode
		timeout time.Duration
	}{
		{"mismatch", `
while IFS= read -r line; do
 case "$line" in
 *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}';;
 *'"method":"account/login/start"'*) printf '%s\n' '{"id":2,"result":{"type":"chatgptAuthTokens"}}';;
 *'"method":"account/read"'*) printf '%s\n' '{"id":3,"result":{"account":{"type":"chatgpt","email":null,"planType":"unknown"},"requiresOpenaiAuth":true}}';;
 *'"method":"account/rateLimits/read"'*) printf '%s\n' '{"id":4,"result":{"accountId":"other-private-account","rateLimits":{}}}';;
 esac
done`, CodeCredential, 2 * time.Second},
		{"timeout", `while IFS= read -r line; do :; done`, CodeTimeout, 100 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			script := writeFakeAppServer(t, tt.body)
			client, _ := NewCodexClient(CodexOptions{Command: []string{script}, Timeout: tt.timeout})
			source := &fakeCredentialSource{cred: auth.Credential{AccessToken: "secret-token", AccountID: "secret-account"}}
			_, err := client.Read(context.Background(), source)
			var qerr *Error
			if !errors.As(err, &qerr) || qerr.Code != tt.code {
				t.Fatalf("error = %v, want %s", err, tt.code)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "other-private") {
				t.Errorf("error leaked data: %v", err)
			}
		})
	}
}

func TestCodexRateLimitPresenceAndStableOrder(t *testing.T) {
	var observation Observation
	err := normalizeCodexLimits(&observation, codexRateLimitsResponse{}, fixedNow)
	var qerr *Error
	if !errors.As(err, &qerr) || qerr.Code != CodeProtocol {
		t.Fatalf("missing rate limits error = %v", err)
	}

	empty := map[string]codexSnapshot{}
	if err := normalizeCodexLimits(&observation, codexRateLimitsResponse{RateLimitsByID: &empty}, fixedNow); err != nil {
		t.Fatalf("explicit empty multi-bucket response: %v", err)
	}
	legacy := codexSnapshot{
		Primary:   &codexWindow{UsedPercent: floatPtr(1)},
		Secondary: &codexWindow{UsedPercent: floatPtr(2)},
	}
	observation = Observation{}
	if err := normalizeCodexLimits(&observation, codexRateLimitsResponse{RateLimits: &legacy}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if len(observation.Quotas) != 2 || observation.Quotas[0].Slot != "primary" || observation.Quotas[1].Slot != "secondary" {
		t.Fatalf("quota order = %+v", observation.Quotas)
	}
}

func TestCodexSpendControlReachedPreservesTrueFalseAndMissing(t *testing.T) {
	reached, clear := true, false
	buckets := map[string]codexSnapshot{
		"a_reached": {SpendControlReached: &reached},
		"b_clear":   {SpendControlReached: &clear},
		"c_missing": {},
	}
	var observation Observation
	if err := normalizeCodexLimits(&observation, codexRateLimitsResponse{RateLimitsByID: &buckets}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if len(observation.Quotas) != 0 {
		t.Fatalf("unexpected synthesized quotas: %+v", observation.Quotas)
	}
	if len(observation.SpendControls) != 2 {
		t.Fatalf("spend controls = %+v", observation.SpendControls)
	}
	if got := observation.SpendControls[0]; got.LimitID != "a_reached" || !got.Reached {
		t.Errorf("true flag = %+v", got)
	}
	if got := observation.SpendControls[1]; got.LimitID != "b_clear" || got.Reached {
		t.Errorf("false flag = %+v", got)
	}
}

func floatPtr(v float64) *float64 { return &v }

func TestCodexSubprocessOutputBounds(t *testing.T) {
	tests := []struct {
		name string
		body string
		opts CodexOptions
		code ErrorCode
	}{
		{
			name: "stdout line",
			body: `while IFS= read -r line; do printf '%s\n' 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done`,
			opts: CodexOptions{MaxLineBytes: 32, MaxOutputBytes: 1024},
			code: CodeTooLarge,
		},
		{
			name: "stderr stream",
			body: `printf '%s' 'stderr-private-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' >&2
while IFS= read -r line; do :; done`,
			opts: CodexOptions{MaxStderrBytes: 16, Timeout: 2 * time.Second},
			code: CodeTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Command = []string{writeFakeAppServer(t, tt.body)}
			client, err := NewCodexClient(tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			source := &fakeCredentialSource{cred: auth.Credential{AccessToken: "token", AccountID: "account"}}
			started := time.Now()
			_, err = client.Read(context.Background(), source)
			var qerr *Error
			if !errors.As(err, &qerr) || qerr.Code != tt.code {
				t.Fatalf("error = %v, want %s", err, tt.code)
			}
			if time.Since(started) > time.Second {
				t.Fatalf("output-bound shutdown took %s", time.Since(started))
			}
			if strings.Contains(err.Error(), "stderr-private") {
				t.Fatalf("stderr leaked in error: %v", err)
			}
		})
	}
}
