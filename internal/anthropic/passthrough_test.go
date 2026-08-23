package anthropic

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/server"
	"github.com/hughescr/utraque/internal/transport"
)

type capture struct {
	mu       sync.Mutex
	method   string
	path     string
	rawPath  string
	rawQuery string
	host     string
	header   http.Header
	body     []byte
	hits     int
}

func (c *capture) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method = r.Method
	c.path = r.URL.Path
	c.rawPath = r.URL.EscapedPath()
	c.rawQuery = r.URL.RawQuery
	c.host = r.Host
	c.header = r.Header.Clone()
	c.body = b
	c.hits++
}

func (c *capture) snapshot() capture {
	c.mu.Lock()
	defer c.mu.Unlock()
	return capture{
		method: c.method, path: c.path, rawPath: c.rawPath, rawQuery: c.rawQuery,
		host: c.host, header: c.header, body: c.body, hits: c.hits,
	}
}

func newTestLeg(t *testing.T, base string, opts ...Option) *Leg {
	t.Helper()
	l, err := New(base, transport.NewStd(transport.DefaultOptions()), opts...)
	if err != nil {
		t.Fatalf("New(%q): %v", base, err)
	}
	return l
}

// noRedirectClient is the test's own client. The default client follows
// redirects, which would mask the leg's no-redirect behaviour.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestForwardsBodyByteIdentically(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	// Odd-but-legal JSON: irregular spacing, a unicode escape, a literal
	// non-ASCII rune, and a trailing newline. Any re-encode would change it.
	body := "{\n  \"model\" : \"claude-opus-5\",\n  \"note\": \"caf\\u00e9 ☕\",\n" +
		"  \"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]\n}\n"

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	got := cap.snapshot()
	if string(got.body) != body {
		t.Errorf("body not forwarded verbatim:\n got %q\nwant %q", got.body, body)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", got.path)
	}
}

func TestForwardsPathQueryAndUpstreamHost(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	// A catch-all path with a percent-escape and a repeated query key.
	resp, err := noRedirectClient().Get(proxy.URL + "/v1/organizations/a%2Fb/models?limit=5&limit=6&x=")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	got := cap.snapshot()
	if want := "/v1/organizations/a%2Fb/models"; got.rawPath != want {
		t.Errorf("escaped path = %q, want %q", got.rawPath, want)
	}
	if want := "limit=5&limit=6&x="; got.rawQuery != want {
		t.Errorf("raw query = %q, want %q", got.rawQuery, want)
	}
	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")
	if got.host != upstreamHost {
		t.Errorf("upstream saw Host %q, want %q (must be the upstream host, not the proxy's)", got.host, upstreamHost)
	}
}

func TestPreservesAuthorizationAndRepeatedAnthropicBeta(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-EXAMPLE")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	betas := []string{"oauth-2025-04-20", "context-1m-2025-08-07", "fine-grained-tool-streaming-2025-05-14"}
	for _, b := range betas {
		req.Header.Add("Anthropic-Beta", b)
	}

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	got := cap.snapshot()
	if v := got.header.Get("Authorization"); v != "Bearer sk-ant-oat01-EXAMPLE" {
		t.Errorf("Authorization = %q, want the caller's bearer token verbatim", v)
	}
	gotBetas := got.header.Values("Anthropic-Beta")
	if len(gotBetas) != len(betas) {
		t.Fatalf("anthropic-beta arrived as %d value(s) %q, want %d separate values (never joined)",
			len(gotBetas), gotBetas, len(betas))
	}
	for i := range betas {
		if gotBetas[i] != betas[i] {
			t.Errorf("anthropic-beta[%d] = %q, want %q", i, gotBetas[i], betas[i])
		}
	}
}

func TestStripsHopByHopRequestHeaders(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Authorization", "Basic bogus")
	req.Header.Set("Proxy-Authenticate", "Basic realm=x")
	req.Header.Set("Te", "trailers")
	req.Header.Set("Trailer", "X-Trailing")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("X-Keep-Me", "yes")

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	got := cap.snapshot()
	for _, h := range []string{"Keep-Alive", "Proxy-Authorization", "Proxy-Authenticate", "Te", "Trailer", "Upgrade"} {
		if v := got.header.Values(h); len(v) != 0 {
			t.Errorf("hop-by-hop header %s leaked upstream as %q", h, v)
		}
	}
	if v := got.header.Get("X-Keep-Me"); v != "yes" {
		t.Errorf("X-Keep-Me = %q, want yes (end-to-end headers must survive)", v)
	}
}

// copyRequestHeaders is unit-tested directly for the Connection-token and
// Content-Length rules, which a Go http.Client will not reliably emit.
func TestCopyRequestHeadersDropsConnectionTokensAndContentLength(t *testing.T) {
	src := http.Header{}
	src.Set("Connection", "X-Hop-One, X-Hop-Two")
	src.Set("X-Hop-One", "a")
	src.Set("X-Hop-Two", "b")
	src.Set("Content-Length", "999")
	src.Set("Authorization", "Bearer keep")
	src.Add("Anthropic-Beta", "one")
	src.Add("Anthropic-Beta", "two")

	dst := http.Header{}
	copyRequestHeaders(dst, src)

	for _, h := range []string{"Connection", "X-Hop-One", "X-Hop-Two", "Content-Length"} {
		if v := dst.Values(h); len(v) != 0 {
			t.Errorf("%s should have been dropped, got %q", h, v)
		}
	}
	if v := dst.Get("Authorization"); v != "Bearer keep" {
		t.Errorf("Authorization = %q, want Bearer keep", v)
	}
	if v := dst.Values("Anthropic-Beta"); len(v) != 2 || v[0] != "one" || v[1] != "two" {
		t.Errorf("Anthropic-Beta = %q, want two separate values", v)
	}
	// The copy must be independent of the source slice.
	src["Anthropic-Beta"][0] = "mutated"
	if dst.Values("Anthropic-Beta")[0] != "one" {
		t.Error("copied header values alias the source slice")
	}
}

func TestStripsHopByHopResponseHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Request-Id", "req_123")
		w.Header().Add("Anthropic-Ratelimit-Requests-Remaining", "42")
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, "brewing")
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	resp, err := noRedirectClient().Get(proxy.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418", resp.StatusCode)
	}
	if v := resp.Header.Get("Keep-Alive"); v != "" {
		t.Errorf("hop-by-hop Keep-Alive leaked downstream: %q", v)
	}
	if v := resp.Header.Get("X-Request-Id"); v != "req_123" {
		t.Errorf("X-Request-Id = %q, want req_123", v)
	}
	if v := resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining"); v != "42" {
		t.Errorf("rate-limit header = %q, want 42", v)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "brewing" {
		t.Errorf("body = %q, want brewing", b)
	}
}

func TestDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect-target", func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		io.WriteString(w, "SHOULD NOT BE FETCHED")
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/redirect-target")
		w.WriteHeader(http.StatusFound)
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	resp, err := noRedirectClient().Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 relayed to the caller", resp.StatusCode)
	}
	if v := resp.Header.Get("Location"); v != "/redirect-target" {
		t.Errorf("Location = %q, want /redirect-target", v)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target was fetched %d time(s); the leg must never follow a redirect", n)
	}
}

func TestSSEIsFlushedIncrementally(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_ = rc.Flush()
		// Hold the stream open. If the leg buffers, the caller cannot see the
		// first frame until this returns.
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("caller never reported the first SSE frame")
		}
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		_ = rc.Flush()
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	resp, err := noRedirectClient().Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	type readResult struct {
		line string
		err  error
	}
	first := make(chan readResult, 1)
	go func() {
		l, e := br.ReadString('\n')
		first <- readResult{l, e}
	}()

	select {
	case got := <-first:
		if got.err != nil {
			t.Fatalf("reading first SSE line: %v", got.err)
		}
		if !strings.Contains(got.line, "message_start") {
			t.Fatalf("first line = %q, want the message_start frame", got.line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE frame did not arrive while the stream was still open: not flushed incrementally")
	}

	releaseOnce()
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading rest of stream: %v", err)
	}
	if !strings.Contains(string(rest), "message_stop") {
		t.Errorf("rest of stream = %q, want the message_stop frame", rest)
	}
}

func TestCountTokensPassthrough(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":17}`)
	}))
	defer upstream.Close()

	leg := newTestLeg(t, upstream.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := leg.CountTokens(w, r, nil); err != nil {
			t.Errorf("CountTokens: %v", err)
		}
	}))
	defer proxy.Close()

	body := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`
	resp, err := noRedirectClient().Post(proxy.URL+"/v1/messages/count_tokens", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	got := cap.snapshot()
	if got.path != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q, want /v1/messages/count_tokens", got.path)
	}
	if string(got.body) != body {
		t.Errorf("body = %q, want %q", got.body, body)
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(b)) != `{"input_tokens":17}` {
		t.Errorf("response = %q, want the upstream JSON verbatim", b)
	}
}

func TestNewRejectsBadBaseURL(t *testing.T) {
	tr := transport.NewStd(transport.DefaultOptions())
	for _, bad := range []string{"", "api.anthropic.com", "ftp://api.anthropic.com", "https://"} {
		if _, err := New(bad, tr); err == nil {
			t.Errorf("New(%q) succeeded, want an error", bad)
		}
	}
	if _, err := New("https://api.anthropic.com", nil); err == nil {
		t.Error("New with a nil transport succeeded, want an error")
	}
}

func TestNewNormalizesBaseURL(t *testing.T) {
	tr := transport.NewStd(transport.DefaultOptions())
	l, err := New("https://user:pw@api.anthropic.com/gw/?a=1#frag", tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := l.BaseURL(), "https://api.anthropic.com/gw"; got != want {
		t.Errorf("BaseURL() = %q, want %q (userinfo, query, fragment, trailing slash all dropped)", got, want)
	}
}

// TestLocalTokenHeaderIsNotForwardedUpstream pins the trust boundary: the
// loopback shared secret authorizes a local process to spend the user's
// subscriptions. Sending it to a third party on every request would disclose
// utraque's own credential.
func TestLocalTokenHeaderIsNotForwardedUpstream(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(localTokenHeader, "SUPER-SECRET-LOCAL-TOKEN")
	req.Header.Set("Authorization", "Bearer keep-me")

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	got := cap.snapshot()
	if v := got.header.Values(localTokenHeader); len(v) != 0 {
		t.Errorf("%s leaked upstream as %q", localTokenHeader, v)
	}
	if v := got.header.Get("Authorization"); v != "Bearer keep-me" {
		t.Errorf("Authorization = %q, want it forwarded verbatim", v)
	}
}

// TestLocalHeaderNamesMatchServer keeps the duplicated constants honest.
func TestLocalHeaderNamesMatchServer(t *testing.T) {
	if localTokenHeader != server.LocalTokenHeader {
		t.Errorf("localTokenHeader = %q, want server.LocalTokenHeader %q", localTokenHeader, server.LocalTokenHeader)
	}
	if responseIDHeader != server.ResponseIDHeader {
		t.Errorf("responseIDHeader = %q, want server.ResponseIDHeader %q", responseIDHeader, server.ResponseIDHeader)
	}
}

// TestUpstreamCannotOverwriteProxyRequestID: the id the client sees and the id
// in our logs must be the same one.
func TestUpstreamCannotOverwriteProxyRequestID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(responseIDHeader, "upstream-forged-id")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	leg := newTestLeg(t, upstream.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(responseIDHeader, "proxy-minted-id")
		leg.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	resp, err := noRedirectClient().Get(proxy.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get(responseIDHeader); got != "proxy-minted-id" {
		t.Errorf("%s = %q, want the proxy's own id", responseIDHeader, got)
	}
}

// TestDropsProxyConnectionAndInventsNoUserAgent covers two fidelity rules: a
// legacy hop-by-hop header must not be relayed, and a caller that sent no
// User-Agent must reach upstream with none rather than with net/http's default.
func TestDropsProxyConnectionAndInventsNoUserAgent(t *testing.T) {
	cap := &capture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL))
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("User-Agent", "") // Go omits the header entirely for "".

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	got := cap.snapshot()
	if v := got.header.Values("Proxy-Connection"); len(v) != 0 {
		t.Errorf("Proxy-Connection leaked upstream as %q", v)
	}
	if v := got.header.Get("User-Agent"); v != "" {
		t.Errorf("User-Agent = %q, want none invented for a caller that sent none", v)
	}
}

// TestUpstreamIdleTimeoutBeforeHeaders asserts a silent upstream is a 504
// timeout_error, not the blanket 502, and that the leg does not wait forever.
func TestUpstreamIdleTimeoutBeforeHeaders(t *testing.T) {
	stall := make(chan struct{})
	defer close(stall)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL, WithUpstreamIdleTimeout(150*time.Millisecond)))
	defer proxy.Close()

	start := time.Now()
	resp, err := noRedirectClient().Get(proxy.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s; the idle deadline did not fire", elapsed)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), string(apierr.TypeTimeout)) {
		t.Errorf("body = %s, want a %s envelope", body, apierr.TypeTimeout)
	}
}

// TestUpstreamIdleTimeoutMidStream asserts the deadline keeps applying after
// the headers arrive: a stream that goes silent must be torn down rather than
// pinning the request, its goroutine and its idle-timer hold indefinitely.
func TestUpstreamIdleTimeoutMidStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		_ = rc.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newTestLeg(t, upstream.URL, WithUpstreamIdleTimeout(150*time.Millisecond)))
	defer proxy.Close()

	start := time.Now()
	resp, err := noRedirectClient().Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("stream stayed open for %s; the rolling idle deadline never fired", elapsed)
	}
	if !strings.Contains(string(got), "message_start") {
		t.Errorf("relayed body = %q, want the frame that did arrive before the stall", got)
	}
}

// TestSlowButLiveStreamSurvivesTheIdleDeadline is the other half of the
// contract: the bound is on silence, not on total duration.
func TestSlowButLiveStreamSurvivesTheIdleDeadline(t *testing.T) {
	const frames = 6
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for i := range frames {
			io.WriteString(w, "event: ping\ndata: {}\n\n")
			_ = rc.Flush()
			if i < frames-1 {
				time.Sleep(40 * time.Millisecond)
			}
		}
	}))
	defer upstream.Close()

	// Total run (~200ms) far exceeds the 150ms bound; no single gap does.
	proxy := httptest.NewServer(newTestLeg(t, upstream.URL, WithUpstreamIdleTimeout(150*time.Millisecond)))
	defer proxy.Close()

	resp, err := noRedirectClient().Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if n := strings.Count(string(got), "event: ping"); n != frames {
		t.Errorf("relayed %d frames, want %d: a live-but-slow stream was cut", n, frames)
	}
}

// TestOversizedBodyIsRequestTooLarge covers the catch-all path, where the body
// is read by the leg itself. An over-limit chunked body arrives as
// *http.MaxBytesError and must be a 413, matching the routed path.
func TestOversizedBodyIsRequestTooLarge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be contacted for an over-limit body")
	}))
	defer upstream.Close()

	const limit = 16
	leg := newTestLeg(t, upstream.URL, WithMaxBodyBytes(limit))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		leg.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	// A chunked request: no Content-Length for anything to pre-check against.
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/models",
		io.NopCloser(strings.NewReader(strings.Repeat("x", 4096))))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), string(apierr.TypeRequestTooLarge)) {
		t.Errorf("body = %s, want a %s envelope", body, apierr.TypeRequestTooLarge)
	}
}

// TestUpstreamErrorDoesNotLogTheQueryString: an upstream transport failure
// stringifies as a *url.Error carrying the caller's URL. A query-string
// credential must not ride into the log through the error's cause.
func TestUpstreamErrorDoesNotLogTheQueryString(t *testing.T) {
	// A listener that is closed immediately, so dialling it always fails.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	leg := newTestLeg(t, deadURL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := leg.forward(w, r, nil)
		if err == nil {
			t.Error("forward succeeded against a dead upstream")
			return
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("query-string secret reached the error text: %v", err)
		}
		_ = apierr.Write(w, err)
	}))
	defer proxy.Close()

	resp, err := noRedirectClient().Get(proxy.URL + "/v1/models?key=hunter2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
}
