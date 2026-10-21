package router_test

import (
	"testing"

	"github.com/hughescr/utraque/internal/effort"
	"github.com/hughescr/utraque/internal/router"
)

// TestWithBodyEffort pins the router half of the effort precedence: an explicit
// model-name suffix beats the request body's output_config.effort, the body
// beats nothing, and the Anthropic leg is never touched.
func TestWithBodyEffort(t *testing.T) {
	bare := func(name string) router.Decision {
		dec, err := router.Resolve(name, "")
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		return dec
	}

	cases := []struct {
		name       string
		dec        router.Decision
		body       string
		wantEffort effort.Level
		wantSource effort.Source
	}{
		{"codex bare alias takes the body", bare("sol"), "high", effort.High, router.EffortSourceBody},
		{"codex suffix beats the body", bare("sol-medium"), "max", effort.Medium, router.EffortSourceSuffix},
		{"deepseek bare model takes the body", bare("deepseek-flash"), "xhigh", effort.XHigh, router.EffortSourceBody},
		{"body is case-insensitive", bare("sol"), " Max", effort.Max, router.EffortSourceBody},
		{"no body effort leaves the decision alone", bare("sol"), "", "", router.EffortSourceNone},
		{"unrecognised body effort is ignored", bare("sol"), "hgh", "", router.EffortSourceNone},
		{"anthropic leg is never touched", bare("claude-sonnet-4-5"), "high", "", router.EffortSourceNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.dec.WithBodyEffort(c.body)
			if got.Effort != c.wantEffort || got.EffortSource != c.wantSource {
				t.Errorf("effort = (%q, %q), want (%q, %q)", got.Effort, got.EffortSource, c.wantEffort, c.wantSource)
			}
			if got.Backend != c.dec.Backend || got.UpstreamModel != c.dec.UpstreamModel || got.ClientModel != c.dec.ClientModel {
				t.Errorf("WithBodyEffort changed more than the effort: %+v -> %+v", c.dec, got)
			}
		})
	}
}
