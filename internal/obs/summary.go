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

	route           string
	clientModel     string
	upstreamModel   string
	effortRequested string
	effortApplied   string
	transport       string
	stopReason      string
	err             string

	reqBytes        int64
	upstreamStatus  int
	outputTokens    int
	inputTokens     int
	cachedTokens    int
	createdTokens   int
	estimatedTokens int

	stream      bool
	interrupted bool

	haveReqBytes        bool
	haveOutputTokens    bool
	haveInputTokens     bool
	haveEstimatedTokens bool
	// The effort fields are presence-tracked rather than "non-empty" because
	// an empty effort is a real observation — a model name with no suffix, a
	// translation that resolved no level — and the request line has to tell
	// that apart from a request that never reached the point of knowing.
	haveEffortRequested bool
	haveEffortApplied   bool
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

// SetRoute names what served the request: one of the three inference legs
// ("anthropic", "codex", "deepseek") or "discovery" for a model-picker
// request that never reached a leg.
func (s *Summary) SetRoute(route string) { s.set(func() { s.route = route }) }

// SetModels records the model the client asked for and the slug it routed to.
func (s *Summary) SetModels(client, upstream string) {
	s.set(func() { s.clientModel, s.upstreamModel = client, upstream })
}

// SetEffortRequested records the reasoning effort the client asked for: the
// "-<level>" suffix on the model name, and nothing else — the router parses
// only the suffix, so an empty value means the name carried none, NOT that a
// default was resolved (the anthropic-beta, config and catalog fallbacks are
// applied later, by the Codex translator, and show up in effort_applied). The
// router sets it right after routing, in cmd/utraque/main.go's dispatch, so
// it is on the line for every routed request whichever leg served it, empty
// value included; only a request that never routed (a /healthz poll) lacks
// it. It is logged as effort_requested.
func (s *Summary) SetEffortRequested(effort string) {
	s.set(func() { s.effortRequested, s.haveEffortRequested = effort, true })
}

// SetEffortApplied records the reasoning effort actually sent upstream, after
// the precedence fallbacks and the catalog clamp. Only the Codex leg knows it
// (internal/codex/leg/leg.go sets it once translation resolves one), so it is
// absent on the Anthropic and DeepSeek legs' lines and on a Codex request
// that fails before translation; once set it is emitted even when empty,
// which is a translation that sent no reasoning effort at all. It is logged
// as effort_applied.
func (s *Summary) SetEffortApplied(effort string) {
	s.set(func() { s.effortApplied, s.haveEffortApplied = effort, true })
}

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

// SetInputTokens records the prompt token counts under Anthropic semantics:
// uncached is the part of the prompt billed at full price, cached is the part
// the upstream served from its prompt cache, and created is the part it
// wrote into the cache (cache_creation_input_tokens). The whole prompt is the
// sum of the three (see stream.promptTokens). Only the Codex and DeepSeek
// legs call this — the Codex leg subtracts the cached count out of the
// inclusive figure Responses gives it (see stream.mapUsage; Responses reports
// no cache-write count, so its created is the 0 the client was told);
// DeepSeek reads its own Anthropic-shaped usage block directly. The Anthropic
// passthrough leg never calls it: it relays the upstream response unparsed
// and sets none of the token fields.
//
// All three go on the request line because the RATIO is the diagnostic: the
// hit rate is cached / (uncached + cached + created), and a cached count that
// stays flat while a conversation's uncached count grows is what a broken
// prompt-cache prefix looks like. It is otherwise invisible until the quota
// runs out.
func (s *Summary) SetInputTokens(uncached, cached, created int) {
	s.set(func() {
		s.inputTokens, s.cachedTokens, s.createdTokens, s.haveInputTokens = uncached, cached, created, true
	})
}

// SetEstimatedInputTokens records the prompt token count utraque computed
// locally and seeded into message_start before the upstream reported the real
// usage. It is logged beside the real counts because the seed is designed to
// be a LOWER BOUND of the billed prompt (see internal/tokens): the invariant
// to monitor is estimated_input_tokens <= input_tokens +
// cache_read_input_tokens, and a line that breaks it is a line on which
// ccusage's per-message dedup could have kept the seed instead of the truth.
func (s *Summary) SetEstimatedInputTokens(n int) {
	if n < 0 {
		return
	}
	s.set(func() { s.estimatedTokens, s.haveEstimatedTokens = n, true })
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

	attrs := make([]slog.Attr, 0, 16)
	str := func(k, v string) {
		if v != "" {
			attrs = append(attrs, slog.String(k, v))
		}
	}
	str("route", s.route)
	str("client_model", s.clientModel)
	str("upstream_model", s.upstreamModel)
	if s.haveEffortRequested {
		attrs = append(attrs, slog.String("effort_requested", s.effortRequested))
	}
	if s.haveEffortApplied {
		attrs = append(attrs, slog.String("effort_applied", s.effortApplied))
	}
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
		attrs = append(attrs, slog.Int("cache_creation_input_tokens", s.createdTokens))
	}
	if s.haveEstimatedTokens {
		attrs = append(attrs, slog.Int("estimated_input_tokens", s.estimatedTokens))
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
