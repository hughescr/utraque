package tokens_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/tokens"
)

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

func TestNameReportsDivisor(t *testing.T) {
	if got := tokens.Default().Name(); got != "chars/4" {
		t.Errorf("Name() = %q, want chars/4", got)
	}
	if got := (tokens.CharsPerToken{Divisor: 3}).Name(); got != "chars/3" {
		t.Errorf("Name() = %q, want chars/3", got)
	}
}

func TestZeroValueUsesDefaults(t *testing.T) {
	var zero tokens.CharsPerToken
	explicit := tokens.CharsPerToken{
		Divisor:            tokens.DefaultDivisor,
		PerMessageOverhead: tokens.DefaultPerMessageOverhead,
		PerToolOverhead:    tokens.DefaultPerToolOverhead,
		ImageTokens:        tokens.DefaultImageTokens,
	}
	p := samplePrompt()
	if a, b := zero.EstimatePrompt(p), explicit.EstimatePrompt(p); a != b {
		t.Errorf("zero value = %d, explicit defaults = %d; the zero value must be usable", a, b)
	}
}

// TestPromptViewsAgree confirms a /v1/messages body and the equivalent
// count_tokens body estimate identically — the context bar must match what the
// same conversation actually costs.
func TestPromptViewsAgree(t *testing.T) {
	msgs := []schema.Message{{Role: schema.RoleUser, Content: schema.StringContent("how much is that?")}}
	sys := schema.StringContent("You are terse.")
	tools := []schema.Tool{{Name: "now", Description: "current time", InputSchema: json.RawMessage(`{"type":"object"}`)}}

	full := tokens.PromptFromMessages(&schema.MessagesRequest{
		Model: "sol", Messages: msgs, System: sys, Tools: tools, MaxTokens: 100,
	})
	count := tokens.PromptFromCountTokens(&schema.CountTokensRequest{
		Model: "sol", Messages: msgs, System: sys, Tools: tools,
	})

	e := tokens.Default()
	if a, b := e.EstimatePrompt(full), e.EstimatePrompt(count); a != b {
		t.Errorf("messages view = %d, count_tokens view = %d", a, b)
	}
	if got := tokens.Count(e, &schema.CountTokensRequest{
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

// TestCharCostsDivideOnce is the property that keeps the estimate stable under
// re-chunking: the same text split across many blocks must not cost more than
// the same text in one block. Rounding each piece up separately would inflate
// a long tool-heavy conversation badly.
func TestCharCostsDivideOnce(t *testing.T) {
	whole := strings.Repeat("abcdefghij", 40) // 400 chars
	oneBlock := []schema.ContentBlock{schema.TextBlock(whole)}
	many := make([]schema.ContentBlock, 0, 400)
	for _, r := range whole {
		many = append(many, schema.TextBlock(string(r)))
	}

	e := tokens.Default()
	a := e.EstimatePrompt(tokens.Prompt{Messages: []schema.Message{
		{Role: schema.RoleUser, Content: schema.BlockContent(oneBlock...)},
	}})
	b := e.EstimatePrompt(tokens.Prompt{Messages: []schema.Message{
		{Role: schema.RoleUser, Content: schema.BlockContent(many...)},
	}})
	if a != b {
		t.Errorf("400 chars in 1 block = %d, in 400 blocks = %d; character costs must be summed then divided once", a, b)
	}
}

// TestMonotonicInPromptSize confirms adding content never lowers the estimate.
func TestMonotonicInPromptSize(t *testing.T) {
	e := tokens.Default()
	prev := 0
	body := ""
	for i := 0; i < 20; i++ {
		body += "some more conversation text. "
		got := e.EstimatePrompt(tokens.Prompt{Messages: []schema.Message{
			{Role: schema.RoleUser, Content: schema.StringContent(body)},
		}})
		if got < prev {
			t.Fatalf("estimate fell from %d to %d as the prompt grew", prev, got)
		}
		prev = got
	}
}

// TestBlockKindsAreCounted walks every content block kind the translator can
// see, so a new kind added to the schema without a cost here is visible as a
// zero-cost block rather than silently free.
func TestBlockKindsAreCounted(t *testing.T) {
	e := tokens.CharsPerToken{PerMessageOverhead: -1, PerToolOverhead: -1}
	cost := func(b schema.ContentBlock) int {
		return e.EstimatePrompt(tokens.Prompt{Messages: []schema.Message{
			{Role: "", Content: schema.BlockContent(b)},
		}})
	}

	text := cost(schema.TextBlock(strings.Repeat("a", 40)))
	if text != 10 {
		t.Errorf("text block = %d tokens, want 10", text)
	}

	thinking := cost(schema.ThinkingBlock(strings.Repeat("t", 40), "sig-should-not-count-abcdefghijklmnop"))
	if thinking != 10 {
		t.Errorf("thinking block = %d tokens, want 10 (the signature must not be charged)", thinking)
	}

	toolUse := cost(schema.ToolUseBlock("id", "get_weather", json.RawMessage(`{"location":"SF"}`)))
	if toolUse == 0 {
		t.Error("tool_use block cost nothing")
	}

	toolResult := cost(schema.ContentBlock{
		Type:      schema.BlockToolResult,
		ToolUseID: "id",
		Content:   schema.BlockContent(schema.TextBlock(strings.Repeat("r", 40))),
	})
	if toolResult < 10 {
		t.Errorf("tool_result block = %d tokens; its nested content must be counted", toolResult)
	}

	// An image is charged a flat figure, not the base64 length: a 100 KB image
	// through the text divisor would be ~25k tokens, which is nonsense.
	image := cost(schema.ContentBlock{
		Type:   schema.BlockImage,
		Source: &schema.Source{Type: "base64", MediaType: "image/png", Data: strings.Repeat("A", 100_000)},
	})
	if image != tokens.DefaultImageTokens {
		t.Errorf("image block = %d tokens, want the flat %d", image, tokens.DefaultImageTokens)
	}
}

// TestOverheadsApply confirms the per-message and per-tool framing costs are
// real and disableable.
func TestOverheadsApply(t *testing.T) {
	p := tokens.Prompt{
		Messages: []schema.Message{
			{Role: "", Content: schema.BlockContent()},
			{Role: "", Content: schema.BlockContent()},
		},
		Tools: []schema.Tool{{Name: ""}},
	}
	with := tokens.CharsPerToken{}.EstimatePrompt(p)
	want := 2*tokens.DefaultPerMessageOverhead + tokens.DefaultPerToolOverhead
	if with != want {
		t.Errorf("overheads = %d, want %d", with, want)
	}
	without := tokens.CharsPerToken{PerMessageOverhead: -1, PerToolOverhead: -1}.EstimatePrompt(p)
	if without != 0 {
		t.Errorf("disabled overheads = %d, want 0", without)
	}
}

// TestCustomDivisor confirms the divisor is honoured, which is what a future
// per-model calibration would turn.
func TestCustomDivisor(t *testing.T) {
	p := tokens.Prompt{Messages: []schema.Message{
		{Role: "", Content: schema.StringContent(strings.Repeat("z", 120))},
	}}
	four := tokens.CharsPerToken{PerMessageOverhead: -1}.EstimatePrompt(p)
	two := tokens.CharsPerToken{Divisor: 2, PerMessageOverhead: -1}.EstimatePrompt(p)
	if four != 30 || two != 60 {
		t.Errorf("divisor 4 = %d (want 30), divisor 2 = %d (want 60)", four, two)
	}
}

// TestMultibyteCountedAsBytes documents the deliberate choice: UTF-8 bytes, not
// runes, because CJK costs far more than a rune count would suggest.
func TestMultibyteCountedAsBytes(t *testing.T) {
	e := tokens.Default()
	// 12 CJK characters = 36 UTF-8 bytes -> 9 tokens; a rune count would say 3.
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
		System: schema.StringContent("You are a careful assistant."),
		Messages: []schema.Message{
			{Role: schema.RoleUser, Content: schema.StringContent("what is the weather in SF?")},
			{Role: schema.RoleAssistant, Content: schema.BlockContent(
				schema.ThinkingBlock("checking", "sig"),
				schema.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"location":"SF"}`)),
			)},
			{Role: schema.RoleUser, Content: schema.BlockContent(schema.ContentBlock{
				Type:      schema.BlockToolResult,
				ToolUseID: "call_1",
				Content:   schema.BlockContent(schema.TextBlock("18C and foggy")),
			})},
		},
		Tools: []schema.Tool{{
			Name:        "get_weather",
			Description: "look up the weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
		}},
	}
}
