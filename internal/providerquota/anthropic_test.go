package providerquota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func TestAnthropicReadModernAndRedactsScope(t *testing.T) {
	const token = "anthropic-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/oauth/usage" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour":{"utilization":99,"resets_at":"2026-09-11T14:00:00Z"},
			"limits":[
				{"kind":"session","group":"session","percent":4,"resets_at":"2026-09-11T14:00:00Z","scope":null,"is_active":true},
				{"kind":"weekly_scoped","group":"weekly","percent":16,"resets_at":null,"scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}
			],
			"extra_usage":{"is_enabled":true,"monthly_limit":100,"used_credits":100,"utilization":100,"currency":"USD"},
			"private_account":{"email":"do-not-copy@example.com"}
		}`))
	}))
	defer server.Close()

	client, err := NewAnthropicClient(AnthropicOptions{URL: server.URL + "/api/oauth/usage", Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Read(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Quotas) != 2 {
		t.Fatalf("quotas = %d, want modern limits only", len(got.Quotas))
	}
	if got.Quotas[0].UsedPercent != 4 || got.Quotas[0].Unit != PercentUnit || *got.Quotas[0].DurationSeconds != 5*60*60 {
		t.Errorf("session quota = %+v", got.Quotas[0])
	}
	if got.Quotas[1].ID != "weekly_scoped:model=Fable" || got.Quotas[1].Scope.Model.DisplayName != "Fable" || got.Quotas[1].ResetsAt != nil {
		t.Errorf("scoped quota = %+v", got.Quotas[1])
	}
	if got.ExtraUsage == nil || got.ExtraUsage.MonthlyLimit == nil || *got.ExtraUsage.MonthlyLimit != "100" || got.ExtraUsage.UsedCredits == nil || *got.ExtraUsage.UsedCredits != "100" || got.ExtraUsage.AmountUnit != "provider_units" {
		t.Errorf("extra usage = %+v", got.ExtraUsage)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{token, got.CacheScope(), "do-not-copy@example.com"} {
		if strings.Contains(string(wire), forbidden) {
			t.Errorf("JSON leaked %q: %s", forbidden, wire)
		}
	}
	wantScope, _ := AnthropicCacheScope(token)
	if got.CacheScope() != wantScope || wantScope == "" {
		t.Errorf("cache scope mismatch")
	}
}

func TestAnthropicLegacyMissingAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr ErrorCode
		wantN   int
	}{
		{"legacy", `{"five_hour":{"utilization":0,"resets_at":"2026-09-11T14:00:00Z"},"seven_day_sonnet":null}`, "", 1},
		{"explicit empty limits", `{"limits":[]}`, "", 0},
		{"unrecognized object", `{}`, CodeInvalidData, 0},
		{"invalid percent", `{"five_hour":{"utilization":101}}`, CodeInvalidData, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			client, _ := NewAnthropicClient(AnthropicOptions{URL: server.URL, Now: func() time.Time { return fixedNow }})
			got, err := client.Read(context.Background(), "token")
			if tt.wantErr != "" {
				var qerr *Error
				if !errors.As(err, &qerr) || qerr.Code != tt.wantErr {
					t.Fatalf("error = %v, want %s", err, tt.wantErr)
				}
				return
			}
			if err != nil || len(got.Quotas) != tt.wantN {
				t.Fatalf("got %+v, err %v", got, err)
			}
			if tt.name == "legacy" && got.Quotas[0].UsedPercent != 0 {
				t.Errorf("real zero was not preserved")
			}
		})
	}
}

func TestAnthropicRejectsRedirectAndBoundsBody(t *testing.T) {
	var followed bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer redirect.Close()
	client, _ := NewAnthropicClient(AnthropicOptions{URL: redirect.URL})
	_, err := client.Read(context.Background(), "secret")
	if err == nil || followed {
		t.Fatalf("redirect err=%v followed=%v", err, followed)
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"limits":[]}` + strings.Repeat(" ", 100)))
	}))
	defer large.Close()
	client, _ = NewAnthropicClient(AnthropicOptions{URL: large.URL, MaxResponseBytes: 10})
	_, err = client.Read(context.Background(), "secret")
	var qerr *Error
	if !errors.As(err, &qerr) || qerr.Code != CodeTooLarge {
		t.Fatalf("error = %v", err)
	}
}
