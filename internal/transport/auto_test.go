package transport

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// discardLogger keeps the switch warning out of test output while still
// exercising the logging path.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// capturingLogger records the messages logged at WARN or above.
type capturingLogger struct {
	mu   sync.Mutex
	buf  strings.Builder
	slog *slog.Logger
}

func newCapturingLogger() *capturingLogger {
	c := &capturingLogger{}
	c.slog = slog.New(slog.NewTextHandler(&lockedWriter{c: c}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return c
}

func (c *capturingLogger) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type lockedWriter struct{ c *capturingLogger }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(p)
}

func TestNewSelectsByMode(t *testing.T) {
	cases := []struct {
		mode     string
		wantKind string
		wantAuto bool
	}{
		{ModeStd, KindStd, false},
		{ModeUTLS, KindUTLS, false},
		{ModeAuto, KindStd, true},
		// An empty mode is the default, and case/space must not matter: these
		// values arrive from an environment variable.
		{"", KindStd, true},
		{"  AUTO ", KindStd, true},
		{"UTLS", KindUTLS, false},
		{" Std", KindStd, false},
	}
	for _, tc := range cases {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			tr, err := New(tc.mode, DefaultOptions(), discardLogger())
			if err != nil {
				t.Fatalf("New(%q): %v", tc.mode, err)
			}
			if tr.Kind() != tc.wantKind {
				t.Errorf("Kind() = %q, want %q", tr.Kind(), tc.wantKind)
			}
			_, isGater := tr.(Gater)
			if isGater != tc.wantAuto {
				t.Errorf("implements Gater = %v, want %v", isGater, tc.wantAuto)
			}
		})
	}
}

func TestNewRejectsUnknownMode(t *testing.T) {
	// A typo must fail loudly at startup. Falling back to std would look
	// identical to a working uTLS switch, right up until the gate it was set
	// for actually appeared.
	tr, err := New("utsl", DefaultOptions(), discardLogger())
	if err == nil {
		t.Fatalf("New(\"utsl\") = %v, want an error", tr)
	}
	if !strings.Contains(err.Error(), "utsl") {
		t.Errorf("error %q does not name the offending value", err)
	}
}

// TestModeValuesAreStable pins the wire values of the mode strings. config
// validates the same enum without importing this package, so the two lists can
// only stay in step if both are pinned to literals.
func TestModeValuesAreStable(t *testing.T) {
	for got, want := range map[string]string{ModeStd: "std", ModeUTLS: "utls", ModeAuto: "auto"} {
		if got != want {
			t.Errorf("mode constant = %q, want %q", got, want)
		}
	}
	if KindStd != "std" || KindUTLS != "utls" {
		t.Errorf("kinds = %q/%q, want std/utls", KindStd, KindUTLS)
	}
}

func TestAutoStartsOnStdAndSwitchesOnce(t *testing.T) {
	log := newCapturingLogger()
	tr := NewAuto(DefaultOptions(), log.slog)

	if tr.Kind() != KindStd {
		t.Fatalf("Kind() = %q before any gate, want %q", tr.Kind(), KindStd)
	}
	if log.text() != "" {
		t.Errorf("something was logged before any gate: %q", log.text())
	}

	if !ReportGate(tr) {
		t.Fatal("the first ReportGate returned false, want true (it performed the switch)")
	}
	if tr.Kind() != KindUTLS {
		t.Errorf("Kind() = %q after a gate, want %q", tr.Kind(), KindUTLS)
	}

	// At most once, for the remainder of the process.
	for i := range 3 {
		if ReportGate(tr) {
			t.Errorf("ReportGate repeat %d returned true, want false", i)
		}
	}
	if tr.Kind() != KindUTLS {
		t.Errorf("Kind() = %q, want it to stay %q", tr.Kind(), KindUTLS)
	}

	got := log.text()
	if strings.Count(got, "GATE DETECTED") != 1 {
		t.Errorf("switch warning logged %d times, want exactly 1:\n%s", strings.Count(got, "GATE DETECTED"), got)
	}
	for _, want := range []string{"level=WARN", "uTLS", honestOriginator, "to=utls"} {
		if !strings.Contains(got, want) {
			t.Errorf("switch warning lacks %q:\n%s", want, got)
		}
	}
}

// TestAutoSwitchesExactlyOnceUnderRace is the -race case: many goroutines
// reporting the same gate at the same moment must produce exactly one switch.
func TestAutoSwitchesExactlyOnceUnderRace(t *testing.T) {
	tr := NewAuto(DefaultOptions(), discardLogger())

	const n = 64
	var switches atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ReportGate(tr) {
				switches.Add(1)
			}
			_ = tr.Kind()
		}()
	}
	close(start)
	wg.Wait()

	if got := switches.Load(); got != 1 {
		t.Errorf("%d goroutines performed the switch, want exactly 1", got)
	}
}

// TestAutoDoesNotSwitchWithoutAGate is the negative half of the contract: only
// a gate-class failure may change the fingerprint. It is asserted through the
// same Gater surface the responses client uses, so a caller that reported an
// auth failure or a 429 by mistake could not be silently tolerated here.
func TestAutoDoesNotSwitchWithoutAGate(t *testing.T) {
	tr := NewAuto(DefaultOptions(), discardLogger())

	// Traffic alone must not switch anything, however it fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	for range 3 {
		resp, err := tr.Client().Get(srv.URL)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if tr.Kind() != KindStd {
		t.Errorf("Kind() = %q after non-gate failures, want %q", tr.Kind(), KindStd)
	}
	if f, ok := tr.(interface{ Flipped() bool }); !ok || f.Flipped() {
		t.Error("the transport switched without a gate being reported")
	}
}

// TestReportGateIsANoOpOnFixedTransports: std and utls are explicit operator
// choices and nothing about a gate may override them.
func TestReportGateIsANoOpOnFixedTransports(t *testing.T) {
	for _, tr := range []Transport{NewStd(DefaultOptions()), NewUTLS(DefaultOptions())} {
		before := tr.Kind()
		if ReportGate(tr) {
			t.Errorf("%s transport reported a switch, want false", before)
		}
		if tr.Kind() != before {
			t.Errorf("%s transport changed Kind() to %q", before, tr.Kind())
		}
	}
}

func TestAutoClientIsStableAcrossTheSwitch(t *testing.T) {
	tr := NewAuto(DefaultOptions(), discardLogger())
	before := tr.Client()
	ReportGate(tr)
	if tr.Client() != before {
		t.Error("Client() changed identity across the switch; callers capture it once at construction")
	}
}

func TestAutoClientDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
	})
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := NewAuto(DefaultOptions(), discardLogger())

	// The policy has to hold on both sides of the switch: after a gate the
	// requests go out through a different stack entirely.
	for _, phase := range []string{"before", "after"} {
		resp, err := tr.Client().Get(srv.URL + "/start")
		if err != nil {
			t.Fatalf("%s: get: %v", phase, err)
		}
		if resp.StatusCode != http.StatusFound {
			t.Errorf("%s: status = %d, want 302", phase, resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "/target" {
			t.Errorf("%s: Location = %q, want /target", phase, got)
		}
		resp.Body.Close()
		ReportGate(tr)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target fetched %d time(s), want 0", n)
	}
}

// TestAutoRoutesThroughTheLiveImplementation proves the switch actually changes
// which stack carries a request, not just what Kind() says.
func TestAutoRoutesThroughTheLiveImplementation(t *testing.T) {
	auto, ok := NewAuto(DefaultOptions(), discardLogger()).(*autoTransport)
	if !ok {
		t.Fatal("NewAuto did not return *autoTransport")
	}
	if auto.active() != auto.std {
		t.Error("active() is not the std transport before a gate")
	}
	auto.ReportGate()
	if auto.active() != auto.utls {
		t.Error("active() is not the uTLS transport after a gate")
	}
}

// TestAutoOnFlipHookRunsOnce guards the test seam itself, which the
// responses-level test relies on to observe a switch.
func TestAutoOnFlipHookRunsOnce(t *testing.T) {
	auto := NewAuto(DefaultOptions(), discardLogger()).(*autoTransport)
	var calls atomic.Int64
	auto.onFlip = func() { calls.Add(1) }
	for range 5 {
		auto.ReportGate()
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("onFlip ran %d times, want 1", got)
	}
}
