package tokens

import (
	aschema "github.com/hughescr/utraque/internal/anthropic/schema"
)

// Estimator is the model-agnostic view: it estimates an Anthropic-shaped
// prompt as the client sent it, before any backend translation. It serves a
// leg whose backend has no exact tokenizer here (DeepSeek), and its numbers
// are heuristics, never lower bounds. The Codex leg uses RequestEstimator.
// Implementations must be safe for concurrent use and must never return a
// negative count.
type Estimator interface {
	// EstimateString estimates the tokens in one string.
	EstimateString(s string) int
	// EstimatePrompt estimates the input tokens of a whole prompt.
	EstimatePrompt(p Prompt) int
	// Name identifies the estimator in logs and /healthz, so an operator can
	// see which method produced a count.
	Name() string
}

// Default returns the model-agnostic heuristic (chars/4). It is NOT what the
// Codex leg uses — see Codex — and it is not an exact count of anything.
func Default() Estimator { return CharsPerToken{} }

// Prompt is the model-agnostic view of everything in an Anthropic request that
// contributes to an input token count. It exists so one Estimator serves both
// /v1/messages and /v1/messages/count_tokens without either request type
// leaking into the estimator interface.
type Prompt struct {
	System   *aschema.Content
	Messages []aschema.Message
	Tools    []aschema.Tool
}

// PromptFromMessages views a Messages request as a Prompt.
func PromptFromMessages(req *aschema.MessagesRequest) Prompt {
	if req == nil {
		return Prompt{}
	}
	return Prompt{System: req.System, Messages: req.Messages, Tools: req.Tools}
}

// PromptFromCountTokens views a count_tokens request as a Prompt.
func PromptFromCountTokens(req *aschema.CountTokensRequest) Prompt {
	if req == nil {
		return Prompt{}
	}
	return Prompt{System: req.System, Messages: req.Messages, Tools: req.Tools}
}

// Count estimates a count_tokens request and returns the API response body.
func Count(e Estimator, req *aschema.CountTokensRequest) aschema.CountTokensResponse {
	if e == nil {
		e = Default()
	}
	return aschema.CountTokensResponse{InputTokens: e.EstimatePrompt(PromptFromCountTokens(req))}
}

// EstimatePrompt estimates the input tokens of a whole Anthropic-shaped prompt.
// Character costs across the entire prompt are summed first and divided ONCE,
// so the estimate does not inflate with the number of pieces the same text is
// split across; discrete framing costs are added as whole tokens on top.
func (e CharsPerToken) EstimatePrompt(p Prompt) int {
	c := promptCounter{imageTokens: settingOr(e.ImageTokens, DefaultImageTokens)}

	c.content(p.System)

	perMessage := settingOr(e.PerItemOverhead, DefaultPerItemOverhead)
	for i := range p.Messages {
		c.tokens += perMessage
		c.chars += len(p.Messages[i].Role)
		c.content(p.Messages[i].Content)
	}

	perTool := settingOr(e.PerToolOverhead, DefaultPerToolOverhead)
	for i := range p.Tools {
		t := &p.Tools[i]
		c.tokens += perTool
		c.chars += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}

	return c.tokens + ceilDiv(c.chars, e.divisor())
}

// promptCounter accumulates the two cost kinds separately: chars are divided
// once at the end, tokens are already whole.
type promptCounter struct {
	chars       int
	tokens      int
	imageTokens int
}

// content folds one Content union. Nested tool_result content recurses.
func (c *promptCounter) content(x *aschema.Content) {
	if x == nil {
		return
	}
	for i := range x.Blocks {
		c.block(&x.Blocks[i])
	}
}

func (c *promptCounter) block(b *aschema.ContentBlock) {
	switch b.Type {
	case aschema.BlockText:
		c.chars += len(b.Text)
	case aschema.BlockThinking:
		// The signature is a credential, not prose the model reads.
		c.chars += len(b.Thinking)
	case aschema.BlockRedactedThinking:
		// Opaque ciphertext: its length says nothing about its token cost.
	case aschema.BlockImage, aschema.BlockDocument:
		c.tokens += c.imageTokens
	case aschema.BlockToolUse:
		c.chars += len(b.Name) + len(b.Input)
	case aschema.BlockToolResult:
		c.chars += len(b.ToolUseID)
		c.content(b.Content)
	default:
		// An unrecognised block still costs something; charge what we can read
		// rather than silently pricing it at zero.
		c.chars += len(b.Text) + len(b.Input)
	}
}
