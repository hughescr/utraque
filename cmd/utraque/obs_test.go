package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/obs"
)

// This file drives the observability surface end to end through main's real
// wiring, against the same fake upstreams the rest of the package uses.
//
// SAFETY: as in codex_test.go, every upstream is an httptest server and every
// credential is a throwaway JWT under t.TempDir(). The real chatgpt.com, the
// real auth.openai.com and the real ~/.codex/auth.json are never touched.

// tokenShapedMaterial is everything a leak test must never find in the log. The
// values are deliberately realistic — a JWT, an sk- key, a bearer token — since
// a test using "SECRET123" would pass against a redactor that only matched
// literal secrets.
type tokenShapedMaterial struct {
	accessToken  string
	refreshToken string
	clientBearer string
	apiKey       string
	accountID    string
}

func (m tokenShapedMaterial) all() []string {
	return []string{m.accessToken, m.refreshToken, m.clientBearer, m.apiKey, m.accountID}
}

// logLines splits the captured JSON log into records.
func logLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// requestLines returns the one-per-request access lines.
func requestLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range logLines(t, raw) {
		if rec["msg"] == "request" {
			out = append(out, rec)
		}
	}
	return out
}

// TestRequestLineCarriesEveryField is the shape half of the contract: one INFO
// line per request, carrying everything needed to explain that request without
// a second line to correlate against.
func TestRequestLineCarriesEveryField(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnvOpts(t, nil, codexEnvOptions{CaptureLogs: true, Level: slog.LevelInfo})

	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol-high","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	lines := requestLines(t, env.logs.String())
	if len(lines) != 1 {
		t.Fatalf("want exactly one request line, got %d:\n%s", len(lines), env.logs.String())
	}
	rec := lines[0]

	for _, key := range []string{
		"request_id", "method", "path", "status", "req_bytes", "ttfb_ms", "total_ms",
		"route", "client_model", "upstream_model", "effort", "stream",
		"upstream_status", "output_tokens", "stop_reason", "interrupted", "transport",
	} {
		if _, ok := rec[key]; !ok {
			t.Errorf("the request line is missing %q:\n%v", key, rec)
		}
	}
	for key, want := range map[string]any{
		"method":          "POST",
		"path":            "/v1/messages",
		"route":           "codex",
		"client_model":    "sol-high",
		"upstream_model":  "gpt-5.6-sol",
		"effort":          "high",
		"stream":          true,
		"stop_reason":     "end_turn",
		"interrupted":     false,
		"transport":       "std",
		"status":          float64(http.StatusOK),
		"upstream_status": float64(http.StatusOK),
		"output_tokens":   float64(4),
	} {
		if rec[key] != want {
			t.Errorf("request line %q = %v, want %v", key, rec[key], want)
		}
	}
	if n, _ := rec["req_bytes"].(float64); n <= 0 {
		t.Errorf("req_bytes = %v, want the real body size", rec["req_bytes"])
	}
	// err must be ABSENT on a successful request, not empty: a reader filtering
	// on the field's presence should find only real failures.
	if _, present := rec["err"]; present {
		t.Errorf("a successful request logged an err field: %v", rec)
	}
}

// TestLogNeverCarriesTokenShapedMaterial is the safety half: a complete request,
// through the production logger at DEBUG (the noisiest setting anyone would
// run), must not put a credential anywhere in the output.
func TestLogNeverCarriesTokenShapedMaterial(t *testing.T) {
	restoreRegistry(t)

	mat := tokenShapedMaterial{
		accessToken:  jwtWithExp(time.Now().Add(2 * time.Hour)),
		refreshToken: "rt_LFRDkQ3mSecretRefreshTokenValue0123456789",
		clientBearer: "sk-ant-oat01-CLIENTOAUTHSECRETVALUE0123456789",
		apiKey:       "sk-ant-api03-METEREDKEYSECRETVALUE0123456789",
		accountID:    "acct-real-subscription-id",
	}

	env := newCodexEnvOpts(t, nil, codexEnvOptions{CaptureLogs: true, Level: slog.LevelDebug})
	// Rewrite auth.json with the realistic secrets, then make the leg use them.
	writeAuthJSONWithAccount(t, env.authPath, mat.accessToken, mat.refreshToken, mat.accountID)

	hdr := func(h http.Header) {
		h.Set("Authorization", "Bearer "+mat.clientBearer)
		h.Set("X-Api-Key", mat.apiKey)
		h.Set("Anthropic-Beta", "oauth-2025-04-20")
		h.Set("X-Request-Id", "leak-check-1")
	}

	// A codex request (the leg signs with the Codex credential) …
	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, hdr)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// … an anthropic passthrough (the CLIENT's credential goes upstream) …
	resp = post(t, env.front.URL+"/v1/messages",
		`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, hdr)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// … a picker open, and a health poll.
	for _, path := range []string{"/v1/models", "/healthz"} {
		r, err := noRedirectClient().Get(env.front.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}

	out := env.logs.String()
	if strings.TrimSpace(out) == "" {
		t.Fatal("nothing was logged; the leak assertion would be vacuous")
	}
	for _, secret := range mat.all() {
		if strings.Contains(out, secret) {
			t.Errorf("the log leaked credential material %q:\n%s", secret, out)
		}
	}
	// Also nothing token-SHAPED, even a value no test planted.
	for _, shape := range []string{"Bearer ey", "Bearer sk-", `"eyJ`, "sk-ant-"} {
		if strings.Contains(out, shape) {
			t.Errorf("the log carried token-shaped material %q:\n%s", shape, out)
		}
	}
	// The request id and the allowlisted headers must still be there: a
	// redactor that logged nothing would also pass the assertions above.
	if !strings.Contains(out, "leak-check-1") {
		t.Errorf("the request id was lost:\n%s", out)
	}
	if !strings.Contains(out, "oauth-2025-04-20") {
		t.Errorf("the allowlisted anthropic-beta value was dropped:\n%s", out)
	}
	if !strings.Contains(out, "authorization") {
		t.Errorf("a withheld header should still be named:\n%s", out)
	}
}

// A client that hangs up mid-stream is reported as interrupted, not as an
// error: an abandoned request is not a failure of the proxy's, and counting it
// as one would make every cancelled turn look like an incident.
func TestInterruptIsReportedAsInterruptNotError(t *testing.T) {
	restoreRegistry(t)
	release := make(chan struct{})
	env := newCodexEnvOpts(t, nil, codexEnvOptions{CaptureLogs: true, Level: slog.LevelInfo})
	env.codex.respond = func(t *testing.T, w http.ResponseWriter, r *http.Request, n int64) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\n")
		_ = rc.Flush()
		<-release
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.front.URL+"/v1/messages",
		strings.NewReader(`{"model":"sol","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	// Read one byte so the response has definitely begun, then hang up.
	one := make([]byte, 1)
	if _, err := resp.Body.Read(one); err != nil {
		t.Fatalf("reading the first byte: %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	var rec map[string]any
	for time.Now().Before(deadline) {
		lines := requestLines(t, env.logs.String())
		if len(lines) > 0 {
			rec = lines[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	if rec == nil {
		t.Fatalf("no request line was emitted:\n%s", env.logs.String())
	}
	if rec["interrupted"] != true {
		t.Errorf("interrupted = %v, want true: %v", rec["interrupted"], rec)
	}
	if rec["route"] != "codex" {
		t.Errorf("route = %v, want codex: %v", rec["route"], rec)
	}
}

// Trace dumps are the future test fixtures, so they must land as three
// well-formed files with the credentials taken out and the prompt left in.
func TestTraceDumpsWriteRedactedFixtures(t *testing.T) {
	restoreRegistry(t)
	dir := t.TempDir()
	env := newCodexEnvOpts(t, nil, codexEnvOptions{
		CaptureLogs: true, Level: slog.LevelInfo, TraceDir: dir,
	})

	hdr := func(h http.Header) {
		h.Set("Authorization", "Bearer sk-ant-oat01-CLIENTOAUTHSECRETVALUE0123456789")
		h.Set("X-Request-Id", "trace-me-1")
	}
	resp := post(t, env.front.URL+"/v1/messages",
		`{"model":"sol","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"a distinctive prompt"}]}`, hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Enabling tracing must be loud.
	if !strings.Contains(env.logs.String(), "PROMPT TEXT") {
		t.Errorf("no startup warning for tracing:\n%s", env.logs.String())
	}

	read := func(suffix string) string {
		b, err := os.ReadFile(filepath.Join(dir, "trace-me-1"+suffix))
		if err != nil {
			t.Fatalf("trace file %s missing: %v", suffix, err)
		}
		return string(b)
	}
	manifest := read(obs.SuffixRequest)
	upstream := read(obs.SuffixUpstream)
	downstream := read(obs.SuffixDownstream)

	if !json.Valid([]byte(manifest)) {
		t.Errorf("the request manifest is not valid JSON:\n%s", manifest)
	}
	if !strings.Contains(manifest, "a distinctive prompt") {
		t.Errorf("the trace should hold the prompt:\n%s", manifest)
	}
	if strings.Contains(manifest, "CLIENTOAUTHSECRETVALUE") {
		t.Errorf("the trace leaked the client credential:\n%s", manifest)
	}
	if !strings.Contains(upstream, "response.output_text.delta") {
		t.Errorf("the upstream dump is not the upstream stream:\n%s", upstream)
	}
	if !strings.Contains(downstream, "content_block_delta") {
		t.Errorf("the downstream dump is not the translated stream:\n%s", downstream)
	}
	// The manifest carries the summary, so a trace explains itself.
	if !strings.Contains(manifest, `"route": "codex"`) {
		t.Errorf("the manifest is missing the request summary:\n%s", manifest)
	}
}

// /healthz must answer every operational question without a secret in it.
func TestHealthzReportsTheFullOperationalPicture(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, nil)

	var health map[string]any
	if err := json.Unmarshal([]byte(getHealth(t, env)), &health); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}

	for _, key := range []string{"codex_auth", "codex_catalog", "codex_stream", "codex_quota", "codex_routing", "transport", "trace"} {
		if _, ok := health[key]; !ok {
			t.Errorf("/healthz is missing %q: %v", key, health)
		}
	}
	transport, _ := health["transport"].(map[string]any)
	if transport["kind"] != "std" {
		t.Errorf("transport.kind = %v, want std", transport["kind"])
	}
	// Quota is present even before the backend has said anything, so "no quota
	// block" can never be misread as "quota is fine".
	quota, _ := health["codex_quota"].(map[string]any)
	if quota["reported"] != false {
		t.Errorf("codex_quota.reported = %v, want false before any upstream answer", quota["reported"])
	}
	trace, _ := health["trace"].(map[string]any)
	if trace["enabled"] != false {
		t.Errorf("trace.enabled = %v, want false by default", trace["enabled"])
	}
}

// The gap observed live: a freshly started daemon reported "models 0" because
// the catalog is fetched lazily and nothing had asked yet — indistinguishable
// from a failure. The startup warm fixes the count; the state field fixes the
// ambiguity.
func TestStartupWarmPopulatesTheCatalogAndDistinguishesTheZeroStates(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, nil)

	// Before the warm: nothing held, and the state says why rather than
	// leaving a bare zero to interpret.
	cat := healthCatalog(t, env)
	if cat["models"] != float64(0) || cat["loaded"] != false {
		t.Fatalf("expected an unpopulated catalog, got %v", cat)
	}
	if cat["state"] != catalogCold {
		t.Errorf("state = %v, want %q before anything has tried", cat["state"], catalogCold)
	}
	if _, present := cat["last_error"]; present {
		t.Errorf("a never-attempted catalog must not report an error: %v", cat)
	}

	env.app.warmCatalog(context.Background())

	cat = waitForCatalog(t, env, func(m map[string]any) bool { return m["loaded"] == true })
	if n, _ := cat["models"].(float64); n != 2 {
		t.Errorf("models = %v, want the 2 the fake backend serves: %v", cat["models"], cat)
	}
	if cat["state"] != catalogLoaded {
		t.Errorf("state = %v, want %q", cat["state"], catalogLoaded)
	}
	if _, ok := cat["age_s"]; !ok {
		t.Errorf("a loaded catalog must report its age: %v", cat)
	}
	if env.codex.catalog.Load() == 0 {
		t.Error("the warm never fetched the catalog")
	}
}

// A failed warm must be reported AS a failure, not as an empty catalog.
func TestWarmFailureIsDistinguishableFromAnEmptyCatalog(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, func(c *config.Config) {})
	env.codex.catalogFail = http.StatusInternalServerError

	env.app.warmCatalog(context.Background())

	cat := waitForCatalog(t, env, func(m map[string]any) bool { return m["state"] == catalogFailed })
	if cat["models"] != float64(0) || cat["loaded"] != false {
		t.Errorf("a failed warm must not claim a catalog: %v", cat)
	}
	if msg, _ := cat["last_error"].(string); msg == "" {
		t.Errorf("a failed warm must say why: %v", cat)
	}
	if _, ok := cat["last_attempt_age_s"]; !ok {
		t.Errorf("a failed warm must report when it tried: %v", cat)
	}
}

// A backend that really serves nothing is "empty", which is a different fact
// from "failed" and must read as one.
func TestEmptyCatalogIsNotAFailure(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, nil)
	env.codex.catalogBody = `{"models":[]}`

	env.app.warmCatalog(context.Background())

	cat := waitForCatalog(t, env, func(m map[string]any) bool { return m["state"] == catalogEmpty })
	if cat["models"] != float64(0) {
		t.Errorf("models = %v, want 0: %v", cat["models"], cat)
	}
	if _, present := cat["last_error"]; present {
		t.Errorf("an empty-but-successful fetch must not report an error: %v", cat)
	}
}

// Without a Codex credential there is nothing to warm, and /healthz should say
// so rather than reporting a failure the operator cannot act on.
func TestNoCredentialReportsUnavailableRatherThanFailed(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, func(c *config.Config) { c.Codex.AuthFile = "" })

	env.app.warmCatalog(context.Background())

	cat := healthCatalog(t, env)
	if cat["state"] != catalogUnavailable {
		t.Errorf("state = %v, want %q with no credential: %v", cat["state"], catalogUnavailable, cat)
	}
	if env.codex.catalog.Load() != 0 {
		t.Error("a credential-less warm must not have contacted the backend")
	}
}

// --- helpers -------------------------------------------------------------

func getHealth(t *testing.T, env *codexEnv) string {
	t.Helper()
	resp, err := noRedirectClient().Get(env.front.URL + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read healthz: %v", err)
	}
	return string(body)
}

func healthCatalog(t *testing.T, env *codexEnv) map[string]any {
	t.Helper()
	var health struct {
		CodexCatalog map[string]any `json:"codex_catalog"`
	}
	if err := json.Unmarshal([]byte(getHealth(t, env)), &health); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	return health.CodexCatalog
}

// waitForCatalog polls /healthz until want is satisfied. The warm is a
// goroutine, so the alternative is a sleep long enough to be flaky in CI.
func waitForCatalog(t *testing.T, env *codexEnv, want func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = healthCatalog(t, env)
		if want(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("codex_catalog never reached the expected state; last was %v", last)
	return nil
}
