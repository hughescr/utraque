package request_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	aschema "github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/translate/request"
)

// TestToolAdditionFromRealShape drives the realistic fixture (the block shape
// Claude Code's aNs builder emits under the mid-conversation-tool-changes and
// inline-tools betas) through Translate and checks what the model is told: the
// inline definitions join the tools array after the declared ones, in
// conversation order; reference additions and removals add nothing; the
// text-bearing system message still becomes a developer item and the
// tool-only one becomes nothing.
func TestToolAdditionFromRealShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(requestsDir, "tool_addition_midconversation.anthropic.input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var req aschema.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	cfg := defaultCase()
	out, meta, err := request.Translate(&req, cfg.dec, cfg.model, cfg.opts)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var names []string
	for _, tool := range out.Tools {
		names = append(names, tool.Name)
	}
	want := []string{"Read", "mcp__notes__search_notes", "mcp__tickets__create_ticket", "mcp__tickets__comment"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
	if want := []string{"mcp__tickets__create_ticket", "mcp__tickets__comment"}; !reflect.DeepEqual(meta.InlineToolsHoisted, want) {
		t.Errorf("InlineToolsHoisted = %v, want %v", meta.InlineToolsHoisted, want)
	}
	if meta.ToolRemovalsIgnored != 1 {
		t.Errorf("ToolRemovalsIgnored = %d, want 1", meta.ToolRemovalsIgnored)
	}
	// Only the second system message has prose, so exactly one developer item.
	developer := 0
	for _, item := range out.Input {
		if item.Role == "developer" {
			developer++
		}
	}
	if developer != 1 || meta.SystemMessagesRemapped != 1 {
		t.Errorf("developer items = %d, SystemMessagesRemapped = %d, want 1 and 1", developer, meta.SystemMessagesRemapped)
	}
}

func TestAddedToolsFirstDeclarationWins(t *testing.T) {
	def := func(name, desc string) aschema.ContentBlock {
		return aschema.ContentBlock{
			Type: aschema.BlockToolAddition,
			Tool: &aschema.ToolTarget{
				Type:       aschema.ToolTargetDefinition,
				Definition: &aschema.Tool{Name: name, Description: desc},
			},
		}
	}
	messages := []aschema.Message{
		{Role: "system", Content: aschema.BlockContent(def("a", "first"), def("declared", "must not replace"), def("", "nameless"))},
		{Role: "system", Content: aschema.BlockContent(def("a", "second"), def("b", "b"))},
		{Role: "system"}, // nil content must not panic
	}
	got := aschema.AddedTools(messages, []aschema.Tool{{Name: "declared"}})
	if len(got) != 2 || got[0].Name != "a" || got[0].Description != "first" || got[1].Name != "b" {
		t.Errorf("AddedTools = %+v, want a(first) then b", got)
	}
}
