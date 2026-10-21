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
	// The fixture removes a tool nothing declares, which changes nothing.
	if len(meta.ToolsRemoved) != 0 {
		t.Errorf("ToolsRemoved = %v, want none", meta.ToolsRemoved)
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
	if len(got.Tools) != 3 || got.Tools[0].Name != "declared" || got.Tools[1].Name != "a" || got.Tools[1].Description != "first" || got.Tools[2].Name != "b" {
		t.Errorf("Tools = %+v, want declared, a(first), b", got.Tools)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(got.Hoisted, want) {
		t.Errorf("Hoisted = %v, want %v", got.Hoisted, want)
	}
}

// resolveNames resolves the given blocks (one per system message, in order)
// against a request that declares the named tools, and returns the active tool
// names, in order.
func resolveNames(t *testing.T, declared []string, blocks ...aschema.ContentBlock) ([]string, aschema.ToolChanges, error) {
	t.Helper()
	tools := make([]aschema.Tool, len(declared))
	for i, name := range declared {
		tools[i] = aschema.Tool{Name: name, Description: "declared " + name}
	}
	messages := make([]aschema.Message, len(blocks))
	for i, b := range blocks {
		messages[i] = aschema.Message{Role: "system", Content: aschema.BlockContent(b)}
	}
	got, err := aschema.ResolveToolChanges(messages, tools)
	var names []string
	for _, tool := range got.Tools {
		names = append(names, tool.Name)
	}
	return names, got, err
}

func removalOf(name string) aschema.ContentBlock {
	return aschema.ContentBlock{Type: aschema.BlockToolRemoval, Tool: &aschema.ToolTarget{Type: aschema.ToolTargetReference, Name: name}}
}

func referenceTo(name string) aschema.ContentBlock {
	return aschema.ContentBlock{Type: aschema.BlockToolAddition, Tool: &aschema.ToolTarget{Type: aschema.ToolTargetReference, Name: name}}
}

func definitionOf(name, desc string) aschema.ContentBlock {
	return aschema.ContentBlock{Type: aschema.BlockToolAddition, Tool: &aschema.ToolTarget{
		Type: aschema.ToolTargetDefinition, Definition: &aschema.Tool{Name: name, Description: desc},
	}}
}

// TestToolRemovalLeavesTheActiveSet pins the semantics both legs share: the
// advertised tools are those active after every change, applied in message
// order, and a removed tool stays known so it can come back.
func TestToolRemovalLeavesTheActiveSet(t *testing.T) {
	tests := []struct {
		name     string
		declared []string
		blocks   []aschema.ContentBlock
		want     []string
		removed  []string
		hoisted  []string
	}{
		{"removing a declared tool", []string{"a", "b"}, []aschema.ContentBlock{removalOf("a")}, []string{"b"}, []string{"a"}, nil},
		{"removing an inline tool", []string{"a"}, []aschema.ContentBlock{definitionOf("x", "x"), removalOf("x")}, []string{"a"}, []string{"x"}, nil},
		{"removing an unknown tool changes nothing", []string{"a"}, []aschema.ContentBlock{removalOf("ghost")}, []string{"a"}, nil, nil},
		{"removing twice is one removal", []string{"a", "b"}, []aschema.ContentBlock{removalOf("a"), removalOf("a")}, []string{"b"}, []string{"a"}, nil},
		{"removing everything", []string{"a"}, []aschema.ContentBlock{removalOf("a")}, nil, []string{"a"}, nil},
		{"re-added by reference to a declared definition", []string{"a", "b"}, []aschema.ContentBlock{removalOf("a"), referenceTo("a")}, []string{"a", "b"}, nil, nil},
		{"re-added by reference to an inline definition", []string{"a"},
			[]aschema.ContentBlock{definitionOf("x", "x"), removalOf("x"), referenceTo("x")}, []string{"a", "x"}, nil, []string{"x"}},
		{"re-added by a definition", []string{"a", "b"}, []aschema.ContentBlock{removalOf("a"), definitionOf("a", "new")}, []string{"a", "b"}, nil, nil},
		{"removed after being re-added", []string{"a", "b"},
			[]aschema.ContentBlock{removalOf("a"), referenceTo("a"), removalOf("a")}, []string{"b"}, []string{"a"}, nil},
		{"a re-added tool keeps its original position", []string{"a", "b", "c"},
			[]aschema.ContentBlock{removalOf("b"), definitionOf("x", "x"), referenceTo("b")}, []string{"a", "b", "c", "x"}, nil, []string{"x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changes, err := resolveNames(t, tc.declared, tc.blocks...)
			if err != nil {
				t.Fatalf("ResolveToolChanges: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("active tools = %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(changes.Removed, tc.removed) {
				t.Errorf("Removed = %v, want %v", changes.Removed, tc.removed)
			}
			if !reflect.DeepEqual(changes.Hoisted, tc.hoisted) {
				t.Errorf("Hoisted = %v, want %v", changes.Hoisted, tc.hoisted)
			}
		})
	}

	t.Run("a definition that re-adds a removed tool replaces the old one", func(t *testing.T) {
		_, changes, err := resolveNames(t, []string{"a"}, removalOf("a"), definitionOf("a", "latest"))
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Tools) != 1 || changes.Tools[0].Description != "latest" {
			t.Errorf("Tools = %+v, want a(latest)", changes.Tools)
		}
	})
	t.Run("a definition of an active tool does not replace it", func(t *testing.T) {
		_, changes, err := resolveNames(t, []string{"a"}, definitionOf("a", "ignored"))
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Tools) != 1 || changes.Tools[0].Description != "declared a" {
			t.Errorf("Tools = %+v, want a(declared a)", changes.Tools)
		}
	})
	t.Run("a reference to a removed tool is valid, one to a never-declared tool is not", func(t *testing.T) {
		if _, _, err := resolveNames(t, []string{"a"}, removalOf("a"), referenceTo("a")); err != nil {
			t.Errorf("reference to a removed tool: %v", err)
		}
		if _, _, err := resolveNames(t, []string{"a"}, removalOf("ghost"), referenceTo("ghost")); err == nil {
			t.Error("reference to a tool that was only ever removed, never declared, was accepted")
		}
	})
	t.Run("a name declared twice in the tools array is removed as one", func(t *testing.T) {
		got, _, err := resolveNames(t, []string{"a", "b", "a"}, removalOf("a"))
		if err != nil || !reflect.DeepEqual(got, []string{"b"}) {
			t.Errorf("active = %v, err %v, want [b]", got, err)
		}
	})
}

// TestRemovedToolIsOutOfTheCodexTools drives a removal through Translate: the
// tool leaves the Responses tools array, its earlier tool_use and tool_result
// stay in the input, and a tool re-added afterwards is back.
func TestRemovedToolIsOutOfTheCodexTools(t *testing.T) {
	const history = `{"role":"user","content":"look"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Grep","input":{"q":"x"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"found"}]},`
	removeGrep := `{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"Grep"}}]},`
	readdGrep := `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"Grep"}}]},`
	last := `{"role":"user","content":"go"}`
	request2 := func(system string) string {
		return `{"model":"m","max_tokens":8,"tool_choice":{"type":"auto"},"tools":[` +
			`{"name":"Read","input_schema":{"type":"object"}},{"name":"Grep","input_schema":{"type":"object"}}],` +
			`"messages":[` + history + system + last + `]}`
	}
	translate := func(body string) ([]string, string, request.Metadata) {
		var req aschema.MessagesRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
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
		encoded, _ := json.Marshal(out.Input)
		return names, string(encoded), meta
	}

	names, input, meta := translate(request2(removeGrep))
	if !reflect.DeepEqual(names, []string{"Read"}) {
		t.Errorf("tools after removal = %v, want [Read]", names)
	}
	if !reflect.DeepEqual(meta.ToolsRemoved, []string{"Grep"}) {
		t.Errorf("ToolsRemoved = %v, want [Grep]", meta.ToolsRemoved)
	}
	if !strings.Contains(input, `"Grep"`) || !strings.Contains(input, "found") {
		t.Errorf("history lost the call to the removed tool or its result: %s", input)
	}

	names, _, meta = translate(request2(removeGrep + readdGrep))
	if !reflect.DeepEqual(names, []string{"Read", "Grep"}) || len(meta.ToolsRemoved) != 0 {
		t.Errorf("tools after removal and re-add = %v (removed %v), want [Read Grep]", names, meta.ToolsRemoved)
	}

	// Every tool removed: no tools, and no tool_choice left pointing at nothing.
	var req aschema.MessagesRequest
	body := `{"model":"m","max_tokens":8,"tool_choice":{"type":"auto"},"tools":[{"name":"Read","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"Read"}}]}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	cfg := defaultCase()
	out, _, err := request.Translate(&req, cfg.dec, cfg.model, cfg.opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 0 || out.ToolChoice != nil {
		t.Errorf("after removing every tool: tools %v, tool_choice %v, want neither", out.Tools, out.ToolChoice)
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
