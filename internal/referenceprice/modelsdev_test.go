package referenceprice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestModelsDevCachesAndRevalidatesWithETag(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	var calls atomic.Int64
	body := testCatalog(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("public catalog request carried authorization: %q", got)
		}
		if call == 2 {
			if got := r.Header.Get("If-None-Match"); got != `"prices-v1"` {
				t.Errorf("If-None-Match=%q", got)
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"prices-v1"`)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client, err := NewModelsDevClient(Options{URL: server.URL, TTL: 5 * time.Minute, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Read(context.Background())
	if err != nil || len(first.Models) != 3 || !first.ObservedAt.Equal(clock.Now()) {
		t.Fatalf("first snapshot=%+v err=%v", first, err)
	}
	if first.Models[1].Provider != "codex" || first.Models[1].Model != "gpt-5.6-sol" || !first.Models[1].HasHigherTier {
		t.Fatalf("OpenAI catalog mapping=%+v", first.Models[1])
	}
	if _, err := client.Read(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("fresh cache calls=%d err=%v", calls.Load(), err)
	}
	clock.Advance(5 * time.Minute)
	second, err := client.Read(context.Background())
	if err != nil || calls.Load() != 2 || !second.ObservedAt.Equal(clock.Now()) || second.Stale {
		t.Fatalf("revalidated snapshot=%+v calls=%d err=%v", second, calls.Load(), err)
	}
}

func TestModelsDevCanceledContextDoesNotStartRefresh(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client, err := NewModelsDevClient(Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Read(ctx); err == nil {
		t.Fatal("canceled read unexpectedly succeeded")
	}
	if calls.Load() != 0 {
		t.Fatalf("canceled read made %d upstream calls", calls.Load())
	}
}

func TestModelsDevUsesLastGoodDuringFailureBackoff(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	var calls atomic.Int64
	body := testCatalog(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, err := NewModelsDevClient(Options{URL: server.URL, TTL: time.Minute, FailureBackoff: 30 * time.Second, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	stale, err := client.Read(context.Background())
	var priceErr *Error
	if !errors.As(err, &priceErr) || !priceErr.Retryable || !stale.Stale || len(stale.Models) != 3 {
		t.Fatalf("stale snapshot=%+v err=%v", stale, err)
	}
	if _, err := client.Read(context.Background()); !errors.As(err, &priceErr) || calls.Load() != 2 {
		t.Fatalf("failure cache calls=%d err=%v", calls.Load(), err)
	}
	clock.Advance(30 * time.Second)
	_, _ = client.Read(context.Background())
	if calls.Load() != 3 {
		t.Fatalf("refresh did not resume: calls=%d", calls.Load())
	}
}

func TestModelsDevCoalescesConcurrentReadsAndDetachesLeaderCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	body := testCatalog(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := NewModelsDevClient(Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	leaderCtx, cancel := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() { _, err := client.Read(leaderCtx); leader <- err }()
	<-started
	follower := make(chan error, 1)
	go func() { _, err := client.Read(context.Background()); follower <- err }()
	cancel()
	if err := <-leader; err == nil {
		t.Fatal("canceled caller unexpectedly succeeded")
	}
	close(release)
	if err := <-follower; err != nil {
		t.Fatalf("healthy follower inherited cancellation: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
}

func TestModelsDevRejectsOversizeAndInvalidCatalogs(t *testing.T) {
	for name, body := range map[string][]byte{
		"oversize":         []byte(`0123456789`),
		"missing_provider": []byte(`{"anthropic":{"id":"anthropic","models":{}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
			defer server.Close()
			max := int64(8 << 20)
			if name == "oversize" {
				max = 5
			}
			client, err := NewModelsDevClient(Options{URL: server.URL, MaxResponseBytes: max})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Read(context.Background())
			var priceErr *Error
			if !errors.As(err, &priceErr) {
				t.Fatalf("err=%v", err)
			}
			// Literal expectations pin the wire values independently of the
			// CodeInvalidData / CodeTooLarge constants.
			var want ErrorCode = "invalid_response"
			if name == "oversize" {
				want = "response_too_large"
			}
			if priceErr.Code != want {
				t.Fatalf("code=%q want %q", priceErr.Code, want)
			}
		})
	}
}

// TestErrorCodeWireValues pins every ErrorCode constant to the string the
// provider report emits for it, so a renamed constant cannot silently change
// the wire vocabulary.
func TestErrorCodeWireValues(t *testing.T) {
	for want, got := range map[string]ErrorCode{
		"configuration_error": CodeConfiguration,
		"unavailable":         CodeUnavailable,
		"timeout":             CodeTimeout,
		"response_too_large":  CodeTooLarge,
		"invalid_response":    CodeInvalidData,
	} {
		if string(got) != want {
			t.Errorf("code=%q want %q", got, want)
		}
	}
}

func testCatalog(t *testing.T) []byte {
	t.Helper()
	root := map[string]any{
		"anthropic": map[string]any{"id": "anthropic", "models": map[string]any{
			"claude-sonnet-5": map[string]any{"id": "claude-sonnet-5", "cost": map[string]any{"input": 2, "output": 10, "cache_read": .2, "cache_write": 2.5}},
		}},
		"openai": map[string]any{"id": "openai", "models": map[string]any{
			"gpt-5.6-sol": map[string]any{"id": "gpt-5.6-sol", "cost": map[string]any{"input": 4, "output": 20, "cache_read": .4, "cache_write": 5, "tiers": []any{map[string]any{"context_over": 272000}}}},
		}},
		"deepseek": map[string]any{"id": "deepseek", "models": map[string]any{
			"deepseek-flash": map[string]any{"id": "deepseek-flash", "cost": map[string]any{"input": .1, "output": .2}},
		}},
	}
	body, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
