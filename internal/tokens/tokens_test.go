package tokens_test

import (
	"encoding/json"
	"strings"
	"testing"

	aschema "github.com/hughescr/utraque/internal/anthropic/schema"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/tokens"
)

// --- the exact o200k_base estimator -----------------------------------------

func exact(t testing.TB) *tokens.O200kBase {
	t.Helper()
	e, err := tokens.O200k()
	if err != nil {
		t.Fatalf("O200k: %v", err)
	}
	return e
}

// TestCodexIsExact pins the construction-site contract: the Codex leg's
// estimator is the real tokenizer, and it says so in its Name.
func TestCodexIsExact(t *testing.T) {
	e := tokens.Codex()
	if _, ok := e.(*tokens.O200kBase); !ok {
		t.Fatalf("Codex() = %T, want *tokens.O200kBase", e)
	}
	if got := e.Name(); got != tokens.O200kName {
		t.Errorf("Name() = %q, want %q", got, tokens.O200kName)
	}
}

// TestKnownAnswers pins o200k_base counts. "hello world" is the published
// two-token example; the multi-byte and code cases were counted once with the
// library and pinned, so a vocabulary or pre-tokenizer regression is visible.
func TestKnownAnswers(t *testing.T) {
	e := exact(t)
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello world", 2},
		{"héllo wörld 日本語 🚀🚀", 11},
		{"func main() {\n\tfmt.Println(\"hi\")\n}\n", 10},
	}
	for _, c := range cases {
		if got := e.EstimateString(c.in); got != c.want {
			t.Errorf("EstimateString(%q) = %d, want %d", c.in, got, c.want)
		}
		// The memo must not change the answer on the second ask.
		if got := e.EstimateString(c.in); got != c.want {
			t.Errorf("EstimateString(%q) second call = %d, want %d", c.in, got, c.want)
		}
	}
}

// fixtureRequest is a realistic translated turn: instructions, a tool set with
// a real schema, a user message, a replayed encrypted reasoning item, a
// function call and its output, and an image part.
func fixtureRequest(blob string) *cschema.ResponsesRequest {
	return &cschema.ResponsesRequest{
		Model:        "gpt-5.6-sol",
		Instructions: "You are Claude Code, Anthropic's official CLI for Claude.\n\nAlways answer tersely.",
		Input: []cschema.InputItem{
			cschema.MessageItem(aschema.RoleUser, cschema.InputText("Which file holds the port?")),
			cschema.ReasoningItem("rs_abc123", blob),
			cschema.MessageItem(aschema.RoleAssistant, cschema.OutputText("Let me look.")),
			cschema.FunctionCall("call_01", "Grep", `{"pattern":"8317","path":"internal/config"}`),
			cschema.FunctionCallOutput("call_01", "internal/config/config.go:41:\tPort int `env:\"UTRAQUE_PORT\"`"),
			cschema.MessageItem(aschema.RoleUser,
				cschema.InputText("And this screenshot?"),
				cschema.InputImage("data:image/png;base64,"+strings.Repeat("iVBORw0KGgo", 400))),
		},
		Tools: []cschema.Tool{
			cschema.FunctionTool("Grep", "Search file contents with a regular expression.",
				json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"The regex to search for"},"path":{"type":"string","description":"Directory to search in"},"output_mode":{"type":"string","enum":["content","files_with_matches","count"],"default":"files_with_matches"}},"required":["pattern"],"additionalProperties":false,"$schema":"http://json-schema.org/draft-07/schema#"}`)),
			cschema.FunctionTool("Bash", "Run a shell command.",
				json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The command to execute"},"timeout":{"type":"number","description":"Timeout in milliseconds","minimum":0}},"required":["command"]}`)),
		},
		ToolChoice:     cschema.AutoChoice(),
		Reasoning:      &cschema.Reasoning{Effort: "high"},
		Include:        []string{cschema.IncludeReasoningEncryptedContent},
		PromptCacheKey: "utq-507508c4d8e8edbb2c44fe299546f59b",
	}
}

// TestLowerBoundRule is the design invariant on a realistic request:
//
//   - the encrypted reasoning blob is skipped: a 4 KB ciphertext costs the
//     same as none at all;
//   - the image is skipped: dropping the data URL changes nothing;
//   - no framing is charged: the count equals the sum of the counted fields
//     with nothing added per item or per tool;
//   - the whole thing is strictly below the same request tokenized as it is
//     serialised on the wire, which is the cheapest upper bound on any
//     rendering that wraps the same text in framing.
func TestLowerBoundRule(t *testing.T) {
	e := exact(t)
	blob := strings.Repeat("ENCRYPTEDBLOBAAA==", 256)
	withBlob := e.EstimateRequest(fixtureRequest(blob))
	noBlob := e.EstimateRequest(fixtureRequest(""))
	if withBlob != noBlob {
		t.Errorf("encrypted_content changed the count: %d with a %d-byte blob, %d without", withBlob, len(blob), noBlob)
	}

	noImage := fixtureRequest("")
	noImage.Input[5].Content = noImage.Input[5].Content[:1]
	if got := e.EstimateRequest(noImage); got != noBlob {
		t.Errorf("the image data URL changed the count: %d with, %d without", noBlob, got)
	}

	// The per-field sum with no framing. Tools are counted through the schema
	// walk, so they are checked separately below; here the item fields.
	req := fixtureRequest("")
	fields := []string{
		req.Instructions,
		"Which file holds the port?",
		"Let me look.",
		"Grep", `{"pattern":"8317","path":"internal/config"}`,
		"internal/config/config.go:41:\tPort int `env:\"UTRAQUE_PORT\"`",
		"And this screenshot?",
	}
	sum := 0
	for _, f := range fields {
		sum += e.EstimateString(f)
	}
	req.Tools = nil
	if got := e.EstimateRequest(req); got != sum {
		t.Errorf("items only = %d, want the bare per-field sum %d (no framing may be charged)", got, sum)
	}

	// The wire form carries every field plus JSON framing, the blob and the
	// image, so it must be strictly larger.
	full := fixtureRequest(blob)
	wire, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	if wireTokens := e.EstimateString(string(wire)); withBlob >= wireTokens {
		t.Errorf("count %d is not strictly below the wire form's %d tokens", withBlob, wireTokens)
	}
	if withBlob <= 0 {
		t.Errorf("count = %d, want positive", withBlob)
	}
}

// TestToolSchemaCountsProseNotJSON pins the tool rule: a declaration costs its
// name, description and the prose the backend's rendering shows the model
// (property names, descriptions, enum values, types, defaults) — never the
// JSON punctuation, the schema keywords, or constraints the rendering omits.
func TestToolSchemaCountsProseNotJSON(t *testing.T) {
	e := exact(t)
	one := func(tool cschema.Tool) int {
		return e.EstimateRequest(&cschema.ResponsesRequest{Tools: []cschema.Tool{tool}})
	}

	schema := json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"The city and state, e.g. San Francisco, CA"},"format":{"type":"string","enum":["celsius","fahrenheit"],"default":"celsius"}},"required":["location"],"additionalProperties":false}`)
	got := one(cschema.FunctionTool("get_weather", "Gets the current weather in the provided location.", schema))

	want := 0
	for _, s := range []string{
		"get_weather", "Gets the current weather in the provided location.",
		"object",
		"location", "string", "The city and state, e.g. San Francisco, CA",
		"format", "string", "celsius", "fahrenheit", "celsius",
	} {
		want += e.EstimateString(s)
	}
	if got != want {
		t.Errorf("tool = %d tokens, want the prose sum %d", got, want)
	}
	if asJSON := e.EstimateString(string(schema)); got >= asJSON {
		t.Errorf("tool = %d tokens, not below its JSON schema's %d: punctuation is being charged", got, asJSON)
	}

	// Keywords the rendering never shows cost nothing, so adding them cannot
	// move the count; the same schema reformatted with whitespace cannot
	// either.
	noisy := json.RawMessage(`{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"properties": {
			"location": {"type": "string", "description": "The city and state, e.g. San Francisco, CA", "minLength": 1, "pattern": "^.+$"},
			"format": {"type": "string", "enum": ["celsius", "fahrenheit"], "default": "celsius"}
		},
		"required": ["location"],
		"additionalProperties": false
	}`)
	if again := one(cschema.FunctionTool("get_weather", "Gets the current weather in the provided location.", noisy)); again != got {
		t.Errorf("reformatted schema with extra keywords = %d, want %d", again, got)
	}

	// Nested schemas are recursed into, so their prose is not lost.
	nested := json.RawMessage(`{"type":"object","properties":{"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string","description":"Text to replace"}}}}}}`)
	n := one(cschema.FunctionTool("MultiEdit", "", nested))
	deep := 0
	for _, s := range []string{"MultiEdit", "object", "edits", "array", "object", "old_string", "string", "Text to replace"} {
		deep += e.EstimateString(s)
	}
	if n != deep {
		t.Errorf("nested schema = %d, want %d", n, deep)
	}

	// An unparseable schema is charged nothing beyond its name and
	// description; the backend would reject it before billing it.
	bad := one(cschema.FunctionTool("x", "", json.RawMessage(`{not json`)))
	if bad != e.EstimateString("x") {
		t.Errorf("broken schema = %d, want just the name's %d", bad, e.EstimateString("x"))
	}
}

// TestFieldsAreCountedSeparately: the count is per field, summed, so text that
// would merge across a boundary if concatenated does not.
func TestFieldsAreCountedSeparately(t *testing.T) {
	e := exact(t)
	req := &cschema.ResponsesRequest{Input: []cschema.InputItem{
		cschema.MessageItem(aschema.RoleUser, cschema.InputText("hello"), cschema.InputText(" world")),
	}}
	if got, want := e.EstimateRequest(req), e.EstimateString("hello")+e.EstimateString(" world"); got != want {
		t.Errorf("two parts = %d, want %d", got, want)
	}
}

// TestUnknownItemTypeIsStillCharged: an item type this package does not know
// is still sent, so its readable text is counted rather than silently free.
func TestUnknownItemTypeIsStillCharged(t *testing.T) {
	e := exact(t)
	out := "some output"
	req := &cschema.ResponsesRequest{Input: []cschema.InputItem{
		{Type: "custom_tool_call_output", Output: &out},
	}}
	if got := e.EstimateRequest(req); got != e.EstimateString(out) {
		t.Errorf("unknown item = %d, want %d", got, e.EstimateString(out))
	}
}

func TestNilRequestIsZero(t *testing.T) {
	if got := exact(t).EstimateRequest(nil); got != 0 {
		t.Errorf("nil request = %d, want 0", got)
	}
	if got := (tokens.CharsPerToken{}).EstimateRequest(nil); got != 0 {
		t.Errorf("chars nil request = %d, want 0", got)
	}
}

// TestMonotonicInPromptSize confirms adding content never lowers the count.
func TestMonotonicInPromptSize(t *testing.T) {
	e := exact(t)
	prev := 0
	body := ""
	for i := 0; i < 20; i++ {
		body += "some more conversation text. "
		got := e.EstimateRequest(&cschema.ResponsesRequest{Input: []cschema.InputItem{
			cschema.MessageItem(aschema.RoleUser, cschema.InputText(body)),
		}})
		if got < prev {
			t.Fatalf("count fell from %d to %d as the prompt grew", prev, got)
		}
		prev = got
	}
}

// --- the chars/N heuristic (Default, and the Codex fallback) ----------------

func TestDefaultIsTheHeuristic(t *testing.T) {
	if got := tokens.Default().Name(); got != "chars/4" {
		t.Errorf("Default().Name() = %q, want chars/4", got)
	}
	if got := (tokens.CharsPerToken{Divisor: 3}).Name(); got != "chars/3" {
		t.Errorf("Name() = %q, want chars/3", got)
	}
}

func TestEstimateStringRoundsUp(t *testing.T) {
	e := tokens.Default()
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},    // never zero for non-empty input
		{"abcd", 1}, // exactly the divisor
		{"abcde", 2},
		{strings.Repeat("x", 400), 100},
	}
	for _, c := range cases {
		if got := e.EstimateString(c.in); got != c.want {
			t.Errorf("EstimateString(%d chars) = %d, want %d", len(c.in), got, c.want)
		}
	}
}

func TestZeroValueUsesDefaults(t *testing.T) {
	var zero tokens.CharsPerToken
	explicit := tokens.CharsPerToken{
		Divisor:         tokens.DefaultDivisor,
		PerItemOverhead: tokens.DefaultPerItemOverhead,
		PerToolOverhead: tokens.DefaultPerToolOverhead,
		ImageTokens:     tokens.DefaultImageTokens,
	}
	p := samplePrompt()
	if a, b := zero.EstimatePrompt(p), explicit.EstimatePrompt(p); a != b {
		t.Errorf("zero value = %d, explicit defaults = %d; the zero value must be usable", a, b)
	}
	r := fixtureRequest("")
	if a, b := zero.EstimateRequest(r), explicit.EstimateRequest(r); a != b {
		t.Errorf("request: zero value = %d, explicit defaults = %d", a, b)
	}
}

// TestPromptViewsAgree confirms a /v1/messages body and the equivalent
// count_tokens body estimate identically under the heuristic.
func TestPromptViewsAgree(t *testing.T) {
	msgs := []aschema.Message{{Role: aschema.RoleUser, Content: aschema.StringContent("how much is that?")}}
	sys := aschema.StringContent("You are terse.")
	tools := []aschema.Tool{{Name: "now", Description: "current time", InputSchema: json.RawMessage(`{"type":"object"}`)}}

	full := tokens.PromptFromMessages(&aschema.MessagesRequest{
		Model: "sol", Messages: msgs, System: sys, Tools: tools, MaxTokens: 100,
	})
	count := tokens.PromptFromCountTokens(&aschema.CountTokensRequest{
		Model: "sol", Messages: msgs, System: sys, Tools: tools,
	})

	e := tokens.Default()
	if a, b := e.EstimatePrompt(full), e.EstimatePrompt(count); a != b {
		t.Errorf("messages view = %d, count_tokens view = %d", a, b)
	}
	if got := tokens.Count(e, &aschema.CountTokensRequest{
		Model: "sol", Messages: msgs, System: sys, Tools: tools,
	}); got.InputTokens != e.EstimatePrompt(count) {
		t.Errorf("Count() = %d, want %d", got.InputTokens, e.EstimatePrompt(count))
	}
}

func TestNilInputsAreZero(t *testing.T) {
	e := tokens.Default()
	if got := e.EstimatePrompt(tokens.Prompt{}); got != 0 {
		t.Errorf("empty prompt = %d, want 0", got)
	}
	if got := e.EstimatePrompt(tokens.PromptFromMessages(nil)); got != 0 {
		t.Errorf("nil messages request = %d, want 0", got)
	}
	if got := tokens.Count(nil, nil).InputTokens; got != 0 {
		t.Errorf("Count(nil, nil) = %d, want 0", got)
	}
}

// TestCharCostsDivideOnce is the property that keeps the heuristic stable under
// re-chunking: the same text split across many blocks must not cost more than
// the same text in one block.
func TestCharCostsDivideOnce(t *testing.T) {
	whole := strings.Repeat("abcdefghij", 40) // 400 chars
	oneBlock := []aschema.ContentBlock{aschema.TextBlock(whole)}
	many := make([]aschema.ContentBlock, 0, 400)
	for _, r := range whole {
		many = append(many, aschema.TextBlock(string(r)))
	}

	e := tokens.Default()
	a := e.EstimatePrompt(tokens.Prompt{Messages: []aschema.Message{
		{Role: aschema.RoleUser, Content: aschema.BlockContent(oneBlock...)},
	}})
	b := e.EstimatePrompt(tokens.Prompt{Messages: []aschema.Message{
		{Role: aschema.RoleUser, Content: aschema.BlockContent(many...)},
	}})
	if a != b {
		t.Errorf("400 chars in 1 block = %d, in 400 blocks = %d; character costs must be summed then divided once", a, b)
	}
}

// TestBlockKindsAreCounted walks every content block kind the heuristic can
// see, so a new kind added to the schema without a cost here is visible as a
// zero-cost block rather than silently free.
func TestBlockKindsAreCounted(t *testing.T) {
	e := tokens.CharsPerToken{PerItemOverhead: -1, PerToolOverhead: -1}
	cost := func(b aschema.ContentBlock) int {
		return e.EstimatePrompt(tokens.Prompt{Messages: []aschema.Message{
			{Role: "", Content: aschema.BlockContent(b)},
		}})
	}

	if text := cost(aschema.TextBlock(strings.Repeat("a", 40))); text != 10 {
		t.Errorf("text block = %d tokens, want 10", text)
	}
	if thinking := cost(aschema.ThinkingBlock(strings.Repeat("t", 40), "sig-should-not-count-abcdefghijklmnop")); thinking != 10 {
		t.Errorf("thinking block = %d tokens, want 10 (the signature must not be charged)", thinking)
	}
	if toolUse := cost(aschema.ToolUseBlock("id", "get_weather", json.RawMessage(`{"location":"SF"}`))); toolUse == 0 {
		t.Error("tool_use block cost nothing")
	}
	toolResult := cost(aschema.ContentBlock{
		Type:      aschema.BlockToolResult,
		ToolUseID: "id",
		Content:   aschema.BlockContent(aschema.TextBlock(strings.Repeat("r", 40))),
	})
	if toolResult < 10 {
		t.Errorf("tool_result block = %d tokens; its nested content must be counted", toolResult)
	}
	image := cost(aschema.ContentBlock{
		Type:   aschema.BlockImage,
		Source: &aschema.Source{Type: "base64", MediaType: "image/png", Data: strings.Repeat("A", 100_000)},
	})
	if image != tokens.DefaultImageTokens {
		t.Errorf("image block = %d tokens, want the flat %d", image, tokens.DefaultImageTokens)
	}
}

// TestHeuristicOverheadsApply confirms the per-item and per-tool framing costs
// are real and disableable, on both views.
func TestHeuristicOverheadsApply(t *testing.T) {
	p := tokens.Prompt{
		Messages: []aschema.Message{
			{Role: "", Content: aschema.BlockContent()},
			{Role: "", Content: aschema.BlockContent()},
		},
		Tools: []aschema.Tool{{Name: ""}},
	}
	want := 2*tokens.DefaultPerItemOverhead + tokens.DefaultPerToolOverhead
	if with := (tokens.CharsPerToken{}).EstimatePrompt(p); with != want {
		t.Errorf("prompt overheads = %d, want %d", with, want)
	}
	if without := (tokens.CharsPerToken{PerItemOverhead: -1, PerToolOverhead: -1}).EstimatePrompt(p); without != 0 {
		t.Errorf("prompt disabled overheads = %d, want 0", without)
	}

	r := &cschema.ResponsesRequest{
		Input: []cschema.InputItem{cschema.MessageItem("user"), cschema.MessageItem("user")},
		Tools: []cschema.Tool{{}},
	}
	if with := (tokens.CharsPerToken{}).EstimateRequest(r); with != want {
		t.Errorf("request overheads = %d, want %d", with, want)
	}
	if without := (tokens.CharsPerToken{PerItemOverhead: -1, PerToolOverhead: -1}).EstimateRequest(r); without != 0 {
		t.Errorf("request disabled overheads = %d, want 0", without)
	}
	img := &cschema.ResponsesRequest{Input: []cschema.InputItem{
		cschema.MessageItem("user", cschema.InputImage("data:image/png;base64,"+strings.Repeat("A", 100_000))),
	}}
	if got := (tokens.CharsPerToken{PerItemOverhead: -1}).EstimateRequest(img); got != tokens.DefaultImageTokens {
		t.Errorf("request image = %d, want the flat %d", got, tokens.DefaultImageTokens)
	}
}

// TestMultibyteCountedAsBytes documents the heuristic's deliberate choice:
// UTF-8 bytes, not runes, because CJK costs far more than a rune count would
// suggest.
func TestMultibyteCountedAsBytes(t *testing.T) {
	e := tokens.Default()
	const cjk = "日本語のテキストです今日は"
	got := e.EstimateString(cjk)
	if got != len(cjk)/4 && got != (len(cjk)+3)/4 {
		t.Errorf("EstimateString(CJK) = %d, want ceil(%d bytes / 4)", got, len(cjk))
	}
	if got <= len([]rune(cjk))/4 {
		t.Errorf("CJK estimate %d is no better than a rune count; bytes must be used", got)
	}
}

func samplePrompt() tokens.Prompt {
	return tokens.Prompt{
		System: aschema.StringContent("You are a careful assistant."),
		Messages: []aschema.Message{
			{Role: aschema.RoleUser, Content: aschema.StringContent("what is the weather in SF?")},
			{Role: aschema.RoleAssistant, Content: aschema.BlockContent(
				aschema.ThinkingBlock("checking", "sig"),
				aschema.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"location":"SF"}`)),
			)},
			{Role: aschema.RoleUser, Content: aschema.BlockContent(aschema.ContentBlock{
				Type:      aschema.BlockToolResult,
				ToolUseID: "call_1",
				Content:   aschema.BlockContent(aschema.TextBlock("18C and foggy")),
			})},
		},
		Tools: []aschema.Tool{{
			Name:        "get_weather",
			Description: "look up the weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
		}},
	}
}
