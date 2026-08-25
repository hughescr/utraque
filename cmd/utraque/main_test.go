package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/server"
)

// noRedirect is how a client must be configured to observe what utraque
// actually returned: following a 3xx here would hide whether the proxy
// followed it upstream.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
}

// frontDoor stands up the exact handler wiring main uses, pointed at a fake
// upstream instead of api.anthropic.com.
func frontDoor(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Anthropic.BaseURL = upstreamURL

	srv, err := newServer(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	return front
}

func post(t *testing.T, url string, body string, set func(http.Header)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if set != nil {
		set(req.Header)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// decodeEnvelope reads an Anthropic error envelope off a response.
func decodeEnvelope(t *testing.T, resp *http.Response) schema.ErrorEvent {
	t.Helper()
	var ev schema.ErrorEvent
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error body: %v", err)
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatalf("decode error envelope from %q: %v", body, err)
	}
	return ev
}

// TestClaudeMessagesForwardedVerbatim drives a claude-* request through main's
// wiring and asserts the fake upstream received the body byte-for-byte with
// the caller's Authorization and both anthropic-beta values intact. Those
// three properties are what keep the call billed to the caller's own Max
// subscription rather than a metered API key.
func TestClaudeMessagesForwardedVerbatim(t *testing.T) {
	const reqBody = `{"model":"claude-sonnet-4-5-20250929","max_tokens":16,` +
		`"messages":[{"role":"user","content":"hi"}],"service_tier":"auto"}`

	var (
		gotBody   []byte
		gotHeader http.Header
		gotMethod string
		gotPath   string
		gotQuery  string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read body: %v", err)
		}
		gotBody, gotHeader = b, r.Header.Clone()
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "42")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_01","type":"message"}`)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp := post(t, front.URL+"/v1/messages?beta=true", reqBody, func(h http.Header) {
		h.Set("Authorization", "Bearer oauth-max-subscription-token")
		h.Add("anthropic-beta", "oauth-2025-04-20")
		h.Add("anthropic-beta", "context-1m-2025-08-07")
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := string(gotBody); got != reqBody {
		t.Errorf("upstream body not byte-identical:\n got %q\nwant %q", got, reqBody)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotQuery != "beta=true" {
		t.Errorf("upstream query = %q, want beta=true", gotQuery)
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer oauth-max-subscription-token" {
		t.Errorf("upstream Authorization = %q, want the caller's bearer token verbatim", got)
	}
	// The two beta values must arrive as two distinct header values. Joining
	// them into one comma-separated line changes the OAuth capability signal.
	wantBetas := []string{"oauth-2025-04-20", "context-1m-2025-08-07"}
	gotBetas := gotHeader.Values("Anthropic-Beta")
	if len(gotBetas) != len(wantBetas) {
		t.Fatalf("upstream anthropic-beta = %q, want two separate values %q", gotBetas, wantBetas)
	}
	for i := range wantBetas {
		if gotBetas[i] != wantBetas[i] {
			t.Errorf("upstream anthropic-beta[%d] = %q, want %q", i, gotBetas[i], wantBetas[i])
		}
	}
	// Upstream response headers must reach the client so rate-limit accounting
	// stays visible.
	if got := resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining"); got != "42" {
		t.Errorf("relayed rate-limit header = %q, want 42", got)
	}
	if got := resp.Header.Get(server.ResponseIDHeader); got == "" {
		t.Errorf("missing %s on the response", server.ResponseIDHeader)
	}
}

// TestUpstreamRedirectIsNotFollowed pins the security property: following a
// 3xx would re-send the caller's Authorization bearer token to whatever host
// upstream named. The 302 must reach the client untouched and the redirect
// target must never be contacted.
func TestUpstreamRedirectIsNotFollowed(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp := post(t, front.URL+"/v1/messages",
		`{"model":"claude-opus-4-20250514","max_tokens":8,"messages":[]}`, nil)

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 relayed to the client", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Location"), target.URL+"/elsewhere"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target was contacted %d times, want 0", n)
	}
}

// TestSSEFlushesIncrementally proves the stream is relayed frame by frame
// rather than buffered: the first frame must reach the client while the
// upstream handler is still blocked mid-response.
func TestSSEFlushesIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstreamDone := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)

		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_ = rc.Flush()

		select {
		case <-release:
		case <-time.After(10 * time.Second):
			t.Error("test never released the upstream handler")
		}

		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		_ = rc.Flush()
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp := post(t, front.URL+"/v1/messages",
		`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	br := bufio.NewReader(resp.Body)
	frames := make(chan string, 1)
	readErr := make(chan error, 1)
	go func() {
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if err != nil {
				readErr <- err
				return
			}
			if line == "\n" {
				frames <- sb.String()
				return
			}
		}
	}()

	select {
	case frame := <-frames:
		if !strings.Contains(frame, "message_start") {
			t.Fatalf("first frame = %q, want the message_start frame", frame)
		}
	case err := <-readErr:
		t.Fatalf("reading the first frame: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first SSE frame never arrived while upstream was still blocked: the stream is being buffered")
	}

	close(release)
	<-upstreamDone

	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading the rest of the stream: %v", err)
	}
	if !strings.Contains(string(rest), "message_stop") {
		t.Errorf("remainder = %q, want the message_stop frame", rest)
	}
}

// TestUnknownModelReturns404Envelope asserts an unroutable model is rejected
// locally, in the Anthropic error shape, without touching either upstream.
func TestUnknownModelReturns404Envelope(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp := post(t, front.URL+"/v1/messages",
		`{"model":"banana-7","max_tokens":8,"messages":[]}`, nil)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	ev := decodeEnvelope(t, resp)
	if ev.Type != "error" {
		t.Errorf("envelope type = %q, want error", ev.Type)
	}
	if ev.Error.Type != string(apierr.TypeNotFound) {
		t.Errorf("error type = %q, want %q", ev.Error.Type, apierr.TypeNotFound)
	}
	if !strings.Contains(ev.Error.Message, "banana-7") {
		t.Errorf("message = %q, want it to name the rejected model", ev.Error.Message)
	}
	if !strings.Contains(ev.Error.Message, "claude-*") {
		t.Errorf("message = %q, want it to list the known route families", ev.Error.Message)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream was contacted %d times for an unknown model, want 0", n)
	}
}

// TestCodexModelWithoutCredentialReturns503 asserts a GPT alias resolves to the
// Codex backend and, with no `codex login` configured, gets a clear 503 rather
// than being silently forwarded to Anthropic — a cross-leg fallback would spend
// the wrong subscription.
func TestCodexModelWithoutCredentialReturns503(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp := post(t, front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	ev := decodeEnvelope(t, resp)
	if ev.Type != "error" {
		t.Errorf("envelope type = %q, want error", ev.Type)
	}
	if ev.Error.Type != string(apierr.TypeAPI) {
		t.Errorf("error type = %q, want %q", ev.Error.Type, apierr.TypeAPI)
	}
	if !strings.Contains(ev.Error.Message, "no Codex credential is configured") {
		t.Errorf("message = %q, want it to name the missing credential", ev.Error.Message)
	}
	if !strings.Contains(ev.Error.Message, "codex login") {
		t.Errorf("message = %q, want it to say how to fix the problem", ev.Error.Message)
	}
	// The error must report the resolved upstream slug, proving the alias
	// registry ran rather than the name being passed through raw.
	if !strings.Contains(ev.Error.Message, "gpt-5.6-sol") {
		t.Errorf("message = %q, want it to name the resolved upstream slug", ev.Error.Message)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("Anthropic upstream was contacted %d times for a Codex model, want 0", n)
	}
}

// TestCountTokensRoutesToPassthrough pins the second Anthropic route.
func TestCountTokensRoutesToPassthrough(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":7}`)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp := post(t, front.URL+"/v1/messages/count_tokens",
		`{"model":"claude-haiku-3-5","messages":[{"role":"user","content":"hi"}]}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q, want /v1/messages/count_tokens", gotPath)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"input_tokens":7`)) {
		t.Errorf("body = %q, want the upstream count relayed", body)
	}
}

// TestCatchAllRoutesToPassthrough asserts an unrecognised path is relayed
// upstream rather than 404'd locally: Claude Code reaches for assorted
// endpoints under the base URL and none of them may fail here.
//
// /v1/models is deliberately NOT the example any more — discovery answers that
// one locally now (see TestModelsServesTheMergedCatalog).
func TestCatchAllRoutesToPassthrough(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp, err := noRedirectClient().Get(front.URL + "/v1/organizations/me")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/v1/organizations/me" {
		t.Errorf("upstream path = %q, want /v1/organizations/me", gotPath)
	}
}

// TestHealthzIsServedLocally asserts /healthz never reaches upstream.
func TestHealthzIsServedLocally(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp, err := noRedirectClient().Get(front.URL + server.HealthPath)
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health["status"] != server.StatusOK {
		t.Errorf("status field = %v, want %q", health["status"], server.StatusOK)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream was contacted %d times for healthz, want 0", n)
	}
}

// TestMalformedBodyReturns400 asserts a body that is not a JSON object is
// rejected locally in the Anthropic error shape.
func TestMalformedBodyReturns400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be contacted for a malformed body")
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	resp := post(t, front.URL+"/v1/messages", `not json at all`, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	ev := decodeEnvelope(t, resp)
	if ev.Error.Type != string(apierr.TypeInvalidRequest) {
		t.Errorf("error type = %q, want %q", ev.Error.Type, apierr.TypeInvalidRequest)
	}
}

// TestNonCanonicalPathIsRelayedNotRedirected pins the catch-all invariant
// against http.ServeMux's own path canonicalization. "/v1//messages" must
// reach upstream exactly as written; answering it locally with a 3xx makes the
// client re-send a POST body against a route we invented.
func TestNonCanonicalPathIsRelayedNotRedirected(t *testing.T) {
	var gotRawPath atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath.Store(r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"relayed":true}`)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	for _, path := range []string{"/v1//messages", "/v1/./models", "/v1/a/../models"} {
		t.Run(path, func(t *testing.T) {
			gotRawPath.Store("")
			resp := post(t, front.URL+path,
				`{"model":"claude-opus-4-20250514","max_tokens":8,"messages":[]}`, nil)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (a local redirect means the mux answered it)", resp.StatusCode)
			}
			if got := gotRawPath.Load().(string); got != path {
				t.Errorf("upstream path = %q, want %q relayed unchanged", got, path)
			}
		})
	}
}

// TestCanonicalRoutesStillMatch guards the fix above from over-reaching: the
// real routes must still be served locally rather than falling to the
// catch-all.
func TestCanonicalRoutesStillMatch(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL)

	// A Codex model on the real /v1/messages route reaches the router, not the
	// Anthropic catch-all.
	resp := post(t, front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":8,"messages":[]}`, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the Codex leg's no-credential 503", resp.StatusCode)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream was contacted %d times, want 0", n)
	}
}

// TestLocalTokenIsNotForwardedThroughTheFrontDoor is the end-to-end form of
// the trust-boundary rule, driven through main's real wiring with the local
// shared secret enabled.
func TestLocalTokenIsNotForwardedThroughTheFrontDoor(t *testing.T) {
	const secret = "SUPER-SECRET-LOCAL-TOKEN"

	var gotToken atomic.Value
	gotToken.Store("")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken.Store(r.Header.Get(server.LocalTokenHeader))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Anthropic.BaseURL = upstream.URL
	cfg.LocalToken = secret

	srv, err := newServer(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	front := httptest.NewServer(srv)
	defer front.Close()

	resp := post(t, front.URL+"/v1/messages",
		`{"model":"claude-opus-4-20250514","max_tokens":8,"messages":[]}`,
		func(h http.Header) { h.Set(server.LocalTokenHeader, secret) })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the token should have authenticated us)", resp.StatusCode)
	}
	if got := gotToken.Load().(string); got != "" {
		t.Errorf("upstream received %s = %q; the local credential must stop at the proxy",
			server.LocalTokenHeader, got)
	}
}

// TestIdleSelfExitIsOffByDefault: nothing restarts utraque in this build, so a
// default self-exit would leave the next request with a connection refused.
func TestIdleSelfExitIsOffByDefault(t *testing.T) {
	if d := config.Default().Idle.Timeout; d != 0 {
		t.Errorf("default idle timeout = %s, want 0 until socket activation lands", d)
	}
}

// jwtWithExp builds an unsigned JWT whose payload carries the given exp claim.
// Only the payload segment matters to utraque's expiry decode (no signature is
// verified), so a placeholder header and signature are fine.
func jwtWithExp(exp time.Time) string {
	payload := fmt.Sprintf(`{"exp":%d}`, exp.Unix())
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + ".sig"
}

// TestHealthzReportsCodexAuthAndCatalog drives /healthz through main's real
// wiring with a fake Codex auth.json and a pre-populated (non-CLI) catalog
// cache, and asserts the codex_auth and codex_catalog fields reflect the real
// state without ever revealing a token or touching the network.
func TestHealthzReportsCodexAuthAndCatalog(t *testing.T) {
	dir := t.TempDir()

	// A fake auth.json whose access token (a JWT) expires comfortably in the
	// future, so the reported state is "ok" and expires_in_s is a positive whole
	// number. The JWT string is the secret we assert never leaks into /healthz.
	accessToken := jwtWithExp(time.Now().Add(2 * time.Hour))
	authPath := filepath.Join(dir, "auth.json")
	authBody := fmt.Sprintf(`{"tokens":{"account_id":"acct-123","access_token":%q,"refresh_token":"r"}}`,
		accessToken)
	if err := os.WriteFile(authPath, []byte(authBody), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	// A catalog cache utraque owns (never the CLI's), holding two models and a
	// recent fetch time so a snapshot reports a non-zero count and a small age.
	cachePath := filepath.Join(dir, "utraque", "models_cache.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	cacheBody := fmt.Sprintf(`{"fetched_at":%q,"models":[`+
		`{"slug":"gpt-5.6-sol","visibility":"list"},`+
		`{"slug":"gpt-5.5","visibility":"list"}]}`,
		time.Now().Add(-30*time.Second).Format(time.RFC3339))
	if err := os.WriteFile(cachePath, []byte(cacheBody), 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be contacted for /healthz")
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Anthropic.BaseURL = upstream.URL
	cfg.Codex.AuthFile = authPath
	cfg.Codex.CachePath = cachePath

	srv, err := newServer(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	front := httptest.NewServer(srv)
	defer front.Close()

	resp, err := noRedirectClient().Get(front.URL + server.HealthPath)
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if bytes.Contains(body, []byte(accessToken)) {
		t.Fatalf("healthz body leaked the access token: %s", body)
	}

	var health struct {
		Status    string `json:"status"`
		CodexAuth struct {
			Status    string `json:"status"`
			ExpiresIn int64  `json:"expires_in_s"`
		} `json:"codex_auth"`
		CodexCatalog struct {
			Models int      `json:"models"`
			AgeS   *float64 `json:"age_s"`
		} `json:"codex_catalog"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode health %q: %v", body, err)
	}

	if health.Status != server.StatusOK {
		t.Errorf("status = %q, want %q", health.Status, server.StatusOK)
	}
	if health.CodexAuth.Status != "ok" {
		t.Errorf("codex_auth.status = %q, want ok", health.CodexAuth.Status)
	}
	if health.CodexAuth.ExpiresIn <= 0 {
		t.Errorf("codex_auth.expires_in_s = %d, want a positive whole number", health.CodexAuth.ExpiresIn)
	}
	if health.CodexCatalog.Models != 2 {
		t.Errorf("codex_catalog.models = %d, want 2", health.CodexCatalog.Models)
	}
	if health.CodexCatalog.AgeS == nil || *health.CodexCatalog.AgeS < 0 {
		t.Errorf("codex_catalog.age_s = %v, want a non-negative age", health.CodexCatalog.AgeS)
	}
}

// TestHealthzReportsMissingCodexAuthWithoutAuthFile asserts that with no
// credential file configured (a bare config.Default, as tests use), /healthz
// reports codex auth "missing" and a zero-model catalog rather than erroring.
func TestHealthzReportsMissingCodexAuthWithoutAuthFile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be contacted for /healthz")
	}))
	defer upstream.Close()

	front := frontDoor(t, upstream.URL) // config.Default: AuthFile and CachePath empty

	resp, err := noRedirectClient().Get(front.URL + server.HealthPath)
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()

	var health struct {
		CodexAuth struct {
			Status    string `json:"status"`
			ExpiresIn *int64 `json:"expires_in_s"`
		} `json:"codex_auth"`
		CodexCatalog struct {
			Models int `json:"models"`
		} `json:"codex_catalog"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.CodexAuth.Status != "missing" {
		t.Errorf("codex_auth.status = %q, want missing", health.CodexAuth.Status)
	}
	if health.CodexAuth.ExpiresIn != nil {
		t.Errorf("codex_auth.expires_in_s = %v, want omitted when missing", *health.CodexAuth.ExpiresIn)
	}
	if health.CodexCatalog.Models != 0 {
		t.Errorf("codex_catalog.models = %d, want 0", health.CodexCatalog.Models)
	}
}

// TestMidStreamFailureAbortsTheConnectionAndStillLogs covers the same
// truncation contract as the passthrough leg's own test, but through the real
// dispatcher and the full middleware chain — which is where the two risks of
// answering with a panic live.
//
// The first is the client-visible one: a body cut short must reach the caller
// as an unfinished transfer, not as a complete one. The second is ours: the
// abort unwinds through every middleware, so the access-log line — written from
// a defer in withObserve, above the recover middleware that re-panics
// http.ErrAbortHandler — must still be emitted. A request that vanishes from
// the log is how a proxy stops being diagnosable.
func TestMidStreamFailureAbortsTheConnectionAndStillLogs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_ = rc.Flush()
		// The upstream link dies with the body half-sent.
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()

	logs := &syncBuffer{}
	log, err := obs.NewLogger(logs, slog.LevelInfo, "json")
	if err != nil {
		t.Fatalf("obs.NewLogger: %v", err)
	}
	cfg := config.Default()
	cfg.Anthropic.BaseURL = upstream.URL
	srv, err := newServer(cfg, log, nil)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	front := httptest.NewServer(srv)
	defer front.Close()

	resp := post(t, front.URL+"/v1/messages",
		`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the failure must arrive after the headers", resp.StatusCode)
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Fatalf("read the body to a clean EOF (%q); a truncated body must not be reported as complete", body)
	}
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("reading the body = %v, want io.ErrUnexpectedEOF from the aborted transfer", readErr)
	}

	// The access-log line is written as the panic unwinds, which can land after
	// the client's read returns. Poll rather than assume an ordering.
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for {
		got = logs.String()
		if strings.Contains(got, `"msg":"request"`) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(got, `"msg":"request"`) {
		t.Fatalf("no access-log line after an aborted response; log was:\n%s", got)
	}
	if !strings.Contains(got, "aborting the connection") {
		t.Errorf("log does not say the connection was aborted; log was:\n%s", got)
	}
	if strings.Contains(got, "panic recovered") {
		t.Errorf("the abort was logged as a panic; http.ErrAbortHandler must pass through untouched:\n%s", got)
	}
}
