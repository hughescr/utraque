package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hughescr/utraque/internal/anthropic/schema"
)

// SyntheticThinkingMarker tags every thinking block utraque mints for a Codex
// turn. Anthropic signs real thinking blocks and rejects a replayed block whose
// signature it did not issue, so a marked block must be stripped before the
// history is sent to the Anthropic leg. The marker is a fixed constant, never
// derived, so the detection scan is a single substring search.
//
// The marker lives ONLY where utraque itself writes bytes: the "signature"
// field of a thinking block, and the "data" field of a redacted_thinking
// block. It is deliberately not looked for in "thinking" text — that text is
// model prose, and a session reasoning about utraque's own source would
// otherwise have its genuine, Anthropic-signed thinking blocks stripped.
const SyntheticThinkingMarker = "utraque-synthetic-v1:"

var syntheticMarker = []byte(SyntheticThinkingMarker)

var errNotJSONObject = errors.New("utraque/anthropic: body is not a JSON object")

// HasSyntheticThinking reports whether raw could contain a block we minted.
// This is the cheap gate on the hot path: no allocation, no parse.
func HasSyntheticThinking(raw []byte) bool {
	return bytes.Contains(raw, syntheticMarker)
}

// IsSyntheticBlock reports whether b is a thinking block minted by utraque.
func IsSyntheticBlock(b schema.ContentBlock) bool {
	switch b.Type {
	case schema.BlockThinking, schema.BlockRedactedThinking:
	default:
		return false
	}
	return strings.Contains(b.Signature, SyntheticThinkingMarker) ||
		strings.Contains(b.Data, SyntheticThinkingMarker)
}

// SanitizeMessages returns in with synthetic thinking blocks removed, plus
// whether anything changed. The input is never mutated.
//
// A message whose blocks are all synthetic is dropped rather than left with an
// empty content array, which Anthropic rejects. Such a message is by
// construction assistant-side reasoning only — it carries no tool_use — so
// dropping it cannot orphan a following tool_result.
//
// Known limitation, the mixed case: an assistant turn of
// [synthetic thinking, tool_use, ...] keeps its tool_use and loses its leading
// thinking block. If that turn is the last assistant message of a request with
// extended thinking enabled, Anthropic rejects it ("a final assistant message
// must start with a thinking block"). There is no repair available here — the
// block carries a signature Anthropic never issued, so replaying it is a
// guaranteed 400, and dropping the tool_use instead would orphan the
// tool_result that follows. Stripping is the strictly lesser failure. Minting
// the block differently is a Codex-translation concern, not a sanitizer one;
// forward returns a warning log when this shape is produced.
func SanitizeMessages(in []schema.Message) ([]schema.Message, bool) {
	changed := false
	out := make([]schema.Message, 0, len(in))
	for _, m := range in {
		if m.Content == nil || m.Content.IsString || len(m.Content.Blocks) == 0 {
			out = append(out, m)
			continue
		}
		kept := make([]schema.ContentBlock, 0, len(m.Content.Blocks))
		dropped := 0
		for _, b := range m.Content.Blocks {
			if IsSyntheticBlock(b) {
				dropped++
				continue
			}
			kept = append(kept, b)
		}
		if dropped == 0 {
			out = append(out, m)
			continue
		}
		changed = true
		if len(kept) == 0 {
			continue
		}
		m.Content = &schema.Content{Blocks: kept}
		out = append(out, m)
	}
	return out, changed
}

// SanitizeMessagesRequest strips synthetic thinking blocks from a decoded
// request in place and reports whether anything changed.
func SanitizeMessagesRequest(req *schema.MessagesRequest) bool {
	if req == nil {
		return false
	}
	msgs, changed := SanitizeMessages(req.Messages)
	if changed {
		req.Messages = msgs
	}
	return changed
}

// Report describes what a raw sanitize did, beyond "something changed".
type Report struct {
	// Changed is true when the returned bytes differ from the input.
	Changed bool

	// Dropped counts the synthetic thinking blocks removed.
	Dropped int

	// HeadlessToolUse is true when at least one assistant turn lost its
	// leading thinking block but kept a tool_use block. See SanitizeMessages
	// for why that shape cannot be repaired here and what it costs.
	HeadlessToolUse bool
}

// Sanitize strips synthetic thinking blocks from a raw Messages or
// CountTokens body, reporting whether anything changed.
func Sanitize(raw []byte) ([]byte, bool, error) {
	out, rep, err := SanitizeWithReport(raw)
	return out, rep.Changed, err
}

// SanitizeWithReport is Sanitize with the detail a caller needs to log.
//
// It returns raw itself — the identical slice, not a copy — whenever nothing
// needs changing: no marker, not a JSON object, no "messages" key, or no
// synthetic block actually found. Only the rewrite path allocates.
//
// The rewrite is surgical: the body, each message, and each content array are
// taken apart into their original raw bytes, the marked blocks are cut out,
// and everything else is written back exactly as it arrived — same bytes, same
// key order. Nothing round-trips through a Go struct, so fields this build
// does not model (citations, container/server-tool payloads, whatever
// Anthropic ships next) cannot be silently dropped or reshaped.
func SanitizeWithReport(raw []byte) ([]byte, Report, error) {
	var rep Report
	if !HasSyntheticThinking(raw) {
		return raw, rep, nil
	}
	obj, err := decodeObject(raw)
	if err != nil {
		return raw, rep, err
	}
	rawMsgs, ok := obj.vals["messages"]
	if !ok {
		return raw, rep, nil
	}
	cleaned, err := sanitizeRawMessages(rawMsgs, &rep)
	if err != nil {
		return raw, Report{}, err
	}
	if !rep.Changed {
		return raw, Report{}, nil
	}
	obj.vals["messages"] = cleaned
	out, err := obj.encode()
	if err != nil {
		return raw, Report{}, err
	}
	return out, rep, nil
}

// sanitizeRawMessages rewrites the raw "messages" array. A message that needs
// no edit is copied through as its original bytes.
func sanitizeRawMessages(rawMsgs []byte, rep *Report) ([]byte, error) {
	msgs, ok := decodeArray(rawMsgs)
	if !ok {
		return nil, errors.New("utraque/anthropic: messages is not a JSON array")
	}
	kept := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		if !HasSyntheticThinking(m) {
			kept = append(kept, m)
			continue
		}
		out, changed, drop, err := sanitizeRawMessage(m, rep)
		if err != nil {
			return nil, err
		}
		switch {
		case !changed:
			kept = append(kept, m)
		case drop:
			rep.Changed = true
		default:
			rep.Changed = true
			kept = append(kept, out)
		}
	}
	if !rep.Changed {
		return nil, nil
	}
	return encodeArray(kept), nil
}

// sanitizeRawMessage rewrites one raw message object. drop reports that every
// block was synthetic, so the message must go rather than be left with an
// empty content array (which Anthropic rejects).
func sanitizeRawMessage(rawMsg []byte, rep *Report) (out []byte, changed bool, drop bool, err error) {
	obj, err := decodeObject(rawMsg)
	if err != nil {
		return nil, false, false, err
	}
	rawContent, ok := obj.vals["content"]
	if !ok {
		return nil, false, false, nil
	}
	blocks, ok := decodeArray(rawContent)
	if !ok {
		// String content: no blocks, nothing to strip.
		return nil, false, false, nil
	}

	kept := make([]json.RawMessage, 0, len(blocks))
	dropped := 0
	toolUse := false
	for _, b := range blocks {
		kind, synthetic, err := classifyRawBlock(b)
		if err != nil {
			return nil, false, false, err
		}
		if synthetic {
			dropped++
			continue
		}
		if kind == schema.BlockToolUse {
			toolUse = true
		}
		kept = append(kept, b)
	}
	if dropped == 0 {
		return nil, false, false, nil
	}
	rep.Dropped += dropped
	if len(kept) == 0 {
		return nil, true, true, nil
	}
	if toolUse {
		rep.HeadlessToolUse = true
	}
	obj.vals["content"] = encodeArray(kept)
	enc, err := obj.encode()
	if err != nil {
		return nil, false, false, err
	}
	return enc, true, false, nil
}

// blockMarks is the minimal shape needed to decide whether a raw content block
// is one utraque minted. Only the fields utraque itself writes are read.
type blockMarks struct {
	Type      string `json:"type"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

func classifyRawBlock(raw []byte) (kind string, synthetic bool, err error) {
	var m blockMarks
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false, fmt.Errorf("utraque/anthropic: decode content block: %w", err)
	}
	switch m.Type {
	case schema.BlockThinking, schema.BlockRedactedThinking:
		synthetic = strings.Contains(m.Signature, SyntheticThinkingMarker) ||
			strings.Contains(m.Data, SyntheticThinkingMarker)
	}
	return m.Type, synthetic, nil
}

// orderedObject is a JSON object whose keys keep their source order and whose
// values keep their source bytes.
type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func decodeObject(raw []byte) (*orderedObject, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("utraque/anthropic: decode body: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotJSONObject
	}
	o := &orderedObject{vals: make(map[string]json.RawMessage)}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("utraque/anthropic: decode key: %w", err)
		}
		k, ok := kt.(string)
		if !ok {
			return nil, errNotJSONObject
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("utraque/anthropic: decode value for %q: %w", k, err)
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("utraque/anthropic: decode body close: %w", err)
	}
	return o, nil
}

// decodeArray splits a raw JSON array into its elements' original bytes. It
// reports false for anything that is not an array, which is how string-valued
// message content is recognised without a second parse.
func decodeArray(raw []byte) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, false
	}
	return arr, true
}

// encodeArray re-emits elements as a JSON array by concatenation, so each
// element keeps the exact bytes it arrived with. json.Marshal would compact
// them, which is a rewrite this package must not perform.
func encodeArray(elems []json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, e := range elems {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(e)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func (o *orderedObject) encode() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, fmt.Errorf("utraque/anthropic: encode key %q: %w", k, err)
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
