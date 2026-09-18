package router

import (
	"errors"
	"log/slog"
	"net/http"
)

// The two conditions every leg must be able to report to the dispatcher, and
// which the dispatcher must never answer with an error envelope.
//
// They live here, in the vocabulary every leg shares, rather than in a
// backend's own package: all upstreams fail in the same two ways, and the
// dispatcher should not have to import one leg to understand another.
// internal/anthropic re-exports these under its original names, so errors.Is
// keeps matching either spelling.
var (
	// ErrResponseStarted wraps any failure that happens after the status line
	// and headers have gone out. A caller must not try to render an error
	// envelope on top of it — the bytes are already committed.
	ErrResponseStarted = errors.New("utraque: response already started")

	// ErrClientGone wraps a failure caused by the caller disconnecting.
	ErrClientGone = errors.New("utraque: client went away")
)

// AbortResponse tears down the caller's connection instead of letting net/http
// finish the response.
//
// It is the required answer to ErrResponseStarted. A handler that simply
// returns after a half-sent body lets net/http complete the response properly
// — on HTTP/1.1 that means writing the terminating zero-length chunk — which
// tells the caller it received the whole body. It did not. When the body is
// compressed (the Anthropic passthrough relays upstream's Content-Encoding
// byte-for-byte) the caller's decompressor then fails on the truncated stream
// and it reports data corruption; a dropped network link is diagnosed as a
// mangled response. Aborting instead closes the connection with no terminating
// chunk, so the caller sees a truncated transfer: a network fault, which is
// what it was, and which every HTTP client already knows how to retry.
//
// http.ErrAbortHandler is net/http's own signal for this and is suppressed by
// its panic logger. It must reach the server: internal/server's recover
// middleware re-panics it rather than swallowing it, and every middleware that
// has cleanup to do — the access-log line, the trace file, the idle-timer hold
// — does that cleanup in a defer, so unwinding past them loses nothing.
func AbortResponse() { panic(http.ErrAbortHandler) }

// Backend names one of the upstream legs a request can be sent to.
type Backend string

// The backends utraque can route to.
const (
	BackendAnthropic Backend = "anthropic"
	BackendCodex     Backend = "codex"
	BackendDeepSeek  Backend = "deepseek"
)

// String renders the backend name.
func (b Backend) String() string { return string(b) }

// Valid reports whether b is one of the known backends.
func (b Backend) Valid() bool {
	switch b {
	case BackendAnthropic, BackendCodex, BackendDeepSeek:
		return true
	default:
		return false
	}
}

// Effort provenance, highest precedence first. A Decision records which of
// these supplied its Effort so a later phase can apply the plan's precedence
// order (suffix > anthropic-beta > config > catalog) without re-deriving it.
const (
	EffortSourceSuffix  = "suffix"
	EffortSourceBeta    = "anthropic-beta"
	EffortSourceConfig  = "config"
	EffortSourceCatalog = "catalog"
	EffortSourceNone    = ""
)

// Decision is the routing verdict for one client-supplied model string.
//
// ClientModel is ordinarily the caller's model string, whitespace-trimmed,
// case preserved: ResolveWith trims on every path before deriving a
// Decision, so this is not a byte-exact copy of what the caller sent. The
// byte-exact spelling, when something needs it, is Request.Raw — the
// Anthropic leg forwards that untouched. The one exception is an
// Anthropic-backed picker route: resolvePicker substitutes the route's
// UpstreamModel into ClientModel there (see resolvePicker), so on that path
// ClientModel can differ from the caller's string by more than trimming.
// UpstreamModel is the slug the Codex leg should ask for, and is empty for
// the Anthropic backend, which does not rewrite the model.
type Decision struct {
	Backend       Backend
	UpstreamModel string
	ClientModel   string
	Effort        string
	EffortSource  string
}

// Request is one dispatched call: the raw body already read by the front
// door, the fields peeked out of it, the routing Decision, and the
// request-scoped logger.
type Request struct {
	Raw    []byte
	Model  string
	Stream bool
	Dec    Decision
	Log    *slog.Logger
}

// Leg is an upstream backend. A leg writes its own response; the error it
// returns is for the dispatcher's logging and error-envelope decision, and is
// non-nil only when something went wrong.
type Leg interface {
	Messages(w http.ResponseWriter, r *http.Request, rq *Request) error
	CountTokens(w http.ResponseWriter, r *http.Request, rq *Request) error
}
