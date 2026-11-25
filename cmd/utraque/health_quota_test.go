package main

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/responses"
)

func TestInvalidOnlyQuotaIsNotAnObservation(t *testing.T) {
	h := make(http.Header)
	h.Set("X-Codex-Primary-Used-Percent", "NaN")
	o := newCodexObserver()
	o.observeRateLimits("scope-a", responses.ParseRateLimits(h, time.Now()))
	if o.quotaHealth("scope-a")["reported"] != false {
		t.Fatal("an invalid field marked the quota reported")
	}
}

// A sample is shown for its own account, or when the current account is
// unknown, and never for another account. The latest sample wins.
func TestQuotaIsShownOnlyForItsAccount(t *testing.T) {
	h := make(http.Header)
	h.Set("X-Codex-Primary-Used-Percent", "17")
	rl := responses.ParseRateLimits(h, time.Now())
	o := newCodexObserver()
	o.observeRateLimits("scope-a", rl)
	for current, want := range map[string]bool{"scope-a": true, "": true, "scope-b": false} {
		if got := o.quotaHealth(current)["reported"]; got != want {
			t.Errorf("A's sample with current %q: reported %v, want %v", current, got, want)
		}
	}
	o.observeRateLimits("scope-b", rl)
	if o.quotaHealth("scope-a")["reported"] != false || o.quotaHealth("scope-b")["reported"] != true {
		t.Error("the latest sample did not replace the previous account's")
	}
}

// A sample's age runs from the moment it was observed, on the observer's own
// clock: near zero when fresh, advancing with the clock, and restarting when a
// newer sample arrives.
func TestQuotaAgeRunsFromTheObservation(t *testing.T) {
	h := make(http.Header)
	h.Set("X-Codex-Primary-Used-Percent", "17")
	rl := responses.ParseRateLimits(h, time.Now())
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	o := newCodexObserver()
	o.nowFn = func() time.Time { return now }
	o.observeRateLimits("scope-a", rl)
	age := func() float64 {
		t.Helper()
		got, ok := o.quota("scope-a")["age_s"].(float64)
		if !ok {
			t.Fatalf("no age_s in %v", o.quota("scope-a"))
		}
		return got
	}
	if got := age(); got != 0 {
		t.Fatalf("a fresh sample's age_s = %v, want 0", got)
	}
	now = now.Add(90 * time.Second)
	if got := age(); got != 90 {
		t.Fatalf("age_s after 90s = %v, want 90", got)
	}
	o.observeRateLimits("scope-a", rl)
	if got := age(); got != 0 {
		t.Fatalf("a newer sample's age_s = %v, want 0", got)
	}
}

func TestHealthzIgnoresInvalidQuotaPercent(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, nil)
	env.codex.respond = func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int64) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "NaN")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		w.Header().Set("X-Codex-Secondary-Used-Percent", "0")
		writeSSE(w, solStream)
	}
	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("messages: status %d, read error %v", resp.StatusCode, err)
	}

	var health struct {
		Quota struct {
			Primary   map[string]json.RawMessage `json:"primary"`
			Secondary map[string]json.RawMessage `json:"secondary"`
		} `json:"codex_quota"`
	}
	if err := json.Unmarshal([]byte(getHealth(t, env)), &health); err != nil {
		t.Fatalf("invalid health JSON: %v", err)
	}
	if _, ok := health.Quota.Primary["used_percent"]; ok {
		t.Error("invalid primary used_percent must be absent")
	}
	if got := string(health.Quota.Primary["window_minutes"]); got != "300" {
		t.Errorf("primary window_minutes = %q, want 300", got)
	}
	if got := string(health.Quota.Secondary["used_percent"]); got != "0" {
		t.Errorf("secondary used_percent = %q, want explicit zero", got)
	}
}
