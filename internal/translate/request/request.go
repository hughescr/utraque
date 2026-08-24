// Package request translates an Anthropic Messages API request into an OpenAI
// Responses API request for the Codex backend.
//
// The translation is defined by the plan's "Request translation" component and
// is deliberately faithful — Claude Code's own system prompt and tool schemas
// are what make its tools work, so they are carried through verbatim rather than
// replaced with Codex's own template. What the Responses backend cannot honour
// (sampling params, stop sequences) is dropped, but every drop is RECORDED in
// the returned Metadata so a later layer can DEBUG-log exactly what was
// discarded.
//
// This package holds behaviour, not wire types, so it may import the router
// (for the routing Decision and its effort provenance) and both schema
// packages. The schema packages themselves stay stdlib-only.
package request

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	aschema "github.com/hughescr/utraque/internal/anthropic/schema"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/router"
)

// DefaultMutatingTools is the built-in set of tool names that, when present in
// a request, force parallel_tool_calls:false — running two of these at once
// against one workspace is a footgun. It is overridable via
// Options.MutatingTools.
var DefaultMutatingTools = map[string]bool{
	"Edit":         true,
	"MultiEdit":    true,
	"Write":        true,
	"NotebookEdit": true,
	"Bash":         true,
	"BashOutput":   true,
	"KillShell":    true,
}

// effortRank is the canonical ordering of reasoning-effort levels, lowest to
// highest, used to clamp a requested effort DOWN to a model's highest supported
// level. It matches the catalog's supported_reasoning_levels ordering.
var effortRank = map[string]int{
	cschema.EffortLow:    0,
	cschema.EffortMedium: 1,
	cschema.EffortHigh:   2,
	cschema.EffortXHigh:  3,
	cschema.EffortMax:    4,
	cschema.EffortUltra:  5,
}

// summaryNone is the catalog's sentinel for "request no reasoning summary". It
// is not a valid Responses summary mode, so it is treated as "omit summary".
const summaryNone = "none"

// The Anthropic parameters utraque drops because the Codex backend ignores
// them. Recorded in Metadata.Dropped when present so the caller can log them.
const (
	DroppedTemperature   = "temperature"
	DroppedTopP          = "top_p"
	DroppedTopK          = "top_k"
	DroppedMaxTokens     = "max_tokens"
	DroppedStopSequences = "stop_sequences"
	// DroppedThinking records that the client's thinking config (budget_tokens)
	// was discarded. Reasoning effort is resolved by precedence (suffix > beta >
	// config > catalog), never from the Anthropic thinking budget, so the budget
	// has no effect — but the package's contract is that every drop is recorded.
	DroppedThinking = "thinking"
)

// DroppedImageEmptyMediaType is the Metadata.DroppedImages reason recorded
// when a base64 image block's media_type or data is empty. Emitting a data
// URL in that case would produce a malformed "data:;base64,..." string that
// vision backends reject, so the image is dropped instead of emitted and this
// reason is recorded so the loss is visible to logging.
const DroppedImageEmptyMediaType = "image: empty media_type or data"

// Options tunes a translation. The zero value is valid: it uses
// DefaultMutatingTools and takes effort/summary entirely from the routing
// Decision and the catalog model.
type Options struct {
	// BetaEffort is an effort level extracted from the request's anthropic-beta
	// header by the caller. It sits below a model-name suffix and above the
	// per-model config override in the effort precedence.
	BetaEffort string
	// ConfigEffort is a per-model config override for the effort level. It sits
	// below the anthropic-beta signal and above the catalog default.
	ConfigEffort string
	// Summary overrides the reasoning summary mode. Empty falls back to the
	// model's default_reasoning_summary. A value of "none" (from either source)
	// omits the summary field entirely.
	Summary string
	// MutatingTools overrides DefaultMutatingTools. Nil uses the default set; a
	// non-nil (even empty) map replaces it wholesale.
	MutatingTools map[string]bool
}

func (o Options) mutatingSet() map[string]bool {
	if o.MutatingTools != nil {
		return o.MutatingTools
	}
	return DefaultMutatingTools
}

// EffortResult records how the reasoning effort was resolved: the level chosen
// by precedence (Requested), the level actually sent after clamping to the
// model's supported set (Applied), which precedence source supplied it, and
// whether clamping changed it.
type EffortResult struct {
	Requested string
	Applied   string
	Source    string
	Clamped   bool
}

// Metadata is the non-wire byproduct of a translation: what was dropped and how
// effort/summary/parallelism were decided. It exists so a later layer can
// DEBUG-log the translation without re-deriving any of it.
type Metadata struct {
	// Dropped names the Anthropic parameters that were present in the input and
	// discarded because the backend ignores them.
	Dropped []string
	// SystemMessagesRemapped counts messages[] entries that arrived with role
	// "system" (the mid-conversation-system beta) and were emitted as developer
	// items instead, because the backend rejects role "system" on input.
	SystemMessagesRemapped int
	// Effort is the reasoning-effort resolution.
	Effort EffortResult
	// Summary is the reasoning summary mode actually applied ("" when none).
	Summary string
	// ParallelToolCallsDisabled reports whether parallel_tool_calls:false was
	// forced, by either a mutating tool or the client's explicit
	// tool_choice.disable_parallel_tool_use.
	ParallelToolCallsDisabled bool
	// MutatingTools lists the request tool names that triggered the disable,
	// sorted, for logging. It may be empty when the disable came solely from the
	// client's disable_parallel_tool_use flag.
	MutatingTools []string
	// OrphanedToolResults lists tool_result call_ids with no matching prior
	// tool_use in the same request. They are still emitted as
	// function_call_output items (structural passthrough), but recorded here
	// because they are a likely sign of a malformed history.
	OrphanedToolResults []string
	// DroppedImages records, one entry per image block that could not be
	// safely rendered as a data URL (see DroppedImageEmptyMediaType), why it
	// was dropped instead of emitted. The image itself is NOT emitted — this
	// is the only trace that it existed in the request.
	DroppedImages []string
}

// Translate converts an Anthropic Messages request into a Codex Responses
// request. dec supplies the upstream model slug and the suffix-effort signal;
// model is the catalog entry for that slug (used to clamp effort and to source
// the default summary) — pass the zero Model when the catalog has no entry, and
// effort is left unclamped. It never mutates its inputs.
func Translate(req *aschema.MessagesRequest, dec router.Decision, model cschema.Model, opts Options) (*cschema.ResponsesRequest, Metadata, error) {
	if req == nil {
		return nil, Metadata{}, fmt.Errorf("translate: nil request")
	}

	var meta Metadata

	instructions, systemDropped := joinSystem(req.System)
	out := &cschema.ResponsesRequest{
		Model:        dec.UpstreamModel,
		Instructions: instructions,
		Store:        false,
		Stream:       true,
	}

	input, orphans, droppedImages, systemRemapped, err := translateMessages(req.Messages)
	if err != nil {
		return nil, Metadata{}, err
	}
	out.Input = input
	meta.OrphanedToolResults = orphans
	meta.DroppedImages = droppedImages
	meta.SystemMessagesRemapped = systemRemapped

	out.Tools = translateTools(req.Tools)
	out.ToolChoice = translateToolChoice(req.ToolChoice)

	disabled, names := disableParallel(req.Tools, opts.mutatingSet())
	clientDisableParallel := req.ToolChoice != nil && req.ToolChoice.DisableParallelToolUse
	if disabled || clientDisableParallel {
		// Either a mutating tool (the local footgun heuristic) or the client's
		// explicit tool_choice.disable_parallel_tool_use forces serial calls.
		f := false
		out.ParallelToolCalls = &f
		meta.ParallelToolCallsDisabled = true
		meta.MutatingTools = names
	}

	out.Reasoning, meta.Effort, meta.Summary = translateReasoning(dec, model, opts)

	meta.Dropped = droppedParams(req)
	meta.Dropped = append(meta.Dropped, systemDropped...)

	return out, meta, nil
}

// joinSystem renders the system field into the Responses "instructions" string:
// a scalar string passes through verbatim; an array joins its text blocks with
// a blank line between them. Non-text system blocks (rare) are dropped from
// the joined string, but each drop is returned as a "system:<block_type>"
// reason so the caller can fold it into Metadata.Dropped rather than lose it
// silently.
func joinSystem(system *aschema.Content) (instructions string, dropped []string) {
	if system.IsEmpty() {
		return "", nil
	}
	parts := make([]string, 0, len(system.Blocks))
	for _, b := range system.Blocks {
		if b.Type == aschema.BlockText {
			parts = append(parts, b.Text)
		} else {
			dropped = append(dropped, "system:"+b.Type)
		}
	}
	return strings.Join(parts, "\n\n"), dropped
}

// translateMessages walks every message's content in order, flattening it into
// the Responses input[] array. Text/image blocks accumulate into a message item
// carrying the message's role; a tool_use flushes any pending message and emits
// a function_call; a tool_result flushes and emits a function_call_output
// (plus, when the result carries images, a following user message holding those
// images). thinking/redacted_thinking history and cache_control are dropped.
//
// LIMITATION: when a turn contains more than one image-bearing tool_result,
// each result's function_call_output is immediately followed by its own
// image-carrying user message, interleaving them (fco_A, imgA, fco_B, imgB)
// rather than grouping all outputs before all images. This is pinned by the
// two_image_tool_results golden fixture rather than changed, to avoid
// reordering call_id/output linkage without a concrete need.
func translateMessages(messages []aschema.Message) (items []cschema.InputItem, orphans []string, droppedImages []string, systemRemapped int, err error) {
	seenCalls := map[string]bool{}

	for mi := range messages {
		msg := messages[mi]
		role := msg.Role
		if role == aschema.RoleSystem {
			// The mid-conversation-system beta puts system-role messages inside
			// messages[]. The Responses API refuses role "system" on an input
			// item; developer is its positional equivalent, so the instruction
			// keeps both its content and its place in the turn order. The
			// top-level system field is unaffected — it still becomes
			// instructions via joinSystem.
			role = cschema.RoleDeveloper
			systemRemapped++
		}
		if msg.Content == nil {
			continue
		}

		var pending []cschema.ContentPart
		flush := func() {
			if len(pending) > 0 {
				items = append(items, cschema.MessageItem(role, pending...))
				pending = nil
			}
		}

		for _, blk := range msg.Content.Blocks {
			switch blk.Type {
			case aschema.BlockText:
				// Every message part in a Responses input[] is input_text,
				// regardless of role: the input content union is
				// input_text/input_image/input_file, and output_text (an output
				// content type) is not accepted on an input message. The role
				// field alone carries the user/assistant distinction.
				pending = append(pending, cschema.InputText(blk.Text))

			case aschema.BlockImage:
				url, drop, ierr := imageURL(blk.Source)
				if ierr != nil {
					return nil, nil, nil, 0, ierr
				}
				if drop {
					droppedImages = append(droppedImages, DroppedImageEmptyMediaType)
					continue
				}
				pending = append(pending, cschema.InputImage(url))

			case aschema.BlockToolUse:
				flush()
				args, aerr := toolArguments(blk.Input)
				if aerr != nil {
					return nil, nil, nil, 0, aerr
				}
				seenCalls[blk.ID] = true
				items = append(items, cschema.FunctionCall(blk.ID, blk.Name, args))

			case aschema.BlockToolResult:
				flush()
				if !seenCalls[blk.ToolUseID] {
					orphans = append(orphans, blk.ToolUseID)
				}
				text, images, imgDropped, terr := flattenToolResult(blk.Content)
				if terr != nil {
					return nil, nil, nil, 0, terr
				}
				droppedImages = append(droppedImages, imgDropped...)
				if blk.IsError {
					// A Responses function_call_output has no is_error field, so
					// the failure signal is preserved as an explicit text marker.
					text = markToolError(text)
				}
				output := text
				if len(images) > 0 {
					output = withImagePlaceholder(text, len(images))
				}
				items = append(items, cschema.FunctionCallOutput(blk.ToolUseID, output))
				if len(images) > 0 {
					// Images can't ride inside a function_call_output, so they
					// follow as a user message referenced by the placeholder.
					// See the LIMITATION note on this function's doc comment for
					// the interleaving this produces across multiple results.
					items = append(items, cschema.MessageItem(aschema.RoleUser, images...))
				}

			case aschema.BlockThinking, aschema.BlockRedactedThinking:
				// Reasoning history is dropped: with store:false it can't be
				// replayed, and a synthetic signature would be rejected on a
				// later Claude turn.

			default:
				// Unknown block types are skipped rather than fatal — an input
				// the translator doesn't model shouldn't sink the whole request.
			}
		}

		flush()
	}

	return items, orphans, droppedImages, systemRemapped, nil
}

// imageURL renders an Anthropic image source as a Responses input_image URL: a
// base64 source becomes a data URL; a url source passes through unchanged. drop
// reports that the source could not be safely rendered — currently, a base64
// source with an empty media_type or empty data, which would otherwise
// produce a malformed "data:;base64,..." URL that vision backends reject. The
// caller must skip emitting the image in that case and record the loss
// instead of treating it as a translation error.
func imageURL(src *aschema.Source) (url string, drop bool, err error) {
	if src == nil {
		return "", false, fmt.Errorf("translate: image block has no source")
	}
	switch src.Type {
	case "url":
		return src.URL, false, nil
	case "base64", "":
		// Anthropic's inline image source is base64; an empty type is treated
		// as base64 for tolerance.
		if src.MediaType == "" || src.Data == "" {
			return "", true, nil
		}
		return "data:" + src.MediaType + ";base64," + src.Data, false, nil
	default:
		return "", false, fmt.Errorf("translate: unsupported image source type %q", src.Type)
	}
}

// toolArguments renders a tool_use input (arbitrary JSON) as the JSON STRING the
// Responses function_call expects. A nil/empty input becomes "{}". The bytes are
// re-compacted so formatting never leaks into the golden output.
func toolArguments(input json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return "{}", nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(trimmed)); err != nil {
		return "", fmt.Errorf("translate: tool_use input is not valid JSON: %w", err)
	}
	return buf.String(), nil
}

// flattenToolResult reduces a tool_result's content into a single text string
// plus any image parts it carried. A scalar-string content is its text; an
// array concatenates text blocks (blank-line separated) and collects images.
// dropped records a reason (see DroppedImageEmptyMediaType) for each image
// that was dropped rather than emitted.
func flattenToolResult(content *aschema.Content) (text string, images []cschema.ContentPart, dropped []string, err error) {
	if content.IsEmpty() {
		return "", nil, nil, nil
	}
	var texts []string
	for _, b := range content.Blocks {
		switch b.Type {
		case aschema.BlockText:
			texts = append(texts, b.Text)
		case aschema.BlockImage:
			url, drop, ierr := imageURL(b.Source)
			if ierr != nil {
				return "", nil, nil, ierr
			}
			if drop {
				dropped = append(dropped, DroppedImageEmptyMediaType)
				continue
			}
			images = append(images, cschema.InputImage(url))
		default:
			// Other block types in a tool_result are ignored.
		}
	}
	return strings.Join(texts, "\n\n"), images, dropped, nil
}

// ToolErrorMarker prefixes the flattened text of a tool_result whose is_error
// flag is set. The Responses function_call_output carries no error field, so
// this explicit marker is the only way to preserve that the tool failed.
const ToolErrorMarker = "[tool error]"

// markToolError prepends ToolErrorMarker to a failed tool result's text.
func markToolError(text string) string {
	if text == "" {
		return ToolErrorMarker
	}
	return ToolErrorMarker + "\n\n" + text
}

// withImagePlaceholder appends a note to a tool result's text saying its images
// follow in the next message, so the flattened output doesn't silently lose the
// fact that images were present.
func withImagePlaceholder(text string, n int) string {
	note := fmt.Sprintf("[%d image(s) provided in the following user message]", n)
	if text == "" {
		return note
	}
	return text + "\n\n" + note
}

// translateTools maps Anthropic tool declarations onto Responses function
// tools, carrying the input_schema through verbatim as the parameters. A nil
// tool list yields nil (the field is omitted).
func translateTools(tools []aschema.Tool) []cschema.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]cschema.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, cschema.FunctionTool(t.Name, t.Description, t.InputSchema))
	}
	return out
}

// translateToolChoice maps Anthropic tool_choice onto the Responses form:
// auto->"auto", any->"required", none->"none", {type:tool,name}->{function,name}.
// A nil choice yields nil (omitted).
func translateToolChoice(tc *aschema.ToolChoice) *cschema.ToolChoice {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case aschema.ToolChoiceAuto:
		return cschema.AutoChoice()
	case aschema.ToolChoiceAny:
		return cschema.RequiredChoice()
	case aschema.ToolChoiceNone:
		return cschema.NoneChoice()
	case aschema.ToolChoiceTool:
		return cschema.FunctionChoice(tc.Name)
	default:
		// An unrecognised choice type is left to the backend default (omitted).
		return nil
	}
}

// disableParallel reports whether any request tool is in the mutating set, and
// returns the sorted list of matching tool names for logging.
func disableParallel(tools []aschema.Tool, mutating map[string]bool) (bool, []string) {
	var hits []string
	for _, t := range tools {
		if mutating[t.Name] {
			hits = append(hits, t.Name)
		}
	}
	if len(hits) == 0 {
		return false, nil
	}
	sort.Strings(hits)
	return true, hits
}

// translateReasoning resolves the reasoning block: the effort by precedence
// (suffix > anthropic-beta > config > catalog default), clamped to the model's
// supported levels, and the summary (config override else catalog default,
// with "none" meaning omit). It returns nil reasoning only when there is no
// effort and no summary to send.
func translateReasoning(dec router.Decision, model cschema.Model, opts Options) (*cschema.Reasoning, EffortResult, string) {
	requested, source := chooseEffort(dec, model, opts)
	applied, clamped := clampEffort(requested, model)
	res := EffortResult{Requested: requested, Applied: applied, Source: source, Clamped: clamped}

	summary := opts.Summary
	if summary == "" {
		summary = model.DefaultReasoningSummary
	}
	emitSummary := summary
	if emitSummary == summaryNone {
		emitSummary = ""
	}

	if applied == "" && emitSummary == "" {
		return nil, res, emitSummary
	}
	return &cschema.Reasoning{Effort: applied, Summary: emitSummary}, res, emitSummary
}

// chooseEffort applies the effort precedence and returns the chosen level and
// its provenance, before any clamping. A model-name suffix (already parsed by
// the router into the Decision) wins; then the anthropic-beta signal; then the
// per-model config override; then the catalog default.
func chooseEffort(dec router.Decision, model cschema.Model, opts Options) (effort, source string) {
	if dec.EffortSource == router.EffortSourceSuffix && dec.Effort != "" {
		return dec.Effort, router.EffortSourceSuffix
	}
	if opts.BetaEffort != "" {
		return opts.BetaEffort, router.EffortSourceBeta
	}
	if opts.ConfigEffort != "" {
		return opts.ConfigEffort, router.EffortSourceConfig
	}
	if model.DefaultReasoningLevel != "" {
		return model.DefaultReasoningLevel, router.EffortSourceCatalog
	}
	return "", router.EffortSourceNone
}

// clampEffort clamps a requested effort to the model's supported levels: if the
// model supports it, it passes unchanged; otherwise it is clamped DOWN to the
// highest supported level not exceeding the request (or, if the request sits
// below every supported level, UP to the lowest supported one). A model that
// declares no supported levels leaves the request unchanged (nothing to clamp
// against). Returns the applied level and whether clamping changed it.
func clampEffort(requested string, model cschema.Model) (applied string, clamped bool) {
	if requested == "" {
		return "", false
	}
	supported := model.SupportedEfforts()
	if len(supported) == 0 {
		return requested, false // no catalog data to validate against
	}
	if model.SupportsEffort(requested) {
		return requested, false
	}

	reqRank, ok := effortRank[requested]
	if !ok {
		// An effort the canonical order doesn't know (e.g. a config or header
		// typo like "hgh"), and the model doesn't support it verbatim. Treat it
		// as below the floor so it clamps DOWN to the lowest supported level
		// rather than escalating to the model's max — an unrecognised token
		// must never silently buy the most expensive reasoning tier.
		reqRank = -1
	}

	bestBelow, bestBelowRank := "", -1
	lowest, lowestRank := "", len(effortRank)+1
	for _, e := range supported {
		r, known := effortRank[e]
		if !known {
			continue
		}
		if r < lowestRank {
			lowest, lowestRank = e, r
		}
		if r <= reqRank && r > bestBelowRank {
			bestBelow, bestBelowRank = e, r
		}
	}
	if bestBelow != "" {
		return bestBelow, true
	}
	if lowest != "" {
		return lowest, true // request below all supported; clamp up to lowest
	}
	return requested, false // supported levels were all unknown tokens
}

// droppedParams lists the Anthropic sampling/limit params present in req that
// the backend ignores, in a stable order. max_tokens is required by the
// Anthropic schema so it is always present and always listed.
func droppedParams(req *aschema.MessagesRequest) []string {
	var dropped []string
	if req.Temperature != nil {
		dropped = append(dropped, DroppedTemperature)
	}
	if req.TopP != nil {
		dropped = append(dropped, DroppedTopP)
	}
	if req.TopK != nil {
		dropped = append(dropped, DroppedTopK)
	}
	dropped = append(dropped, DroppedMaxTokens)
	if len(req.StopSequences) > 0 {
		dropped = append(dropped, DroppedStopSequences)
	}
	if req.Thinking != nil {
		dropped = append(dropped, DroppedThinking)
	}
	return dropped
}
