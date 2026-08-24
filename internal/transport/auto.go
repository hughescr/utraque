package transport

import (
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
)

// Gater is implemented by a Transport that can react to an upstream bot/TLS
// gate. Only the auto transport does; std and utls are fixed choices and
// nothing about a gate should change what they do.
type Gater interface {
	// ReportGate records that the upstream answered a bot/TLS challenge rather
	// than the API. It reports whether THIS call performed the switch, so the
	// caller can tell a first detection from a repeat.
	//
	// It is safe to call concurrently and from any goroutine.
	ReportGate() bool
}

// ReportGate tells tr about an upstream bot/TLS gate, and reports whether that
// flipped it onto the uTLS transport. A transport that cannot switch — std,
// utls, or a test double — takes no action and returns false, so callers need
// no type switch of their own.
func ReportGate(tr Transport) bool {
	g, ok := tr.(Gater)
	return ok && g.ReportGate()
}

// autoTransport starts on std and switches to uTLS the first time a gate is
// reported, for the remainder of the process.
//
// The switch is one-way and one-shot on purpose. Flapping between fingerprints
// would make every failure ambiguous ("which stack was that?") and would
// re-open the possibility of a request going out on the fingerprint that was
// just proven to be blocked. Recovering from a false positive costs a restart,
// which is the cheap direction to be wrong in.
//
// autoTransport is itself the RoundTripper behind its own Client, so the flip
// takes effect on the single *http.Client every caller already holds. Callers
// capture Client() once at construction; handing them a new client later would
// not reach them.
type autoTransport struct {
	std  Transport
	utls Transport

	// flipped is read on every request, so it is an atomic rather than a
	// mutex read. once is what makes the switch happen exactly once even
	// when several in-flight requests are gated at the same moment.
	flipped atomic.Bool
	once    sync.Once

	client *http.Client
	log    *slog.Logger

	// onFlip, when set, runs after a switch. It exists for tests; production
	// leaves it nil.
	onFlip func()
}

func (t *autoTransport) Client() *http.Client { return t.client }

// Kind reports the fingerprint currently in use — KindStd before a gate has
// been seen, KindUTLS after. It is what the request log records, so it must
// name the stack that actually carried the request, not the configured mode.
func (t *autoTransport) Kind() string {
	if t.flipped.Load() {
		return KindUTLS
	}
	return KindStd
}

// Flipped reports whether the switch has happened. /healthz and tests read it;
// nothing branches on it inside the request path.
func (t *autoTransport) Flipped() bool { return t.flipped.Load() }

func (t *autoTransport) ReportGate() bool {
	switched := false
	t.once.Do(func() {
		t.flipped.Store(true)
		switched = true
	})
	if !switched {
		return false
	}
	t.log.Warn("UPSTREAM BOT/TLS GATE DETECTED: switching to the uTLS (Chrome TLS fingerprint) transport for the rest of this process. "+
		"Only the TLS handshake changes — the request identity stays honest (originator "+honestOriginator+", no forged browser headers). "+
		"If this did not fix the gate, the block is not fingerprint-based; restart to return to the standard transport",
		slog.String("from", KindStd),
		slog.String("to", KindUTLS),
	)
	if t.onFlip != nil {
		t.onFlip()
	}
	return true
}

// RoundTrip dispatches to whichever implementation is live. It reads the
// underlying transports' RoundTrippers rather than their Clients, so the
// no-redirect policy that matters is the one on autoTransport's own client —
// which every path goes through.
func (t *autoTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.active().Client().Transport.RoundTrip(req)
}

func (t *autoTransport) active() Transport {
	if t.flipped.Load() {
		return t.utls
	}
	return t.std
}

// CloseIdleConnections drains both pools: the std pool may still hold
// connections after a flip.
func (t *autoTransport) CloseIdleConnections() {
	t.std.Client().CloseIdleConnections()
	t.utls.Client().CloseIdleConnections()
}

// NewAuto builds the self-switching transport: std until a gate is reported
// through ReportGate, uTLS from then on.
//
// Both implementations are constructed up front. Building the uTLS one costs
// nothing until it dials, and doing it here means the switch cannot fail at
// the one moment the proxy is already in trouble.
//
// log may be nil, in which case slog.Default is used.
func NewAuto(opts Options, log *slog.Logger) Transport {
	if log == nil {
		log = slog.Default()
	}
	t := &autoTransport{
		std:  NewStd(opts),
		utls: NewUTLS(opts),
		log:  log,
	}
	t.client = &http.Client{
		Transport:     t,
		CheckRedirect: noRedirect,
	}
	return t
}
