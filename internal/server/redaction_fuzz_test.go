package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/proxyhdr"
)

func FuzzSanitizeRequestID(f *testing.F) {
	for _, seed := range []string{
		"", "request-123", "!~", "sk-canary0123456789", "?refresh_token=TestOnlyCanary", "space here", "a\r\nb", "\x00", "\x7f", "\xff",
		strings.Repeat("x", 128), strings.Repeat("x", 129),
		// Each control byte alone, inside, and trailing: a loosened range check
		// admits exactly one of these, and only random search found that before.
		"a\nb", "a\rb", "a\tb", "a\x00b", "a\x7fb", "a\x1fb", "a b", "a\x80b", "\n", "abc\n", "\nabc",
		strings.Repeat("a", 127) + "\n", strings.Repeat("~", 128), strings.Repeat("!", 129), strings.Repeat("a", 4096),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, id string) {
		got := sanitizeRequestID(id)
		printable := func(r rune) bool { return r >= '!' && r <= '~' }
		want := "" // Refused IDs are replaced by the middleware.
		if len(id) <= 128 && strings.TrimFunc(id, printable) == "" && obs.Scrub(id) == id {
			want = id
		}
		if got != want {
			t.Fatalf("request ID admission mismatch (input %d bytes, accepted %t)", len(id), got != "")
		}
	})
}

// The fuzz oracle above calls obs.Scrub itself, so a Scrub regression would
// change oracle and implementation together. These fixed positive controls
// state the expected outcome literally. Every value is space-free printable
// ASCII (a space would be refused by the character check regardless of Scrub,
// proving nothing about it), except the one Bearer form the brief names.
func TestSanitizeRequestIDRefusesCredentialShapes(t *testing.T) {
	const secret = "abcdefgh12345678"
	for _, id := range []string{
		"Bearer " + secret,
		"sk-" + secret + "abcd",
		"req-sk-" + secret + "abcd",
		"eyJ" + secret + ".ijklmnop12345678.signature",
		"?access_token=" + secret,
		"x?refresh_token=" + secret,
		"api_key=" + secret,
		`{"refresh_token":"` + secret + `"}`,
	} {
		got := sanitizeRequestID(id)
		if got != "" || strings.Contains(got, secret) {
			t.Errorf("credential-shaped request ID %q was admitted as %q", id, got)
		}
	}
	// Control: an ordinary id is admitted unchanged, so the refusals above are
	// not the effect of a function that rejects everything.
	if got := sanitizeRequestID("request-123_ABC.def"); got != "request-123_ABC.def" {
		t.Errorf("ordinary request ID was altered to %q", got)
	}
}

// leakedWindow returns an n-byte window of secret that appears in haystack, or
// "" when none does. A partial disclosure (a prefix in an error message, a
// suffix in a diagnostic) fails a whole-value check but not this one.
func leakedWindow(haystack, secret string, n int) string {
	for i := 0; i+n <= len(secret); i++ {
		if strings.Contains(haystack, secret[i:i+n]) {
			return secret[i : i+n]
		}
	}
	return ""
}

func FuzzHealthAndErrorBodies(f *testing.F) {
	const requestMarker = "TESTONLY_REQUEST_CREDENTIAL_"
	// Every secret is built from private chunks at its start and end, so that
	// disclosing only a prefix or a suffix is detected. The values are random-
	// looking on purpose: no 8-byte window of them can occur in public text.
	const (
		localStart, localEnd = "Qz7Kx2MaVn", "W9pD4r6Tby"
		keyStart, keyEnd     = "Hm3Lc8Yu0e", "Rj5Fs1Ng7o"
		reqStart, reqEnd     = "Vb6Zt2Xq9d", "Ek4Aw8Ui3h"
	)
	cfg := config.Default()
	cfg.LocalToken = localStart + "m1dd1eL0calTokenQ8w" + localEnd
	cfg.DeepSeek.APIKey = keyStart + "m1dd1eProviderKeyR5t" + keyEnd
	s, err := New(Options{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		f.Fatal(err)
	}
	routes := [...]struct {
		method, path string
		status       int
	}{
		{http.MethodGet, HealthPath, http.StatusOK},
		{http.MethodPost, HealthPath, http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/messages", http.StatusUnauthorized},
		{http.MethodGet, "/not-a-route", http.StatusNotFound},
	}
	for i := range routes {
		f.Add(byte(i), "opaque\"<&")
	}
	f.Fuzz(func(t *testing.T, choice byte, suffix string) {
		if len(suffix) > 4096 {
			t.Skip()
		}
		// A fuzzer copying a fixture chunk into the request is not the server
		// disclosing it.
		for _, chunk := range []string{localStart, localEnd, keyStart, keyEnd, reqStart, reqEnd} {
			if strings.Contains(suffix, chunk) {
				t.Skip()
			}
		}
		route := routes[int(choice)%len(routes)]
		credential := reqStart + requestMarker + suffix + reqEnd
		r := httptest.NewRequest(route.method, route.path+"?access_token="+url.QueryEscape(credential), nil)
		r.Host = "localhost"
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("X-Api-Key", credential)
		r.Header.Set(proxyhdr.LocalToken, credential)
		if route.status == http.StatusNotFound {
			r.Header.Set(proxyhdr.LocalToken, cfg.LocalToken) // Reach the error behind the auth gate.
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != route.status || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("%s %s: status %d (want %d), valid JSON required", route.method, route.path, w.Code, route.status)
		}
		// The ASCII markers survive JSON escaping, even for non-UTF-8 suffixes.
		// Headers are part of the response too, so they are scanned as well.
		var headers strings.Builder
		for name, values := range w.Header() {
			headers.WriteString(name + ": " + strings.Join(values, "\n") + "\n")
		}
		for _, surface := range []struct{ what, text string }{{"body", body}, {"headers", headers.String()}} {
			for _, marker := range []string{requestMarker, cfg.LocalToken, cfg.DeepSeek.APIKey, localStart, localEnd, keyStart, keyEnd, reqStart, reqEnd} {
				if strings.Contains(surface.text, marker) {
					t.Fatalf("%s %s: response %s disclosed %s", route.method, route.path, surface.what, marker)
				}
			}
			// The configured secrets are fixed, so any 8-byte window of them is
			// private: the 401 must not hint at how the right token begins.
			for _, secret := range []string{cfg.LocalToken, cfg.DeepSeek.APIKey} {
				if leakedWindow(surface.text, secret, 8) != "" {
					t.Fatalf("%s %s: response %s disclosed a fragment of a configured secret", route.method, route.path, surface.what)
				}
			}
		}
	})
}
