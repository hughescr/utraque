package obs

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"
)

// Summary is the one-line-per-request record. The server middleware owns the
// fields it can see for itself (method, path, sizes, timings, status) and every
// layer below contributes what only it knows: the router names the route and
// the models, the responses client names the upstream status, the stream
// translator names the stop reason and the output tokens.
//
// It exists so there is exactly ONE access-log line per request rather than a
// scattering of half-lines that a reader has to correlate by hand. Every setter
// is safe on a nil *Summary, so a call site never needs a nil check and a leg
// stays testable without a server around it.
//
// Nothing here may hold credential material. The fields are deliberately all
// enumerated scalars — a model name, a status, a count — rather than anything
// free-form off the request.
type Summary struct {
	mu sync.Mutex

	route         string
	clientModel   string
	upstreamModel string
	effort        string
	transport     string
	stopReason    string
	err           string

	reqBytes       int64
	upstreamStatus int
	outputTokens   int
	inputTokens    int
	cachedTokens   int

	stream      bool
	interrupted bool

	haveReqBytes     bool
	haveOutputTokens bool
	haveInputTokens  bool
}

// NewSummary builds an empty Summary.
func NewSummary() *Summary { return &Summary{} }

type summaryKey struct{}

// WithSummary attaches sum to ctx.
func WithSummary(ctx context.Context, sum *Summary) context.Context {
	if sum == nil {
		return ctx
	}
	return context.WithValue(ctx, summaryKey{}, sum)
}

// SummaryFrom returns the in-flight request's Summary, or nil. A nil result is
// usable: every method tolerates it.
func SummaryFrom(ctx context.Context) *Summary {
	if ctx == nil {
		return nil
	}
	sum, _ := ctx.Value(summaryKey{}).(*Summary)
	return sum
}

func (s *Summary) set(f func()) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

// SetRoute names the backend that served the request ("anthropic" or "codex").
func (s *Summary) SetRoute(route string) { s.set(func() { s.route = route }) }

// SetModels records the model the client asked for and the slug it routed to.
func (s *Summary) SetModels(client, upstream string) {
	s.set(func() { s.clientModel, s.upstreamModel = client, upstream })
}

// SetEffort records the resolved reasoning effort.
func (s *Summary) SetEffort(effort string) { s.set(func() { s.effort = effort }) }

// SetStream records whether the client asked for a streamed answer.
func (s *Summary) SetStream(stream bool) { s.set(func() { s.stream = stream }) }

// SetTransport records which HTTP transport served the upstream leg.
func (s *Summary) SetTransport(kind string) { s.set(func() { s.transport = kind }) }

// SetReqBytes records the request body size. A negative value (an undeclared
// Content-Length) is ignored, leaving the field absent rather than misleading.
func (s *Summary) SetReqBytes(n int64) {
	if n < 0 {
		return
	}
	s.set(func() { s.reqBytes, s.haveReqBytes = n, true })
}

// SetUpstreamStatus records the status the upstream answered with. It is
// distinct from the status utraque returns: a 429 upstream may become a 429
// downstream, but an upstream 200 whose body carried no events becomes a 502.
func (s *Summary) SetUpstreamStatus(code int) { s.set(func() { s.upstreamStatus = code }) }

// SetOutputTokens records the completion's output-token count.
func (s *Summary) SetOutputTokens(n int) {
	s.set(func() { s.outputTokens, s.haveOutputTokens = n, true })
}

// SetInputTokens records the prompt token count and how much of it the upstream
// served from its prompt cache.
//
// Both go on the request line because the RATIO is the diagnostic: a cached
// count that stays flat while a conversation's input grows is what a broken
// prompt-cache prefix looks like, and it is otherwise invisible until the
// quota runs out.
func (s *Summary) SetInputTokens(total, cached int) {
	s.set(func() { s.inputTokens, s.cachedTokens, s.haveInputTokens = total, cached, true })
}

// SetStopReason records the Anthropic stop_reason the answer terminated with.
func (s *Summary) SetStopReason(reason string) { s.set(func() { s.stopReason = reason }) }

// SetInterrupted marks a request the client abandoned mid-flight. It only ever
// latches on: an interrupt cannot be un-observed.
func (s *Summary) SetInterrupted(v bool) {
	if !v {
		return
	}
	s.set(func() { s.interrupted = true })
}

// SetErr records a failure. Only the message is kept, and it goes through the
// scrubbing handler like every other string. A nil error clears nothing.
func (s *Summary) SetErr(err error) {
	if err == nil {
		return
	}
	s.set(func() { s.err = err.Error() })
}

// Route reports the recorded route, for a caller deciding what else to log.
func (s *Summary) Route() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.route
}

// Attrs renders the contributed fields for the request line. Fields nothing
// ever set are omitted: an absent upstream_status is honest about a request
// that never reached an upstream, where a zero would not be.
func (s *Summary) Attrs() []slog.Attr {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	attrs := make([]slog.Attr, 0, 12)
	str := func(k, v string) {
		if v != "" {
			attrs = append(attrs, slog.String(k, v))
		}
	}
	str("route", s.route)
	str("client_model", s.clientModel)
	str("upstream_model", s.upstreamModel)
	str("effort", s.effort)
	attrs = append(attrs, slog.Bool("stream", s.stream))
	if s.haveReqBytes {
		attrs = append(attrs, slog.Int64("req_bytes", s.reqBytes))
	}
	if s.upstreamStatus != 0 {
		attrs = append(attrs, slog.Int("upstream_status", s.upstreamStatus))
	}
	if s.haveOutputTokens {
		attrs = append(attrs, slog.Int("output_tokens", s.outputTokens))
	}
	if s.haveInputTokens {
		attrs = append(attrs, slog.Int("input_tokens", s.inputTokens))
		attrs = append(attrs, slog.Int("cache_read_input_tokens", s.cachedTokens))
	}
	str("stop_reason", s.stopReason)
	attrs = append(attrs, slog.Bool("interrupted", s.interrupted))
	str("transport", s.transport)
	str("err", s.err)
	return attrs
}

// Fields renders the summary as a plain map, for the trace dump's manifest.
func (s *Summary) Fields() map[string]any {
	out := map[string]any{}
	for _, a := range s.Attrs() {
		out[a.Key] = a.Value.Any()
	}
	return out
}

// Millis renders a duration in milliseconds to microsecond precision, which is
// the unit every *_ms field in the request line uses.
func Millis(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000
}
