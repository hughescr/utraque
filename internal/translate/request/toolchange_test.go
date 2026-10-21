package request_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		{Role: "system", Content: aschema.BlockContent(def("a", "first"), def("declared", "must not replace"))},
		{Role: "system", Content: aschema.BlockContent(def("a", "second"), def("b", "b"))},
		{Role: "system"}, // nil content must not panic
	}
	got, err := aschema.ResolveToolChanges(messages, []aschema.Tool{{Name: "declared"}})
	if err != nil {
		t.Fatalf("ResolveToolChanges: %v", err)
	}
	if len(got.Added) != 2 || got.Added[0].Name != "a" || got.Added[0].Description != "first" || got.Added[1].Name != "b" {
		t.Errorf("Added = %+v, want a(first) then b", got.Added)
	}
}

// TestMalformedToolChangeIsRefused pins the contract both legs share: a tool
// change the translator cannot honour is an error (a 400 at the leg), never a
// block silently dropped. The DeepSeek leg runs the same rules.
func TestMalformedToolChangeIsRefused(t *testing.T) {
	cases := map[string]string{
		"reference to an undeclared tool": `{"type":"tool_addition","tool":{"type":"tool_reference","name":"ghost"}}`,
		"nameless definition":             `{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"input_schema":{}}}}`,
		"missing definition":              `{"type":"tool_addition","tool":{"type":"tool_definition"}}`,
		"unknown target type":             `{"type":"tool_addition","tool":{"type":"tool_bundle","name":"x"}}`,
		"missing tool object":             `{"type":"tool_addition"}`,
		"removal by definition":           `{"type":"tool_removal","tool":{"type":"tool_definition","definition":{"name":"x"}}}`,
		"removal with no name":            `{"type":"tool_removal","tool":{"type":"tool_reference"}}`,
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			raw := `{"model":"m","max_tokens":8,"tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[` +
				`{"role":"user","content":"hi"},{"role":"system","content":[` + block + `]}]}`
			var req aschema.MessagesRequest
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				t.Fatal(err)
			}
			cfg := defaultCase()
			_, _, err := request.Translate(&req, cfg.dec, cfg.model, cfg.opts)
			if !errors.Is(err, request.ErrToolChange) {
				t.Fatalf("Translate error = %v, want request.ErrToolChange", err)
			}
		})
	}

	t.Run("a reference to a tool defined earlier inline is valid", func(t *testing.T) {
		raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[` +
			`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"late","input_schema":{"type":"object"}}}},` +
			`{"type":"tool_addition","tool":{"type":"tool_reference","name":"late"}}]}]}`
		var req aschema.MessagesRequest
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Fatal(err)
		}
		cfg := defaultCase()
		if _, _, err := request.Translate(&req, cfg.dec, cfg.model, cfg.opts); err != nil {
			t.Fatalf("Translate: %v", err)
		}
	})
}

// TestToolChangeTypeEncodingIsEquivalent: JSON may write any character of a
// block's type as an escape; the translation must not depend on how it did.
func TestToolChangeTypeEncodingIsEquivalent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(requestsDir, "tool_addition_midconversation.anthropic.input.json"))
	if err != nil {
		t.Fatal(err)
	}
	bs := string(rune(92)) // a backslash, spelled so no tool normalises the escape away
	escaped := strings.NewReplacer(`"tool_addition"`, `"tool`+bs+`u005faddition"`, `"tool_removal"`, `"`+bs+`u0074ool_removal"`).Replace(string(raw))
	if escaped == string(raw) {
		t.Fatal("fixture has no tool change blocks to escape")
	}
	translate := func(body string) []byte {
		var req aschema.MessagesRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		cfg := defaultCase()
		out, _, err := request.Translate(&req, cfg.dec, cfg.model, cfg.opts)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		b, _ := json.Marshal(out)
		return b
	}
	if a, b := translate(string(raw)), translate(escaped); string(a) != string(b) {
		t.Errorf("escaped and literal types translated differently:\n%s\n%s", a, b)
	}
}
