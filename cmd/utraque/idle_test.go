package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/idle"
	"github.com/hughescr/utraque/internal/launchd"
)

// stallingUpstream stands in for api.anthropic.com with a response that starts,
// then holds the connection open until the test releases it — the shape of a
// long SSE answer, which is exactly what an idle exit must never cut off. It
// contacts nothing real.
func stallingUpstream(t *testing.T) (url string, started <-chan struct{}, release chan<- struct{}) {
	t.Helper()
	startedC := make(chan struct{})
	releaseC := make(chan struct{})
	var once atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: first\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if once.CompareAndSwap(false, true) {
			close(startedC)
		}
		<-releaseC
		_, _ = io.WriteString(w, "event: last\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, startedC, releaseC
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestIdleTimerHeldByInFlightRequest is the guarantee that makes idle self-exit
// safe: a request that is still running holds the timer open, however long it
// takes and however little it says. Without it, a quiet hour inside one long
// streamed answer would kill the daemon mid-stream.
func TestIdleTimerHeldByInFlightRequest(t *testing.T) {
	upstream, started, release := stallingUpstream(t)

	cfg := config.Default()
	cfg.Anthropic.BaseURL = upstream

	var fired atomic.Bool
	firedC := make(chan struct{})
	var once atomic.Bool
	timer := idle.New(50*time.Millisecond, func() {
		fired.Store(true)
		if once.CompareAndSwap(false, true) {
			close(firedC)
		}
	})

	srv, err := newServer(cfg, discardLogger(), timer)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	front := httptest.NewServer(srv)
	defer front.Close()

	timer.Start()
	defer timer.Stop()

	type result struct {
		body string
		err  error
	}
	resc := make(chan result, 1)
	go func() {
		resp, err := http.Get(front.URL + "/v1/organizations/whatever")
		if err != nil {
			resc <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, rerr := io.ReadAll(resp.Body)
		resc <- result{body: string(b), err: rerr}
	}()

	<-started
	// Ten idle periods pass with the request still open and nothing on the wire.
	time.Sleep(500 * time.Millisecond)
	if fired.Load() {
		t.Fatal("the idle timer fired while a request was in flight")
	}
	if n := timer.InFlight(); n != 1 {
		t.Errorf("InFlight = %d, want 1 while the request runs", n)
	}

	close(release)
	res := <-resc
	if res.err != nil {
		t.Fatalf("the request failed: %v", res.err)
	}
	if res.body != "event: first\n\nevent: last\n\n" {
		t.Errorf("body = %q, want the whole stream", res.body)
	}

	// Once the hold is gone the timer is free to fire, which is what tells
	// launchd it may reclaim the process.
	select {
	case <-firedC:
	case <-time.After(10 * time.Second):
		t.Fatal("the idle timer never fired after the request finished")
	}
	if n := timer.InFlight(); n != 0 {
		t.Errorf("InFlight = %d after the request finished", n)
	}
}

// TestIdleExitDrainsInFlightResponse drives the real exit path: the idle
// callback cancels the serving context, and the response that is mid-flight
// still arrives complete.
func TestIdleExitDrainsInFlightResponse(t *testing.T) {
	upstream, started, release := stallingUpstream(t)

	cfg := config.Default()
	cfg.Anthropic.BaseURL = upstream

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := idle.New(time.Hour, cancel)

	srv, err := newServer(cfg, discardLogger(), timer)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	timer.Start()
	defer timer.Stop()

	done := make(chan error, 1)
	go func() { done <- srv.ServeAll(ctx, ln) }()

	type result struct {
		body string
		err  error
	}
	resc := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/v1/organizations/whatever")
		if err != nil {
			resc <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, rerr := io.ReadAll(resp.Body)
		resc <- result{body: string(b), err: rerr}
	}()

	<-started
	cancel() // stand in for the idle callback: begin the graceful exit
	close(release)

	res := <-resc
	if res.err != nil {
		t.Fatalf("the in-flight response failed during the drain: %v", res.err)
	}
	if res.body != "event: first\n\nevent: last\n\n" {
		t.Errorf("body = %q, want the whole stream to have drained", res.body)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeAll = %v, want nil after a clean idle exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeAll did not return after the idle exit")
	}
}

// TestIdlePolicy pins when self-exit turns itself on. Exiting is only safe when
// something can bring the daemon back, so the source of the socket decides the
// default and an explicit setting always wins.
func TestIdlePolicy(t *testing.T) {
	cases := map[string]struct {
		in   config.Idle
		src  launchd.Source
		want time.Duration
	}{
		"launchd, unset": {
			in: config.Idle{}, src: launchd.SourceLaunchd, want: config.DefaultLaunchdIdleTimeout,
		},
		"launchd, explicit 0 means never exit": {
			in: config.Idle{Timeout: 0, Explicit: true}, src: launchd.SourceLaunchd, want: 0,
		},
		"launchd, explicit value wins": {
			in: config.Idle{Timeout: 5 * time.Minute, Explicit: true}, src: launchd.SourceLaunchd, want: 5 * time.Minute,
		},
		"manual start stays off": {
			in: config.Idle{}, src: launchd.SourceListen, want: 0,
		},
		"manual start, explicit value is honoured": {
			in: config.Idle{Timeout: 90 * time.Second, Explicit: true}, src: launchd.SourceListen, want: 90 * time.Second,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := idlePolicy(tc.in, tc.src).Timeout; got != tc.want {
				t.Errorf("idlePolicy = %s, want %s", got, tc.want)
			}
		})
	}
}
