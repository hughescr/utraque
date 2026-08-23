package request_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	aschema "github.com/hughescr/utraque/internal/anthropic/schema"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/translate/request"
)

// -update regenerates the *.responses.golden.json files from the current
// translator output. Commit with goldens fixed:
//
//	go test ./internal/translate/request -run TestGolden -update
var update = flag.Bool("update", false, "regenerate golden files")

const requestsDir = "../../../testdata/requests"

// --- catalog model fixtures (shapes copied from the real Codex catalog) ---

func solModel() cschema.Model {
	return cschema.Model{
		Slug:                    "gpt-5.6-sol",
		DefaultReasoningLevel:   cschema.EffortLow,
		DefaultReasoningSummary: "none",
		SupportedReasoningLevels: levels(
			cschema.EffortLow, cschema.EffortMedium, cschema.EffortHigh,
			cschema.EffortXHigh, cschema.EffortMax, cschema.EffortUltra),
	}
}

func lunaModel() cschema.Model {
	return cschema.Model{
		Slug:                    "gpt-5.6-luna",
		DefaultReasoningLevel:   cschema.EffortMedium,
		DefaultReasoningSummary: "none",
		SupportedReasoningLevels: levels(
			cschema.EffortLow, cschema.EffortMedium, cschema.EffortHigh,
			cschema.EffortXHigh, cschema.EffortMax),
	}
}

func gpt54Model() cschema.Model {
	return cschema.Model{
		Slug:                    "gpt-5.4",
		DefaultReasoningLevel:   cschema.EffortMedium,
		DefaultReasoningSummary: "none",
		SupportedReasoningLevels: levels(
			cschema.EffortLow, cschema.EffortMedium, cschema.EffortHigh, cschema.EffortXHigh),
	}
}

func levels(efforts ...string) []cschema.ReasoningLevel {
	out := make([]cschema.ReasoningLevel, len(efforts))
	for i, e := range efforts {
		out[i] = cschema.ReasoningLevel{Effort: e}
	}
	return out
}

// caseCfg pairs a fixture with the routing Decision, catalog model, and options
// the translator should run under. Fixtures absent from the table use
// defaultCase.
type caseCfg struct {
	dec   router.Decision
	model cschema.Model
	opts  request.Options
}

func defaultCase() caseCfg {
	return caseCfg{
		dec:   router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol"},
		model: solModel(),
	}
}

// caseConfigs overrides the default per-fixture. The keys are fixture basenames
// (without the .anthropic.json suffix).
var caseConfigs = map[string]caseCfg{
	"effort_suffix": {
		dec:   router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", Effort: cschema.EffortHigh, EffortSource: router.EffortSourceSuffix},
		model: solModel(),
	},
	"effort_clamp_down": {
		dec:   router.Decision{Backend: router.BackendCodex, UpstreamModel: "gpt-5.4", Effort: cschema.EffortUltra, EffortSource: router.EffortSourceSuffix},
		model: gpt54Model(),
	},
}

func configFor(name string) caseCfg {
	if c, ok := caseConfigs[name]; ok {
		return c
	}
	return defaultCase()
}

func TestGolden(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join(requestsDir, "*.anthropic.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatalf("no fixtures under %s", requestsDir)
	}

	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".anthropic.json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatalf("read %s: %v", in, err)
			}
			var req aschema.MessagesRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatalf("unmarshal %s: %v", in, err)
			}

			cfg := configFor(name)
			out, _, err := request.Translate(&req, cfg.dec, cfg.model, cfg.opts)
			if err != nil {
				t.Fatalf("translate %s: %v", name, err)
			}

			got, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				t.Fatalf("marshal output: %v", err)
			}
			got = append(got, '\n')

			goldenPath := filepath.Join(requestsDir, name+".responses.golden.json")
			if *update {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
			}
		})
	}
}

// TestEffortMatrix exercises the full precedence order (suffix > anthropic-beta
// > config > catalog default) and the clamp behaviour, including an effort above
// a model's max clamping down and a request below a model's floor clamping up.
func TestEffortMatrix(t *testing.T) {
	suffix := func(e string) router.Decision {
		return router.Decision{UpstreamModel: "m", Effort: e, EffortSource: router.EffortSourceSuffix}
	}
	none := router.Decision{UpstreamModel: "m"}

	cases := []struct {
		name        string
		dec         router.Decision
		model       cschema.Model
		opts        request.Options
		wantApplied string
		wantSource  string
		wantClamped bool
	}{
		// Precedence: suffix beats everything below it.
		{"suffix_wins", suffix(cschema.EffortHigh), solModel(),
			request.Options{BetaEffort: cschema.EffortLow, ConfigEffort: cschema.EffortMedium},
			cschema.EffortHigh, router.EffortSourceSuffix, false},
		// Precedence: beta beats config and catalog.
		{"beta_wins", none, solModel(),
			request.Options{BetaEffort: cschema.EffortMedium, ConfigEffort: cschema.EffortLow},
			cschema.EffortMedium, router.EffortSourceBeta, false},
		// Precedence: config beats catalog default.
		{"config_wins", none, solModel(),
			request.Options{ConfigEffort: cschema.EffortHigh},
			cschema.EffortHigh, router.EffortSourceConfig, false},
		// Precedence: catalog default when nothing else is set.
		{"catalog_default", none, solModel(), request.Options{},
			cschema.EffortLow, router.EffortSourceCatalog, false},
		// Clamp DOWN: ultra requested on a model topping out at xhigh.
		{"clamp_down_to_xhigh", suffix(cschema.EffortUltra), gpt54Model(), request.Options{},
			cschema.EffortXHigh, router.EffortSourceSuffix, true},
		// Clamp DOWN: ultra requested on luna (tops out at max).
		{"clamp_down_to_max", suffix(cschema.EffortUltra), lunaModel(), request.Options{},
			cschema.EffortMax, router.EffortSourceSuffix, true},
		// No clamp: max is supported by sol.
		{"no_clamp_supported", suffix(cschema.EffortMax), solModel(), request.Options{},
			cschema.EffortMax, router.EffortSourceSuffix, false},
		// Clamp UP: a model whose floor is medium, request low.
		{"clamp_up_to_floor", suffix(cschema.EffortLow),
			cschema.Model{DefaultReasoningLevel: cschema.EffortMedium,
				SupportedReasoningLevels: levels(cschema.EffortMedium, cschema.EffortHigh)},
			request.Options{},
			cschema.EffortMedium, router.EffortSourceSuffix, true},
		// No catalog levels: request passes through unclamped.
		{"no_catalog_levels", suffix(cschema.EffortUltra),
			cschema.Model{}, request.Options{},
			cschema.EffortUltra, router.EffortSourceSuffix, false},
	}

	req := &aschema.MessagesRequest{
		Model:    "m",
		Messages: []aschema.Message{{Role: "user", Content: aschema.StringContent("hi")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, meta, err := request.Translate(req, tc.dec, tc.model, tc.opts)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if meta.Effort.Applied != tc.wantApplied {
				t.Errorf("applied effort = %q, want %q", meta.Effort.Applied, tc.wantApplied)
			}
			if meta.Effort.Source != tc.wantSource {
				t.Errorf("effort source = %q, want %q", meta.Effort.Source, tc.wantSource)
			}
			if meta.Effort.Clamped != tc.wantClamped {
				t.Errorf("clamped = %v, want %v", meta.Effort.Clamped, tc.wantClamped)
			}
			// The applied effort must also reach the wire reasoning block.
			if out.Reasoning == nil {
				t.Fatalf("reasoning block is nil")
			}
			if out.Reasoning.Effort != tc.wantApplied {
				t.Errorf("wire reasoning.effort = %q, want %q", out.Reasoning.Effort, tc.wantApplied)
			}
		})
	}
}

// TestDroppedParams asserts the record of dropped sampling/limit params.
func TestDroppedParams(t *testing.T) {
	raw := readFixture(t, "dropped_params")
	var req aschema.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, meta, err := request.Translate(&req, defaultCase().dec, defaultCase().model, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := []string{
		request.DroppedTemperature, request.DroppedTopP, request.DroppedTopK,
		request.DroppedMaxTokens, request.DroppedStopSequences,
	}
	if !reflect.DeepEqual(meta.Dropped, want) {
		t.Errorf("dropped = %v, want %v", meta.Dropped, want)
	}
}

// TestDroppedMaxTokensAlways confirms max_tokens is always recorded as dropped
// even when no sampling params are present (it is required by the Anthropic
// schema, so always present).
func TestDroppedMaxTokensAlways(t *testing.T) {
	req := &aschema.MessagesRequest{
		Model:     "m",
		MaxTokens: 100,
		Messages:  []aschema.Message{{Role: "user", Content: aschema.StringContent("hi")}},
	}
	_, meta, err := request.Translate(req, defaultCase().dec, defaultCase().model, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !reflect.DeepEqual(meta.Dropped, []string{request.DroppedMaxTokens}) {
		t.Errorf("dropped = %v, want [max_tokens]", meta.Dropped)
	}
}

// TestOrphanedToolResult confirms an unmatched tool_result is emitted as a
// function_call_output AND recorded in the metadata.
func TestOrphanedToolResult(t *testing.T) {
	raw := readFixture(t, "orphaned_tool_result")
	var req aschema.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, meta, err := request.Translate(&req, defaultCase().dec, defaultCase().model, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if want := []string{"toolu_missing"}; !reflect.DeepEqual(meta.OrphanedToolResults, want) {
		t.Errorf("orphaned = %v, want %v", meta.OrphanedToolResults, want)
	}
	if len(out.Input) != 1 || out.Input[0].Type != cschema.ItemFunctionCallOutput {
		t.Fatalf("expected one function_call_output, got %+v", out.Input)
	}
	if out.Input[0].CallID != "toolu_missing" {
		t.Errorf("call_id = %q, want toolu_missing", out.Input[0].CallID)
	}
}

// TestPairedToolResultNotOrphaned confirms a matched tool_result is NOT flagged.
func TestPairedToolResultNotOrphaned(t *testing.T) {
	raw := readFixture(t, "tool_use_result")
	var req aschema.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, meta, err := request.Translate(&req, defaultCase().dec, defaultCase().model, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(meta.OrphanedToolResults) != 0 {
		t.Errorf("orphaned = %v, want none", meta.OrphanedToolResults)
	}
}

// TestParallelDisable covers the mutating-tool parallel-disable, its metadata,
// and that a non-mutating tool set leaves parallel_tool_calls unset.
func TestParallelDisable(t *testing.T) {
	raw := readFixture(t, "mutating_parallel_disable")
	var req aschema.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, meta, err := request.Translate(&req, defaultCase().dec, defaultCase().model, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out.ParallelToolCalls == nil || *out.ParallelToolCalls != false {
		t.Fatalf("ParallelToolCalls = %v, want *false", out.ParallelToolCalls)
	}
	if !meta.ParallelToolCallsDisabled {
		t.Error("metadata should record ParallelToolCallsDisabled")
	}
	if !reflect.DeepEqual(meta.MutatingTools, []string{"Bash"}) {
		t.Errorf("MutatingTools = %v, want [Bash]", meta.MutatingTools)
	}

	// A non-mutating tool set: field stays unset.
	raw = readFixture(t, "tool_choice_auto")
	var req2 aschema.MessagesRequest
	if err := json.Unmarshal(raw, &req2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out2, meta2, err := request.Translate(&req2, defaultCase().dec, defaultCase().model, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out2.ParallelToolCalls != nil {
		t.Errorf("ParallelToolCalls = %v, want nil for non-mutating tools", *out2.ParallelToolCalls)
	}
	if meta2.ParallelToolCallsDisabled {
		t.Error("non-mutating tools should not disable parallel calls")
	}
}

// TestCustomMutatingSet confirms Options.MutatingTools replaces the default set.
func TestCustomMutatingSet(t *testing.T) {
	req := &aschema.MessagesRequest{
		Model: "m", MaxTokens: 10,
		Tools:    []aschema.Tool{{Name: "Bash"}, {Name: "DangerTool"}},
		Messages: []aschema.Message{{Role: "user", Content: aschema.StringContent("hi")}},
	}
	// Only DangerTool is mutating now; Bash is not.
	out, meta, err := request.Translate(req, defaultCase().dec, defaultCase().model,
		request.Options{MutatingTools: map[string]bool{"DangerTool": true}})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out.ParallelToolCalls == nil {
		t.Fatal("expected parallel disabled by DangerTool")
	}
	if !reflect.DeepEqual(meta.MutatingTools, []string{"DangerTool"}) {
		t.Errorf("MutatingTools = %v, want [DangerTool]", meta.MutatingTools)
	}

	// An empty (non-nil) set disables the default behaviour entirely.
	out2, _, err := request.Translate(req, defaultCase().dec, defaultCase().model,
		request.Options{MutatingTools: map[string]bool{}})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out2.ParallelToolCalls != nil {
		t.Error("empty mutating set should leave parallel_tool_calls unset")
	}
}

// TestSummaryOverride confirms the reasoning summary comes from the catalog
// default and is overridable, with "none" meaning omit.
func TestSummaryOverride(t *testing.T) {
	req := &aschema.MessagesRequest{
		Model: "m", MaxTokens: 10,
		Messages: []aschema.Message{{Role: "user", Content: aschema.StringContent("hi")}},
	}
	// Catalog default is "none" -> summary omitted.
	out, meta, err := request.Translate(req, defaultCase().dec, solModel(), request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out.Reasoning.Summary != "" || meta.Summary != "" {
		t.Errorf("summary = %q/%q, want empty (none omits)", out.Reasoning.Summary, meta.Summary)
	}

	// Override to "auto".
	out2, meta2, err := request.Translate(req, defaultCase().dec, solModel(), request.Options{Summary: "auto"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out2.Reasoning.Summary != "auto" || meta2.Summary != "auto" {
		t.Errorf("summary = %q/%q, want auto", out2.Reasoning.Summary, meta2.Summary)
	}

	// Model default is a real mode; no override keeps it.
	m := solModel()
	m.DefaultReasoningSummary = "concise"
	out3, _, err := request.Translate(req, defaultCase().dec, m, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out3.Reasoning.Summary != "concise" {
		t.Errorf("summary = %q, want concise", out3.Reasoning.Summary)
	}
}

// TestAlwaysStoreFalseStreamTrue asserts the two invariants on every request.
func TestAlwaysStoreFalseStreamTrue(t *testing.T) {
	for _, streamIn := range []bool{true, false} {
		req := &aschema.MessagesRequest{
			Model: "m", MaxTokens: 10, Stream: streamIn,
			Messages: []aschema.Message{{Role: "user", Content: aschema.StringContent("hi")}},
		}
		out, _, err := request.Translate(req, defaultCase().dec, solModel(), request.Options{})
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		if out.Store {
			t.Error("store must always be false")
		}
		if !out.Stream {
			t.Error("stream must always be true")
		}
	}
}

// TestToolChoiceObjectShape checks the pinned-function tool_choice marshals to
// the object form.
func TestToolChoiceObjectShape(t *testing.T) {
	b, err := json.Marshal(cschema.FunctionChoice("get_weather"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(b); got != `{"type":"function","name":"get_weather"}` {
		t.Errorf("function tool_choice = %s", got)
	}
	b, _ = json.Marshal(cschema.AutoChoice())
	if got := string(b); got != `"auto"` {
		t.Errorf("auto tool_choice = %s", got)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(requestsDir, name+".anthropic.json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}
