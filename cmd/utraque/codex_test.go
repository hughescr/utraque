package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/leg"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/sse"
)

// This file drives the Codex leg end to end through main's real wiring.
//
// SAFETY: every upstream here is an httptest server and every credential is a
// throwaway JWT under t.TempDir(). The real chatgpt.com, the real
// auth.openai.com, and the real ~/.codex/auth.json are never contacted, read or
// written by anything in this package — cfg.Codex.BaseURL, cfg.Codex.TokenURL
// and cfg.Codex.AuthFile are all overridden before the server is built.

// codexBasePath mirrors the real backend's path prefix so the fake upstream
// exercises the same URL joining production does.
const codexBasePath = "/backend-api/codex"

// fakeCatalogBody is the models list the fake backend serves. sol's supported
// levels stop at "ultra" with no "xhigh", which is what the effort-clamping
// assertion below relies on.
const fakeCatalogBody = `{"models":[
  {"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","visibility":"list",
   "context_window":400000,"default_reasoning_level":"low","priority":10,
   "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"max"},{"effort":"ultra"}]},
  {"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list",
   "context_window":272000,"default_reasoning_level":"medium","priority":5,
   "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}
]}`

// solStream is a complete, well-formed Codex Responses stream for a short text
// answer. It is the fixture the "a GPT model answers through the proxy"
// milestone is asserted against.
const solStream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_sol_1","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Sol "}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"answers."}

event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"Sol answers."}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_sol_1","status":"completed","usage":{"input_tokens":19,"output_tokens":4,"input_tokens_details":{"cached_tokens":8}}}}

`

// --- fake upstreams ------------------------------------------------------

// fakeCodex is a stand-in for chatgpt.com/backend-api/codex. It records what
// each /responses call received and answers with whatever the test installed.
type fakeCodex struct {
	srv *httptest.Server

	mu       sync.Mutex
	bodies   []string
	authHdrs []string
	accounts []string
	betas    []string

	calls   atomic.Int64
	catalog atomic.Int64

	// catalogBody overrides the models list the fake backend serves. Empty means
	// fakeCatalogBody. Set it before the first catalog read.
	catalogBody string

	// catalogFail, when non-zero, makes the catalog endpoint answer with that
	// status instead of a model list.
	catalogFail int

	// respond serves one /responses call. n is the 1-based attempt number, so a
	// test can fail the first attempt and succeed on the retry.
	respond func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64)
}

func newFakeCodex(t *testing.T) *fakeCodex {
	t.Helper()
	f := &fakeCodex{}
	mux := http.NewServeMux()
	mux.HandleFunc(codexBasePath+"/models", func(w http.ResponseWriter, r *http.Request) {
		f.catalog.Add(1)
		if f.catalogFail != 0 {
			w.WriteHeader(f.catalogFail)
			_, _ = io.WriteString(w, `{"error":{"message":"catalog unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := f.catalogBody
		if body == "" {
			body = fakeCatalogBody
		}
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc(codexBasePath+"/responses", func(w http.ResponseWriter, r *http.Request) {
		n := f.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		f.authHdrs = append(f.authHdrs, r.Header.Get("Authorization"))
		f.accounts = append(f.accounts, r.Header.Get("chatgpt-account-id"))
		f.betas = append(f.betas, r.Header.Get("OpenAI-Beta"))
		f.mu.Unlock()
		if f.respond == nil {
			writeSSE(w, solStream)
			return
		}
		f.respond(t, w, r, n)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fake codex backend received an unexpected path %q", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCodex) baseURL() string { return f.srv.URL + codexBasePath }

func (f *fakeCodex) lastBody(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("the codex backend was never called")
	}
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeCodex) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.authHdrs))
	copy(out, f.authHdrs)
	return out
}

// lastIdentity returns the credential headers of the most recent call: the
// bearer token, the account id, and the beta flag the Codex CLI itself sends.
func (f *fakeCodex) lastIdentity(t *testing.T) (authorization, account, beta string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.authHdrs)
	if n == 0 {
		t.Fatal("the codex backend was never called")
	}
	return f.authHdrs[n-1], f.accounts[n-1], f.betas[n-1]
}

// writeSSE streams a fixture out frame by frame, flushing each one, so the
// proxy sees a real incremental stream rather than one buffered blob.
func writeSSE(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for _, frame := range strings.SplitAfter(body, "\n\n") {
		if frame == "" {
			continue
		}
		_, _ = io.WriteString(w, frame)
		_ = rc.Flush()
	}
}

// codexEnv is one fully isolated Codex environment: a fake backend, a fake
// OAuth token endpoint, a throwaway auth.json, and the front door built over
// them by main's own newServer.
type codexEnv struct {
	front *httptest.Server
	codex *fakeCodex
	// app is the assembled process behind front. Tests reach for it only when
	// they need the background work — the startup catalog warm — that an
	// ordinary handler test must never trigger.
	app *app
	// logs holds everything the proxy logged, for the tests that assert on what
	// did (and did not) reach the log.
	logs      *syncBuffer
	authPath  string
	refreshes atomic.Int64
	// anthropicHits counts any request that reached the Anthropic upstream. A
	// codex-routed request must never contribute to it: a cross-leg fallback
	// would spend the wrong subscription.
	anthropicHits atomic.Int64
}

// syncBuffer is a bytes.Buffer a test can read while the server is still
// writing to it. A plain buffer races: the log is written on the request
// goroutine and read on the test's, which is exactly the pattern -race exists
// to catch.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// codexEnvOptions tunes the environment beyond the config.
type codexEnvOptions struct {
	// Level is the log level captured into env.logs. Zero means "capture
	// nothing", which is what almost every test wants.
	Level slog.Level
	// CaptureLogs turns on log capture through the production obs handler —
	// including its scrubbing wrapper, which is the point.
	CaptureLogs bool
	// TraceDir, when set, turns on per-request trace dumps.
	TraceDir string
}

// newCodexEnv stands up the whole thing. mutate, when non-nil, adjusts the
// config just before the server is built.
func newCodexEnv(t *testing.T, mutate func(*config.Config)) *codexEnv {
	t.Helper()
	return newCodexEnvOpts(t, mutate, codexEnvOptions{})
}

// newCodexEnvOpts is newCodexEnv with the observability knobs exposed.
func newCodexEnvOpts(t *testing.T, mutate func(*config.Config), opts codexEnvOptions) *codexEnv {
	t.Helper()
	env := &codexEnv{codex: newFakeCodex(t)}

	// A throwaway auth.json whose access token expires far in the future, so no
	// refresh happens unless a test provokes one.
	dir := t.TempDir()
	env.authPath = filepath.Join(dir, "auth.json")
	writeAuthJSON(t, env.authPath, jwtWithExp(time.Now().Add(2*time.Hour)), "refresh-token-1")

	// A fake OAuth token endpoint. The real auth.openai.com is never contacted.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := env.refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-token-%d"}`,
			jwtWithExp(time.Now().Add(3*time.Hour)), n+1)
	}))
	t.Cleanup(tokenSrv.Close)

	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.anthropicHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_anthropic","type":"message"}`)
	}))
	t.Cleanup(anthropic.Close)

	cfg := config.Default()
	cfg.Anthropic.BaseURL = anthropic.URL
	cfg.Codex.BaseURL = env.codex.baseURL()
	cfg.Codex.TokenURL = tokenSrv.URL
	cfg.Codex.AuthFile = env.authPath
	// CachePath stays empty: the catalog runs memory-only, so no test ever
	// writes into the user's cache directory.
	if mutate != nil {
		mutate(&cfg)
	}

	log := slog.New(slog.DiscardHandler)
	if opts.CaptureLogs {
		// The production handler, scrubbing wrapper included — an assertion
		// against a hand-rolled handler would prove nothing about what the
		// daemon actually writes.
		env.logs = &syncBuffer{}
		var err error
		log, err = obs.NewLogger(env.logs, opts.Level, "json")
		if err != nil {
			t.Fatalf("obs.NewLogger: %v", err)
		}
	}

	var tracer *obs.Tracer
	if opts.TraceDir != "" {
		var err error
		tracer, err = obs.NewTracer(opts.TraceDir, log)
		if err != nil {
			t.Fatalf("obs.NewTracer: %v", err)
		}
	}

	// newApp, not newServer: the app carries the startup catalog warm, which no
	// test runs unless it asks for it by name.
	a, err := newApp(cfg, log, nil, tracer)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	env.app = a
	env.front = httptest.NewServer(a.srv)
	t.Cleanup(env.front.Close)
	return env
}

func writeAuthJSON(t *testing.T, path, accessToken, refreshToken string) {
	t.Helper()
	writeAuthJSONWithAccount(t, path, accessToken, refreshToken, "acct-fake")
}

// writeAuthJSONWithAccount is writeAuthJSON with the account id chosen, for the
// tests that assert it never reaches a log in the clear.
func writeAuthJSONWithAccount(t *testing.T, path, accessToken, refreshToken, accountID string) {
	t.Helper()
	body := fmt.Sprintf(
		`{"tokens":{"account_id":%q,"access_token":%q,"refresh_token":%q},"codex_cli_only":"preserve me"}`,
		accountID, accessToken, refreshToken)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

func readAccessToken(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var af struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
		CodexCLIOnly string `json:"codex_cli_only"`
	}
	if err := json.Unmarshal(raw, &af); err != nil {
		t.Fatalf("decode auth.json %q: %v", raw, err)
	}
	if af.CodexCLIOnly != "preserve me" {
		t.Errorf("auth.json lost a key utraque does not own: %s", raw)
	}
	return af.Tokens.AccessToken
}

// --- SSE reading ---------------------------------------------------------

type frame struct {
	event string
	data  string
}

// readFrames drains an Anthropic SSE response into frames.
func readFrames(t *testing.T, r io.Reader) []frame {
	t.Helper()
	var out []frame
	sc := sse.NewScanner(r)
	for sc.Scan() {
		f := sc.Frame()
		out = append(out, frame{event: f.Event, data: string(f.Data)})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning the translated stream: %v", err)
	}
	return out
}

func eventNames(frames []frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.event
	}
	return out
}

// --- tests ---------------------------------------------------------------

// TestCodexStreamingProducesAnthropicSSE is the milestone: a `sol` request
// walks the whole leg — translation, credential, upstream stream, incremental
// translation — and comes back as a well-formed Anthropic SSE stream.
func TestCodexStreamingProducesAnthropicSSE(t *testing.T) {
	env := newCodexEnv(t, nil)

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":64,"stream":true,`+
			`"system":"You are terse.",`+
			`"messages":[{"role":"user","content":"say hi"}]}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Streaming response headers, including the ones that stop an intermediary
	// re-buffering a true-incremental translation back into one lump.
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
		leg.HeaderRoute:     "codex",
		leg.HeaderModel:     "gpt-5.6-sol",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	frames := readFrames(t, resp.Body)
	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if got := eventNames(frames); !equalStrings(got, want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}

	var start struct {
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Role  string `json:"role"`
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(frames[0].data), &start); err != nil {
		t.Fatalf("decode message_start %q: %v", frames[0].data, err)
	}
	// The model is echoed back exactly as the caller wrote it, not as the
	// upstream slug: the client matches this against what it asked for.
	if start.Message.Model != "sol" {
		t.Errorf("message_start.model = %q, want sol", start.Message.Model)
	}
	if start.Message.Role != "assistant" {
		t.Errorf("message_start.role = %q, want assistant", start.Message.Role)
	}
	if start.Message.Usage.InputTokens <= 0 {
		t.Errorf("message_start usage.input_tokens = %d, want the local estimate seeded",
			start.Message.Usage.InputTokens)
	}

	var text strings.Builder
	for _, f := range frames {
		if f.event != "content_block_delta" {
			continue
		}
		var d struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(f.data), &d); err != nil {
			t.Fatalf("decode delta %q: %v", f.data, err)
		}
		text.WriteString(d.Delta.Text)
	}
	if got := text.String(); got != "Sol answers." {
		t.Errorf("assembled text = %q, want %q", got, "Sol answers.")
	}

	var delta struct {
		Delta struct {
			StopReason *string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(frames[5].data), &delta); err != nil {
		t.Fatalf("decode message_delta %q: %v", frames[5].data, err)
	}
	if delta.Delta.StopReason == nil || *delta.Delta.StopReason != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", delta.Delta.StopReason)
	}
	// Usage must be the upstream's own numbers, not the estimate.
	if delta.Usage.InputTokens != 19 || delta.Usage.OutputTokens != 4 || delta.Usage.CacheReadInputTokens != 8 {
		t.Errorf("usage = %+v, want the upstream's 19/4/8", delta.Usage)
	}

	// The credential the backend saw: the Codex OAuth bearer token and account
	// id from the fake auth.json, under the Codex CLI's own beta flag. The
	// caller's Anthropic Authorization header must never appear here.
	authorization, account, beta := env.codex.lastIdentity(t)
	if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) <= len("Bearer ") {
		t.Errorf("upstream Authorization = %q, want a bearer token from auth.json", authorization)
	}
	if account != "acct-fake" {
		t.Errorf("chatgpt-account-id = %q, want acct-fake", account)
	}
	if beta != "responses=experimental" {
		t.Errorf("OpenAI-Beta = %q, want responses=experimental", beta)
	}

	// What the backend actually received.
	body := env.codex.lastBody(t)
	var sent struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Store        bool   `json:"store"`
		Stream       bool   `json:"stream"`
		Reasoning    *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decode upstream request %q: %v", body, err)
	}
	if sent.Model != "gpt-5.6-sol" {
		t.Errorf("upstream model = %q, want gpt-5.6-sol", sent.Model)
	}
	if sent.Instructions != "You are terse." {
		t.Errorf("instructions = %q, want the client's system prompt verbatim", sent.Instructions)
	}
	if sent.Store {
		t.Error("store = true; utraque must always send store:false")
	}
	if !sent.Stream {
		t.Error("stream = false; the responses client must force stream:true")
	}
	// No suffix and no config override, so effort comes from the catalog's
	// default_reasoning_level for this slug.
	if sent.Reasoning == nil || sent.Reasoning.Effort != "low" {
		t.Errorf("reasoning = %+v, want the catalog default effort low", sent.Reasoning)
	}
	if len(sent.Input) != 1 || sent.Input[0].Role != "user" ||
		len(sent.Input[0].Content) != 1 || sent.Input[0].Content[0].Text != "say hi" {
		t.Errorf("input = %+v, want one user message carrying the prompt", sent.Input)
	}

	if n := env.anthropicHits.Load(); n != 0 {
		t.Errorf("the Anthropic upstream was contacted %d times for a codex request, want 0", n)
	}
	if n := env.refreshes.Load(); n != 0 {
		t.Errorf("the token endpoint was contacted %d times for a live credential, want 0", n)
	}
}

// TestCodexEffortSuffixIsClampedToTheCatalog pins the catalog's role in the
// leg: "sol-xhigh" asks for a level this model does not list, so it is clamped
// DOWN to the highest one it does.
func TestCodexEffortSuffixIsClampedToTheCatalog(t *testing.T) {
	env := newCodexEnv(t, nil)

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol-xhigh","max_tokens":32,"stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	var sent struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	body := env.codex.lastBody(t)
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decode upstream request %q: %v", body, err)
	}
	if sent.Reasoning.Effort != "high" {
		t.Errorf("effort = %q, want it clamped down to high (sol lists no xhigh)", sent.Reasoning.Effort)
	}
	if n := env.codex.catalog.Load(); n == 0 {
		t.Error("the catalog was never read, so the clamp could not have consulted it")
	}
}

// TestCodexNonStreamingReturnsMessagesResponse asserts stream:false folds the
// same translated events into one complete Anthropic MessagesResponse.
func TestCodexNonStreamingReturnsMessagesResponse(t *testing.T) {
	env := newCodexEnv(t, nil)

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":64,"messages":[{"role":"user","content":"say hi"}]}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Get(leg.HeaderRoute); got != "codex" {
		t.Errorf("%s = %q, want codex", leg.HeaderRoute, got)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var msg struct {
		ID           string  `json:"id"`
		Type         string  `json:"type"`
		Role         string  `json:"role"`
		Model        string  `json:"model"`
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
		Content      []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode MessagesResponse %q: %v", raw, err)
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("type/role = %q/%q, want message/assistant", msg.Type, msg.Role)
	}
	if msg.Model != "sol" {
		t.Errorf("model = %q, want sol echoed back", msg.Model)
	}
	if msg.StopReason == nil || *msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg.StopReason)
	}
	if msg.StopSequence != nil {
		t.Errorf("stop_sequence = %v, want null", *msg.StopSequence)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "Sol answers." {
		t.Errorf("content = %+v, want one text block %q", msg.Content, "Sol answers.")
	}
	if msg.Usage.InputTokens != 19 || msg.Usage.OutputTokens != 4 {
		t.Errorf("usage = %+v, want the upstream's 19/4", msg.Usage)
	}
	// A non-streaming answer must not leak SSE framing.
	if strings.Contains(string(raw), "event:") || strings.Contains(string(raw), "data:") {
		t.Errorf("non-streaming body carries SSE framing: %s", raw)
	}
}

// TestCodex401RefreshesAndRetriesExactlyOnce pins the single sanctioned retry:
// one 401, one refresh, one retry, then success — and the rotated token is
// written back so the Codex CLI keeps working.
func TestCodex401RefreshesAndRetriesExactlyOnce(t *testing.T) {
	env := newCodexEnv(t, nil)
	env.codex.respond = func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64) {
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"token expired","type":"invalid_request_error"}}`)
			return
		}
		writeSSE(w, solStream)
	}

	before := readAccessToken(t, env.authPath)

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the refresh-and-retry", resp.StatusCode)
	}
	frames := readFrames(t, resp.Body)
	if len(frames) == 0 || frames[0].event != "message_start" {
		t.Fatalf("frames = %v, want a real stream after the retry", eventNames(frames))
	}
	if frames[len(frames)-1].event != "message_stop" {
		t.Errorf("last event = %q, want message_stop", frames[len(frames)-1].event)
	}

	if n := env.codex.calls.Load(); n != 2 {
		t.Errorf("upstream /responses attempts = %d, want exactly 2 (original + one retry)", n)
	}
	if n := env.refreshes.Load(); n != 1 {
		t.Errorf("token refreshes = %d, want exactly 1", n)
	}

	tokens := env.codex.tokens()
	if len(tokens) == 2 && tokens[0] == tokens[1] {
		t.Error("the retry presented the same bearer token; the refresh was not applied")
	}
	after := readAccessToken(t, env.authPath)
	if after == before {
		t.Error("auth.json still holds the rejected access token; the rotation was not written back")
	}
}

// TestCodex429SurfacesRetryAfter asserts a rate-limited backend reaches the
// client as a real 429 with the upstream's own Retry-After and quota headers,
// and that utraque does not retry it or fall back to the Anthropic leg.
func TestCodex429SurfacesRetryAfter(t *testing.T) {
	env := newCodexEnv(t, nil)
	env.codex.respond = func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Retry-After", "42")
		h.Set("x-codex-primary-used-percent", "97.5")
		h.Set("x-codex-primary-reset-after-seconds", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"usage limit reached","type":"rate_limit_error"}}`)
	}

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q, want the upstream's 42 forwarded verbatim", got)
	}
	if got := resp.Header.Get("x-codex-primary-used-percent"); got != "97.5" {
		t.Errorf("x-codex-primary-used-percent = %q, want 97.5 forwarded", got)
	}
	if got := resp.Header.Get(leg.HeaderModel); got != "gpt-5.6-sol" {
		t.Errorf("%s = %q, want gpt-5.6-sol", leg.HeaderModel, got)
	}
	ev := decodeEnvelope(t, resp)
	if ev.Error.Type != string(apierr.TypeRateLimit) {
		t.Errorf("error type = %q, want %q", ev.Error.Type, apierr.TypeRateLimit)
	}
	if !strings.Contains(ev.Error.Message, "usage limit reached") {
		t.Errorf("message = %q, want the upstream's own message carried through", ev.Error.Message)
	}

	if n := env.codex.calls.Load(); n != 1 {
		t.Errorf("upstream attempts = %d, want exactly 1: a 429 is never retried here", n)
	}
	if n := env.anthropicHits.Load(); n != 0 {
		t.Errorf("the Anthropic upstream was contacted %d times, want 0: a codex 429 must fail cleanly", n)
	}
}

// TestCodexMidStreamFailureEmitsErrorEvent covers failure mode 2: output had
// already started, so the stream closes its block, emits an error event, and
// STOPS. A faked message_stop would present a broken answer as a finished one.
func TestCodexMidStreamFailureEmitsErrorEvent(t *testing.T) {
	env := newCodexEnv(t, nil)
	env.codex.respond = func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64) {
		writeSSE(w, `event: response.created
data: {"type":"response.created","response":{"id":"resp_bad"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"part":{"type":"output_text"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"half an ans"}

event: response.failed
data: {"type":"response.failed","response":{"id":"resp_bad","status":"failed","error":{"message":"the model gave up"}}}

`)
	}

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the failure happened after the stream opened)", resp.StatusCode)
	}

	names := eventNames(readFrames(t, resp.Body))
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "error"}
	if !equalStrings(names, want) {
		t.Fatalf("event sequence = %v, want %v", names, want)
	}
}

// TestCodexNonStreamingMidStreamFailureIsAnHTTPError is the same upstream
// failure seen by a non-streaming client, which has no way to notice a
// truncation: it must be a real HTTP error, never a short answer.
func TestCodexNonStreamingMidStreamFailureIsAnHTTPError(t *testing.T) {
	env := newCodexEnv(t, nil)
	env.codex.respond = func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64) {
		writeSSE(w, `event: response.created
data: {"type":"response.created","response":{"id":"resp_bad"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"part":{"type":"output_text"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"half an ans"}

event: response.failed
data: {"type":"response.failed","response":{"id":"resp_bad","status":"failed","error":{"message":"the model gave up"}}}

`)
	}

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200; a truncated fold must never be presented as a finished message")
	}
	ev := decodeEnvelope(t, resp)
	if ev.Type != "error" {
		t.Errorf("envelope type = %q, want error", ev.Type)
	}
	if !strings.Contains(ev.Error.Message, "the model gave up") {
		t.Errorf("message = %q, want the upstream's failure reason", ev.Error.Message)
	}
}

// TestCodexEmptyStreamRendersRealStatus covers failure mode 1 arriving through
// a 200: the backend opened a stream and said nothing. Because the client's own
// status line is written lazily, utraque can still answer with a real status
// instead of an empty, well-formed, useless stream.
func TestCodexEmptyStreamRendersRealStatus(t *testing.T) {
	env := newCodexEnv(t, nil)
	env.codex.respond = func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (an error envelope, not a stream)", got)
	}
	ev := decodeEnvelope(t, resp)
	if !strings.Contains(ev.Error.Message, "no events") {
		t.Errorf("message = %q, want it to say the backend sent no events", ev.Error.Message)
	}
}

// TestCodexClientInterruptCancelsUpstream covers failure mode 3: the caller
// hangs up mid-stream and the upstream request is cancelled rather than left
// running (and billing) for nobody.
func TestCodexClientInterruptCancelsUpstream(t *testing.T) {
	env := newCodexEnv(t, nil)
	upstreamCanceled := make(chan struct{})
	env.codex.respond = func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"part\":{\"type\":\"output_text\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"tick\"}\n\n")
		_ = rc.Flush()
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(10 * time.Second):
			t.Error("the upstream request was never cancelled after the client hung up")
			close(upstreamCanceled)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.front.URL+"/v1/messages",
		strings.NewReader(`{"model":"sol","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("do request: %v", err)
	}

	// Read far enough to prove bytes are flowing, then hang up.
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		cancel()
		t.Fatalf("reading the first bytes: %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	select {
	case <-upstreamCanceled:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream request outlived the client")
	}
}

// TestCodexUnknownModelStill404s guards the router's contract now that a real
// codex leg exists: an unroutable name must still be rejected locally, not
// guessed at against either backend.
func TestCodexUnknownModelStill404s(t *testing.T) {
	env := newCodexEnv(t, nil)

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"banana-7","max_tokens":8,"messages":[]}`, nil)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	ev := decodeEnvelope(t, resp)
	if ev.Error.Type != string(apierr.TypeNotFound) {
		t.Errorf("error type = %q, want %q", ev.Error.Type, apierr.TypeNotFound)
	}
	if n := env.codex.calls.Load(); n != 0 {
		t.Errorf("the codex backend was contacted %d times for an unknown model, want 0", n)
	}
	if n := env.anthropicHits.Load(); n != 0 {
		t.Errorf("the Anthropic upstream was contacted %d times for an unknown model, want 0", n)
	}
}

// TestModelsServesTheMergedCatalog asserts /v1/models is answered locally by
// discovery and that every id it emits routes back through the router. An id
// the picker offers but the proxy cannot resolve is a dead entry.
func TestModelsServesTheMergedCatalog(t *testing.T) {
	env := newCodexEnv(t, nil)

	resp, err := noRedirectClient().Get(env.front.URL + "/v1/models?limit=1000")
	if err != nil {
		t.Fatalf("get /v1/models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(body.Data) == 0 {
		t.Fatal("the merged catalog is empty")
	}

	var sawClaude, sawCodex bool
	for _, m := range body.Data {
		if m.ID == "" || m.DisplayName == "" {
			t.Errorf("row %+v is missing the two fields the client reads", m)
		}
		// The client discards anything failing /(claude|anthropic)/i, so a row
		// that fails it is pure noise.
		lower := strings.ToLower(m.ID)
		if !strings.Contains(lower, "claude") && !strings.Contains(lower, "anthropic") {
			t.Errorf("row id %q would be discarded by the client's own filter", m.ID)
		}
		if strings.HasPrefix(lower, "anthropic-compat.") {
			sawCodex = true
		} else if strings.Contains(lower, "claude") {
			sawClaude = true
		}
		// The invariant that makes a picked row work.
		dec, err := router.Resolve(m.ID, "")
		if err != nil {
			t.Errorf("advertised id %q does not route: %v", m.ID, err)
			continue
		}
		if !dec.Backend.Valid() {
			t.Errorf("advertised id %q resolved to an invalid backend %q", m.ID, dec.Backend)
		}
	}
	if !sawClaude {
		t.Error("no Claude rows in the merged catalog")
	}
	if !sawCodex {
		t.Error("no anthropic-compat.* Codex rows in the merged catalog")
	}

	// A picker open must not have spent an inference request or rotated a token.
	if n := env.codex.calls.Load(); n != 0 {
		t.Errorf("the /responses endpoint was called %d times for a picker open, want 0", n)
	}
	if n := env.refreshes.Load(); n != 0 {
		t.Errorf("the token endpoint was called %d times for a picker open, want 0", n)
	}
}

// TestCountTokensOnBothLegs asserts the estimator answers for a codex-routed
// model without touching either upstream, while a Claude model still passes
// through to Anthropic unchanged.
func TestCountTokensOnBothLegs(t *testing.T) {
	env := newCodexEnv(t, nil)

	t.Run("codex is estimated locally", func(t *testing.T) {
		resp := post(t, env.front.URL+"/v1/messages/count_tokens",
			`{"model":"sol","messages":[{"role":"user","content":"count these tokens please"}]}`, nil)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get(leg.HeaderRoute); got != "codex" {
			t.Errorf("%s = %q, want codex", leg.HeaderRoute, got)
		}
		var out struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode count: %v", err)
		}
		if out.InputTokens <= 0 {
			t.Errorf("input_tokens = %d, want a positive estimate", out.InputTokens)
		}
		if n := env.codex.calls.Load(); n != 0 {
			t.Errorf("counting tokens spent %d upstream inference requests, want 0", n)
		}
	})

	t.Run("claude still passes through", func(t *testing.T) {
		before := env.anthropicHits.Load()
		resp := post(t, env.front.URL+"/v1/messages/count_tokens",
			`{"model":"claude-haiku-3-5","messages":[{"role":"user","content":"hi"}]}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get(leg.HeaderRoute); got != "" {
			t.Errorf("%s = %q, want it absent on the Anthropic leg", leg.HeaderRoute, got)
		}
		if env.anthropicHits.Load() != before+1 {
			t.Error("the claude count_tokens request did not reach the Anthropic upstream")
		}
	})
}

// TestClaudeLegUnchangedWithCodexWired re-asserts the byte-faithful passthrough
// with the Codex leg live alongside it. The Anthropic half is the officially
// sanctioned one; wiring GPT models in must not have disturbed a single byte of
// it.
func TestClaudeLegUnchangedWithCodexWired(t *testing.T) {
	const reqBody = `{"model":"claude-sonnet-4-5-20250929","max_tokens":16,` +
		`"messages":[{"role":"user","content":"hi"}],"service_tier":"auto"}`

	var (
		gotBody   []byte
		gotHeader http.Header
		gotPath   string
	)
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotHeader, gotPath = b, r.Header.Clone(), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_01","type":"message"}`)
	}))
	defer anthropic.Close()

	env := newCodexEnv(t, func(c *config.Config) { c.Anthropic.BaseURL = anthropic.URL })

	resp := post(t, env.front.URL+"/v1/messages", reqBody, func(h http.Header) {
		h.Set("Authorization", "Bearer oauth-max-subscription-token")
		h.Add("anthropic-beta", "oauth-2025-04-20")
		h.Add("anthropic-beta", "context-1m-2025-08-07")
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	if got := string(gotBody); got != reqBody {
		t.Errorf("upstream body not byte-identical:\n got %q\nwant %q", got, reqBody)
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer oauth-max-subscription-token" {
		t.Errorf("upstream Authorization = %q, want the caller's bearer token verbatim", got)
	}
	if got := gotHeader.Values("Anthropic-Beta"); len(got) != 2 ||
		got[0] != "oauth-2025-04-20" || got[1] != "context-1m-2025-08-07" {
		t.Errorf("upstream anthropic-beta = %q, want two separate values preserved", got)
	}
	// The Codex leg must leave no trace on an Anthropic-routed request.
	if got := resp.Header.Get(leg.HeaderRoute); got != "" {
		t.Errorf("%s = %q, want it absent on the Anthropic leg", leg.HeaderRoute, got)
	}
	if n := env.codex.calls.Load(); n != 0 {
		t.Errorf("the codex backend was contacted %d times for a claude request, want 0", n)
	}
	// A codex credential must never be sent to Anthropic.
	if _, ok := gotHeader["Chatgpt-Account-Id"]; ok {
		t.Error("a codex account header reached the Anthropic upstream")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// restoreRegistry snapshots the process-wide alias registry and puts the static
// seed back afterwards, so a test that loads a live catalog into it cannot leak
// that state into the next one.
func restoreRegistry(t *testing.T) {
	t.Helper()
	t.Cleanup(router.DefaultRegistry.LoadStatic)
}

// TestLiveCatalogRepublishesTheRouterAliases is the wiring the alias contract
// rests on. Without it the registry stays on the compiled-in static seed for
// the life of the process: a retired slug keeps resolving to a model the
// backend no longer serves, and a codename OpenAI shipped this morning never
// resolves at all — while /healthz and the leg's own effort clamping, which do
// read the live catalog, show the new one.
func TestLiveCatalogRepublishesTheRouterAliases(t *testing.T) {
	restoreRegistry(t)

	// The static seed says sol is gpt-5.6-sol. This backend has retired that and
	// serves gpt-5.7-sol instead.
	const rolled = `{"models":[
	  {"slug":"gpt-5.7-sol","display_name":"GPT-5.7-Sol","visibility":"list","priority":10,
	   "default_reasoning_level":"low",
	   "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}
	]}`

	env := newCodexEnv(t, nil)
	env.codex.catalogBody = rolled

	if dec, err := router.Resolve("sol", ""); err != nil || dec.UpstreamModel != "gpt-5.6-sol" {
		t.Fatalf("precondition: sol = %+v/%v, want the static seed gpt-5.6-sol", dec, err)
	}

	// A picker open is one of the two paths that hold a live catalog.
	resp, err := noRedirectClient().Get(env.front.URL + "/v1/models?limit=1000")
	if err != nil {
		t.Fatalf("get /v1/models: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	dec, err := router.Resolve("sol", "")
	if err != nil {
		t.Fatalf(`Resolve("sol") after the catalog read: %v`, err)
	}
	if dec.UpstreamModel != "gpt-5.7-sol" {
		t.Errorf("sol = %q, want the live catalog's gpt-5.7-sol", dec.UpstreamModel)
	}
	// The retired slug is no longer a routable alias, so nothing silently sends
	// a request to a model the backend has dropped.
	if _, err := router.Resolve("sol-5.6", ""); err == nil {
		t.Error(`"sol-5.6" still resolves after the catalog retired gpt-5.6-sol`)
	}

	// And an inference request goes to the slug the live catalog named.
	post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(env.codex.lastBody(t)), &sent); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	if sent.Model != "gpt-5.7-sol" {
		t.Errorf("upstream model = %q, want gpt-5.7-sol", sent.Model)
	}
}

// TestAliasOverrideFromConfigMakesAnIrregularSlugRoutable exercises the
// routing.alias_overrides escape hatch: a slug the grammar cannot parse becomes
// routable from configuration alone, with no new build.
func TestAliasOverrideFromConfigMakesAnIrregularSlugRoutable(t *testing.T) {
	restoreRegistry(t)

	const irregular = `{"models":[
	  {"slug":"gpt-5.8-codex-ember","display_name":"Ember","visibility":"list","priority":9,
	   "supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}
	]}`

	env := newCodexEnv(t, func(c *config.Config) {
		c.Routing.AliasOverrides = []config.AliasOverride{
			{Slug: "gpt-5.8-codex-ember", Codename: "ember", Version: "5.8"},
		}
	})
	env.codex.catalogBody = irregular

	resp, err := noRedirectClient().Get(env.front.URL + "/v1/models?limit=1000")
	if err != nil {
		t.Fatalf("get /v1/models: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	for _, name := range []string{"ember", "ember-5.8", "ember-high"} {
		dec, err := router.Resolve(name, "")
		if err != nil {
			t.Errorf("Resolve(%q): %v", name, err)
			continue
		}
		if dec.UpstreamModel != "gpt-5.8-codex-ember" {
			t.Errorf("Resolve(%q) upstream = %q, want gpt-5.8-codex-ember", name, dec.UpstreamModel)
		}
	}
}

// TestHealthzReportsQuotaAndProtocolDrift covers the two reporting gaps: the
// usage-window headers the backend sends on EVERY answer (previously visible
// only on a failure), and the unknown-event counters that are the early warning
// for upstream protocol drift.
func TestHealthzReportsQuotaAndProtocolDrift(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, nil)
	env.codex.respond = func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64) {
		h := w.Header()
		h.Set("x-codex-primary-used-percent", "42.5")
		h.Set("x-codex-primary-window-minutes", "300")
		h.Set("x-codex-primary-reset-after-seconds", "900")
		writeSSE(w, `event: response.created
data: {"type":"response.created","response":{"id":"resp_drift"}}

event: response.something_new
data: {"type":"response.something_new"}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"part":{"type":"output_text"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"ok"}

event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"text":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_drift","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}

`)
	}

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	hresp, err := noRedirectClient().Get(env.front.URL + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer hresp.Body.Close()

	var health struct {
		CodexQuota struct {
			Primary struct {
				UsedPercent   float64 `json:"used_percent"`
				WindowMinutes int     `json:"window_minutes"`
				ResetAfterS   float64 `json:"reset_after_s"`
			} `json:"primary"`
		} `json:"codex_quota"`
		CodexStream struct {
			UnknownEvents     int            `json:"unknown_events"`
			UnknownEventTypes map[string]int `json:"unknown_event_types"`
		} `json:"codex_stream"`
		CodexRouting struct {
			Families []string `json:"families"`
		} `json:"codex_routing"`
	}
	if err := json.NewDecoder(hresp.Body).Decode(&health); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}

	if health.CodexQuota.Primary.UsedPercent != 42.5 {
		t.Errorf("codex_quota.primary.used_percent = %v, want 42.5", health.CodexQuota.Primary.UsedPercent)
	}
	if health.CodexQuota.Primary.WindowMinutes != 300 {
		t.Errorf("codex_quota.primary.window_minutes = %d, want 300", health.CodexQuota.Primary.WindowMinutes)
	}
	if health.CodexQuota.Primary.ResetAfterS != 900 {
		t.Errorf("codex_quota.primary.reset_after_s = %v, want 900", health.CodexQuota.Primary.ResetAfterS)
	}
	if health.CodexStream.UnknownEvents != 1 {
		t.Errorf("codex_stream.unknown_events = %d, want 1", health.CodexStream.UnknownEvents)
	}
	if n := health.CodexStream.UnknownEventTypes["response.something_new"]; n != 1 {
		t.Errorf("unknown_event_types[response.something_new] = %d, want 1", n)
	}
	if len(health.CodexRouting.Families) == 0 {
		t.Error("codex_routing.families is empty; the router advertises no route families")
	}
}
