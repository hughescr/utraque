package responses

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/schema"
)

// Every test in this file talks to an httptest fake upstream and uses a
// fabricated credential. The real chatgpt.com / auth.openai.com endpoints and
// the real ~/.codex/auth.json are never touched.

const (
	fakeToken   = "fake-access-token-not-a-real-secret"
	fakeAccount = "acct_fake_0123456789"
)

func testCred() auth.Credential {
	return auth.Credential{
		AccessToken: fakeToken,
		AccountID:   fakeAccount,
		Exp:         time.Now().Add(time.Hour),
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func testRequest() *schema.ResponsesRequest {
	return &schema.ResponsesRequest{
		Model: "gpt-5.6-sol",
		Input: []schema.InputItem{
			schema.MessageItem("user", schema.InputText("hello")),
		},
		Store:  false,
		Stream: false, // Stream must force this true on a copy, not in place
	}
}

// capture is one observed upstream request, handed to the test over a channel
// so nothing is shared between the handler goroutine and the test goroutine.
type capture struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

func newClient(t *testing.T, baseURL string, tweak ...func(*Options)) *Client {
	t.Helper()
	o := Options{BaseURL: baseURL, Logger: quietLogger()}
	for _, f := range tweak {
		f(&o)
	}
	return New(o)
}

// sseServer serves a fixed SSE body and records the request it saw.
func sseServer(t *testing.T, body string) (*httptest.Server, <-chan capture) {
	t.Helper()
	rec := make(chan capture, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec <- capture{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: b}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestStreamSendsCodexHeadersAndBody(t *testing.T) {
	srv, rec := sseServer(t, "event: response.created\ndata: {}\n\n")
	c := newClient(t, srv.URL)

	req := testRequest()
	body, err := c.Stream(context.Background(), testCred(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = body.Close() }()

	got := <-rec
	if got.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.Method)
	}
	if got.Path != Path {
		t.Errorf("path = %q, want %q", got.Path, Path)
	}
	wantHeaders := map[string]string{
		"Authorization":      "Bearer " + fakeToken,
		"Chatgpt-Account-Id": fakeAccount,
		"Openai-Beta":        "responses=experimental",
		"Originator":         "codex_cli_rs",
		"Content-Type":       "application/json",
		"Accept":             "text/event-stream",
	}
	for k, want := range wantHeaders {
		if got := got.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	var sent map[string]any
	if err := json.Unmarshal(got.Body, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, got.Body)
	}
	if sent["model"] != "gpt-5.6-sol" {
		t.Errorf("model = %v, want gpt-5.6-sol", sent["model"])
	}
	if sent["stream"] != true {
		t.Errorf("stream = %v, want true (Stream must force it)", sent["stream"])
	}
	if sent["store"] != false {
		t.Errorf("store = %v, want false", sent["store"])
	}
	if req.Stream {
		t.Error("Stream mutated the caller's request (Stream flag set in place)")
	}
}

func TestStreamReturnsReadableSSEBody(t *testing.T) {
	const wire = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	srv, _ := sseServer(t, wire)
	c := newClient(t, srv.URL)

	body, err := c.Stream(context.Background(), testCred(), testRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != wire {
		t.Errorf("body =\n%q\nwant\n%q", got, wire)
	}
	if err := body.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Errorf("second Close must be a no-op, got %v", err)
	}
}

func TestStreamResponseCarriesMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-codex-primary-used-percent", "42.5")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-after-seconds", "1800")
		w.Header().Set("x-request-id", "req_123")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()

	var seen []RateLimits
	var mu sync.Mutex
	c := newClient(t, srv.URL, func(o *Options) {
		o.OnRateLimits = func(rl RateLimits) {
			mu.Lock()
			seen = append(seen, rl)
			mu.Unlock()
		}
	})

	resp, err := c.StreamResponse(context.Background(), testCred(), testRequest())
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	defer func() { _ = resp.Close() }()

	if resp.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", resp.Status)
	}
	if got := resp.Header.Get("x-request-id"); got != "req_123" {
		t.Errorf("Header x-request-id = %q, want req_123", got)
	}
	if !resp.RateLimits.Primary.HasUsedPercent || resp.RateLimits.Primary.UsedPercent != 42.5 {
		t.Errorf("primary used percent = %v/%v, want 42.5", resp.RateLimits.Primary.UsedPercent, resp.RateLimits.Primary.HasUsedPercent)
	}
	if resp.RateLimits.Primary.WindowMinutes != 300 {
		t.Errorf("primary window = %d, want 300", resp.RateLimits.Primary.WindowMinutes)
	}
	if resp.RateLimits.Primary.ResetAfter != 30*time.Minute {
		t.Errorf("primary reset = %v, want 30m", resp.RateLimits.Primary.ResetAfter)
	}
	// The non-quota header must not be forwarded.
	if _, ok := resp.RateLimits.Headers["X-Request-Id"]; ok {
		t.Error("x-request-id must not be in the forwarded rate-limit header set")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("OnRateLimits called %d times, want 1 (success path must report quota too)", len(seen))
	}
	if seen[0].Primary.UsedPercent != 42.5 {
		t.Errorf("hook primary used percent = %v, want 42.5", seen[0].Primary.UsedPercent)
	}
}

func TestStreamErrorClassification(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantClass   Class
		wantStatus  int // rendered HTTP status
		wantKind    apierr.Type
		wantMsgPart string
		wantRetry   bool
		wantRefresh bool
	}{
		{
			name: "400 invalid request", status: 400, contentType: "application/json",
			body:      `{"error":{"message":"unknown parameter: foo","type":"invalid_request_error","code":"bad_param"}}`,
			wantClass: ClassTerminal, wantStatus: 400, wantKind: apierr.TypeInvalidRequest,
			wantMsgPart: "unknown parameter: foo",
		},
		{
			name: "401 auth", status: 401, contentType: "application/json",
			body:      `{"error":{"message":"token expired"}}`,
			wantClass: ClassAuth, wantStatus: 401, wantKind: apierr.TypeAuthentication,
			wantMsgPart: "token expired", wantRefresh: true,
		},
		{
			name: "403 permission with json body is not a gate", status: 403, contentType: "application/json",
			body:      `{"detail":"your plan does not include codex"}`,
			wantClass: ClassTerminal, wantStatus: 403, wantKind: apierr.TypePermission,
			wantMsgPart: "your plan does not include codex",
		},
		{
			name: "404 not found", status: 404, contentType: "application/json",
			body:      `{"error":{"message":"no such model"}}`,
			wantClass: ClassTerminal, wantStatus: 404, wantKind: apierr.TypeNotFound,
			wantMsgPart: "no such model",
		},
		{
			name: "413 too large", status: 413, contentType: "application/json",
			body:      `{"error":{"message":"request too large"}}`,
			wantClass: ClassTerminal, wantStatus: 413, wantKind: apierr.TypeRequestTooLarge,
			wantMsgPart: "request too large",
		},
		{
			name: "422 unmapped 4xx renders as invalid_request", status: 422, contentType: "application/json",
			body:      `{"detail":{"message":"input.0.content is malformed"}}`,
			wantClass: ClassTerminal, wantStatus: 422, wantKind: apierr.TypeInvalidRequest,
			wantMsgPart: "input.0.content is malformed",
		},
		{
			name: "500 upstream", status: 500, contentType: "application/json",
			body:      `{"error":{"message":"internal error"}}`,
			wantClass: ClassUpstream, wantStatus: 500, wantKind: apierr.TypeAPI,
			wantMsgPart: "internal error", wantRetry: true,
		},
		{
			name: "502 upstream", status: 502, contentType: "text/plain",
			body:      "upstream connect error",
			wantClass: ClassUpstream, wantStatus: 502, wantKind: apierr.TypeAPI,
			wantMsgPart: "upstream connect error", wantRetry: true,
		},
		{
			name: "503 upstream renders as overloaded", status: 503, contentType: "application/json",
			body:      `{"error":{"message":"server overloaded"}}`,
			wantClass: ClassUpstream, wantStatus: 503, wantKind: apierr.TypeOverloaded,
			wantMsgPart: "server overloaded", wantRetry: true,
		},
		{
			name: "504 upstream renders as timeout", status: 504, contentType: "application/json",
			body:      `{"error":{"message":"gateway timeout"}}`,
			wantClass: ClassUpstream, wantStatus: 504, wantKind: apierr.TypeTimeout,
			wantMsgPart: "gateway timeout", wantRetry: true,
		},
		{
			name: "204 is not a stream", status: 204, contentType: "",
			body:      "",
			wantClass: ClassTerminal, wantStatus: 502, wantKind: apierr.TypeAPI,
			wantMsgPart: "unexpected response",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newClient(t, srv.URL)
			body, err := c.Stream(context.Background(), testCred(), testRequest())
			if err == nil {
				_ = body.Close()
				t.Fatalf("status %d: want an error, got a stream", tc.status)
			}
			if body != nil {
				t.Error("a failed Stream must not return a body")
			}

			ue, ok := AsUpstream(err)
			if !ok {
				t.Fatalf("error is not an *UpstreamError: %#v", err)
			}
			if ue.Class != tc.wantClass {
				t.Errorf("Class = %q, want %q", ue.Class, tc.wantClass)
			}
			if ue.Status != tc.status {
				t.Errorf("upstream Status = %d, want %d", ue.Status, tc.status)
			}
			if ue.Retryable() != tc.wantRetry {
				t.Errorf("Retryable() = %v, want %v", ue.Retryable(), tc.wantRetry)
			}
			if ue.NeedsCredentialRefresh() != tc.wantRefresh {
				t.Errorf("NeedsCredentialRefresh() = %v, want %v", ue.NeedsCredentialRefresh(), tc.wantRefresh)
			}
			if ue.IsGate() {
				t.Error("IsGate() = true, want false")
			}

			// The leg renders through apierr: errors.As must reach it.
			ae := apierr.From(err)
			if ae == nil {
				t.Fatal("apierr.From returned nil")
			}
			if ae.HTTPStatus() != tc.wantStatus {
				t.Errorf("rendered status = %d, want %d", ae.HTTPStatus(), tc.wantStatus)
			}
			if ae.Kind != tc.wantKind {
				t.Errorf("error kind = %q, want %q", ae.Kind, tc.wantKind)
			}
			if ue.HTTPStatus() != tc.wantStatus {
				t.Errorf("UpstreamError.HTTPStatus() = %d, want %d", ue.HTTPStatus(), tc.wantStatus)
			}
			if !strings.Contains(ae.Message, tc.wantMsgPart) {
				t.Errorf("message %q does not contain %q", ae.Message, tc.wantMsgPart)
			}
			env := ae.Envelope()
			if env.Error.Type != string(tc.wantKind) {
				t.Errorf("envelope type = %q, want %q", env.Error.Type, tc.wantKind)
			}
			if strings.Contains(err.Error(), fakeToken) {
				t.Error("error text leaked the access token")
			}
		})
	}
}

func TestStream401CarriesUpstreamDiscriminators(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"expired","type":"invalid_token","code":401}}`)
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).Stream(context.Background(), testCred(), testRequest())
	ue, ok := AsUpstream(err)
	if !ok {
		t.Fatalf("not an *UpstreamError: %v", err)
	}
	if ue.UpstreamMessage != "expired" {
		t.Errorf("UpstreamMessage = %q, want %q", ue.UpstreamMessage, "expired")
	}
	if ue.UpstreamType != "invalid_token" {
		t.Errorf("UpstreamType = %q, want invalid_token", ue.UpstreamType)
	}
	if ue.UpstreamCode != "401" {
		t.Errorf("UpstreamCode = %q, want 401 (numeric codes are stringified)", ue.UpstreamCode)
	}
	if ClassOf(err) != ClassAuth {
		t.Errorf("ClassOf = %q, want %q", ClassOf(err), ClassAuth)
	}
}

func TestStream429ForwardsRetryAfterAndQuotaHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Retry-After", "42")
		h.Set("x-codex-primary-used-percent", "100")
		h.Set("x-codex-primary-window-minutes", "300")
		h.Set("x-codex-primary-reset-after-seconds", "1800")
		h.Set("x-codex-secondary-used-percent", "63.25")
		h.Set("x-codex-secondary-window-minutes", "10080")
		h.Set("x-ratelimit-limit-requests", "100")
		h.Set("x-request-id", "req_should_not_forward")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"You have hit your usage limit","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()

	var hookSeen int
	c := newClient(t, srv.URL, func(o *Options) {
		o.OnRateLimits = func(RateLimits) { hookSeen++ }
	})

	_, err := c.Stream(context.Background(), testCred(), testRequest())
	ue, ok := AsUpstream(err)
	if !ok {
		t.Fatalf("not an *UpstreamError: %v", err)
	}
	if ue.Class != ClassRateLimit {
		t.Errorf("Class = %q, want %q", ue.Class, ClassRateLimit)
	}
	if !ue.Retryable() {
		t.Error("a 429 must be marked retryable")
	}
	if ue.HTTPStatus() != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", ue.HTTPStatus())
	}
	if apierr.From(err).Kind != apierr.TypeRateLimit {
		t.Errorf("kind = %q, want rate_limit_error", apierr.From(err).Kind)
	}
	d, ok := ue.RetryAfterDelay()
	if !ok || d != 42*time.Second {
		t.Errorf("RetryAfterDelay = %v/%v, want 42s/true", d, ok)
	}
	if ue.RateLimits.RetryAfterRaw != "42" {
		t.Errorf("RetryAfterRaw = %q, want 42", ue.RateLimits.RetryAfterRaw)
	}
	if ue.RateLimits.Secondary.UsedPercent != 63.25 || ue.RateLimits.Secondary.WindowMinutes != 10080 {
		t.Errorf("secondary window = %+v", ue.RateLimits.Secondary)
	}
	if !strings.Contains(apierr.From(err).Message, "You have hit your usage limit") {
		t.Errorf("message lost the upstream text: %q", apierr.From(err).Message)
	}

	// ApplyHeaders is how the leg preserves 429 semantics on its own response.
	out := http.Header{}
	ue.ApplyHeaders(out)
	if out.Get("Retry-After") != "42" {
		t.Errorf("forwarded Retry-After = %q, want 42", out.Get("Retry-After"))
	}
	if out.Get("X-Codex-Primary-Reset-After-Seconds") != "1800" {
		t.Errorf("forwarded reset-after = %q, want 1800", out.Get("X-Codex-Primary-Reset-After-Seconds"))
	}
	if out.Get("X-Ratelimit-Limit-Requests") != "100" {
		t.Errorf("forwarded x-ratelimit header = %q, want 100", out.Get("X-Ratelimit-Limit-Requests"))
	}
	if out.Get("X-Request-Id") != "" {
		t.Error("only quota headers may be forwarded")
	}
	if hookSeen != 1 {
		t.Errorf("OnRateLimits called %d times on a 429, want 1", hookSeen)
	}
}

func TestStream429RetryAfterHTTPDate(t *testing.T) {
	fixed := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", fixed.Add(90*time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, func(o *Options) { o.Now = func() time.Time { return fixed } })
	_, err := c.Stream(context.Background(), testCred(), testRequest())
	ue, ok := AsUpstream(err)
	if !ok {
		t.Fatalf("not an *UpstreamError: %v", err)
	}
	d, ok := ue.RetryAfterDelay()
	if !ok || d != 90*time.Second {
		t.Errorf("RetryAfterDelay = %v/%v, want 90s/true", d, ok)
	}
	if !strings.Contains(apierr.From(err).Message, "retry after 1m30s") {
		t.Errorf("message should state the delay, got %q", apierr.From(err).Message)
	}
}

const cloudflareChallenge = `<!DOCTYPE html><html><head><title>Just a moment...</title></head>` +
	`<body><div id="cf-browser-verification">Enable JavaScript and cookies to continue</div></body></html>`

func TestStreamDetectsCloudflareGate(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		headers     map[string]string
		body        string
		wantGate    bool
		wantClass   Class
		wantStatus  int
	}{
		{
			name: "403 with cf-ray and challenge html", status: 403, contentType: "text/html",
			headers:  map[string]string{"cf-ray": "8f0a1b2c3d4e5f60-SJC", "Server": "cloudflare"},
			body:     cloudflareChallenge,
			wantGate: true, wantClass: ClassGate, wantStatus: 502,
		},
		{
			name: "403 with cf-ray and no body", status: 403, contentType: "",
			headers:  map[string]string{"cf-ray": "8f0a1b2c3d4e5f60-SJC"},
			body:     "",
			wantGate: true, wantClass: ClassGate, wantStatus: 502,
		},
		{
			name: "200 that is really a challenge page", status: 200, contentType: "text/html",
			headers:  map[string]string{"Server": "cloudflare"},
			body:     cloudflareChallenge,
			wantGate: true, wantClass: ClassGate, wantStatus: 502,
		},
		{
			name: "503 cloudflare block page", status: 503, contentType: "text/html",
			headers:  map[string]string{"Server": "cloudflare", "cf-ray": "aaaa-SJC"},
			body:     `<html><body><h1>Sorry, you have been blocked</h1></body></html>`,
			wantGate: true, wantClass: ClassGate, wantStatus: 502,
		},
		{
			name: "403 api error behind cloudflare is not a gate", status: 403, contentType: "application/json",
			headers:  map[string]string{"cf-ray": "8f0a1b2c3d4e5f60-SJC", "Server": "cloudflare"},
			body:     `{"error":{"message":"forbidden for this account"}}`,
			wantGate: false, wantClass: ClassTerminal, wantStatus: 403,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			body, err := newClient(t, srv.URL).Stream(context.Background(), testCred(), testRequest())
			if err == nil {
				_ = body.Close()
				t.Fatal("want an error, got a stream")
			}
			ue, ok := AsUpstream(err)
			if !ok {
				t.Fatalf("not an *UpstreamError: %v", err)
			}
			if ue.IsGate() != tc.wantGate {
				t.Errorf("IsGate() = %v, want %v", ue.IsGate(), tc.wantGate)
			}
			if ue.Class != tc.wantClass {
				t.Errorf("Class = %q, want %q", ue.Class, tc.wantClass)
			}
			if ue.Status != tc.status {
				t.Errorf("upstream Status = %d, want %d", ue.Status, tc.status)
			}
			if ue.HTTPStatus() != tc.wantStatus {
				t.Errorf("rendered status = %d, want %d", ue.HTTPStatus(), tc.wantStatus)
			}
			msg := apierr.From(err).Message
			if tc.wantGate {
				if ue.Retryable() {
					t.Error("a gate must not be marked retryable")
				}
				for _, want := range []string{"bot/TLS challenge", "uTLS", "phase 8"} {
					if !strings.Contains(msg, want) {
						t.Errorf("gate message %q lacks %q", msg, want)
					}
				}
				if strings.Contains(msg, "<") {
					t.Errorf("gate message must not echo the challenge HTML: %q", msg)
				}
			} else if strings.Contains(msg, "uTLS") {
				t.Errorf("non-gate error must not advertise uTLS: %q", msg)
			}
		})
	}
}

func TestStreamNeverFollowsRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/responses", http.StatusFound)
	}))
	defer srv.Close()

	body, err := newClient(t, srv.URL).Stream(context.Background(), testCred(), testRequest())
	if err == nil {
		_ = body.Close()
		t.Fatal("a redirect must not be followed into a stream")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("redirect target was contacted %d times; the bearer token must never be replayed", n)
	}
	ue, _ := AsUpstream(err)
	if ue == nil || ue.Class != ClassTerminal {
		t.Fatalf("Class = %v, want terminal: %v", ue, err)
	}
	if ue.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", ue.HTTPStatus())
	}
	if !strings.Contains(apierr.From(err).Message, "redirect") {
		t.Errorf("message should name the redirect: %q", apierr.From(err).Message)
	}
}

func TestStreamTransportFailure(t *testing.T) {
	// A server that is started and immediately closed leaves a port nothing is
	// listening on — a dial failure with no HTTP response at all.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()

	_, err := newClient(t, dead).Stream(context.Background(), testCred(), testRequest())
	ue, ok := AsUpstream(err)
	if !ok {
		t.Fatalf("not an *UpstreamError: %v", err)
	}
	if ue.Class != ClassNetwork {
		t.Errorf("Class = %q, want %q", ue.Class, ClassNetwork)
	}
	if ue.Status != 0 {
		t.Errorf("Status = %d, want 0 (no response was received)", ue.Status)
	}
	if !ue.Retryable() {
		t.Error("a transport failure should be retryable")
	}
	if ue.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", ue.HTTPStatus())
	}
}

func TestStreamContextCancelBeforeHeaders(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		waitDone(r) // never answer; wait for the client to give up
		close(released)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	defer cancel()

	_, err := newClient(t, srv.URL).Stream(ctx, testCred(), testRequest())
	if err == nil {
		t.Fatal("want an error after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
	ue, ok := AsUpstream(err)
	if !ok {
		t.Fatalf("not an *UpstreamError: %v", err)
	}
	if ue.Class != ClassCanceled {
		t.Errorf("Class = %q, want %q", ue.Class, ClassCanceled)
	}
	if ue.Retryable() {
		t.Error("a cancelled request must not be marked retryable")
	}
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Error("cancellation did not propagate to the upstream request")
	}
}

func TestStreamContextDeadlineBeforeHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		waitDone(r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := newClient(t, srv.URL).Stream(ctx, testCred(), testRequest())
	ue, ok := AsUpstream(err)
	if !ok {
		t.Fatalf("not an *UpstreamError: %v", err)
	}
	if ue.Class != ClassTimeout {
		t.Errorf("Class = %q, want %q", ue.Class, ClassTimeout)
	}
	if ue.HTTPStatus() != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", ue.HTTPStatus())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false; err = %v", err)
	}
}

func TestStreamContextCancelMidStream(t *testing.T) {
	upstreamDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		waitDone(r) // hold the stream open until the client hangs up
		close(upstreamDone)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body, err := newClient(t, srv.URL).Stream(ctx, testCred(), testRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	br := bufio.NewReader(body)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	if !strings.HasPrefix(line, "event: response.created") {
		t.Errorf("first line = %q", line)
	}

	cancel()

	// The in-flight read must fail rather than hang.
	readErr := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(br)
		readErr <- err
	}()
	select {
	case err := <-readErr:
		if err == nil {
			t.Error("read after cancellation returned no error")
		} else if !errors.Is(err, context.Canceled) {
			t.Errorf("read error = %v, want one wrapping context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not unblock after cancellation")
	}

	if err := body.Close(); err != nil {
		t.Errorf("Close after cancellation: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Errorf("second Close must be a no-op: %v", err)
	}

	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Error("cancellation did not reach the upstream handler")
	}
}

func TestStreamRejectsUnusableInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be contacted for an unusable request")
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)

	if _, err := c.Stream(context.Background(), testCred(), nil); err == nil {
		t.Error("nil request: want an error")
	} else if apierr.From(err).HTTPStatus() != http.StatusBadRequest {
		t.Errorf("nil request status = %d, want 400", apierr.From(err).HTTPStatus())
	}

	for _, cred := range []auth.Credential{{}, {AccessToken: fakeToken}, {AccountID: fakeAccount}} {
		if _, err := c.Stream(context.Background(), cred, testRequest()); err == nil {
			t.Errorf("credential %+v: want an error", cred)
		} else if apierr.From(err).Kind != apierr.TypeAuthentication {
			t.Errorf("credential %+v: kind = %q, want authentication_error", cred, apierr.From(err).Kind)
		}
	}
}

// fakeSource is a CredentialSource over a fixed list of tokens. Each Invalidate
// advances to the next one, standing in for a refresh. It never touches disk.
type fakeSource struct {
	mu          sync.Mutex
	tokens      []string
	idx         int
	invalidated []string
	getErrAfter int // >0: Get fails from this call number on
	gets        int
}

func (f *fakeSource) Get(context.Context) (auth.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErrAfter > 0 && f.gets >= f.getErrAfter {
		return auth.Credential{}, apierr.Authentication("codex auth.json has no tokens.refresh_token; run `codex login`")
	}
	i := f.idx
	if i >= len(f.tokens) {
		i = len(f.tokens) - 1
	}
	return auth.Credential{AccessToken: f.tokens[i], AccountID: fakeAccount, Exp: time.Now().Add(time.Hour)}, nil
}

func (f *fakeSource) Invalidate(cred auth.Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, cred.AccessToken)
	if f.idx < len(f.tokens)-1 {
		f.idx++
	}
}

func (f *fakeSource) invalidations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.invalidated))
	copy(out, f.invalidated)
	return out
}

// tokenServer answers 200 SSE for tokens in ok, and 401 for everything else,
// recording the bearer tokens it saw.
func tokenServer(t *testing.T, ok map[string]bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, tok)
		mu.Unlock()
		if ok[tok] {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: response.completed\ndata: {}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid access token"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

func TestStreamWithRefreshRetriesOnceOn401(t *testing.T) {
	srv, seen := tokenServer(t, map[string]bool{"tok-fresh": true})
	src := &fakeSource{tokens: []string{"tok-stale", "tok-fresh"}}

	body, err := newClient(t, srv.URL).StreamWithRefresh(context.Background(), src, testRequest())
	if err != nil {
		t.Fatalf("StreamWithRefresh: %v", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), "response.completed") {
		t.Errorf("body = %q", got)
	}
	if want := []string{"tok-stale", "tok-fresh"}; !equalStrings(seen(), want) {
		t.Errorf("upstream saw %v, want %v", seen(), want)
	}
	if inv := src.invalidations(); !equalStrings(inv, []string{"tok-stale"}) {
		t.Errorf("invalidated %v, want [tok-stale]", inv)
	}
}

func TestStreamWithRefreshRetriesAtMostOnce(t *testing.T) {
	srv, seen := tokenServer(t, nil) // every token is rejected
	src := &fakeSource{tokens: []string{"tok-a", "tok-b", "tok-c"}}

	_, err := newClient(t, srv.URL).StreamWithRefresh(context.Background(), src, testRequest())
	if err == nil {
		t.Fatal("want the second 401 to surface")
	}
	if ClassOf(err) != ClassAuth {
		t.Errorf("Class = %q, want auth", ClassOf(err))
	}
	if got := seen(); len(got) != 2 {
		t.Errorf("upstream called %d times (%v), want exactly 2", len(got), got)
	}
}

func TestStreamWithRefreshDoesNotRetryOtherFailures(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()

	src := &fakeSource{tokens: []string{"tok-a", "tok-b"}}
	_, err := newClient(t, srv.URL).StreamWithRefresh(context.Background(), src, testRequest())
	if ClassOf(err) != ClassUpstream {
		t.Errorf("Class = %q, want upstream", ClassOf(err))
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("upstream called %d times, want 1 (only a 401 may be retried)", calls)
	}
	if inv := src.invalidations(); len(inv) != 0 {
		t.Errorf("invalidated %v on a non-401; want none", inv)
	}
}

func TestStreamWithRefreshSkipsRetryWhenTokenUnchanged(t *testing.T) {
	srv, seen := tokenServer(t, nil)
	src := &fakeSource{tokens: []string{"tok-same"}} // refresh yields the same token

	_, err := newClient(t, srv.URL).StreamWithRefresh(context.Background(), src, testRequest())
	if ClassOf(err) != ClassAuth {
		t.Errorf("Class = %q, want auth", ClassOf(err))
	}
	if got := seen(); len(got) != 1 {
		t.Errorf("upstream called %d times (%v), want 1: an unchanged token would fail identically", len(got), got)
	}
}

func TestStreamWithRefreshSurfacesRefreshFailure(t *testing.T) {
	srv, _ := tokenServer(t, nil)
	src := &fakeSource{tokens: []string{"tok-a", "tok-b"}, getErrAfter: 2}

	_, err := newClient(t, srv.URL).StreamWithRefresh(context.Background(), src, testRequest())
	if err == nil {
		t.Fatal("want the refresh failure")
	}
	if !strings.Contains(err.Error(), "codex login") {
		t.Errorf("want the actionable refresh error, got %v", err)
	}
	if apierr.From(err).Kind != apierr.TypeAuthentication {
		t.Errorf("kind = %q, want authentication_error", apierr.From(err).Kind)
	}
}

func TestStreamWithRefreshRejectsNilSource(t *testing.T) {
	srv, _ := sseServer(t, "data: {}\n\n")
	if _, err := newClient(t, srv.URL).StreamWithRefresh(context.Background(), nil, testRequest()); err == nil {
		t.Fatal("want an error for a nil credential source")
	}
}

func TestUpstreamMessageIsBoundedAndSingleLine(t *testing.T) {
	long := strings.Repeat("abcdefghij ", 200) // ~2200 chars, with newlines injected below
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "line one\nline two\r\n" + long},
		})
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).Stream(context.Background(), testCred(), testRequest())
	msg := apierr.From(err).Message
	if strings.ContainsAny(msg, "\n\r") {
		t.Errorf("upstream text must be collapsed to one line: %q", msg)
	}
	if len([]rune(msg)) > maxErrorMessage+200 {
		t.Errorf("message is %d runes, want it truncated near %d", len([]rune(msg)), maxErrorMessage)
	}
	if !strings.Contains(msg, "line one line two") {
		t.Errorf("message lost the useful prefix: %q", msg)
	}
}

func TestHTMLErrorBodyIsNotEchoed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "<html><body><h1>500 Internal Server Error</h1><p>nginx</p></body></html>")
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).Stream(context.Background(), testCred(), testRequest())
	ue, ok := AsUpstream(err)
	if !ok {
		t.Fatalf("not an *UpstreamError: %v", err)
	}
	if ue.Class != ClassUpstream {
		t.Errorf("Class = %q, want upstream (plain HTML 500 is not a bot gate)", ue.Class)
	}
	if strings.Contains(apierr.From(err).Message, "<") {
		t.Errorf("HTML must not reach the error envelope: %q", apierr.From(err).Message)
	}
}

func TestErrorBodyReadIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, huge)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, func(o *Options) { o.MaxErrorBody = 1024 })
	_, err := c.Stream(context.Background(), testCred(), testRequest())
	if ClassOf(err) != ClassUpstream {
		t.Errorf("Class = %q, want upstream", ClassOf(err))
	}
	if n := len([]rune(apierr.From(err).Message)); n > maxErrorMessage+200 {
		t.Errorf("message is %d runes; the body read must stay bounded", n)
	}
}

func TestNewDefaults(t *testing.T) {
	c := New(Options{})
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.http == nil {
		t.Fatal("no HTTP client")
	}
	if c.http.CheckRedirect == nil {
		t.Error("the default client must be the no-redirect transport")
	}
	if c.http.Timeout != 0 {
		t.Errorf("client timeout = %v; a whole-request deadline would cut long streams", c.http.Timeout)
	}
	if got := New(Options{BaseURL: "http://example.invalid/base/"}).baseURL; got != "http://example.invalid/base" {
		t.Errorf("trailing slash not trimmed: %q", got)
	}
}

// drain consumes the request body. The net/http server only starts watching for
// a client disconnect once the body has hit EOF, so a handler that means to
// wait on r.Context() MUST drain first or the context never fires.
func drain(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
}

// waitDone blocks until the client hangs up, with a ceiling so a regression
// fails the test instead of wedging the suite until the package timeout.
func waitDone(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
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

// TestStream200NonSSEIsClassifiedNotStreamed pins the content-type gate on a
// 200. The backend occasionally answers a failure with the WRONG status: HTTP
// 200 carrying a JSON error body. Handing that to the SSE translator would lose
// the upstream's own message and surface as a generic "no events" 502, so a 200
// that is not an event stream is classified like any other failure.
func TestStream200NonSSEIsClassifiedNotStreamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"error":{"message":"maintenance in progress","type":"server_error"}}`)
	}))
	defer srv.Close()

	body, err := newClient(t, srv.URL).Stream(context.Background(), testCred(), testRequest())
	if err == nil {
		_ = body.Close()
		t.Fatal("a 200 application/json body was accepted as an SSE stream")
	}
	ue, ok := AsUpstream(err)
	if !ok {
		t.Fatalf("error %v is not an *UpstreamError", err)
	}
	if ue.Status != http.StatusOK {
		t.Errorf("upstream status = %d, want the 200 recorded verbatim", ue.Status)
	}
	if ue.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("rendered status = %d, want 502", ue.HTTPStatus())
	}
	if ue.UpstreamMessage != "maintenance in progress" {
		t.Errorf("upstream message = %q, want the backend's own diagnostic", ue.UpstreamMessage)
	}
	if !strings.Contains(apierr.From(err).Message, "maintenance in progress") {
		t.Errorf("rendered message = %q, want it to carry the upstream diagnostic",
			apierr.From(err).Message)
	}
}

// TestStream200WithoutContentTypeIsStreamed guards the other side of the gate: a
// content type is legal to omit on a chunked response, and refusing one would
// turn a working stream into a 502.
func TestStream200WithoutContentTypeIsStreamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h["Content-Type"] = nil // suppress net/http's sniffing default
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
	}))
	defer srv.Close()

	body, err := newClient(t, srv.URL).Stream(context.Background(), testCred(), testRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.Contains(string(got), "response.created") {
		t.Errorf("stream body = %q, want the upstream frames", got)
	}
}

// TestRefreshRetriesWhenOnlyTheAccountChanged: the request is signed by the
// token AND the account id, so a refresh that corrects only the account id still
// makes the retry a different call.
func TestRefreshRetriesWhenOnlyTheAccountChanged(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"wrong account"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
	}))
	defer srv.Close()

	src := &accountRotatingSource{token: fakeToken, first: "acct_wrong", second: "acct_right"}
	body, err := newClient(t, srv.URL).StreamWithRefresh(context.Background(), src, testRequest())
	if err != nil {
		t.Fatalf("StreamWithRefresh: %v", err)
	}
	defer func() { _ = body.Close() }()

	if n := attempts.Load(); n != 2 {
		t.Errorf("upstream attempts = %d, want exactly 2 (original + one retry)", n)
	}
	if !src.invalidated {
		t.Error("the rejected credential was never invalidated")
	}
}

// accountRotatingSource hands out the same access token twice but a corrected
// account id the second time.
type accountRotatingSource struct {
	token         string
	first, second string
	gets          int
	invalidated   bool
}

func (s *accountRotatingSource) Get(context.Context) (auth.Credential, error) {
	s.gets++
	account := s.first
	if s.gets > 1 {
		account = s.second
	}
	return auth.Credential{AccessToken: s.token, AccountID: account, Exp: time.Now().Add(time.Hour)}, nil
}

func (s *accountRotatingSource) Invalidate(auth.Credential) { s.invalidated = true }
