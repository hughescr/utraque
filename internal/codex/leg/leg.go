// Package leg is the Codex inference leg: the end-to-end path from an
// Anthropic Messages request to a GPT answer billed against the caller's Codex
// subscription.
//
// One request walks five hops, each owned by a package that knows nothing about
// the others:
//
//	Messages JSON -> translate/request.Translate -> codex/auth credential ->
//	codex/responses.Stream -> translate/stream.Translator -> Sink
//
// The Sink is the only fork in the road: SSEWriter when the client asked for
// stream:true, Aggregator when it did not. Both consume identical calls from the
// one Translator, so the two request modes cannot drift apart.
//
// # The three failure modes
//
// The plan's phase-5 contract is kept intact end to end, and this package is
// where the distinction becomes visible to the client:
//
//  1. Before any byte reaches the client — a non-200 from the backend, a bot
//     gate, a network failure, or a 200 whose body carried no events — the leg
//     answers with a real HTTP status and an Anthropic error envelope. A 429
//     forwards the upstream's Retry-After (and the quota headers) verbatim, so
//     the client's own backoff sees what the backend actually said.
//  2. Mid-stream — the upstream failed after output began — the Translator
//     closes the open block, emits an SSE error event and STOPS. No clean
//     terminus is faked over a broken stream. For a non-streaming client there
//     is no way to show a truncation, so the Aggregator turns the same condition
//     into a real HTTP error instead of a short answer.
//  3. Client interrupt — the caller hung up — cancels the upstream request,
//     writes nothing further, and joins the reader goroutine, so nothing leaks
//     and no terminus is invented.
//
// The single retry the design sanctions (upstream 401 -> auth.Invalidate ->
// refresh -> retry exactly once) is delegated to
// responses.Client.StreamWithRefresh, which is the only layer that can tell a
// 401 from any other failure.
package leg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/responses"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/tokens"
	"github.com/hughescr/utraque/internal/translate/request"
	"github.com/hughescr/utraque/internal/translate/stream"
)

// Debug headers set on every response this leg produces. They name the backend
// that served the request and the upstream slug it was translated to, which is
// the first question anyone debugging a mixed-model session asks. Neither
// carries any credential material.
const (
	HeaderRoute = "X-Utraque-Route"
	HeaderModel = "X-Utraque-Model"
)

// DefaultCatalogTimeout bounds the per-request catalog lookup. The catalog is
// consulted only to clamp reasoning effort and to source the default summary;
// missing it costs precision, not correctness, so it is never allowed to hold up
// the actual inference request for long.
const DefaultCatalogTimeout = 2 * time.Second

// Catalog is the slice of the Codex model catalog this leg needs: the current
// model list, from which the entry for the routed slug supplies the supported
// reasoning levels used to clamp effort. It matches catalog.Catalog's Models
// method, so *catalog.Client satisfies it directly.
type Catalog interface {
	Models(ctx context.Context, cred auth.Credential) ([]cschema.CatalogModel, error)
}

// Options configures a Leg. Only Client is required.
type Options struct {
	// Client opens the upstream stream. Required.
	Client responses.Streamer

	// Credentials supplies the Codex OAuth credential. Nil is a supported
	// state — it means "no `codex login` on this machine" — and makes every
	// codex-routed request answer with a clear 503 rather than failing
	// obscurely at construction time.
	Credentials auth.CredentialSource

	// Catalog supplies the routed model's entry for effort clamping. Nil skips
	// the lookup and leaves the requested effort unclamped, which is what the
	// backend would then validate for us.
	Catalog Catalog

	// CatalogTimeout bounds the lookup. Zero uses DefaultCatalogTimeout.
	CatalogTimeout time.Duration

	// OnCatalog, when set, receives every model list the leg successfully reads
	// from the catalog. It is how the router's alias registry is kept in step
	// with what Codex actually serves, without this package taking a dependency
	// on the registry. It runs inline on the request path, so it must be cheap
	// and must not block; the caller is expected to no-op on an unchanged list.
	// The slice is the leg's own copy and is not retained.
	OnCatalog func([]cschema.CatalogModel)

	// OnUnknownEvents, when set, receives the per-type counts of Codex stream
	// events the Translator did not recognise, once per completed stream. It is
	// the early warning for upstream protocol drift, surfaced on /healthz. It
	// must not block.
	OnUnknownEvents func(map[string]int)

	// Estimator counts input tokens for the message_start seed and for
	// count_tokens. Nil uses tokens.Codex(), the exact o200k_base count.
	Estimator tokens.RequestEstimator

	// EmitReasoning is "thinking" (default) or "drop"; OnTruncate is "error"
	// (default) or "finish". Both are passed through to the Translator.
	EmitReasoning string
	OnTruncate    string

	// Summary overrides the reasoning summary mode ("none" omits it). Empty
	// takes the model's catalog default.
	Summary string

	// Heartbeat is the SSE keepalive interval for streaming responses. Zero
	// uses the Translator's default; negative disables it. It is always
	// disabled for a non-streaming request, where a keepalive means nothing.
	Heartbeat time.Duration

	// UpstreamIdle aborts a stream after this much upstream silence. Zero uses
	// the Translator's default; negative disables it.
	UpstreamIdle time.Duration

	// Logger receives operational logs. Nil discards.
	Logger *slog.Logger
}

// Leg is the Codex inference backend. It satisfies router.Leg and is safe for
// concurrent use.
type Leg struct {
	client         responses.Streamer
	creds          auth.CredentialSource
	cat            Catalog
	catalogTimeout time.Duration
	onCatalog      func([]cschema.CatalogModel)
	onUnknown      func(map[string]int)
	est            tokens.RequestEstimator
	emitReasoning  string
	onTruncate     string
	summary        string
	heartbeat      time.Duration
	upstreamIdle   time.Duration
	log            *slog.Logger
}

var _ router.Leg = (*Leg)(nil)

// New builds a Leg.
func New(opts Options) (*Leg, error) {
	if opts.Client == nil {
		return nil, errors.New("utraque/codex/leg: nil responses client")
	}
	l := &Leg{
		client:         opts.Client,
		creds:          opts.Credentials,
		cat:            opts.Catalog,
		catalogTimeout: opts.CatalogTimeout,
		onCatalog:      opts.OnCatalog,
		onUnknown:      opts.OnUnknownEvents,
		est:            opts.Estimator,
		emitReasoning:  opts.EmitReasoning,
		onTruncate:     opts.OnTruncate,
		summary:        opts.Summary,
		heartbeat:      opts.Heartbeat,
		upstreamIdle:   opts.UpstreamIdle,
		log:            opts.Logger,
	}
	if l.catalogTimeout <= 0 {
		l.catalogTimeout = DefaultCatalogTimeout
	}
	if l.est == nil {
		l.est = tokens.Codex()
	}
	if l.log == nil {
		l.log = slog.New(slog.DiscardHandler)
	}
	return l, nil
}

// Messages serves POST /v1/messages for a codex-routed model.
//
// It renders every failure it is able to render itself, so the error it returns
// is for the dispatcher's log — and, when it wraps router.ErrResponseStarted,
// to tell the dispatcher that bytes are already committed and no envelope may
// follow.
func (l *Leg) Messages(w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	ctx := r.Context()
	log := l.logger(rq)
	markRoute(w.Header(), rq)

	var req aschema.MessagesRequest
	if err := json.Unmarshal(rq.Raw, &req); err != nil {
		return apierr.Wrap(err, apierr.TypeInvalidRequest,
			"request body is not a valid Messages request")
	}
	if l.creds == nil {
		return noCredentialError(rq.Dec)
	}

	// The credential is resolved before translation so a missing or unrefreshable
	// login fails fast, with the actionable message, before any work is done.
	cred, err := l.creds.Get(ctx)
	if err != nil {
		return credentialError(err)
	}

	model := l.catalogModel(ctx, cred, rq.Dec.UpstreamModel, log)
	creq, meta, err := request.Translate(&req, rq.Dec, model, request.Options{Summary: l.summary})
	if err != nil {
		return apierr.Wrap(err, apierr.TypeInvalidRequest, "translating the request for the codex backend failed")
	}
	logTranslation(ctx, log, rq, meta)

	// The message_start seed is counted on its own goroutine, from the request
	// exactly as it goes upstream, so a 200k-token prompt never delays sending
	// it: the translator asks for the number only when it needs it, and waits
	// only if the count is slower than the upstream's first event.
	seed := startSeed(l.est, creq, log)

	// The resolved effort belongs on the request line: "why did this answer
	// take so long" is usually answered by the effort, not the model.
	obs.SummaryFrom(ctx).SetEffort(meta.Effort.Applied)

	// The request body as it was received. This is prompt text, which is why
	// tracing is behind its own env var and prints a warning at startup.
	trace := obs.TraceFrom(ctx)
	trace.SetBody(rq.Raw)

	// StreamWithRefresh owns the one sanctioned retry: on an upstream 401 it
	// invalidates the credential, refreshes, and retries exactly once.
	upstream, err := l.client.StreamWithRefresh(ctx, l.creds, creq)
	if err != nil {
		// Nothing has been written yet, so this is failure mode 1: a real HTTP
		// status, the Anthropic envelope, and the upstream's own quota headers.
		return l.renderUpstreamFailure(ctx, w, log, err)
	}
	defer func() { _ = upstream.Close() }()

	// Tee the raw upstream stream to <id>.upstream.sse when tracing. A trace of
	// the bytes we RECEIVED, alongside the bytes we SENT, is what makes a
	// translation bug reproducible as a fixture instead of a bug report.
	upstream = trace.TeeUpstream(upstream)

	if rq.Stream {
		return l.serveStream(ctx, w, rq, upstream, seed, log)
	}
	return l.serveAggregate(ctx, w, rq, upstream, seed, log)
}

// CountTokens serves POST /v1/messages/count_tokens for a codex-routed model.
//
// The Codex backend exposes no token-counting endpoint, and asking it would
// spend a real inference request, so this is answered locally. The request is
// run through the same translation a real turn would get and the RESULT is
// counted, so the number reflects what would actually go upstream — the
// billing header dropped, thinking text replaced by its encrypted replay, and
// so on — rather than the Anthropic-shaped body the client sent. It is a
// lower bound by design (see internal/tokens): it drives the client's context
// bar, never anything billing-shaped, and a bar that reads a few percent low
// in a reasoning-heavy session is the accepted cost of a seed that never
// overshoots.
func (l *Leg) CountTokens(w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	markRoute(w.Header(), rq)

	var req aschema.CountTokensRequest
	if err := json.Unmarshal(rq.Raw, &req); err != nil {
		return apierr.Wrap(err, apierr.TypeInvalidRequest,
			"request body is not a valid count_tokens request")
	}

	// No catalog lookup: it needs a credential, and effort clamping does not
	// change a single prompt token.
	creq, _, err := request.Translate(&aschema.MessagesRequest{
		Model:      req.Model,
		Messages:   req.Messages,
		System:     req.System,
		Tools:      req.Tools,
		ToolChoice: req.ToolChoice,
	}, rq.Dec, cschema.CatalogModel{}, request.Options{Summary: l.summary})
	if err != nil {
		return apierr.Wrap(err, apierr.TypeInvalidRequest, "translating the request for the codex backend failed")
	}

	body, err := json.Marshal(aschema.CountTokensResponse{InputTokens: l.est.EstimateRequest(creq)})
	if err != nil {
		return apierr.Wrap(err, apierr.TypeAPI, "encoding the token count failed")
	}
	body = append(body, '\n')

	l.logger(rq).LogAttrs(r.Context(), slog.LevelDebug, "codex count_tokens counted locally",
		slog.String("estimator", l.est.Name()), slog.Int("bytes", len(rq.Raw)))

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

// serveStream translates the upstream SSE body straight onto the client's
// connection.
//
// The client's 200 is NOT committed up front: it is written lazily by the first
// sink frame. That is what keeps failure mode 1 available for a 200 whose body
// turns out to be empty — a status line already on the wire could not be taken
// back, and an error envelope appended to it would corrupt the stream.
func (l *Leg) serveStream(ctx context.Context, w http.ResponseWriter, rq *router.Request, upstream io.ReadCloser, seed *seed, log *slog.Logger) error {
	lw := newLazyWriter(w, func(h http.Header) {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		// Connection is deliberately NOT set: it is hop-by-hop, net/http already
		// manages keep-alive on HTTP/1.1, and it is illegal over HTTP/2.
		//
		// Nginx and friends buffer text/event-stream by default, which would
		// re-batch a true-incremental translation back into one lump.
		h.Set("X-Accel-Buffering", "no")
	})

	// The downstream tee sits between the translator and the client, so the
	// trace records exactly the bytes the client received — including the
	// heartbeat pings and the frame boundaries, which is where an SSE bug
	// usually is.
	tr := stream.New(l.translatorOptions(ctx, rq, seed, l.heartbeat, log))
	res, err := tr.Run(ctx, upstream, stream.NewSSEWriter(obs.TraceFrom(ctx).TeeDownstream(lw)))
	l.logResult(ctx, log, rq, res, seed)

	if err == nil {
		return nil
	}
	if lw.Started() {
		// Bytes are committed: failure mode 3 (or a write failure onto a dead
		// connection). Report it for the log; the dispatcher must not answer.
		markInterrupted(ctx, err)
		return fmt.Errorf("%w: %w", router.ErrResponseStarted, err)
	}
	return l.renderStartFailure(ctx, w, log, err)
}

// serveAggregate folds the upstream stream into one MessagesResponse.
//
// Nothing is written until the fold succeeds, so every failure — including a
// mid-stream one — reaches the client as a real HTTP status rather than as a
// truncated answer dressed up as a complete one.
func (l *Leg) serveAggregate(ctx context.Context, w http.ResponseWriter, rq *router.Request, upstream io.ReadCloser, seed *seed, log *slog.Logger) error {
	agg := stream.NewAggregator()
	// A keepalive has no meaning when nothing is on the wire yet.
	tr := stream.New(l.translatorOptions(ctx, rq, seed, -1, log))
	res, err := tr.Run(ctx, upstream, agg)
	l.logResult(ctx, log, rq, res, seed)
	if err != nil {
		return l.renderStartFailure(ctx, w, log, err)
	}

	body, err := agg.MessageJSON()
	if err != nil {
		return l.renderStartFailure(ctx, w, log, err)
	}
	body = append(body, '\n')
	// A non-streaming answer is a body, not a stream, so it is traced as one
	// rather than dressed up in SSE frames it never had.
	obs.TraceFrom(ctx).WriteDownstream(body)

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

// translatorOptions builds the Translator configuration shared by both sinks, so
// the streaming and non-streaming paths cannot be configured differently by
// accident.
func (l *Leg) translatorOptions(ctx context.Context, rq *router.Request, seed *seed, heartbeat time.Duration, log *slog.Logger) stream.Options {
	return stream.Options{
		// Echo the upstream slug utraque actually called, not the model string
		// the caller wrote. Claude Code (verified against 2.1.241) does not match
		// this against its request, and it records what it receives into its own
		// session log — so echoing the slug is what makes that log name the model
		// that really served the turn, which is what usage reporting downstream
		// has to price. The reasoning effort is deliberately left off: it is not
		// part of the model's identity, and the client does not record it for
		// Anthropic models either.
		UpstreamModel:   upstreamModel(rq),
		InputTokensFunc: func() int { return seed.Value(ctx) },
		EmitReasoning:   l.emitReasoning,
		OnTruncate:      l.onTruncate,
		Heartbeat:       heartbeat,
		UpstreamIdle:    l.upstreamIdle,
		Logger:          log,
	}
}

// upstreamModel is the model name echoed back to the caller: the slug the leg
// actually asked Codex for. It falls back to the caller's own string when the
// decision carries no slug — which the Codex leg should never see, since routing
// here always resolves one, but a zero Decision must not echo an empty model.
func upstreamModel(rq *router.Request) string {
	if rq.Dec.UpstreamModel != "" {
		return rq.Dec.UpstreamModel
	}
	return rq.Model
}

// renderUpstreamFailure answers a failure that happened before the upstream
// stream opened. A typed UpstreamError carries the status utraque should answer
// with and the quota headers to forward; anything else falls back to the
// dispatcher's generic rendering.
func (l *Leg) renderUpstreamFailure(ctx context.Context, w http.ResponseWriter, log *slog.Logger, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The caller hung up while we were opening the stream. There is nobody
		// left to render an envelope for.
		obs.SummaryFrom(ctx).SetInterrupted(true)
		return fmt.Errorf("%w: %w", router.ErrClientGone, ctxErr)
	}
	obs.SummaryFrom(ctx).SetErr(err)

	ue, ok := responses.AsUpstream(err)
	if !ok {
		// A plain *apierr.Error (no credential, unencodable request); the
		// dispatcher renders it.
		return err
	}

	// Retry-After and the x-codex-*/x-ratelimit-* windows are forwarded verbatim
	// so the client's backoff obeys the backend rather than a number we invented.
	ue.ApplyHeaders(w.Header())
	status := ue.HTTPStatus()
	log.LogAttrs(ctx, slog.LevelWarn, "codex leg failed before the response started",
		slog.String("class", string(ue.Class)),
		slog.Int("upstream_status", ue.Status),
		slog.Int("status", status))
	_ = ue.APIError().Render(w, status)
	return nil
}

// renderStartFailure answers a failure the Translator reported with nothing yet
// written (failure mode 1 as seen from inside the stream), or an Aggregator fold
// that refused to present a partial message.
func (l *Leg) renderStartFailure(ctx context.Context, w http.ResponseWriter, log *slog.Logger, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		obs.SummaryFrom(ctx).SetInterrupted(true)
		return fmt.Errorf("%w: %w", router.ErrClientGone, ctxErr)
	}
	obs.SummaryFrom(ctx).SetErr(err)
	ae := classifyStreamFailure(err)
	log.LogAttrs(ctx, slog.LevelWarn, "codex leg failed before any output reached the client",
		slog.Int("status", ae.HTTPStatus()), slog.String("err", err.Error()))
	_ = ae.Render(w, 0)
	return nil
}

// markInterrupted records a mid-stream failure that was the client hanging up
// rather than the upstream failing. The distinction is the whole reason
// "interrupted" is its own field: an abandoned request is not an error worth
// paging over, and it must not be counted as one.
func markInterrupted(ctx context.Context, err error) {
	sum := obs.SummaryFrom(ctx)
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		sum.SetInterrupted(true)
		return
	}
	sum.SetErr(err)
}

// classifyStreamFailure maps a Translator/Aggregator failure onto the status the
// client should see. An upstream that opened a 200 and then said nothing is a
// gateway failure (502), not an internal one (500): the distinction is what
// tells the caller whether retrying could help.
func classifyStreamFailure(err error) *apierr.Error {
	switch {
	case errors.Is(err, stream.ErrNoData):
		e := apierr.Wrap(err, apierr.TypeAPI,
			"the codex backend opened a stream but sent no events")
		e.Status = http.StatusBadGateway
		return e
	case errors.Is(err, stream.ErrIdleTimeout):
		return apierr.Wrap(err, apierr.TypeTimeout, "the codex backend stopped sending data")
	case errors.Is(err, stream.ErrTruncated), errors.Is(err, stream.ErrIncomplete):
		e := apierr.Wrap(err, apierr.TypeAPI,
			"the codex backend closed the stream before the response was complete")
		e.Status = http.StatusBadGateway
		return e
	}
	if ue, ok := responses.AsUpstream(err); ok {
		return ue.APIError()
	}
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae
	}
	e := apierr.Wrap(err, apierr.TypeAPI, "the codex backend stream failed")
	e.Status = http.StatusBadGateway
	return e
}

// catalogModel looks up the routed slug's catalog entry, used only to clamp
// reasoning effort and to source the default summary. Every failure — no
// catalog, a fetch error, an unknown slug — yields the zero Model, which leaves
// the requested effort unclamped. A picker open must never be blocked on this,
// and neither must an inference request.
func (l *Leg) catalogModel(ctx context.Context, cred auth.Credential, slug string, log *slog.Logger) cschema.CatalogModel {
	if l.cat == nil || slug == "" {
		return cschema.CatalogModel{}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, l.catalogTimeout)
	defer cancel()

	models, err := l.cat.Models(lookupCtx, cred)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelDebug,
			"codex catalog unavailable; sending the requested reasoning effort unclamped",
			slog.String("slug", slug), slog.String("err", err.Error()))
		return cschema.CatalogModel{}
	}
	// The same read that clamps effort is also the freshest view of what Codex
	// serves, so it is what keeps the router's aliases current. Doing it here
	// rather than on a timer means no extra request and no extra goroutine.
	if l.onCatalog != nil && len(models) > 0 {
		l.onCatalog(models)
	}
	for i := range models {
		if strings.EqualFold(models[i].Slug, slug) {
			return models[i]
		}
	}
	log.LogAttrs(ctx, slog.LevelDebug, "codex catalog has no entry for the routed slug",
		slog.String("slug", slug))
	return cschema.CatalogModel{}
}

func (l *Leg) logger(rq *router.Request) *slog.Logger {
	if rq != nil && rq.Log != nil {
		return rq.Log
	}
	return l.log
}

// logResult records the translation outcome, including the unknown-event counts
// that are the early warning for upstream protocol drift.
//
// How the answer ended — the stop reason and the completion size — goes on the
// request line rather than a second log line of its own, so one request stays
// one record.
func (l *Leg) logResult(ctx context.Context, log *slog.Logger, rq *router.Request, res stream.Result, seed *seed) {
	if sum := obs.SummaryFrom(ctx); sum != nil {
		sum.SetStopReason(res.StopReason)
		if res.Terminated && !res.Errored {
			sum.SetOutputTokens(res.OutputTokens)
			sum.SetInputTokens(res.InputTokens, res.CachedInputTokens)
		}
		// The seed goes beside the real counts so the lower-bound invariant
		// (estimated <= input + cache_read) can be checked from the log. Once
		// message_start went out the count is already resolved; before that
		// the request failed, and a failed request is not held up for a number
		// nothing will compare against.
		if res.Started {
			sum.SetEstimatedInputTokens(seed.Value(ctx))
		} else if n, ok := seed.Peek(); ok {
			sum.SetEstimatedInputTokens(n)
		}
	}

	attrs := []slog.Attr{
		slog.String("upstream_model", rq.Dec.UpstreamModel),
		slog.Bool("started", res.Started),
		slog.Bool("terminated", res.Terminated),
		slog.Bool("errored", res.Errored),
	}
	if len(res.UnknownEvents) > 0 {
		if l.onUnknown != nil {
			l.onUnknown(res.UnknownEvents)
		}
		attrs = append(attrs, slog.Any("unknown_events", res.UnknownEvents))
		log.LogAttrs(ctx, slog.LevelInfo, "codex stream carried unrecognised event types", attrs...)
		return
	}
	log.LogAttrs(ctx, slog.LevelDebug, "codex stream translated", attrs...)
}

// logTranslation reports what the request translator dropped and how effort was
// resolved. It is DEBUG because it is per-request detail, and it exists so a
// surprising answer can be traced to the translation without re-deriving it.
func logTranslation(ctx context.Context, log *slog.Logger, rq *router.Request, meta request.Metadata) {
	if !log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{
		slog.String("upstream_model", rq.Dec.UpstreamModel),
		slog.String("effort", meta.Effort.Applied),
		slog.String("effort_requested", meta.Effort.Requested),
		slog.String("effort_source", meta.Effort.Source),
		slog.Bool("effort_clamped", meta.Effort.Clamped),
		slog.Bool("parallel_tool_calls_disabled", meta.ParallelToolCallsDisabled),
	}
	if meta.ReasoningReplayed > 0 || meta.ReasoningUnreplayable > 0 {
		attrs = append(attrs,
			slog.Int("reasoning_replayed", meta.ReasoningReplayed),
			slog.Int("reasoning_unreplayable", meta.ReasoningUnreplayable))
	}
	if len(meta.Dropped) > 0 {
		attrs = append(attrs, slog.Any("dropped", meta.Dropped))
	}
	if len(meta.OrphanedToolResults) > 0 {
		attrs = append(attrs, slog.Int("orphaned_tool_results", len(meta.OrphanedToolResults)))
	}
	if len(meta.DroppedImages) > 0 {
		attrs = append(attrs, slog.Int("dropped_images", len(meta.DroppedImages)))
	}
	if len(meta.RewrittenPatterns) > 0 {
		attrs = append(attrs, slog.Any("rewritten_patterns", meta.RewrittenPatterns))
	}
	if len(meta.DroppedPatterns) > 0 {
		attrs = append(attrs, slog.Any("dropped_patterns", meta.DroppedPatterns))
	}
	log.LogAttrs(ctx, slog.LevelDebug, "translated a Messages request for the codex backend", attrs...)
}

// seed is the message_start input-token estimate, counted on its own goroutine
// so that tokenizing the prompt never sits between translating the request and
// sending it upstream. The translator takes Value as its lazy seed and calls
// it on the first event that needs the number; by then the upstream has
// usually spent far longer thinking than the count took.
//
// The count is bounded — internal/tokens caps the work per field at a small
// multiple of its length — so the wait is short even on a hostile prompt,
// and it is also cancellable: a request whose context ends while the count
// is still running takes 0 rather than waiting, so a client that has gone
// away never holds the translator (and its idle timer) on a number nobody
// will read. The goroutine itself runs to completion; its per-field results
// are memoized, so a retry of the same prompt gets them for free.
type seed struct {
	done chan struct{}
	n    int
}

// startSeed begins counting creq. creq is read concurrently by the responses
// client, which only marshals it; nothing writes to it after translation.
//
// A panic on a helper goroutine would take the whole process down, where the
// same panic on the request goroutine is recovered by net/http, so it is
// recovered here and reported as a zero seed: a missing estimate is a blemish
// on one message_start, a crash is every session on the machine.
func startSeed(est tokens.RequestEstimator, creq *cschema.ResponsesRequest, log *slog.Logger) *seed {
	s := &seed{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer func() {
			if r := recover(); r != nil {
				s.n = 0
				log.LogAttrs(context.Background(), slog.LevelError,
					"input token count panicked; seeding message_start with 0",
					slog.String("estimator", est.Name()), slog.Any("panic", r))
			}
		}()
		s.n = est.EstimateRequest(creq)
	}()
	return s
}

// Value waits for the count, or for ctx to end, whichever is first; a
// finished count wins over a finished context, so a request that completed
// normally always logs its real seed.
func (s *seed) Value(ctx context.Context) int {
	select {
	case <-s.done:
		return s.n
	default:
	}
	select {
	case <-s.done:
		return s.n
	case <-ctx.Done():
		return 0
	}
}

// Peek returns the count if it is ready, without waiting.
func (s *seed) Peek() (int, bool) {
	select {
	case <-s.done:
		return s.n, true
	default:
		return 0, false
	}
}

// markRoute stamps the debug headers. They are set before anything is written so
// they survive whichever layer ends up committing the status line — including
// the dispatcher rendering an error envelope on this leg's behalf.
func markRoute(h http.Header, rq *router.Request) {
	h.Set(HeaderRoute, string(router.BackendCodex))
	if rq != nil && rq.Dec.UpstreamModel != "" {
		h.Set(HeaderModel, rq.Dec.UpstreamModel)
	}
}

// noCredentialError is the answer when no Codex login is configured at all.
//
// It is a 503 rather than a 401: the caller's own Authorization header is fine —
// it is utraque that has nothing to spend — and a 401 would send Claude Code
// off to re-authenticate against Anthropic, which would not help.
func noCredentialError(dec router.Decision) *apierr.Error {
	return apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeAPI,
		"codex leg unavailable: no Codex credential is configured, so model %q "+
			"(upstream model %q) cannot be served; run `codex login` or set UTRAQUE_CODEX_AUTH_FILE",
		dec.ClientModel, dec.UpstreamModel)
}

// credentialError renders a failure to obtain or refresh the Codex credential.
// A refresh failure is actionable by the operator, so it says what to do; the
// token value itself never appears.
func credentialError(err error) *apierr.Error {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae
	}
	return apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeAPI,
		"codex leg unavailable: could not obtain a usable Codex credential (%v); try `codex login`", err)
}
