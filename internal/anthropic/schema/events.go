package schema

// SSE event names.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventPing              = "ping"
	EventError             = "error"
)

// Delta types.
const (
	DeltaText      = "text_delta"
	DeltaInputJSON = "input_json_delta"
	DeltaThinking  = "thinking_delta"
	DeltaSignature = "signature_delta"
)

// MessageStartEvent opens a stream.
type MessageStartEvent struct {
	Type    string           `json:"type"`
	Message MessagesResponse `json:"message"`
}

// ContentBlockStartEvent opens a content block.
type ContentBlockStartEvent struct {
	Type         string       `json:"type"`
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

// Delta is an incremental block update.
type Delta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

// ContentBlockDeltaEvent carries a Delta.
type ContentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta Delta  `json:"delta"`
}

// ContentBlockStopEvent closes a content block.
type ContentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

// MessageDelta carries terminal message metadata.
type MessageDelta struct {
	StopReason   string  `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

// MessageDeltaEvent is the penultimate stream event.
type MessageDeltaEvent struct {
	Type  string       `json:"type"`
	Delta MessageDelta `json:"delta"`
	Usage *Usage       `json:"usage,omitempty"`
}

// MessageStopEvent terminates a stream.
type MessageStopEvent struct {
	Type string `json:"type"`
}

// PingEvent is a keepalive.
type PingEvent struct {
	Type string `json:"type"`
}

// ErrorBody is the inner error object shared by the HTTP envelope and the
// mid-stream SSE error frame.
type ErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ErrorEvent is the full error envelope.
type ErrorEvent struct {
	Type  string    `json:"type"`
	Error ErrorBody `json:"error"`
}

// NewErrorEvent builds an error envelope.
func NewErrorEvent(kind, message string) ErrorEvent {
	return ErrorEvent{Type: EventError, Error: ErrorBody{Type: kind, Message: message}}
}
