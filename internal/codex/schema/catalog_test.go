package schema_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/schema"
)

// sampleCache is structurally copied from the plan's documented
// models_cache.json shape ({client_version, etag, fetched_at, models[]}), with
// supported_reasoning_levels as an ARRAY OF OBJECTS. It is a fixture, not the
// real on-disk file.
const sampleCache = `{
  "fetched_at": "2026-08-23T21:31:40.672476Z",
  "etag": "W/\"88ec06819eef0168a374351aeec2bc6c\"",
  "client_version": "0.149.0",
  "models": [
    {
      "slug": "gpt-5.6-sol",
      "display_name": "GPT-5.6-Sol",
      "description": "Latest frontier agentic coding model.",
      "visibility": "list",
      "context_window": 400000,
      "default_reasoning_level": "low",
      "supported_reasoning_levels": [
        {"effort": "low", "description": "Fast."},
        {"effort": "medium", "description": "Balanced."},
        {"effort": "high", "description": "Deep."},
        {"effort": "max", "description": "Deeper."},
        {"effort": "ultra", "description": "Deepest."}
      ],
      "priority": 10
    },
    {
      "slug": "gpt-5.3-codex-spark",
      "display_name": "GPT-5.3-Codex-Spark",
      "visibility": "hide",
      "context_window": 272000,
      "default_reasoning_level": "medium",
      "supported_reasoning_levels": [
        {"effort": "medium"},
        {"effort": "high"}
      ],
      "priority": 3
    }
  ]
}`

func TestCacheUnmarshal(t *testing.T) {
	var c schema.Cache
	if err := json.Unmarshal([]byte(sampleCache), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if c.ClientVersion != "0.149.0" {
		t.Errorf("ClientVersion = %q, want 0.149.0", c.ClientVersion)
	}
	if c.ETag != `W/"88ec06819eef0168a374351aeec2bc6c"` {
		t.Errorf("ETag = %q", c.ETag)
	}
	want, _ := time.Parse(time.RFC3339Nano, "2026-08-23T21:31:40.672476Z")
	if !c.FetchedAt.Equal(want) {
		t.Errorf("FetchedAt = %v, want %v", c.FetchedAt, want)
	}
	if len(c.Models) != 2 {
		t.Fatalf("len(Models) = %d, want 2", len(c.Models))
	}

	sol := c.Models[0]
	if sol.Slug != "gpt-5.6-sol" || sol.DisplayName != "GPT-5.6-Sol" {
		t.Errorf("sol slug/display = %q/%q", sol.Slug, sol.DisplayName)
	}
	if sol.ContextWindow != 400000 {
		t.Errorf("sol ContextWindow = %d, want 400000", sol.ContextWindow)
	}
	if sol.DefaultReasoningLevel != "low" {
		t.Errorf("sol DefaultReasoningLevel = %q, want low", sol.DefaultReasoningLevel)
	}
	if sol.Priority != 10 {
		t.Errorf("sol Priority = %d, want 10", sol.Priority)
	}

	// supported_reasoning_levels must decode as objects with an Effort field.
	if got := sol.SupportedEfforts(); len(got) != 5 || got[0] != "low" || got[4] != "ultra" {
		t.Errorf("sol SupportedEfforts = %v, want [low medium high max ultra]", got)
	}
	if sol.SupportedReasoningLevels[2].Description != "Deep." {
		t.Errorf("sol level[2].Description = %q, want Deep.", sol.SupportedReasoningLevels[2].Description)
	}
	if !sol.SupportsEffort("ultra") || sol.SupportsEffort("xhigh") || sol.SupportsEffort("") {
		t.Errorf("sol SupportsEffort mismatch: ultra=%v xhigh=%v empty=%v",
			sol.SupportsEffort("ultra"), sol.SupportsEffort("xhigh"), sol.SupportsEffort(""))
	}
}

func TestListed(t *testing.T) {
	if !(schema.Model{Visibility: "list"}).Listed() {
		t.Error(`Visibility "list" should be Listed`)
	}
	// Fail closed: hidden and empty visibility are not advertised.
	if (schema.Model{Visibility: "hide"}).Listed() {
		t.Error(`Visibility "hide" should not be Listed`)
	}
	if (schema.Model{}).Listed() {
		t.Error("empty visibility should not be Listed")
	}
}

// TestModelsResponseUnmarshal covers the wire body shape (a bare "models"
// array without the on-disk metadata).
func TestModelsResponseUnmarshal(t *testing.T) {
	const body = `{"models":[{"slug":"gpt-5.5","visibility":"list","priority":2}]}`
	var r schema.ModelsResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Models) != 1 || r.Models[0].Slug != "gpt-5.5" {
		t.Fatalf("Models = %+v", r.Models)
	}
}
