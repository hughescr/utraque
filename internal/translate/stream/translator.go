package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/sse"
)

// emit_reasoning modes.
const (
	emitReasoningThinking = "thinking"
	emitReasoningDrop     = "drop"
)

// on_truncate modes.
const (
	onTruncateError  = "error"
	onTruncateFinish = "finish"
)

// Defaults for the pending-buffer bounds and the timers.
const (
	defaultMaxPendingBytes = 1 << 20 // 1 MiB per buffered item
	defaultMaxPendingItems = 16
	defaultHeartbeat       = 15 * time.Second
	defaultUpstreamIdle    = 120 * time.Second
)

// Sentinel end-of-stream conditions.
var (
	// ErrNoData reports that the upstream closed before emitting a single event,
	// so the translator wrote nothing. The caller renders a real HTTP status and
	// Anthropic error envelope (failure mode 1).
	ErrNoData = errors.New("utraque/stream: upstream closed before any event")

	// ErrTruncated reports an upstream stream that ended mid-response, after some
	// output but before a terminal event.
	ErrTruncated = errors.New("utraque/stream: upstream stream truncated")

	// ErrIdleTimeout reports that no upstream event arrived within the idle bound.
	ErrIdleTimeout = errors.New("utraque/stream: upstream idle timeout")
)

// Options configure a Translator. The zero value is usable except for Model,
// which should be the client-requested model string echoed back in
// message_start.
type Options struct {
	// Model is the client-requested model string, echoed in message_start.model.
	Model string
	// InputTokens seeds message_start usage.input_tokens (an estimate, or 0).
	InputTokens int
	// EmitReasoning is "thinking" (default) or "drop".
	EmitReasoning string
	// OnTruncate is "error" (default) or "finish".
	OnTruncate string
	// Heartbeat injects a ping after this much silence. Zero uses the default;
	// negative disables it.
	Heartbeat time.Duration
	// UpstreamIdle aborts the stream after this much upstream silence. Zero uses
	// the default; negative disables it.
	UpstreamIdle time.Duration
	// MaxPendingBytes bounds one buffered item; zero uses the default.
	MaxPendingBytes int
	// MaxPendingItems bounds the number of buffered items; zero uses the default.
	MaxPendingItems int
	// Logger receives once-per-type unknown-event notices and bound warnings.
	Logger *slog.Logger
}

// Result summarises a Run: whether anything was emitted, whether a terminus was
// reached, whether that terminus was an error frame, and the unknown-event
// counts (keyed by type) for /healthz.
type Result struct {
	Started       bool
	Terminated    bool
	Errored       bool
	UnknownEvents map[string]int
}

// Translator maps a Codex Responses SSE stream onto an Anthropic Messages SSE
// stream, one event at a time, through a Sink.
//
// States: Idle -> Started -> (BetweenBlocks <-> BlockOpen)* -> Finalizing ->
// Done, with Aborted reachable from anywhere. At most one Anthropic content
// block is open at a time; interleaved parallel items that arrive while a block
// is open are buffered by output_index and drained in FIFO order.
//
// Three failure modes are kept distinct:
//
//  1. Before any byte is written (nothing emitted): Run returns a non-nil error
//     with Result.Started false, and the caller renders a real HTTP status and
//     an Anthropic error envelope.
//  2. Mid-stream failure (response.failed, an error frame, a transport error, a
//     truncated body, or the idle timeout): the active block is closed with a
//     bare content_block_stop, an error event is emitted, and the stream STOPS —
//     no message_delta / message_stop is faked over a broken stream. Run returns
//     nil with Result.Terminated and Result.Errored true.
//  3. Client interrupt (ctx cancelled): the upstream reader is closed, the sink
//     is abandoned with no terminus, and the scan goroutine is joined so nothing
//     leaks. Run returns ctx.Err() with Result.Terminated false.
//
// A Translator runs one stream at a time; construct one per request (Run resets
// its state defensively but is not safe for concurrent Runs).
type Translator struct {
	model           string
	inputTokens     int
	emitReasoning   string
	onTruncate      string
	heartbeat       time.Duration
	upstreamIdle    time.Duration
	maxPendingBytes int
	maxPendingItems int
	log             *slog.Logger

	// per-Run state
	responseID          string
	started             bool
	terminated          bool
	errored             bool
	nextIndex           int
	active              *block
	blocks              map[int]*block
	order               []int
	sawToolUse          bool
	incomplete          bool
	incompleteMaxTokens bool
	usage               schema.Usage
	unknown             map[string]int
}

// New builds a Translator from opts, filling defaults.
func New(opts Options) *Translator {
	t := &Translator{
		model:           opts.Model,
		inputTokens:     opts.InputTokens,
		emitReasoning:   opts.EmitReasoning,
		onTruncate:      opts.OnTruncate,
		heartbeat:       opts.Heartbeat,
		upstreamIdle:    opts.UpstreamIdle,
		maxPendingBytes: opts.MaxPendingBytes,
		maxPendingItems: opts.MaxPendingItems,
		log:             opts.Logger,
	}
	if t.emitReasoning == "" {
		t.emitReasoning = emitReasoningThinking
	}
	if t.onTruncate == "" {
		t.onTruncate = onTruncateError
	}
	if t.heartbeat == 0 {
		t.heartbeat = defaultHeartbeat
	}
	if t.upstreamIdle == 0 {
		t.upstreamIdle = defaultUpstreamIdle
	}
	if t.maxPendingBytes <= 0 {
		t.maxPendingBytes = defaultMaxPendingBytes
	}
	if t.maxPendingItems <= 0 {
		t.maxPendingItems = defaultMaxPendingItems
	}
	if t.log == nil {
		t.log = slog.New(slog.DiscardHandler)
	}
	return t
}

func (t *Translator) reset() {
	t.responseID = ""
	t.started = false
	t.terminated = false
	t.errored = false
	t.nextIndex = 0
	t.active = nil
	t.blocks = make(map[int]*block)
	t.order = nil
	t.sawToolUse = false
	t.incomplete = false
	t.incompleteMaxTokens = false
	t.usage = schema.Usage{}
	t.unknown = make(map[string]int)
}

func (t *Translator) result() Result {
	return Result{Started: t.started, Terminated: t.terminated, Errored: t.errored, UnknownEvents: t.unknown}
}

// Run drives the translation of r into sink until a terminus, an error, or ctx
// cancellation. See Translator for the three failure modes and their return
// contract.
func (t *Translator) Run(ctx context.Context, r io.Reader, sink Sink) (Result, error) {
	t.reset()

	scanCtx, cancel := context.WithCancel(ctx)
	frames := make(chan sse.Frame)
	scanErrc := make(chan error, 1)
	scanDone := make(chan struct{})

	go func() {
		defer close(scanDone)
		sc := sse.NewScanner(r)
		for sc.Scan() {
			select {
			case frames <- sc.Frame():
			case <-scanCtx.Done():
				return
			}
		}
		scanErrc <- sc.Err()
	}()

	// finish tears the reader goroutine down before returning: cancel unblocks a
	// pending frame send, closing the reader unblocks a pending Read, and the
	// join guarantees no goroutine outlives the call.
	finish := func(err error) (Result, error) {
		cancel()
		if c, ok := r.(io.Closer); ok {
			_ = c.Close()
		}
		<-scanDone
		return t.result(), err
	}

	var hb, idle *time.Timer
	var heartbeatC, idleC <-chan time.Time
	if t.heartbeat > 0 {
		hb = time.NewTimer(t.heartbeat)
		heartbeatC = hb.C
		defer hb.Stop()
	}
	if t.upstreamIdle > 0 {
		idle = time.NewTimer(t.upstreamIdle)
		idleC = idle.C
		defer idle.Stop()
	}
	// onUpstream resets both timers: an upstream frame is both activity (idle) and
	// a reason to defer the next keepalive (heartbeat).
	onUpstream := func() {
		if hb != nil {
			hb.Reset(t.heartbeat)
		}
		if idle != nil {
			idle.Reset(t.upstreamIdle)
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Client interrupt (failure mode 3): abandon without a terminus.
			return finish(ctx.Err())

		case fr := <-frames:
			onUpstream()
			done, err := t.handle(sink, fr)
			if err != nil {
				return finish(err)
			}
			// done covers the explicit terminus paths; t.terminated additionally
			// catches a mid-stream error emitted from deep inside handling (a
			// pending-buffer-bounds abort), which returns done=false.
			if done || t.terminated {
				return finish(nil)
			}

		case err := <-scanErrc:
			return finish(t.handleEnd(sink, err))

		case <-heartbeatC:
			// A ping before message_start would commit a 200 with bytes on the wire
			// while Result.Started is still false, breaking the mode-1 contract, and
			// would precede message_start — an order real Anthropic streams never
			// emit. Hold the keepalive until the stream has started.
			if t.started {
				if err := sink.Ping(); err != nil {
					return finish(err)
				}
			}
			// A ping is our own output, not upstream activity: reset only the
			// heartbeat, leaving the idle bound to keep counting upstream silence.
			hb.Reset(t.heartbeat)

		case <-idleC:
			// No upstream event within the idle bound: treat as mid-stream failure.
			return finish(t.handleEnd(sink, ErrIdleTimeout))
		}
	}
}

// handle dispatches one Codex event. It returns done=true once a terminus (a
// clean message_stop or an error frame) has been emitted, and a non-nil error
// only for failure mode 1 (nothing emitted yet) or a sink write failure.
func (t *Translator) handle(sink Sink, fr sse.Frame) (bool, error) {
	var ev cschema.StreamEvent
	if len(fr.Data) > 0 {
		if err := json.Unmarshal(fr.Data, &ev); err != nil {
			t.countUnknown("<malformed-json>")
			return false, nil
		}
	}
	typ := ev.Type
	if typ == "" {
		typ = fr.Event
	}

	switch typ {
	case cschema.EventResponseCreated:
		if ev.Response != nil {
			t.responseID = ev.Response.ID
		}
		return false, t.ensureStarted(sink)

	case cschema.EventResponseInProgress:
		return false, nil

	case cschema.EventOutputItemAdded:
		return false, t.onItemAdded(sink, &ev)

	case cschema.EventContentPartAdded:
		return false, t.onContentPartAdded(sink, &ev)

	case cschema.EventOutputTextDelta:
		if err := t.ensureStarted(sink); err != nil {
			return false, err
		}
		b := t.blockFor(ev.OutputIndex, kindText)
		return false, t.pushDelta(sink, b, schema.Delta{Type: schema.DeltaText, Text: ev.Delta})

	case cschema.EventOutputTextDone:
		b := t.blockFor(ev.OutputIndex, kindText)
		return false, t.markDoneOrClose(sink, b)

	case cschema.EventReasoningSummaryPartAdded:
		return false, t.onReasoningPartAdded(sink, &ev)

	case cschema.EventReasoningSummaryTextDelta, cschema.EventReasoningTextDelta:
		if err := t.ensureStarted(sink); err != nil {
			return false, err
		}
		b := t.blockFor(ev.OutputIndex, kindThinking)
		return false, t.pushDelta(sink, b, schema.Delta{Type: schema.DeltaThinking, Thinking: ev.Delta})

	case cschema.EventReasoningSummaryTextDone, cschema.EventReasoningTextDone:
		b := t.blockFor(ev.OutputIndex, kindThinking)
		return false, t.markDoneOrClose(sink, b)

	case cschema.EventFunctionCallArgumentsDelta:
		if err := t.ensureStarted(sink); err != nil {
			return false, err
		}
		t.sawToolUse = true
		b := t.blockFor(ev.OutputIndex, kindToolUse)
		b.argsSeen = true
		return false, t.pushDelta(sink, b, schema.Delta{Type: schema.DeltaInputJSON, PartialJSON: ev.Delta})

	case cschema.EventFunctionCallArgumentsDone:
		b := t.blockFor(ev.OutputIndex, kindToolUse)
		if ev.Arguments != "" {
			b.fullArgs = ev.Arguments
		}
		b.argsDone = true
		return false, t.markDoneOrClose(sink, b)

	case cschema.EventOutputItemDone:
		return false, t.onItemDone(sink, &ev)

	case cschema.EventContentPartDone:
		// The text block is closed by output_text.done; nothing to do here.
		return false, nil

	case cschema.EventResponseCompleted:
		if ev.Response != nil {
			t.usage = mapUsage(ev.Response.Usage)
		}
		if err := t.ensureStarted(sink); err != nil {
			return true, err
		}
		return true, t.finalizeClean(sink)

	case cschema.EventResponseIncomplete:
		if ev.Response != nil {
			t.usage = mapUsage(ev.Response.Usage)
			t.incomplete = true
			if d := ev.Response.IncompleteDetails; d != nil && d.Reason == cschema.IncompleteReasonMaxOutputTokens {
				t.incompleteMaxTokens = true
			}
		}
		if err := t.ensureStarted(sink); err != nil {
			return true, err
		}
		return true, t.finalizeClean(sink)

	case cschema.EventResponseFailed:
		msg := "the upstream response failed"
		if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
			msg = ev.Response.Error.Message
		}
		return t.fail(sink, msg)

	case cschema.EventError:
		msg := ev.Message
		if msg == "" {
			msg = "upstream error"
		}
		return t.fail(sink, msg)

	default:
		t.countUnknown(typ)
		return false, nil
	}
}

func (t *Translator) onItemAdded(sink Sink, ev *cschema.StreamEvent) error {
	if ev.Item == nil {
		return nil
	}
	switch ev.Item.Type {
	case cschema.OutputItemFunctionCall:
		if err := t.ensureStarted(sink); err != nil {
			return err
		}
		t.sawToolUse = true
		b := t.blockFor(ev.OutputIndex, kindToolUse)
		if ev.Item.CallID != "" {
			b.callID = ev.Item.CallID
		}
		if ev.Item.Name != "" {
			b.name = ev.Item.Name
		}
		if ev.Item.Arguments != "" {
			b.fullArgs = ev.Item.Arguments
		}
		// Open immediately when the call's identity is known and nothing else is
		// open; otherwise it opens when promoted from the buffer.
		if b.callID != "" && b.name != "" && t.active == nil {
			return t.openBlock(sink, b)
		}
		return nil
	default:
		// message / reasoning items open on their content_part / delta events.
		return nil
	}
}

func (t *Translator) onContentPartAdded(sink Sink, ev *cschema.StreamEvent) error {
	if err := t.ensureStarted(sink); err != nil {
		return err
	}
	b := t.blockFor(ev.OutputIndex, kindText)
	if t.active == nil {
		return t.openBlock(sink, b)
	}
	return nil
}

func (t *Translator) onReasoningPartAdded(sink Sink, ev *cschema.StreamEvent) error {
	if err := t.ensureStarted(sink); err != nil {
		return err
	}
	b := t.blockFor(ev.OutputIndex, kindThinking)
	if b.dropped {
		return nil
	}
	if t.active == nil {
		return t.openBlock(sink, b)
	}
	return nil
}

func (t *Translator) onItemDone(sink Sink, ev *cschema.StreamEvent) error {
	if ev.Item == nil {
		return nil
	}
	switch ev.Item.Type {
	case cschema.OutputItemFunctionCall:
		// A tool call whose only sighting is output_item.done still counts.
		t.sawToolUse = true
		b := t.blockFor(ev.OutputIndex, kindToolUse)
		if b.callID == "" && ev.Item.CallID != "" {
			b.callID = ev.Item.CallID
		}
		if b.name == "" && ev.Item.Name != "" {
			b.name = ev.Item.Name
		}
		if b.fullArgs == "" && ev.Item.Arguments != "" {
			b.fullArgs = ev.Item.Arguments
		}
		b.argsDone = true
		return t.markDoneOrClose(sink, b)
	default:
		b := t.blocks[ev.OutputIndex]
		if b == nil {
			return nil
		}
		return t.markDoneOrClose(sink, b)
	}
}

// fail handles a stream-level failure event (response.failed / an error frame).
// Before message_start it is failure mode 1 (return an error for the caller to
// render); after, it is mode 2 (close the active block, emit an error frame).
func (t *Translator) fail(sink Sink, msg string) (bool, error) {
	if !t.started {
		// 502, not the api_error default of 500: the UPSTREAM failed, and the
		// distinction is what tells the caller whether retrying could help. It
		// matches the status ErrNoData and ErrTruncated already carry for the
		// same class of condition.
		return true, apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI, "%s", msg)
	}
	return true, t.emitMidStreamError(sink, msg)
}

// handleEnd resolves the end of the upstream stream. cause is nil for a clean
// EOF, or the read/idle error otherwise.
func (t *Translator) handleEnd(sink Sink, cause error) error {
	if t.terminated {
		return nil
	}
	if !t.started {
		if cause == nil {
			return ErrNoData
		}
		return cause
	}
	// A clean EOF with on_truncate=finish synthesises a clean terminus; every
	// other end after some output is a mid-stream failure.
	if cause == nil && t.onTruncate == onTruncateFinish {
		return t.finalizeClean(sink)
	}
	return t.emitMidStreamError(sink, endMessage(cause))
}

// endMessage phrases the mid-stream error for an end-of-stream cause.
func endMessage(cause error) string {
	switch {
	case cause == nil || errors.Is(cause, ErrTruncated):
		return "the upstream stream was truncated before completion"
	case errors.Is(cause, ErrIdleTimeout):
		return "no data received from the upstream model"
	default:
		return "the upstream stream failed: " + cause.Error()
	}
}

// emitMidStreamError closes the active block (keeping the block grammar
// balanced) and emits an error frame, then STOPS: no message_delta /
// message_stop is written over a broken stream.
func (t *Translator) emitMidStreamError(sink Sink, message string) error {
	if b := t.active; b != nil {
		// A thinking block still closes with its synthetic signature_delta, even
		// on a failure. That signature carries the marker the Anthropic-leg
		// sanitizer recognises; without it the client keeps an UNSIGNED thinking
		// block in its history, the sanitizer cannot classify it, and the next
		// turn on a Claude model forwards it to Anthropic, which rejects an
		// unsigned thinking block with a 400.
		//
		// Only thinking. A tool_use closed here is deliberately left without
		// fabricated arguments: the stream is broken, and inventing "{}" would
		// present an unmade call as a made one.
		if b.kind == kindThinking {
			sig := schema.Delta{Type: schema.DeltaSignature, Signature: t.syntheticSignature(b)}
			if err := sink.BlockDelta(b.index, sig); err != nil {
				return err
			}
		}
		if err := sink.BlockStop(b.index); err != nil {
			return err
		}
		b.stopped = true
		t.active = nil
	}
	t.terminated = true
	t.errored = true
	return sink.Error(schema.ErrorBody{Type: string(apierr.TypeAPI), Message: message})
}

// finalizeClean drains every block, then emits message_delta and message_stop.
// A tool call whose arguments streamed incrementally but never finalized (no
// function_call_arguments.done / output_item.done) is a truncated call: the
// partial_json already on the wire is not valid JSON, so a clean terminus would
// present an unparseable tool_use. Refuse to fake it — abort with a mid-stream
// error instead. This also governs on_truncate=finish over a partial tool call.
func (t *Translator) finalizeClean(sink Sink) error {
	for _, oi := range t.order {
		if b := t.blocks[oi]; b != nil && b.kind == kindToolUse && b.argsSeen && !b.argsDone {
			return t.emitMidStreamError(sink, "the upstream stream was truncated before the tool call arguments completed")
		}
	}
	if err := t.finalizeAllBlocks(sink); err != nil {
		return err
	}
	if err := sink.MessageDelta(MessageDelta{StopReason: t.stopReason(), Usage: t.terminalUsage()}); err != nil {
		return err
	}
	if err := sink.MessageStop(); err != nil {
		return err
	}
	t.terminated = true
	return nil
}

// terminalUsage is the usage reported on message_delta.
//
// An upstream terminus that carried no usage block, or an explicit zero input
// count, keeps the message_start estimate: a zero-token prompt is never a true
// statement, and a client that shows a context bar would read it as one.
//
// The substitution belongs HERE rather than in a sink. Both sinks see the same
// MessageDelta, so stream:true and stream:false report identical numbers by
// construction — which is the whole point of the Sink seam. Doing it in the
// Aggregator alone (as it once was) let the streaming path emit
// "input_tokens":0 for a request whose non-streaming twin reported the estimate.
func (t *Translator) terminalUsage() schema.Usage {
	u := t.usage
	if u.InputTokens == 0 {
		u.InputTokens = t.inputTokens
	}
	return u
}

// stopReason applies the finalization rule: tool_use wins; else max_tokens on a
// max-output-tokens incomplete; else end_turn.
func (t *Translator) stopReason() string {
	switch {
	case t.sawToolUse:
		return schema.StopToolUse
	case t.incompleteMaxTokens:
		return schema.StopMaxTokens
	default:
		return schema.StopEndTurn
	}
}

// ensureStarted emits message_start exactly once, on the first event that needs
// it. The id is derived from the captured response id.
func (t *Translator) ensureStarted(sink Sink) error {
	if t.started {
		return nil
	}
	t.started = true
	return sink.MessageStart(MessageStart{
		ID:          "msg_codex_" + t.responseID,
		Model:       t.model,
		InputTokens: t.inputTokens,
	})
}

// countUnknown records an unrecognised event type and logs it once per type.
func (t *Translator) countUnknown(typ string) {
	n := t.unknown[typ]
	t.unknown[typ] = n + 1
	if n == 0 {
		t.log.Debug("unknown Codex stream event type", "type", typ)
	}
}

// mapUsage maps a Responses usage block onto the Anthropic usage block, folding
// cached input tokens into cache_read_input_tokens.
func mapUsage(u *cschema.Usage) schema.Usage {
	if u == nil {
		return schema.Usage{}
	}
	return schema.Usage{
		InputTokens:          u.InputTokens,
		OutputTokens:         u.OutputTokens,
		CacheReadInputTokens: u.CachedTokens(),
	}
}
