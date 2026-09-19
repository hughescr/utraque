// Package relay holds the upstream-relay plumbing the passthrough legs
// (Anthropic and DeepSeek) share: the rolling upstream-idle guard, the reader
// that re-arms it on every byte, the URL scrubber that keeps a caller's query
// string out of a *url.Error, and the classifier that turns a failed round
// trip into the right API error. It imports only the standard library and
// internal/apierr, so any leg can use it without depending on the router.
//
// This is the byte-level idle bound for a leg that relays an upstream body
// verbatim. The Codex leg's SSE translator keeps its own event-level timer,
// because it must turn an idle end into either a pre-start HTTP error or a
// post-start SSE error frame; that timer is deliberately not this one.
package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
)

// UpstreamIdleGuard arms the rolling upstream-idle deadline for one round
// trip. The zero value (no timeout configured) is inert: Stop, Fired and Wrap
// all become no-ops, so a leg's forward path needs no branches.
type UpstreamIdleGuard struct {
	timeout time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	tripped atomic.Bool
}

// WithUpstreamIdleDeadline derives from parent a context that is cancelled
// once the upstream has been silent for timeout, and returns the guard that
// owns the countdown. A timeout of zero or less disables the bound: parent is
// returned unchanged with an inert guard.
//
// The guard cancels only the derived context, so the caller's own
// cancellation stays distinguishable from a timeout when the error is
// classified afterwards (see UpstreamError). The countdown starts here and is
// re-armed by every read through Wrap; the caller must Stop the guard when
// the round trip ends.
func WithUpstreamIdleDeadline(parent context.Context, timeout time.Duration) (context.Context, *UpstreamIdleGuard) {
	g := &UpstreamIdleGuard{timeout: timeout}
	if timeout <= 0 {
		return parent, g
	}
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	g.timer = time.AfterFunc(timeout, func() {
		g.tripped.Store(true)
		cancel()
	})
	return ctx, g
}

// Stop disarms the countdown and releases the derived context. It is safe to
// call on an inert guard and more than once.
func (g *UpstreamIdleGuard) Stop() {
	if g.timer != nil {
		g.timer.Stop()
	}
	if g.cancel != nil {
		g.cancel()
	}
}

// Fired reports whether the idle deadline has tripped.
func (g *UpstreamIdleGuard) Fired() bool { return g.tripped.Load() }

// Timeout is the configured idle bound, or zero when the guard is inert. It
// is what a leg names in its "went silent for" messages.
func (g *UpstreamIdleGuard) Timeout() time.Duration { return g.timeout }

// Wrap restarts the countdown on every byte read from r, which is what makes
// the bound an idle one rather than a total one. An inert guard returns r
// unchanged.
func (g *UpstreamIdleGuard) Wrap(r io.Reader) io.Reader {
	if g.timer == nil {
		return r
	}
	return &IdleResetReader{r: r, g: g}
}

// IdleResetReader is the io.Reader Wrap returns: each successful read re-arms
// the guard's countdown, unless the deadline has already tripped.
type IdleResetReader struct {
	r io.Reader
	g *UpstreamIdleGuard
}

// Read reads from the wrapped reader and, when it delivered bytes, re-arms the
// idle countdown.
func (ir *IdleResetReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 && !ir.g.tripped.Load() {
		ir.g.timer.Reset(ir.g.timeout)
	}
	return n, err
}

// UpstreamError classifies a failed round trip. prefix names the upstream in
// the message ("upstream" for the Anthropic passthrough, "deepseek upstream"
// for the DeepSeek leg); the three messages are external, so a leg keeps its
// own wording by choosing its prefix. An idle expiry, or any other timeout,
// is a 504 timeout_error rather than the blanket 502: the client's retry
// policy for "upstream is slow" differs from "upstream is broken". The cause
// is scrubbed with ScrubURLError before it is wrapped.
func UpstreamError(prefix string, err error, idle *UpstreamIdleGuard) error {
	if idle != nil && idle.Fired() {
		return apierr.Wrap(ScrubURLError(err), apierr.TypeTimeout,
			"%s sent nothing for %s", prefix, idle.Timeout())
	}
	var ne net.Error
	if (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return apierr.Wrap(ScrubURLError(err), apierr.TypeTimeout, "%s request timed out", prefix)
	}
	e := apierr.Wrap(ScrubURLError(err), apierr.TypeAPI, "%s request failed", prefix)
	e.Status = http.StatusBadGateway
	return e
}

// ScrubURLError strips the query string (and any userinfo) from the URL
// net/http stamps into a *url.Error. That URL is the caller's own, verbatim,
// and reaches the log through the error's cause; a query-string credential
// must not end up there. Go redacts only the userinfo password. An error that
// is not a *url.Error is returned unchanged; a URL that does not parse is
// replaced by "invalid-url".
func ScrubURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safe := "invalid-url"
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.User = nil
		u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
		u.ForceQuery = false
		safe = u.String()
	}
	return &url.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}
