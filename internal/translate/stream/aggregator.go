package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
)

// Aggregator is the non-streaming Sink. It folds the very same semantic events
// the SSEWriter serialises into one complete Anthropic MessagesResponse, so a
// stream:false request and a stream:true request share 100% of the translation
// logic: the Translator is the only place that understands the Codex protocol,
// and the Aggregator merely accumulates what it is told.
//
// The two paths therefore cannot diverge by construction, and
// TestAggregatorEqualsSSEFold pins that down for every stream fixture by
// comparing the Aggregator's folded message against the same fold performed
// over the SSEWriter's committed golden bytes.
//
// Error handling is the one place a buffering sink must be opinionated. A
// mid-stream failure (Sink.Error) leaves a half message: some blocks complete,
// no stop_reason, no usage. Returning that to a non-streaming client would
// present a truncated answer as a finished one, so Message reports the failure
// as an error and the caller renders an Anthropic error envelope instead.
//
// A non-streaming caller should build its Translator with Heartbeat: -1 — a
// keepalive has no meaning when nothing is on the wire. Ping is tolerated (and
// ignored) regardless.
type Aggregator struct {
	id    string
	model string

	content []*aggBlock
	byIndex map[int]*aggBlock
	open    *aggBlock

	started    bool
	stopped    bool
	stopReason string
	usage      schema.Usage

	failed  bool
	errBody schema.ErrorBody
}

var _ Sink = (*Aggregator)(nil)

// ErrIncomplete reports that the sink was asked for a message before a terminus
// arrived — a truncated fold that must never be presented as a finished answer.
var ErrIncomplete = errors.New("utraque/stream: message incomplete")

// aggBlock accumulates one content block between its start and its stop.
type aggBlock struct {
	index int
	kind  string
	id    string // tool_use call id
	name  string // tool_use name

	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	args      strings.Builder

	raw     schema.ContentBlock // verbatim start payload, for unknown kinds
	stopped bool
}

// NewAggregator builds an empty Aggregator ready to receive one stream.
func NewAggregator() *Aggregator {
	return &Aggregator{byIndex: make(map[int]*aggBlock)}
}

func aggErrf(format string, args ...any) error {
	return fmt.Errorf("utraque/stream: aggregator: "+format, args...)
}

// MessageStart records the message identity and seeds usage with the input
// estimate carried in message_start.
func (a *Aggregator) MessageStart(m MessageStart) error {
	if a.started {
		return aggErrf("second message_start")
	}
	a.started = true
	a.id = m.ID
	a.model = m.Model
	a.usage = schema.Usage{InputTokens: m.InputTokens}
	return nil
}

// BlockStart opens a block. The Sink contract guarantees one block open at a
// time with strictly increasing indices; violating it is a translator bug, so
// it is reported rather than papered over.
func (a *Aggregator) BlockStart(index int, blk schema.ContentBlock) error {
	if !a.started {
		return aggErrf("content_block_start(%d) before message_start", index)
	}
	if a.open != nil {
		return aggErrf("content_block_start(%d) while block %d is still open", index, a.open.index)
	}
	if _, dup := a.byIndex[index]; dup {
		return aggErrf("duplicate block index %d", index)
	}
	b := &aggBlock{index: index, kind: blk.Type, id: blk.ID, name: blk.Name, raw: blk}
	// A start payload may prefill text/thinking; keep it so nothing is lost.
	b.text.WriteString(blk.Text)
	b.thinking.WriteString(blk.Thinking)
	b.signature.WriteString(blk.Signature)
	a.byIndex[index] = b
	a.content = append(a.content, b)
	a.open = b
	return nil
}

// BlockDelta appends one delta to the open block.
func (a *Aggregator) BlockDelta(index int, d schema.Delta) error {
	if a.open == nil || a.open.index != index {
		return aggErrf("content_block_delta(%d) with no matching open block", index)
	}
	b := a.open
	switch d.Type {
	case schema.DeltaText:
		b.text.WriteString(d.Text)
	case schema.DeltaThinking:
		b.thinking.WriteString(d.Thinking)
	case schema.DeltaSignature:
		b.signature.WriteString(d.Signature)
	case schema.DeltaInputJSON:
		b.args.WriteString(d.PartialJSON)
	default:
		// An unfoldable delta would silently vanish from the non-streaming
		// answer while still reaching a streaming client — exactly the
		// divergence this sink exists to prevent. Fail loudly instead.
		return aggErrf("unknown delta type %q on block %d", d.Type, index)
	}
	return nil
}

// BlockStop closes the open block.
func (a *Aggregator) BlockStop(index int) error {
	if a.open == nil || a.open.index != index {
		return aggErrf("content_block_stop(%d) with no matching open block", index)
	}
	a.open.stopped = true
	a.open = nil
	return nil
}

// MessageDelta records the terminal stop reason and usage.
//
// The Translator already substitutes the message_start estimate for a zero
// upstream input count (see Translator.terminalUsage), so both sinks see the
// same numbers. This repeats the rule only for a caller driving the Sink
// directly, which has no translator to do it: it must never be the ONLY place
// the substitution happens, or the streaming path would diverge again.
func (a *Aggregator) MessageDelta(d MessageDelta) error {
	a.stopReason = d.StopReason
	u := d.Usage
	if u.InputTokens == 0 {
		u.InputTokens = a.usage.InputTokens
	}
	a.usage = u
	return nil
}

// MessageStop marks the fold complete.
func (a *Aggregator) MessageStop() error {
	a.stopped = true
	return nil
}

// Error records a mid-stream failure; Message then reports it to the caller.
func (a *Aggregator) Error(e schema.ErrorBody) error {
	a.failed = true
	a.errBody = e
	return nil
}

// Ping is a streaming keepalive and carries no content to fold.
func (a *Aggregator) Ping() error { return nil }

// Failed reports whether a mid-stream error terminated the fold.
func (a *Aggregator) Failed() bool { return a.failed }

// Message returns the folded Anthropic response.
//
// It returns an error, never a partial message, when the stream failed
// mid-flight, when nothing was emitted at all, or when no terminus arrived. The
// error is an *apierr.Error the caller can render directly as an Anthropic
// error envelope with a matching HTTP status.
func (a *Aggregator) Message() (*schema.MessagesResponse, error) {
	blocks, err := a.finishedBlocks()
	if err != nil {
		return nil, err
	}
	return &schema.MessagesResponse{
		ID:      a.id,
		Type:    "message",
		Role:    schema.RoleAssistant,
		Model:   a.model,
		Content: blocks,
		// stop_sequence stays null: the Codex leg has no stop-sequence concept,
		// so claiming one would be a fabrication.
		StopReason: a.stopReason,
		Usage:      a.usage,
	}, nil
}

// MessageJSON renders the folded response as the exact Anthropic wire JSON, the
// non-streaming counterpart to SSEWriter's byte-faithful frames: a text block
// really carries "text":"", a tool_use really carries an "input" object, and
// stop_reason / stop_sequence are present as null when unset. Serving
// Message through the shared omitempty schema types would drop those fields.
func (a *Aggregator) MessageJSON() ([]byte, error) {
	msg, err := a.Message()
	if err != nil {
		return nil, err
	}
	out := wireResponse{
		ID:           msg.ID,
		Type:         msg.Type,
		Role:         msg.Role,
		Model:        msg.Model,
		Content:      make([]any, 0, len(msg.Content)),
		StopReason:   strPtr(msg.StopReason),
		StopSequence: msg.StopSequence,
		Usage:        msg.Usage,
	}
	for _, b := range msg.Content {
		switch b.Type {
		case schema.BlockText:
			out.Content = append(out.Content, wireRespText{Type: schema.BlockText, Text: b.Text})
		case schema.BlockThinking:
			out.Content = append(out.Content, wireRespThinking{
				Type: schema.BlockThinking, Thinking: b.Thinking, Signature: b.Signature,
			})
		case schema.BlockToolUse:
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out.Content = append(out.Content, wireStartToolUse{
				Type: schema.BlockToolUse, ID: b.ID, Name: b.Name, Input: input,
			})
		default:
			out.Content = append(out.Content, b)
		}
	}
	return json.Marshal(out)
}

// finishedBlocks materialises every accumulated block, after enforcing the
// conditions under which a complete message may be claimed at all.
func (a *Aggregator) finishedBlocks() ([]schema.ContentBlock, error) {
	if a.failed {
		kind := apierr.Type(a.errBody.Type)
		if kind == "" {
			kind = apierr.TypeAPI
		}
		msg := a.errBody.Message
		if msg == "" {
			msg = "the upstream stream failed"
		}
		e := apierr.New(kind, "%s", msg)
		if kind == apierr.TypeAPI {
			// The generic type means "the upstream failed and said no more", which
			// is a gateway failure (502), not an internal one (500). Its siblings
			// ErrNoData / ErrTruncated / ErrIncomplete are pinned to 502 for the
			// same reason: the status is what tells the caller whether retrying
			// could help. A more specific upstream type keeps its own status.
			e.Status = http.StatusBadGateway
		}
		return nil, e
	}
	if !a.started {
		return nil, apierr.Wrap(ErrNoData, apierr.TypeAPI, "the upstream returned no response")
	}
	if !a.stopped {
		return nil, apierr.Wrap(ErrIncomplete, apierr.TypeAPI,
			"the upstream stream ended before the message was complete")
	}

	blocks := make([]schema.ContentBlock, 0, len(a.content))
	for _, b := range a.content {
		if !b.stopped {
			return nil, apierr.Wrap(ErrIncomplete, apierr.TypeAPI,
				"content block %d was never closed", b.index)
		}
		cb := b.finish()
		// A tool_use whose arguments are not parseable JSON is unusable to the
		// client. The Translator refuses to reach a clean terminus over one, so
		// this is a belt-and-braces guard against ever shipping one.
		if cb.Type == schema.BlockToolUse && !json.Valid(cb.Input) {
			return nil, apierr.API("the upstream tool call %q produced invalid JSON arguments", cb.Name)
		}
		blocks = append(blocks, cb)
	}
	return blocks, nil
}

// finish renders one accumulated block as its Anthropic content block.
func (b *aggBlock) finish() schema.ContentBlock {
	switch b.kind {
	case schema.BlockText:
		return schema.ContentBlock{Type: schema.BlockText, Text: b.text.String()}
	case schema.BlockThinking:
		return schema.ContentBlock{
			Type:      schema.BlockThinking,
			Thinking:  b.thinking.String(),
			Signature: b.signature.String(),
		}
	case schema.BlockToolUse:
		args := b.args.String()
		if args == "" {
			args = "{}"
		}
		return schema.ContentBlock{
			Type:  schema.BlockToolUse,
			ID:    b.id,
			Name:  b.name,
			Input: json.RawMessage(args),
		}
	default:
		return b.raw
	}
}

// --- wire types: exact Anthropic non-streaming JSON shapes ---

type wireRespText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireRespThinking struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type wireResponse struct {
	ID           string       `json:"id"`
	Type         string       `json:"type"`
	Role         string       `json:"role"`
	Model        string       `json:"model"`
	Content      []any        `json:"content"`
	StopReason   *string      `json:"stop_reason"`
	StopSequence *string      `json:"stop_sequence"`
	Usage        schema.Usage `json:"usage"`
}
