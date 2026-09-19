package providerquota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/leg"
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

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestRetryAfterDeadline(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		raw  string
		want time.Time
		ok   bool
	}{
		{name: "delta seconds", raw: "90", want: now.Add(90 * time.Second), ok: true},
		{name: "delta longer than one day", raw: "172800", want: now.Add(48 * time.Hour), ok: true},
		{name: "http date", raw: now.Add(2 * time.Hour).Format(http.TimeFormat), want: now.Add(2 * time.Hour), ok: true},
		{name: "past http date", raw: now.Add(-time.Hour).Format(http.TimeFormat)},
		{name: "negative", raw: "-1"},
		{name: "malformed", raw: "later"},
		{name: "overflow", raw: "18446744073709551615"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRetryAfterDeadline(tt.raw, now)
			if ok != tt.ok || ok && !got.Equal(tt.want) {
				t.Fatalf("deadline=(%v,%v), want (%v,%v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestFallbackBackoffIncreasesAndCaps(t *testing.T) {
	for _, tt := range []struct {
		consecutive uint
		want        time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{4, 8 * time.Minute},
		{5, 15 * time.Minute},
		{100, 15 * time.Minute},
	} {
		if got := fallbackBackoff(tt.consecutive); got != tt.want {
			t.Errorf("fallbackBackoff(%d)=%v want %v", tt.consecutive, got, tt.want)
		}
	}
}

func TestAnthropicRateLimitCooldownAndFallback(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	var calls atomic.Int64
	retryAfter := "120"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"limits":[]}`))
	}))
	defer server.Close()
	client, err := NewAnthropicClient(AnthropicOptions{URL: server.URL, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}

	read := func() *Error {
		_, err := client.Read(context.Background(), "token-a")
		var quotaErr *Error
		if !errors.As(err, &quotaErr) {
			t.Fatalf("error=%v, want provider quota error", err)
		}
		return quotaErr
	}
	first := read()
	if first.Code != CodeRateLimited || first.RetryAt == nil || !first.RetryAt.Equal(clock.Now().Add(120*time.Second)) || !first.AttemptedAt.Equal(clock.Now()) {
		t.Fatalf("first rate-limit error=%+v", first)
	}
	if second := read(); second.RetryAt == nil || !second.RetryAt.Equal(*first.RetryAt) || !second.AttemptedAt.Equal(first.AttemptedAt) {
		t.Fatalf("cooldown error=%+v, want original retry/attempt metadata", second)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d during cooldown, want 1", calls.Load())
	}

	clock.Add(121 * time.Second)
	retryAfter = ""
	third := read()
	if third.RetryAt == nil || !third.RetryAt.Equal(clock.Now().Add(2*time.Minute)) {
		t.Fatalf("second consecutive fallback=%+v, want two-minute backoff", third)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d after cooldown, want 2", calls.Load())
	}
	clock.Add(2*time.Minute + time.Second)
	if _, err := client.Read(context.Background(), "token-a"); err != nil {
		t.Fatalf("read after cooldown: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls=%d after resume, want 3", calls.Load())
	}
}

func TestRateLimitCooldownIsAccountScoped(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	var tokenA, tokenB atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			tokenA.Add(1)
			w.Header().Set("Retry-After", "300")
			w.WriteHeader(http.StatusTooManyRequests)
		case "Bearer token-b":
			tokenB.Add(1)
			_, _ = w.Write([]byte(`{"limits":[]}`))
		default:
			t.Fatalf("unexpected authorization header")
		}
	}))
	defer server.Close()
	client, _ := NewAnthropicClient(AnthropicOptions{URL: server.URL, Now: clock.Now})
	_, _ = client.Read(context.Background(), "token-a")
	if _, err := client.Read(context.Background(), "token-b"); err != nil {
		t.Fatalf("independent account was suppressed: %v", err)
	}
	_, _ = client.Read(context.Background(), "token-a")
	if tokenA.Load() != 1 || tokenB.Load() != 1 {
		t.Fatalf("calls token-a=%d token-b=%d, want 1 each", tokenA.Load(), tokenB.Load())
	}
}

func TestConcurrentRateLimitRequestsShareOneAttempt(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client, _ := NewAnthropicClient(AnthropicOptions{URL: server.URL, Now: clock.Now})

	const readers = 12
	var ready, done sync.WaitGroup
	ready.Add(readers)
	done.Add(readers)
	for range readers {
		go func() {
			defer done.Done()
			ready.Done()
			_, err := client.Read(context.Background(), "same-token")
			var quotaErr *Error
			if !errors.As(err, &quotaErr) || quotaErr.Code != CodeRateLimited {
				t.Errorf("error=%v, want rate_limited", err)
			}
		}()
	}
	ready.Wait()
	<-started
	close(release)
	done.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent upstream calls=%d, want 1", calls.Load())
	}
}

func TestWaitingCallerCanCancelWithoutCancelingFlight(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"limits":[]}`))
	}))
	defer server.Close()
	client, _ := NewAnthropicClient(AnthropicOptions{URL: server.URL, Now: clock.Now})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := client.Read(context.Background(), "same-token")
		leaderDone <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := client.Read(ctx, "same-token")
		waiterDone <- err
	}()
	cancel()
	select {
	case err := <-waiterDone:
		var quotaErr *Error
		if !errors.As(err, &quotaErr) || quotaErr.Code != CodeUnavailable {
			t.Fatalf("canceled waiter error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked")
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader was canceled with waiter: %v", err)
	}
}

func TestLeaderCancellationIsSharedAndLaterCallRetries(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	started := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"limits":[]}`))
	}))
	defer server.Close()
	client, _ := NewAnthropicClient(AnthropicOptions{URL: server.URL, Now: clock.Now})
	ctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := client.Read(ctx, "same-token")
		leaderDone <- err
	}()
	<-started
	followerDone := make(chan error, 1)
	go func() {
		_, err := client.Read(context.Background(), "same-token")
		followerDone <- err
	}()
	scope, _ := AnthropicCacheScope("same-token")
	waitForFlightWaiter(t, client.http.state, scope)
	cancel()
	for name, done := range map[string]<-chan error{"leader": leaderDone, "follower": followerDone} {
		var quotaErr *Error
		if err := <-done; !errors.As(err, &quotaErr) || quotaErr.Code != CodeUnavailable {
			t.Fatalf("%s error=%v, want shared unavailable", name, err)
		}
	}
	if _, err := client.Read(context.Background(), "same-token"); err != nil {
		t.Fatalf("later retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want canceled attempt plus retry", calls.Load())
	}
}

func waitForFlightWaiter(t *testing.T, state *httpState, scope string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state.mu.Lock()
		entry := state.entries[scope]
		waiting := entry != nil && entry.flight != nil && entry.flight.waiters > 0
		state.mu.Unlock()
		if waiting {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("follower did not join the in-flight request")
}

func TestCooldownCapacityNeverEvictsActiveEntry(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	state := &httpState{entries: make(map[string]*httpStateEntry)}
	for i := range maxHTTPStateEntries {
		scope := "active-" + strconv.Itoa(i)
		state.entries[scope] = &httpStateEntry{retryAt: now.Add(time.Hour), lastAttempt: now, touchedAt: now}
	}
	settings := httpSettings{now: func() time.Time { return now }, state: state}
	_, _, err := settings.acquire(context.Background(), leg.Anthropic, "new-scope")
	var quotaErr *Error
	if !errors.As(err, &quotaErr) || quotaErr.Code != CodeUnavailable {
		t.Fatalf("capacity error=%v, want unavailable", err)
	}
	if len(state.entries) != maxHTTPStateEntries {
		t.Fatalf("state entries=%d want %d", len(state.entries), maxHTTPStateEntries)
	}
	for i := range maxHTTPStateEntries {
		if state.entries["active-"+strconv.Itoa(i)] == nil {
			t.Fatalf("active cooldown %d was evicted", i)
		}
	}
}

func TestProvidersHaveIndependentCooldowns(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	anthropicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer anthropicServer.Close()
	deepSeekServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[]}`))
	}))
	defer deepSeekServer.Close()
	anthropic, _ := NewAnthropicClient(AnthropicOptions{URL: anthropicServer.URL, Now: clock.Now})
	deepSeek, _ := NewDeepSeekClient(DeepSeekOptions{BaseURL: deepSeekServer.URL, APIKey: "deepseek-token", Now: clock.Now})
	_, _ = anthropic.Read(context.Background(), "anthropic-token")
	if _, err := deepSeek.Read(context.Background()); err != nil {
		t.Fatalf("DeepSeek was blocked by Anthropic cooldown: %v", err)
	}
}
