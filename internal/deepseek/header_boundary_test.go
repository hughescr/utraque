package deepseek

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/proxyhdr"
)

func TestHeaderCopyBoundaries(t *testing.T) {
	foreign := []string{proxyhdr.LocalToken, proxyhdr.RequestID, proxyhdr.Model, proxyhdr.Route, proxyhdr.TokenCountMethod, "X-Utraque-Future"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range append(slices.Clone(foreign), "Connection", "Anthropic-Beta", "X-Request-Id") {
			if values := r.Header.Values(name); len(values) != 0 {
				t.Errorf("copied request %s: %q", name, values)
			}
		}
		if r.Header.Get("X-Api-Key") != testAPIKey || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("Connection nominations changed generated request headers")
		}
		if values := r.Header.Values("Accept"); !slices.Equal(values, []string{"application/json", "text/plain"}) {
			t.Errorf("surviving request Accept = %q", values)
		}
		for _, name := range foreign {
			w.Header().Set(name, "foreign")
		}
		w.Header().Add("Connection", "retry-AFTER, x-RateLimit-Foo, Content-Length")
		w.Header().Add("Connection", " \tReQuEsT-Id, X-Content-Type-Options, X-Utraque-Model")
		for _, k := range []string{"Retry-After", "X-Ratelimit-Foo", "Request-Id"} {
			w.Header().Set(k, "drop")
		}
		w.Header().Add("Anthropic-Ratelimit-Foo", "one")
		w.Header().Add("Anthropic-Ratelimit-Foo", "two")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	h := http.Header{
		"Connection":     {"anthropic-BETA, x-api-key, content-type"},
		"cOnNeCtIoN":     {"\tx-REQUEST-ID , Accept-Encoding"},
		"Anthropic-Beta": {"drop"},
		"X-Request-Id":   {"drop"},
		"Accept":         {"application/json", "text/plain"},
	}
	for _, name := range foreign {
		h.Set(name, "foreign")
		h[strings.ToLower(name)] = []string{"also foreign"}
	}
	const body = `{"model":"deepseek-flash","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	rec, err := callLeg(t, testLeg(t, upstream.URL), "deepseek-flash", body, false, false, h)
	if err != nil {
		t.Fatal(err)
	}
	dst := rec.Result().Header
	for _, name := range []string{"Connection", "Retry-After", "X-Ratelimit-Foo", "Request-Id", proxyhdr.LocalToken, proxyhdr.RequestID, proxyhdr.TokenCountMethod, "X-Utraque-Future"} {
		if values := dst.Values(name); len(values) != 0 {
			t.Errorf("copied response %s: %q", name, values)
		}
	}
	if !slices.Equal(dst.Values(proxyhdr.Route), []string{"deepseek"}) || !slices.Equal(dst.Values(proxyhdr.Model), []string{"deepseek-flash"}) || dst.Get("X-Content-Type-Options") != "nosniff" || dst.Get("Content-Length") == "" {
		t.Errorf("missing or forged generated response headers: %v", dst)
	}
	if values := dst.Values("Anthropic-Ratelimit-Foo"); !slices.Equal(values, []string{"one", "two"}) {
		t.Errorf("surviving response rate limit = %q", values)
	}
}

// Raw map keys bypass canonicalisation, and a middleware can supply them in any
// case. None of the three copiers may let a proxy-reserved name through.
func TestHeaderCopiersDropNonCanonicalReservedKeys(t *testing.T) {
	leg := testLeg(t, "https://fixture.invalid/anthropic")
	for _, tc := range []struct {
		name string
		copy func(dst, src http.Header)
		// The request copier looks allowed fields up by canonical name, so a
		// raw lowercase key is simply not found; the others canonicalise.
		relaysRawAllowed bool
	}{
		{"request", copyRequestHeaders, false},
		{"response", copyResponseHeaders, true},
		{"error", leg.copyErrorHeaders, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := http.Header{
				"x-utraque-foo":   {"foreign"},
				"X-UTRAQUE-ROUTE": {"foreign"},
				"x-uTrAqUe-Token": {"foreign"},
				// Allowed fields in raw spellings must still survive, so the
				// test cannot pass by dropping everything.
				"x-request-id": {"req-1"},
			}
			dst := http.Header{}
			tc.copy(dst, src)
			for name := range dst {
				if strings.HasPrefix(strings.ToLower(name), "x-utraque-") {
					t.Errorf("forwarded reserved header %q = %q", name, dst[name])
				}
			}
			if got := dst.Values("X-Request-Id"); tc.relaysRawAllowed && !slices.Equal(got, []string{"req-1"}) {
				t.Errorf("X-Request-Id = %q, want it relayed under its canonical name", got)
			}
		})
	}
}
