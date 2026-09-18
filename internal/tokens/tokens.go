// Package tokens counts the input tokens of the Responses request utraque
// sends to the Codex backend, with the real GPT tokenizer (o200k_base).
//
// Two consumers need a number before, or instead of, the truth the backend
// reports in its terminal usage block:
//
//   - The message_start usage.input_tokens seed. Claude Code writes one
//     transcript line per content block, all sharing message.id: the first
//     carries this seed and the last carries the real usage from message_delta.
//     ccusage dedups lines sharing an id by keeping the one with the LARGER
//     total (input + cache_create + cache_read + output). A seed that overshoots
//     the real prompt by more than the answer's size therefore wins the dedup
//     and erases that turn's cache reads and output from the report. The seed
//     must be a LOWER BOUND of the billed prompt so the real line always wins.
//   - POST /v1/messages/count_tokens for a codex-routed model, which drives
//     Claude Code's context bar.
//
// # The lower-bound rule
//
// The count is of the text utraque actually sends upstream and nothing else:
//
//   - Every string field the backend renders in front of the model — the
//     instructions, each message part's text, each function call's name and
//     arguments, each function result's output, and each tool's name,
//     description and the prose inside its parameter schema — is tokenized
//     SEPARATELY and the counts summed. The backend frames each field in its
//     own turn or declaration, so per-field counting is the faithful model; a
//     field never merges into a neighbour across a frame boundary.
//   - NO flat framing overheads are added. The backend adds its own (role and
//     turn markers, tool namespace syntax, its hidden preamble), and the seed
//     must stay <= billed, so they are left out rather than guessed at.
//   - Opaque blobs are skipped: encrypted reasoning items, image data URLs,
//     anything base64. Tokenizing ciphertext would overshoot wildly, and the
//     backend counts the real content we cannot see. This is the one place the
//     count is knowingly LOW by a lot: a session with heavy reasoning replay
//     carries thousands of reasoning tokens per turn that the seed does not
//     see, so count_tokens — and the context bar it drives — reads a few
//     percent low in such sessions. That is accepted; a low bar is harmless
//     and a seed that wins the dedup is not.
//
// Anything billing-shaped must use the upstream's own reported usage, never
// this.
//
// # Two views, two interfaces
//
// The exact count is of the Responses request AFTER translation, because that
// is the text the backend sees: the billing header Claude Code prepends is
// gone, thinking text has become an encrypted replay item, images have become
// data URLs. Counting the Anthropic-shaped body instead would mean
// re-deriving every one of those decisions here, and drifting. So
// RequestEstimator takes the translated request, and Codex returns the exact
// implementation of it for the Codex leg.
//
// Estimator is the older, model-agnostic view over the Anthropic-shaped
// prompt (system, messages, tools). It is what a leg with no exact tokenizer
// for its backend uses — the DeepSeek leg, whose models are not o200k — and
// Default returns the CharsPerToken heuristic for it. CharsPerToken
// implements both views and is the documented fallback for the Codex leg too,
// should the compiled-in tokenizer ever fail to initialise.
package tokens

import (
	"encoding/json"
	"strconv"

	"github.com/hughescr/utraque/internal/codex/schema"
)

// RequestEstimator counts the input tokens of a translated Responses request.
// Implementations must be safe for concurrent use and must never return a
// negative count.
type RequestEstimator interface {
	// EstimateString counts the tokens in one string.
	EstimateString(s string) int
	// EstimateRequest counts the input tokens of a whole Responses request
	// under the lower-bound rule described in the package comment.
	EstimateRequest(req *cschema.ResponsesRequest) int
	// Name identifies the estimator in logs and /healthz, so an operator can
	// see which method produced a count.
	Name() string
}

// Codex returns the estimator for codex-routed models: the exact o200k_base
// tokenizer. Should the tokenizer fail to initialise — it is compiled in, so
// this would be a build defect rather than a runtime condition — the chars/4
// heuristic is returned instead, and its Name says so in the log, because a
// request that fails for want of a token count is worse than a rough count.
func Codex() RequestEstimator {
	if e, err := O200k(); err == nil {
		return e
	}
	return CharsPerToken{}
}

// walkFields visits every string field of req that the backend renders in
// front of the model, in wire order, plus one call per image part. It is the
// single definition of "text utraque actually sends" that both estimators
// share, so they cannot disagree about what is counted — only about how.
//
// What is deliberately NOT visited, and why:
//
//   - req.Model: a routing key, never prompt text.
//   - Item and part types, roles, call ids, reasoning item ids: framing the
//     backend renders in its own syntax (or not at all), so charging their
//     JSON spelling would overshoot.
//   - A reasoning item's encrypted_content: opaque ciphertext. Its real
//     content is counted by the backend; tokenizing base64 would overshoot.
//   - A reasoning item's summary: utraque always sends the empty array.
//   - An image part's URL: a data URL is base64, and a remote URL is fetched
//     and billed by tile geometry that cannot be known here. Reported to the
//     visitor as an image so the fallback heuristic can charge a flat figure;
//     the exact estimator counts it as zero, the only lower bound available.
//   - Tool schemas: visited through the visitor's Tool method rather than as
//     one string, because the backend does not show the model the JSON text
//     (see schemaText).
//   - tool_choice, parallel_tool_calls, reasoning effort, include, the cache
//     key, store and stream: request control, not prompt text.
func walkFields(req *cschema.ResponsesRequest, v fieldVisitor) {
	if req == nil {
		return
	}
	v.Text(req.Instructions)
	for i := range req.Input {
		it := &req.Input[i]
		v.Item()
		switch it.Type {
		case cschema.ItemMessage:
			for j := range it.Content {
				p := &it.Content[j]
				switch p.Type {
				case cschema.PartInputImage:
					v.Image()
				default:
					v.Text(p.Text)
				}
			}
		case cschema.ItemFunctionCall:
			v.Text(it.Name)
			v.Text(it.Arguments)
		case cschema.ItemFunctionCallOutput:
			if it.Output != nil {
				v.Text(*it.Output)
			}
		case cschema.ItemReasoning:
			// Nothing: see above.
		default:
			// An item type this package does not know is still sent; charge
			// the text it can see rather than silently pricing it at zero.
			for j := range it.Content {
				v.Text(it.Content[j].Text)
			}
			v.Text(it.Name)
			v.Text(it.Arguments)
			if it.Output != nil {
				v.Text(*it.Output)
			}
		}
	}
	for i := range req.Tools {
		v.Tool(&req.Tools[i])
	}
}

// fieldVisitor receives walkFields' visits.
type fieldVisitor interface {
	// Text is one string field the model reads as prose. Empty strings are
	// visited too; implementations treat them as zero.
	Text(s string)
	// Image is one image part.
	Image()
	// Item marks the start of one input item, for a visitor that charges
	// per-item framing.
	Item()
	// Tool is one tool declaration.
	Tool(t *cschema.Tool)
}

// schemaText visits the strings inside a tool's JSON Schema that the backend
// renders in front of the model.
//
// The Responses backend does not show the model the schema's JSON text. The
// harmony format the o-series and GPT models are trained on (published for
// gpt-oss, which mirrors the production Responses API) renders each function
// as a TypeScript-style declaration: the property NAMES, their TYPES, their
// DESCRIPTIONS as comments and their ENUM values as a union — and none of the
// JSON keywords ("properties", "required", "additionalProperties"), quotes,
// braces or colons. Tokenizing the compact JSON would therefore overshoot by
// roughly the schema's punctuation, which on a first turn with a large tool
// set and a short answer is enough to break the lower-bound invariant.
//
// So only the prose is counted: property names, descriptions, enum values,
// defaults and type words, each as its own field. Schema keywords the
// rendering does not show at all (required, minLength, pattern, $schema, ...)
// are skipped, and every combinator and nested schema is recursed into, so the
// count is a lower bound of the rendering whatever syntax it wraps the prose
// in. Definitions under $defs/definitions are counted once, which is <=
// inlining them at each reference. The one word charged that the rendering
// may not show is the root "object" type — one token per tool, against the
// ten-odd tokens of declaration syntax per tool that are not charged. A
// schema that does not parse is charged nothing: it would be rejected
// upstream before it was billed.
func schemaText(raw json.RawMessage, text func(string)) {
	if len(raw) == 0 {
		return
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return
	}
	schemaNode(node, text)
}

func schemaNode(node any, text func(string)) {
	obj, ok := node.(map[string]any)
	if !ok {
		return
	}
	for key, val := range obj {
		switch key {
		case "description":
			if s, ok := val.(string); ok {
				text(s)
			}
		case "type":
			switch t := val.(type) {
			case string:
				text(t)
			case []any:
				for _, e := range t {
					if s, ok := e.(string); ok {
						text(s)
					}
				}
			}
		case "enum":
			if list, ok := val.([]any); ok {
				for _, e := range list {
					text(scalarText(e))
				}
			}
		case "default":
			// Rendered as a trailing "// default: x" comment.
			text(scalarText(val))
		case "properties", "$defs", "definitions", "patternProperties":
			if props, ok := val.(map[string]any); ok {
				for name, sub := range props {
					text(name)
					schemaNode(sub, text)
				}
			}
		case "items", "additionalProperties", "not", "if", "then", "else":
			schemaNode(val, text)
		case "anyOf", "oneOf", "allOf", "prefixItems":
			if list, ok := val.([]any); ok {
				for _, sub := range list {
					schemaNode(sub, text)
				}
			}
		}
	}
}

// scalarText is the spelling of an enum or default value as the model
// would see it: the string itself, or a number/bool as written. Composite
// values are not rendered and cost nothing.
func scalarText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}
