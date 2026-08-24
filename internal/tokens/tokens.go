// Package tokens estimates the input token count of an Anthropic Messages
// prompt for the models utraque routes to the Codex backend.
//
// It is an ESTIMATE, deliberately and visibly so. The real GPT tokenizer is
// o200k_base; vendoring it (a ~2.5 MB merge table plus a BPE implementation) is
// a future improvement tracked in the plan. Until then a characters-per-token
// heuristic serves the two consumers that need a number rather than the truth:
//
//   - POST /v1/messages/count_tokens for a codex-routed model, which drives
//     Claude Code's context bar. A bar that is a few percent off is harmless; a
//     request that fails because we could not tokenize is not.
//   - The message_start usage.input_tokens seed, which the client shows before
//     the upstream reports real usage in message_delta.
//
// Anything billing-shaped must use the upstream's own reported usage, never
// this. Estimator exists so the swap to a real tokenizer is a one-line change
// at the construction site.
package tokens

import (
	"strconv"

	"github.com/hughescr/utraque/internal/anthropic/schema"
)

// Defaults for the heuristic. All are overridable per Estimator instance.
const (
	// DefaultDivisor is the UTF-8 bytes-per-token ratio. Four is the usual
	// rule of thumb for English text under o200k. Counting BYTES rather than
	// characters is deliberate: for Latin text the two are identical, and for
	// CJK — where o200k spends roughly one token per character, and each
	// character costs three UTF-8 bytes — bytes/4 lands far closer than
	// characters/4 would.
	DefaultDivisor = 4

	// DefaultPerMessageOverhead is the fixed cost of the role and turn framing
	// wrapped around each message.
	DefaultPerMessageOverhead = 4

	// DefaultPerToolOverhead is the fixed framing cost of one tool definition.
	DefaultPerToolOverhead = 8

	// DefaultImageTokens is the flat cost charged for one image or document
	// block. Real image cost is a function of tile geometry, which we cannot
	// compute without decoding the payload; charging base64 length through the
	// text divisor would overestimate by roughly an order of magnitude, so a
	// flat plausible figure is the more honest wrong answer.
	DefaultImageTokens = 1024
)

// Prompt is the model-agnostic view of everything that contributes to an input
// token count. It exists so one Estimator serves both /v1/messages and
// /v1/messages/count_tokens without either request type leaking into the
// estimator interface.
type Prompt struct {
	System   *schema.Content
	Messages []schema.Message
	Tools    []schema.Tool
}

// PromptFromMessages views a Messages request as a Prompt.
func PromptFromMessages(req *schema.MessagesRequest) Prompt {
	if req == nil {
		return Prompt{}
	}
	return Prompt{System: req.System, Messages: req.Messages, Tools: req.Tools}
}

// PromptFromCountTokens views a count_tokens request as a Prompt.
func PromptFromCountTokens(req *schema.CountTokensRequest) Prompt {
	if req == nil {
		return Prompt{}
	}
	return Prompt{System: req.System, Messages: req.Messages, Tools: req.Tools}
}

// Estimator turns text and prompts into token counts. Implementations must be
// safe for concurrent use and must never return a negative count.
type Estimator interface {
	// EstimateString estimates the tokens in one string.
	EstimateString(s string) int
	// EstimatePrompt estimates the input tokens of a whole prompt.
	EstimatePrompt(p Prompt) int
	// Name identifies the estimator in logs and /healthz, so an operator can
	// see which method produced a count.
	Name() string
}

// CharsPerToken is the default Estimator: a bytes-per-token heuristic with
// flat overheads for message framing, tool definitions, and images. The zero
// value is usable and applies every Default* constant.
type CharsPerToken struct {
	// Divisor is UTF-8 bytes per token; <= 0 uses DefaultDivisor.
	Divisor int
	// PerMessageOverhead is tokens of framing per message; < 0 disables it,
	// 0 uses DefaultPerMessageOverhead.
	PerMessageOverhead int
	// PerToolOverhead is tokens of framing per tool definition; < 0 disables
	// it, 0 uses DefaultPerToolOverhead.
	PerToolOverhead int
	// ImageTokens is the flat cost of one image or document block; < 0
	// disables it, 0 uses DefaultImageTokens.
	ImageTokens int
}

var _ Estimator = CharsPerToken{}

// Default returns the estimator used for codex-routed models.
func Default() Estimator { return CharsPerToken{} }

func (e CharsPerToken) divisor() int {
	if e.Divisor <= 0 {
		return DefaultDivisor
	}
	return e.Divisor
}

func settingOr(v, dflt int) int {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return dflt
	default:
		return v
	}
}

// Name reports the heuristic and its divisor, e.g. "chars/4".
func (e CharsPerToken) Name() string { return "chars/" + strconv.Itoa(e.divisor()) }

// EstimateString estimates the tokens in one string, rounding up so a non-empty
// string never estimates to zero tokens.
func (e CharsPerToken) EstimateString(s string) int {
	return ceilDiv(len(s), e.divisor())
}

// EstimatePrompt estimates the input tokens of a whole prompt. Character costs
// across the entire prompt are summed first and divided ONCE, so the estimate
// does not inflate with the number of pieces the same text is split across;
// discrete framing costs are added as whole tokens on top.
func (e CharsPerToken) EstimatePrompt(p Prompt) int {
	c := counter{imageTokens: settingOr(e.ImageTokens, DefaultImageTokens)}

	c.content(p.System)

	perMessage := settingOr(e.PerMessageOverhead, DefaultPerMessageOverhead)
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

// counter accumulates the two cost kinds separately: chars are divided once at
// the end, tokens are already whole.
type counter struct {
	chars       int
	tokens      int
	imageTokens int
}

// content folds one Content union. Nested tool_result content recurses.
func (c *counter) content(x *schema.Content) {
	if x == nil {
		return
	}
	for i := range x.Blocks {
		c.block(&x.Blocks[i])
	}
}

func (c *counter) block(b *schema.ContentBlock) {
	switch b.Type {
	case schema.BlockText:
		c.chars += len(b.Text)
	case schema.BlockThinking:
		// The signature is a credential, not prose the model reads, and the
		// request translator drops it before the upstream ever sees it.
		c.chars += len(b.Thinking)
	case schema.BlockRedactedThinking:
		// Opaque ciphertext: its length says nothing about its token cost.
	case schema.BlockImage, schema.BlockDocument:
		c.tokens += c.imageTokens
	case schema.BlockToolUse:
		c.chars += len(b.Name) + len(b.Input)
	case schema.BlockToolResult:
		c.chars += len(b.ToolUseID)
		c.content(b.Content)
	default:
		// An unrecognised block still costs something; charge what we can read
		// rather than silently pricing it at zero.
		c.chars += len(b.Text) + len(b.Input)
	}
}

// Count estimates a count_tokens request and returns the API response body.
func Count(e Estimator, req *schema.CountTokensRequest) schema.CountTokensResponse {
	if e == nil {
		e = Default()
	}
	return schema.CountTokensResponse{InputTokens: e.EstimatePrompt(PromptFromCountTokens(req))}
}

// ceilDiv divides rounding up, for non-negative n and positive d.
func ceilDiv(n, d int) int {
	if n <= 0 {
		return 0
	}
	return (n + d - 1) / d
}
