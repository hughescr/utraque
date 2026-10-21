package deepseek

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/effort"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/router"
)

// TestDeepseekEffortMapping pins the level table to DeepSeek's documented one
// (https://api-docs.deepseek.com/guides/thinking_mode): the endpoint takes
// low|high|max, folding minimal->low, medium->high, xhigh->high, ultra->max.
func TestDeepseekEffortMapping(t *testing.T) {
	cases := map[effort.Level]string{
		effort.Low:    "low",
		effort.Medium: "high",
		effort.High:   "high",
		effort.XHigh:  "high",
		effort.Max:    "max",
		effort.Ultra:  "max",
		"":            "",
		"hgh":         "",
	}
	for in, want := range cases {
		if got := deepseekEffort(in); got != want {
			t.Errorf("deepseekEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func rewriteForTest(t *testing.T, body string, requested effort.Level) (map[string]json.RawMessage, string) {
	t.Helper()
	out, _, applied, err := rewriteRequest([]byte(body), "deepseek-flash", requested)
	if err != nil {
		t.Fatalf("rewriteRequest: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	return obj, applied
}

func TestRewriteEffort(t *testing.T) {
	const base = `"model":"deepseek-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}]`
	cases := []struct {
		name        string
		body        string
		requested   effort.Level
		wantApplied string
		wantOC      string // exact output_config JSON, "" when it must be absent
	}{
		{"claude code's xhigh is sent as high", `{` + base + `,"output_config":{"effort":"xhigh"}}`,
			effort.XHigh, "high", `{"effort":"high"}`},
		{"max is sent as max", `{` + base + `,"output_config":{"effort":"max"}}`,
			effort.Max, "max", `{"effort":"max"}`},
		{"medium is sent as high", `{` + base + `,"output_config":{"effort":"medium"}}`,
			effort.Medium, "high", `{"effort":"high"}`},
		{"mixed case is normalised", `{` + base + `,"output_config":{"effort":"LOW"}}`,
			effort.Low, "low", `{"effort":"low"}`},
		{"no effort named leaves no output_config", `{` + base + `}`,
			"", "", ""},
		{"an unrecognised effort is dropped rather than forwarded", `{` + base + `,"output_config":{"effort":"hgh"}}`,
			"", "", ""},
		{"a non-string effort is dropped", `{` + base + `,"output_config":{"effort":7}}`,
			"", "", ""},
		{"a resolved effort is written even when the body had none", `{` + base + `}`,
			effort.High, "high", `{"effort":"high"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, applied := rewriteForTest(t, c.body, c.requested)
			if applied != c.wantApplied {
				t.Errorf("applied = %q, want %q", applied, c.wantApplied)
			}
			got, present := obj["output_config"]
			switch {
			case c.wantOC == "" && present:
				t.Errorf("output_config = %s, want it absent", got)
			case c.wantOC != "" && string(got) != c.wantOC:
				t.Errorf("output_config = %s, want %s", got, c.wantOC)
			}
		})
	}
}

// TestMessagesSendsMappedEffortAndLogsIt drives the real leg: a bare model with
// the effort in the body reaches DeepSeek as output_config.effort in
// DeepSeek's own vocabulary, and the request line gets effort_applied.
func TestMessagesSendsMappedEffortAndLogsIt(t *testing.T) {
	var sent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	body := `{"model":"deepseek-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"xhigh"}}`
	dec, err := router.Resolve("deepseek-flash", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	dec = dec.WithBodyEffort("xhigh")

	sum := obs.NewSummary()
	req := httptest.NewRequest(http.MethodPost, "http://utraque.test/v1/messages", strings.NewReader(body))
	req = req.WithContext(obs.WithSummary(req.Context(), sum))
	rec := httptest.NewRecorder()
	rq := &router.Request{Raw: []byte(body), Dec: dec}
	if err := testLeg(t, upstream.URL+"/anthropic").Messages(rec, req, rq); err != nil {
		t.Fatalf("Messages: %v", err)
	}

	var obj struct {
		OutputConfig map[string]string `json:"output_config"`
	}
	if err := json.Unmarshal([]byte(sent), &obj); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, sent)
	}
	if obj.OutputConfig["effort"] != "high" {
		t.Errorf("upstream output_config = %v, want effort high (xhigh folds to high)", obj.OutputConfig)
	}
	if got := sum.Fields()["effort_applied"]; got != "high" {
		t.Errorf("request line effort_applied = %v, want high", got)
	}
}
