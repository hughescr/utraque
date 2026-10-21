// Package effort names the reasoning-effort levels utraque recognises and the
// places a request's effort can come from. It is the one home for that
// vocabulary, shared by the router's suffix grammar, the request translator's
// clamp and the discovery picker, so adding a level is one edit rather than a
// coordinated change across three packages.
//
// The recognised set is closed: Known reports membership and Rank gives the
// order the translator clamps against. The set the Codex catalog advertises
// per model is NOT closed — upstream may add a level before utraque learns its
// name — so the catalog's own tokens stay raw strings in internal/codex/schema
// and are converted to a Level only at the boundary. Clamp tolerates an
// unknown token on either side rather than rejecting it, and never lets one
// buy a higher tier than it names.
//
// Dependency contract: effort imports only the standard library, so any
// package in the tree may import it without creating a cycle.
package effort

import "strings"

// Level is one reasoning-effort level. Its string value is the wire spelling
// the Responses API accepts in reasoning.effort and the catalog uses in
// supported_reasoning_levels, and the suffix token the router splits off a
// model name ("sol-high").
type Level string

// The recognised levels, lowest to highest. This is the canonical order the
// request translator clamps against; it mirrors the catalog's
// supported_reasoning_levels ordering.
const (
	Low    Level = "low"
	Medium Level = "medium"
	High   Level = "high"
	XHigh  Level = "xhigh"
	Max    Level = "max"
	Ultra  Level = "ultra"
)

// levels lists the recognised levels in rank order. Rank is the index.
var levels = [...]Level{Low, Medium, High, XHigh, Max, Ultra}

// String renders the wire spelling.
func (l Level) String() string { return string(l) }

// Known reports whether l is one of the recognised levels. The comparison is
// exact: callers that accept mixed case lower the token first.
func Known(l Level) bool { return Rank(l) >= 0 }

// Rank returns l's position in the canonical order, 0 for the lowest level,
// or -1 for a level that is not recognised. An unrecognised token ranks below
// every recognised one on purpose: a config or header typo ("hgh") must never
// silently buy the most expensive reasoning tier.
func Rank(l Level) int {
	for i, k := range levels {
		if k == l {
			return i
		}
	}
	return -1
}

// Clamp fits requested to the set of levels a model supports. If the model
// supports it verbatim, it passes unchanged; otherwise it is clamped DOWN to
// the highest supported level not exceeding the request or, if the request
// sits below every supported level, UP to the lowest supported one. An empty
// request, or an empty supported set (no catalog data to clamp against),
// leaves the request unchanged. Returns the applied level and whether clamping
// changed it.
//
// supported is the model's raw catalog tokens, in catalog order; a token the
// canonical order does not know is skipped rather than ranked, and if every
// supported token is unknown the request passes through unchanged.
func Clamp(requested Level, supported []string) (applied Level, clamped bool) {
	if requested == "" || len(supported) == 0 {
		return requested, false
	}
	for _, s := range supported {
		if s == string(requested) {
			return requested, false
		}
	}

	reqRank := Rank(requested)
	bestBelow, bestBelowRank := Level(""), -1
	lowest, lowestRank := Level(""), len(levels)+1
	for _, s := range supported {
		l := Level(s)
		r := Rank(l)
		if r < 0 {
			continue
		}
		if r < lowestRank {
			lowest, lowestRank = l, r
		}
		if r <= reqRank && r > bestBelowRank {
			bestBelow, bestBelowRank = l, r
		}
	}
	if bestBelow != "" {
		return bestBelow, true
	}
	if lowest != "" {
		return lowest, true // request below all supported; clamp up to lowest
	}
	return requested, false // supported levels were all unknown tokens
}

// Parse reads an effort token off the wire (a request body's
// output_config.effort) and reports whether it is a level utraque recognises.
// It trims and lowercases first, so "High" parses; a token outside the
// recognised set reports ok=false and the caller treats the request as having
// named no effort, rather than guessing a tier for a typo.
func Parse(token string) (Level, bool) {
	l := Level(strings.ToLower(strings.TrimSpace(token)))
	if !Known(l) {
		return "", false
	}
	return l, true
}

// Source names where a request's effort came from. A routing Decision records
// which of these supplied its Level so the translator can apply the precedence
// order (suffix > body > anthropic-beta > config > catalog) without
// re-deriving it. Its string value is what the debug "routed" record's
// "effort_source" field prints; LogName is the coarser spelling the
// per-request line uses.
type Source string

// Effort provenance, highest precedence first.
const (
	// SourceSuffix is a "-<level>" suffix on the client's model name.
	SourceSuffix Source = "suffix"
	// SourceBody is the request body's output_config.effort, which is where
	// Claude Code puts the effort its /effort setting or a subagent's
	// frontmatter chose. It sits below an explicit model-name suffix.
	SourceBody Source = "body"
	// SourceBeta is the anthropic-beta header's effort signal.
	SourceBeta Source = "anthropic-beta"
	// SourceConfig is the per-model effort override in utraque's config.
	SourceConfig Source = "config"
	// SourceCatalog is the model's default_reasoning_level from the catalog.
	SourceCatalog Source = "catalog"
	// SourceNone means nothing supplied an effort.
	SourceNone Source = ""
)

// String renders the provenance name.
func (s Source) String() string { return string(s) }

// LogName is the provenance as the per-request line prints it: "suffix",
// "body", "anthropic-beta" or "config" when the client or operator named an
// effort, and "default" when nothing did (the catalog default, or no effort at
// all).
func (s Source) LogName() string {
	switch s {
	case SourceSuffix, SourceBody, SourceBeta, SourceConfig:
		return string(s)
	default:
		return "default"
	}
}
