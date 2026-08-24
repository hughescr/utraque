package stream

import (
	"encoding/json"
	"strconv"

	"github.com/hughescr/utraque/internal/anthropic"
	"github.com/hughescr/utraque/internal/anthropic/schema"
)

// Block kinds map one-to-one onto the Anthropic content block types utraque
// emits from a Codex stream.
const (
	kindText     = schema.BlockText
	kindThinking = schema.BlockThinking
	kindToolUse  = schema.BlockToolUse
)

// block is the per-output-index state for one Anthropic content block. At most
// one block is "active" (open and emitting) at a time — the core invariant.
// Every other seen output index is either buffering (deltas held in pending
// until it is promoted) or already stopped.
type block struct {
	outIdx int    // Codex output_index this block maps
	index  int    // Anthropic block index, assigned when opened
	kind   string // text | thinking | tool_use

	// tool_use identity, needed at content_block_start time.
	callID string
	name   string

	started bool // content_block_start emitted (block is or was active)
	stopped bool // content_block_stop emitted
	dropped bool // reasoning suppressed under emit_reasoning=drop; never opens
	done    bool // a terminal *.done arrived while buffering; close on promotion

	argsSeen bool   // a function_call_arguments.delta arrived for this call
	fullArgs string // full arguments from arguments.done / output_item.done

	pending []schema.Delta // deltas buffered while another block is active
	bytes   int            // size of buffered deltas, for the overflow bound
}

// blockFor returns the block for outIdx, creating it (and registering it in the
// FIFO order) on first sight. A thinking block created under emit_reasoning=drop
// is born dropped, so it never opens and its deltas are swallowed.
func (t *Translator) blockFor(outIdx int, kind string) *block {
	if b := t.blocks[outIdx]; b != nil {
		return b
	}
	b := &block{outIdx: outIdx, kind: kind}
	if kind == kindThinking && t.emitReasoning == emitReasoningDrop {
		b.dropped = true
	}
	t.blocks[outIdx] = b
	t.order = append(t.order, outIdx)
	return b
}

// contentBlock builds the content_block_start payload for b. The SSEWriter
// renders the empty-field forms ("text":"", input {}) exactly.
func contentBlock(b *block) schema.ContentBlock {
	switch b.kind {
	case kindToolUse:
		return schema.ContentBlock{Type: kindToolUse, ID: b.callID, Name: b.name, Input: json.RawMessage(`{}`)}
	default:
		return schema.ContentBlock{Type: b.kind}
	}
}

// openBlock allocates the monotonic index, makes b the active block, emits its
// content_block_start, and flushes any deltas buffered while it waited. It is a
// no-op on a block already started, stopped, or dropped.
func (t *Translator) openBlock(sink Sink, b *block) error {
	if b.started || b.stopped || b.dropped {
		return nil
	}
	b.index = t.nextIndex
	t.nextIndex++
	b.started = true
	t.active = b
	if err := sink.BlockStart(b.index, contentBlock(b)); err != nil {
		return err
	}
	for _, d := range b.pending {
		if err := sink.BlockDelta(b.index, d); err != nil {
			return err
		}
	}
	b.pending = nil
	b.bytes = 0
	return nil
}

// stopBlock closes b: it opens b first if it never opened (so there is a block
// to stop), emits the per-kind finalization (a synthetic signature_delta for a
// thinking block; the full arguments as one input_json_delta for a tool call
// that never streamed argument deltas), then content_block_stop. active is
// cleared if it was b.
func (t *Translator) stopBlock(sink Sink, b *block) error {
	if b.stopped || b.dropped {
		return nil
	}
	if !b.started {
		if err := t.openBlock(sink, b); err != nil {
			return err
		}
	}
	switch b.kind {
	case kindThinking:
		sig := schema.Delta{Type: schema.DeltaSignature, Signature: t.syntheticSignature(b)}
		if err := sink.BlockDelta(b.index, sig); err != nil {
			return err
		}
	case kindToolUse:
		if !b.argsSeen {
			args := b.fullArgs
			if args == "" {
				args = "{}"
			}
			d := schema.Delta{Type: schema.DeltaInputJSON, PartialJSON: args}
			if err := sink.BlockDelta(b.index, d); err != nil {
				return err
			}
		}
	}
	if err := sink.BlockStop(b.index); err != nil {
		return err
	}
	b.stopped = true
	if t.active == b {
		t.active = nil
	}
	return nil
}

// pushDelta routes one delta for block b. Fast path when b is the active block;
// lazy-open when nothing is active; otherwise buffer under b (never closing the
// active block early), enforcing the pending bounds.
func (t *Translator) pushDelta(sink Sink, b *block, d schema.Delta) error {
	if b.dropped || b.stopped {
		return nil
	}
	if t.active == b {
		return sink.BlockDelta(b.index, d)
	}
	if t.active == nil {
		if err := t.openBlock(sink, b); err != nil {
			return err
		}
		return sink.BlockDelta(b.index, d)
	}
	b.pending = append(b.pending, d)
	b.bytes += deltaSize(d)
	return t.enforceBounds(sink)
}

// markDoneOrClose closes b if it is the active block; otherwise records that its
// terminal .done arrived so the drain closes it when it is promoted. When
// nothing is active it kicks the drain so a head item that is already done makes
// progress immediately.
func (t *Translator) markDoneOrClose(sink Sink, b *block) error {
	if b.stopped || b.dropped {
		return nil
	}
	if t.active == b {
		return t.closeActive(sink)
	}
	b.done = true
	if t.active == nil {
		return t.drainNext(sink)
	}
	return nil
}

// closeActive stops the active block and promotes the next buffered item.
func (t *Translator) closeActive(sink Sink) error {
	if t.active == nil {
		return nil
	}
	if err := t.stopBlock(sink, t.active); err != nil {
		return err
	}
	return t.drainNext(sink)
}

// drainNext promotes buffered items in FIFO order into the active slot. A
// promoted item that already saw its .done is opened, flushed, and stopped in
// one step, and the drain continues to the next.
func (t *Translator) drainNext(sink Sink) error {
	for t.active == nil {
		b := t.firstBuffering()
		if b == nil {
			return nil
		}
		if err := t.openBlock(sink, b); err != nil {
			return err
		}
		if b.done {
			if err := t.stopBlock(sink, b); err != nil {
				return err
			}
			continue
		}
		return nil
	}
	return nil
}

// firstBuffering returns the earliest-seen block that is waiting to open.
func (t *Translator) firstBuffering() *block {
	for _, oi := range t.order {
		b := t.blocks[oi]
		if b != nil && !b.started && !b.stopped && !b.dropped {
			return b
		}
	}
	return nil
}

// finalizeAllBlocks opens, flushes, and stops every block still unfinished, in
// FIFO order, leaving no block open. Used at a clean terminus.
func (t *Translator) finalizeAllBlocks(sink Sink) error {
	for {
		b := t.active
		if b == nil {
			b = t.firstBuffering()
		}
		if b == nil {
			return nil
		}
		if err := t.stopBlock(sink, b); err != nil {
			return err
		}
	}
}

// enforceBounds guards the pending buffer. When the count of buffered items
// exceeds pending_items, or any one exceeds pending_item_bytes, it force-closes
// the active block (relieving the head-of-line block that is holding everything
// else up) and lets the drain promote the next item.
func (t *Translator) enforceBounds(sink Sink) error {
	if t.active == nil {
		return nil
	}
	count := 0
	overflow := false
	for _, oi := range t.order {
		b := t.blocks[oi]
		if b == nil || b.started || b.stopped || b.dropped {
			continue
		}
		count++
		if b.bytes > t.maxPendingBytes {
			overflow = true
		}
	}
	if count <= t.maxPendingItems && !overflow {
		return nil
	}
	t.log.Warn("stream pending buffer bounds exceeded; force-closing active block",
		"pending_items", count, "max_pending_items", t.maxPendingItems,
		"byte_overflow", overflow, "max_pending_bytes", t.maxPendingBytes)
	if err := t.stopBlock(sink, t.active); err != nil {
		return err
	}
	return t.drainNext(sink)
}

// syntheticSignature builds the signature carried by a thinking block's closing
// signature_delta. It begins with the fixed marker the Anthropic-leg sanitizer
// strips, so a mixed-model replay never sends this fabricated signature on to
// Anthropic. It is deterministic in the response id and output index.
func (t *Translator) syntheticSignature(b *block) string {
	return anthropic.SyntheticThinkingMarker + t.responseID + "-" + strconv.Itoa(b.outIdx)
}

// deltaSize is the buffered byte cost of one delta.
func deltaSize(d schema.Delta) int {
	return len(d.Text) + len(d.PartialJSON) + len(d.Thinking) + len(d.Signature)
}
