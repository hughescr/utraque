package router

import (
	"strings"

	"github.com/hughescr/utraque/internal/apierr"
)

// anthropicFamilies are non-"claude"/"anthropic"-prefixed shorthands that
// still route to the Anthropic leg. This is a Phase 1 placeholder for
// whatever bare picker/agent-frontmatter names actually reach the proxy
// unprefixed (model-selection's existing Claude route vocabulary); extend
// or empty it once real traffic shows what's needed.
//
// They are matched as PREFIXES, not exact names. Claude Code accepts a family
// of decorated forms built on these words — "opusplan", "opus[1m]",
// "sonnet[1m]", "opus-high" — and any of them can reach the proxy in the
// model field. Matching exactly would hard-404 a Claude model on the
// officially supported leg, which is a worse failure than forwarding a name
// Anthropic itself will reject authoritatively.
var anthropicFamilies = []string{"opus", "sonnet", "haiku", "fable"}

// isAnthropicName reports whether an already-lowercased model name belongs to
// the Anthropic leg.
func isAnthropicName(lower string) bool {
	if strings.HasPrefix(lower, "claude") || strings.HasPrefix(lower, "anthropic") {
		return true
	}
	for _, f := range anthropicFamilies {
		if strings.HasPrefix(lower, f) {
			return true
		}
	}
	return false
}

// anthropicCompatPrefix namespaces the filter-passing picker variants of
// Codex aliases (see the plan's "Alias generation" section):
// "anthropic-compat.sol" must resolve as the Codex alias "sol", never as an
// Anthropic model — even though it contains the substring "anthropic".
// This is why the prefix check runs before the generic Anthropic-name
// check in Resolve.
const anthropicCompatPrefix = "anthropic-compat."

// effortLevels are the suffix tokens ParseEffortSuffix recognises as
// reasoning-effort levels, drawn from the levels named across the plan
// (sol up to ultra, luna up to max, gpt-5.4 up to xhigh).
//
// Whether a *specific* resolved model actually supports a given level is
// NOT checked here.
//
// TODO(phase 3/4): once the catalog carries per-model
// supported_reasoning_levels, validate/clamp the parsed level against the
// resolved model's supported set instead of accepting any of these
// unconditionally for any model.
var effortLevels = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"max":    true,
	"ultra":  true,
	"xhigh":  true,
}

// ParseEffortSuffix splits a trailing "-<level>" reasoning-effort suffix
// off name, e.g. "sol-high" -> ("sol", "high", true) and
// "sol-5.6-high" -> ("sol-5.6", "high", true). name is expected to already
// be lowercased. Returns ok=false (base=name) when there's no hyphen or the
// trailing token isn't a recognised effort level — this is what keeps
// "gpt-5.4-mini" from being misparsed as base "gpt-5.4" + bogus effort
// "mini".
func ParseEffortSuffix(name string) (base string, effort string, ok bool) {
	idx := strings.LastIndex(name, "-")
	if idx < 0 {
		return name, "", false
	}
	suffix := name[idx+1:]
	if !effortLevels[suffix] {
		return name, "", false
	}
	return name[:idx], suffix, true
}

// DefaultRegistry is the process-wide alias registry Resolve uses. It's a
// package-level var (not embedded as a constant table) precisely so Phase 3
// can call DefaultRegistry.LoadCatalog(...) to swap in live catalog data
// without changing Resolve's signature or any caller.
var DefaultRegistry = NewStaticRegistry()

// Resolve maps a client-supplied model string (and, in a future phase, the
// anthropic-beta header's effort/capability signal) to a routing Decision.
//
// betaHeader is threaded through now so this signature doesn't change when
// Phase 3/4 adds EffortSourceBeta precedence; it is unused today.
//
// TODO(phase 3/4): parse betaHeader for an effort/capability signal and
// apply it at EffortSourceBeta precedence (below suffix, above config/
// catalog, per the plan's stated precedence order) once that header
// convention is decided.
func Resolve(model string, betaHeader string) (Decision, error) {
	_ = betaHeader // TODO(phase 3/4): see doc comment above.

	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)

	if lower == "" {
		return Decision{}, unknownModelError(trimmed)
	}

	// "anthropic-compat.*" is the Codex picker-variant namespace: it must
	// never match the generic Anthropic-name check below, even though it
	// contains "anthropic". Strip it and resolve the remainder as a Codex
	// alias only — this namespace never means "route to Anthropic".
	if rest, isCompat := strings.CutPrefix(lower, anthropicCompatPrefix); isCompat {
		if dec, ok := resolveCodex(rest, trimmed); ok {
			return dec, nil
		}
		return Decision{}, unknownModelError(trimmed)
	}

	if isAnthropicName(lower) {
		return Decision{
			Backend:      BackendAnthropic,
			ClientModel:  trimmed,
			EffortSource: EffortSourceNone,
		}, nil
	}

	if dec, ok := resolveCodex(lower, trimmed); ok {
		return dec, nil
	}

	// Generic "gpt-*" fallback: not a known alias yet, but shaped like a
	// Codex slug, so route it there as raw-slug passthrough. Phase 3's live
	// catalog will confirm or reject it upstream; router doesn't validate
	// existence beyond the registry it has today.
	if strings.HasPrefix(lower, "gpt-") {
		base, effort, hasEffort := ParseEffortSuffix(lower)
		dec := Decision{
			Backend:       BackendCodex,
			UpstreamModel: base,
			ClientModel:   trimmed,
		}
		if hasEffort {
			dec.Effort = effort
			dec.EffortSource = EffortSourceSuffix
		}
		return dec, nil
	}

	return Decision{}, unknownModelError(trimmed)
}

// resolveCodex resolves a (already-lowercased, "anthropic-compat."-stripped)
// name against DefaultRegistry, after stripping any effort suffix. ok=false
// means no registry match — callers fall through to the generic "gpt-*"
// passthrough or to the unknown-model error.
func resolveCodex(lower string, clientModel string) (Decision, bool) {
	// The whole name is tried against the registry first. A catalog slug whose
	// own last token happens to be an effort word ("gpt-5.7-max") must resolve
	// to itself, not be split into a different — and nonexistent — model with
	// an effort of "max". A name the catalog serves verbatim is never a
	// suffixed form of something else.
	if upstream, ok := DefaultRegistry.Resolve(lower); ok {
		return Decision{
			Backend:       BackendCodex,
			UpstreamModel: upstream,
			ClientModel:   clientModel,
		}, true
	}

	base, effort, hasEffort := ParseEffortSuffix(lower)
	if !hasEffort {
		// base == lower, which the lookup above already rejected.
		return Decision{}, false
	}

	upstream, ok := DefaultRegistry.Resolve(base)
	if !ok {
		return Decision{}, false
	}

	return Decision{
		Backend:       BackendCodex,
		UpstreamModel: upstream,
		ClientModel:   clientModel,
		Effort:        effort,
		EffortSource:  EffortSourceSuffix,
	}, true
}

// unknownModelError builds the 404 Anthropic-shaped error main renders for
// a model Resolve couldn't place in any backend, listing the known route
// families so the caller can see what would have worked.
func unknownModelError(model string) error {
	families := append([]string{"claude-*", "anthropic-*", "gpt-*"}, DefaultRegistry.Families()...)
	return apierr.UnknownModel(model, families)
}
