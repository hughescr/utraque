package stream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aschema "github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/translate/stream"
)

// aggregateFixture translates raw through an Aggregator and returns the folded
// wire JSON (or the fold error).
func aggregateFixture(t *testing.T, raw []byte, opts stream.Options) ([]byte, error) {
	t.Helper()
	agg := stream.NewAggregator()
	tr := stream.New(opts)
	if _, err := tr.Run(context.Background(), bytes.NewReader(raw), agg); err != nil {
		t.Fatalf("run: %v", err)
	}
	return agg.MessageJSON()
}

// foldGolden replays the committed SSEWriter golden bytes back into a fresh
// Aggregator, producing the semantic fold of the streaming output.
func foldGolden(t *testing.T, golden []byte) ([]byte, error) {
	t.Helper()
	agg := stream.NewAggregator()
	if err := replayInto(parseFrames(t, golden), agg); err != nil {
		t.Fatalf("replay golden: %v", err)
	}
	return agg.MessageJSON()
}

// TestAggregatorEqualsSSEFold is the guarantee that stream:false and
// stream:true can never diverge. For EVERY stream fixture it compares the
// Aggregator's own fold of the translator's sink calls against the fold of the
// SSEWriter's committed golden bytes, and requires them byte-identical —
// including the error path, where both must fail the same way.
func TestAggregatorEqualsSSEFold(t *testing.T) {
	for _, in := range fixtureInputs(t) {
		name := caseName(in)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			golden, err := os.ReadFile(filepath.Join(streamsDir, name+".anthropic.sse"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}

			got, gotErr := aggregateFixture(t, raw, goldenOptions())
			want, wantErr := foldGolden(t, golden)

			switch {
			case (gotErr == nil) != (wantErr == nil):
				t.Fatalf("error disagreement: aggregator err=%v, golden fold err=%v", gotErr, wantErr)
			case gotErr != nil:
				if gotErr.Error() != wantErr.Error() {
					t.Fatalf("error text differs:\n aggregator: %v\n golden fold: %v", gotErr, wantErr)
				}
				return
			}
			if !bytes.Equal(got, want) {
				t.Errorf("folded message differs\n--- aggregator ---\n%s\n--- golden fold ---\n%s", got, want)
			}
		})
	}
}

// TestAggregatorFoldsCleanFixtures pins the actual folded content for the
// representative shapes, so the equivalence test above cannot pass by both
// paths being equally wrong.
func TestAggregatorFoldsCleanFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
	}{
		{
			"text_only",
			`{"id":"msg_codex_resp_test","type":"message","role":"assistant","model":"gpt-5.6-sol",` +
				`"content":[{"type":"text","text":"Hello, world"}],` +
				`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":3}}`,
		},
		{
			"one_tool_call",
			`{"id":"msg_codex_resp_test","type":"message","role":"assistant","model":"gpt-5.6-sol",` +
				`"content":[{"type":"tool_use","id":"call_abc","name":"get_weather","input":{"location":"SF"}}],` +
				`"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":9}}`,
		},
		{
			"empty_tool_args",
			`{"id":"msg_codex_resp_test","type":"message","role":"assistant","model":"gpt-5.6-sol",` +
				`"content":[{"type":"tool_use","id":"call_e","name":"now","input":{}}],` +
				`"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":2}}`,
		},
		{
			"usage_cached_tokens",
			`{"id":"msg_codex_resp_test","type":"message","role":"assistant","model":"gpt-5.6-sol",` +
				`"content":[{"type":"text","text":"Reusing the cached prompt."}],` +
				`"stop_reason":"end_turn","stop_sequence":null,` +
				`"usage":{"input_tokens":1000,"output_tokens":20,"cache_read_input_tokens":800}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(streamsDir, c.fixture+".codex.sse"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			got, err := aggregateFixture(t, raw, goldenOptions())
			if err != nil {
				t.Fatalf("fold: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("folded message:\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

// TestAggregatorOrdersBlocksAndKeepsThinking confirms multi-block folds keep
// block order and carry the thinking text plus its synthetic signature.
func TestAggregatorOrdersBlocksAndKeepsThinking(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(streamsDir, "reasoning_and_text.codex.sse"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	agg := stream.NewAggregator()
	tr := stream.New(goldenOptions())
	if _, err := tr.Run(context.Background(), bytes.NewReader(raw), agg); err != nil {
		t.Fatalf("run: %v", err)
	}
	msg, err := agg.Message()
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2: %+v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Type != "thinking" || msg.Content[1].Type != "text" {
		t.Errorf("block order = %s,%s want thinking,text", msg.Content[0].Type, msg.Content[1].Type)
	}
	if msg.Content[0].Thinking != "Thinking it through" {
		t.Errorf("thinking = %q", msg.Content[0].Thinking)
	}
	if !strings.HasPrefix(msg.Content[0].Signature, "utraque-synthetic-v1:") {
		t.Errorf("thinking signature = %q, want the synthetic marker prefix", msg.Content[0].Signature)
	}
	if msg.Content[1].Text != "The answer is 42." {
		t.Errorf("text = %q", msg.Content[1].Text)
	}
	if msg.Role != "assistant" || msg.Type != "message" {
		t.Errorf("role/type = %s/%s, want assistant/message", msg.Role, msg.Type)
	}
}

// TestAggregatorTwoInterleavedTools confirms parallel items that arrive
// interleaved fold into two ordered, individually valid tool_use blocks.
func TestAggregatorTwoInterleavedTools(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(streamsDir, "two_interleaved_tools.codex.sse"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	agg := stream.NewAggregator()
	tr := stream.New(goldenOptions())
	if _, err := tr.Run(context.Background(), bytes.NewReader(raw), agg); err != nil {
		t.Fatalf("run: %v", err)
	}
	msg, err := agg.Message()
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(msg.Content))
	}
	wantNames := []string{"foo", "bar"}
	wantArgs := []string{`{"a":1}`, `{"b":2}`}
	for i, b := range msg.Content {
		if b.Type != "tool_use" || b.Name != wantNames[i] {
			t.Errorf("block %d = %s/%s, want tool_use/%s", i, b.Type, b.Name, wantNames[i])
		}
		if string(b.Input) != wantArgs[i] {
			t.Errorf("block %d input = %s, want %s", i, b.Input, wantArgs[i])
		}
		if !json.Valid(b.Input) {
			t.Errorf("block %d input is not valid JSON: %s", i, b.Input)
		}
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", msg.StopReason)
	}
}

// TestAggregatorErrorPathSurfacesError confirms a mid-stream failure is
// reported as an error, NOT as a half message with the partial text in it. A
// non-streaming client cannot see a truncation the way a streaming one can, so
// presenting the fragment as a finished answer would be a lie.
func TestAggregatorErrorPathSurfacesError(t *testing.T) {
	for _, fixture := range []string{"midstream_error", "response_failed", "truncated_stream"} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(streamsDir, fixture+".codex.sse"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			agg := stream.NewAggregator()
			tr := stream.New(goldenOptions())
			res, runErr := tr.Run(context.Background(), bytes.NewReader(raw), agg)
			if runErr != nil {
				t.Fatalf("run: %v", runErr)
			}
			if !res.Errored {
				t.Fatalf("fixture %s should terminate in an error, got %+v", fixture, res)
			}
			if !agg.Failed() {
				t.Error("Failed() should report the mid-stream error")
			}

			msg, err := agg.Message()
			if err == nil {
				t.Fatalf("Message() returned a half message instead of an error: %+v", msg)
			}
			if msg != nil {
				t.Errorf("Message() must return a nil message alongside its error, got %+v", msg)
			}
			var ae *apierr.Error
			if !errors.As(err, &ae) {
				t.Fatalf("error %v is not an *apierr.Error the caller can render", err)
			}
			if ae.HTTPStatus() < 400 {
				t.Errorf("error status = %d, want a failure status", ae.HTTPStatus())
			}
			if _, jerr := agg.MessageJSON(); jerr == nil {
				t.Error("MessageJSON() must fail on a failed stream too")
			}
		})
	}
}

// TestAggregatorNoTerminusIsIncomplete confirms a fold with no terminus at all
// (a client interrupt, failure mode 3) refuses to produce a message.
func TestAggregatorNoTerminusIsIncomplete(t *testing.T) {
	// A stream that starts and opens a block but never terminates: feed the
	// sink directly, since the translator always resolves to some terminus.
	agg := stream.NewAggregator()
	if err := agg.MessageStart(stream.MessageStart{ID: "msg_x", Model: "gpt-5.6-sol", InputTokens: 3}); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	if _, err := agg.Message(); !errors.Is(err, stream.ErrIncomplete) {
		t.Errorf("Message() err = %v, want ErrIncomplete", err)
	}

	// Nothing at all emitted is the distinct mode-1 condition.
	empty := stream.NewAggregator()
	if _, err := empty.Message(); !errors.Is(err, stream.ErrNoData) {
		t.Errorf("empty Message() err = %v, want ErrNoData", err)
	}
}

// TestAggregatorRejectsOutOfOrderCalls confirms the sink reports a violation of
// the Sink contract rather than folding nonsense into a plausible-looking
// message.
func TestAggregatorRejectsOutOfOrderCalls(t *testing.T) {
	start := func() *stream.Aggregator {
		a := stream.NewAggregator()
		if err := a.MessageStart(stream.MessageStart{ID: "m", Model: "x"}); err != nil {
			t.Fatalf("message_start: %v", err)
		}
		return a
	}
	textBlock := aschema.ContentBlock{Type: aschema.BlockText}
	textDelta := func(s string) aschema.Delta {
		return aschema.Delta{Type: aschema.DeltaText, Text: s}
	}

	t.Run("block before message_start", func(t *testing.T) {
		if err := stream.NewAggregator().BlockStart(0, textBlock); err == nil {
			t.Error("want an error for content_block_start before message_start")
		}
	})
	t.Run("second message_start", func(t *testing.T) {
		a := start()
		if err := a.MessageStart(stream.MessageStart{ID: "m2"}); err == nil {
			t.Error("want an error for a second message_start")
		}
	})
	t.Run("two blocks open", func(t *testing.T) {
		a := start()
		if err := a.BlockStart(0, textBlock); err != nil {
			t.Fatalf("block_start: %v", err)
		}
		if err := a.BlockStart(1, textBlock); err == nil {
			t.Error("want an error for a second open block")
		}
	})
	t.Run("delta with no open block", func(t *testing.T) {
		a := start()
		if err := a.BlockDelta(0, textDelta("hi")); err == nil {
			t.Error("want an error for a delta with no open block")
		}
	})
	t.Run("stop mismatched index", func(t *testing.T) {
		a := start()
		if err := a.BlockStart(0, textBlock); err != nil {
			t.Fatalf("block_start: %v", err)
		}
		if err := a.BlockStop(1); err == nil {
			t.Error("want an error for a mismatched content_block_stop")
		}
	})
	t.Run("unknown delta type", func(t *testing.T) {
		a := start()
		if err := a.BlockStart(0, textBlock); err != nil {
			t.Fatalf("block_start: %v", err)
		}
		d := textDelta("hi")
		d.Type = "some_future_delta"
		if err := a.BlockDelta(0, d); err == nil {
			t.Error("want an error for an unfoldable delta type")
		}
	})
}

// TestAggregatorPingIsIgnored confirms a keepalive contributes nothing.
func TestAggregatorPingIsIgnored(t *testing.T) {
	a := stream.NewAggregator()
	if err := a.MessageStart(stream.MessageStart{ID: "m", Model: "x", InputTokens: 2}); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	if err := a.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := a.MessageDelta(stream.MessageDelta{StopReason: "end_turn"}); err != nil {
		t.Fatalf("message_delta: %v", err)
	}
	if err := a.MessageStop(); err != nil {
		t.Fatalf("message_stop: %v", err)
	}
	msg, err := a.Message()
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	if len(msg.Content) != 0 {
		t.Errorf("ping produced content: %+v", msg.Content)
	}
	// A zero upstream input count must not erase the message_start estimate.
	if msg.Usage.InputTokens != 2 {
		t.Errorf("input_tokens = %d, want the message_start estimate 2", msg.Usage.InputTokens)
	}
}

// TestAggregatorMidStreamFailureIs502: an upstream that opened a stream and then
// failed is a GATEWAY failure, not an internal one. The distinction is what
// tells the caller whether retrying could help, and it is the status the
// streaming path's siblings (ErrNoData, ErrTruncated) already use.
func TestAggregatorMidStreamFailureIs502(t *testing.T) {
	for _, fixture := range []string{"midstream_error", "response_failed", "truncated_stream"} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(streamsDir, fixture+".codex.sse"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			agg := stream.NewAggregator()
			tr := stream.New(goldenOptions())
			if _, err := tr.Run(context.Background(), bytes.NewReader(raw), agg); err != nil {
				t.Fatalf("run: %v", err)
			}
			_, err = agg.Message()
			if err == nil {
				t.Fatal("a failed stream folded into a message")
			}
			var ae *apierr.Error
			if !errors.As(err, &ae) {
				t.Fatalf("error %v is not an *apierr.Error", err)
			}
			if ae.HTTPStatus() != 502 {
				t.Errorf("status = %d, want 502", ae.HTTPStatus())
			}
		})
	}
}
