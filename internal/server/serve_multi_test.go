package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/server"
)

// listenLocal opens a loopback listener that the test closes if it is never
// handed to a server.
func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func get(t *testing.T, addr, path string) (string, error) {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// TestServeAllAcceptsOnEveryListener is the socket-activation shape: launchd
// hands over one descriptor per address family, and every one of them has to be
// accepted on.
func TestServeAllAcceptsOnEveryListener(t *testing.T) {
	s, _ := newServer(t, func(o *server.Options) {
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "served")
		})
	})
	a, b := listenLocal(t), listenLocal(t)
	addrA, addrB := a.Addr().String(), b.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeAll(ctx, a, b) }()

	for _, addr := range []string{addrA, addrB} {
		body, err := get(t, addr, "/anything")
		if err != nil {
			t.Fatalf("GET %s: %v", addr, err)
		}
		if body != "served" {
			t.Errorf("GET %s body = %q", addr, body)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeAll = %v, want nil on a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeAll did not return after the shutdown")
	}

	// Both listeners are closed by the shutdown, so neither still answers.
	for _, addr := range []string{addrA, addrB} {
		if _, err := get(t, addr, "/anything"); err == nil {
			t.Errorf("%s still accepts after the shutdown", addr)
		}
	}
}

// TestServeAllDrainsInFlightOnEveryListener is the idle-exit guarantee seen
// from the server: the cancellation that the idle timer delivers must let a
// response that is already streaming finish.
func TestServeAllDrainsInFlightOnEveryListener(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s, _ := newServer(t, func(o *server.Options) {
		o.ShutdownGrace = 5 * time.Second
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "first-chunk ")
			w.(http.Flusher).Flush()
			close(started)
			<-release
			_, _ = io.WriteString(w, "last-chunk")
		})
	})
	a, b := listenLocal(t), listenLocal(t)
	secondAddr := b.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeAll(ctx, a, b) }()

	type result struct {
		body string
		err  error
	}
	resc := make(chan result, 1)
	go func() {
		body, err := get(t, secondAddr, "/stream")
		resc <- result{body, err}
	}()

	<-started
	cancel() // the idle timer's self-exit arrives exactly like this
	close(release)

	res := <-resc
	if res.err != nil {
		t.Fatalf("the in-flight response failed during the drain: %v", res.err)
	}
	if res.body != "first-chunk last-chunk" {
		t.Errorf("body = %q, want the whole response", res.body)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeAll = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeAll did not return after the drain")
	}
}

func TestServeAllNeedsAListener(t *testing.T) {
	s, _ := newServer(t, nil)
	if err := s.ServeAll(context.Background()); err == nil {
		t.Error("ServeAll with no listeners must be an error")
	}
}

// TestServeAllStopsWhenOneListenerFails: a listener that dies takes the daemon
// down rather than leaving one of launchd's addresses unaccepted.
func TestServeAllStopsWhenOneListenerFails(t *testing.T) {
	s, _ := newServer(t, nil)
	good := listenLocal(t)
	bad := listenLocal(t)
	_ = bad.Close()

	done := make(chan error, 1)
	go func() { done <- s.ServeAll(context.Background(), good, bad) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("ServeAll = nil, want the accept failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeAll hung on a failed listener")
	}
}
