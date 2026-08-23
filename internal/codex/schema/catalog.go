// Package schema holds the wire and on-disk types for the Codex backend.
// It is a pure type package: it imports nothing beyond the standard library so
// the catalog client, the request translator, and the stream translator can all
// share these definitions without risking an import cycle.
//
// This file covers the model-catalog shapes (GET /models and the interoperable
// on-disk cache). The Responses API request/response types land alongside it in
// a later phase.
package schema

import "time"

// Visibility values the catalog uses to say whether a model should be offered
// to users. Only VisibilityList models are surfaced by default; everything else
// is reachable by raw slug but not advertised.
const (
	VisibilityList = "list"
	VisibilityHide = "hide"
)

// ReasoningLevel is one entry of a model's supported_reasoning_levels array.
// The catalog encodes these as OBJECTS ({effort, description}), not bare
// strings, so callers must read Effort rather than treating the element as a
// scalar.
type ReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description,omitempty"`
}

// Model is one entry in the Codex model catalog. Unknown fields are ignored on
// decode; the fields here are the ones utraque routes and reasons about.
type Model struct {
	Slug                     string           `json:"slug"`
	DisplayName              string           `json:"display_name,omitempty"`
	Description              string           `json:"description,omitempty"`
	Visibility               string           `json:"visibility,omitempty"`
	ContextWindow            int              `json:"context_window,omitempty"`
	DefaultReasoningLevel    string           `json:"default_reasoning_level,omitempty"`
	DefaultReasoningSummary  string           `json:"default_reasoning_summary,omitempty"`
	SupportedReasoningLevels []ReasoningLevel `json:"supported_reasoning_levels,omitempty"`
	Priority                 int              `json:"priority,omitempty"`
}

// Listed reports whether the model should be advertised by default. A model is
// listed only when its visibility is exactly VisibilityList; an empty or
// unknown visibility is treated as not-listed (fail closed — never advertise a
// model the catalog did not explicitly mark for listing).
func (m Model) Listed() bool { return m.Visibility == VisibilityList }

// SupportedEfforts returns the effort levels this model accepts, in catalog
// order. It flattens the object array into the effort strings callers clamp
// against.
func (m Model) SupportedEfforts() []string {
	if len(m.SupportedReasoningLevels) == 0 {
		return nil
	}
	out := make([]string, 0, len(m.SupportedReasoningLevels))
	for _, l := range m.SupportedReasoningLevels {
		if l.Effort != "" {
			out = append(out, l.Effort)
		}
	}
	return out
}

// SupportsEffort reports whether level is one of the model's supported
// reasoning efforts. An empty level, or a model that declares no levels,
// reports false.
func (m Model) SupportsEffort(level string) bool {
	if level == "" {
		return false
	}
	for _, l := range m.SupportedReasoningLevels {
		if l.Effort == level {
			return true
		}
	}
	return false
}

// ModelsResponse is the body of GET {base}/models: a single "models" array.
// It is deliberately a distinct type from Cache so the on-disk metadata
// (etag/fetched_at/client_version) never leaks into what we treat as a live
// wire response.
type ModelsResponse struct {
	Models []Model `json:"models"`
}

// Cache is the on-disk catalog shape: the model list plus the metadata needed
// to revalidate it. Its field set and JSON tags match the Codex CLI's own
// models_cache.json ({client_version, etag, fetched_at, models}), so a file
// utraque writes is structurally interoperable with the CLI's cache. utraque
// still keeps its OWN cache file by default and never overwrites the CLI's.
type Cache struct {
	ClientVersion string    `json:"client_version,omitempty"`
	ETag          string    `json:"etag,omitempty"`
	FetchedAt     time.Time `json:"fetched_at"`
	Models        []Model   `json:"models"`
}
