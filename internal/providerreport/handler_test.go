package providerreport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/referenceprice"
	"github.com/hughescr/utraque/internal/usagehistory"
)

type historyFunc func(context.Context, time.Time, time.Time) (usagehistory.Report, error)

func (f historyFunc) Collect(c context.Context, s, u time.Time) (usagehistory.Report, error) {
	return f(c, s, u)
}

type anthropicFunc func(context.Context, string) (providerquota.Observation, error)

func (f anthropicFunc) Read(c context.Context, s string) (providerquota.Observation, error) {
	return f(c, s)
}

type deepSeekFunc func(context.Context) (providerquota.Observation, error)

func (f deepSeekFunc) Read(c context.Context) (providerquota.Observation, error) { return f(c) }

type codexFunc func(context.Context, auth.CredentialSource, auth.Credential) (providerquota.Observation, error)

func (f codexFunc) ReadCredential(c context.Context, s auth.CredentialSource, cr auth.Credential) (providerquota.Observation, error) {
	return f(c, s, cr)
}

type priceFunc func(context.Context) (referenceprice.Snapshot, error)

func (f priceFunc) Read(c context.Context) (referenceprice.Snapshot, error) { return f(c) }

type sourceFunc func(context.Context) (auth.Credential, error)

func (f sourceFunc) Get(c context.Context) (auth.Credential, error) { return f(c) }
func (sourceFunc) Invalidate(auth.Credential)                       {}

type switchingSource struct {
	mu   sync.Mutex
	cred auth.Credential
}

func (s *switchingSource) Get(context.Context) (auth.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred, nil
}
func (*switchingSource) Invalidate(auth.Credential) {}
func (s *switchingSource) set(cred auth.Credential) { s.mu.Lock(); s.cred = cred; s.mu.Unlock() }

type sequenceSource struct {
	mu    sync.Mutex
	creds []auth.Credential
	next  int
}

func (s *sequenceSource) Get(context.Context) (auth.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.creds) {
		return s.creds[len(s.creds)-1], nil
	}
	cred := s.creds[s.next]
	s.next++
	return cred, nil
}
func (*sequenceSource) Invalidate(auth.Credential) {}

func TestUnauthenticatedLoopbackEndpointAndReservedMethods(t *testing.T) {
	h := New(Options{History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
		return usagehistory.Report{}, nil
	})})
	for _, tc := range []struct {
		method string
		status int
	}{{http.MethodGet, 200}, {http.MethodPost, 405}, {http.MethodDelete, 405}} {
		r := httptest.NewRequest(tc.method, "/v1/utraque/providers", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s status=%d want %d", tc.method, w.Code, tc.status)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control=%q", got)
		}
	}
}

func TestEndpointRejectsNonLoopbackCaller(t *testing.T) {
	h := New(Options{History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
		t.Fatal("collection must not run")
		return usagehistory.Report{}, nil
	})})
	r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil)
	r.RemoteAddr = "192.0.2.10:4000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestReportUsesCallerBearerOnlyForAnthropicAndCachesCoherently(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	var anthToken string
	var historyCalls atomic.Int64
	history := historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
		historyCalls.Add(1)
		return sampleHistory(now), nil
	})
	obs := func(p providerquota.Provider) providerquota.Observation {
		return providerquota.Observation{Source: p, CollectedAt: now}
	}
	h := New(Options{History: history,
		Anthropic: anthropicFunc(func(_ context.Context, token string) (providerquota.Observation, error) {
			anthToken = token
			return obs(providerquota.ProviderAnthropic), nil
		}),
		DeepSeek: deepSeekFunc(func(context.Context) (providerquota.Observation, error) {
			return obs(providerquota.ProviderDeepSeek), nil
		}),
		Codex: codexFunc(func(context.Context, auth.CredentialSource, auth.Credential) (providerquota.Observation, error) {
			return obs(providerquota.ProviderCodex), nil
		}),
		CodexSource: sourceFunc(func(context.Context) (auth.Credential, error) {
			return auth.Credential{AccessToken: "codex-secret", AccountID: "account"}, nil
		}),
		DeepSeekAPIKey: "deepseek-secret", Now: func() time.Time { return now }})
	for range 2 {
		r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		r.Header.Set("Authorization", "Bearer anthropic-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		for _, secret := range []string{"anthropic-secret", "codex-secret", "deepseek-secret", "account"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("response leaked %q", secret)
			}
		}
		var report Report
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Providers) != 3 {
			t.Fatalf("providers=%d", len(report.Providers))
		}
	}
	if anthToken != "anthropic-secret" {
		t.Fatalf("anthropic token=%q", anthToken)
	}
	if historyCalls.Load() != 1 {
		t.Fatalf("history calls=%d want 1", historyCalls.Load())
	}
}

func TestRateLimitedQuotaReportsRetryTimeAndActualAttempt(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	attempted := now.Add(-time.Minute)
	retryAt := now.Add(4 * time.Minute)
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			return sampleHistory(now), nil
		}),
		Anthropic: anthropicFunc(func(context.Context, string) (providerquota.Observation, error) {
			return providerquota.Observation{}, &providerquota.Error{Provider: providerquota.ProviderAnthropic, Code: providerquota.CodeRateLimited, Retryable: true, RetryAt: &retryAt, AttemptedAt: attempted}
		}),
		Now: func() time.Time { return now },
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	p := providerNamed(decodeReport(t, w), "anthropic")
	if p == nil || !p.LastAttempt.Equal(attempted) || len(p.Errors) == 0 {
		t.Fatalf("provider=%+v", p)
	}
	err := p.Errors[0]
	if err.Section != "quota_after" || err.Code != "rate_limited" || !err.Retryable || err.RetryAt == nil || !err.RetryAt.Equal(retryAt) {
		t.Fatalf("report error=%+v", err)
	}
	if p.Calibration == nil || p.Calibration.UnavailableReason != "paired_quota_measurement_unavailable" {
		t.Fatalf("calibration=%+v", p.Calibration)
	}
}

func TestSlowHistoryDoesNotDelayHealthyQuotaRead(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	historyStarted := make(chan struct{})
	releaseHistory := make(chan struct{})
	quotaRead := make(chan struct{})
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			close(historyStarted)
			<-releaseHistory
			return sampleHistory(now), nil
		}),
		Anthropic: anthropicFunc(func(context.Context, string) (providerquota.Observation, error) {
			close(quotaRead)
			return providerquota.Observation{Source: providerquota.ProviderAnthropic, CollectedAt: now}, nil
		}),
		Now: func() time.Time { return now },
	})
	reportDone := make(chan Report, 1)
	go func() {
		reportDone <- h.collect(context.Background(), credentials{anthropicToken: "token"}, utcDate(now).AddDate(0, 0, -29), utcDate(now))
	}()
	<-historyStarted
	select {
	case <-quotaRead:
	case <-time.After(time.Second):
		t.Fatal("quota read waited for local history")
	}
	close(releaseHistory)
	p := providerNamed(<-reportDone, "anthropic")
	if p == nil || p.Quota == nil || p.QuotaBefore != nil || p.Paired != nil {
		t.Fatalf("provider=%+v", p)
	}
}

func TestReferencePricesUseCanonicalCandidatesAndObservedHistory(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	cacheRead, cacheWrite := .4, 5.0
	history := sampleHistory(now)
	history.Daily = append(history.Daily,
		usagehistory.DailyModelUsage{Date: utcDate(now), Source: "claude", Model: "old-gpt", Provider: usagehistory.ProviderCodex, TotalTokens: 10},
		usagehistory.DailyModelUsage{Date: utcDate(now), Source: "claude", Model: "sol", Provider: usagehistory.ProviderCodex, TotalTokens: 10})
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) { return history, nil }),
		ReferencePrices: priceFunc(func(context.Context) (referenceprice.Snapshot, error) {
			return referenceprice.Snapshot{Source: referenceprice.SourceModelsDev, ObservedAt: now, Unit: referenceprice.USDPerMillion,
				Models: []referenceprice.ModelPrice{
					{Provider: "codex", Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: &cacheRead, CacheWrite: &cacheWrite, HasHigherTier: true},
					{Provider: "codex", Model: "old-gpt", Input: 1, Output: 2},
					{Provider: "codex", Model: "unseen-gpt", Input: .1, Output: .2},
					{Provider: "anthropic", Model: "claude-sonnet-5", Input: 2, Output: 10},
				}}, nil
		}),
		EligiblePriceModels: func(provider string) []string {
			if provider == "codex" {
				return []string{"gpt-5.6-sol"}
			}
			return nil
		},
		NormalizePriceModel: func(provider, model string) string {
			if provider == "codex" && model == "sol" {
				return "gpt-5.6-sol"
			}
			return model
		},
		Now: func() time.Time { return now },
	})
	p := providerNamed(h.collect(context.Background(), credentials{}, utcDate(now).AddDate(0, 0, -29), utcDate(now)), "codex")
	if p == nil || p.ReferencePrices == nil || len(p.ReferencePrices.Models) != 2 {
		t.Fatalf("prices=%+v", p)
	}
	if got := p.ReferencePrices.Models[0]; got.Model != "gpt-5.6-sol" || !got.Eligible || got.CacheRead == nil || got.CacheWrite == nil {
		t.Fatalf("candidate=%+v", got)
	}
	if got := p.ReferencePrices.Models[1]; got.Model != "old-gpt" || got.Eligible {
		t.Fatalf("history-only=%+v", got)
	}
	if len(p.ReferencePrices.Assumptions) != 1 || p.ReferencePrices.Assumptions[0] != "base_tier" {
		t.Fatalf("assumptions=%v", p.ReferencePrices.Assumptions)
	}
	body, err := json.Marshal(p.ReferencePrices)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"provider"`) || strings.Contains(string(body), `"cache_read":null`) {
		t.Fatalf("internal or absent fields leaked: %s", body)
	}
}

func TestReferencePriceFailureIsProviderLocalAndCollectionConcurrent(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	priceStarted := make(chan struct{})
	releasePrice := make(chan struct{})
	quotaRead := make(chan struct{})
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			return sampleHistory(now), nil
		}),
		Anthropic: anthropicFunc(func(context.Context, string) (providerquota.Observation, error) {
			close(quotaRead)
			return providerquota.Observation{Source: providerquota.ProviderAnthropic, CollectedAt: now}, nil
		}),
		ReferencePrices: priceFunc(func(context.Context) (referenceprice.Snapshot, error) {
			close(priceStarted)
			<-releasePrice
			return referenceprice.Snapshot{}, &referenceprice.Error{Code: "timeout", Retryable: true}
		}),
		Now: func() time.Time { return now },
	})
	done := make(chan Report, 1)
	go func() {
		done <- h.collect(context.Background(), credentials{anthropicToken: "token"}, utcDate(now).AddDate(0, 0, -29), utcDate(now))
	}()
	<-priceStarted
	select {
	case <-quotaRead:
	case <-time.After(time.Second):
		t.Fatal("quota read waited for reference prices")
	}
	close(releasePrice)
	report := <-done
	for _, provider := range report.Providers {
		found := false
		for _, reportErr := range provider.Errors {
			if reportErr.Section == "reference_prices" && reportErr.Code == "timeout" && reportErr.Retryable {
				found = true
			}
		}
		if !found {
			t.Fatalf("provider %s errors=%+v", provider.Provider, provider.Errors)
		}
	}
}

func decodeReport(t *testing.T, w *httptest.ResponseRecorder) Report {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var report Report
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestConcurrentRequestsCoalesceAndFailedRefreshRetainsSeparateSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	failing := false
	var calls atomic.Int64
	history := historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
		calls.Add(1)
		if failing {
			return usagehistory.Report{}, errors.New("failed")
		}
		return sampleHistory(now), nil
	})
	reader := deepSeekFunc(func(context.Context) (providerquota.Observation, error) {
		if failing {
			return providerquota.Observation{}, errors.New("failed")
		}
		return providerquota.Observation{Source: providerquota.ProviderDeepSeek, CollectedAt: now}, nil
	})
	h := New(Options{History: history, DeepSeek: reader, CacheTTL: time.Second, Now: func() time.Time { return now }})
	request := func() Report {
		r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out Report
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	var wg sync.WaitGroup
	wg.Add(8)
	for range 8 {
		go func() { defer wg.Done(); request() }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("coalesced history calls=%d", calls.Load())
	}
	first := request()
	var originalCollected time.Time
	for _, p := range first.Providers {
		if p.Provider == "deepseek" && p.Quota != nil {
			originalCollected = p.Quota.CollectedAt
		}
	}
	now = now.Add(2 * time.Second)
	failing = true
	report := request()
	var age1 float64
	for _, p := range report.Providers {
		if p.Provider == "deepseek" {
			if p.LastComplete == nil {
				t.Fatal("deepseek missing last complete snapshot")
			}
			if !p.LastComplete.Freshness.Stale {
				t.Fatal("deepseek previous snapshot not stale")
			}
			if p.LastComplete.Quota == nil || !p.LastComplete.Quota.CollectedAt.Equal(originalCollected) {
				t.Fatal("original observation timestamp changed")
			}
			age1 = p.LastComplete.Freshness.AgeSeconds
		}
	}
	if age1 == 0 {
		t.Fatal("deepseek provider missing")
	}
	now = now.Add(2 * time.Second)
	report = request()
	for _, p := range report.Providers {
		if p.Provider == "deepseek" {
			if p.LastComplete == nil || p.LastComplete.Freshness.AgeSeconds <= age1 {
				t.Fatalf("age did not increase: before=%v after=%+v", age1, p.LastComplete)
			}
			if p.LastComplete.Quota == nil || !p.LastComplete.Quota.CollectedAt.Equal(originalCollected) {
				t.Fatal("repeated failure changed original observation")
			}
			return
		}
	}
	t.Fatal("deepseek provider missing after repeated failure")
}

func TestCodexAccountSwitchDuringCollectionCannotReplaceNewAccount(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	source := &switchingSource{cred: auth.Credential{AccessToken: "token-a", AccountID: "account-a"}}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var historyCalls atomic.Int64
	history := historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
		if historyCalls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return sampleHistory(now), nil
	})
	codex := codexFunc(func(_ context.Context, _ auth.CredentialSource, cred auth.Credential) (providerquota.Observation, error) {
		pct := 10.0
		if cred.AccountID == "account-b" {
			pct = 20
		}
		return providerquota.Observation{Source: providerquota.ProviderCodex, CollectedAt: now, Quotas: []providerquota.Quota{{ID: "primary", UsedPercent: pct}}}, nil
	})
	h := New(Options{History: history, Codex: codex, CodexSource: source, Now: func() time.Time { return now }})
	request := func() Report {
		r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out Report
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	aDone := make(chan Report, 1)
	go func() { aDone <- request() }()
	<-firstStarted
	source.set(auth.Credential{AccessToken: "token-b", AccountID: "account-b"})
	b := request()
	close(releaseFirst)
	a := <-aDone
	findCodex := func(r Report) *ProviderReport {
		for i := range r.Providers {
			if r.Providers[i].Provider == "codex" {
				return &r.Providers[i]
			}
		}
		return nil
	}
	bp := findCodex(b)
	if bp == nil || bp.Quota == nil || bp.Quota.Quotas[0].UsedPercent != 20 {
		t.Fatalf("new account report=%+v", bp)
	}
	ap := findCodex(a)
	if ap == nil || ap.Quota != nil || ap.Paired != nil || ap.LastComplete != nil {
		t.Fatalf("switched old account leaked=%+v", ap)
	}
	bCached := findCodex(request())
	if bCached == nil || bCached.Quota == nil || bCached.Quota.Quotas[0].UsedPercent != 20 {
		t.Fatalf("late A replaced B cache: %+v", bCached)
	}
	if historyCalls.Load() != 2 {
		t.Fatalf("history calls=%d want 2", historyCalls.Load())
	}
}

func TestCanceledCoalescedCallerDoesNotCancelSharedCollection(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	history := historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
		calls.Add(1)
		close(started)
		<-release
		return sampleHistory(time.Now().UTC()), nil
	})
	h := New(Options{History: history, Timeout: 2 * time.Second})
	do := func(ctx context.Context) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil).WithContext(ctx)
		r.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan int, 1)
	go func() { first <- do(ctx) }()
	<-started
	second := make(chan int, 1)
	go func() { second <- do(context.Background()) }()
	cancel()
	if status := <-first; status != http.StatusGatewayTimeout {
		t.Fatalf("canceled status=%d", status)
	}
	close(release)
	if status := <-second; status != http.StatusOK {
		t.Fatalf("coalesced status=%d", status)
	}
	if calls.Load() != 1 {
		t.Fatalf("history calls=%d", calls.Load())
	}
}

func TestMalformedFinalCodexScopeCannotRestorePreviousSnapshot(t *testing.T) {
	for name, malformed := range map[string]string{"whitespace": "   ", "overlong": strings.Repeat("x", (256<<10)+1)} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
			valid := auth.Credential{AccessToken: "token-a", AccountID: "account-a"}
			source := &sequenceSource{creds: []auth.Credential{valid, valid, valid, {AccessToken: "token-x", AccountID: malformed}}}
			history := historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
				return sampleHistory(now), nil
			})
			codex := codexFunc(func(context.Context, auth.CredentialSource, auth.Credential) (providerquota.Observation, error) {
				return providerquota.Observation{Source: providerquota.ProviderCodex, CollectedAt: now}, nil
			})
			h := New(Options{History: history, Codex: codex, CodexSource: source, CacheTTL: time.Second, Now: func() time.Time { return now }})
			request := func() Report {
				r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil)
				r.RemoteAddr = "127.0.0.1:4000"
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				var out Report
				_ = json.Unmarshal(w.Body.Bytes(), &out)
				return out
			}
			first := request()
			if p := providerNamed(first, "codex"); p == nil || p.Quota == nil || p.Paired != nil || p.Status != "ok" {
				t.Fatalf("initial snapshot=%+v", p)
			}
			now = now.Add(2 * time.Second)
			p := providerNamed(request(), "codex")
			if p == nil || p.QuotaBefore != nil || p.Quota != nil || p.Paired != nil || p.LastComplete != nil {
				t.Fatalf("malformed final scope leaked snapshot: %+v", p)
			}
			found := false
			for _, e := range p.Errors {
				if e.Code == "account_scope_unverified" {
					found = true
				}
			}
			if !found {
				t.Fatalf("errors=%+v", p.Errors)
			}
		})
	}
}

func providerNamed(r Report, name string) *ProviderReport {
	for i := range r.Providers {
		if r.Providers[i].Provider == name {
			return &r.Providers[i]
		}
	}
	return nil
}

func TestDeepSeekEstimateWeightsSourcesAndHandlesCurrencyAndPricing(t *testing.T) {
	trueValue := true
	models := []ModelStats{
		{Source: "claude", Model: "deepseek-v4", TotalTokens: 100, CostUSD: ptr(1.0), HistoricalEffectiveUSDToken: ptr(.01)},
		{Source: "opencode", Model: "deepseek-v4", TotalTokens: 300, CostUSD: ptr(3.0), HistoricalEffectiveUSDToken: ptr(.01)},
	}
	obs := providerquota.Observation{Available: &trueValue, Balances: []providerquota.Balance{{Currency: "USD", Total: "2.50"}, {Currency: "CNY", Total: "10.25"}}}
	got := estimateDeepSeek(obs, models)
	if len(got) != 2 || got[0].Tokens == nil || *got[0].Tokens != 250 {
		t.Fatalf("weighted estimates=%+v", got)
	}
	if got[1].Tokens != nil || got[1].UnavailableReason != "unsupported_balance_currency" {
		t.Fatalf("currency estimate=%+v", got[1])
	}
	obs.Balances = []providerquota.Balance{{Currency: "USD", Total: "0"}}
	if zero := estimateDeepSeek(obs, models); len(zero) != 1 || zero[0].Tokens == nil || *zero[0].Tokens != 0 {
		t.Fatalf("zero estimate=%+v", zero)
	}
	models = append(models, ModelStats{Source: "other", Model: "deepseek-v4", TotalTokens: 1})
	if unpriced := estimateDeepSeek(obs, models); unpriced[0].Tokens != nil || unpriced[0].UnavailableReason != "historical_effective_rate_unavailable" {
		t.Fatalf("unpriced estimate=%+v", unpriced)
	}
}

func TestCachedWindowPastResetIsStaleAndHasNoEstimate(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Second)
	report := Report{GeneratedAt: now, Providers: []ProviderReport{{Provider: "anthropic", Quota: &providerquota.Observation{Quotas: []providerquota.Quota{{ResetsAt: &reset}}}, Calibration: &Calibration{ConditionalRemaining: ptr(10.0)}}}}
	now = now.Add(2 * time.Second)
	report.GeneratedAt = now
	markFreshness(&report, true, false, 2*time.Second)
	if !report.Providers[0].SourceFreshness.Stale || report.Providers[0].Calibration.UnavailableReason != "quota_window_reset_after_collection" {
		t.Fatalf("provider=%+v", report.Providers[0])
	}
}

func TestInactivePastResetDoesNotInvalidateActiveFutureWindow(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	active, inactive := true, false
	report := Report{GeneratedAt: now, Providers: []ProviderReport{{Provider: "anthropic", Quota: &providerquota.Observation{Quotas: []providerquota.Quota{
		{ID: "session", Active: &active, ResetsAt: &future},
		{ID: "weekly_scoped", Active: &inactive, ResetsAt: &past, Scope: &providerquota.Scope{Model: &providerquota.ScopeLabel{ID: "claude-opus"}}},
	}}, Calibration: &Calibration{ConditionalRemaining: ptr(10.0)}}}}
	markFreshness(&report, true, false, time.Second)
	if report.Providers[0].SourceFreshness.Stale || report.Providers[0].Calibration.ConditionalRemaining == nil {
		t.Fatalf("provider=%+v", report.Providers[0])
	}
}

func sampleHistory(now time.Time) usagehistory.Report {
	cost := 1.0
	return usagehistory.Report{StartedAt: now, FinishedAt: now, SinceDate: now.AddDate(0, 0, -29), UntilDate: now,
		ToolVersion: "20.0.20", InvocationMode: usagehistory.InvocationNative, Source: usagehistory.SourceCCUsage, Coverage: usagehistory.CoverageLocalOnly,
		CostBasis: usagehistory.CostBasisCalculatedAPIReference, UnitPriceUnavailableReason: usagehistory.UnitPriceUnavailableCCUsage,
		Daily: []usagehistory.DailyModelUsage{{Date: utcDate(now), Source: "claude", Model: "deepseek-v4", Provider: usagehistory.ProviderDeepSeek, TotalTokens: 100, CostUSD: &cost, CostStatus: usagehistory.CostAvailable}}}
}

func ptr[T any](v T) *T { return &v }
