package stream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/anthropic"
	schema "github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/sse"
	"github.com/hughescr/utraque/internal/translate/stream"
)

// -update regenerates the *.anthropic.sse golden files from the current
// translator output. The simplest goldens (text_only, one_tool_call) are
// hand-verified against the Anthropic streaming spec and are ground truth;
// commit with goldens fixed:
//
//	go test ./internal/translate/stream -run TestGolden -update
var update = flag.Bool("update", false, "regenerate golden files")

const streamsDir = "../../../testdata/streams"

// goldenOptions is the fixed configuration every golden and grammar case runs
// under. Heartbeat and idle are disabled so the output is deterministic (no
// pings), and the response id in every fixture is "resp_test".
func goldenOptions() stream.Options {
	return stream.Options{
		Model:         "gpt-5.6-sol",
		InputTokens:   7,
		EmitReasoning: "thinking",
		OnTruncate:    "error",
		Heartbeat:     -1,
		UpstreamIdle:  -1,
	}
}

// runToBytes translates input through an SSEWriter into a buffer.
func runToBytes(t *testing.T, input []byte, opts stream.Options) ([]byte, stream.Result, error) {
	t.Helper()
	var buf bytes.Buffer
	w := stream.NewSSEWriter(&buf)
	tr := stream.New(opts)
	res, err := tr.Run(context.Background(), bytes.NewReader(input), w)
	if ferr := w.Flush(); ferr != nil && err == nil {
		err = ferr
	}
	return buf.Bytes(), res, err
}

func fixtureInputs(t *testing.T) []string {
	t.Helper()
	inputs, err := filepath.Glob(filepath.Join(streamsDir, "*.codex.sse"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatalf("no fixtures under %s", streamsDir)
	}
	return inputs
}

func caseName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".codex.sse")
}

// TestGolden runs every fixture and compares its Anthropic SSE output against
// the committed golden.
func TestGolden(t *testing.T) {
	for _, in := range fixtureInputs(t) {
		name := caseName(in)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatalf("read %s: %v", in, err)
			}
			got, _, err := runToBytes(t, raw, goldenOptions())
			if err != nil {
				t.Fatalf("run %s: unexpected error: %v", name, err)
			}

			// The emitted stream must itself satisfy the grammar invariants.
			checkGrammar(t, got)

			goldenPath := filepath.Join(streamsDir, name+".anthropic.sse")
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
			if !bytes.Equal(got, want) {
				t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
			}
		})
	}
}

// TestGrammarInvariantAllCases asserts the grammar checker passes on every
// fixture's full output (a redundant guard alongside TestGolden that keeps the
// invariant explicit).
func TestGrammarInvariantAllCases(t *testing.T) {
	for _, in := range fixtureInputs(t) {
		name := caseName(in)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			got, _, err := runToBytes(t, raw, goldenOptions())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			checkGrammar(t, got)
		})
	}
}

// TestPrefixTruncation is the highest-value property test: for every fixture,
// feed EVERY byte-prefix of the input and assert the emitted output is always a
// well-formed Anthropic stream (or empty, when nothing was emitted yet). Every
// truncation point — mid-frame, mid-JSON, mid-block — must still yield grammar.
//
// It sweeps BOTH on_truncate modes. error-mode force-closes via an error
// terminus; finish-mode synthesises a clean terminus — and the grammar checker
// enforces that a clean terminus never carries invalid tool JSON, so a prefix
// cut mid-tool-arguments in finish mode must resolve to an error terminus, not a
// fabricated tool_use the client cannot parse.
func TestPrefixTruncation(t *testing.T) {
	modes := []string{"error", "finish"}
	for _, in := range fixtureInputs(t) {
		name := caseName(in)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			for _, mode := range modes {
				opts := goldenOptions()
				opts.OnTruncate = mode
				for k := 0; k <= len(raw); k++ {
					got, _, _ := runToBytes(t, raw[:k], opts)
					if err := grammarError(got); err != nil {
						t.Fatalf("on_truncate=%s prefix len %d of %d produced ill-formed output: %v\n--- output ---\n%s",
							mode, k, len(raw), err, got)
					}
				}
			}
		})
	}
}

// TestClientInterruptNoLeak asserts a ctx cancel mid-stream abandons the sink
// without a terminus and leaks no reader goroutine (a local stand-in for
// go.uber.org/goleak, which is unavailable offline).
func TestClientInterruptNoLeak(t *testing.T) {
	// A reader that serves the first frame then blocks in Read until Close, the
	// way a live upstream body blocks until the transport cancels it.
	created := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n"
	br := &blockingReader{first: []byte(created), closed: make(chan struct{})}

	runtime.GC()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	tr := stream.New(goldenOptions())
	var buf bytes.Buffer
	w := stream.NewSSEWriter(&buf)

	type outcome struct {
		res stream.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := tr.Run(ctx, br, w)
		done <- outcome{res, err}
	}()

	// Let the translator consume the first frame and block on the next Read.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("Run err = %v, want context.Canceled", got.err)
		}
		if got.res.Terminated {
			t.Error("interrupted stream must not report a terminus")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	// The interrupt must not have written a terminus onto the wire.
	if out := buf.String(); strings.Contains(out, "message_stop") || strings.Contains(out, "event: error") {
		t.Errorf("interrupted stream wrote a terminus:\n%s", out)
	}

	// No leaked reader goroutine: the count returns to baseline.
	assertNoLeak(t, before)
}

// TestNothingEmittedSignalsMode1 confirms an empty upstream yields Started=false
// and a propagated error, so the caller can render an HTTP status envelope.
func TestNothingEmittedSignalsMode1(t *testing.T) {
	_, res, err := runToBytes(t, nil, goldenOptions())
	if err == nil {
		t.Fatal("want an error for an empty stream")
	}
	if res.Started {
		t.Error("Started should be false when nothing was emitted")
	}
	if res.Terminated {
		t.Error("Terminated should be false when nothing was emitted")
	}
}

// TestReasoningDropMode confirms emit_reasoning=drop suppresses thinking blocks
// entirely while the text answer still streams.
func TestReasoningDropMode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(streamsDir, "reasoning_and_text.codex.sse"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	opts := goldenOptions()
	opts.EmitReasoning = "drop"
	got, _, err := runToBytes(t, raw, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	checkGrammar(t, got)
	if strings.Contains(string(got), "thinking") {
		t.Errorf("emit_reasoning=drop still produced a thinking block:\n%s", got)
	}
	if !strings.Contains(string(got), "The answer is 42.") {
		t.Errorf("text answer missing under drop mode:\n%s", got)
	}
}

// TestOnTruncateFinish confirms on_truncate=finish synthesises a clean terminus
// on a truncated stream instead of an error frame.
func TestOnTruncateFinish(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(streamsDir, "truncated_stream.codex.sse"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	opts := goldenOptions()
	opts.OnTruncate = "finish"
	got, _, err := runToBytes(t, raw, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	checkGrammar(t, got)
	s := string(got)
	if !strings.Contains(s, "message_stop") {
		t.Errorf("on_truncate=finish should end in message_stop:\n%s", s)
	}
	if strings.Contains(s, "event: error") {
		t.Errorf("on_truncate=finish should not emit an error frame:\n%s", s)
	}
}

// sseFrame renders one Anthropic SSE frame from an event name and JSON body.
func sseFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// TestGrammarRejectsIllFormed feeds the grammar checker hand-crafted ILL-FORMED
// Anthropic streams and asserts it rejects each. Without these, the checker's
// rejection branches are never exercised (every other test feeds it well-formed
// translator output), so a deleted invariant would leave the suite green.
func TestGrammarRejectsIllFormed(t *testing.T) {
	msgStart := sseFrame("message_start", `{"type":"message_start","message":{"id":"m","model":"x","content":[],"usage":{"input_tokens":1}}}`)
	cases := []struct {
		name   string
		stream string
	}{
		{
			"content_block_start before message_start",
			sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		},
		{
			"non-monotonic block index",
			msgStart +
				sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
				sseFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
				sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		},
		{
			"two blocks open at once",
			msgStart +
				sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
				sseFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		},
		{
			"invalid tool JSON on a clean terminus",
			msgStart +
				sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`) +
				sseFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a"}}`) +
				sseFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
				sseFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`) +
				sseFrame("message_stop", `{"type":"message_stop"}`),
		},
		{
			"delta kind mismatched to block kind",
			msgStart +
				sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`) +
				sseFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"oops"}}`),
		},
		{
			"event after terminus",
			msgStart +
				sseFrame("message_stop", `{"type":"message_stop"}`) +
				sseFrame("message_stop", `{"type":"message_stop"}`),
		},
		{
			"event name disagrees with json type",
			msgStart +
				sseFrame("error", `{"type":"message_stop"}`),
		},
		{
			"no terminus",
			msgStart +
				sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
				sseFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := grammarError([]byte(c.stream)); err == nil {
				t.Errorf("grammarError accepted an ill-formed stream:\n%s", c.stream)
			}
		})
	}

	// Positive control: a minimal well-formed stream is accepted.
	good := msgStart + sseFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`) +
		sseFrame("message_stop", `{"type":"message_stop"}`)
	if err := grammarError([]byte(good)); err != nil {
		t.Errorf("grammarError rejected a well-formed stream: %v", err)
	}
}

// TestFinishModePartialToolErrors confirms on_truncate=finish over a tool call
// truncated mid-arguments does NOT fabricate a clean tool_use the client cannot
// parse: it resolves to an error terminus instead. This is the cardinal sin the
// finalize path must refuse — a faked clean terminus over truncated tool JSON.
func TestFinishModePartialToolErrors(t *testing.T) {
	input := sseFrame("response.created", `{"type":"response.created","response":{"id":"resp_test"}}`) +
		sseFrame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_0","call_id":"call_0","name":"get_weather"}}`) +
		sseFrame("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"loc"}`)
	opts := goldenOptions()
	opts.OnTruncate = "finish"
	got, res, err := runToBytes(t, []byte(input), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	checkGrammar(t, got)
	s := string(got)
	if strings.Contains(s, "message_stop") {
		t.Errorf("finish mode faked a clean terminus over a partial tool call:\n%s", s)
	}
	if !strings.Contains(s, "event: error") {
		t.Errorf("finish mode over a partial tool call should emit an error terminus:\n%s", s)
	}
	if !res.Errored {
		t.Errorf("Result.Errored should be true, got %+v", res)
	}
}

// TestPendingBoundsAbort confirms exceeding the pending-item bound aborts the
// stream with an error terminus rather than silently force-closing a block (and
// dropping its later deltas or stranding invalid tool JSON under a clean stop).
func TestPendingBoundsAbort(t *testing.T) {
	var b strings.Builder
	b.WriteString(sseFrame("response.created", `{"type":"response.created","response":{"id":"resp_test"}}`))
	// Open output 0 as a text block that stays active and streaming.
	b.WriteString(sseFrame("response.content_part.added", `{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`))
	b.WriteString(sseFrame("response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"delta":"hello"}`))
	// Announce and buffer many parallel tool items (each a distinct output index),
	// then stream a delta for each so they accumulate in the pending buffer.
	for i := 1; i <= 20; i++ {
		idx := fmt.Sprintf("%d", i)
		b.WriteString(sseFrame("response.output_item.added", `{"type":"response.output_item.added","output_index":`+idx+`,"item":{"type":"function_call","id":"fc_`+idx+`","call_id":"call_`+idx+`","name":"f"}}`))
		b.WriteString(sseFrame("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":`+idx+`,"delta":"{}"}`))
	}
	// A clean completion afterwards must NOT rescue this into a message_stop.
	b.WriteString(sseFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_test","status":"completed","usage":{"input_tokens":7,"output_tokens":1}}}`))

	opts := goldenOptions() // maxPendingItems defaults to 16
	got, res, err := runToBytes(t, []byte(b.String()), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	checkGrammar(t, got)
	s := string(got)
	if strings.Contains(s, "message_stop") {
		t.Errorf("bounds overflow produced a clean terminus:\n%s", s)
	}
	if !strings.Contains(s, "event: error") || !res.Errored {
		t.Errorf("bounds overflow should abort with an error terminus, got %+v:\n%s", res, s)
	}
}

// TestPendingBytesBoundAbort covers the OTHER half of enforceBounds: a single
// buffered item whose accumulated deltas exceed max_pending_bytes. The item
// count stays comfortably inside max_pending_items, so only the byte branch can
// trip — and it must reach the same honest terminus as the item-count branch:
// an error event, no message_stop, even though the upstream then completes
// cleanly. Force-closing the oversized block instead would strand its
// half-streamed tool arguments under a fabricated clean stop.
func TestPendingBytesBoundAbort(t *testing.T) {
	var b strings.Builder
	b.WriteString(sseFrame("response.created", `{"type":"response.created","response":{"id":"resp_test"}}`))
	// Output 0 opens as a text block and stays active, so everything after it
	// buffers rather than opening.
	b.WriteString(sseFrame("response.content_part.added", `{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`))
	b.WriteString(sseFrame("response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"delta":"hello"}`))
	// One parallel tool call whose arguments are far larger than the byte bound.
	b.WriteString(sseFrame("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f"}}`))
	big := strings.Repeat("x", 4096)
	args := `{\"a\":\"` + big + `\"}`
	b.WriteString(sseFrame("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"`+args+`"}`))
	b.WriteString(sseFrame("response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","output_index":1,"arguments":"`+args+`"}`))
	b.WriteString(sseFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_test","status":"completed","usage":{"input_tokens":7,"output_tokens":1}}}`))

	opts := goldenOptions()
	opts.MaxPendingBytes = 64 // one item, way over the byte bound
	opts.MaxPendingItems = 16 // default headroom: the count branch must NOT trip
	got, res, err := runToBytes(t, []byte(b.String()), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	checkGrammar(t, got)
	s := string(got)
	if strings.Contains(s, "message_stop") {
		t.Errorf("byte-bound overflow produced a clean terminus:\n%s", s)
	}
	if !strings.Contains(s, "event: error") || !res.Errored {
		t.Errorf("byte-bound overflow should abort with an error terminus, got %+v:\n%s", res, s)
	}
	if !strings.Contains(s, "pending-buffer bounds") {
		t.Errorf("error frame should name the bounds abort:\n%s", s)
	}
	// The oversized arguments must not have leaked onto the wire under a
	// content_block_start that was never opened.
	if strings.Contains(s, big) {
		t.Errorf("buffered oversized arguments were flushed to the client:\n%s", s)
	}

	// Control: the same stream inside the byte bound completes cleanly, proving
	// the abort above is the bound and not the fixture shape.
	ok := opts
	ok.MaxPendingBytes = 1 << 20
	got2, res2, err := runToBytes(t, []byte(b.String()), ok)
	if err != nil {
		t.Fatalf("control run: %v", err)
	}
	checkGrammar(t, got2)
	if res2.Errored || !strings.Contains(string(got2), "message_stop") {
		t.Errorf("control run should complete cleanly, got %+v:\n%s", res2, got2)
	}
}

// TestErrorFrameBeforeStartIsMode1 confirms an error frame arriving before any
// output is failure mode 1: nothing emitted, Started false, error propagated for
// the caller to render an HTTP envelope.
func TestErrorFrameBeforeStartIsMode1(t *testing.T) {
	input := sseFrame("error", `{"type":"error","message":"upstream boom"}`)
	got, res, err := runToBytes(t, []byte(input), goldenOptions())
	if err == nil {
		t.Fatal("want an error for an error-frame-before-start")
	}
	if res.Started {
		t.Errorf("Started should be false, got %+v", res)
	}
	if len(got) != 0 {
		t.Errorf("nothing should be written before message_start, got:\n%s", got)
	}
}

// TestDoneBeforeStartKeepsOrder confirms a terminal *.done arriving before any
// start-triggering event still emits message_start before the block it opens, so
// the "message_start first" invariant holds even on a pathological ordering.
func TestDoneBeforeStartKeepsOrder(t *testing.T) {
	input := sseFrame("response.output_text.done", `{"type":"response.output_text.done","output_index":0,"text":"hi"}`)
	got, _, err := runToBytes(t, []byte(input), goldenOptions())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	checkGrammar(t, got)
}

// TestSinkFoldEquivalence asserts decode(SSEWriter(events)) == events: the
// semantic sink calls the translator makes, once serialised by SSEWriter and
// parsed back, reproduce the identical sequence. This is the seam the phase-6
// Aggregator plugs into — proof the wire form loses nothing.
func TestSinkFoldEquivalence(t *testing.T) {
	for _, in := range fixtureInputs(t) {
		name := caseName(in)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			// Direct: translator -> recording sink.
			direct := &recordingSink{}
			tr := stream.New(goldenOptions())
			if _, err := tr.Run(context.Background(), bytes.NewReader(raw), direct); err != nil {
				t.Fatalf("direct run: %v", err)
			}

			// Folded: translator -> SSEWriter -> parse -> recording sink.
			wire, _, err := runToBytes(t, raw, goldenOptions())
			if err != nil {
				t.Fatalf("wire run: %v", err)
			}
			folded := &recordingSink{}
			if err := replayInto(parseFrames(t, wire), folded); err != nil {
				t.Fatalf("replay: %v", err)
			}

			if len(direct.recs) != len(folded.recs) {
				t.Fatalf("record count %d != %d\ndirect: %v\nfolded: %v",
					len(direct.recs), len(folded.recs), direct.recs, folded.recs)
			}
			for i := range direct.recs {
				if direct.recs[i] != folded.recs[i] {
					t.Errorf("record %d differs:\n direct: %s\n folded: %s", i, direct.recs[i], folded.recs[i])
				}
			}
		})
	}
}

// TestSyntheticSignatureMarker confirms the reasoning close carries the fixed
// marker the Anthropic-leg sanitizer strips.
func TestSyntheticSignatureMarker(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(streamsDir, "reasoning_and_text.codex.sse"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, _, err := runToBytes(t, raw, goldenOptions())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(got), anthropic.SyntheticThinkingMarker) {
		t.Errorf("thinking close is missing the synthetic marker %q:\n%s",
			anthropic.SyntheticThinkingMarker, got)
	}
}

// TestUnknownEventCounts confirms unknown event types are counted per type.
func TestUnknownEventCounts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(streamsDir, "unknown_events.codex.sse"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, res, err := runToBytes(t, raw, goldenOptions())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.UnknownEvents) != 2 {
		t.Errorf("unknown event types = %v, want 2 distinct", res.UnknownEvents)
	}
	for typ, n := range res.UnknownEvents {
		if n != 1 {
			t.Errorf("unknown type %q counted %d times, want 1", typ, n)
		}
	}
}

// ---------------------------------------------------------------------------
// Grammar-invariant checker
// ---------------------------------------------------------------------------

// anthEvent is a permissive decode of one Anthropic stream event, enough to
// check the grammar and reconstruct the semantic sink call.
type anthEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type         string  `json:"type"`
		Text         string  `json:"text"`
		PartialJSON  string  `json:"partial_json"`
		Thinking     string  `json:"thinking"`
		Signature    string  `json:"signature"`
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	ContentBlock struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content_block"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage *schema.Usage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseFrames(t *testing.T, data []byte) []sse.Frame {
	t.Helper()
	sc := sse.NewScanner(bytes.NewReader(data))
	var out []sse.Frame
	for sc.Scan() {
		f := sc.Frame()
		out = append(out, sse.Frame{Event: f.Event, Data: append([]byte(nil), f.Data...)})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("parse frames: %v", err)
	}
	return out
}

func checkGrammar(t *testing.T, data []byte) {
	t.Helper()
	if err := grammarError(data); err != nil {
		t.Fatalf("grammar violation: %v\n--- output ---\n%s", err, data)
	}
}

// grammarError returns nil when data is a well-formed Anthropic stream (or
// empty), else a descriptive error. Invariants: message_start first and once;
// one block open at a time; block indices monotonic and unique; each start
// matched by a stop before the terminus; tool blocks accumulate valid JSON on a
// clean terminus; the stream ends in exactly one of message_stop or error, with
// nothing after it.
func grammarError(data []byte) error {
	sc := sse.NewScanner(bytes.NewReader(data))
	var evs []anthEvent
	for sc.Scan() {
		f := sc.Frame()
		if len(f.Data) == 0 {
			continue
		}
		var ev anthEvent
		if err := json.Unmarshal(f.Data, &ev); err != nil {
			return fmt.Errorf("undecodable frame %q: %w", f.Data, err)
		}
		// The SSE event: name and the JSON type must agree.
		if f.Event != "" && f.Event != ev.Type {
			return fmt.Errorf("event name %q disagrees with json type %q", f.Event, ev.Type)
		}
		evs = append(evs, ev)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	if len(evs) == 0 {
		return nil // nothing emitted: valid (mode 1 handled by the caller)
	}

	msgStarts := 0
	terminus := "" // "message_stop" | "error"
	openIdx := -1
	lastStartIdx := -1
	partial := map[int]string{}
	blockKind := map[int]string{}

	for i, ev := range evs {
		if terminus != "" {
			return fmt.Errorf("event %q after terminus %q", ev.Type, terminus)
		}
		switch ev.Type {
		case "ping":
			// A keepalive may appear anywhere before the terminus.
		case "message_start":
			if i != 0 {
				return fmt.Errorf("message_start at position %d, must be first", i)
			}
			msgStarts++
		case "content_block_start":
			if msgStarts == 0 {
				return errors.New("content_block_start before message_start")
			}
			if openIdx != -1 {
				return fmt.Errorf("content_block_start(%d) while block %d still open", ev.Index, openIdx)
			}
			if ev.Index <= lastStartIdx {
				return fmt.Errorf("block index %d not greater than previous %d", ev.Index, lastStartIdx)
			}
			openIdx = ev.Index
			lastStartIdx = ev.Index
			blockKind[ev.Index] = ev.ContentBlock.Type
		case "content_block_delta":
			if openIdx != ev.Index {
				return fmt.Errorf("content_block_delta(%d) but open block is %d", ev.Index, openIdx)
			}
			// The delta kind must match the open block's kind: text_delta into text,
			// thinking/signature_delta into thinking, input_json_delta into tool_use.
			// A translator bug routing the wrong delta into a block is otherwise
			// invisible.
			if kind := blockKind[ev.Index]; ev.Delta.Type != "" {
				ok := false
				switch ev.Delta.Type {
				case "text_delta":
					ok = kind == "text"
				case "thinking_delta", "signature_delta":
					ok = kind == "thinking"
				case "input_json_delta":
					ok = kind == "tool_use"
				}
				if !ok {
					return fmt.Errorf("delta type %q into %q block %d", ev.Delta.Type, kind, ev.Index)
				}
			}
			if ev.Delta.Type == "input_json_delta" {
				partial[ev.Index] += ev.Delta.PartialJSON
			}
		case "content_block_stop":
			if openIdx != ev.Index {
				return fmt.Errorf("content_block_stop(%d) but open block is %d", ev.Index, openIdx)
			}
			openIdx = -1
		case "message_delta":
			if openIdx != -1 {
				return fmt.Errorf("message_delta while block %d still open", openIdx)
			}
			if msgStarts == 0 {
				return errors.New("message_delta before message_start")
			}
		case "message_stop":
			if openIdx != -1 {
				return fmt.Errorf("message_stop while block %d still open", openIdx)
			}
			terminus = "message_stop"
		case "error":
			terminus = "error"
		default:
			return fmt.Errorf("unexpected event type %q", ev.Type)
		}
	}

	if msgStarts != 1 {
		return fmt.Errorf("message_start count = %d, want 1", msgStarts)
	}
	if openIdx != -1 {
		return fmt.Errorf("stream ended with block %d still open", openIdx)
	}
	if terminus == "" {
		return errors.New("stream ended without a terminus (message_stop or error)")
	}
	// On a clean terminus, every tool block's accumulated arguments must be valid
	// JSON. On an error terminus the block may have been force-closed mid-args.
	if terminus == "message_stop" {
		for idx, kind := range blockKind {
			if kind == "tool_use" {
				if !json.Valid([]byte(partial[idx])) {
					return fmt.Errorf("tool block %d accumulated invalid JSON %q", idx, partial[idx])
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Recording sink + replay (fold equivalence)
// ---------------------------------------------------------------------------

type recordingSink struct{ recs []string }

var _ stream.Sink = (*recordingSink)(nil)

func (s *recordingSink) MessageStart(m stream.MessageStart) error {
	s.recs = append(s.recs, fmt.Sprintf("message_start id=%s model=%s input=%d", m.ID, m.Model, m.InputTokens))
	return nil
}

func (s *recordingSink) BlockStart(i int, b schema.ContentBlock) error {
	s.recs = append(s.recs, fmt.Sprintf("block_start %d type=%s text=%q thinking=%q id=%s name=%s input=%s",
		i, b.Type, b.Text, b.Thinking, b.ID, b.Name, string(b.Input)))
	return nil
}

func (s *recordingSink) BlockDelta(i int, d schema.Delta) error {
	s.recs = append(s.recs, fmt.Sprintf("block_delta %d type=%s text=%q json=%q thinking=%q sig=%q",
		i, d.Type, d.Text, d.PartialJSON, d.Thinking, d.Signature))
	return nil
}

func (s *recordingSink) BlockStop(i int) error {
	s.recs = append(s.recs, fmt.Sprintf("block_stop %d", i))
	return nil
}

func (s *recordingSink) MessageDelta(d stream.MessageDelta) error {
	u, _ := json.Marshal(d.Usage)
	s.recs = append(s.recs, fmt.Sprintf("message_delta stop=%s usage=%s", d.StopReason, u))
	return nil
}

func (s *recordingSink) MessageStop() error {
	s.recs = append(s.recs, "message_stop")
	return nil
}

func (s *recordingSink) Error(e schema.ErrorBody) error {
	s.recs = append(s.recs, fmt.Sprintf("error type=%s msg=%s", e.Type, e.Message))
	return nil
}

func (s *recordingSink) Ping() error {
	s.recs = append(s.recs, "ping")
	return nil
}

// replayInto decodes Anthropic SSE frames back into semantic Sink calls, the
// mirror of SSEWriter. It reconstructs each call from the wire so a round trip
// through SSEWriter and back reproduces the original sink sequence.
func replayInto(frames []sse.Frame, sink stream.Sink) error {
	for _, f := range frames {
		if len(f.Data) == 0 {
			continue
		}
		var ev anthEvent
		if err := json.Unmarshal(f.Data, &ev); err != nil {
			return fmt.Errorf("decode %q: %w", f.Data, err)
		}
		var err error
		switch ev.Type {
		case "message_start":
			m := stream.MessageStart{}
			if ev.Message != nil {
				m = stream.MessageStart{ID: ev.Message.ID, Model: ev.Message.Model, InputTokens: ev.Message.Usage.InputTokens}
			}
			err = sink.MessageStart(m)
		case "content_block_start":
			cb := schema.ContentBlock{
				Type:     ev.ContentBlock.Type,
				Text:     ev.ContentBlock.Text,
				Thinking: ev.ContentBlock.Thinking,
				ID:       ev.ContentBlock.ID,
				Name:     ev.ContentBlock.Name,
				Input:    ev.ContentBlock.Input,
			}
			err = sink.BlockStart(ev.Index, cb)
		case "content_block_delta":
			d := schema.Delta{
				Type:        ev.Delta.Type,
				Text:        ev.Delta.Text,
				PartialJSON: ev.Delta.PartialJSON,
				Thinking:    ev.Delta.Thinking,
				Signature:   ev.Delta.Signature,
			}
			err = sink.BlockDelta(ev.Index, d)
		case "content_block_stop":
			err = sink.BlockStop(ev.Index)
		case "message_delta":
			stop := ""
			if ev.Delta.StopReason != nil {
				stop = *ev.Delta.StopReason
			}
			u := schema.Usage{}
			if ev.Usage != nil {
				u = *ev.Usage
			}
			err = sink.MessageDelta(stream.MessageDelta{StopReason: stop, Usage: u})
		case "message_stop":
			err = sink.MessageStop()
		case "error":
			b := schema.ErrorBody{}
			if ev.Error != nil {
				b = schema.ErrorBody{Type: ev.Error.Type, Message: ev.Error.Message}
			}
			err = sink.Error(b)
		case "ping":
			err = sink.Ping()
		default:
			return fmt.Errorf("replay: unknown event %q", ev.Type)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Interrupt-test helpers
// ---------------------------------------------------------------------------

// blockingReader serves an initial payload then blocks every subsequent Read
// until Close, modelling a live upstream body that unblocks only when the
// transport cancels it.
type blockingReader struct {
	mu     sync.Mutex
	first  []byte
	closed chan struct{}
	once   sync.Once
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.mu.Lock()
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		b.mu.Unlock()
		return n, nil
	}
	b.mu.Unlock()
	<-b.closed
	return 0, io.EOF
}

func (b *blockingReader) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func assertNoLeak(t *testing.T, before int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		runtime.GC()
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: baseline=%d, still-running=%d", before, runtime.NumGoroutine())
}

// TestTerminalUsageFallsBackToTheEstimateOnBothSinks is the regression guard for
// a divergence TestAggregatorEqualsSSEFold structurally cannot see: it folds the
// SSE golden back through the SAME Aggregator, so a substitution done only in
// the Aggregator looks identical on both sides. The check that matters is what
// goes ON THE WIRE, so this asserts the SSE bytes directly and then confirms the
// non-streaming fold reports the same number.
func TestTerminalUsageFallsBackToTheEstimateOnBothSinks(t *testing.T) {
	// A completed response whose usage block is absent entirely: mapUsage yields
	// the zero Usage, so the terminal input count is 0.
	input := sseFrame("response.created", `{"type":"response.created","response":{"id":"resp_test"}}`) +
		sseFrame("response.content_part.added",
			`{"type":"response.content_part.added","output_index":0,"part":{"type":"output_text"}}`) +
		sseFrame("response.output_text.delta",
			`{"type":"response.output_text.delta","output_index":0,"delta":"hi"}`) +
		sseFrame("response.output_text.done",
			`{"type":"response.output_text.done","output_index":0,"text":"hi"}`) +
		sseFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_test","status":"completed"}}`)

	wire, _, err := runToBytes(t, []byte(input), goldenOptions())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	checkGrammar(t, wire)
	if strings.Contains(string(wire), `"input_tokens":0`) {
		t.Errorf("the streamed message_delta reported a zero-token prompt:\n%s", wire)
	}
	if !strings.Contains(string(wire), `"usage":{"input_tokens":7`) {
		t.Errorf("the streamed message_delta did not carry the message_start estimate:\n%s", wire)
	}

	folded, err := aggregateFixture(t, []byte(input), goldenOptions())
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if !strings.Contains(string(folded), `"usage":{"input_tokens":7`) {
		t.Errorf("the folded message disagrees with the stream:\n%s", folded)
	}
}

// TestMidStreamErrorSignsAnOpenThinkingBlock: a thinking block force-closed by a
// mid-stream failure must still carry the synthetic signature. Without it the
// client keeps an unsigned thinking block, the Anthropic-leg sanitizer cannot
// classify it, and the next turn on a Claude model is rejected with a 400.
func TestMidStreamErrorSignsAnOpenThinkingBlock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(streamsDir, "midstream_error_in_thinking.codex.sse"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, res, err := runToBytes(t, raw, goldenOptions())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Errored {
		t.Fatalf("fixture should terminate in an error, got %+v", res)
	}
	checkGrammar(t, got)
	if !strings.Contains(string(got), anthropic.SyntheticThinkingMarker) {
		t.Errorf("a thinking block closed by a mid-stream error carries no synthetic signature:\n%s", got)
	}
}

// TestUpstreamFailureBeforeMessageStartIs502: the upstream opened a 200 and
// immediately failed. That is a gateway failure, the same class as an empty or
// truncated stream, and must not read as an internal 500.
func TestUpstreamFailureBeforeMessageStartIs502(t *testing.T) {
	for _, tc := range []struct{ name, frame string }{
		{"response.failed", sseFrame("response.failed",
			`{"type":"response.failed","response":{"id":"r","status":"failed","error":{"message":"backend on fire"}}}`)},
		{"error event", sseFrame("error", `{"type":"error","message":"backend on fire"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, res, err := runToBytes(t, []byte(tc.frame), goldenOptions())
			if err == nil {
				t.Fatal("want an error the caller can render as an HTTP status")
			}
			if res.Started {
				t.Error("Started should be false: nothing was emitted")
			}
			if len(out) != 0 {
				t.Errorf("bytes were committed before the failure:\n%s", out)
			}
			ae := apierr.From(err)
			if ae.HTTPStatus() != 502 {
				t.Errorf("status = %d, want 502", ae.HTTPStatus())
			}
			if !strings.Contains(ae.Message, "backend on fire") {
				t.Errorf("message = %q, want the upstream's own reason", ae.Message)
			}
		})
	}
}
