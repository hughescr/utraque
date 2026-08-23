package anthropic

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/transport"
)

func TestSanitizeLeavesUnmarkedBodyByteIdentical(t *testing.T) {
	// A real thinking block with a genuine-looking Anthropic signature. No
	// marker anywhere, so not one byte may change.
	body := []byte(`{"model":"claude-opus-5","messages":[` +
		`{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"let me consider","signature":"ErUBCkYIBRgCIkAx"},` +
		`{"type":"text","text":"hello"}]}],"max_tokens":1024}`)

	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if changed {
		t.Error("changed = true, want false for an unmarked body")
	}
	if string(got) != string(body) {
		t.Errorf("body was rewritten:\n got %s\nwant %s", got, body)
	}
	if &got[0] != &body[0] {
		t.Error("Sanitize allocated a new slice for an unmarked body; it must return the input untouched")
	}
}

func TestSanitizeStripsMarkedThinkingBlocks(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"gpt reasoning","signature":"` + SyntheticThinkingMarker + `abc"},` +
		`{"type":"text","text":"hello"}]}],"max_tokens":1024}`)

	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if HasSyntheticThinking(got) {
		t.Errorf("marker survived sanitizing: %s", got)
	}

	var out struct {
		Model     string           `json:"model"`
		MaxTokens int              `json:"max_tokens"`
		Messages  []schema.Message `json:"messages"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("sanitized body is not valid JSON: %v (%s)", err, got)
	}
	if out.Model != "claude-opus-5" || out.MaxTokens != 1024 {
		t.Errorf("unrelated fields changed: model=%q max_tokens=%d", out.Model, out.MaxTokens)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(out.Messages))
	}
	blocks := out.Messages[1].Content.Blocks
	if len(blocks) != 1 || blocks[0].Type != schema.BlockText || blocks[0].Text != "hello" {
		t.Errorf("assistant blocks = %+v, want only the text block", blocks)
	}
}

func TestSanitizeStripsRedactedThinkingAndDropsEmptiedMessage(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[` +
		`{"type":"redacted_thinking","data":"` + SyntheticThinkingMarker + `xyz"}]},` +
		`{"role":"user","content":"again"}]}`)

	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	var out struct {
		Messages []schema.Message `json:"messages"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("sanitized body is not valid JSON: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (the emptied assistant message must be dropped, not left with [])", len(out.Messages))
	}
	if out.Messages[0].Content.Text() != "hi" || out.Messages[1].Content.Text() != "again" {
		t.Errorf("surviving messages = %+v", out.Messages)
	}
}

func TestSanitizePreservesUnknownTopLevelFieldsAndKeyOrder(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5",` +
		`"future_field":{"nested":[1,2,3]},` +
		`"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"x","signature":"` + SyntheticThinkingMarker + `s"},` +
		`{"type":"text","text":"kept"}]}],` +
		`"metadata":{"user_id":"u1"}}`)

	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	s := string(got)
	if !strings.Contains(s, `"future_field":{"nested":[1,2,3]}`) {
		t.Errorf("unmodelled field was lost or re-encoded: %s", s)
	}
	if !strings.Contains(s, `"metadata":{"user_id":"u1"}`) {
		t.Errorf("metadata was lost: %s", s)
	}
	iModel := strings.Index(s, `"model"`)
	iFuture := strings.Index(s, `"future_field"`)
	iMessages := strings.Index(s, `"messages"`)
	iMeta := strings.Index(s, `"metadata"`)
	if !(iModel < iFuture && iFuture < iMessages && iMessages < iMeta) {
		t.Errorf("key order changed: %s", s)
	}
}

func TestSanitizeMarkerHitButNoSyntheticBlock(t *testing.T) {
	// The marker appears in prose, not in a thinking block. Nothing to strip,
	// so the body must come back untouched.
	body := []byte(`{"messages":[{"role":"user","content":"what does ` +
		SyntheticThinkingMarker + ` mean?"}]}`)
	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if changed {
		t.Error("changed = true, want false")
	}
	if string(got) != string(body) {
		t.Errorf("body was rewritten:\n got %s\nwant %s", got, body)
	}
}

func TestSanitizeFailsOpenOnNonObject(t *testing.T) {
	body := []byte(`["` + SyntheticThinkingMarker + `"]`)
	got, changed, err := Sanitize(body)
	if err == nil {
		t.Error("err = nil, want an error for a non-object body")
	}
	if changed {
		t.Error("changed = true, want false")
	}
	if string(got) != string(body) {
		t.Error("body must be returned unchanged when sanitizing cannot proceed")
	}
}

func TestSanitizeNoMessagesKey(t *testing.T) {
	body := []byte(`{"note":"` + SyntheticThinkingMarker + `x"}`)
	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if changed || string(got) != string(body) {
		t.Errorf("body without a messages key must pass through unchanged, got changed=%v %s", changed, got)
	}
}

func TestSanitizeMessagesDoesNotMutateInput(t *testing.T) {
	in := []schema.Message{{
		Role: schema.RoleAssistant,
		Content: schema.BlockContent(
			schema.ThinkingBlock("x", SyntheticThinkingMarker+"s"),
			schema.TextBlock("kept"),
		),
	}}
	before := len(in[0].Content.Blocks)
	out, changed := SanitizeMessages(in)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if len(in[0].Content.Blocks) != before {
		t.Error("SanitizeMessages mutated its input")
	}
	if len(out[0].Content.Blocks) != 1 {
		t.Errorf("output blocks = %d, want 1", len(out[0].Content.Blocks))
	}
}

func TestSanitizerRunsOnThePassthroughPath(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	leg, err := New(upstream.URL, transport.NewStd(transport.DefaultOptions()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxy := httptest.NewServer(leg)
	defer proxy.Close()

	marked := `{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"gpt","signature":"` + SyntheticThinkingMarker + `s"},` +
		`{"type":"text","text":"hi"}]}]}`
	resp, err := noRedirectClient().Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(marked))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := cap.snapshot(); HasSyntheticThinking(got.body) {
		t.Errorf("synthetic marker reached upstream: %s", got.body)
	}

	// And an unmarked body still arrives byte-identical through the same path.
	plain := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`
	resp2, err := noRedirectClient().Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(plain))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	if got := cap.snapshot(); string(got.body) != plain {
		t.Errorf("unmarked body was altered:\n got %s\nwant %s", got.body, plain)
	}
}

func TestWithSanitizerOffForwardsMarkedBodyVerbatim(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL, WithSanitizer(false)))
	defer proxy.Close()

	marked := `{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"x","signature":"` + SyntheticThinkingMarker + `s"}]}]}`
	resp, err := noRedirectClient().Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(marked))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := cap.snapshot(); string(got.body) != marked {
		t.Errorf("with the sanitizer off the body must pass verbatim:\n got %s\nwant %s", got.body, marked)
	}
}

// TestSanitizeKeepsSignedBlockThatQuotesTheMarker is the self-referential
// case: a session developing utraque reasons about utraque's own source, so
// the marker appears in genuine, Anthropic-signed thinking text. Matching on
// that text would silently strip real signed blocks from replayed history.
func TestSanitizeKeepsSignedBlockThatQuotesTheMarker(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"the constant is ` + SyntheticThinkingMarker +
		` and it tags minted blocks","signature":"ErUBCkYIBBgCKk"},` +
		`{"type":"text","text":"hello"}]}]}`)

	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if changed {
		t.Errorf("changed = true; a genuine signed block was stripped for quoting the marker:\n%s", got)
	}
	if string(got) != string(body) {
		t.Errorf("body was rewritten:\n got %s\nwant %s", got, body)
	}
}

// TestSanitizePreservesUnmodelledBlockFields pins the fidelity contract inside
// the messages array, not just at the top level. Anthropic ships block fields
// this build has never heard of — citations, search-result payloads — and a
// sanitize must not reshape any of them.
func TestSanitizePreservesUnmodelledBlockFields(t *testing.T) {
	const cited = `{"type":"text","text":"cited","citations":[{"type":"char_location","cited_text":"x","document_index":0}],"some_future_field":42}`
	const searchResult = `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://example.com","title":"T","page_age":"1 day","encrypted_content":"ZZZ"}]}`

	body := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"x","signature":"` + SyntheticThinkingMarker + `s"},` +
		cited + `,` + searchResult + `]}]}`)

	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	s := string(got)
	if !strings.Contains(s, cited) {
		t.Errorf("text block was reshaped; citations and unknown fields must survive byte-for-byte:\n%s", s)
	}
	if !strings.Contains(s, searchResult) {
		t.Errorf("web_search_tool_result was reshaped; its content is result objects, not content blocks:\n%s", s)
	}
	if HasSyntheticThinking(got) {
		t.Errorf("marker survived sanitizing: %s", s)
	}
}

// TestSanitizePreservesMessageKeyOrderAndUntouchedMessages: only the message
// that actually carried a marked block may be rewritten.
func TestSanitizePreservesMessageKeyOrderAndUntouchedMessages(t *testing.T) {
	const untouched = `{  "role" : "user" ,  "content" : [ {"type":"text","text":"spaced out"} ] }`
	body := []byte(`{"messages":[` + untouched + `,` +
		`{"role":"assistant","cache_hint":"keep","content":[` +
		`{"type":"thinking","thinking":"x","signature":"` + SyntheticThinkingMarker + `s"},` +
		`{"type":"text","text":"kept"}]}]}`)

	got, changed, err := Sanitize(body)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	s := string(got)
	if !strings.Contains(s, untouched) {
		t.Errorf("an untouched message was re-encoded:\n%s", s)
	}
	if !strings.Contains(s, `"cache_hint":"keep"`) {
		t.Errorf("an unmodelled message key was lost:\n%s", s)
	}
	iRole := strings.Index(s, `"role":"assistant"`)
	iHint := strings.Index(s, `"cache_hint"`)
	iContent := strings.Index(s, `"content":[{"type":"text","text":"kept"}]`)
	if iRole < 0 || iHint < 0 || iContent < 0 || !(iRole < iHint && iHint < iContent) {
		t.Errorf("message key order changed:\n%s", s)
	}
}

// TestSanitizeReportsHeadlessToolUse: the shape the sanitizer cannot repair.
// Stripping is still the lesser failure, but the operator gets told, because
// it is the likely cause of an otherwise baffling 400 from Anthropic.
func TestSanitizeReportsHeadlessToolUse(t *testing.T) {
	body := []byte(`{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"x","signature":"` + SyntheticThinkingMarker + `s"},` +
		`{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}]}]}`)

	_, rep, err := SanitizeWithReport(body)
	if err != nil {
		t.Fatalf("SanitizeWithReport: %v", err)
	}
	if !rep.Changed || rep.Dropped != 1 {
		t.Fatalf("report = %+v, want one dropped block", rep)
	}
	if !rep.HeadlessToolUse {
		t.Error("HeadlessToolUse = false; a tool_use turn that lost its leading thinking block must be reported")
	}
}

// TestSanitizeDoesNotReportHeadlessToolUseForTextOnlyTurns keeps the warning
// meaningful: a turn with no tool_use is not at risk.
func TestSanitizeDoesNotReportHeadlessToolUseForTextOnlyTurns(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"x","signature":"` + SyntheticThinkingMarker + `s"},` +
		`{"type":"text","text":"kept"}]}]}`)

	_, rep, err := SanitizeWithReport(body)
	if err != nil {
		t.Fatalf("SanitizeWithReport: %v", err)
	}
	if !rep.Changed {
		t.Fatal("changed = false, want true")
	}
	if rep.HeadlessToolUse {
		t.Error("HeadlessToolUse = true for a turn carrying no tool_use")
	}
}
