package leg

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/responses"
	"github.com/hughescr/utraque/internal/proxyhdr"
	"github.com/hughescr/utraque/internal/router"
)

type headerTestCreds struct{ stubCreds }

func (headerTestCreds) Get(context.Context) (auth.Credential, error) {
	return auth.Credential{AccessToken: "test-token", AccountID: "test-account"}, nil
}

// A throttled response exercises the boundary where Codex actually relays
// selected upstream fields; a successful stream hands the translator only a body.
func TestHeaderCopyBoundaries(t *testing.T) {
	foreign := []string{proxyhdr.LocalToken, proxyhdr.RequestID, proxyhdr.Model, proxyhdr.Route, proxyhdr.TokenCountMethod, "X-Utraque-Future"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, k := range foreign {
			if v := r.Header.Values(k); len(v) != 0 {
				t.Errorf("copied request %s: %q", k, v)
			}
			w.Header().Set(k, "foreign")
		}
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"try later"}}`)
	}))
	defer upstream.Close()
	l, err := New(Options{Client: responses.New(responses.Options{BaseURL: upstream.URL}), Credentials: headerTestCreds{}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for _, k := range foreign {
		r.Header.Set(k, "foreign")
		r.Header[strings.ToLower(k)] = []string{"also foreign"}
	}
	w := httptest.NewRecorder()
	w.Header().Set(proxyhdr.RequestID, "proxy-minted")
	err = l.Messages(w, r, &router.Request{
		Raw: []byte(`{"model":"sol","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`),
		Dec: router.Decision{Backend: router.BackendCodex, ClientModel: "sol", UpstreamModel: "gpt-5.6-sol"},
	})
	if err != nil || w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "37" {
		t.Fatalf("expected upstream throttle with Retry-After, got HTTP %d, %v; headers %v", w.Code, err, w.Header())
	}
	for _, k := range []string{proxyhdr.LocalToken, proxyhdr.TokenCountMethod, "X-Utraque-Future"} {
		if v := w.Header().Values(k); len(v) != 0 {
			t.Errorf("copied response %s: %q", k, v)
		}
	}
	for k, want := range map[string]string{proxyhdr.RequestID: "proxy-minted", proxyhdr.Model: "gpt-5.6-sol", proxyhdr.Route: "codex"} {
		if values := w.Header().Values(k); !slices.Equal(values, []string{want}) {
			t.Errorf("generated %s = %q, want only %q", k, values, want)
		}
	}
}
