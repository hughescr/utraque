package leg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/effort"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/proxyhdr"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/tokens"
	"github.com/hughescr/utraque/internal/translate/request"
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

// TestNewValidatesStreamPolicies pins that New, not the Translator, is where an
// unknown emit_reasoning / on_truncate mode is rejected: the Translator takes
// its modes as validated, and an unknown value there would silently behave as
// the default.
func TestNewValidatesStreamPolicies(t *testing.T) {
	if _, err := New(Options{Client: &stubStreamer{}, EmitReasoning: "loud"}); err == nil {
		t.Error("New accepted an unknown EmitReasoning mode")
	}
	if _, err := New(Options{Client: &stubStreamer{}, TruncateMode: "ignore"}); err == nil {
		t.Error("New accepted an unknown TruncateMode")
	}
	for _, m := range []stream.ReasoningMode{"", stream.ReasoningThinking, stream.ReasoningDrop} {
		if _, err := New(Options{Client: &stubStreamer{}, EmitReasoning: m}); err != nil {
			t.Errorf("New(EmitReasoning=%q): %v", m, err)
		}
	}
	for _, m := range []stream.TruncateMode{"", stream.TruncateError, stream.TruncateFinish} {
		if _, err := New(Options{Client: &stubStreamer{}, TruncateMode: m}); err != nil {
			t.Errorf("New(TruncateMode=%q): %v", m, err)
		}
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
		Raw: []byte(`{"model":"sol","max_tokens":8,"messages":[]}`),
		Dec: router.Decision{Backend: router.BackendCodex, ClientModel: "sol", UpstreamModel: "gpt-5.6-sol"},
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
	if got := w.Header().Get(proxyhdr.Route); got != "codex" {
		t.Errorf("%s = %q, want codex", proxyhdr.Route, got)
	}
	if got := w.Header().Get(proxyhdr.Model); got != "gpt-5.6-sol" {
		t.Errorf("%s = %q, want gpt-5.6-sol", proxyhdr.Model, got)
	}
}

func TestMessagesRejectsUnparseableBody(t *testing.T) {
	l, err := New(Options{Client: &stubStreamer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rq := &router.Request{Raw: []byte(`{"model":`), Dec: router.Decision{ClientModel: "sol"}}

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
		name    string
		err     error
		status  int
		errType apierr.ErrorType
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
			if ae.Type != tc.errType {
				t.Errorf("errType = %q, want %q", ae.Type, tc.errType)
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

func (failingCatalog) Models(context.Context, auth.Credential) ([]cschema.CatalogModel, error) {
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
		Raw: []byte(`{"model":"sol","messages":[{"role":"user","content":"count me"}]}`),
		Dec: router.Decision{Backend: router.BackendCodex, ClientModel: "sol", UpstreamModel: "gpt-5.6-sol"},
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

// TestUpstreamModelEchoesTheResolvedSlug pins what the caller is told it talked
// to. The client writes this value straight into its own session log, so a
// request routed as an alias must come back as the slug that actually served
// it: that log is the only record usage reporting downstream can price. The
// fallback covers a zero Decision, which must never echo an empty model.
func TestUpstreamModelEchoesTheResolvedSlug(t *testing.T) {
	cases := []struct {
		name string
		rq   *router.Request
		want string
	}{
		{
			name: "resolved slug wins over the alias the caller wrote",
			rq: &router.Request{
				Dec: router.Decision{
					Backend:       router.BackendCodex,
					ClientModel:   "sol-high",
					UpstreamModel: "gpt-5.6-sol",
				},
			},
			want: "gpt-5.6-sol",
		},
		{
			name: "a decision with no slug falls back to the caller's string",
			rq:   &router.Request{Dec: router.Decision{ClientModel: "sol-high"}},
			want: "sol-high",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamModel(tc.rq); got != tc.want {
				t.Errorf("upstreamModel = %q, want %q", got, tc.want)
			}
		})
	}
}

// stubCreds hands out a fixed credential; it never reads a file.
type stubCreds struct{}

func (stubCreds) Get(context.Context) (auth.Credential, error) { return auth.Credential{}, nil }
func (stubCreds) Invalidate(auth.Credential)                   {}

// signalStreamer is a stubStreamer that announces the moment the upstream
// request is sent, so a test can order it against other work.
type signalStreamer struct {
	stubStreamer
	sent chan struct{}
	once sync.Once
}

func (s *signalStreamer) StreamWithRefresh(ctx context.Context, src auth.CredentialSource, req *cschema.ResponsesRequest) (io.ReadCloser, error) {
	s.once.Do(func() { close(s.sent) })
	return s.stubStreamer.StreamWithRefresh(ctx, src, req)
}

// gatedEstimator blocks every count until released, standing in for a
// tokenizer that is slower than the upstream.
type gatedEstimator struct {
	release chan struct{}
	n       int
	calls   atomic.Int32
}

func (g *gatedEstimator) EstimateString(string) int { return g.n }
func (g *gatedEstimator) Name() string              { return "gated" }
func (g *gatedEstimator) EstimateRequest(*cschema.ResponsesRequest) int {
	g.calls.Add(1)
	<-g.release
	return g.n
}

func textOnlyFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/streams/text_only.codex.sse")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

// TestSeedDoesNotDelayTheUpstreamRequest is the performance contract of the
// message_start seed: the upstream request goes out BEFORE the prompt count
// finishes, the count is asked for exactly once, message_start carries it, the
// terminal usage carries the upstream's own number, and the request line
// records the seed beside it.
func TestSeedDoesNotDelayTheUpstreamRequest(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "aggregate"}[streaming], func(t *testing.T) {
			st := &signalStreamer{stubStreamer: stubStreamer{body: textOnlyFixture(t)}, sent: make(chan struct{})}
			est := &gatedEstimator{release: make(chan struct{}), n: 4242}
			l, err := New(Options{Client: st, Credentials: stubCreds{}, Estimator: est, Heartbeat: -1, UpstreamIdleTimeout: -1})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			sum := obs.NewSummary()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			r = r.WithContext(obs.WithSummary(r.Context(), sum))
			rq := &router.Request{
				Raw:    []byte(`{"model":"sol","max_tokens":8,"stream":` + strconv.FormatBool(streaming) + `,"messages":[{"role":"user","content":"hello"}]}`),
				Stream: streaming,
				Dec:    router.Decision{Backend: router.BackendCodex, ClientModel: "sol", UpstreamModel: "gpt-5.6-sol"},
			}

			done := make(chan error, 1)
			go func() { done <- l.Messages(w, r, rq) }()

			// The upstream request must be sent while the count is still
			// blocked.
			select {
			case <-st.sent:
			case err := <-done:
				t.Fatalf("Messages returned (%v) before the upstream request was sent", err)
			case <-time.After(5 * time.Second):
				t.Fatal("the upstream request was not sent while the estimator was blocked")
			}
			// And the response cannot complete until the count arrives,
			// because message_start needs it.
			select {
			case err := <-done:
				t.Fatalf("Messages returned (%v) before the seed was available", err)
			case <-time.After(50 * time.Millisecond):
			}

			close(est.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Messages: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Messages did not return after the seed was released")
			}

			if got := est.calls.Load(); got != 1 {
				t.Errorf("the estimator was asked %d times, want 1", got)
			}
			body := w.Body.String()
			if streaming {
				if !strings.Contains(body, `"usage":{"input_tokens":4242,"output_tokens":0}`) {
					t.Errorf("message_start does not carry the seed:\n%s", body)
				}
				if !strings.Contains(body, `"usage":{"input_tokens":7,"output_tokens":3}`) {
					t.Errorf("message_delta does not carry the upstream's usage:\n%s", body)
				}
			} else if !strings.Contains(body, `"usage":{"input_tokens":7,"output_tokens":3}`) {
				t.Errorf("the folded message does not carry the upstream's usage:\n%s", body)
			}
			fields := sum.Fields()
			if got := fields["estimated_input_tokens"]; got != int64(4242) {
				t.Errorf("estimated_input_tokens = %v (%T), want 4242", got, got)
			}
			if got := fields["input_tokens"]; got != int64(7) {
				t.Errorf("input_tokens = %v, want 7", got)
			}
		})
	}
}

// TestSeedWaitEndsWithTheRequest: a request whose context ends while the
// count is still running does not wait for it. The upstream has answered,
// message_start needs the seed, the client is gone — the translator must be
// free to notice, not parked on the tokenizer. The count goroutine finishes
// on its own afterwards.
func TestSeedWaitEndsWithTheRequest(t *testing.T) {
	st := &signalStreamer{stubStreamer: stubStreamer{body: textOnlyFixture(t)}, sent: make(chan struct{})}
	est := &gatedEstimator{release: make(chan struct{}), n: 4242}
	l, err := New(Options{Client: st, Credentials: stubCreds{}, Estimator: est, Heartbeat: -1, UpstreamIdleTimeout: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	rq := &router.Request{
		Raw:    []byte(`{"model":"sol","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}`),
		Stream: true,
		Dec:    router.Decision{Backend: router.BackendCodex, ClientModel: "sol", UpstreamModel: "gpt-5.6-sol"},
	}

	done := make(chan error, 1)
	go func() { done <- l.Messages(w, r, rq) }()
	select {
	case <-st.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was not sent")
	}
	// Let the translator reach message_start and block on the gated count
	// (the body is already in hand), then end the request instead.
	select {
	case err := <-done:
		t.Fatalf("Messages returned (%v) with the count still gated", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Messages waited on the count after its context ended")
	}
	if got := est.calls.Load(); got != 1 {
		t.Errorf("the estimator was started %d times, want 1", got)
	}
	close(est.release)
}

// TestCountTokensCountsTheTranslatedRequest: count_tokens is answered from the
// request as it would go upstream, not from the Anthropic body. Thinking text
// is the visible difference — the translator drops it (it is replaced by the
// encrypted replay item, or nothing), so it must not be counted — and the
// number must be the exact tokenizer's, which the Name on the leg confirms.
func TestCountTokensCountsTheTranslatedRequest(t *testing.T) {
	l, err := New(Options{Client: &stubStreamer{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := l.est.Name(); got != tokens.O200kName {
		t.Fatalf("default estimator = %q, want %q", got, tokens.O200kName)
	}
	count := func(t *testing.T, raw string) int {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
		rq := &router.Request{Raw: []byte(raw),
			Dec: router.Decision{Backend: router.BackendCodex, ClientModel: "sol", UpstreamModel: "gpt-5.6-sol"}}
		if err := l.CountTokens(w, r, rq); err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
		var out struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("body %q: %v", w.Body.String(), err)
		}
		return out.InputTokens
	}

	plain := count(t, `{"model":"sol","system":"Be terse.","messages":[{"role":"user","content":"hello world"}]}`)
	if want := l.est.EstimateString("Be terse.") + l.est.EstimateString("hello world"); plain != want {
		t.Errorf("count = %d, want the per-field sum %d", plain, want)
	}
	withThinking := count(t, `{"model":"sol","system":"Be terse.","messages":[`+
		`{"role":"user","content":"hello world"},`+
		`{"role":"assistant","content":[{"type":"thinking","thinking":"`+strings.Repeat("deep thoughts about the port number ", 50)+`","signature":"nope"}]}]}`)
	if withThinking != plain {
		t.Errorf("unreplayable thinking text was counted: %d vs %d", withThinking, plain)
	}
}

// debugRecords runs f against a JSON logger at DEBUG and returns every record
// it wrote, keyed by msg, for asserting the log-schema contract of this leg's
// per-request records.
func debugRecords(t *testing.T, f func(*slog.Logger)) map[string]map[string]any {
	t.Helper()
	var buf bytes.Buffer
	f(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		out[rec["msg"].(string)] = rec
	}
	return out
}

// TestLogTranslationKeys pins the DEBUG translation record: both efforts
// under their own names, the parallel_tool_calls provenance beside the bool it
// folds to, and a key per kind of drop.
func TestLogTranslationKeys(t *testing.T) {
	rq := &router.Request{Dec: router.Decision{UpstreamModel: "gpt-5.6-sol"}}
	meta := request.Metadata{
		DroppedParams:           []string{request.DroppedTemperature, request.DroppedTopP},
		DroppedSystemBlocks:     []string{request.DroppedBillingHeader},
		ParallelDisableReason:   request.ParallelDisableBoth,
		ParallelDisableTriggers: []string{"Bash", "Edit"},
		Effort: request.EffortResult{
			Requested: effort.Ultra, Applied: effort.High, Source: effort.SourceSuffix, Clamped: true,
		},
	}
	recs := debugRecords(t, func(log *slog.Logger) {
		logTranslation(context.Background(), log, rq, meta)
	})
	rec := recs["translated a Messages request for the codex backend"]
	if rec == nil {
		t.Fatalf("no translation record: %v", recs)
	}
	for key, want := range map[string]any{
		"upstream_model":               "gpt-5.6-sol",
		"effort_requested":             "ultra",
		"effort_applied":               "high",
		"effort_source":                "suffix",
		"effort_clamped":               true,
		"parallel_tool_calls_disabled": true, // deprecated: remove in the next release
		"parallel_tool_calls_reason":   "both",
	} {
		if rec[key] != want {
			t.Errorf("%s = %v, want %v", key, rec[key], want)
		}
	}
	for key, want := range map[string][]any{
		"parallel_tool_calls_triggers": {"Bash", "Edit"},
		"dropped_params":               {"temperature", "top_p"},
		"dropped_system_blocks":        {"system:billing-header"},
	} {
		got, _ := rec[key].([]any)
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", key, rec[key], want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s = %v, want %v", key, rec[key], want)
			}
		}
	}
	for _, old := range []string{"effort", "dropped"} {
		if _, present := rec[old]; present {
			t.Errorf("translation record still carries the old key %q: %v", old, rec)
		}
	}

	// Nothing dropped and parallelism untouched: the per-kind keys are absent
	// rather than empty, and the reason is the empty string.
	recs = debugRecords(t, func(log *slog.Logger) {
		logTranslation(context.Background(), log, rq, request.Metadata{})
	})
	rec = recs["translated a Messages request for the codex backend"]
	for _, key := range []string{"dropped_params", "dropped_system_blocks", "parallel_tool_calls_triggers"} {
		if _, present := rec[key]; present {
			t.Errorf("%s present with nothing to report: %v", key, rec)
		}
	}
	if rec["parallel_tool_calls_reason"] != "" || rec["parallel_tool_calls_disabled"] != false {
		t.Errorf("parallel keys = %v / %v, want \"\" / false", rec["parallel_tool_calls_reason"], rec["parallel_tool_calls_disabled"])
	}
}

// TestLogResultUnknownEventsMatchHealthz pins that the stream record spells
// the unknown-event counts the way /healthz does: unknown_events is the total
// and unknown_event_types the per-type map.
func TestLogResultUnknownEventsMatchHealthz(t *testing.T) {
	var seen map[string]int
	l, err := New(Options{Client: &stubStreamer{}, OnUnknownEvents: func(m map[string]int) { seen = m }})
	if err != nil {
		t.Fatal(err)
	}
	rq := &router.Request{Dec: router.Decision{UpstreamModel: "gpt-5.6-sol"}}
	res := stream.Result{
		Started: true, Terminus: stream.TerminusClean, StopReason: "end_turn",
		InputTokens: 7, CachedInputTokens: 3, CacheCreationInputTokens: 0, OutputTokens: 2,
		UnknownEvents: map[string]int{"response.novel": 2, "response.other": 1},
	}
	sum := obs.NewSummary()
	ctx := obs.WithSummary(context.Background(), sum)
	sd := &seed{done: make(chan struct{}), n: 5}
	close(sd.done)
	eff := request.EffortResult{Requested: effort.Ultra, Applied: effort.High, Source: effort.SourceSuffix, Clamped: true}
	recs := debugRecords(t, func(log *slog.Logger) {
		l.logResult(ctx, log, rq, res, sd, eff)
	})
	rec := recs["codex stream carried unrecognised event types"]
	if rec == nil {
		t.Fatalf("no unknown-events record: %v", recs)
	}
	if rec["unknown_events"] != float64(3) {
		t.Errorf("unknown_events = %v, want the total 3", rec["unknown_events"])
	}
	types, _ := rec["unknown_event_types"].(map[string]any)
	if types["response.novel"] != float64(2) || types["response.other"] != float64(1) {
		t.Errorf("unknown_event_types = %v, want the per-type map", rec["unknown_event_types"])
	}
	if seen["response.novel"] != 2 {
		t.Errorf("OnUnknownEvents saw %v", seen)
	}
	// The INFO form is the DEBUG record plus the counts, so it carries the
	// effort pair too.
	if rec["effort_requested"] != "ultra" || rec["effort_applied"] != "high" {
		t.Errorf("effort pair = %v / %v, want ultra / high", rec["effort_requested"], rec["effort_applied"])
	}
	// The three prompt components reach the request line together.
	f := sum.Fields()
	if f["input_tokens"] != int64(7) || f["cache_read_input_tokens"] != int64(3) || f["cache_creation_input_tokens"] != int64(0) {
		t.Errorf("summary token fields = %v", f)
	}
}

// TestLogResultStreamRecordCarriesEffort pins the DEBUG "codex stream
// translated" record: the effort the translator chose and the one it sent,
// under the same keys the request line uses, beside the model — and never
// under the old single `effort` key.
func TestLogResultStreamRecordCarriesEffort(t *testing.T) {
	l, err := New(Options{Client: &stubStreamer{}})
	if err != nil {
		t.Fatal(err)
	}
	rq := &router.Request{Dec: router.Decision{UpstreamModel: "gpt-5.6-sol"}}
	res := stream.Result{Started: true, Terminus: stream.TerminusClean, StopReason: "end_turn"}
	sd := &seed{done: make(chan struct{}), n: 5}
	close(sd.done)

	eff := request.EffortResult{Requested: effort.Ultra, Applied: effort.High, Source: effort.SourceSuffix, Clamped: true}
	recs := debugRecords(t, func(log *slog.Logger) {
		l.logResult(context.Background(), log, rq, res, sd, eff)
	})
	rec := recs["codex stream translated"]
	if rec == nil {
		t.Fatalf("no stream record: %v", recs)
	}
	for key, want := range map[string]any{
		"upstream_model":   "gpt-5.6-sol",
		"effort_requested": "ultra",
		"effort_applied":   "high",
		"started":          true,
		"terminated":       true,
		"errored":          false,
	} {
		if rec[key] != want {
			t.Errorf("%s = %v, want %v", key, rec[key], want)
		}
	}
	for _, absent := range []string{"effort", "unknown_events", "unknown_event_types"} {
		if _, present := rec[absent]; present {
			t.Errorf("stream record carries %q, which it must not: %v", absent, rec)
		}
	}

	// A translation that resolved no effort at all (no suffix, no beta, no
	// config, no catalog default) is a known outcome, not an unknown one:
	// both keys are present and empty rather than missing.
	recs = debugRecords(t, func(log *slog.Logger) {
		l.logResult(context.Background(), log, rq, res, sd, request.EffortResult{})
	})
	rec = recs["codex stream translated"]
	for _, key := range []string{"effort_requested", "effort_applied"} {
		got, present := rec[key]
		if !present || got != "" {
			t.Errorf("%s = %v (present %v), want an empty string", key, got, present)
		}
	}
}
