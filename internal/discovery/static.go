package discovery

import "github.com/hughescr/utraque/internal/anthropic"

// staticClaudeModels is the fallback Claude catalog: what utraque advertises
// when the upstream read is unavailable, refused, or too slow.
//
// The ids and display names are the first-party model ids and labels Claude
// Code itself ships in its built-in table, so a picker row served from here
// reads identically to one the client would have shown on its own. Retired and
// pre-4.5 models are deliberately omitted — a fallback list should offer what a
// user would actually pick today, not the full history.
//
// This list is a fallback, not a source of truth. Anything it gets wrong is
// corrected the moment an upstream read succeeds, and it is overridable via
// Options.StaticAnthropicModels.
var staticClaudeModels = []anthropic.CatalogModel{
	{ID: "claude-opus-5", DisplayName: "Opus 5", Type: modelType},
	{ID: "claude-fable-5", DisplayName: "Fable 5", Type: modelType},
	{ID: "claude-sonnet-5", DisplayName: "Sonnet 5", Type: modelType},
	{ID: "claude-opus-4-8", DisplayName: "Opus 4.8", Type: modelType},
	{ID: "claude-opus-4-7", DisplayName: "Opus 4.7", Type: modelType},
	{ID: "claude-opus-4-6", DisplayName: "Opus 4.6", Type: modelType},
	{ID: "claude-sonnet-4-6", DisplayName: "Sonnet 4.6", Type: modelType},
	{ID: "claude-haiku-4-5", DisplayName: "Haiku 4.5", Type: modelType},
}

// StaticAnthropicModels returns a copy of the built-in Claude fallback list.
func StaticAnthropicModels() []anthropic.CatalogModel {
	out := make([]anthropic.CatalogModel, len(staticClaudeModels))
	copy(out, staticClaudeModels)
	return out
}

// defaultNativeOneMModels are the Claude model families whose *native* context
// window is 1M tokens, matched against a discovered id by prefix.
//
// Why utraque has to name them at all: Claude Code only offers the
// "(1M context)" picker entry for a model it can verify supports it, and behind
// a gateway base URL it cannot verify, so it offers nothing. The long window
// still works — see oneMSuffix — but only if something puts the row in front of
// the user. That is this list's job.
//
// It matches the native_1m set in Claude Code's own model table as of client
// 2.1.226. Override with Options.OneM.Models when that set moves.
var defaultNativeOneMModels = []string{
	"claude-sonnet-5",
	"claude-fable-5",
	"claude-opus-5",
	"claude-opus-4-7",
	"claude-opus-4-8",
}

// DefaultNativeOneMModels returns a copy of the built-in native-1M prefix list.
func DefaultNativeOneMModels() []string {
	out := make([]string, len(defaultNativeOneMModels))
	copy(out, defaultNativeOneMModels)
	return out
}
