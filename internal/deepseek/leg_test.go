package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/proxyhdr"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/sse"
	"github.com/hughescr/utraque/internal/transport"
)

const testAPIKey = "sk-deepseek-test-not-real"

func testLeg(t *testing.T, base string) *Leg {
	t.Helper()
	l, err := New(base, testAPIKey, transport.NewStd(transport.DefaultOptions()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

func callLeg(t *testing.T, l *Leg, model, body string, stream, count bool, headers http.Header) (*httptest.ResponseRecorder, error) {
	t.Helper()
	dec, err := router.Resolve(model, "")
	if err != nil {
		t.Fatalf("Resolve(%q): %v", model, err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://utraque.test/v1/messages", strings.NewReader(body))
	if headers != nil {
		req.Header = headers.Clone()
	}
	rec := httptest.NewRecorder()
	rq := &router.Request{Raw: []byte(body), Stream: stream, Dec: dec}
	if count {
		err = l.CountTokens(rec, req, rq)
	} else {
		err = l.Messages(rec, req, rq)
	}
	return rec, err
}

func TestMessagesOwnsCredentialAndCanonicalizesLegacyFlash(t *testing.T) {
	type captured struct {
		path   string
		header http.Header
		body   map[string]json.RawMessage
	}
	gotCh := make(chan captured, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(body, &obj)
		gotCh <- captured{path: r.URL.Path, header: r.Header.Clone(), body: obj}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer caller-oauth-must-not-leave")
	headers.Set("Cookie", "session=must-not-leave")
	headers.Set("X-Utraque-Token", "local-secret-must-not-leave")
	headers.Set("X-Api-Key", "caller-key-must-not-leave")
	headers.Set("X-Private-Extension", "must-not-leave")
	headers.Add("Anthropic-Beta", "one")
	headers.Add("Anthropic-Beta", "two")
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("User-Agent", "test-agent")

	body := `{"model":"deepseek-v4-flash","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"u"},"service_tier":"auto"}`
	rec, err := callLeg(t, testLeg(t, upstream.URL+"/anthropic"), "deepseek-v4-flash", body, false, false, headers)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	got := <-gotCh
	if got.path != "/anthropic/v1/messages" {
		t.Errorf("path = %q", got.path)
	}
	if got.header.Get("X-Api-Key") != testAPIKey {
		t.Errorf("X-Api-Key was not the configured key")
	}
	for _, name := range []string{"Authorization", "Cookie", "X-Utraque-Token", "X-Private-Extension"} {
		if value := got.header.Get(name); value != "" {
			t.Errorf("%s leaked upstream: %q", name, value)
		}
	}
	if values := got.header.Values("Anthropic-Beta"); len(values) != 2 || values[0] != "one" || values[1] != "two" {
		t.Errorf("Anthropic-Beta = %q, want two preserved values", values)
	}
	var upstreamModel string
	if err := json.Unmarshal(got.body["model"], &upstreamModel); err != nil || upstreamModel != "deepseek-flash" {
		t.Errorf("upstream model = %q, err %v", upstreamModel, err)
	}
	if _, ok := got.body["metadata"]; !ok {
		t.Error("non-routing request fields were not preserved")
	}

	var response map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("response JSON: %v", err)
	}
	var responseModel string
	_ = json.Unmarshal(response["model"], &responseModel)
	if responseModel != "deepseek-flash" {
		t.Errorf("response model = %q, want canonical deepseek-flash", responseModel)
	}
}

type followingTransport struct{ client *http.Client }

func (t followingTransport) Client() *http.Client { return t.client }
func (followingTransport) Kind() string           { return transport.KindStd }

func TestStreamingCanonicalizesMessageStartAndPreservesToolFrames(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frames := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"wrong\",\"content\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_1\",\"name\":\"read\",\"input\":{}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"x\\\"}\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			fl.Flush()
		}
	}))
	defer upstream.Close()

	body := `{"model":"deepseek-v4-flash-vision-exp","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	rec, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-v4-flash-vision-exp", body, true, false, nil)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	scanner := sse.NewScanner(rec.Body)
	var events []sse.Frame
	for scanner.Scan() {
		events = append(events, scanner.Frame())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan response: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("events = %d, want 6\n%s", len(events), rec.Body.String())
	}
	var start struct {
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal(events[0].Data, &start); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	if start.Message.Model != "deepseek-flash" {
		t.Errorf("stream model = %q, want deepseek-flash", start.Message.Model)
	}
	if events[1].Event != "content_block_start" || !strings.Contains(string(events[1].Data), `"tool_use"`) {
		t.Errorf("tool frame changed or disappeared: %+v", events[1])
	}
}

func TestCountTokensIsEstimatedLocallyBecauseUpstreamDoesNotDocumentIt(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`
	rec, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-v4-flash", body, false, true, nil)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("count_tokens contacted an undocumented upstream endpoint")
	}
	var response struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.InputTokens <= 0 {
		t.Errorf("body = %s, err = %v", rec.Body.String(), err)
	}
	if got := rec.Header().Get(proxyhdr.TokenCountMethod); got != "estimated; estimator=chars/4" {
		t.Errorf("%s = %q, want explicit local-estimate method", proxyhdr.TokenCountMethod, got)
	}
}

func TestToolResultErrorsAreMarkedAndRemainReplayable(t *testing.T) {
	gotBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":"recovered"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	body := `{"model":"deepseek-flash","max_tokens":16,"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","is_error":true,"content":"file missing"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tool_2","name":"read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_2","is_error":true,"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"eA=="}},{"type":"text","text":"bad image"}]}]},` +
		`{"role":"user","content":"continue after both failures"}]}`
	if _, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-flash", body, false, false, nil); err != nil {
		t.Fatalf("Messages: %v", err)
	}

	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if raw := <-gotBody; json.Unmarshal(raw, &request) != nil {
		t.Fatalf("invalid rewritten request: %s", raw)
	}
	if len(request.Messages) != 5 || string(request.Messages[4].Content) != `"continue after both failures"` {
		t.Fatalf("subsequent turn was not preserved: %s", request.Messages[4].Content)
	}

	var first []map[string]json.RawMessage
	if err := json.Unmarshal(request.Messages[1].Content, &first); err != nil {
		t.Fatal(err)
	}
	if _, exists := first[0]["is_error"]; exists {
		t.Fatal("is_error leaked to DeepSeek")
	}
	var firstText string
	_ = json.Unmarshal(first[0]["content"], &firstText)
	if firstText != toolErrorMarker+"\n\nfile missing" {
		t.Errorf("string tool error = %q", firstText)
	}

	var second []map[string]json.RawMessage
	if err := json.Unmarshal(request.Messages[3].Content, &second); err != nil {
		t.Fatal(err)
	}
	if _, exists := second[0]["is_error"]; exists {
		t.Fatal("array tool error retained is_error")
	}
	var nested []map[string]json.RawMessage
	if err := json.Unmarshal(second[0]["content"], &nested); err != nil {
		t.Fatal(err)
	}
	var marked string
	_ = json.Unmarshal(nested[1]["text"], &marked)
	if marked != toolErrorMarker+"\n\nbad image" {
		t.Errorf("block-array tool error = %q", marked)
	}
}

func TestToolReferencesBecomeTextWhenSchemasRemainAvailable(t *testing.T) {
	// The fixture is the exact tool_result content recorded from Claude Code.
	// Its transcript does not expose the surrounding request, so this test adds
	// the tool definitions whose continued presence the rewrite requires.
	observedContent, err := os.ReadFile("testdata/tool_search_result_content.json")
	if err != nil {
		t.Fatal(err)
	}

	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		t.Run(model, func(t *testing.T) {
			gotBody := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotBody <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"`+model+`","content":[{"type":"text","text":"searching"}],"stop_reason":"tool_use","usage":{"input_tokens":8,"output_tokens":1}}`)
			}))
			defer upstream.Close()

			body := `{"model":"` + model + `","max_tokens":16,"tools":[` +
				`{"name":"ToolSearch","description":"Find tools","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}},` +
				`{"name":"WebSearch","description":"Search the web","defer_loading":true,"input_schema":{"type":"object","properties":{"query":{"type":"string"}}}},` +
				`{"name":"WebFetch","description":"Fetch a URL","defer_loading":true,"input_schema":{"type":"object","properties":{"url":{"type":"string"}}}}],` +
				`"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_00_q0GdFBXOSD1d3nfaTZ4V8104","name":"ToolSearch","input":{"query":"select:WebSearch,WebFetch"}}]},` +
				`{"role":"user","content":` + string(observedContent) + `}]}`
			if _, err := callLeg(t, testLeg(t, upstream.URL), model, body, false, false, nil); err != nil {
				t.Fatalf("Messages: %v", err)
			}

			var request struct {
				Tools []struct {
					Name         string          `json:"name"`
					DeferLoading json.RawMessage `json:"defer_loading"`
					InputSchema  struct {
						Type       string `json:"type"`
						Properties map[string]struct {
							Type string `json:"type"`
						} `json:"properties"`
					} `json:"input_schema"`
				} `json:"tools"`
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if raw := <-gotBody; json.Unmarshal(raw, &request) != nil {
				t.Fatalf("invalid rewritten request: %s", raw)
			}
			if len(request.Tools) != 3 || request.Tools[1].Name != "WebSearch" || request.Tools[1].InputSchema.Type != "object" ||
				request.Tools[1].InputSchema.Properties["query"].Type != "string" || request.Tools[2].Name != "WebFetch" ||
				request.Tools[2].InputSchema.Type != "object" || request.Tools[2].InputSchema.Properties["url"].Type != "string" {
				t.Fatalf("discovered tool schemas did not reach DeepSeek intact: %+v", request.Tools)
			}
			if len(request.Tools[1].DeferLoading) != 0 || len(request.Tools[2].DeferLoading) != 0 {
				t.Fatalf("referenced tool remained deferred: %+v", request.Tools)
			}

			var outer []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(request.Messages[1].Content, &outer); err != nil {
				t.Fatal(err)
			}
			if len(outer) != 1 || outer[0].Type != "tool_result" || len(outer[0].Content) != 2 {
				t.Fatalf("rewritten discovery result = %+v", outer)
			}
			if got := outer[0].Content[0]; got.Type != "text" || got.Text != `[tool available: "WebSearch"]` {
				t.Errorf("first discovered tool = %+v", got)
			}
			if got := outer[0].Content[1]; got.Type != "text" || got.Text != `[tool available: "WebFetch"]` {
				t.Errorf("second discovered tool = %+v", got)
			}
		})
	}
}

func TestToolSchemaPatternsAreTranslatedForDeepSeek(t *testing.T) {
	// The fixture is the shape of Claude Code's Artifact tool, whose
	// file_paths item pattern "^[^\0]*$" DeepSeek rejects as not a regex.
	artifactSchema, err := os.ReadFile("testdata/artifact_tool_schema.json")
	if err != nil {
		t.Fatal(err)
	}
	// A tool no dialect needs to touch, so its schema must arrive as sent;
	// and one whose pattern has no DeepSeek spelling, so it falls back to the
	// description. Both are deliberately not gofmt-tidy JSON: compaction is
	// the only change the wire form may show.
	const readSchema = `{"type":"object","properties":{"file_path":{"type":"string","pattern":"^/"},"limit":{"type":"integer","minimum":1}},"required":["file_path"]}`
	const quotedSchema = `{"type":"object","properties":{"q":{"type":"string","description":"A literal.","pattern":"\\Qa.b\\E"}}}`

	gotBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	leg := testLeg(t, upstream.URL)
	WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))(leg)

	body := `{"model":"deepseek-flash","max_tokens":16,"tools":[` +
		`{"name":"Artifact","description":"publish a page","input_schema":` + string(artifactSchema) + `},` +
		`{"name":"Read","description":"read a file","input_schema":` + readSchema + `},` +
		`{"name":"Quoted","description":"quoted","input_schema":` + quotedSchema + `}],` +
		`"messages":[{"role":"user","content":"publish it"}]}`
	if _, err := callLeg(t, leg, "deepseek-flash", body, false, false, nil); err != nil {
		t.Fatalf("Messages: %v", err)
	}

	var request struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	raw := <-gotBody
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatalf("invalid rewritten request: %s", raw)
	}
	if len(request.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(request.Tools))
	}

	// Artifact: exactly one pattern respelled, everything else preserved.
	var wantArtifact map[string]any
	if err := json.Unmarshal(artifactSchema, &wantArtifact); err != nil {
		t.Fatal(err)
	}
	items := wantArtifact["properties"].(map[string]any)["file_paths"].(map[string]any)["items"].(map[string]any)
	if items["pattern"] != `^[^\0]*$` {
		t.Fatalf("fixture drifted: file_paths.items.pattern = %q", items["pattern"])
	}
	items["pattern"] = `^[^\x00]*$`
	wantArtifactBytes, _ := json.Marshal(wantArtifact)
	if got := string(request.Tools[0].InputSchema); got != string(wantArtifactBytes) {
		t.Errorf("Artifact schema on the wire:\n got %s\nwant %s", got, wantArtifactBytes)
	}

	// Read: byte-identical (compaction aside, and it was already compact).
	if got := string(request.Tools[1].InputSchema); got != readSchema {
		t.Errorf("untouched Read schema changed on the wire:\n got %s\nwant %s", got, readSchema)
	}

	// Quoted: pattern dropped, constraint folded into the description.
	var quoted map[string]any
	if err := json.Unmarshal(request.Tools[2].InputSchema, &quoted); err != nil {
		t.Fatal(err)
	}
	q := quoted["properties"].(map[string]any)["q"].(map[string]any)
	if _, ok := q["pattern"]; ok {
		t.Errorf("Quoted pattern was not dropped: %v", q)
	}
	if got, want := q["description"], `A literal. Must match the regular expression: \Qa.b\E`; got != want {
		t.Errorf("Quoted description = %q, want %q", got, want)
	}

	// The rewrite is logged the way the codex leg logs its translation.
	var entry struct {
		Msg       string   `json:"msg"`
		Rewritten []string `json:"rewritten_patterns"`
		Dropped   []string `json:"dropped_patterns"`
	}
	found := false
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		if json.Unmarshal(line, &entry) == nil && strings.Contains(entry.Msg, "tool schema patterns") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no pattern rewrite log line in:\n%s", logs.String())
	}
	if got, want := entry.Rewritten, []string{"Artifact.properties.file_paths.items"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rewritten_patterns = %v, want %v", got, want)
	}
	if got, want := entry.Dropped, []string{"Quoted.properties.q"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dropped_patterns = %v, want %v", got, want)
	}
}

func TestToolSchemasWithoutPatternsAreNotReencoded(t *testing.T) {
	gotBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	// Key order inside a tool is the sender's; an untouched tools array must
	// keep it, which shows the array itself was not decoded and re-encoded.
	const tools = `[{"name":"Read","description":"read","input_schema":{"type":"object","properties":{"path":{"type":"string","pattern":"^(?!\\.)[^\\x00]+$"}}}}]`
	body := `{"model":"deepseek-flash","max_tokens":16,"tools":` + tools + `,"messages":[{"role":"user","content":"hi"}]}`
	if _, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-flash", body, false, false, nil); err != nil {
		t.Fatalf("Messages: %v", err)
	}
	var request map[string]json.RawMessage
	raw := <-gotBody
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatalf("invalid rewritten request: %s", raw)
	}
	if got := string(request["tools"]); got != tools {
		t.Errorf("tools were re-encoded:\n got %s\nwant %s", got, tools)
	}
}

func TestTruncatedStreamAfterMessageStartIsNeverBlessedAsComplete(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"wrong\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()

	body := `{"model":"deepseek-flash","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	_, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-flash", body, true, false, nil)
	if !errors.Is(err, router.ErrResponseStarted) || !strings.Contains(err.Error(), "without message_stop or error") {
		t.Fatalf("error = %v, want a started-response truncation error", err)
	}
}

func TestTruncatedTerminalAndErrorFramesAreRejected(t *testing.T) {
	for _, terminal := range []string{"message_stop", "error"} {
		t.Run(terminal, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"deepseek-flash\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
				_, _ = io.WriteString(w, "event: "+terminal+"\ndata: {\"type\":")
			}))
			defer upstream.Close()

			body := `{"model":"deepseek-flash","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
			_, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-flash", body, true, false, nil)
			if !errors.Is(err, router.ErrResponseStarted) || !strings.Contains(err.Error(), "invalid deepseek stream") {
				t.Fatalf("error = %v, want invalid started response", err)
			}
		})
	}
}

func TestContradictoryKnownResponseModelIsRejected(t *testing.T) {
	t.Run("JSON", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		defer upstream.Close()
		body := `{"model":"deepseek-v4-pro","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
		rec, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-v4-pro", body, false, false, nil)
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.HTTPStatus() != http.StatusBadGateway || !strings.Contains(err.Error(), "contradicts") {
			t.Fatalf("error = %v, want model-mismatch 502", err)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("contradictory response was forwarded: %s", rec.Body.String())
		}
	})

	t.Run("SSE", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"deepseek-flash\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		}))
		defer upstream.Close()
		body := `{"model":"deepseek-v4-pro","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
		rec, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-v4-pro", body, true, false, nil)
		if !errors.Is(err, router.ErrResponseStarted) || !strings.Contains(err.Error(), "contradicts") {
			t.Fatalf("error = %v, want started model-mismatch error", err)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("contradictory message_start was forwarded: %s", rec.Body.String())
		}
	})
}

func TestUnsupportedContentIsRejectedBeforeSpending(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()
	l := testLeg(t, upstream.URL)

	cases := []struct {
		name, model, content string
		wantMessage          string // exact client-facing text when it matters
	}{
		{name: "document", model: "deepseek-flash", content: `[{"type":"document","source":{"type":"base64","data":"x"}}]`},
		{name: "redacted thinking", model: "deepseek-flash", content: `[{"type":"redacted_thinking","data":"x"}]`},
		// The image gate is driven by the catalog's SupportsImages flag; the
		// message names the model and is part of the client-visible contract.
		{name: "pro image", model: "deepseek-v4-pro", content: `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]`,
			wantMessage: "deepseek-v4-pro does not support image content"},
		{name: "unresolved nested tool reference", model: "deepseek-flash", content: `[{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"tool_reference","tool_name":"WebSearch"}]}]`},
		{name: "top-level tool reference", model: "deepseek-flash", content: `[{"type":"tool_reference","tool_name":"WebSearch"}]`},
		{name: "unknown nested tool result block", model: "deepseek-flash", content: `[{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"future_tool_result"}]}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"` + tc.model + `","max_tokens":8,"messages":[{"role":"user","content":` + tc.content + `}]}`
			rec, err := callLeg(t, l, tc.model, body, false, false, nil)
			if err == nil {
				t.Fatalf("expected rejection, got status %d body %s", rec.Code, rec.Body.String())
			}
			var ae *apierr.Error
			if !errors.As(err, &ae) || ae.HTTPStatus() != http.StatusBadRequest {
				t.Fatalf("error = %v, want invalid-request 400", err)
			}
			if tc.wantMessage != "" && ae.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", ae.Message, tc.wantMessage)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("unsupported requests reached upstream %d time(s)", hits.Load())
	}

	for _, extra := range []string{
		`,"top_k":10`,
		`,"service_tier":"priority"`,
		`,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`,
		`,"output_config":{"format":{"type":"json_schema"}}`,
	} {
		body := `{"model":"deepseek-flash","max_tokens":8,"messages":[{"role":"user","content":"hello"}]` + extra + `}`
		if _, err := callLeg(t, l, "deepseek-flash", body, false, false, nil); err == nil {
			t.Errorf("semantic requirement %s was silently accepted", extra)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("semantic requirements reached upstream %d time(s)", hits.Load())
	}
}

func TestRedirectIsRejectedWithoutForwardingTheKeyAgain(t *testing.T) {
	var redirectHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/credential-sink", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/credential-sink", func(http.ResponseWriter, *http.Request) {
		redirectHits.Add(1)
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	l, newErr := New(upstream.URL, testAPIKey, followingTransport{client: &http.Client{}})
	if newErr != nil {
		t.Fatal(newErr)
	}
	body := `{"model":"deepseek-flash","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	_, err := callLeg(t, l, "deepseek-flash", body, false, false, nil)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("error = %v, want api error 502", err)
	}
	if redirectHits.Load() != 0 {
		t.Fatal("redirect was followed; the DeepSeek key could have leaked")
	}
}

func TestCanceledCallerStopsUpstreamRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer upstream.Close()
	l := testLeg(t, upstream.URL)
	body := `{"model":"deepseek-flash","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	dec, _ := router.Resolve("deepseek-flash", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	err := l.Messages(httptest.NewRecorder(), req, &router.Request{Raw: []byte(body), Dec: dec})
	if !errors.Is(err, router.ErrClientGone) {
		t.Fatalf("error = %v, want ErrClientGone", err)
	}
}
