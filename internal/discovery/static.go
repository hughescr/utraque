package discovery

import "github.com/hughescr/utraque/internal/anthropic"

// staticClaudeModels is the fallback Claude catalog: what utraque advertises
// when the upstream read is unavailable, refused, or too slow.
//
// The ids and display names are the first-party model ids and labels Claude
// Code itself ships in its built-in table, so a picker row served from here
// reads identically to one the client would have shown on its own. Older and
// retired models are deliberately omitted — a fallback list should offer what
// a user would actually pick today, not the full history.
//
// This list is a fallback, not a source of truth. Anything it gets wrong is
// corrected the moment an upstream read succeeds, and it is overridable via
// Options.StaticAnthropicModels.
var staticClaudeModels = []anthropic.CatalogModel{
	{ID: "claude-fable-5", DisplayName: "Fable 5", Type: modelType},
	{ID: "claude-opus-5", DisplayName: "Opus 5", Type: modelType},
	{ID: "claude-sonnet-5", DisplayName: "Sonnet 5", Type: modelType},
	{ID: "claude-haiku-4-5", DisplayName: "Haiku 4.5", Type: modelType},
}

// StaticAnthropicModels returns a copy of the built-in Claude fallback list.
func StaticAnthropicModels() []anthropic.CatalogModel {
	out := make([]anthropic.CatalogModel, len(staticClaudeModels))
	copy(out, staticClaudeModels)
	return out
}
