package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/providerreport"
	"github.com/hughescr/utraque/internal/proxyhdr"
	"github.com/hughescr/utraque/internal/server"
	"github.com/hughescr/utraque/internal/usagehistory"
)

const localSecret = "loopback-shared-secret"

func newServer(t *testing.T, mutate func(*server.Options)) (*server.Server, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	opts := server.Options{
		Config:  config.Default(),
		Logger:  slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Version: "1.2.3-test",
	}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := server.New(opts)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return s, &buf
}

func do(t *testing.T, s *server.Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func decodeErrorEnvelope(t *testing.T, body []byte) aschema.ErrorEvent {
	t.Helper()
	var ev aschema.ErrorEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatalf("decoding the error envelope %q: %v", body, err)
	}
	if ev.Type != "error" {
		t.Errorf("envelope type = %q, want error", ev.Type)
	}
	return ev
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	bad := config.Default()
	bad.Listen = ""
	if _, err := server.New(server.Options{Config: bad}); err == nil {
		t.Fatal("New must reject an invalid config")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	s, err := server.New(server.Options{Config: config.Default()})
	if err != nil {
		t.Fatal(err)
	}
	if s.Version() != "0.0.0-dev" {
		t.Errorf("Version = %q, want the default", s.Version())
	}
	if s.Logger() == nil || s.Handler() == nil {
		t.Error("Logger and Handler must be non-nil")
	}
}

func TestHealthz(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	clock := start
	s, _ := newServer(t, func(o *server.Options) {
		o.Config.LocalToken = localSecret
		o.Now = func() time.Time { return clock }
	})
	clock = start.Add(90 * time.Second)

	w := do(t, s, httptest.NewRequest(http.MethodGet, server.HealthPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	// The exact bytes are part of the contract: sorted keys, uptime_s as a
	// bare number, one trailing newline.
	if want := "{\"status\":\"ok\",\"uptime_s\":90,\"version\":\"1.2.3-test\"}\n"; w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}
	var got server.HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if got.Status != server.StatusOK {
		t.Errorf("status = %q", got.Status)
	}
	if got.Version != "1.2.3-test" {
		t.Errorf("version = %q", got.Version)
	}
	if got.UptimeS != 90 {
		t.Errorf("uptime_s = %v, want 90", got.UptimeS)
	}
	if strings.Contains(w.Body.String(), localSecret) {
		t.Fatalf("/healthz leaked the local token: %s", w.Body.String())
	}
}

func TestHealthzHead(t *testing.T) {
	s, _ := newServer(t, nil)
	w := do(t, s, httptest.NewRequest(http.MethodHead, server.HealthPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD returned a body: %q", w.Body.String())
	}
	if w.Header().Get("Content-Length") == "" {
		t.Error("HEAD must still declare Content-Length")
	}
}

func TestHealthzExtraCannotOverrideReserved(t *testing.T) {
	s, _ := newServer(t, func(o *server.Options) {
		o.HealthExtra = func(context.Context) map[string]any {
			return map[string]any{"status": "hacked", "version": "evil", "codex_auth": "ok", "unknown_events": 3}
		}
	})
	w := do(t, s, httptest.NewRequest(http.MethodGet, server.HealthPath, nil))
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["status"] != server.StatusOK {
		t.Errorf("status = %v, extras must not override reserved keys", m["status"])
	}
	if m["version"] != "1.2.3-test" {
		t.Errorf("version = %v, extras must not override reserved keys", m["version"])
	}
	if m["codex_auth"] != "ok" {
		t.Errorf("extras not merged: %v", m)
	}
}

func TestHealthzMethodNotAllowed(t *testing.T) {
	s, _ := newServer(t, nil)
	w := do(t, s, httptest.NewRequest(http.MethodPost, server.HealthPath, nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
	decodeErrorEnvelope(t, w.Body.Bytes())
}

func TestRequestIDGeneratedAndEchoed(t *testing.T) {
	s, buf := newServer(t, nil)
	w := do(t, s, httptest.NewRequest(http.MethodGet, server.HealthPath, nil))
	id := w.Header().Get(proxyhdr.RequestID)
	if id == "" {
		t.Fatal("no response id header")
	}
	if !strings.Contains(buf.String(), id) {
		t.Errorf("request id %q missing from the log: %s", id, buf.String())
	}

	second := do(t, s, httptest.NewRequest(http.MethodGet, server.HealthPath, nil))
	if second.Header().Get(proxyhdr.RequestID) == id {
		t.Error("generated request ids must be unique")
	}

	r := httptest.NewRequest(http.MethodGet, server.HealthPath, nil)
	r.Header.Set(server.RequestIDHeader, "caller-supplied-id")
	if got := do(t, s, r).Header().Get(proxyhdr.RequestID); got != "caller-supplied-id" {
		t.Errorf("inbound id not honoured: %q", got)
	}

	for _, bad := range []string{"bad id with spaces", strings.Repeat("x", 200), "tab\there"} {
		r := httptest.NewRequest(http.MethodGet, server.HealthPath, nil)
		r.Header.Set(server.RequestIDHeader, bad)
		if got := do(t, s, r).Header().Get(proxyhdr.RequestID); got == bad {
			t.Errorf("malformed inbound id %q must be replaced", bad)
		}
	}
}

// The request id is echoed to the caller, written to the request line, and used
// to name a trace file. A caller that puts something credential-shaped in the
// header does not get it honoured, and it does not reach the log.
func TestCredentialShapedRequestIDIsNotHonoured(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJleHAiOjIwMDAwMDAwMDAsInN1YiI6ImZha2UifQ." +
		"c2lnbmF0dXJlLXRoYXQtaXMtbm90LXJlYWw"

	for _, bad := range []string{jwt, "sk-ant-oat01-CALLERSECRETVALUE0123"} {
		s, buf := newServer(t, nil)
		r := httptest.NewRequest(http.MethodGet, server.HealthPath, nil)
		r.Header.Set(server.RequestIDHeader, bad)
		got := do(t, s, r).Header().Get(proxyhdr.RequestID)
		if got == bad {
			t.Errorf("a credential-shaped inbound id %q was honoured", bad)
		}
		if got == "" {
			t.Error("rejecting the inbound id must still yield a generated one")
		}
		if strings.Contains(buf.String(), bad) {
			t.Errorf("a credential-shaped inbound id reached the log: %s", buf.String())
		}
	}
}

func TestAccessLogRedactsAuthorization(t *testing.T) {
	s, buf := newServer(t, nil)
	r := httptest.NewRequest(http.MethodGet, server.HealthPath+"?key=sk-in-query", nil)
	r.Header.Set("Authorization", "Bearer sk-ant-oat01-LEAKME")
	r.Header.Set("Content-Type", "application/json")
	do(t, s, r)
	out := buf.String()
	if strings.Contains(out, "LEAKME") {
		t.Fatalf("the access log leaked the credential: %s", out)
	}
	if strings.Contains(out, "sk-in-query") {
		t.Fatalf("the access log leaked the query string: %s", out)
	}
	if !strings.Contains(out, "application/json") {
		t.Errorf("an allowlisted header is missing from the log: %s", out)
	}
	if !strings.Contains(out, `"status":200`) {
		t.Errorf("status not logged: %s", out)
	}
}

func TestLocalTokenGate(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	s, _ := newServer(t, func(o *server.Options) {
		o.Config.LocalToken = localSecret
		o.Routes.Passthrough = echo
	})

	w := do(t, s, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d", w.Code)
	}
	ev := decodeErrorEnvelope(t, w.Body.Bytes())
	if ev.Error.Type != "authentication_error" {
		t.Errorf("error type = %q", ev.Error.Type)
	}
	if strings.Contains(w.Body.String(), localSecret) {
		t.Fatal("the 401 body leaked the token")
	}

	for _, wrong := range []string{"wrong", localSecret + "x", localSecret[:5]} {
		r := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
		r.Header.Set(proxyhdr.LocalToken, wrong)
		if w := do(t, s, r); w.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", wrong, w.Code)
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	r.Header.Set(proxyhdr.LocalToken, localSecret)
	if w := do(t, s, r); w.Code != http.StatusOK {
		t.Fatalf("correct token: status = %d, body = %s", w.Code, w.Body.String())
	}

	if w := do(t, s, httptest.NewRequest(http.MethodGet, server.HealthPath, nil)); w.Code != http.StatusOK {
		t.Fatalf("/healthz must be exempt from the token gate: %d", w.Code)
	}
}

func TestAuthorizationHeaderIsNotTheLocalToken(t *testing.T) {
	// The client's Authorization header holds the Anthropic OAuth credential
	// and must never be accepted as, or consumed by, the local-token gate.
	var seen string
	s, _ := newServer(t, func(o *server.Options) {
		o.Config.LocalToken = localSecret
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		})
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	r.Header.Set("Authorization", "Bearer "+localSecret)
	if w := do(t, s, r); w.Code != http.StatusUnauthorized {
		t.Fatalf("Authorization must not satisfy the local-token gate: %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	r.Header.Set("Authorization", "Bearer upstream-oauth-token")
	r.Header.Set(proxyhdr.LocalToken, localSecret)
	if w := do(t, s, r); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if seen != "Bearer upstream-oauth-token" {
		t.Errorf("Authorization reached the handler as %q, want it untouched", seen)
	}
}

func TestNoLocalTokenMeansOpen(t *testing.T) {
	s, _ := newServer(t, func(o *server.Options) {
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	})
	if w := do(t, s, httptest.NewRequest(http.MethodGet, "/x", nil)); w.Code != http.StatusTeapot {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestProviderReportPathNeverFallsThrough(t *testing.T) {
	var reports, relays int
	s, _ := newServer(t, func(o *server.Options) {
		o.Config.LocalToken = localSecret
		o.Routes.ProviderReport = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reports++
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { relays++; w.WriteHeader(http.StatusTeapot) })
	})
	for _, tc := range []struct {
		method string
		status int
	}{{http.MethodGet, 200}, {http.MethodHead, 200}, {http.MethodPost, 405}, {http.MethodPatch, 405}} {
		r := httptest.NewRequest(tc.method, server.ProviderReportPath, nil)
		r.Header.Set(proxyhdr.LocalToken, localSecret)
		if w := do(t, s, r); w.Code != tc.status {
			t.Fatalf("%s status=%d want %d", tc.method, w.Code, tc.status)
		}
	}
	if reports != 4 || relays != 0 {
		t.Fatalf("reports=%d relays=%d", reports, relays)
	}
	unauthorized := httptest.NewRequest(http.MethodGet, server.ProviderReportPath, nil)
	if w := do(t, s, unauthorized); w.Code != http.StatusUnauthorized || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unauthorized report status=%d Cache-Control=%q", w.Code, w.Header().Get("Cache-Control"))
	}
}

// TestProviderReportVersionedPaths pins the route layout: schema 1 at
// /utraque/providers/v1 and, byte-identical, at the deprecated
// /v1/utraque/providers alias; schema 2 at /utraque/providers/v2; every
// report path under the same local-token and no-store rules; /healthz
// untouched by any of it.
func TestProviderReportVersionedPaths(t *testing.T) {
	v1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Report-Schema", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"schema_version":1,"path":%q}`, "same for every alias")
	})
	v2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"schema_version":2}`)
	})
	s, _ := newServer(t, func(o *server.Options) {
		o.Config.LocalToken = localSecret
		o.Routes.ProviderReport = v1
		o.Routes.ProviderReportV2 = v2
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	})
	if server.ProviderReportV1Path != "/utraque/providers/v1" || server.ProviderReportV2Path != "/utraque/providers/v2" || server.ProviderReportPath != "/v1/utraque/providers" {
		t.Fatalf("paths %q %q %q", server.ProviderReportV1Path, server.ProviderReportV2Path, server.ProviderReportPath)
	}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(proxyhdr.LocalToken, localSecret)
		return do(t, s, r)
	}
	canonical := get(server.ProviderReportV1Path)
	alias := get(server.ProviderReportPath)
	second := get(server.ProviderReportV2Path)
	for path, w := range map[string]*httptest.ResponseRecorder{server.ProviderReportV1Path: canonical, server.ProviderReportPath: alias, server.ProviderReportV2Path: second} {
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s status=%d Cache-Control=%q body=%s", path, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
		}
	}
	if !bytes.Equal(canonical.Body.Bytes(), alias.Body.Bytes()) {
		t.Fatalf("alias body differs:\n%s\n%s", canonical.Body.String(), alias.Body.String())
	}
	for _, h := range []string{"Content-Type", "X-Report-Schema", "Cache-Control"} {
		if canonical.Header().Get(h) != alias.Header().Get(h) {
			t.Fatalf("alias header %s differs: %q vs %q", h, canonical.Header().Get(h), alias.Header().Get(h))
		}
	}
	if !strings.Contains(canonical.Body.String(), `"schema_version":1`) || !strings.Contains(second.Body.String(), `"schema_version":2`) {
		t.Fatalf("wrong handler on a path: v1=%s v2=%s", canonical.Body.String(), second.Body.String())
	}
	for _, path := range []string{server.ProviderReportV1Path, server.ProviderReportPath, server.ProviderReportV2Path} {
		unauthorized := httptest.NewRequest(http.MethodGet, path, nil)
		if w := do(t, s, unauthorized); w.Code != http.StatusUnauthorized || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s without token status=%d Cache-Control=%q", path, w.Code, w.Header().Get("Cache-Control"))
		}
		post := httptest.NewRequest(http.MethodPost, path, nil)
		post.Header.Set(proxyhdr.LocalToken, localSecret)
		if w := do(t, s, post); w.Code == http.StatusTeapot {
			t.Fatalf("%s POST fell through to passthrough", path)
		}
	}
	// /healthz is unaffected: still token-exempt, same fields.
	health := do(t, s, httptest.NewRequest(http.MethodGet, server.HealthPath, nil))
	if health.Code != http.StatusOK {
		t.Fatalf("/healthz status=%d", health.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil || body["version"] != "1.2.3-test" || body["status"] != "ok" || len(body) != 3 {
		t.Fatalf("/healthz body=%s err=%v", health.Body.String(), err)
	}
	// Only the report handlers mount on report paths: with neither set, the
	// paths are ordinary unmatched routes.
	bare, _ := newServer(t, nil)
	for _, path := range []string{server.ProviderReportV1Path, server.ProviderReportPath, server.ProviderReportV2Path} {
		if w := do(t, bare, httptest.NewRequest(http.MethodGet, path, nil)); w.Code != http.StatusNotFound {
			t.Fatalf("%s with no report handler status=%d", path, w.Code)
		}
	}
}

// reportHistory is the smallest HistoryCollector the route tests need: it
// counts collections so a rejected request can be shown to have run none.
type reportHistory struct{ calls atomic.Int64 }

func (h *reportHistory) Collect(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
	h.calls.Add(1)
	return usagehistory.Report{}, nil
}

// TestProviderReportRoutesEnforceAccess mounts the real report handler the
// way main does (schema 1 on both v1 paths, Handler.V2() on the v2 path) and
// pins, per path, the access contract the README states: a non-loopback
// caller holding a valid local token gets 403, no-store, and no collection
// runs; an authenticated unsupported method gets exactly 405 with
// Allow: GET, HEAD and never reaches passthrough; a loopback GET gets the
// schema the path names, and all three paths share one collection.
func TestProviderReportRoutesEnforceAccess(t *testing.T) {
	history := &reportHistory{}
	report := providerreport.New(providerreport.Options{History: history})
	var relays atomic.Int64
	s, _ := newServer(t, func(o *server.Options) {
		o.Config.LocalToken = localSecret
		o.Routes.ProviderReport = report
		o.Routes.ProviderReportV2 = report.V2()
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			relays.Add(1)
			w.WriteHeader(http.StatusTeapot)
		})
	})
	request := func(method, path, remoteAddr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = remoteAddr
		r.Header.Set(proxyhdr.LocalToken, localSecret)
		return do(t, s, r)
	}
	errorType := func(t *testing.T, body []byte) string {
		t.Helper()
		var env struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("decoding %q: %v", body, err)
		}
		return env.Error.Type
	}
	for _, tc := range []struct {
		path   string
		schema float64
	}{
		{server.ProviderReportV1Path, 1},
		{server.ProviderReportPath, 1},
		{server.ProviderReportV2Path, 2},
	} {
		t.Run(tc.path, func(t *testing.T) {
			before := history.calls.Load()
			// A remote caller is refused even with the local token, for GET
			// and HEAD alike, over IPv4 and IPv6.
			for _, addr := range []string{"192.0.2.10:4000", "[2001:db8::1]:4000", "10.0.0.7:5000"} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					w := request(method, tc.path, addr)
					if w.Code != http.StatusForbidden || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("%s from %s: status=%d Cache-Control=%q body=%s", method, addr, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
					}
					if method == http.MethodHead {
						if w.Body.Len() != 0 {
							t.Fatalf("HEAD from %s carried a body: %s", addr, w.Body.String())
						}
					} else if got := errorType(t, w.Body.Bytes()); got != "permission_error" {
						t.Fatalf("GET from %s: error type=%q", addr, got)
					}
				}
			}
			// Unsupported methods are answered by the report handler itself
			// with exactly 405 and the Allow list, whoever sends them.
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
				for _, addr := range []string{"127.0.0.1:4000", "192.0.2.10:4000"} {
					w := request(method, tc.path, addr)
					if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("%s from %s: status=%d Allow=%q Cache-Control=%q body=%s", method, addr, w.Code, w.Header().Get("Allow"), w.Header().Get("Cache-Control"), w.Body.String())
					}
					if got := errorType(t, w.Body.Bytes()); got != "method_not_allowed" {
						t.Fatalf("%s from %s: error type=%q", method, addr, got)
					}
				}
			}
			if got := history.calls.Load(); got != before {
				t.Fatalf("rejected requests ran %d collection(s)", got-before)
			}
			// A loopback GET, IPv4 or IPv6, serves the schema the path names.
			for _, addr := range []string{"127.0.0.1:4000", "[::1]:4000"} {
				w := request(http.MethodGet, tc.path, addr)
				if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("GET from %s: status=%d Cache-Control=%q body=%s", addr, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
				}
				var doc map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
					t.Fatalf("decoding %s: %v", w.Body.String(), err)
				}
				if doc["schema_version"] != tc.schema {
					t.Fatalf("GET from %s: schema_version=%v want %v", addr, doc["schema_version"], tc.schema)
				}
			}
		})
	}
	if relays.Load() != 0 {
		t.Fatalf("%d report request(s) reached passthrough", relays.Load())
	}
	// Six accepted GETs across three paths and two schemas were served from
	// one collection: the paths share the handler's cache.
	if got := history.calls.Load(); got != 1 {
		t.Fatalf("collections=%d want 1", got)
	}
}

func TestBodyLimitDeclaredContentLength(t *testing.T) {
	reached := false
	s, _ := newServer(t, func(o *server.Options) {
		o.Config.Limits.MaxBodyBytes = 16
		o.Routes.Messages = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("x", 64)))
	w := do(t, s, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if reached {
		t.Error("an oversized request must not reach the handler")
	}
	if ev := decodeErrorEnvelope(t, w.Body.Bytes()); ev.Error.Type != "request_too_large" {
		t.Errorf("error type = %q", ev.Error.Type)
	}
}

func TestBodyLimitOnRead(t *testing.T) {
	var readErr error
	s, _ := newServer(t, func(o *server.Options) {
		o.Config.Limits.MaxBodyBytes = 16
		o.Routes.Messages = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, readErr = server.ReadBody(r)
			if readErr != nil {
				_ = apierr.Write(w, readErr)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	})
	// No declared Content-Length, so the cap trips inside ReadBody instead.
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("y", 64)))
	r.ContentLength = -1
	w := do(t, s, r)
	if readErr == nil {
		t.Fatal("ReadBody must fail past the limit")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ev := decodeErrorEnvelope(t, w.Body.Bytes()); ev.Error.Type != "request_too_large" {
		t.Errorf("error type = %q", ev.Error.Type)
	}
}

func TestReadBodyHappyPath(t *testing.T) {
	var got []byte
	s, _ := newServer(t, func(o *server.Options) {
		o.Routes.Messages = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, err := server.ReadBody(r)
			if err != nil {
				t.Errorf("ReadBody: %v", err)
			}
			got = b
			w.WriteHeader(http.StatusOK)
		})
	})
	do(t, s, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"a":1}`)))
	if string(got) != `{"a":1}` {
		t.Errorf("body = %q", got)
	}
	if b, err := server.ReadBody(nil); b != nil || err != nil {
		t.Errorf("ReadBody(nil) = %v, %v", b, err)
	}
}

func TestPanicRecovered(t *testing.T) {
	s, buf := newServer(t, func(o *server.Options) {
		o.Routes.Passthrough = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})
	})
	w := do(t, s, httptest.NewRequest(http.MethodGet, "/explode", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	ev := decodeErrorEnvelope(t, w.Body.Bytes())
	if ev.Error.Type != "api_error" {
		t.Errorf("error type = %q", ev.Error.Type)
	}
	if strings.Contains(ev.Error.Message, "boom") {
		t.Error("the panic value must not reach the client")
	}
	if !strings.Contains(buf.String(), "panic recovered") || !strings.Contains(buf.String(), "boom") {
		t.Errorf("the panic was not logged: %s", buf.String())
	}
}

func TestPanicAfterPartialWriteDoesNotDoubleWrite(t *testing.T) {
	s, _ := newServer(t, func(o *server.Options) {
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "partial")
			panic("late boom")
		})
	})
	w := do(t, s, httptest.NewRequest(http.MethodGet, "/late", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, the already-sent status must stand", w.Code)
	}
	if w.Body.String() != "partial" {
		t.Errorf("body = %q, an error envelope must not be appended", w.Body.String())
	}
}

func TestRecorderCapturesStatusBytesAndTTFB(t *testing.T) {
	clock := time.Unix(0, 0)
	var info *server.ResponseInfo
	s, _ := newServer(t, func(o *server.Options) {
		o.Now = func() time.Time { return clock }
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info = server.InfoFrom(r.Context())
			if info.Wrote() {
				t.Error("Wrote() = true before anything was written")
			}
			clock = clock.Add(250 * time.Millisecond)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "hello")
			_, _ = io.WriteString(w, "!")
		})
	})
	do(t, s, httptest.NewRequest(http.MethodGet, "/x", nil))
	if info == nil {
		t.Fatal("InfoFrom returned nil inside the handler")
	}
	if info.Status() != http.StatusAccepted {
		t.Errorf("status = %d", info.Status())
	}
	if info.Bytes() != 6 {
		t.Errorf("bytes = %d, want 6", info.Bytes())
	}
	if info.TTFB() != 250*time.Millisecond {
		t.Errorf("ttfb = %s", info.TTFB())
	}
	if !info.Wrote() {
		t.Error("Wrote = false")
	}
}

func TestResponseInfoNilAccessors(t *testing.T) {
	var i *server.ResponseInfo
	if i.Status() != 0 || i.Bytes() != 0 || i.TTFB() != 0 || i.Wrote() {
		t.Error("nil ResponseInfo accessors must be zero-valued")
	}
	if server.InfoFrom(context.Background()) != nil {
		t.Error("InfoFrom must be nil outside a request")
	}
}

func TestRecorderImplicit200(t *testing.T) {
	var info *server.ResponseInfo
	s, _ := newServer(t, func(o *server.Options) {
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info = server.InfoFrom(r.Context())
			_, _ = io.WriteString(w, "implicit")
		})
	})
	do(t, s, httptest.NewRequest(http.MethodGet, "/x", nil))
	if info.Status() != http.StatusOK {
		t.Errorf("status = %d, want an implicit 200", info.Status())
	}
}

func TestFlushReachesUnderlyingWriter(t *testing.T) {
	s, _ := newServer(t, func(o *server.Options) {
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for i := range 3 {
				fmt.Fprintf(w, "data: %d\n\n", i)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("Flush through the recorder failed: %v", err)
				}
			}
		})
	})
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "data: 2") {
		t.Errorf("stream body = %q", b)
	}
	if resp.Header.Get(proxyhdr.RequestID) == "" {
		t.Error("the response id is missing on a streamed response")
	}
}

func TestNotFoundEnvelopeWhenNoPassthrough(t *testing.T) {
	s, _ := newServer(t, nil)
	w := do(t, s, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	if ev := decodeErrorEnvelope(t, w.Body.Bytes()); ev.Error.Type != "not_found_error" {
		t.Errorf("error type = %q", ev.Error.Type)
	}
}

func TestRoutesMounted(t *testing.T) {
	hit := map[string]bool{}
	mark := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hit[name] = true
			w.WriteHeader(http.StatusOK)
		})
	}
	s, _ := newServer(t, func(o *server.Options) {
		o.Routes = server.Routes{
			Messages:    mark("messages"),
			CountTokens: mark("count"),
			Models:      mark("models"),
			Passthrough: mark("passthrough"),
		}
	})
	do(t, s, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	do(t, s, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader("{}")))
	do(t, s, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	do(t, s, httptest.NewRequest(http.MethodGet, "/v1/organizations/whatever", nil))
	for _, name := range []string{"messages", "count", "models", "passthrough"} {
		if !hit[name] {
			t.Errorf("route %s was not reached", name)
		}
	}
}

func TestUnmatchedMethodFallsToPassthrough(t *testing.T) {
	// Claude Code hits assorted endpoints under the base URL; none may 404.
	var path string
	s, _ := newServer(t, func(o *server.Options) {
		o.Routes.Messages = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			w.WriteHeader(http.StatusOK)
		})
	})
	do(t, s, httptest.NewRequest(http.MethodDelete, "/v1/messages", nil))
	if path != "/v1/messages" {
		t.Errorf("an unmatched method must fall through to the catch-all, got %q", path)
	}
}

type fakeTracker struct {
	mu       sync.Mutex
	holds    int
	releases int
	cur      int
	fired    bool
}

// Fired makes fakeTracker a server.DrainReporter.
func (f *fakeTracker) Fired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

func (f *fakeTracker) Hold() func() {
	f.mu.Lock()
	f.holds++
	f.cur++
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.releases++
		f.cur--
		f.mu.Unlock()
	}
}

func TestActivityHeldForRequestsButNotHealth(t *testing.T) {
	tr := &fakeTracker{}
	s, _ := newServer(t, func(o *server.Options) {
		o.Activity = tr
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})
	do(t, s, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))
	do(t, s, httptest.NewRequest(http.MethodGet, server.HealthPath, nil))

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.holds != 1 || tr.releases != 1 {
		t.Errorf("holds = %d, releases = %d, want 1 and 1 (health must be exempt)", tr.holds, tr.releases)
	}
	if tr.cur != 0 {
		t.Errorf("a hold leaked: cur = %d", tr.cur)
	}
}

// A connection accepted in the gap between the idle timer deciding it is idle
// and the server beginning to shut down would otherwise start work — possibly a
// long stream — on a draining process, and the drain grace would cut it. It is
// refused instead: retryable, and under launchd the retry restarts the daemon.
func TestRequestArrivingAfterTheIdleDeadlineIsRefused(t *testing.T) {
	tr := &fakeTracker{fired: true}
	served := false
	s, buf := newServer(t, func(o *server.Options) {
		o.Activity = tr
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served = true
			w.WriteHeader(http.StatusOK)
		})
	})

	w := do(t, s, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))
	if served {
		t.Error("the handler ran on a process that had already decided to exit")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Connection"); got != "close" {
		t.Errorf("Connection = %q, want close", got)
	}
	ev := decodeErrorEnvelope(t, w.Body.Bytes())
	if !strings.Contains(ev.Error.Message, "shutting down") {
		t.Errorf("message = %q, want it to explain the refusal", ev.Error.Message)
	}
	tr.mu.Lock()
	cur := tr.cur
	tr.mu.Unlock()
	if cur != 0 {
		t.Errorf("the refusal leaked a hold: cur = %d", cur)
	}

	// /healthz is exempt from the activity hold, so it must still answer: a
	// draining daemon still has to be able to say what it is doing.
	if code := do(t, s, httptest.NewRequest(http.MethodGet, server.HealthPath, nil)).Code; code != http.StatusOK {
		t.Errorf("/healthz status = %d while draining, want 200", code)
	}
	if !strings.Contains(buf.String(), "after the idle deadline fired") {
		t.Errorf("the refusal was not logged: %s", buf.String())
	}
}

func TestActivityReleasedOnPanic(t *testing.T) {
	tr := &fakeTracker{}
	s, _ := newServer(t, func(o *server.Options) {
		o.Activity = tr
		o.Routes.Passthrough = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})
	})
	do(t, s, httptest.NewRequest(http.MethodGet, "/boom", nil))
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.cur != 0 {
		t.Errorf("a hold leaked through a panic: cur = %d", tr.cur)
	}
}

func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s, _ := newServer(t, func(o *server.Options) {
		o.ShutdownGrace = 5 * time.Second
		o.Routes.Passthrough = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "drained")
		})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	type result struct {
		body string
		err  error
	}
	resc := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			resc <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		resc <- result{body: string(b), err: err}
	}()

	<-started
	cancel() // begin the graceful shutdown while the request is in flight
	close(release)

	res := <-resc
	if res.err != nil {
		t.Fatalf("the in-flight request failed during the drain: %v", res.err)
	}
	if res.body != "drained" {
		t.Errorf("body = %q", res.body)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want nil on a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the shutdown")
	}

	if err := s.Shutdown(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("a second Shutdown = %v", err)
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	s, _ := newServer(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // Serve must surface the accept failure, not hang
	if err := s.Serve(context.Background(), ln); err == nil {
		t.Fatal("Serve on a closed listener must return an error")
	}
}

func TestListenAndServeBindError(t *testing.T) {
	s, _ := newServer(t, func(o *server.Options) { o.Config.Listen = "127.0.0.1:1" })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.ListenAndServe(ctx); err == nil {
		t.Error("binding a privileged port must fail")
	}
}

func TestShutdownBeforeServeIsNoOp(t *testing.T) {
	s, _ := newServer(t, nil)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
}

func TestSignalContext(t *testing.T) {
	ctx, stop := server.SignalContext(context.Background())
	defer stop()
	select {
	case <-ctx.Done():
		t.Error("the signal context must not start cancelled")
	default:
	}
}

func TestUptimeAndAccessors(t *testing.T) {
	clock := time.Unix(100, 0)
	s, _ := newServer(t, func(o *server.Options) { o.Now = func() time.Time { return clock } })
	clock = clock.Add(3 * time.Second)
	if s.Uptime() != 3*time.Second {
		t.Errorf("Uptime = %s", s.Uptime())
	}
	if s.Version() != "1.2.3-test" {
		t.Errorf("Version = %q", s.Version())
	}
	if s.Config().Listen != config.DefaultListen {
		t.Errorf("Config().Listen = %q", s.Config().Listen)
	}
}

func TestIsHealth(t *testing.T) {
	if !server.IsHealth(httptest.NewRequest(http.MethodGet, server.HealthPath, nil)) {
		t.Error("IsHealth must match /healthz")
	}
	if server.IsHealth(httptest.NewRequest(http.MethodGet, "/v1/messages", nil)) {
		t.Error("IsHealth must not match other paths")
	}
	if server.IsHealth(nil) {
		t.Error("IsHealth(nil) must be false")
	}
}
