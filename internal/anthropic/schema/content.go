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
// a tool name is declared at that point (in the request's tools array or by an
// earlier inline definition). The rules:
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

// ToolChanges is what a conversation's tool_addition and tool_removal blocks
// amount to.
type ToolChanges struct {
	// Added are the tools defined inline, in conversation order. A name that
	// the request declares, or that an earlier block added, is skipped, so the
	// first declaration wins and the top-level tools array is never
	// contradicted.
	Added []Tool
	// Removals counts the tool_removal blocks.
	Removals int
}

// ResolveToolChanges validates every tool_addition and tool_removal block in
// messages with CheckToolChange and returns what they add and remove. The
// error names the offending block as messages[i].content[j]. References add
// nothing: the tool they name is declared already.
func ResolveToolChanges(messages []Message, declared []Tool) (ToolChanges, error) {
	seen := make(map[string]struct{}, len(declared))
	for i := range declared {
		seen[declared[i].Name] = struct{}{}
	}
	isDeclared := func(name string) bool { _, ok := seen[name]; return ok }

	var changes ToolChanges
	for i := range messages {
		if messages[i].Content == nil {
			continue
		}
		for j, blk := range messages[i].Content.Blocks {
			if blk.Type != BlockToolAddition && blk.Type != BlockToolRemoval {
				continue
			}
			if err := CheckToolChange(blk.Type, blk.Tool, isDeclared); err != nil {
				return ToolChanges{}, fmt.Errorf("messages[%d].content[%d] %w", i, j, err)
			}
			switch {
			case blk.Type == BlockToolRemoval:
				changes.Removals++
			case blk.Tool.Type == ToolTargetDefinition && !isDeclared(blk.Tool.Definition.Name):
				seen[blk.Tool.Definition.Name] = struct{}{}
				changes.Added = append(changes.Added, *blk.Tool.Definition)
			}
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
