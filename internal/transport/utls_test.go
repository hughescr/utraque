package transport

import (
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every test here talks to an httptest server on loopback. Nothing in this
// package may contact chatgpt.com, auth.openai.com or api.anthropic.com.

// tlsRoots returns a pool trusting only srv's own certificate, which is how a
// test exercises a real uTLS handshake without weakening verification.
func tlsRoots(t *testing.T, srv *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// newTLSServer starts an HTTPS test server, optionally offering h2 at ALPN.
func newTLSServer(t *testing.T, http2 bool, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = http2
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestNewUTLSShape(t *testing.T) {
	tr := NewUTLS(DefaultOptions())
	if tr.Kind() != KindUTLS {
		t.Errorf("Kind() = %q, want %q", tr.Kind(), KindUTLS)
	}
	c := tr.Client()
	if c == nil {
		t.Fatal("Client() = nil")
	}
	if c != tr.Client() {
		t.Error("Client() returned a different client on the second call; the pool must be shared")
	}
	if c.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (an overall deadline would cut SSE streams)", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect = nil, want the no-redirect policy")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect returned %v, want http.ErrUseLastResponse", err)
	}
}

// TestUTLSChromeALPNIsOffered pins the ALPN list, which is part of the
// fingerprint this transport exists to present: JA4 hashes the first offered
// protocol, so quietly dropping h2 would defeat the whole point.
func TestUTLSChromeALPNIsOffered(t *testing.T) {
	if len(chromeALPN) != 2 || chromeALPN[0] != "h2" || chromeALPN[1] != "http/1.1" {
		t.Errorf("chromeALPN = %v, want [h2 http/1.1]", chromeALPN)
	}
}

// TestUTLSNegotiatesHTTP2 is the real thing: a Chrome-shaped ClientHello
// against an h2-capable server, and the request must come out the far side as
// HTTP/2 rather than silently degrading.
func TestUTLSNegotiatesHTTP2(t *testing.T) {
	var proto atomic.Value
	srv := newTLSServer(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto.Store(r.Proto)
		io.WriteString(w, "ok")
	}))

	opts := DefaultOptions()
	opts.RootCAs = tlsRoots(t, srv)
	resp, err := NewUTLS(opts).Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	if got := proto.Load(); got != "HTTP/2.0" {
		t.Errorf("server saw %v, want HTTP/2.0", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestUTLSFallsBackToHTTP11 covers the other branch: a server that declines h2
// at ALPN must be demoted to HTTP/1.1 rather than left hanging on a protocol
// mismatch, and the demotion must stick.
func TestUTLSFallsBackToHTTP11(t *testing.T) {
	var protos struct {
		sync.Mutex
		seen []string
	}
	srv := newTLSServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protos.Lock()
		protos.seen = append(protos.seen, r.Proto)
		protos.Unlock()
		io.WriteString(w, "ok")
	}))

	opts := DefaultOptions()
	opts.RootCAs = tlsRoots(t, srv)
	tr := NewUTLS(opts)

	for i := range 2 {
		resp, err := tr.Client().Get(srv.URL)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("read body %d: %v", i, err)
		}
		resp.Body.Close()
	}

	protos.Lock()
	seen := append([]string(nil), protos.seen...)
	protos.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests, want 2: %v", len(seen), seen)
	}
	for i, p := range seen {
		if p != "HTTP/1.1" {
			t.Errorf("request %d arrived as %s, want HTTP/1.1", i, p)
		}
	}

	// The demotion must be remembered, or every request pays a doomed h2 dial.
	rt := tr.Client().Transport.(*utlsRoundTripper)
	key := authorityOf(mustURL(t, srv.URL))
	if !rt.isH1Only(key) {
		t.Errorf("authority %q was not recorded as HTTP/1.1-only", key)
	}
}

// TestUTLSPostSurvivesProtocolFallback is the case a naive fallback breaks: the
// h2 attempt must not consume the body the HTTP/1.1 retry then needs.
func TestUTLSPostSurvivesProtocolFallback(t *testing.T) {
	var got atomic.Value
	srv := newTLSServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(string(b))
		io.WriteString(w, "ok")
	}))

	opts := DefaultOptions()
	opts.RootCAs = tlsRoots(t, srv)

	const payload = `{"model":"gpt-5","stream":true}`
	resp, err := NewUTLS(opts).Client().Post(srv.URL, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if v := got.Load(); v != payload {
		t.Errorf("upstream received %q, want %q", v, payload)
	}
}

func TestNewUTLSClientDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		io.WriteString(w, "followed")
	})
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	srv := newTLSServer(t, true, mux)

	opts := DefaultOptions()
	opts.RootCAs = tlsRoots(t, srv)
	resp, err := NewUTLS(opts).Client().Get(srv.URL + "/start")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/target" {
		t.Errorf("Location = %q, want /target", got)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target fetched %d time(s), want 0", n)
	}
}

// TestUTLSHTTP2ResponseHeaderTimeout pins the parity that x/net/http2 does not
// provide for free: a peer that accepts the request and then says nothing must
// not pin the request forever, exactly as http.Transport.ResponseHeaderTimeout
// guarantees on the HTTP/1.1 side.
func TestUTLSHTTP2ResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := newTLSServer(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release) })

	opts := DefaultOptions()
	opts.RootCAs = tlsRoots(t, srv)
	opts.ResponseHeaderTimeout = 150 * time.Millisecond

	start := time.Now()
	resp, err := NewUTLS(opts).Client().Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("get succeeded, want a header-timeout failure")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("header timeout took %v, want it bounded near 150ms", elapsed)
	}
}

// TestUTLSPlainHTTPStillWorks: an http:// base URL has no ClientHello to shape,
// so it must simply behave like an ordinary client rather than failing.
func TestUTLSPlainHTTPStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain")
	}))
	defer srv.Close()

	resp, err := NewUTLS(DefaultOptions()).Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "plain" {
		t.Errorf("body = %q, want %q", b, "plain")
	}
}

func TestAuthorityOf(t *testing.T) {
	cases := map[string]string{
		"https://chatgpt.com/backend-api/codex": "chatgpt.com:443",
		"https://chatgpt.com:8443/x":            "chatgpt.com:8443",
		"http://127.0.0.1/x":                    "127.0.0.1:80",
		"http://127.0.0.1:9999/x":               "127.0.0.1:9999",
	}
	for raw, want := range cases {
		if got := authorityOf(mustURL(t, raw)); got != want {
			t.Errorf("authorityOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
