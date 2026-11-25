package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/launchd"
)

func TestBoundListeners(t *testing.T) {
	cases := []struct{ host, class string }{
		{"127.0.0.1", ""}, {"127.0.0.2", ""}, {"::1", ""}, {"::ffff:127.0.0.1", ""}, {"0.0.0.0", "wildcard"}, {"::", "wildcard"},
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Logf("cannot enumerate interfaces: %v", err)
	}
	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPNet); ok && ip.IP.IsGlobalUnicast() {
			cases = append(cases, struct{ host, class string }{ip.IP.String(), "non-loopback"})
			break
		}
	}
	for _, tc := range cases {
		for _, token := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/token=%t", tc.host, token), func(t *testing.T) {
				lns, src, err := launchd.Listen(launchd.Options{SocketName: "BoundListenerTest", Addr: net.JoinHostPort(tc.host, "0")})
				if err != nil {
					if tc.host == "127.0.0.1" || tc.host == "0.0.0.0" {
						t.Fatal(err)
					}
					t.Skipf("bind unavailable: %v", err)
				}
				defer launchd.CloseAll(lns)
				if src != launchd.SourceListen || len(lns) != 1 {
					t.Fatalf("expected one fallback listener, got %d from %s", len(lns), src)
				}
				ln := lns[0]
				if strings.Contains(tc.host, "ffff") {
					t.Logf("mapped bind: addr=%s IP bytes=%x", ln.Addr(), []byte(ln.Addr().(*net.TCPAddr).IP))
				}
				var logs bytes.Buffer
				err = validateListeners(lns, token, slog.New(slog.NewTextHandler(&logs, nil)))
				if !token && tc.class != "" {
					if err == nil || !strings.Contains(err.Error(), "UTRAQUE_LOCAL_TOKEN") || !strings.Contains(err.Error(), tc.class) {
						t.Fatalf("expected %s refusal; got %v", tc.class, err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				wantWarnings := 0
				if token && tc.class != "" {
					wantWarnings = 1
				}
				if strings.Count(logs.String(), "level=WARN") != wantWarnings {
					t.Fatalf("unexpected warnings: %s", &logs)
				}
			})
		}
	}
}

func TestUnixListenerRequiresToken(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	err = validateListeners([]net.Listener{ln}, false, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "unclassifiable") {
		t.Fatalf("expected unclassifiable refusal; got %v", err)
	}
}

// addrListener reports a fixed address; it is never accepted on.
type addrListener struct {
	net.Listener
	addr net.Addr
}

func (l addrListener) Addr() net.Addr { return l.addr }

// One refusal names every distinct unsafe class and every unsafe address, so
// a single attempt shows the whole fix.
func TestRefusalNamesEveryUnsafeListener(t *testing.T) {
	lns := []net.Listener{
		addrListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1).To4(), Port: 1}},
		addrListener{addr: &net.TCPAddr{IP: net.IPv4zero.To4(), Port: 2}},
		addrListener{addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.7").To4(), Port: 3}},
		addrListener{addr: &net.TCPAddr{IP: net.IPv6unspecified, Port: 4}},
		addrListener{addr: &net.UnixAddr{Name: "/nonexistent/sock", Net: "unix"}},
	}
	err := validateListeners(lns, false, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("unsafe listeners admitted")
	}
	msg := err.Error()
	for _, want := range []string{"wildcard", "non-loopback", "unclassifiable", "0.0.0.0:2", "192.0.2.7:3", "[::]:4", "/nonexistent/sock", "UTRAQUE_LOCAL_TOKEN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
	summary, _, _ := strings.Cut(msg, " listener refused")
	if summary != "wildcard, non-loopback, unclassifiable" {
		t.Errorf("refusal summary %q does not name each unsafe class once", summary)
	}
	if strings.Contains(msg, "127.0.0.1") {
		t.Errorf("refusal %q lists a safe listener", msg)
	}
	var logs bytes.Buffer
	if err := validateListeners(lns, true, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "level=WARN") != 1 || !strings.Contains(logs.String(), "192.0.2.7:3") {
		t.Errorf("token-admitted warning: %s", logs.String())
	}
}

type countedAccept struct {
	net.Listener
	accepts atomic.Int32
}

func (l *countedAccept) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	return l.Listener.Accept()
}

// A refused socket activation waits out a retry delay (a minute, not
// injectable) so a queued connection cannot respawn it every second. The
// listeners must already be closed while it waits, and cancelling the context
// must end the wait promptly.
func TestRefusedActivationClosesListenersBeforeWaitingAndStopsOnCancel(t *testing.T) {
	var listeners []net.Listener
	var dialAddrs []string
	for _, bind := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
		ln, err := net.Listen("tcp4", bind)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		listeners = append(listeners, ln)
		dialAddrs = append(dialAddrs, net.JoinHostPort("127.0.0.1", fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)))
	}
	dir := t.TempDir()
	env := map[string]string{"HOME": dir, config.EnvCodexAuthFile: filepath.Join(dir, "auth.json"), config.EnvCodexCacheFile: filepath.Join(dir, "catalog.json"), config.EnvCodexClientVersion: "1.0.0"}
	previousLog := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLog) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var logs bytes.Buffer
		done <- runWithListen(ctx, func(k string) string { return env[k] }, &logs, func(launchd.Options) ([]net.Listener, launchd.Source, error) {
			return listeners, launchd.SourceLaunchd, nil
		})
	}()

	// Before any cancellation: each refused listener stops accepting, and the
	// call is still waiting.
	for _, addr := range dialAddrs {
		deadline := time.Now().Add(2 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp4", addr, 200*time.Millisecond)
			if err != nil {
				break
			}
			_ = conn.Close()
			if time.Now().After(deadline) {
				t.Fatalf("listener %s still accepts connections while the retry delay runs", addr)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("runWithListen returned %v without waiting out the retry delay or a cancellation", err)
	case <-time.After(150 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "wildcard") {
			t.Fatalf("expected wildcard refusal; got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runWithListen did not return within a second of the context being cancelled")
	}
}

func TestMixedInheritedSetStartsNothing(t *testing.T) {
	var listeners []net.Listener
	for _, bind := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
		ln, err := net.Listen("tcp4", bind)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		listeners = append(listeners, &countedAccept{Listener: ln})
	}
	var warms atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { warms.Add(1) }))
	defer up.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, fmt.Appendf(nil, `{"tokens":{"account_id":"synthetic","access_token":%q,"refresh_token":"fixture"}}`, jwtWithExp(time.Now().Add(time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": dir, config.EnvCodexAuthFile: path, config.EnvCodexCacheFile: filepath.Join(dir, "catalog.json"), config.EnvCodexClientVersion: "1.0.0", config.EnvCodexBaseURL: up.URL}
	previousLog := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLog) })
	// Cancellation also interrupts the misconfigured launch agent's retry delay.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var logs bytes.Buffer
	err := runWithListen(ctx, func(k string) string { return env[k] }, &logs, func(launchd.Options) ([]net.Listener, launchd.Source, error) {
		return listeners, launchd.SourceLaunchd, nil
	})
	if err == nil || !strings.Contains(err.Error(), "wildcard") {
		t.Fatalf("expected wildcard refusal; got %v", err)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Error("refused socket activation exited without waiting for cancellation or backoff")
	}
	for _, ln := range listeners {
		if n := ln.(*countedAccept).accepts.Load(); n != 0 {
			t.Errorf("Accept called %d times", n)
		}
		if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("listener was not closed: %v", err)
		}
	}
	if warms.Load() != 0 || strings.Contains(logs.String(), "catalog warmed") {
		t.Fatal("startup fetched catalog")
	}
}
