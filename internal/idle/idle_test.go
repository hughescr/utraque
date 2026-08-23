package idle_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hughescr/utraque/internal/idle"
)

func TestFiresAfterTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		defer tm.Stop()

		time.Sleep(59 * time.Minute)
		synctest.Wait()
		if fired.Load() != 0 {
			t.Fatal("fired early, at 59m")
		}

		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got := fired.Load(); got != 1 {
			t.Fatalf("fired = %d, want 1", got)
		}
		if !tm.Fired() {
			t.Error("Fired() = false")
		}
	})
}

func TestTouchDefersFire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		defer tm.Stop()

		time.Sleep(30 * time.Minute)
		tm.Touch()

		time.Sleep(59 * time.Minute) // 89m elapsed, but only 59m since Touch
		synctest.Wait()
		if fired.Load() != 0 {
			t.Fatal("fired despite activity at 30m")
		}

		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got := fired.Load(); got != 1 {
			t.Fatalf("fired = %d, want 1", got)
		}
	})
}

func TestRepeatedTouchNeverFires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		defer tm.Stop()

		for range 12 {
			time.Sleep(30 * time.Minute)
			tm.Touch()
		}
		synctest.Wait()
		if fired.Load() != 0 {
			t.Fatal("fired after 6h of steady traffic")
		}
	})
}

func TestHoldPreventsFire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		defer tm.Stop()

		release := tm.Hold()
		if tm.InFlight() != 1 {
			t.Fatalf("InFlight = %d, want 1", tm.InFlight())
		}
		time.Sleep(5 * time.Hour) // a very long stream
		synctest.Wait()
		if fired.Load() != 0 {
			t.Fatal("fired while a request was in flight")
		}

		release()
		release() // idempotent
		if tm.InFlight() != 0 {
			t.Fatalf("InFlight = %d after release", tm.InFlight())
		}

		time.Sleep(61 * time.Minute)
		synctest.Wait()
		if got := fired.Load(); got != 1 {
			t.Fatalf("fired = %d, want 1 once the hold was released", got)
		}
	})
}

func TestNestedHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		defer tm.Stop()

		r1 := tm.Hold()
		r2 := tm.Hold()
		if tm.InFlight() != 2 {
			t.Fatalf("InFlight = %d, want 2", tm.InFlight())
		}
		time.Sleep(3 * time.Hour)
		r1()
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		if fired.Load() != 0 {
			t.Fatal("fired while the second hold was outstanding")
		}
		r2()
		time.Sleep(61 * time.Minute)
		synctest.Wait()
		if got := fired.Load(); got != 1 {
			t.Fatalf("fired = %d, want 1", got)
		}
	})
}

func TestCallbackFiresAtMostOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		defer tm.Stop()

		time.Sleep(10 * time.Hour)
		synctest.Wait()
		tm.Touch()
		time.Sleep(10 * time.Hour)
		synctest.Wait()

		if got := fired.Load(); got != 1 {
			t.Fatalf("fired = %d, want exactly 1", got)
		}
	})
}

func TestStopPreventsFire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		time.Sleep(30 * time.Minute)
		tm.Stop()
		tm.Stop() // idempotent
		time.Sleep(5 * time.Hour)
		synctest.Wait()
		if fired.Load() != 0 {
			t.Fatal("fired after Stop")
		}
	})
}

func TestStopBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Stop()
		tm.Start() // must stay stopped
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		if fired.Load() != 0 {
			t.Fatal("Start after Stop restarted the timer")
		}
	})
}

func TestStartIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		tm.Start()
		defer tm.Stop()
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := fired.Load(); got != 1 {
			t.Fatalf("fired = %d, want 1 despite a double Start", got)
		}
	})
}

func TestDisabledTimerIsInert(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		for _, off := range []*idle.Timer{
			idle.New(0, func() { fired.Add(1) }),
			idle.New(time.Hour, nil),
			idle.New(-time.Hour, func() { fired.Add(1) }),
		} {
			if off.Enabled() {
				t.Error("Enabled() = true for a disabled timer")
			}
			off.Start()
			off.Touch()
			off.Hold()()
			if off.InFlight() != 0 {
				t.Errorf("a disabled timer tracked a hold: %d", off.InFlight())
			}
			time.Sleep(3 * time.Hour)
			synctest.Wait()
			off.Stop()
		}
		if got := fired.Load(); got != 0 {
			t.Fatalf("a disabled timer fired: %d", got)
		}
	})
}

func TestNilTimerAccessors(t *testing.T) {
	var tm *idle.Timer
	if tm.Enabled() || tm.Fired() || tm.InFlight() != 0 || tm.Timeout() != 0 {
		t.Error("nil Timer accessors must be zero-valued")
	}
	if !tm.LastActivity().IsZero() {
		t.Error("nil Timer LastActivity must be zero")
	}
	tm.Stop() // must not panic
}

func TestLastActivityAdvances(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm := idle.New(time.Hour, func() {})
		tm.Start()
		defer tm.Stop()
		first := tm.LastActivity()
		time.Sleep(10 * time.Minute)
		tm.Touch()
		if !tm.LastActivity().After(first) {
			t.Error("Touch did not advance LastActivity")
		}
	})
}

func TestMiddlewareHoldsAcrossRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fired atomic.Int64
		tm := idle.New(time.Hour, func() { fired.Add(1) })
		tm.Start()
		defer tm.Stop()

		h := tm.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(3 * time.Hour) // a very slow stream
			w.WriteHeader(http.StatusOK)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/messages", nil))

		synctest.Wait()
		if fired.Load() != 0 {
			t.Fatal("fired during a 3h request")
		}
		if tm.InFlight() != 0 {
			t.Errorf("InFlight = %d after the handler returned", tm.InFlight())
		}

		time.Sleep(61 * time.Minute)
		synctest.Wait()
		if got := fired.Load(); got != 1 {
			t.Fatalf("fired = %d, want 1 once the request finished", got)
		}
	})
}

func TestMiddlewareReleasesOnPanic(t *testing.T) {
	tm := idle.New(time.Hour, func() {})
	tm.Start()
	defer tm.Stop()

	h := tm.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if tm.InFlight() != 0 {
		t.Errorf("hold leaked through a panic: InFlight = %d", tm.InFlight())
	}
}

func TestMiddlewareDisabledPassesThrough(t *testing.T) {
	tm := idle.New(0, nil)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	w := httptest.NewRecorder()
	tm.Middleware(inner).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d", w.Code)
	}
}

// TestConcurrentActivity exercises the mutex under -race with the real clock.
func TestConcurrentActivity(t *testing.T) {
	var fired atomic.Int64
	tm := idle.New(50*time.Millisecond, func() { fired.Add(1) })
	tm.Start()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tm.Touch()
				release := tm.Hold()
				_ = tm.InFlight()
				_ = tm.LastActivity()
				_ = tm.Fired()
				release()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	tm.Stop()
	if tm.InFlight() != 0 {
		t.Errorf("InFlight = %d after every hold was released", tm.InFlight())
	}
}

// TestRealTimeFire is the belt-and-braces check that the production clock, not
// just synctest's, drives the callback. Loose bounds keep it non-flaky.
func TestRealTimeFire(t *testing.T) {
	done := make(chan struct{})
	var once sync.Once
	tm := idle.New(50*time.Millisecond, func() { once.Do(func() { close(done) }) })
	tm.Start()
	defer tm.Stop()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the idle callback never fired with the real clock")
	}
	if !tm.Fired() {
		t.Error("Fired() = false after the callback ran")
	}
}

// TestWithClock checks the injectable-clock seam works end to end.
func TestWithClock(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	readNow := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	var fired atomic.Int64
	tm := idle.New(time.Hour, func() { fired.Add(1) },
		idle.WithClock(readNow, time.AfterFunc),
		idle.WithClock(nil, nil), // nil funcs must leave the clock intact
	)
	tm.Start()
	defer tm.Stop()
	if got := tm.LastActivity(); !got.Equal(now) {
		t.Errorf("LastActivity = %s, want the injected clock %s", got, now)
	}
	mu.Lock()
	now = now.Add(30 * time.Minute)
	mu.Unlock()
	tm.Touch()
	if got := tm.LastActivity(); !got.Equal(readNow()) {
		t.Errorf("Touch did not use the injected clock: %s", got)
	}
	if fired.Load() != 0 {
		t.Error("fired without any real time passing")
	}
}
