package leg

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/auth"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/translate/stream"
)

// The end-to-end behaviour of this leg is asserted in cmd/utraque, against
// main's real wiring and a fake ChatGPT upstream. What is tested here is the
// handful of contracts that are hard to provoke from outside: the exact status a
// stream failure maps to, and the lazy status line the whole failure-mode-1
// guarantee rests on.

// stubStreamer stands in for the responses client. It never opens a socket.
type stubStreamer struct {
	body string
	err  error

	calls int
}

func (s *stubStreamer) Stream(context.Context, auth.Credential, *cschema.ResponsesRequest) (io.ReadCloser, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(strings.NewReader(s.body)), nil
}

func (s *stubStreamer) StreamWithRefresh(ctx context.Context, _ auth.CredentialSource, req *cschema.ResponsesRequest) (io.ReadCloser, error) {
	return s.Stream(ctx, auth.Credential{}, req)
}

func TestNewRejectsNilClient(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no responses client returned no error")
	}
}

// TestMessagesWithoutCredentialsIs503 pins the "no `codex login` here" answer:
// a 503 naming both the client model and the resolved slug, never a 401 that
// would send Claude Code off to re-authenticate against the wrong provider.
func TestMessagesWithoutCredentialsIs503(t *testing.T) {
	l, err := New(Options{Client: &stubStreamer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rq := &router.Request{
		Raw:   []byte(`{"model":"sol","max_tokens":8,"messages":[]}`),
		Model: "sol",
		Dec:   router.Decision{Backend: router.BackendCodex, ClientModel: "sol", UpstreamModel: "gpt-5.6-sol"},
	}

	legErr := l.Messages(w, r, rq)
	if legErr == nil {
		t.Fatal("Messages returned no error for a missing credential")
	}
	ae := apierr.From(legErr)
	if got := ae.HTTPStatus(); got != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", got)
	}
	if !strings.Contains(ae.Message, "gpt-5.6-sol") {
		t.Errorf("message = %q, want it to name the resolved slug", ae.Message)
	}
	// The debug headers are set before anything is rendered, so they survive the
	// dispatcher writing the envelope on the leg's behalf.
	if got := w.Header().Get(HeaderRoute); got != "codex" {
		t.Errorf("%s = %q, want codex", HeaderRoute, got)
	}
	if got := w.Header().Get(HeaderModel); got != "gpt-5.6-sol" {
		t.Errorf("%s = %q, want gpt-5.6-sol", HeaderModel, got)
	}
}

func TestMessagesRejectsUnparseableBody(t *testing.T) {
	l, err := New(Options{Client: &stubStreamer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rq := &router.Request{Raw: []byte(`{"model":`), Model: "sol"}

	legErr := l.Messages(w, r, rq)
	if legErr == nil {
		t.Fatal("Messages accepted a truncated body")
	}
	if got := apierr.From(legErr).HTTPStatus(); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

// TestClassifyStreamFailure pins the status each stream failure reaches the
// client as. The 502s matter: an upstream that opened a stream and then failed
// is a gateway problem the caller may retry, not an internal 500.
func TestClassifyStreamFailure(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		kind   apierr.Type
	}{
		{"no data", stream.ErrNoData, http.StatusBadGateway, apierr.TypeAPI},
		{"idle timeout", stream.ErrIdleTimeout, http.StatusGatewayTimeout, apierr.TypeTimeout},
		{"truncated", stream.ErrTruncated, http.StatusBadGateway, apierr.TypeAPI},
		{"incomplete fold", stream.ErrIncomplete, http.StatusBadGateway, apierr.TypeAPI},
		{"unknown", errors.New("boom"), http.StatusBadGateway, apierr.TypeAPI},
		// An error the translator already classified passes through unchanged.
		{"already classified", apierr.RateLimit("slow down"), http.StatusTooManyRequests, apierr.TypeRateLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ae := classifyStreamFailure(tc.err)
			if got := ae.HTTPStatus(); got != tc.status {
				t.Errorf("status = %d, want %d", got, tc.status)
			}
			if ae.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", ae.Kind, tc.kind)
			}
		})
	}
}

// TestLazyWriterDefersTheStatusLine is the mechanism behind failure mode 1 on
// the streaming path: until a frame is actually produced, no status has been
// committed and a real error can still be rendered.
func TestLazyWriterDefersTheStatusLine(t *testing.T) {
	w := httptest.NewRecorder()
	lw := newLazyWriter(w, func(h http.Header) { h.Set("Content-Type", "text/event-stream") })

	// Flushing before the first write must not commit anything.
	lw.Flush()
	if lw.Started() {
		t.Fatal("Started = true after only a flush")
	}
	if w.Header().Get("Content-Type") != "" {
		t.Error("headers were prepared before any body byte existed")
	}

	if _, err := lw.Write([]byte("event: ping\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !lw.Started() {
		t.Fatal("Started = false after a write")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := w.Body.String(); got != "event: ping\n" {
		t.Errorf("body = %q", got)
	}
}

// TestCatalogModelFallsBackWhenUnavailable: a catalog that cannot answer must
// cost precision, never the request.
func TestCatalogModelFallsBackWhenUnavailable(t *testing.T) {
	l, err := New(Options{Client: &stubStreamer{}, Catalog: failingCatalog{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := l.catalogModel(context.Background(), auth.Credential{}, "gpt-5.6-sol", l.log)
	if got.Slug != "" {
		t.Errorf("model = %+v, want the zero Model when the catalog fails", got)
	}
}

type failingCatalog struct{}

func (failingCatalog) Models(context.Context, auth.Credential) ([]cschema.Model, error) {
	return nil, errors.New("catalog unavailable")
}

// TestCountTokensAnswersLocally asserts count_tokens never reaches the backend:
// the Codex API has no counting endpoint, and asking it would spend a real
// inference request.
func TestCountTokensAnswersLocally(t *testing.T) {
	st := &stubStreamer{}
	l, err := New(Options{Client: st})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	rq := &router.Request{
		Raw:   []byte(`{"model":"sol","messages":[{"role":"user","content":"count me"}]}`),
		Model: "sol",
		Dec:   router.Decision{Backend: router.BackendCodex, ClientModel: "sol", UpstreamModel: "gpt-5.6-sol"},
	}
	if err := l.CountTokens(w, r, rq); err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"input_tokens"`) {
		t.Errorf("body = %q, want an input_tokens count", w.Body.String())
	}
	if st.calls != 0 {
		t.Errorf("the responses client was called %d times, want 0", st.calls)
	}
}
