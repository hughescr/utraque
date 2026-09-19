// Package deepseek implements utraque's dedicated DeepSeek leg using the
// vendor's Anthropic-compatible API. Unlike the Anthropic passthrough, it owns
// an API key and therefore forwards request headers by allowlist only.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/proxyhdr"
	"github.com/hughescr/utraque/internal/relay"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/sse"
	"github.com/hughescr/utraque/internal/tokens"
	"github.com/hughescr/utraque/internal/toolschema"
	"github.com/hughescr/utraque/internal/transport"
)

const (
	DefaultBaseURL          = "https://api.deepseek.com/anthropic"
	defaultMaxResponseBytes = 128 << 20
	copyBufferSize          = 32 << 10
)

// Option configures a Leg.
type Option func(*Leg)

func WithLogger(log *slog.Logger) Option {
	return func(l *Leg) {
		if log != nil {
			l.log = log
		}
	}
}

func WithUpstreamIdleTimeout(d time.Duration) Option {
	return func(l *Leg) {
		if d < 0 {
			d = 0
		}
		l.upstreamIdle = d
	}
}

// Leg is the DeepSeek Anthropic-compatible backend. The key is kept private to
// this object and installed only after all caller headers have been filtered.
type Leg struct {
	base         *url.URL
	apiKey       string
	tr           transport.Transport
	client       *http.Client
	log          *slog.Logger
	upstreamIdle time.Duration
	estimator    tokens.Estimator
}

var _ router.Leg = (*Leg)(nil)

func New(baseURL, apiKey string, tr transport.Transport, opts ...Option) (*Leg, error) {
	if tr == nil {
		return nil, errors.New("utraque/deepseek: nil transport")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("utraque/deepseek: empty api key")
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("utraque/deepseek: parse base url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("utraque/deepseek: base url must be an absolute http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("utraque/deepseek: base url must not contain credentials, a query, or a fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""

	client := *tr.Client()
	// Enforce this at the credential-owning leg even when a test or future
	// transport implementation hands us a client with the default follow policy.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	l := &Leg{
		base:      u,
		apiKey:    strings.TrimSpace(apiKey),
		tr:        tr,
		client:    &client,
		log:       slog.New(slog.DiscardHandler),
		estimator: tokens.Default(),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

func (l *Leg) BaseURL() string { return l.base.String() }

func (l *Leg) Messages(w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	markRoute(w.Header(), rq)
	return l.forward(w, r, rq, "/v1/messages", true)
}

func (l *Leg) CountTokens(w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	if rq == nil || rq.Dec.Backend != router.BackendDeepSeek || rq.Dec.UpstreamModel == "" {
		return apierr.InvalidRequest("deepseek request has no resolved model")
	}
	if _, _, err := rewriteRequest(rq.Raw, rq.Dec.UpstreamModel); err != nil {
		return err
	}
	var request aschema.CountTokensRequest
	if err := json.Unmarshal(rq.Raw, &request); err != nil {
		return apierr.Wrap(err, apierr.TypeInvalidRequest, "request body is not a valid count_tokens request")
	}
	body, err := json.Marshal(tokens.Count(l.estimator, &request))
	if err != nil {
		return apierr.Wrap(err, apierr.TypeAPI, "encoding the deepseek token estimate failed")
	}
	body = append(body, '\n')
	l.log.DebugContext(r.Context(), "deepseek count_tokens estimated locally",
		"estimator", l.estimator.Name(), "bytes", len(rq.Raw))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set(proxyhdr.TokenCountMethod, "estimated; estimator="+l.estimator.Name())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

// markRoute stamps the rewritten model before any response bytes are committed.
func markRoute(h http.Header, rq *router.Request) {
	h.Set(proxyhdr.Route, string(router.BackendDeepSeek))
	if rq != nil && rq.Dec.UpstreamModel != "" {
		h.Set(proxyhdr.Model, rq.Dec.UpstreamModel)
	}
}

func (l *Leg) forward(w http.ResponseWriter, r *http.Request, rq *router.Request, path string, responseHasModel bool) error {
	if rq == nil || rq.Dec.Backend != router.BackendDeepSeek || rq.Dec.UpstreamModel == "" {
		return apierr.InvalidRequest("deepseek request has no resolved model")
	}
	// The dispatcher has already read and size-limited the body into rq.Raw
	// (this leg has no catch-all ServeHTTP, so there is no other way in).
	body, report, err := rewriteRequest(rq.Raw, rq.Dec.UpstreamModel)
	if err != nil {
		return err
	}
	l.logRewrite(r.Context(), rq, report)

	ctx, idle := relay.WithUpstreamIdleDeadline(r.Context(), l.upstreamIdle)
	defer idle.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base.String()+path, bytes.NewReader(body))
	if err != nil {
		return apierr.Wrap(err, apierr.TypeInvalidRequest, "build deepseek request")
	}
	copyRequestHeaders(req.Header, r.Header)
	req.Header.Set("X-Api-Key", l.apiKey)
	req.Header.Set("Content-Type", "application/json")
	// The response body is inspected to canonicalize the model. Asking for an
	// identity representation prevents a caller's compression preference from
	// making those bytes opaque to the proxy.
	req.Header.Set("Accept-Encoding", "identity")
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = nil
	}
	req.ContentLength = int64(len(body))
	req.Host = ""

	obs.SummaryFrom(r.Context()).SetTransport(l.tr.Kind())
	resp, err := l.client.Do(req)
	if err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", router.ErrClientGone, ctxErr)
		}
		return relay.UpstreamError("deepseek upstream", err, idle)
	}
	defer resp.Body.Close()
	obs.SummaryFrom(r.Context()).SetUpstreamStatus(resp.StatusCode)

	if enc := strings.TrimSpace(resp.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI,
			"deepseek upstream returned an unexpected content encoding")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI,
			"deepseek upstream returned a redirect, which utraque will not follow")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		return l.copyBody(w, r, idle, resp.Body)
	}
	if responseHasModel && rq.Stream {
		if kind := mediaType(resp.Header); kind != "" && kind != "text/event-stream" {
			return apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI,
				"deepseek returned a non-stream response to a streaming request")
		}
		return l.streamResponse(w, r, idle, resp, rq.Dec.UpstreamModel)
	}
	return l.jsonResponse(w, r, idle, resp, rq.Dec.UpstreamModel, responseHasModel)
}

// logRewrite records, at DEBUG, what rewriteRequest changed in a tool schema:
// the same rewritten_patterns and dropped_patterns attributes the codex leg
// logs for its translation, so a tool that stopped enforcing a constraint can
// be traced to the rewrite without re-deriving it. Requests whose schemas went
// through untouched log nothing.
func (l *Leg) logRewrite(ctx context.Context, rq *router.Request, report toolschema.Report) {
	if report.Empty() {
		return
	}
	if !l.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := append([]slog.Attr{slog.String("upstream_model", rq.Dec.UpstreamModel)}, report.LogAttrs()...)
	l.log.LogAttrs(ctx, slog.LevelDebug, "rewrote tool schema patterns for the deepseek backend", attrs...)
}

func (l *Leg) jsonResponse(w http.ResponseWriter, r *http.Request, idle *relay.UpstreamIdleGuard, resp *http.Response, canonical string, rewriteModel bool) error {
	b, err := io.ReadAll(io.LimitReader(idle.Wrap(resp.Body), defaultMaxResponseBytes+1))
	if err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", router.ErrClientGone, ctxErr)
		}
		return relay.UpstreamError("deepseek upstream", err, idle)
	}
	if len(b) > defaultMaxResponseBytes {
		return apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI, "deepseek response exceeded the safety limit")
	}
	if rewriteModel {
		b, err = rewriteMessageResponse(b, canonical)
		if err != nil {
			e := apierr.Wrap(err, apierr.TypeAPI, "deepseek returned an invalid message response")
			e.Status = http.StatusBadGateway
			return e
		}
		observeMessage(r.Context(), b)
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(b)
	if err != nil {
		return fmt.Errorf("%w: writing deepseek response: %w", router.ErrResponseStarted, err)
	}
	return nil
}

func (l *Leg) streamResponse(w http.ResponseWriter, r *http.Request, idle *relay.UpstreamIdleGuard, resp *http.Response, canonical string) error {
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(resp.StatusCode)

	scanner := sse.NewScanner(idle.Wrap(resp.Body))
	writer := sse.NewFrameWriter(w)
	sawStart, sawTerminal, sawError := false, false, false
	for scanner.Scan() {
		frame := scanner.Frame()
		data, eventType, err := rewriteStreamFrame(frame, canonical)
		if err != nil {
			return fmt.Errorf("%w: invalid deepseek stream: %w", router.ErrResponseStarted, err)
		}
		if eventType == aschema.EventMessageStart {
			sawStart = true
		}
		switch eventType {
		case aschema.EventMessageStop:
			sawTerminal = true
		case aschema.EventError:
			sawTerminal = true
			sawError = true
		}
		observeStreamFrame(r.Context(), eventType, data)
		if err := writer.WriteFrame(frame.Event, data); err != nil {
			return fmt.Errorf("%w: writing deepseek stream: %w", router.ErrResponseStarted, err)
		}
		if err := writer.Flush(); err != nil {
			return fmt.Errorf("%w: flushing deepseek stream: %w", router.ErrResponseStarted, err)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w: %w", router.ErrResponseStarted, router.ErrClientGone, ctxErr)
		}
		if idle.Fired() {
			return fmt.Errorf("%w: deepseek stream went silent for %s: %w", router.ErrResponseStarted, l.upstreamIdle, err)
		}
		return fmt.Errorf("%w: reading deepseek stream: %w", router.ErrResponseStarted, err)
	}
	if !sawTerminal {
		return fmt.Errorf("%w: deepseek stream ended without message_stop or error", router.ErrResponseStarted)
	}
	if !sawStart && !sawError {
		// A terminal error frame is allowed to stand alone. A clean message_stop
		// is not: without message_start there was never a response to complete.
		return fmt.Errorf("%w: deepseek stream ended without message_start", router.ErrResponseStarted)
	}
	return nil
}

func (l *Leg) copyBody(w http.ResponseWriter, r *http.Request, idle *relay.UpstreamIdleGuard, src io.Reader) error {
	_, err := io.CopyBuffer(w, idle.Wrap(src), make([]byte, copyBufferSize))
	if err == nil {
		return nil
	}
	if ctxErr := r.Context().Err(); ctxErr != nil {
		return fmt.Errorf("%w: %w: %w", router.ErrResponseStarted, router.ErrClientGone, ctxErr)
	}
	return fmt.Errorf("%w: relaying deepseek response: %w", router.ErrResponseStarted, err)
}

var requestHeaderAllow = map[string]struct{}{
	"Accept":            {},
	"Anthropic-Beta":    {},
	"Anthropic-Version": {},
	"Content-Type":      {},
	"User-Agent":        {},
	"X-Request-Id":      {},
}

func copyRequestHeaders(dst, src http.Header) {
	for name := range requestHeaderAllow {
		if values, ok := src[name]; ok {
			dst[name] = slices.Clone(values)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		lower := strings.ToLower(canonical)
		switch {
		case canonical == "Content-Type", canonical == "Cache-Control", canonical == "Retry-After",
			canonical == "Request-Id", canonical == "X-Request-Id",
			strings.HasPrefix(lower, "x-ratelimit-"), strings.HasPrefix(lower, "anthropic-ratelimit-"):
			dst[canonical] = slices.Clone(values)
		}
	}
	dst.Set("X-Content-Type-Options", "nosniff")
}

func mediaType(h http.Header) string {
	t, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return strings.ToLower(t)
}

func observeMessage(ctx context.Context, b []byte) {
	var msg aschema.MessagesResponse
	if json.Unmarshal(b, &msg) != nil {
		return
	}
	sum := obs.SummaryFrom(ctx)
	sum.SetInputTokens(msg.Usage.InputTokens, msg.Usage.CacheReadInputTokens, msg.Usage.CacheCreationInputTokens)
	sum.SetOutputTokens(msg.Usage.OutputTokens)
	sum.SetStopReason(msg.StopReason)
}

func observeStreamFrame(ctx context.Context, event string, data []byte) {
	sum := obs.SummaryFrom(ctx)
	switch event {
	case aschema.EventMessageStart:
		var e aschema.MessageStartEvent
		if json.Unmarshal(data, &e) == nil {
			u := e.Message.Usage
			sum.SetInputTokens(u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
		}
	case aschema.EventMessageDelta:
		var e aschema.MessageDeltaEvent
		if json.Unmarshal(data, &e) == nil {
			if e.Usage != nil {
				sum.SetOutputTokens(e.Usage.OutputTokens)
			}
			sum.SetStopReason(e.Delta.StopReason)
		}
	}
}
