// Package idle implements the inactivity timer that lets utraque exit
// gracefully under launchd socket activation. launchd keeps the listening
// socket, so exiting after a quiet period costs nothing: the next connection
// re-launches the daemon.
//
// The timer never fires while a request is in flight. Callers wrap each
// request in Hold/release, so a long-running SSE stream that emits nothing for
// an hour cannot trigger a mid-stream exit.
package idle

import (
	"net/http"
	"sync"
	"time"
)

// Timer counts down to an idle callback and is safe for concurrent use.
//
// The implementation is deliberately self-correcting rather than Reset-driven:
// Touch only records the activity time, and the expiry callback re-checks how
// long it has actually been idle, rescheduling for the remainder when it is
// early. That removes every Stop/Reset/already-firing race at the cost of at
// most one extra wakeup per idle period.
type Timer struct {
	timeout time.Duration
	onIdle  func()

	now       func() time.Time
	afterFunc func(time.Duration, func()) *time.Timer

	mu       sync.Mutex
	timer    *time.Timer
	last     time.Time
	inflight int
	started  bool
	stopped  bool
	fired    bool
}

// Option customises a Timer.
type Option func(*Timer)

// WithClock injects a clock, for tests. A nil func leaves that half unchanged.
func WithClock(now func() time.Time, afterFunc func(time.Duration, func()) *time.Timer) Option {
	return func(t *Timer) {
		if now != nil {
			t.now = now
		}
		if afterFunc != nil {
			t.afterFunc = afterFunc
		}
	}
}

// New builds a Timer that calls onIdle once, after timeout has passed with no
// activity and no request in flight. A timeout of 0 or a nil callback disables
// it. The countdown does not begin until Start is called.
func New(timeout time.Duration, onIdle func(), opts ...Option) *Timer {
	t := &Timer{
		timeout:   timeout,
		onIdle:    onIdle,
		now:       time.Now,
		afterFunc: time.AfterFunc,
	}
	for _, o := range opts {
		o(t)
	}
	t.last = t.now()
	return t
}

// Enabled reports whether this timer can ever fire.
func (t *Timer) Enabled() bool { return t != nil && t.timeout > 0 && t.onIdle != nil }

// Timeout is the configured idle period.
func (t *Timer) Timeout() time.Duration {
	if t == nil {
		return 0
	}
	return t.timeout
}

// Start begins the countdown. It is idempotent, and a no-op when the timer is
// disabled or already stopped.
func (t *Timer) Start() {
	if !t.Enabled() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started || t.stopped {
		return
	}
	t.started = true
	t.last = t.now()
	t.timer = t.afterFunc(t.timeout, t.expire)
}

// Touch records activity, deferring the idle callback by a further timeout.
//
// The proxy does not call it: Hold's release already refreshes the activity
// time, so wrapping a request is enough. It exists for activity that is not a
// request, and the tests use it to drive the expiry logic directly.
func (t *Timer) Touch() {
	if !t.Enabled() {
		return
	}
	t.mu.Lock()
	t.last = t.now()
	t.mu.Unlock()
}

// Hold marks a request in flight and returns its release func. The timer
// cannot fire while any hold is outstanding. Release is idempotent.
func (t *Timer) Hold() func() {
	if !t.Enabled() {
		return func() {}
	}
	t.mu.Lock()
	t.inflight++
	t.last = t.now()
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.inflight > 0 {
				t.inflight--
			}
			t.last = t.now()
			t.mu.Unlock()
		})
	}
}

// InFlight is the number of outstanding holds.
func (t *Timer) InFlight() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inflight
}

// LastActivity is when activity was last recorded.
func (t *Timer) LastActivity() time.Time {
	if t == nil {
		return time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

// Fired reports whether the idle callback has run.
func (t *Timer) Fired() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fired
}

// Stop halts the timer permanently. It is idempotent and safe to call after
// the callback has fired, or before Start.
func (t *Timer) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	if t.timer != nil {
		t.timer.Stop()
	}
}

// expire runs on the timer goroutine. It reschedules whenever it turns out not
// to be idle after all, and invokes onIdle at most once.
func (t *Timer) expire() {
	t.mu.Lock()
	if t.stopped || t.fired {
		t.mu.Unlock()
		return
	}
	if t.inflight > 0 {
		// A request is running: nothing is idle. Check again a period later.
		t.timer.Reset(t.timeout)
		t.mu.Unlock()
		return
	}
	if elapsed := t.now().Sub(t.last); elapsed < t.timeout {
		t.timer.Reset(t.timeout - elapsed)
		t.mu.Unlock()
		return
	}
	t.fired = true
	cb := t.onIdle
	t.mu.Unlock()

	if cb != nil {
		cb()
	}
}

// Middleware holds the timer open for the duration of each request.
//
// The proxy does NOT use it. server.withActivity does the same thing and two
// more besides: it exempts /healthz, so a monitoring poll cannot keep the daemon
// alive forever, and it refuses a request that arrived after Fired, so a
// connection accepted in the shutdown gap cannot start a stream the drain would
// cut. Prefer that one; this is the standalone version, for a caller with no
// server around it.
func (t *Timer) Middleware(next http.Handler) http.Handler {
	if !t.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release := t.Hold()
		defer release()
		next.ServeHTTP(w, r)
	})
}
