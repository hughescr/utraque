package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/providerquota"
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

type reportSource struct{}

func (reportSource) Get(context.Context) (auth.Credential, error) {
	return auth.Credential{AccessToken: "throwaway", AccountID: "throwaway-account"}, nil
}
func (reportSource) Invalidate(auth.Credential) {}

func TestProductionReportCompositionUsesAllInjectedSources(t *testing.T) {
	var historyCalls, anthropicCalls, deepSeekCalls, codexCalls atomic.Int64
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	deps := &reportDependencies{
		history: reportHistoryFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			historyCalls.Add(1)
			return usagehistory.Report{StartedAt: now, FinishedAt: now, Source: usagehistory.SourceCCUsage, Coverage: usagehistory.CoverageLocalOnly, CostBasis: usagehistory.CostBasisCalculatedAPIReference}, nil
		}),
		anthropic: reportAnthropicFunc(func(context.Context, string) (providerquota.Observation, error) {
			anthropicCalls.Add(1)
			return providerquota.Observation{Source: providerquota.ProviderAnthropic, CollectedAt: now}, nil
		}),
		deepseek: reportDeepSeekFunc(func(context.Context) (providerquota.Observation, error) {
			deepSeekCalls.Add(1)
			return providerquota.Observation{Source: providerquota.ProviderDeepSeek, CollectedAt: now}, nil
		}),
		codex: reportCodexFunc(func(context.Context, auth.CredentialSource, auth.Credential) (providerquota.Observation, error) {
			codexCalls.Add(1)
			return providerquota.Observation{Source: providerquota.ProviderCodex, CollectedAt: now}, nil
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
	if historyCalls.Load() != 1 || anthropicCalls.Load() != 1 || deepSeekCalls.Load() != 1 || codexCalls.Load() != 1 {
		t.Fatalf("calls history=%d anthropic=%d deepseek=%d codex=%d", historyCalls.Load(), anthropicCalls.Load(), deepSeekCalls.Load(), codexCalls.Load())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("report response is cacheable")
	}
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
