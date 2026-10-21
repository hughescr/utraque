package deepseek

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/anthropic/schema"
)

type rewritten struct {
	Tools []struct {
		Name         string          `json:"name"`
		DeferLoading *bool           `json:"defer_loading"`
		InputSchema  json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (r rewritten) toolNames() []string {
	var names []string
	for _, tool := range r.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func rewriteBody(t *testing.T, body string) (rewritten, rewriteReport, string) {
	t.Helper()
	out, report, err := rewriteRequest([]byte(body), "deepseek-flash", "")
	if err != nil {
		t.Fatalf("rewriteRequest: %v", err)
	}
	var got rewritten
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten body: %v\n%s", err, out)
	}
	return got, report, string(out)
}

// TestToolAdditionFixtureIsHoisted runs the realistic Claude Code request (the
// shape of aNs's output under the mid-conversation-tool-changes and
// inline-tools betas) through the DeepSeek rewrite and checks that no
// tool_addition or tool_removal block reaches DeepSeek while every tool the
// conversation defined is declared, in order.
func TestToolAdditionFixtureIsHoisted(t *testing.T) {
	raw, err := os.ReadFile("testdata/tool_addition_request.json")
	if err != nil {
		t.Fatal(err)
	}
	got, report, out := rewriteBody(t, string(raw))

	for _, banned := range []string{"tool_addition", "tool_removal", "tool_definition", "defer_loading"} {
		if strings.Contains(out, banned) {
			t.Errorf("%q reached DeepSeek:\n%s", banned, out)
		}
	}
	want := []string{"Read", "mcp__notes__search_notes", "mcp__tickets__create_ticket", "mcp__tickets__comment"}
	if !reflect.DeepEqual(got.toolNames(), want) {
		t.Errorf("tools = %v, want %v", got.toolNames(), want)
	}
	if want := []string{"mcp__tickets__create_ticket", "mcp__tickets__comment"}; !reflect.DeepEqual(report.InlineToolsHoisted, want) {
		t.Errorf("InlineToolsHoisted = %v, want %v", report.InlineToolsHoisted, want)
	}
	// The fixture removes a tool nothing declares, which changes nothing.
	if len(report.ToolsRemoved) != 0 {
		t.Errorf("ToolsRemoved = %v, want none", report.ToolsRemoved)
	}
	// The hoisted definition arrives whole, schema included.
	if !strings.Contains(string(got.Tools[2].InputSchema), `"urgent"`) {
		t.Errorf("hoisted tool lost its schema: %s", got.Tools[2].InputSchema)
	}

	// Fixture messages: user, assistant, user(tool_result), system(tools only),
	// system(tool + text), user. The tools-only system message disappears and
	// the other keeps just its prose.
	if len(got.Messages) != 5 {
		t.Fatalf("messages = %d, want 5 (the tools-only system message dropped)", len(got.Messages))
	}
	system := got.Messages[3]
	if system.Role != "system" || !strings.Contains(string(system.Content), "tickets server just connected") ||
		strings.Contains(string(system.Content), "tool_") {
		t.Errorf("surviving system message = %s %s", system.Role, system.Content)
	}
}

func TestToolChangeEdgeCases(t *testing.T) {
	const head = `{"model":"deepseek-flash","max_tokens":8,`
	def := func(name string) string {
		return `{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"` + name + `","description":"d","input_schema":{"type":"object","properties":{}}}}}`
	}
	system := func(blocks ...string) string {
		return `{"role":"system","content":[` + strings.Join(blocks, ",") + `]}`
	}
	user := `{"role":"user","content":"hi"}`

	t.Run("a request with no declared tools gains a tools array", func(t *testing.T) {
		got, _, _ := rewriteBody(t, head+`"messages":[`+user+`,`+system(def("late"))+`]}`)
		if !reflect.DeepEqual(got.toolNames(), []string{"late"}) {
			t.Errorf("tools = %v, want [late]", got.toolNames())
		}
	})
	t.Run("a name already declared keeps its first declaration", func(t *testing.T) {
		got, report, _ := rewriteBody(t, head+`"tools":[{"name":"dup","description":"top-level","input_schema":{"type":"object"}}],`+
			`"messages":[`+user+`,`+system(def("dup"), def("new"), def("new"))+`]}`)
		if !reflect.DeepEqual(got.toolNames(), []string{"dup", "new"}) {
			t.Errorf("tools = %v, want [dup new]", got.toolNames())
		}
		if !reflect.DeepEqual(report.InlineToolsHoisted, []string{"new"}) {
			t.Errorf("hoisted = %v, want [new]", report.InlineToolsHoisted)
		}
	})
	t.Run("an inline definition's defer_loading is dropped", func(t *testing.T) {
		_, _, out := rewriteBody(t, head+`"messages":[`+user+`,`+system(
			`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"x","defer_loading":true,"input_schema":{"type":"object"}}}}`)+`]}`)
		if strings.Contains(out, "defer_loading") {
			t.Errorf("defer_loading reached DeepSeek: %s", out)
		}
	})
	t.Run("a tool_reference to a tool defined inline is accepted", func(t *testing.T) {
		got, _, _ := rewriteBody(t, head+`"messages":[`+user+`,`+system(def("late"))+`,`+
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{}}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"late"}]}]}]}`)
		if !reflect.DeepEqual(got.toolNames(), []string{"late"}) {
			t.Errorf("tools = %v", got.toolNames())
		}
	})
	t.Run("a removal takes the tool out of the tools array", func(t *testing.T) {
		got, report, _ := rewriteBody(t, head+`"tools":[{"name":"keep","input_schema":{"type":"object"}},{"name":"gone","input_schema":{"type":"object"}}],"messages":[`+user+`,`+
			system(`{"type":"tool_removal","tool":{"type":"tool_reference","name":"gone"}}`)+`]}`)
		if !reflect.DeepEqual(got.toolNames(), []string{"keep"}) || len(got.Messages) != 1 || !reflect.DeepEqual(report.ToolsRemoved, []string{"gone"}) {
			t.Errorf("tools %v, messages %d, removed %v", got.toolNames(), len(got.Messages), report.ToolsRemoved)
		}
	})
	t.Run("a removal of a tool nothing declares changes nothing", func(t *testing.T) {
		got, report, _ := rewriteBody(t, head+`"tools":[{"name":"keep","input_schema":{"type":"object"}}],"messages":[`+user+`,`+
			system(`{"type":"tool_removal","tool":{"type":"tool_reference","name":"ghost"}}`)+`]}`)
		if !reflect.DeepEqual(got.toolNames(), []string{"keep"}) || len(report.ToolsRemoved) != 0 {
			t.Errorf("tools %v, removed %v", got.toolNames(), report.ToolsRemoved)
		}
	})
	t.Run("removing every tool drops tools and tool_choice", func(t *testing.T) {
		_, report, out := rewriteBody(t, head+`"tool_choice":{"type":"auto"},"tools":[{"name":"only","input_schema":{"type":"object"}}],"messages":[`+user+`,`+
			system(`{"type":"tool_removal","tool":{"type":"tool_reference","name":"only"}}`)+`]}`)
		if strings.Contains(out, `"tools"`) || strings.Contains(out, "tool_choice") || !reflect.DeepEqual(report.ToolsRemoved, []string{"only"}) {
			t.Errorf("removed %v, body %s", report.ToolsRemoved, out)
		}
	})
	t.Run("a tool removed and re-added by reference is declared, in its place, and not deferred", func(t *testing.T) {
		got, report, _ := rewriteBody(t, head+`"tools":[{"name":"a","input_schema":{"type":"object"}},{"name":"b","defer_loading":true,"input_schema":{"type":"object"}},{"name":"c","input_schema":{"type":"object"}}],"messages":[`+user+`,`+
			system(`{"type":"tool_removal","tool":{"type":"tool_reference","name":"b"}}`)+`,`+
			system(def("late"), `{"type":"tool_addition","tool":{"type":"tool_reference","name":"b"}}`)+`]}`)
		if !reflect.DeepEqual(got.toolNames(), []string{"a", "b", "c", "late"}) || len(report.ToolsRemoved) != 0 {
			t.Errorf("tools %v, removed %v", got.toolNames(), report.ToolsRemoved)
		}
		for _, tool := range got.Tools {
			if tool.DeferLoading != nil {
				t.Errorf("%s is still deferred after being re-added", tool.Name)
			}
		}
	})
	t.Run("a tool removed and re-added by definition takes the new definition", func(t *testing.T) {
		got, _, out := rewriteBody(t, head+`"tools":[{"name":"a","description":"old","input_schema":{"type":"object"}}],"messages":[`+user+`,`+
			system(`{"type":"tool_removal","tool":{"type":"tool_reference","name":"a"}}`, def("a"))+`]}`)
		if !reflect.DeepEqual(got.toolNames(), []string{"a"}) || strings.Contains(out, `"old"`) {
			t.Errorf("tools %v, body %s", got.toolNames(), out)
		}
	})
	t.Run("a removed inline tool is not hoisted", func(t *testing.T) {
		got, report, _ := rewriteBody(t, head+`"messages":[`+user+`,`+
			system(def("x"), `{"type":"tool_removal","tool":{"type":"tool_reference","name":"x"}}`)+`]}`)
		if len(got.toolNames()) != 0 || len(report.InlineToolsHoisted) != 0 || !reflect.DeepEqual(report.ToolsRemoved, []string{"x"}) {
			t.Errorf("tools %v, hoisted %v, removed %v", got.toolNames(), report.InlineToolsHoisted, report.ToolsRemoved)
		}
	})
	t.Run("history keeps a call to a removed tool and a tool_reference to it", func(t *testing.T) {
		got, _, out := rewriteBody(t, head+`"tools":[{"name":"ToolSearch","input_schema":{"type":"object"}},{"name":"found","defer_loading":true,"input_schema":{"type":"object"}}],"messages":[`+user+`,`+
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{}}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"found"}]}]},`+
			`{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"found","input":{}}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"done"}]},`+
			system(`{"type":"tool_removal","tool":{"type":"tool_reference","name":"found"}}`)+`]}`)
		if !reflect.DeepEqual(got.toolNames(), []string{"ToolSearch"}) {
			t.Errorf("tools = %v, want [ToolSearch]", got.toolNames())
		}
		if !strings.Contains(out, `"name":"found"`) || !strings.Contains(out, "done") || !strings.Contains(out, "tool available") {
			t.Errorf("history lost the removed tool's call, result or reference: %s", out)
		}
	})

	errCases := map[string]string{
		"reference to an undeclared tool": system(`{"type":"tool_addition","tool":{"type":"tool_reference","name":"ghost"}}`),
		"nameless definition":             system(`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"input_schema":{}}}}`),
		"unknown target type":             system(`{"type":"tool_addition","tool":{"type":"tool_bundle","name":"x"}}`),
		"missing tool object":             system(`{"type":"tool_addition"}`),
		"removal by definition":           system(`{"type":"tool_removal","tool":{"type":"tool_definition","definition":{"name":"x"}}}`),
		"removal with no name":            system(`{"type":"tool_removal","tool":{"type":"tool_reference"}}`),
		"definition with no definition":   system(`{"type":"tool_addition","tool":{"type":"tool_definition"}}`),
	}
	for name, message := range errCases {
		t.Run("rejects "+name, func(t *testing.T) {
			_, _, err := rewriteRequest([]byte(head+`"messages":[`+user+`,`+message+`]}`), "deepseek-flash", "")
			if err == nil {
				t.Fatal("accepted a tool change DeepSeek cannot be given faithfully")
			}
		})
	}
}

// TestToolChangeTypeEncodingIsEquivalent: JSON lets any character of a block's
// type be written as an escape, so a valid tool_addition spelled that way must
// be hoisted exactly like the literal spelling (it once skipped the hoist and
// hit the unknown-block 400), on both /v1/messages and count_tokens.
func TestToolChangeTypeEncodingIsEquivalent(t *testing.T) {
	raw, err := os.ReadFile("testdata/tool_addition_request.json")
	if err != nil {
		t.Fatal(err)
	}
	literal := string(raw)
	bs := string(rune(92)) // a backslash, spelled so no tool normalises the escape away
	escaped := strings.NewReplacer(`"tool_addition"`, `"tool`+bs+`u005faddition"`, `"tool_removal"`, `"`+bs+`u0074ool_removal"`).Replace(literal)
	if escaped == literal {
		t.Fatal("fixture has no tool change blocks to escape")
	}

	// The rewrite itself, byte for byte.
	a, ra, err := rewriteRequest([]byte(literal), "deepseek-flash", "")
	if err != nil {
		t.Fatal(err)
	}
	b, rb, err := rewriteRequest([]byte(escaped), "deepseek-flash", "")
	if err != nil {
		t.Fatalf("escaped spelling refused: %v", err)
	}
	if string(a) != string(b) || !reflect.DeepEqual(ra.InlineToolsHoisted, rb.InlineToolsHoisted) {
		t.Errorf("escaped spelling rewrote differently:\n%s\n%s", a, b)
	}

	// What the leg sends upstream, and what count_tokens answers. The bodies
	// are edited as text so the escapes under test survive.
	var sent []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	l := testLeg(t, upstream.URL+"/anthropic")
	for _, body := range []string{literal, escaped} {
		body = strings.Replace(body, `"stream": true`, `"stream": false`, 1)
		if _, err := callLeg(t, l, "deepseek-flash", body, false, false, nil); err != nil {
			t.Fatalf("Messages: %v", err)
		}
	}
	if len(sent) != 2 || sent[0] != sent[1] {
		t.Errorf("upstream bodies differ between spellings:\n%v", sent)
	}
	count := func(body string) string {
		body = strings.NewReplacer(`"max_tokens": 8192,`, ``, `"stream": true,`, ``).Replace(body)
		rec, err := callLeg(t, l, "deepseek-flash", body, false, true, nil)
		if err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
		return rec.Body.String()
	}
	if x, y := count(literal), count(escaped); x != y {
		t.Errorf("count_tokens differs between spellings: %s vs %s", x, y)
	}
}

// TestMessagesHoistsToolAdditionUpstream drives the whole leg and checks the
// bytes DeepSeek receives and that count_tokens counts the hoisted tool.
func TestMessagesHoistsToolAdditionUpstream(t *testing.T) {
	raw, err := os.ReadFile("testdata/tool_addition_request.json")
	if err != nil {
		t.Fatal(err)
	}
	var sent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	l := testLeg(t, upstream.URL+"/anthropic")

	// stream:false so the plain JSON reply path is exercised.
	body := strings.Replace(string(raw), `"stream": true`, `"stream": false`, 1)
	if _, err := callLeg(t, l, "deepseek-flash", body, false, false, nil); err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if strings.Contains(sent, "tool_addition") || !strings.Contains(sent, "mcp__tickets__create_ticket") {
		t.Errorf("upstream body did not carry the hoisted tool in place of the block:\n%s", sent)
	}

	// count_tokens: the estimate must grow when the hoisted definitions are
	// counted, so compare against the same request without the added tools.
	count := func(body string) int {
		rec, err := callLeg(t, l, "deepseek-flash", body, false, true, nil)
		if err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
		var resp aschema.CountTokensResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.InputTokens
	}
	countBody := func(s string) string {
		var obj map[string]json.RawMessage
		_ = json.Unmarshal([]byte(s), &obj)
		delete(obj, "max_tokens")
		delete(obj, "stream")
		out, _ := json.Marshal(obj)
		return string(out)
	}
	withAdditions := count(countBody(string(raw)))
	// The same request with the definition blocks reduced to a plain text block.
	stripped := strings.NewReplacer(
		`"tool_definition"`, `"tool_reference", "name": "mcp__notes__search_notes"`,
	).Replace(string(raw))
	if withAdditions <= count(countBody(stripped)) {
		t.Errorf("count_tokens did not count the hoisted tool definitions")
	}
}
