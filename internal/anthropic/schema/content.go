// Package aschema holds the Anthropic Messages API wire types. It imports
// nothing beyond the standard library so every translator can share it
// without an import cycle.
package aschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	// RoleSystem appears on messages inside messages[] (not the top-level
	// system field) when the client negotiates the
	// mid-conversation-system beta.
	RoleSystem = "system"
)

// Content block types.
const (
	BlockText             = "text"
	BlockImage            = "image"
	BlockDocument         = "document"
	BlockToolUse          = "tool_use"
	BlockToolResult       = "tool_result"
	BlockThinking         = "thinking"
	BlockRedactedThinking = "redacted_thinking"
	// BlockToolAddition and BlockToolRemoval are the mid-conversation tool
	// change blocks (betas mid-conversation-tool-changes-2026-07-01 and
	// inline-tools-2026-09-15). They ride in a role "system" message and carry
	// a Tool target; see ToolTarget.
	BlockToolAddition = "tool_addition"
	BlockToolRemoval  = "tool_removal"
)

// Tool target types: what a tool_addition or tool_removal block points at.
const (
	// ToolTargetReference names a tool by name. For an addition the tool is
	// already declared in the top-level tools array (typically with
	// defer_loading) and is merely announced as available from here on.
	ToolTargetReference = "tool_reference"
	// ToolTargetDefinition carries the whole tool inline, so the tool exists
	// only because of this block.
	ToolTargetDefinition = "tool_definition"
)

// ToolTarget is the "tool" object of a tool_addition or tool_removal block:
// {"type":"tool_reference","name":"..."} or
// {"type":"tool_definition","definition":{"name":"...","input_schema":{...}}}.
type ToolTarget struct {
	Type       string `json:"type"`
	Name       string `json:"name,omitempty"`
	Definition *Tool  `json:"definition,omitempty"`
}

// CheckToolChange validates one tool_addition or tool_removal block of the
// given kind against its target. It is the single rule set both the Codex and
// the DeepSeek legs apply, so a malformed block is refused the same way on
// either: silently losing a tool is worse than a 400. declared reports whether
// a tool name is known at that point (in the request's tools array or by an
// earlier inline definition), including a tool an earlier block removed, so a
// reference addition can bring it back. The rules:
//
//   - the block needs a tool object;
//   - a removal must be a tool_reference with a name;
//   - an addition must be a tool_definition whose definition has a name, or a
//     tool_reference naming a declared tool;
//   - any other target type is unknown.
func CheckToolChange(kind string, t *ToolTarget, declared func(name string) bool) error {
	if t == nil {
		return errors.New("has no tool object")
	}
	switch {
	case kind == BlockToolRemoval && t.Type == ToolTargetReference:
		if strings.TrimSpace(t.Name) == "" {
			return errors.New("removes a tool with no name")
		}
	case kind == BlockToolAddition && t.Type == ToolTargetDefinition:
		if t.Definition == nil || strings.TrimSpace(t.Definition.Name) == "" {
			return errors.New("defines a tool with no name")
		}
	case kind == BlockToolAddition && t.Type == ToolTargetReference:
		if !declared(t.Name) {
			return fmt.Errorf("adds tool %q, which nothing declares", t.Name)
		}
	default:
		return fmt.Errorf("%s has an unknown tool target type %q", kind, t.Type)
	}
	return nil
}

// ToolSet tracks which tools a conversation has declared and which of them are
// active, as tool_addition and tool_removal blocks arrive in message order. The
// two are kept apart on purpose: a removal makes a tool inactive but it stays
// known, so a later tool_reference addition (or a fresh definition) makes it
// active again. Both legs drive the same ToolSet, so they agree on the final
// set of tools to advertise upstream.
//
// A tool occupies a slot; slots follow declaration order: first the request's
// tools array, then each inline definition in the order the conversation
// introduced it. The caller keeps its own per-slot definitions (the Codex leg
// typed, the DeepSeek leg raw JSON, so each keeps what it needs) and asks
// Apply what to do with them. A tool's name, not its slot, carries the active
// flag, so a name that appears twice in the tools array is removed as one.
type ToolSet struct {
	names    []string       // one per slot
	first    map[string]int // name -> its first slot
	removed  map[string]struct{}
	declared int // slots that came from the request's tools array
}

// NewToolSet starts a ToolSet from the names in the request's tools array, one
// slot each, every tool active. A nameless entry takes a slot but is never
// known, so no block can refer to it.
func NewToolSet(declared []string) *ToolSet {
	s := &ToolSet{
		names:    append([]string(nil), declared...),
		first:    make(map[string]int, len(declared)),
		removed:  map[string]struct{}{},
		declared: len(declared),
	}
	for i, name := range declared {
		if _, dup := s.first[name]; !dup && name != "" {
			s.first[name] = i
		}
	}
	return s
}

// Len is the number of slots.
func (s *ToolSet) Len() int { return len(s.names) }

// Known reports whether a tool of that name has been declared, active or not.
func (s *ToolSet) Known(name string) bool { _, ok := s.first[name]; return ok }

// Active reports whether the tool in slot is currently advertised.
func (s *ToolSet) Active(slot int) bool {
	_, gone := s.removed[s.names[slot]]
	return !gone
}

// ToolEdit is what Apply decided about one block.
type ToolEdit struct {
	// Slot is the slot of the tool the block names, or -1 for a removal of a
	// tool that was never declared (which changes nothing).
	Slot int
	// Store tells the caller to keep the block's definition in Slot: append it
	// when Slot equals the Len the caller saw before the call, else replace what
	// the slot held.
	Store bool
}

// Apply checks one tool_addition or tool_removal block with CheckToolChange and
// applies it. The rules:
//
//   - a removal makes the named tool inactive; the definition stays known;
//   - a reference addition makes the named known tool active again (and adds
//     nothing new: the tool was declared by the request or an earlier block);
//   - a definition addition of an unknown name declares a new slot;
//   - a definition addition of an active, known name is ignored, so the first
//     declaration wins and the request's own tools are never contradicted;
//   - a definition addition of a removed name makes it active with the new
//     definition, which is the client's latest word on the tool.
func (s *ToolSet) Apply(kind string, t *ToolTarget) (ToolEdit, error) {
	if err := CheckToolChange(kind, t, s.Known); err != nil {
		return ToolEdit{}, err
	}
	if kind == BlockToolRemoval {
		slot, known := s.first[t.Name]
		if !known {
			return ToolEdit{Slot: -1}, nil
		}
		s.removed[t.Name] = struct{}{}
		return ToolEdit{Slot: slot}, nil
	}
	if t.Type == ToolTargetReference {
		delete(s.removed, t.Name)
		return ToolEdit{Slot: s.first[t.Name]}, nil
	}
	name := t.Definition.Name
	slot, known := s.first[name]
	switch _, gone := s.removed[name]; {
	case !known:
		s.names = append(s.names, name)
		s.first[name] = len(s.names) - 1
		return ToolEdit{Slot: len(s.names) - 1, Store: true}, nil
	case gone:
		delete(s.removed, name)
		return ToolEdit{Slot: slot, Store: true}, nil
	default:
		return ToolEdit{Slot: slot}, nil
	}
}

// Removed names the known tools that are inactive now, in slot order.
func (s *ToolSet) Removed() []string {
	var out []string
	seen := map[string]struct{}{}
	for _, name := range s.names {
		if _, gone := s.removed[name]; !gone {
			continue
		}
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// Hoisted names the active tools that blocks defined inline (those past the
// request's tools array), in slot order.
func (s *ToolSet) Hoisted() []string {
	var out []string
	for slot := s.declared; slot < len(s.names); slot++ {
		if s.Active(slot) {
			out = append(out, s.names[slot])
		}
	}
	return out
}

// ToolChanges is what a conversation's tool_addition and tool_removal blocks
// amount to.
type ToolChanges struct {
	// Tools is the final active tool set to advertise upstream: the request's
	// tools followed by the tools blocks defined inline, in conversation order,
	// minus the tools removed and not re-added.
	Tools []Tool
	// Hoisted names the inline-defined tools that are in Tools.
	Hoisted []string
	// Removed names the declared tools left out of Tools by a removal.
	Removed []string
}

// ResolveToolChanges validates every tool_addition and tool_removal block in
// messages in order, applying them to a ToolSet seeded from declared. The error
// names the offending block as messages[i].content[j]. History is untouched:
// this only decides which tools are advertised, so earlier tool_use and
// tool_result blocks for a removed tool stay as they are.
func ResolveToolChanges(messages []Message, declared []Tool) (ToolChanges, error) {
	names := make([]string, len(declared))
	for i := range declared {
		names[i] = declared[i].Name
	}
	set := NewToolSet(names)
	defs := append([]Tool(nil), declared...)

	for i := range messages {
		if messages[i].Content == nil {
			continue
		}
		for j, blk := range messages[i].Content.Blocks {
			if blk.Type != BlockToolAddition && blk.Type != BlockToolRemoval {
				continue
			}
			before := set.Len()
			edit, err := set.Apply(blk.Type, blk.Tool)
			if err != nil {
				return ToolChanges{}, fmt.Errorf("messages[%d].content[%d] %w", i, j, err)
			}
			if !edit.Store {
				continue
			}
			if edit.Slot == before {
				defs = append(defs, *blk.Tool.Definition)
			} else {
				defs[edit.Slot] = *blk.Tool.Definition
			}
		}
	}

	changes := ToolChanges{Hoisted: set.Hoisted(), Removed: set.Removed()}
	changes.Tools = make([]Tool, 0, len(defs))
	for slot := range defs {
		if set.Active(slot) {
			changes.Tools = append(changes.Tools, defs[slot])
		}
	}
	return changes, nil
}

// Stop reasons.
const (
	StopEndTurn      = "end_turn"
	StopMaxTokens    = "max_tokens"
	StopStopSequence = "stop_sequence"
	StopToolUse      = "tool_use"
	StopRefusal      = "refusal"
)

// CacheControl marks a block as a prompt-cache breakpoint.
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// Source is an image or document payload.
type Source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ContentBlock is the flat, tagged form of every Anthropic content block.
// Type discriminates; only the fields belonging to that type are populated.
type ContentBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	Source *Source `json:"source,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string   `json:"tool_use_id,omitempty"`
	Content   *Content `json:"content,omitempty"`
	IsError   bool     `json:"is_error,omitempty"`

	// Tool is the target of a tool_addition or tool_removal block.
	Tool *ToolTarget `json:"tool,omitempty"`

	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// TextBlock builds a text block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockText, Text: text}
}

// ToolUseBlock builds a tool_use block.
func ToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: BlockToolUse, ID: id, Name: name, Input: input}
}

// ThinkingBlock builds a thinking block.
func ThinkingBlock(thinking, signature string) ContentBlock {
	return ContentBlock{Type: BlockThinking, Thinking: thinking, Signature: signature}
}

// Content is Anthropic's string-or-array union. IsString records the wire form
// so MarshalJSON round-trips whatever came in.
type Content struct {
	Blocks   []ContentBlock
	IsString bool
}

// StringContent builds the scalar-string form.
func StringContent(s string) *Content {
	return &Content{Blocks: []ContentBlock{TextBlock(s)}, IsString: true}
}

// BlockContent builds the array form.
func BlockContent(blocks ...ContentBlock) *Content {
	return &Content{Blocks: blocks}
}

// UnmarshalJSON accepts either a bare string or an array of blocks.
func (c *Content) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.Blocks, c.IsString = nil, false
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		c.Blocks, c.IsString = []ContentBlock{TextBlock(s)}, true
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return err
	}
	c.Blocks, c.IsString = blocks, false
	return nil
}

// MarshalJSON emits the same wire form the value was parsed from.
func (c Content) MarshalJSON() ([]byte, error) {
	if c.IsString {
		return json.Marshal(c.Text())
	}
	if c.Blocks == nil {
		return json.Marshal([]ContentBlock{})
	}
	return json.Marshal(c.Blocks)
}

// Text concatenates every text block. A nil Content yields "".
func (c *Content) Text() string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range c.Blocks {
		if blk.Type == BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// IsEmpty reports whether there is no content at all.
func (c *Content) IsEmpty() bool { return c == nil || len(c.Blocks) == 0 }
