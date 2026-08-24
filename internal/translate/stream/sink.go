// Package stream is the true-incremental translator from OpenAI Codex Responses
// streaming events to Anthropic Messages streaming events. A Translator drives a
// Sink; the SSEWriter sink writes Anthropic SSE frames on the fly, mapping each
// upstream event as it arrives rather than buffering the whole response.
//
// The state machine, its invariants, and the three failure modes are documented
// on Translator in translator.go. Two sinks share that one implementation
// through the Sink seam: SSEWriter for stream:true, and Aggregator (in
// aggregator.go) which folds the identical calls into a single MessagesResponse
// for stream:false. Neither sink interprets the Codex protocol, so the two
// request modes cannot drift apart.
package stream

import (
	"encoding/json"
	"io"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/sse"
)

// MessageStart carries the data for an Anthropic message_start event.
type MessageStart struct {
	ID          string
	Model       string
	InputTokens int
}

// MessageDelta carries the terminal metadata for a message_delta event.
type MessageDelta struct {
	StopReason string
	Usage      schema.Usage
}

// Sink receives the semantic Anthropic stream events the Translator produces.
// Every method returns an error so a sink writing to a network connection can
// abort the translation the instant the client goes away. The Translator
// guarantees the call order is a well-formed Anthropic stream: exactly one
// MessageStart first; each BlockStart matched by a BlockStop before any
// terminus; and the stream ends in exactly one of {MessageStop, Error}, or in
// nothing at all on a client interrupt.
type Sink interface {
	MessageStart(m MessageStart) error
	BlockStart(index int, block schema.ContentBlock) error
	BlockDelta(index int, delta schema.Delta) error
	BlockStop(index int) error
	MessageDelta(d MessageDelta) error
	MessageStop() error
	Error(err schema.ErrorBody) error
	Ping() error
}

// SSEWriter is the streaming Sink: it serialises each event to an Anthropic SSE
// frame and flushes, so bytes reach the client as the upstream produces them.
// It builds the wire JSON explicitly (rather than through the shared, omitempty
// schema types) so the frames match the Anthropic streaming format byte for
// byte — a content_block_start for text really carries "text":"", a message_start
// really carries "stop_reason":null.
type SSEWriter struct {
	fw *sse.FrameWriter
}

var _ Sink = (*SSEWriter)(nil)

// NewSSEWriter builds an SSEWriter over w. If w flushes (an http.Flusher), each
// frame is pushed on write.
func NewSSEWriter(w io.Writer) *SSEWriter {
	return &SSEWriter{fw: sse.NewFrameWriter(w)}
}

// --- wire types: exact Anthropic streaming JSON shapes ---

type wireMessage struct {
	ID           string                `json:"id"`
	Type         string                `json:"type"`
	Role         string                `json:"role"`
	Model        string                `json:"model"`
	Content      []schema.ContentBlock `json:"content"`
	StopReason   *string               `json:"stop_reason"`
	StopSequence *string               `json:"stop_sequence"`
	Usage        schema.Usage          `json:"usage"`
}

type wireMessageStart struct {
	Type    string      `json:"type"`
	Message wireMessage `json:"message"`
}

type wireStartText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireStartThinking struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type wireStartToolUse struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type wireBlockStart struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type wireBlockDelta struct {
	Type  string       `json:"type"`
	Index int          `json:"index"`
	Delta schema.Delta `json:"delta"`
}

type wireBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type wireMessageDeltaBody struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type wireMessageDelta struct {
	Type  string               `json:"type"`
	Delta wireMessageDeltaBody `json:"delta"`
	Usage schema.Usage         `json:"usage"`
}

// MessageStart writes the message_start frame. content is an empty array,
// stop_reason and stop_sequence are null, output_tokens starts at 0.
func (w *SSEWriter) MessageStart(m MessageStart) error {
	msg := wireMessageStart{
		Type: schema.EventMessageStart,
		Message: wireMessage{
			ID:      m.ID,
			Type:    "message",
			Role:    schema.RoleAssistant,
			Model:   m.Model,
			Content: []schema.ContentBlock{},
			Usage:   schema.Usage{InputTokens: m.InputTokens, OutputTokens: 0},
		},
	}
	return w.write(schema.EventMessageStart, msg)
}

// BlockStart writes content_block_start, choosing the exact wire shape for the
// block type so empty text/thinking fields and an empty tool input serialise.
func (w *SSEWriter) BlockStart(index int, block schema.ContentBlock) error {
	var cb any
	switch block.Type {
	case schema.BlockText:
		cb = wireStartText{Type: schema.BlockText, Text: block.Text}
	case schema.BlockThinking:
		cb = wireStartThinking{Type: schema.BlockThinking, Thinking: block.Thinking}
	case schema.BlockToolUse:
		input := block.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		cb = wireStartToolUse{Type: schema.BlockToolUse, ID: block.ID, Name: block.Name, Input: input}
	default:
		cb = block
	}
	return w.write(schema.EventContentBlockStart, wireBlockStart{
		Type: schema.EventContentBlockStart, Index: index, ContentBlock: cb,
	})
}

// BlockDelta writes content_block_delta.
func (w *SSEWriter) BlockDelta(index int, delta schema.Delta) error {
	return w.write(schema.EventContentBlockDelta, wireBlockDelta{
		Type: schema.EventContentBlockDelta, Index: index, Delta: delta,
	})
}

// BlockStop writes content_block_stop.
func (w *SSEWriter) BlockStop(index int) error {
	return w.write(schema.EventContentBlockStop, wireBlockStop{
		Type: schema.EventContentBlockStop, Index: index,
	})
}

// MessageDelta writes message_delta with the final stop reason and usage.
// stop_sequence is always null.
func (w *SSEWriter) MessageDelta(d MessageDelta) error {
	return w.write(schema.EventMessageDelta, wireMessageDelta{
		Type:  schema.EventMessageDelta,
		Delta: wireMessageDeltaBody{StopReason: strPtr(d.StopReason)},
		Usage: d.Usage,
	})
}

// MessageStop writes message_stop.
func (w *SSEWriter) MessageStop() error {
	return w.write(schema.EventMessageStop, map[string]string{"type": schema.EventMessageStop})
}

// Error writes a mid-stream error frame.
func (w *SSEWriter) Error(errBody schema.ErrorBody) error {
	return w.write(schema.EventError, schema.ErrorEvent{Type: schema.EventError, Error: errBody})
}

// Ping writes a ping keepalive frame.
func (w *SSEWriter) Ping() error {
	return w.write(schema.EventPing, map[string]string{"type": schema.EventPing})
}

// Flush flushes any buffered frame to the underlying writer.
func (w *SSEWriter) Flush() error { return w.fw.Flush() }

// write marshals payload and writes it as one SSE frame, flushing immediately so
// the client sees the event without waiting for the next one.
func (w *SSEWriter) write(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := w.fw.WriteFrame(event, data); err != nil {
		return err
	}
	return w.fw.Flush()
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
