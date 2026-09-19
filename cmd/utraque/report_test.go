package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/leg"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/providerreport"
	"github.com/hughescr/utraque/internal/referenceprice"
	"github.com/hughescr/utraque/internal/server"
	"github.com/hughescr/utraque/internal/usagehistory"
)

type reportHistoryFunc func(context.Context, time.Time, time.Time) (usagehistory.Report, error)

func (f reportHistoryFunc) Collect(c context.Context, s, u time.Time) (usagehistory.Report, error) {
	return f(c, s, u)
}

type reportAnthropicFunc func(context.Context, string) (providerquota.Observation, error)

func (f reportAnthropicFunc) Read(c context.Context, s string) (providerquota.Observation, error) {
	return f(c, s)
}

type reportDeepSeekFunc func(context.Context) (providerquota.Observation, error)

func (f reportDeepSeekFunc) Read(c context.Context) (providerquota.Observation, error) { return f(c) }

type reportCodexFunc func(context.Context, auth.CredentialSource, auth.Credential) (providerquota.Observation, error)

func (f reportCodexFunc) ReadCredential(c context.Context, s auth.CredentialSource, cr auth.Credential) (providerquota.Observation, error) {
	return f(c, s, cr)
}

type reportPricesFunc func(context.Context) (referenceprice.Snapshot, error)

func (f reportPricesFunc) Read(c context.Context) (referenceprice.Snapshot, error) { return f(c) }

type reportSource struct{}

func (reportSource) Get(context.Context) (auth.Credential, error) {
	return auth.Credential{AccessToken: "throwaway", AccountID: "throwaway-account"}, nil
}
func (reportSource) Invalidate(auth.Credential) {}

func TestProductionReportCompositionUsesAllInjectedSources(t *testing.T) {
	var historyCalls, anthropicCalls, deepSeekCalls, codexCalls, priceCalls atomic.Int64
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	deps := &reportDependencies{
		history: reportHistoryFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			historyCalls.Add(1)
			return usagehistory.Report{StartedAt: now, FinishedAt: now, Source: usagehistory.SourceCCUsage, Coverage: usagehistory.CoverageLocalOnly, CostBasis: usagehistory.CostBasisCalculatedAPIReference}, nil
		}),
		anthropic: reportAnthropicFunc(func(context.Context, string) (providerquota.Observation, error) {
			anthropicCalls.Add(1)
			return providerquota.Observation{Source: leg.Anthropic, CollectedAt: now}, nil
		}),
		deepseek: reportDeepSeekFunc(func(context.Context) (providerquota.Observation, error) {
			deepSeekCalls.Add(1)
			return providerquota.Observation{Source: leg.DeepSeek, CollectedAt: now}, nil
		}),
		codex: reportCodexFunc(func(context.Context, auth.CredentialSource, auth.Credential) (providerquota.Observation, error) {
			codexCalls.Add(1)
			return providerquota.Observation{Source: leg.Codex, CollectedAt: now}, nil
		}),
		prices: reportPricesFunc(func(context.Context) (referenceprice.Snapshot, error) {
			priceCalls.Add(1)
			return referenceprice.Snapshot{Source: referenceprice.SourceModelsDev, ObservedAt: now, Unit: referenceprice.USDPerMillion,
				Models: []referenceprice.ModelPrice{{Provider: "codex", Model: "gpt-5.6-sol", Input: 4, Output: 20}}}, nil
		}),
	}
	cfg := config.Default()
	cfg.LocalToken = "local-secret"
	cfg.DeepSeek.APIKey = "deep-secret"
	h, err := newProviderReport(cfg, reportSource{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{Config: cfg, Routes: server.Routes{ProviderReport: h, Passthrough: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, server.ProviderReportPath, nil)
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set(server.LocalTokenHeader, cfg.LocalToken)
	r.Header.Set("Authorization", "Bearer caller-token")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if historyCalls.Load() != 1 || anthropicCalls.Load() != 1 || deepSeekCalls.Load() != 1 || codexCalls.Load() != 1 || priceCalls.Load() != 1 {
		t.Fatalf("calls history=%d anthropic=%d deepseek=%d codex=%d prices=%d", historyCalls.Load(), anthropicCalls.Load(), deepSeekCalls.Load(), codexCalls.Load(), priceCalls.Load())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("report response is cacheable")
	}
	var report providerreport.Report
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if codex := reportProviderNamed(report, leg.Codex); codex == nil || codex.ReferencePrices == nil || len(codex.ReferencePrices.Models) != 1 || !codex.ReferencePrices.Models[0].Eligible {
		t.Fatalf("codex reference prices=%+v", codex)
	}
}

func TestReferencePriceCandidatesAndHistoryNormalizationFollowRoutes(t *testing.T) {
	cfg := config.Default()
	anthropicModels := eligibleReferencePriceModels(cfg, leg.Anthropic)
	if !contains(anthropicModels, "claude-haiku-4-5") || contains(anthropicModels, "claude-3-haiku") {
		t.Fatalf("anthropic candidates=%v", anthropicModels)
	}
	if codexModels := eligibleReferencePriceModels(cfg, leg.Codex); !contains(codexModels, "gpt-5.6-sol") {
		t.Fatalf("codex candidates=%v", codexModels)
	}
	if got := eligibleReferencePriceModels(cfg, leg.DeepSeek); len(got) != 0 {
		t.Fatalf("unconfigured DeepSeek candidates=%v", got)
	}
	cfg.DeepSeek.APIKey = "configured"
	if got := eligibleReferencePriceModels(cfg, leg.DeepSeek); len(got) != 2 || !contains(got, "deepseek-flash") || !contains(got, "deepseek-v4-pro") {
		t.Fatalf("DeepSeek candidates=%v", got)
	}
	for _, tc := range []struct {
		id    leg.ID
		model string
		want  string
	}{{leg.Codex, "sol", "gpt-5.6-sol"}, {leg.DeepSeek, "deepseek-v4-flash", "deepseek-flash"}, {leg.Anthropic, "CLAUDE-SONNET-5", "claude-sonnet-5"},
		// An alias that resolves to another leg is left alone, not relabelled.
		{leg.DeepSeek, "sol", "sol"}, {leg.Codex, "deepseek-v4-flash", "deepseek-v4-flash"}} {
		if got := normalizeReferencePriceModel(tc.id, tc.model); got != tc.want {
			t.Errorf("normalizeReferencePriceModel(%q,%q)=%q want %q", tc.id, tc.model, got, tc.want)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func reportProviderNamed(report providerreport.Report, id leg.ID) *providerreport.ProviderReport {
	for i := range report.Providers {
		if report.Providers[i].Provider == id {
			return &report.Providers[i]
		}
	}
	return nil
}

func TestDeepSeekAccountBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.deepseek.com/anthropic":   "https://api.deepseek.com",
		"http://127.0.0.1:1234/test/anthropic": "http://127.0.0.1:1234/test",
		"http://127.0.0.1:1234":                "http://127.0.0.1:1234",
	} {
		if got := deepSeekAccountBase(in); got != want {
			t.Errorf("deepSeekAccountBase(%q)=%q want %q", in, got, want)
		}
	}
}
