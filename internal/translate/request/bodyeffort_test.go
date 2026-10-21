package request_test

import (
	"testing"

	aschema "github.com/hughescr/utraque/internal/anthropic/schema"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/effort"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/translate/request"
)

// TestEffortFromBody covers the body (output_config.effort) rung of the
// precedence: it beats the config/catalog defaults, loses to a model-name
// suffix, and is clamped to the model's catalog like any other requested
// effort. Claude Code sends low|medium|high|xhigh|max.
func TestEffortFromBody(t *testing.T) {
	body := func(e effort.Level) router.Decision {
		return router.Decision{UpstreamModel: "m", Effort: e, EffortSource: router.EffortSourceBody}
	}
	suffix := func(e effort.Level) router.Decision {
		return router.Decision{UpstreamModel: "m", Effort: e, EffortSource: router.EffortSourceSuffix}
	}
	// A model that lists max but not xhigh, like the fake catalog's sol, and one
	// that stops at xhigh, like gpt-5.5.
	noXHigh := cschema.CatalogModel{
		DefaultReasoningLevel:    string(effort.Low),
		SupportedReasoningLevels: levels(effort.Low, effort.Medium, effort.High, effort.Max),
	}

	cases := []struct {
		name        string
		dec         router.Decision
		model       cschema.CatalogModel
		opts        request.Options
		wantApplied effort.Level
		wantSource  effort.Source
		wantClamped bool
	}{
		{"body beats catalog default", body(effort.High), solModel(), request.Options{},
			effort.High, router.EffortSourceBody, false},
		{"body beats beta and config", body(effort.Medium), solModel(),
			request.Options{BetaEffort: effort.Low, ConfigEffort: effort.Low},
			effort.Medium, router.EffortSourceBody, false},
		{"suffix beats body", suffix(effort.Low), solModel(), request.Options{},
			effort.Low, router.EffortSourceSuffix, false},
		{"max passes where the model lists it", body(effort.Max), solModel(), request.Options{},
			effort.Max, router.EffortSourceBody, false},
		{"max clamps down to xhigh where max is missing", body(effort.Max), gpt55Model(), request.Options{},
			effort.XHigh, router.EffortSourceBody, true},
		{"xhigh clamps down to high where xhigh is missing", body(effort.XHigh), noXHigh, request.Options{},
			effort.High, router.EffortSourceBody, true},
		{"no body effort falls back to the catalog default", router.Decision{UpstreamModel: "m"}, solModel(),
			request.Options{}, effort.Low, router.EffortSourceCatalog, false},
	}

	req := &aschema.MessagesRequest{
		Model:    "m",
		Messages: []aschema.Message{{Role: "user", Content: aschema.StringContent("hi")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, meta, err := request.Translate(req, c.dec, c.model, c.opts)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if meta.Effort.Applied != c.wantApplied || meta.Effort.Source != c.wantSource || meta.Effort.Clamped != c.wantClamped {
				t.Errorf("effort = applied %q source %q clamped %v, want %q %q %v",
					meta.Effort.Applied, meta.Effort.Source, meta.Effort.Clamped,
					c.wantApplied, c.wantSource, c.wantClamped)
			}
			if meta.Effort.Requested != c.dec.Effort && c.dec.Effort != "" {
				t.Errorf("requested = %q, want %q", meta.Effort.Requested, c.dec.Effort)
			}
			if out.Reasoning == nil || out.Reasoning.Effort != string(c.wantApplied) {
				t.Errorf("wire reasoning = %+v, want effort %q", out.Reasoning, c.wantApplied)
			}
		})
	}
}
