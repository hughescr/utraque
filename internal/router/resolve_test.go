package router_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/router"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		beta    string
		want    router.Decision
		wantErr bool
	}{
		{name: "claude full slug passthrough", model: "claude-opus-4-20250514",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "claude-opus-4-20250514", EffortSource: router.EffortSourceNone}},
		{name: "claude sonnet slug passthrough", model: "claude-sonnet-4-5-20250929",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "claude-sonnet-4-5-20250929", EffortSource: router.EffortSourceNone}},
		{name: "claude case-insensitive", model: "CLAUDE-HAIKU-3-5",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "CLAUDE-HAIKU-3-5", EffortSource: router.EffortSourceNone}},
		{name: "anthropic bedrock-style id passthrough", model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", EffortSource: router.EffortSourceNone}},
		{name: "bare claude", model: "claude",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "claude", EffortSource: router.EffortSourceNone}},
		{name: "static anthropic shorthand opus", model: "opus",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "opus", EffortSource: router.EffortSourceNone}},
		{name: "static anthropic shorthand sonnet", model: "sonnet",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "sonnet", EffortSource: router.EffortSourceNone}},
		{name: "static anthropic shorthand haiku", model: "haiku",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "haiku", EffortSource: router.EffortSourceNone}},
		{name: "static anthropic shorthand fable", model: "fable",
			want: router.Decision{Backend: router.BackendAnthropic, ClientModel: "fable", EffortSource: router.EffortSourceNone}},
		{name: "bare codename sol", model: "sol",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "sol", EffortSource: router.EffortSourceNone}},
		{name: "bare codename terra", model: "terra",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-terra", ClientModel: "terra", EffortSource: router.EffortSourceNone}},
		{name: "bare codename luna", model: "luna",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-luna", ClientModel: "luna", EffortSource: router.EffortSourceNone}},
		{name: "bare codename spark (override-derived)", model: "spark",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.3-codex-spark", ClientModel: "spark", EffortSource: router.EffortSourceNone}},
		{name: "codename case-insensitive", model: "SOL",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "SOL", EffortSource: router.EffortSourceNone}},
		{name: "codename-version pinned", model: "sol-5.6",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "sol-5.6", EffortSource: router.EffortSourceNone}},
		{name: "effort suffix on bare codename", model: "sol-high",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "sol-high", Effort: "high", EffortSource: router.EffortSourceSuffix}},
		{name: "effort suffix on pinned codename-version", model: "sol-5.6-high",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "sol-5.6-high", Effort: "high", EffortSource: router.EffortSourceSuffix}},
		{name: "effort suffix on override-derived pinned spark", model: "spark-5.3-ultra",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.3-codex-spark", ClientModel: "spark-5.3-ultra", Effort: "ultra", EffortSource: router.EffortSourceSuffix}},
		{name: "raw slug sol", model: "gpt-5.6-sol",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "gpt-5.6-sol", EffortSource: router.EffortSourceNone}},
		{name: "raw slug terra", model: "gpt-5.6-terra",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-terra", ClientModel: "gpt-5.6-terra", EffortSource: router.EffortSourceNone}},
		{name: "raw slug luna", model: "gpt-5.6-luna",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-luna", ClientModel: "gpt-5.6-luna", EffortSource: router.EffortSourceNone}},
		{name: "version-only slug 5.5", model: "gpt-5.5",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.5", ClientModel: "gpt-5.5", EffortSource: router.EffortSourceNone}},
		{name: "version-only slug 5.4", model: "gpt-5.4",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.4", ClientModel: "gpt-5.4", EffortSource: router.EffortSourceNone}},
		{name: "version+modifier slug 5.4-mini not misparsed as effort", model: "gpt-5.4-mini",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.4-mini", ClientModel: "gpt-5.4-mini", EffortSource: router.EffortSourceNone}},
		{name: "bare version+modifier alias without gpt- prefix", model: "5.4-mini",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.4-mini", ClientModel: "5.4-mini", EffortSource: router.EffortSourceNone}},
		{name: "bare version alias without gpt- prefix", model: "5.5",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.5", ClientModel: "5.5", EffortSource: router.EffortSourceNone}},
		{name: "irregular raw slug codex-spark", model: "gpt-5.3-codex-spark",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.3-codex-spark", ClientModel: "gpt-5.3-codex-spark", EffortSource: router.EffortSourceNone}},
		{name: "picker variant bare", model: "anthropic-compat.sol",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "anthropic-compat.sol", EffortSource: router.EffortSourceNone}},
		{name: "picker variant pinned", model: "anthropic-compat.sol-5.6",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "anthropic-compat.sol-5.6", EffortSource: router.EffortSourceNone}},
		{name: "picker variant with effort suffix", model: "anthropic-compat.sol-high",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "anthropic-compat.sol-high", Effort: "high", EffortSource: router.EffortSourceSuffix}},
		{name: "picker variant mixed case", model: "Anthropic-Compat.SOL-High",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "Anthropic-Compat.SOL-High", Effort: "high", EffortSource: router.EffortSourceSuffix}},
		{name: "unknown gpt-* slug falls through to raw codex passthrough", model: "gpt-6.0-nova",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-6.0-nova", ClientModel: "gpt-6.0-nova", EffortSource: router.EffortSourceNone}},
		{name: "unknown gpt-* slug with effort suffix and mixed case", model: "GPT-6.0-Nova-high",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-6.0-nova", ClientModel: "GPT-6.0-Nova-high", Effort: "high", EffortSource: router.EffortSourceSuffix}},
		{name: "whitespace trimmed", model: "  sol  ",
			want: router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", ClientModel: "sol", EffortSource: router.EffortSourceNone}},
		{name: "unknown model", model: "banana", wantErr: true},
		{name: "empty model", model: "", wantErr: true},
		{name: "whitespace-only model", model: "   ", wantErr: true},
		{name: "gpt without trailing hyphen is not gpt-* family", model: "gpt", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := router.Resolve(tc.model, tc.beta)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q): expected error, got Decision %+v", tc.model, got)
				}
				var apiErr *apierr.Error
				if !errors.As(err, &apiErr) {
					t.Fatalf("Resolve(%q): error %v is not *apierr.Error", tc.model, err)
				}
				if apiErr.HTTPStatus() != http.StatusNotFound {
					t.Fatalf("Resolve(%q): HTTPStatus = %d, want %d", tc.model, apiErr.HTTPStatus(), http.StatusNotFound)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): unexpected error: %v", tc.model, err)
			}
			if got != tc.want {
				t.Fatalf("Resolve(%q) = %+v, want %+v", tc.model, got, tc.want)
			}
		})
	}
}

// TestRegistryLoadCatalogCollision exercises the Phase 3 hook now: when two
// versions carry the same codename, the newer version wins the bare
// (rolling) alias while both keep their own pinned alias.
func TestRegistryLoadCatalogCollision(t *testing.T) {
	r := router.NewRegistry()
	r.LoadCatalog([]router.CatalogEntry{
		{Slug: "gpt-5.6-sol"},
		{Slug: "gpt-5.7-sol"},
	})

	if u, ok := r.Resolve("sol"); !ok || u != "gpt-5.7-sol" {
		t.Fatalf(`Resolve("sol") = (%q, %v), want ("gpt-5.7-sol", true)`, u, ok)
	}
	if u, ok := r.Resolve("sol-5.6"); !ok || u != "gpt-5.6-sol" {
		t.Fatalf(`Resolve("sol-5.6") = (%q, %v), want ("gpt-5.6-sol", true)`, u, ok)
	}
	if u, ok := r.Resolve("sol-5.7"); !ok || u != "gpt-5.7-sol" {
		t.Fatalf(`Resolve("sol-5.7") = (%q, %v), want ("gpt-5.7-sol", true)`, u, ok)
	}
	if u, ok := r.Resolve("gpt-5.6-sol"); !ok || u != "gpt-5.6-sol" {
		t.Fatalf(`Resolve("gpt-5.6-sol") = (%q, %v), want ("gpt-5.6-sol", true)`, u, ok)
	}
}

// TestResolveClaudeCodeModelAliases pins the Anthropic leg against a hard
// local 404. These are all values Claude Code accepts for ANTHROPIC_MODEL and
// can put in the model field; 404ing a Claude model on the officially
// supported leg is worse than forwarding a name Anthropic will reject itself.
func TestResolveClaudeCodeModelAliases(t *testing.T) {
	for _, model := range []string{
		"opusplan", "opus[1m]", "sonnet[1m]", "opus-high", "sonnet-4-5", "haiku4", "Fable[1m]",
	} {
		t.Run(model, func(t *testing.T) {
			got, err := router.Resolve(model, "")
			if err != nil {
				t.Fatalf("Resolve(%q): %v", model, err)
			}
			if got.Backend != router.BackendAnthropic {
				t.Errorf("Resolve(%q).Backend = %v, want Anthropic", model, got.Backend)
			}
			if got.ClientModel != model {
				t.Errorf("Resolve(%q).ClientModel = %q, want the caller's exact bytes", model, got.ClientModel)
			}
		})
	}
}

// TestResolveCatalogSlugEndingInAnEffortWord: a slug the catalog actually
// serves must resolve to itself. Splitting the effort suffix first would send
// a different — and nonexistent — model upstream, silently.
func TestResolveCatalogSlugEndingInAnEffortWord(t *testing.T) {
	saved := router.DefaultRegistry
	t.Cleanup(func() { router.DefaultRegistry = saved })

	r := router.NewRegistry()
	r.LoadCatalog([]router.CatalogEntry{{Slug: "gpt-5.7-max"}, {Slug: "gpt-5.6-sol"}})
	router.DefaultRegistry = r

	got, err := router.Resolve("gpt-5.7-max", "")
	if err != nil {
		t.Fatalf(`Resolve("gpt-5.7-max"): %v`, err)
	}
	if got.UpstreamModel != "gpt-5.7-max" {
		t.Errorf("UpstreamModel = %q, want gpt-5.7-max (the exact slug the catalog serves)", got.UpstreamModel)
	}
	if got.Effort != "" {
		t.Errorf("Effort = %q, want none: the trailing token is part of the slug", got.Effort)
	}

	// The bare alias derived from that slug behaves the same way.
	bare, err := router.Resolve("max", "")
	if err != nil {
		t.Fatalf(`Resolve("max"): %v`, err)
	}
	if bare.UpstreamModel != "gpt-5.7-max" || bare.Effort != "" {
		t.Errorf(`Resolve("max") = %+v, want gpt-5.7-max with no effort`, bare)
	}

	// And a genuine effort suffix on a real alias still parses.
	suffixed, err := router.Resolve("sol-high", "")
	if err != nil {
		t.Fatalf(`Resolve("sol-high"): %v`, err)
	}
	if suffixed.UpstreamModel != "gpt-5.6-sol" || suffixed.Effort != "high" {
		t.Errorf(`Resolve("sol-high") = %+v, want gpt-5.6-sol with effort high`, suffixed)
	}
}
