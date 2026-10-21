package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/config"
)

const toolAdditionFixture = "../../testdata/requests/tool_addition_midconversation.anthropic.input.json"

func toolNames(t *testing.T, body string) []string {
	t.Helper()
	var sent struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	var names []string
	for _, tool := range sent.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// TestToolAdditionThroughBothLegs sends the same Claude Code request, with
// tool_addition and tool_removal blocks, to the Codex and DeepSeek legs and
// checks both upstreams get every tool the conversation defined and neither
// gets a block it cannot read.
func TestToolAdditionThroughBothLegs(t *testing.T) {
	restoreRegistry(t)
	raw, err := os.ReadFile(toolAdditionFixture)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Read", "mcp__notes__search_notes", "mcp__tickets__create_ticket", "mcp__tickets__comment"}

	deepseekBody := make(chan string, 1)
	deepseek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		deepseekBody <- string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_ds","type":"message","role":"assistant","model":"deepseek-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(deepseek.Close)
	env := newCodexEnv(t, func(cfg *config.Config) {
		cfg.DeepSeek.BaseURL = deepseek.URL
		cfg.DeepSeek.APIKey = "deepseek-test-key"
	})

	for _, model := range []string{"sol", "deepseek-flash"} {
		// The fake DeepSeek answers with plain JSON, so ask for a plain reply.
		body := strings.NewReplacer(`"model": "sol"`, `"model": "`+model+`"`, `"stream": true`, `"stream": false`).Replace(string(raw))
		resp := post(t, env.front.URL+"/v1/messages", body, nil)
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: status = %d, want 200: %s", model, resp.StatusCode, msg)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	codexBody := env.codex.lastBody(t)
	if got := toolNames(t, codexBody); !reflect.DeepEqual(got, want) {
		t.Errorf("codex tools = %v, want %v", got, want)
	}
	dsBody := <-deepseekBody
	if got := toolNames(t, dsBody); !reflect.DeepEqual(got, want) {
		t.Errorf("deepseek tools = %v, want %v", got, want)
	}
	for name, body := range map[string]string{"codex": codexBody, "deepseek": dsBody} {
		for _, banned := range []string{"tool_addition", "tool_removal"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s upstream received a %s block", name, banned)
			}
		}
	}
}

// TestMalformedToolChangeIs400OnBothLegs: a tool change neither leg can honour
// is refused on /v1/messages and /v1/messages/count_tokens alike, with the
// reason in the body, instead of the block being dropped and the request
// answered 200 without the tool. A valid change spelled with JSON escapes is
// accepted on both legs and both routes.
func TestMalformedToolChangeIs400OnBothLegs(t *testing.T) {
	restoreRegistry(t)
	deepseek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_ds","type":"message","role":"assistant","model":"deepseek-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(deepseek.Close)
	env := newCodexEnv(t, func(cfg *config.Config) {
		cfg.DeepSeek.BaseURL = deepseek.URL
		cfg.DeepSeek.APIKey = "deepseek-test-key"
	})

	bad := `{"type":"tool_addition","tool":{"type":"tool_reference","name":"ghost"}}`
	good := `{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"late","input_schema":{"type":"object"}}}}`
	bs := string(rune(92)) // a backslash, spelled so no tool normalises the escape away
	escapedGood := strings.Replace(good, `"tool_addition"`, `"tool`+bs+`u005faddition"`, 1)
	if escapedGood == good {
		t.Fatal("test bug: nothing was escaped")
	}
	body := func(model, block string) string {
		return `{"model":"` + model + `","max_tokens":8,"tools":[{"name":"Read","input_schema":{"type":"object"}}],` +
			`"messages":[{"role":"user","content":"hi"},{"role":"system","content":[` + block + `]},{"role":"user","content":"go"}]}`
	}
	for _, model := range []string{"sol", "deepseek-flash"} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			send := func(block string) (int, string) {
				resp := post(t, env.front.URL+path, body(model, block), nil)
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, string(b)
			}
			if status, msg := send(bad); status != http.StatusBadRequest || !strings.Contains(msg, "ghost") {
				t.Errorf("%s %s: malformed change = %d %s, want 400 naming the tool", model, path, status, msg)
			}
			for name, block := range map[string]string{"literal": good, "escaped": escapedGood} {
				if status, msg := send(block); status != http.StatusOK {
					t.Errorf("%s %s: %s valid change = %d %s, want 200", model, path, name, status, msg)
				}
			}
		}
	}
}
