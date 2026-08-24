package schema

// This file holds the OpenAI Responses API STREAMING event types utraque reads
// off the Codex backend's SSE body. Like the rest of this package it is a pure
// type package — nothing beyond the standard library — so the stream translator
// can share it without an import cycle.
//
// The envelope is deliberately permissive: StreamEvent flattens every field any
// event we care about carries, and unknown event types simply leave every field
// zero. An event type utraque does not recognise is never an error — the backend
// is undocumented and adds event types without notice — so the translator counts
// it and moves on rather than failing the stream.

// Streaming event type names on the Responses SSE body. These match the JSON
// "type" field of each event's data payload (and the SSE event: line).
const (
	EventResponseCreated    = "response.created"
	EventResponseInProgress = "response.in_progress"
	EventResponseCompleted  = "response.completed"
	EventResponseIncomplete = "response.incomplete"
	EventResponseFailed     = "response.failed"

	EventOutputItemAdded = "response.output_item.added"
	EventOutputItemDone  = "response.output_item.done"

	EventContentPartAdded = "response.content_part.added"
	EventContentPartDone  = "response.content_part.done"
	EventOutputTextDelta  = "response.output_text.delta"
	EventOutputTextDone   = "response.output_text.done"

	EventReasoningSummaryPartAdded = "response.reasoning_summary_part.added"
	EventReasoningSummaryTextDelta = "response.reasoning_summary_text.delta"
	EventReasoningSummaryTextDone  = "response.reasoning_summary_text.done"
	EventReasoningTextDelta        = "response.reasoning_text.delta"
	EventReasoningTextDone         = "response.reasoning_text.done"

	EventFunctionCallArgumentsDelta = "response.function_call_arguments.delta"
	EventFunctionCallArgumentsDone  = "response.function_call_arguments.done"

	// EventError is the standalone error frame the backend sends on a stream-level
	// failure (also delivered on the SSE event: line as "error").
	EventError = "error"
)

// Output item types carried on output_item.added / .done.
const (
	OutputItemMessage      = "message"
	OutputItemFunctionCall = "function_call"
	OutputItemReasoning    = "reasoning"
)

// Content-part types inside a message item's content / reasoning summary.
const (
	OutputPartText          = "output_text"
	OutputPartSummaryText   = "summary_text"
	OutputPartReasoningText = "reasoning_text"
)

// Response statuses on the response object.
const (
	ResponseStatusCompleted  = "completed"
	ResponseStatusIncomplete = "incomplete"
	ResponseStatusFailed     = "failed"
)

// IncompleteReasonMaxOutputTokens is the incomplete_details.reason that maps
// onto the Anthropic max_tokens stop reason.
const IncompleteReasonMaxOutputTokens = "max_output_tokens"

// StreamEvent is the permissive envelope for one Responses streaming event. Only
// the fields relevant to Type are populated; the rest stay zero. Both the SSE
// event: name and this Type should agree, and the translator prefers Type when
// present, falling back to the frame's event name.
type StreamEvent struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number,omitempty"`

	// Routing indices. OutputIndex identifies which output item an item-scoped
	// event belongs to; it is the key the translator buffers interleaved
	// parallel items by. ContentIndex is the part index within an item.
	OutputIndex  int    `json:"output_index,omitempty"`
	ContentIndex int    `json:"content_index,omitempty"`
	ItemID       string `json:"item_id,omitempty"`

	// Incremental text: Delta is a text/reasoning/arguments increment; Text and
	// Arguments are the accumulated final value on a *.done event.
	Delta     string `json:"delta,omitempty"`
	Text      string `json:"text,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// Structured payloads.
	Part     *Part       `json:"part,omitempty"`
	Item     *OutputItem `json:"item,omitempty"`
	Response *Response   `json:"response,omitempty"`

	// Error frame fields (type == "error"): a flat code/message pair.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Part is a content or reasoning-summary part on a *_part.added event.
type Part struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// OutputItem is the output item carried on output_item.added / .done. For a
// function_call, Name/CallID/Arguments describe the call; for a message, Content
// holds its parts.
type OutputItem struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Status    string `json:"status,omitempty"`
	Role      string `json:"role,omitempty"`
	Name      string `json:"name,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Content   []Part `json:"content,omitempty"`
	Summary   []Part `json:"summary,omitempty"`
}

// Response is the response object on created/in_progress/completed/incomplete/
// failed events. Only the fields the translator reads are modelled.
type Response struct {
	ID                string             `json:"id,omitempty"`
	Status            string             `json:"status,omitempty"`
	Model             string             `json:"model,omitempty"`
	Usage             *Usage             `json:"usage,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	Error             *ResponseError     `json:"error,omitempty"`
}

// Usage is the Responses token accounting block. cache_read maps from
// input_tokens_details.cached_tokens.
type Usage struct {
	InputTokens         int                  `json:"input_tokens,omitempty"`
	OutputTokens        int                  `json:"output_tokens,omitempty"`
	TotalTokens         int                  `json:"total_tokens,omitempty"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// InputTokensDetails carries the cached-prompt token count.
type InputTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// OutputTokensDetails carries the reasoning token count.
type OutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// IncompleteDetails explains why a response stopped early.
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

// ResponseError is the error object on a failed response.
type ResponseError struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// CachedTokens returns the cached input-token count, tolerating a missing
// details block.
func (u *Usage) CachedTokens() int {
	if u == nil || u.InputTokensDetails == nil {
		return 0
	}
	return u.InputTokensDetails.CachedTokens
}
