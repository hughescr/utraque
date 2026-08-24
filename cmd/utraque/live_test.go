//go:build live

// This file is EXCLUDED from every default test run. It is the only test in
// this package that contacts a real host, spends real subscription quota, and
// reads the real Codex credential file. Run it deliberately:
//
//	go test -tags live -run TestLiveContract ./cmd/utraque/...
//
// The build tag is the gate, not the name: a default run cannot compile this
// file at all. (One HERMETIC test in this package is called
// TestLiveCatalogRepublishesTheRouterAliases — "live" there means live catalog
// DATA off a fake backend — which is why the two live cases here carry the
// distinct TestLiveContract prefix.)
//
// What it is for: the Codex Responses backend is undocumented and adds stream
// event types without notice. The translator handles an unknown type by
// counting it and moving on — the right behaviour for a live proxy, and a
// silent one. This is the tripwire that makes it loud. One real request per leg,
// and the event types the REAL stream carried are compared against the
// translator's own mapping table (stream.HandledEventTypes). Anything the
// backend has started sending that utraque does not understand fails the test by
// name.
//
// Both legs need a credential, and neither is invented here:
//
//   - Codex: the real ~/.codex/auth.json (or CODEX_HOME / UTRAQUE_CODEX_AUTH_FILE).
//     Run `codex login` first. A refresh may rewrite that file, exactly as the
//     daemon would.
//   - Anthropic: whatever you put in UTRAQUE_LIVE_ANTHROPIC_TOKEN (sent as a
//     bearer token, the shape a Claude Code subscription session uses) or
//     ANTHROPIC_API_KEY (sent as x-api-key). With neither set, the Anthropic
//     case skips rather than pretending to have tested the passthrough.
//
// Everything else stays out of the user's way: the catalog cache is redirected
// into t.TempDir(), and the trace dump this test reads its evidence from is
// written there too.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/server"
	"github.com/hughescr/utraque/internal/sse"
	"github.com/hughescr/utraque/internal/translate/stream"
)

// Live knobs. Each has a working default; they exist so the test survives a
// model being renamed upstream without needing a new build.
const (
	envLiveCodexModel     = "UTRAQUE_LIVE_CODEX_MODEL"
	envLiveAnthropicModel = "UTRAQUE_LIVE_ANTHROPIC_MODEL"
	envLiveAnthropicToken = "UTRAQUE_LIVE_ANTHROPIC_TOKEN"
	envAnthropicAPIKey    = "ANTHROPIC_API_KEY"

	defaultLiveCodexModel     = "sol"
	defaultLiveAnthropicModel = "claude-sonnet-5"

	// livePrompt is deliberately tiny: this test spends real subscription
	// quota, and one word of output is enough to prove the contract.
	livePrompt = "Reply with the single word: pong."

	// liveTimeout bounds one live request end to end.
	liveTimeout = 120 * time.Second
)

// TestLiveContractCodexStreamEventTypes is the upstream-drift tripwire.
//
// It sends one real `sol` request through the whole production wiring, captures
// the RAW upstream SSE via the trace dump, and asserts that the set of event
// types the backend actually sent is a subset of the translator's mapping
// table. It also cross-checks /healthz's own drift counters, which are fed by
// the translator's default branch — two independent readings of the same fact,
// so a bug in either one cannot hide a real drift.
func TestLiveContractCodexStreamEventTypes(t *testing.T) {
	env := newLiveEnv(t)
	env.requireCodexCredential(t)

	model := envOr(envLiveCodexModel, defaultLiveCodexModel)
	body := fmt.Sprintf(
		`{"model":%q,"max_tokens":64,"stream":true,"messages":[{"role":"user","content":%q}]}`,
		model, livePrompt)

	resp := env.post(t, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live %s request: status = %d, want 200; body: %s",
			model, resp.StatusCode, readAll(t, resp))
	}
	frames := readFrames(t, resp.Body)
	assertAnthropicSSESequence(t, model, frames)

	// The raw upstream stream, byte for byte, as the trace recorded it.
	upstream := waitForUpstreamTrace(t, env.traceDir)
	observed := codexEventTypes(t, upstream)
	if len(observed) == 0 {
		t.Fatalf("the upstream trace %s carried no typed events at all", upstream)
	}
	t.Logf("live %s stream carried %d distinct upstream event types: %s",
		model, len(observed), strings.Join(observed, " "))

	// The contract: observed ⊆ mapping table.
	var unknown []string
	for _, typ := range observed {
		if !stream.Handles(typ) {
			unknown = append(unknown, typ)
		}
	}
	if len(unknown) > 0 {
		t.Errorf("THE CODEX STREAM PROTOCOL HAS DRIFTED: the backend sent %d event type(s) "+
			"the translator does not map: %s\n"+
			"Every one of these is currently being DROPPED from the translated answer. "+
			"Add a case for each to Translator.handle and list it in handledEventTypes "+
			"(internal/translate/stream/translator.go), or confirm it is safe to ignore and "+
			"list it anyway so this test stays meaningful.\n"+
			"The translator understands: %s",
			len(unknown), strings.Join(unknown, " "), strings.Join(stream.HandledEventTypes(), " "))
	}

	// A stream that never reached a terminus would make the subset check
	// vacuous — half a conversation cannot show drift in the half it missed.
	for _, want := range []string{cschema.EventResponseCreated, cschema.EventResponseCompleted} {
		if !slices.Contains(observed, want) {
			t.Errorf("the live stream never carried %q, so it was not a complete response; "+
				"observed: %s", want, strings.Join(observed, " "))
		}
	}

	// The same fact read off the production counter rather than the trace.
	total, byType := env.unknownEventCounters(t)
	if total != 0 || len(byType) != 0 {
		t.Errorf("/healthz reports %d unrecognised upstream event(s) %v; "+
			"the translator dropped them silently", total, byType)
	}
}

// TestLiveContractAnthropicPassthrough is the other leg: one real request to
// api.anthropic.com, relayed byte-for-byte on the caller's own credential, must
// come back as a well-formed Anthropic SSE stream. It proves the passthrough
// still passes through — the half of the proxy that has no translation to drift.
func TestLiveContractAnthropicPassthrough(t *testing.T) {
	env := newLiveEnv(t)
	auth := anthropicCredential(t)

	model := envOr(envLiveAnthropicModel, defaultLiveAnthropicModel)
	body := fmt.Sprintf(
		`{"model":%q,"max_tokens":64,"stream":true,"messages":[{"role":"user","content":%q}]}`,
		model, livePrompt)

	resp := env.post(t, body, func(h http.Header) {
		h.Set("anthropic-version", "2023-06-01")
		auth(h)
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live %s request: status = %d, want 200; body: %s",
			model, resp.StatusCode, readAll(t, resp))
	}
	assertAnthropicSSESequence(t, model, readFrames(t, resp.Body))
}

// --- harness --------------------------------------------------------------

// liveEnv is the production wiring, pointed at the real backends, with the
// catalog cache and the trace dump redirected into t.TempDir().
type liveEnv struct {
	cfg      config.Config
	front    *httptest.Server
	traceDir string
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()

	// The REAL environment: real chatgpt.com, real api.anthropic.com, real
	// credential path. That is the point of this file.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// The one deviation: keep utraque's own catalog cache out of the user's
	// cache directory, so a live run leaves nothing behind.
	cfg.Codex.CachePath = filepath.Join(t.TempDir(), "models_cache.json")

	traceDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tracer, err := obs.NewTracer(traceDir, log)
	if err != nil {
		t.Fatalf("obs.NewTracer: %v", err)
	}

	a, err := newApp(cfg, log, nil, tracer)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	// No http.TimeoutHandler around the proxy: it buffers the whole body and
	// supports neither Flusher nor Hijacker, which would turn the incremental
	// translation this test exists to observe back into one lump. The deadline
	// belongs on the client side, in post.
	front := httptest.NewServer(a.srv)
	t.Cleanup(front.Close)

	return &liveEnv{cfg: cfg, front: front, traceDir: traceDir}
}

// post sends one live /v1/messages request. It deliberately does NOT reuse the
// hermetic suite's post helper: that one caps a request at 10 seconds, which is
// right for a fake upstream and far too short for a real reasoning model.
func (e *liveEnv) post(t *testing.T, body string, set func(http.Header)) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), liveTimeout)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.front.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build live request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	e.setHeaders(req.Header)
	if set != nil {
		set(req.Header)
	}

	// No client Timeout: the context above bounds the whole exchange, and a
	// client timeout would also cut a long but healthy stream mid-answer.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("live request failed: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// setHeaders supplies the loopback shared secret when one is configured, so the
// test works on a machine set up the way the README recommends.
func (e *liveEnv) setHeaders(h http.Header) {
	if e.cfg.HasLocalToken() {
		h.Set(server.LocalTokenHeader, e.cfg.LocalToken)
	}
}

// requireCodexCredential fails loudly rather than skipping: running with the
// live tag is an explicit act, and a silent skip would report success for a
// tripwire that never fired.
func (e *liveEnv) requireCodexCredential(t *testing.T) {
	t.Helper()
	if e.cfg.Codex.AuthFile == "" {
		t.Fatal("no Codex credential path resolved; set CODEX_HOME or UTRAQUE_CODEX_AUTH_FILE")
	}
	if _, err := os.Stat(e.cfg.Codex.AuthFile); err != nil {
		t.Fatalf("the Codex credential file %s is unusable (%v); run `codex login` first",
			e.cfg.Codex.AuthFile, err)
	}
}

// unknownEventCounters reads /healthz's codex_stream block: the running count of
// upstream event types the translator did not recognise, and the per-type
// breakdown.
func (e *liveEnv) unknownEventCounters(t *testing.T) (int, map[string]int) {
	t.Helper()
	resp, err := http.Get(e.front.URL + server.HealthPath)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	var health struct {
		CodexStream struct {
			UnknownEvents int            `json:"unknown_events"`
			ByType        map[string]int `json:"unknown_event_types"`
		} `json:"codex_stream"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	return health.CodexStream.UnknownEvents, health.CodexStream.ByType
}

// anthropicCredential returns the header setter for the Anthropic leg, or skips
// the test when no credential was supplied. The proxy stores no Anthropic
// secret of its own, so the test has to bring one exactly as the client does.
func anthropicCredential(t *testing.T) func(http.Header) {
	t.Helper()
	if tok := strings.TrimSpace(os.Getenv(envLiveAnthropicToken)); tok != "" {
		return func(h http.Header) { h.Set("Authorization", "Bearer "+tok) }
	}
	if key := strings.TrimSpace(os.Getenv(envAnthropicAPIKey)); key != "" {
		return func(h http.Header) { h.Set("x-api-key", key) }
	}
	t.Skipf("no Anthropic credential: set %s (a subscription OAuth token, sent as a bearer) "+
		"or %s to exercise the passthrough leg", envLiveAnthropicToken, envAnthropicAPIKey)
	return nil
}

// waitForUpstreamTrace returns the path of the request trace's raw upstream SSE
// dump, waiting for the trace to be CLOSED first.
//
// The wait is not decorative: the client can finish reading the translated
// stream before the middleware's deferred close has flushed the dump. The
// manifest (<id>.request.json) is written last by Trace.Close, so its existence
// is the signal that the sibling .upstream.sse is complete.
func waitForUpstreamTrace(t *testing.T, dir string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		matches, err := filepath.Glob(filepath.Join(dir, "*"+obs.SuffixUpstream))
		if err != nil {
			t.Fatalf("globbing the trace directory: %v", err)
		}
		for _, m := range matches {
			manifest := strings.TrimSuffix(m, obs.SuffixUpstream) + obs.SuffixRequest
			if _, err := os.Stat(manifest); err == nil {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no completed upstream trace appeared in %s (found %v)", dir, matches)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// codexEventTypes reads the raw upstream dump and returns the distinct event
// types it carried, sorted. It prefers the JSON "type" field and falls back to
// the SSE event: line, which is the same precedence the translator applies.
func codexEventTypes(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open upstream trace: %v", err)
	}
	defer f.Close()

	seen := map[string]struct{}{}
	sc := sse.NewScanner(f)
	for sc.Scan() {
		fr := sc.Frame()
		typ := ""
		if len(fr.Data) > 0 {
			var ev cschema.StreamEvent
			if err := json.Unmarshal(fr.Data, &ev); err != nil {
				// A frame the translator could not parse either. Name it, so a
				// malformed upstream body is not mistaken for a clean stream.
				seen["<malformed-json>"] = struct{}{}
				continue
			}
			typ = ev.Type
		}
		if typ == "" {
			typ = fr.Event
		}
		if typ != "" {
			seen[typ] = struct{}{}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning the upstream trace: %v", err)
	}

	out := make([]string, 0, len(seen))
	for typ := range seen {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// assertAnthropicSSESequence checks the shape every Anthropic client depends on:
// message_start first, message_stop last, no error frame in between.
func assertAnthropicSSESequence(t *testing.T, model string, frames []frame) {
	t.Helper()
	if len(frames) == 0 {
		t.Fatalf("live %s request produced no SSE frames", model)
	}
	names := eventNames(frames)
	if names[0] != "message_start" {
		t.Errorf("live %s stream began with %q, want message_start; sequence: %v", model, names[0], names)
	}
	if last := names[len(names)-1]; last != "message_stop" {
		t.Errorf("live %s stream ended with %q, want message_stop; sequence: %v", model, last, names)
	}
	for i, f := range frames {
		if f.event == "error" {
			t.Fatalf("live %s stream carried an error frame at %d: %s", model, i, f.data)
		}
	}
	t.Logf("live %s stream: %v", model, names)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// readAll reads a bounded prefix of a failing response, for the error message.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return string(b)
}
