package relay_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/relay"
)

// TestInertGuardLeavesEverythingAlone: no timeout configured means the parent
// context comes back untouched and every method is a no-op, so a leg's
// forward path never has to branch on whether the bound is on.
func TestInertGuardLeavesEverythingAlone(t *testing.T) {
	parent := context.Background()
	for _, d := range []time.Duration{0, -time.Second} {
		ctx, g := relay.WithUpstreamIdleDeadline(parent, d)
		if ctx != parent {
			t.Errorf("timeout %s: context was derived, want the parent back", d)
		}
		if g.Fired() {
			t.Errorf("timeout %s: an inert guard fired", d)
		}
		r := strings.NewReader("x")
		if g.Wrap(r) != io.Reader(r) {
			t.Errorf("timeout %s: Wrap wrapped the reader", d)
		}
		g.Stop()
		g.Stop()
	}
}

// TestGuardFiresAfterSilence: with nothing read, the deadline trips, cancels
// the derived context and reports Fired.
func TestGuardFiresAfterSilence(t *testing.T) {
	ctx, g := relay.WithUpstreamIdleDeadline(context.Background(), 20*time.Millisecond)
	defer g.Stop()
	if g.Timeout() != 20*time.Millisecond {
		t.Fatalf("Timeout() = %s", g.Timeout())
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the idle deadline never cancelled the context")
	}
	if !g.Fired() {
		t.Error("Fired() = false after the deadline tripped")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want Canceled (the guard cancels, it does not set a deadline)", ctx.Err())
	}
}

// slowReader hands out one byte per call after a fixed pause, for as many
// calls as it has bytes.
type slowReader struct {
	pause time.Duration
	left  int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.pause)
	s.left--
	p[0] = 'x'
	return 1, nil
}

// TestReadsReArmTheGuard is the property that makes the bound idle rather
// than total: a stream that keeps trickling bytes at intervals shorter than
// the timeout outlives many timeouts' worth of wall clock.
func TestReadsReArmTheGuard(t *testing.T) {
	const timeout = 60 * time.Millisecond
	ctx, g := relay.WithUpstreamIdleDeadline(context.Background(), timeout)
	defer g.Stop()
	src := &slowReader{pause: timeout / 3, left: 12} // 12 * 20ms = 240ms > 60ms
	r := g.Wrap(src)
	if _, ok := r.(*relay.IdleResetReader); !ok {
		t.Fatalf("Wrap returned %T, want *relay.IdleResetReader", r)
	}
	n, err := io.Copy(io.Discard, r)
	if err != nil || n != 12 {
		t.Fatalf("copy = %d, %v; want 12, nil", n, err)
	}
	if g.Fired() || ctx.Err() != nil {
		t.Error("a live stream tripped the idle deadline")
	}
}

// TestStopDisarms: stopping before the deadline means it never fires.
func TestStopDisarms(t *testing.T) {
	ctx, g := relay.WithUpstreamIdleDeadline(context.Background(), 20*time.Millisecond)
	g.Stop()
	time.Sleep(50 * time.Millisecond)
	if g.Fired() {
		t.Error("a stopped guard fired")
	}
	// Stop releases the derived context, which is what a deferred Stop is for.
	if ctx.Err() == nil {
		t.Error("Stop did not release the derived context")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

var _ net.Error = timeoutErr{}

// TestUpstreamErrorClassification pins the three messages each leg emits;
// they are external error text, so both prefixes are spelled out here.
func TestUpstreamErrorClassification(t *testing.T) {
	fired := func() *relay.UpstreamIdleGuard {
		_, g := relay.WithUpstreamIdleDeadline(context.Background(), time.Nanosecond)
		for !g.Fired() {
			time.Sleep(time.Millisecond)
		}
		return g
	}
	cause := errors.New("connection refused")
	cases := []struct {
		name    string
		prefix  string
		err     error
		idle    *relay.UpstreamIdleGuard
		typ     apierr.ErrorType
		status  int
		message string
	}{
		{"anthropic idle", "upstream", cause, fired(), apierr.TypeTimeout, 0, "upstream sent nothing for 1ns"},
		{"deepseek idle", "deepseek upstream", cause, fired(), apierr.TypeTimeout, 0, "deepseek upstream sent nothing for 1ns"},
		{"net timeout", "upstream", timeoutErr{}, nil, apierr.TypeTimeout, 0, "upstream request timed out"},
		{"deadline exceeded", "deepseek upstream", context.DeadlineExceeded, &relay.UpstreamIdleGuard{}, apierr.TypeTimeout, 0, "deepseek upstream request timed out"},
		{"other", "upstream", cause, &relay.UpstreamIdleGuard{}, apierr.TypeAPI, http.StatusBadGateway, "upstream request failed"},
		{"deepseek other", "deepseek upstream", cause, nil, apierr.TypeAPI, http.StatusBadGateway, "deepseek upstream request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := relay.UpstreamError(tc.prefix, tc.err, tc.idle)
			var ae *apierr.Error
			if !errors.As(err, &ae) {
				t.Fatalf("UpstreamError returned %T, want *apierr.Error", err)
			}
			if ae.Type != tc.typ || ae.Message != tc.message || ae.Status != tc.status {
				t.Errorf("got type=%s status=%d message=%q; want type=%s status=%d message=%q",
					ae.Type, ae.Status, ae.Message, tc.typ, tc.status, tc.message)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("the cause was not preserved: %v", err)
			}
		})
	}
}

// TestScrubURLError: the query string, userinfo and fragment leave; the
// operation, the cause and the rest of the URL stay.
func TestScrubURLError(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	in := &url.Error{Op: "Get", URL: "https://user:hunter2@example.com/v1/models?key=hunter2#frag", Err: cause}
	out := relay.ScrubURLError(in)
	var ue *url.Error
	if !errors.As(out, &ue) {
		t.Fatalf("scrubbed error is %T, want *url.Error", out)
	}
	if ue.URL != "https://example.com/v1/models" {
		t.Errorf("URL = %q, want the path only", ue.URL)
	}
	if ue.Op != "Get" || !errors.Is(out, cause) {
		t.Errorf("op or cause lost: %v", out)
	}
	if strings.Contains(out.Error(), "hunter2") {
		t.Errorf("secret survived scrubbing: %v", out)
	}

	// Wrapped one level down, it is still found and still scrubbed.
	wrapped := relay.ScrubURLError(errors.Join(errors.New("outer"), in))
	if strings.Contains(wrapped.Error(), "hunter2") {
		t.Errorf("secret survived scrubbing through a wrapper: %v", wrapped)
	}

	// Not a *url.Error: returned as-is.
	if got := relay.ScrubURLError(cause); got != cause {
		t.Errorf("non-URL error was replaced: %v", got)
	}

	// Unparseable URL: replaced by a fixed marker rather than leaked.
	bad := relay.ScrubURLError(&url.Error{Op: "Get", URL: "http://[::1]:namedport?key=hunter2", Err: cause})
	if !strings.Contains(bad.Error(), "invalid-url") || strings.Contains(bad.Error(), "hunter2") {
		t.Errorf("unparseable URL not replaced: %v", bad)
	}
}
