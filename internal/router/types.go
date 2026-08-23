package router

import (
	"log/slog"
	"net/http"
)

// Backend names one of the two upstream legs a request can be sent to.
type Backend string

// The backends utraque can route to.
const (
	BackendAnthropic Backend = "anthropic"
	BackendCodex     Backend = "codex"
)

// String renders the backend name.
func (b Backend) String() string { return string(b) }

// Valid reports whether b is one of the known backends.
func (b Backend) Valid() bool {
	switch b {
	case BackendAnthropic, BackendCodex:
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
// ClientModel is preserved exactly as the caller wrote it (case included) so
// the Anthropic leg can forward it byte-for-byte. UpstreamModel is the slug the
// Codex leg should ask for, and is empty for the Anthropic backend, which does
// not rewrite the model.
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
