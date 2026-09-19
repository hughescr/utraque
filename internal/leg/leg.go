// Package leg names the upstream legs: the backends utraque can dispatch a
// request to. It is the one home for that identity, shared by the router's
// Decision, the provider-quota Observation, usage history's attribution and
// the provider report, so the compiler can relate a routing verdict to a
// quota reading instead of each package comparing its own string literal.
//
// A leg is one upstream backend with its own credential, transport and
// accounting: Anthropic, Codex or DeepSeek. Unknown is not a leg; it exists
// only so usage history can say "attribution failed" with the same wire value
// it uses today, and Valid excludes it.
//
// The "route" in internal/obs is a superset of this set: an access line's
// route names a leg or "discovery", so obs keeps it as a plain string and
// this package is not imported there.
//
// Dependency contract: leg imports only the standard library, so any package
// in the tree may import it without creating a cycle.
package leg

// ID names one upstream leg. Its string value is the wire spelling used in
// the provider report's "provider" and "source" keys and in the debug log's
// backend field.
type ID string

// The legs utraque can dispatch to, plus the not-a-leg value usage history
// records when a model name matches none of them.
const (
	// Anthropic is the Anthropic Messages passthrough leg.
	Anthropic ID = "anthropic"
	// Codex is the ChatGPT-subscription (Codex) leg.
	Codex ID = "codex"
	// DeepSeek is the DeepSeek Anthropic-compatible leg.
	DeepSeek ID = "deepseek"
	// Unknown is recorded when attribution fails. It is never routable: Valid
	// returns false for it, and the router's picker tier drops routes that
	// carry it.
	Unknown ID = "unknown"
)

// String renders the leg name.
func (id ID) String() string { return string(id) }

// Valid reports whether id is one of the three routable legs. Unknown, the
// empty string and any other spelling are not.
func (id ID) Valid() bool {
	switch id {
	case Anthropic, Codex, DeepSeek:
		return true
	default:
		return false
	}
}
