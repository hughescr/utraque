// Package schema holds the Anthropic Messages API wire types. It imports
// nothing beyond the standard library so every translator can share it
// without an import cycle.
package schema

import (
	"bytes"
	"encoding/json"
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
)

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
