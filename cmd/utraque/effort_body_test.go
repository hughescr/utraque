package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hughescr/utraque/internal/config"
)

// TestBodyEffortThroughTheDispatcher drives the whole path for Claude Code's
// real shape: a bare model name with the effort in output_config.effort. The
// fake catalog lists sol's levels as low|medium|high|max|ultra (no xhigh).
func TestBodyEffortThroughTheDispatcher(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnvOpts(t, nil, codexEnvOptions{CaptureLogs: true, Level: slog.LevelInfo})

	cases := []struct {
		name          string
		model         string
		outputConfig  string
		wantSent      string // reasoning.effort the Codex backend received
		wantRequested string
		wantSource    string
	}{
		{"bare alias takes the body effort", "sol", `{"effort":"medium"}`, "medium", "medium", "body"},
		{"max is supported and passes", "sol", `{"effort":"max"}`, "max", "max", "body"},
		{"xhigh clamps down to high", "sol", `{"effort":"xhigh"}`, "high", "xhigh", "body"},
		{"suffix beats the body", "sol-high", `{"effort":"low"}`, "high", "high", "suffix"},
		{"no body effort uses the catalog default", "sol", `null`, "low", "", "default"},
		{"an unrecognised body effort is ignored", "sol", `{"effort":"hgh"}`, "low", "", "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(requestLines(t, env.logs.String()))
			body := `{"model":"` + c.model + `","max_tokens":16,"stream":true,"output_config":` + c.outputConfig +
				`,"messages":[{"role":"user","content":"hi"}]}`
			resp := post(t, env.front.URL+"/v1/messages", body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			var sent struct {
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.Unmarshal([]byte(env.codex.lastBody(t)), &sent); err != nil {
				t.Fatalf("decode upstream request: %v", err)
			}
			if sent.Reasoning.Effort != c.wantSent {
				t.Errorf("upstream reasoning.effort = %q, want %q", sent.Reasoning.Effort, c.wantSent)
			}

			lines := requestLines(t, env.logs.String())
			if len(lines) != before+1 {
				t.Fatalf("want one new request line, got %d", len(lines)-before)
			}
			rec := lines[len(lines)-1]
			if rec["effort_requested"] != c.wantRequested || rec["effort_source"] != c.wantSource || rec["effort_applied"] != c.wantSent {
				t.Errorf("request line effort = requested %v source %v applied %v, want %q %q %q",
					rec["effort_requested"], rec["effort_source"], rec["effort_applied"],
					c.wantRequested, c.wantSource, c.wantSent)
			}
		})
	}
}

// TestBodyEffortReachesDeepSeekInItsOwnVocabulary: the DeepSeek leg takes the
// same body effort and sends DeepSeek's low|high|max spelling.
func TestBodyEffortReachesDeepSeekInItsOwnVocabulary(t *testing.T) {
	restoreRegistry(t)
	got := make(chan string, 1)
	deepseek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_ds","type":"message","role":"assistant","model":"deepseek-v4-pro","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(deepseek.Close)
	env := newCodexEnvOpts(t, func(cfg *config.Config) {
		cfg.DeepSeek.BaseURL = deepseek.URL
		cfg.DeepSeek.APIKey = "deepseek-test-key"
	}, codexEnvOptions{CaptureLogs: true, Level: slog.LevelInfo})

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"deepseek-v4-pro","max_tokens":16,"output_config":{"effort":"medium"},"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	var sent struct {
		OutputConfig map[string]string `json:"output_config"`
	}
	if err := json.Unmarshal([]byte(<-got), &sent); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	if sent.OutputConfig["effort"] != "high" {
		t.Errorf("upstream output_config = %v, want effort high (medium folds to high)", sent.OutputConfig)
	}
	lines := requestLines(t, env.logs.String())
	if len(lines) != 1 {
		t.Fatalf("want one request line, got %d", len(lines))
	}
	rec := lines[0]
	if rec["effort_requested"] != "medium" || rec["effort_source"] != "body" || rec["effort_applied"] != "high" {
		t.Errorf("request line effort = requested %v source %v applied %v, want medium body high",
			rec["effort_requested"], rec["effort_source"], rec["effort_applied"])
	}
}
