package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/proxyhdr"
)

type browserCase struct {
	name, host, site, mode, rule string
	origins                      []string
}

var browserCases = []browserCase{
	{name: "plain", host: "localhost:8317"},
	{name: "different-port", host: "localhost:8317", origins: []string{"http://localhost:8000"}, rule: "origin"},
	{name: "exact-origin", host: "localhost:8317", origins: []string{"http://localhost:8317"}, rule: "origin"},
	{name: "null", host: "localhost", origins: []string{"null"}, rule: "origin"},
	{name: "malformed-origin", host: "localhost", origins: []string{"not an origin"}, rule: "origin"},
	{name: "empty-origin", host: "localhost", origins: []string{""}, rule: "origin"},
	{name: "repeated-origin", host: "localhost", origins: []string{"", "private-marker"}, rule: "origin"},
	{name: "cross-site", host: "localhost", site: "cross-site", rule: "fetch-site"},
	{name: "same-site", host: "localhost", site: "same-site", rule: "fetch-site"},
	{name: "same-origin", host: "localhost", site: "same-origin"},
	{name: "none", host: "localhost", site: "none"},
	{name: "mode-only", host: "localhost", mode: "cors"},
	{name: "foreign", host: "foreign.example", rule: "host"},
	{name: "ip-prefix", host: "127.0.0.1.evil.example", rule: "host"},
	{name: "localhost-prefix", host: "localhost.evil.example", rule: "host"},
	{name: "upper", host: "LOCALHOST:8317"},
	{name: "ipv6", host: "[::1]:8317"},
	{name: "bare-ipv6", host: "::1"},
	{name: "bracketed-ipv6", host: "[::1]"},
	{name: "mapped-host", host: "[::ffff:127.0.0.1]", rule: "host"},
	{name: "other-loopback", host: "127.9.9.9"},
	{name: "missing-host", rule: "host"},
	// The configured Listen name is not an accepted Host on a loopback listener.
	{name: "listen-name", host: "proxy.test:8317", rule: "host"},
	{name: "listen-name-upper", host: "PROXY.test", rule: "host"},
}

type activityCount int

func (a *activityCount) Hold() func() { *a++; return func() {} }

type boundListener struct {
	net.Listener
	addr net.Addr
}

func (l boundListener) Addr() net.Addr { return l.addr }

func listenerContext(s *Server, ip string) context.Context {
	return s.HTTPServer(context.Background()).BaseContext(boundListener{addr: &net.TCPAddr{IP: netip.MustParseAddr(ip).AsSlice()}})
}

func TestBrowserGuardMatrix(t *testing.T) {
	for _, token := range []string{"unset", "right", "wrong"} {
		cfg := config.Default()
		cfg.Listen = "proxy.test:8317"
		if token != "unset" {
			cfg.LocalToken = "right"
		}
		var logs bytes.Buffer
		var holds activityCount
		var routed, upstream, health int
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { routed++; w.WriteHeader(204) })
		pass := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { upstream++; w.WriteHeader(202) })
		s, err := New(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Activity: &holds,
			Routes:      Routes{Messages: h, CountTokens: h, Models: h, ProviderReport: h, ProviderReportV2: h, Passthrough: pass},
			HealthExtra: func(context.Context) map[string]any { health++; return nil }})
		if err != nil {
			t.Fatal(err)
		}
		ctx := listenerContext(s, "127.0.0.1")
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/models", ProviderReportPath, ProviderReportV1Path, ProviderReportV2Path, HealthPath, "/unknown"} {
			for _, method := range []string{"GET", "HEAD", "POST"} {
				for _, tc := range browserCases {
					t.Run(token+path+"/"+method+"/"+tc.name, func(t *testing.T) {
						routed, upstream, health, holds = 0, 0, 0, 0
						logs.Reset()
						r := httptest.NewRequestWithContext(ctx, method, path+"?q=private-query", nil)
						r.Host = tc.host
						if token != "unset" {
							r.Header.Set(proxyhdr.LocalToken, token)
						}
						for _, o := range tc.origins {
							r.Header.Add("Origin", o)
						}
						if tc.site != "" {
							r.Header.Set("Sec-Fetch-Site", tc.site)
						}
						if tc.mode != "" {
							r.Header.Set("Sec-Fetch-Mode", tc.mode)
						}
						r.Header.Set("X-Forwarded-Host", "localhost")
						r.Header.Set("Sec-Fetch-Dest", "document")
						w := httptest.NewRecorder()
						s.ServeHTTP(w, r)
						if tc.rule != "" {
							if w.Code != 403 || w.Header().Get("Cache-Control") != "no-store" || routed+upstream+health+int(holds) != 0 {
								t.Fatalf("refusal: status %d, headers %v; routed/upstream/health/holds %d/%d/%d/%d", w.Code, w.Header(), routed, upstream, health, holds)
							}
							for k := range w.Header() {
								if strings.HasPrefix(k, "Access-Control-") {
									t.Fatalf("CORS header %q", k)
								}
							}
							var envelope struct {
								Type  string
								Error struct{ Type, Message string }
							}
							if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Type != "error" || envelope.Error.Type != "permission_error" || envelope.Error.Message == "" {
								t.Fatalf("wrong envelope: %s", w.Body)
							}
							if tc.rule == "host" && !strings.Contains(envelope.Error.Message, "http://127.0.0.1:PORT") {
								t.Fatalf("host refusal does not say what to use: %s", envelope.Error.Message)
							}
							if strings.Count(logs.String(), "\n") != 1 || !strings.Contains(logs.String(), "rule="+tc.rule) || !strings.Contains(logs.String(), "path="+path) {
								t.Fatalf("expected one rejection log: %s", &logs)
							}
							for _, value := range []string{"private-", "http://localhost:8000", "http://localhost:8317", "evil.example", "foreign.example", "proxy.test", "PROXY.test"} {
								if strings.Contains(logs.String(), value) || strings.Contains(w.Body.String(), value) {
									t.Fatalf("reflected %q", value)
								}
							}
							return
						}
						want := 204
						switch {
						case path == HealthPath && method == "POST":
							want = 405
						case path == HealthPath:
							want = 200
						case token == "wrong":
							want = 401
						case path == "/unknown" || path == "/v1/models" && method == "POST" || strings.HasPrefix(path, "/v1/messages") && method != "POST":
							want = 202
						}
						if w.Code != want {
							t.Fatalf("got %d, want %d", w.Code, want)
						}
					})
				}
			}
		}
	}
}

// Guard decisions depend on the listening socket, not the connection's local
// endpoint: a connection to a wildcard listener may have a loopback endpoint.
func TestBrowserGuardWildcard(t *testing.T) {
	cfg := config.Default()
	cfg.LocalToken = "right"
	s, err := New(Options{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(listenerContext(s, "0.0.0.0"), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	r := httptest.NewRequestWithContext(ctx, "GET", HealthPath, nil)
	r.Host = "foreign.example"
	for _, want := range []int{200, 403} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("got %d, want %d", w.Code, want)
		}
		r.Header.Set("Origin", "null")
	}
}

// A listener that is not positively classified as non-loopback must enforce
// the Host allowlist: a direct handler call carries neither the server's
// listener class nor a connection-local address, and must fail closed.
func TestBrowserGuardUnknownListenerAppliesHostCheck(t *testing.T) {
	s, err := New(Options{Config: config.Default(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	var reached int
	h := s.withBrowserGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		host string
		want int
	}{
		{"evil.example", http.StatusForbidden},
		{"", http.StatusForbidden},
		{"localhost", http.StatusNoContent},
		{"127.0.0.1:8317", http.StatusNoContent},
	} {
		reached = 0
		r := httptest.NewRequestWithContext(context.Background(), "GET", "/v1/models", nil)
		r.Host = tc.host
		if r.Context().Value(http.LocalAddrContextKey) != nil {
			t.Fatal("test context unexpectedly carries a local address")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want || (tc.want == http.StatusNoContent) != (reached == 1) {
			t.Errorf("Host %q on an unknown listener: status %d, handler reached %d times; want status %d", tc.host, w.Code, reached, tc.want)
		}
	}
}

// Mounted behind a plain http.Server (no BaseContext from HTTPServer), the
// only evidence of the listener is http.LocalAddrContextKey; a loopback
// socket there must enforce the Host check.
func TestBrowserGuardBehindPlainServerUsesLocalAddr(t *testing.T) {
	s, err := New(Options{Config: config.Default(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(s) // A plain http.Server on 127.0.0.1.
	defer front.Close()
	for _, tc := range []struct {
		host string
		want int
	}{
		{"evil.example", http.StatusForbidden},
		{"localhost", http.StatusOK},
		{"", http.StatusOK}, // The client fills in 127.0.0.1:port.
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+HealthPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.host != "" {
			req.Host = tc.host
		}
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("Host %q behind a plain server: got %d, want %d", tc.host, resp.StatusCode, tc.want)
		}
	}
}

// With no server-supplied listener class, a connection-local address that is
// positively non-loopback is the evidence that lifts the Host check (a routable
// or wildcard bind, where the local token is mandatory and remote clients use
// any Host); a loopback one keeps it.
func TestBrowserGuardLocalAddrFallbackClassifies(t *testing.T) {
	s, err := New(Options{Config: config.Default(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	h := s.withBrowserGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		local net.Addr
		want  int
	}{
		{&net.TCPAddr{IP: net.IPv4(192, 0, 2, 7).To4()}, http.StatusNoContent},
		{&net.TCPAddr{IP: net.IPv4zero.To4()}, http.StatusNoContent},
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1).To4()}, http.StatusForbidden},
		{&net.TCPAddr{IP: net.IPv6loopback}, http.StatusForbidden},
		{&net.UnixAddr{Name: "/nonexistent", Net: "unix"}, http.StatusForbidden}, // Unclassifiable.
	} {
		ctx := context.WithValue(context.Background(), http.LocalAddrContextKey, tc.local)
		r := httptest.NewRequestWithContext(ctx, "GET", "/v1/models", nil)
		r.Host = "foreign.example"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("foreign Host with local address %v: got %d, want %d", tc.local, w.Code, tc.want)
		}
	}
}

// The class HTTPServer's BaseContext records is the one the guard acts on; a
// listener whose address cannot be classified keeps the Host check.
func TestBrowserGuardServerListenerClass(t *testing.T) {
	s, err := New(Options{Config: config.Default(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	h := s.withBrowserGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		listener net.Addr
		want     int
	}{
		{&net.TCPAddr{IP: net.IPv4(192, 0, 2, 7).To4()}, http.StatusNoContent},
		{&net.TCPAddr{IP: net.IPv6unspecified}, http.StatusNoContent},
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1).To4()}, http.StatusForbidden},
		{&net.UnixAddr{Name: "/nonexistent", Net: "unix"}, http.StatusForbidden},
	} {
		ctx := s.HTTPServer(context.Background()).BaseContext(boundListener{addr: tc.listener})
		r := httptest.NewRequestWithContext(ctx, "GET", "/v1/models", nil)
		r.Host = "foreign.example"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("foreign Host on listener %v: got %d, want %d", tc.listener, w.Code, tc.want)
		}
	}
}

func TestAllowedHost(t *testing.T) {
	for _, tc := range []struct {
		authority string
		want      bool
	}{
		{"localhost", true},
		{"LOCALHOST:8317", true},
		{"127.0.0.1:8317", true},
		{"127.9.9.9", true},
		{"[::1]:8317", true},
		{"[::1]", true},
		{"::1", true},
		{"evil.example", false},
		{"proxy.test:9000", false},
		{"", false},
		// U+017F folds to "s" under strings.EqualFold, so only the explicit
		// non-ASCII rejection keeps this from matching.
		{"localhoſt", false},
		{"localhost:80\x00", false},
	} {
		if got := allowedHost(tc.authority); got != tc.want {
			t.Errorf("allowedHost(%q) = %t, want %t", tc.authority, got, tc.want)
		}
	}
}

func TestBrowserGuardOnWire(t *testing.T) {
	s, err := New(Options{Config: config.Default(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewUnstartedServer(nil)
	front.Config = s.HTTPServer(t.Context())
	front.Start()
	defer front.Close()
	for _, tc := range []struct {
		request string
		want    int
	}{
		{"GET /healthz HTTP/1.0\r\n", 403},
		{"GET /unknown HTTP/1.0\r\nHost: localhost\r\n", 404},
		{"GET /unknown HTTP/1.0\r\nHost: foreign.example\r\n", 403},
		{"OPTIONS * HTTP/1.0\r\nHost: localhost\r\n", 200},
		{"OPTIONS * HTTP/1.0\r\nHost: localhost\r\nOrigin:\r\n", 403},
		{"GET /healthz HTTP/1.0\r\nHost: localhost\r\nSec-Fetch-Site: none\r\nSec-Fetch-Site: cross-site\r\n", 403},
	} {
		conn, err := net.Dial("tcp", front.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprint(conn, tc.request+"\r\n"); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("request %q: got %d, want %d", tc.request, resp.StatusCode, tc.want)
		}
	}
}

func FuzzBrowserGuard(f *testing.F) {
	f.Add("", false, "", true, "localhost")
	f.Add("", false, "unknown", true, "localhost")
	f.Add("", false, "none\ncross-site", true, "localhost")
	for _, tc := range browserCases {
		f.Add(strings.Join(tc.origins, "\n"), tc.origins != nil, tc.site, tc.site != "", tc.host)
	}
	cfg := config.Default()
	cfg.Listen = "proxy.test:8317"
	s, err := New(Options{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		f.Fatal(err)
	}
	ctx := listenerContext(s, "::1")
	f.Fuzz(func(t *testing.T, origin string, hasOrigin bool, site string, hasSite bool, host string) {
		r := httptest.NewRequestWithContext(ctx, "GET", HealthPath, nil)
		r.Host = host
		if hasOrigin {
			r.Header["Origin"] = strings.SplitN(origin, "\n", 2)
		}
		if hasSite {
			r.Header["Sec-Fetch-Site"] = strings.SplitN(site, "\n", 2)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code == 403 {
			return
		}
		// Independent positive oracle; never ask the guard if its decision was right.
		h := (&url.URL{Host: host}).Hostname()
		if net.ParseIP(host) != nil { // url.Hostname expects brackets on IPv6.
			h = host
		}
		ip := net.ParseIP(h)
		if w.Code != 200 || hasOrigin || hasSite && site != "same-origin" && site != "none" ||
			!(strings.EqualFold(h, "localhost") || ip.IsLoopback() && (!strings.Contains(h, ":") || ip.To4() == nil)) {
			t.Fatalf("unexpected pass: Host=%q, Origin present=%t, site=%q present=%t", host, hasOrigin, site, hasSite)
		}
	})
}

func BenchmarkBrowserGuard(b *testing.B) {
	s, err := New(Options{Config: config.Default(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	h := s.withBrowserGuard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequestWithContext(listenerContext(s, "127.0.0.1"), "POST", "http://127.0.0.1:8317/v1/messages", nil)
	r.Header.Set("Sec-Fetch-Mode", "cors")
	w := httptest.NewRecorder()
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(w, r)
	}
}
