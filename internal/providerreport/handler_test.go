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

type sourceFunc func(context.Context) (auth.Credential, error)

func (f sourceFunc) Get(c context.Context) (auth.Credential, error) { return f(c) }
func (sourceFunc) Invalidate(auth.Credential)                       {}

func TestEndpointRequiresLocalTokenConfigurationAndReservesMethods(t *testing.T) {
	h := New(Options{})
	for _, tc := range []struct {
		method string
		status int
	}{{http.MethodGet, 503}, {http.MethodPost, 405}, {http.MethodDelete, 405}} {
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
	h := New(Options{LocalTokenConfigured: true, History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
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
	h := New(Options{LocalTokenConfigured: true, History: history,
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
	h := New(Options{LocalTokenConfigured: true, History: history, DeepSeek: reader, CacheTTL: time.Second, Now: func() time.Time { return now }})
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
	now = now.Add(2 * time.Second)
	failing = true
	report := request()
	for _, p := range report.Providers {
		if p.Provider == "deepseek" {
			if p.LastComplete == nil {
				t.Fatal("deepseek missing last complete snapshot")
			}
			if !p.LastComplete.Freshness.Stale {
				t.Fatal("deepseek previous snapshot not stale")
			}
			return
		}
	}
	t.Fatal("deepseek provider missing")
}

func TestCalibrationRejectsMixedProviderAndComputesExactClaudeBlock(t *testing.T) {
	start := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	reset := start.Add(6 * time.Hour)
	q := providerquota.Quota{ID: "five_hour", UsedPercent: 25, DurationSeconds: ptr(int64(18000)), ResetsAt: &reset}
	pair := &PairedMeasurement{StartedAt: start, EndedAt: start.Add(time.Hour), Before: providerquota.Observation{CollectedAt: start, Quotas: []providerquota.Quota{q}}, After: providerquota.Observation{CollectedAt: start.Add(time.Hour), Quotas: []providerquota.Quota{q}}}
	block := usagehistory.BlockSummary{StartTime: reset.Add(-5 * time.Hour), EndTime: reset, ActualEndTime: ptr(start.Add(30 * time.Minute)), TotalTokens: 100, Models: []usagehistory.BlockModel{{Model: "claude-sonnet", Provider: usagehistory.ProviderAnthropic}}}
	history := &HistorySummary{StartedAt: start.Add(15 * time.Minute), FinishedAt: start.Add(45 * time.Minute), Blocks: []usagehistory.BlockSummary{block}}
	c := calibrateAnthropic(pair, history)
	if c.ConditionalRemaining == nil || *c.ConditionalRemaining != 300 {
		t.Fatalf("calibration=%+v", c)
	}
	history.Blocks[0].Models = append(history.Blocks[0].Models, usagehistory.BlockModel{Model: "gpt-5", Provider: usagehistory.ProviderCodex})
	c = calibrateAnthropic(pair, history)
	if c.UnavailableReason != "block_contains_non_anthropic_models" {
		t.Fatalf("reason=%q", c.UnavailableReason)
	}
}

func TestCalibrationRejectsScopedZeroAndDriftingQuota(t *testing.T) {
	start := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	reset := start.Add(6 * time.Hour)
	duration := int64(18000)
	base := providerquota.Quota{ID: "five_hour", Kind: "session", UsedPercent: 20, DurationSeconds: &duration, ResetsAt: &reset}
	pair := &PairedMeasurement{StartedAt: start, EndedAt: start.Add(time.Hour), Before: providerquota.Observation{Quotas: []providerquota.Quota{base}}, After: providerquota.Observation{CollectedAt: start.Add(time.Hour), Quotas: []providerquota.Quota{base}}}
	history := &HistorySummary{StartedAt: start.Add(time.Minute), FinishedAt: start.Add(30 * time.Minute)}
	pair.After.Quotas[0].UsedPercent = 0
	if got := calibrateAnthropic(pair, history).UnavailableReason; got != "quota_percent_not_calibratable" {
		t.Fatalf("zero reason=%q", got)
	}
	pair.After.Quotas[0].UsedPercent = 19
	if got := calibrateAnthropic(pair, history).UnavailableReason; got != "quota_percent_decreased_during_collection" {
		t.Fatalf("decrease reason=%q", got)
	}
	pair.After.Quotas[0].UsedPercent = 23
	if got := calibrateAnthropic(pair, history).UnavailableReason; got != "quota_changed_materially_during_collection" {
		t.Fatalf("drift reason=%q", got)
	}
	scope := &providerquota.Scope{Model: &providerquota.ScopeLabel{ID: "claude-sonnet"}}
	pair.Before.Quotas[0].Scope = scope
	pair.After.Quotas[0].Scope = scope
	if got := calibrateAnthropic(pair, history).UnavailableReason; got != "five_hour_quota_unavailable" {
		t.Fatalf("scoped reason=%q", got)
	}
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
	obs.SpendControls = []providerquota.SpendControl{{ScopeID: "credits", Reached: true}}
	if restricted := estimateDeepSeek(obs, models); len(restricted) != 1 || restricted[0].UnavailableReason != "spend_control_reached" {
		t.Fatalf("restricted=%+v", restricted)
	}
}

func TestCachedWindowPastResetIsStaleAndHasNoEstimate(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Second)
	report := Report{GeneratedAt: now, Providers: []ProviderReport{{Provider: "anthropic", QuotaAfter: &providerquota.Observation{Quotas: []providerquota.Quota{{ResetsAt: &reset}}}, Calibration: &Calibration{ConditionalRemaining: ptr(10.0)}}}}
	now = now.Add(2 * time.Second)
	report.GeneratedAt = now
	markFreshness(&report, true, false, 2*time.Second)
	if !report.Providers[0].SourceFreshness.Stale || report.Providers[0].Calibration.UnavailableReason != "quota_window_reset_after_collection" {
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
