package tokens

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hughescr/utraque/internal/codex/schema"
)

// fresh builds an exact estimator over its own memo, so a test or benchmark
// can observe cold behaviour without the process-wide cache in the way.
func fresh(t testing.TB, capacity int) *O200kBase {
	t.Helper()
	shared, err := O200k()
	if err != nil {
		t.Fatalf("O200k: %v", err)
	}
	return &O200kBase{codec: shared.codec, memo: newMemo(capacity)}
}

func TestMemoHitsOnRepeat(t *testing.T) {
	e := fresh(t, 1024)
	req := &cschema.ResponsesRequest{
		Instructions: "be brief",
		Input: []cschema.InputItem{
			cschema.MessageItem("user", cschema.InputText("first turn")),
			cschema.FunctionCall("c1", "Grep", `{"pattern":"x"}`),
		},
		Tools: []cschema.Tool{cschema.FunctionTool("Grep", "search", json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}}}`))},
	}
	first := e.EstimateRequest(req)
	// instructions, one text part, name, arguments, one tool declaration.
	if got := e.memo.Len(); got != 5 {
		t.Fatalf("memo holds %d entries after one request, want 5", got)
	}
	if again := e.EstimateRequest(req); again != first || e.memo.Len() != 5 {
		t.Fatalf("second pass = %d (first %d), memo %d entries; repeats must hit", again, first, e.memo.Len())
	}
	// A new turn adds only its new fields.
	req.Input = append(req.Input, cschema.MessageItem("user", cschema.InputText("second turn")))
	if e.EstimateRequest(req) <= first || e.memo.Len() != 6 {
		t.Fatalf("a new field did not add exactly one entry: %d", e.memo.Len())
	}
}

// TestMemoIsBounded: the cache rotates generations and never holds more than
// two of them, however many distinct fields flow through it.
func TestMemoIsBounded(t *testing.T) {
	const capacity = 64
	e := fresh(t, capacity)
	for i := 0; i < 10*capacity; i++ {
		e.EstimateString(fmt.Sprintf("distinct field number %d", i))
		if n := e.memo.Len(); n > 2*capacity {
			t.Fatalf("memo grew to %d entries, cap is %d per generation", n, capacity)
		}
	}
	if n := e.memo.Len(); n < capacity {
		t.Fatalf("memo holds only %d entries after churn; the young generation should be full-ish", n)
	}
	// A field hit in the old generation is carried forward, so something in
	// steady use survives a rotation.
	survivor := "the field every turn repeats"
	e.EstimateString(survivor)
	for i := 0; i < 3*capacity; i++ {
		e.EstimateString(fmt.Sprintf("churn %d", i))
		if i%(capacity/2) == 0 {
			e.EstimateString(survivor)
		}
	}
	if _, ok := e.memo.get(survivor); !ok {
		t.Error("a field touched every half-generation was evicted")
	}
}

// TestMemoConcurrent is for the race detector: many goroutines counting the
// same and different fields through one cache.
func TestMemoConcurrent(t *testing.T) {
	e := fresh(t, 32)
	want := e.EstimateString("shared text that everyone counts")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if got := e.EstimateString("shared text that everyone counts"); got != want {
					t.Errorf("goroutine %d: got %d, want %d", g, got, want)
				}
				e.EstimateString(fmt.Sprintf("private %d/%d", g, i))
			}
		}(g)
	}
	wg.Wait()
}

// TestToolKeyDistinguishesFieldBoundaries: the tool memo hashes the three
// fields with separators, so moving bytes between fields is a different key.
func TestToolKeyDistinguishesFieldBoundaries(t *testing.T) {
	a := toolKey(&cschema.Tool{Name: "ab", Description: "c"})
	b := toolKey(&cschema.Tool{Name: "a", Description: "bc"})
	if a == b {
		t.Fatal("tool keys collided across a field boundary")
	}
}

// syntheticPrompt is a ~400 KB translated request shaped like a long agentic
// session: a system prompt, a Claude-Code-sized tool set, and turns of
// message / call / result with realistic mixed prose, code and JSON.
func syntheticPrompt(target int) *cschema.ResponsesRequest {
	req := &cschema.ResponsesRequest{
		Instructions: strings.Repeat("You are Claude Code, Anthropic's official CLI for Claude. Answer tersely. Prefer the smallest correct change.\n", 40),
	}
	schema := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The command to execute"},"description":{"type":"string","description":"Clear, concise description of what this command does in active voice."},"timeout":{"type":"number","description":"Optional timeout in milliseconds (max 600000)"},"run_in_background":{"type":"boolean","description":"Set to true to run this command in the background."}},"required":["command"],"additionalProperties":false}`)
	for i := 0; i < 20; i++ {
		req.Tools = append(req.Tools, cschema.FunctionTool(fmt.Sprintf("Tool%d", i),
			strings.Repeat("Executes a bash command and returns its output. Working directory persists between calls. ", 8), schema))
	}
	size := 0
	for turn := 0; size < target; turn++ {
		user := fmt.Sprintf("Turn %d: please look at internal/codex/leg/leg.go and explain how the seed reaches message_start.", turn)
		call := fmt.Sprintf(`{"command":"sed -n %d,%dp internal/codex/leg/leg.go","description":"Read part of the leg"}`, turn*40, turn*40+40)
		// Every field is distinct per turn (the turn number is woven in), so a
		// cold pass really tokenizes the whole prompt and the memo cannot
		// short-circuit a repeat within one request.
		var sb strings.Builder
		for line := 0; line < 12; line++ {
			fmt.Fprintf(&sb, "%4d\tfunc (l *Leg) serveStream%d(ctx context.Context, w http.ResponseWriter, rq *router.Request, upstream io.ReadCloser, seed *seed, log *slog.Logger) error {\n\tlw := newLazyWriter(w, func(h http.Header) {\n\t\th.Set(\"Content-Type\", \"text/event-stream\") // %d\n\t})\n", turn*40+line, turn, line)
		}
		result := sb.String()
		reply := fmt.Sprintf("Turn %d: the translator asks the seed for its value on the first event that needs it, so the count and the upstream request overlap.", turn)
		req.Input = append(req.Input,
			cschema.MessageItem("user", cschema.InputText(user)),
			cschema.FunctionCall(fmt.Sprintf("call_%d", turn), "Tool3", call),
			cschema.FunctionCallOutput(fmt.Sprintf("call_%d", turn), result),
			cschema.MessageItem("assistant", cschema.OutputText(reply)),
		)
		size += len(user) + len(call) + len(result) + len(reply)
	}
	return req
}

// promptBytes is the byte size of every counted field, for the benchmark's
// throughput figure.
func promptBytes(req *cschema.ResponsesRequest) int {
	v := &charsVisitor{}
	walkFields(req, v)
	return v.bytes
}

// BenchmarkEstimateRequestCold tokenizes a ~400 KB prompt from scratch on
// every iteration (a fresh memo each time).
func BenchmarkEstimateRequestCold(b *testing.B) {
	req := syntheticPrompt(400_000)
	bytes := promptBytes(req)
	shared := fresh(b, 16)
	var toks int
	b.SetBytes(int64(bytes))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := &O200kBase{codec: shared.codec, memo: newMemo(memoGenerationEntries)}
		toks = e.EstimateRequest(req)
	}
	b.ReportMetric(float64(toks)*float64(b.N)/b.Elapsed().Seconds(), "tok/s")
	b.ReportMetric(float64(toks), "tokens")
}

// BenchmarkEstimateRequestWarm counts the same ~400 KB prompt again with every
// field already memoized: the cost of an agentic turn that resends its history.
func BenchmarkEstimateRequestWarm(b *testing.B) {
	req := syntheticPrompt(400_000)
	bytes := promptBytes(req)
	e := fresh(b, memoGenerationEntries)
	toks := e.EstimateRequest(req)
	b.SetBytes(int64(bytes))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := e.EstimateRequest(req); got != toks {
			b.Fatalf("warm count %d != cold count %d", got, toks)
		}
	}
	b.ReportMetric(float64(toks)*float64(b.N)/b.Elapsed().Seconds(), "tok/s")
	b.ReportMetric(float64(toks), "tokens")
}
