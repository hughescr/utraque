// Package responses is the HTTP client that actually calls the Codex backend:
// POST {base}/responses, signed with the Codex OAuth credential, opened as a
// Server-Sent Events stream.
//
// It deliberately does two things and no more:
//
//   - Open the stream. On success the caller gets the raw SSE body to hand to
//     the stream translator; this package never parses an event.
//   - Classify a failure. Nothing streams before the response headers arrive,
//     so a non-200 is the "mode 1" failure of the plan's phase 5: a real HTTP
//     status the leg can render as an Anthropic error envelope. Every failure
//     becomes an *UpstreamError carrying the upstream status, the upstream's own
//     message when it sent one, the forwarded rate-limit headers, and an
//     *apierr.Error with the status utraque should answer with.
//
// It does NOT retry. Retry policy belongs to the leg, which can see the whole
// request. The single exception is the documented credential-refresh hook:
// StreamPinned invalidates a rejected credential and permits one retry with
// a different token for the same account. A changed login requires resubmission.
//
// A bot/TLS gate (a Cloudflare challenge rather than an API answer) is detected,
// typed as ClassGate with an actionable message, and reported to the transport.
// In the default auto mode that report is what switches the process onto the
// uTLS (Chrome TLS fingerprint) transport, once. This package decides only that
// a gate happened; the transport decides what to do about it.
//
// No token or account id is ever logged, returned in an error, or otherwise
// disclosed: the credential is used solely to sign the request.
package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/codex/wire"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/transport"
)

// Wire constants. The backend root and the Codex CLI identity headers come
// from internal/codex/wire, so the set sent here is exactly the one the
// catalog client sends; the rest are the ordinary HTTP names this endpoint
// needs.
const (
	// Path is the responses endpoint under the base URL.
	Path = "/responses"

	headerAuthorization = "Authorization"
	headerContentType   = "Content-Type"
	headerAccept        = "Accept"
	headerSessionID     = "session_id"

	// promptCacheKeyPrefix is what request.promptCacheKey stamps on every key it
	// mints. Only a key carrying it can name a conversation here.
	promptCacheKeyPrefix = "utq-"

	acceptSSE       = "text/event-stream"
	contentTypeJSON = "application/json"

	// defaultMaxErrorBody bounds how much of a failing response we read. An
	// error body is small; a challenge page is not, and neither may be
	// unbounded.
	defaultMaxErrorBody = 64 << 10
	// maxErrorMessage bounds (in runes) how much upstream text reaches an error
	// envelope or a log line.
	maxErrorMessage = 512

	loginChangedMessage = "the Codex login changed while this request was in flight; it was not sent under the new account — resubmit to send it under the current login"
)

// Options configures a Client. Every field is optional.
type Options struct {
	// BaseURL is the Codex backend root. Defaults to wire.DefaultBaseURL.
	BaseURL string
	// Transport supplies the HTTP client. Defaults to the standard no-redirect
	// transport. A redirect is never followed: it would replay the caller's
	// bearer token at whatever host the upstream named.
	Transport transport.Transport
	// MaxErrorBody bounds the bytes read from a failing response. Defaults to
	// defaultMaxErrorBody.
	MaxErrorBody int64
	// OnRateLimits, when set, gets the signing credential's account scope and
	// the quota headers of every upstream response, success or failure, so
	// /healthz and the request log can report the usage window. It must not block.
	OnRateLimits func(scope string, rl RateLimits)
	// Now supplies the current time, used to resolve an HTTP-date Retry-After.
	// Defaults to time.Now.
	Now func() time.Time
	// Logger receives redacted operational logs. Defaults to slog.Default.
	Logger *slog.Logger
}

// Client posts to the Codex responses endpoint. It is safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
	// tr is held, not just its Kind() snapshot: the auto transport can switch
	// stacks mid-process, and a kind captured at construction would keep
	// logging "std" after every request had moved to uTLS.
	tr           transport.Transport
	maxErrorBody int64
	onRateLimits func(string, RateLimits)
	now          func() time.Time
	log          *slog.Logger
}

// Streamer opens the SSE stream using the snapshot used for translation.
// The source is consulted only after a refreshable rejection.
type Streamer interface {
	StreamPinned(ctx context.Context, cred auth.Credential, src auth.CredentialSource, req *cschema.ResponsesRequest) (*Response, error)
}

var _ Streamer = (*Client)(nil)

// New builds a Client. No network access happens here.
func New(opts Options) *Client {
	c := &Client{
		baseURL:      strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		maxErrorBody: opts.MaxErrorBody,
		onRateLimits: opts.OnRateLimits,
		now:          opts.Now,
		log:          opts.Logger,
	}
	if c.baseURL == "" {
		c.baseURL = wire.DefaultBaseURL
	}
	if c.maxErrorBody <= 0 {
		c.maxErrorBody = defaultMaxErrorBody
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	tr := opts.Transport
	if tr == nil {
		tr = transport.NewStd(transport.DefaultOptions())
	}
	c.tr = tr
	c.http = tr.Client()
	return c
}

// Response is an opened upstream stream plus the metadata that came with its
// headers. Body is the raw SSE stream; the caller owns it and MUST Close it.
type Response struct {
	// PublicDetail redacts errors with the credential that signed this attempt
	// and any token an earlier, rejected attempt of the same request presented.
	PublicDetail func(string) string
	// Status is the upstream HTTP status (always 200 here — any other status is
	// returned as an *UpstreamError instead).
	Status int
	// Header is a copy of the upstream response headers.
	Header http.Header
	// RateLimits are the forwarded quota headers for /healthz and logging.
	RateLimits RateLimits
	// Body is the SSE stream. Close is idempotent.
	Body io.ReadCloser
}

// Close releases the stream. It is safe to call more than once.
func (r *Response) Close() error {
	if r == nil || r.Body == nil {
		return nil
	}
	return r.Body.Close()
}

// Stream POSTs req to {base}/responses and returns the upstream SSE body.
//
// A non-200 never reaches the caller as a body: it is read (bounded), closed,
// and returned as an *UpstreamError. On success the caller owns the returned
// stream and must Close it; closing it aborts the upstream request.
//
// req is not mutated — stream:true is forced on a copy, since this entry point
// only ever opens a stream.
func (c *Client) Stream(ctx context.Context, cred auth.Credential, req *cschema.ResponsesRequest) (io.ReadCloser, error) {
	resp, err := c.StreamResponse(ctx, cred, req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// StreamResponse is Stream plus the response metadata (status, headers, and the
// forwarded rate-limit headers) the leg reports on /healthz.
func (c *Client) StreamResponse(ctx context.Context, cred auth.Credential, req *cschema.ResponsesRequest) (*Response, error) {
	return c.streamAttempt(ctx, cred, req)
}

// streamAttempt is StreamResponse whose public-detail redaction also covers
// rejected, the tokens earlier attempts of the same request presented.
func (c *Client) streamAttempt(ctx context.Context, cred auth.Credential, req *cschema.ResponsesRequest, rejected ...string) (*Response, error) {
	if req == nil {
		return nil, apierr.InvalidRequest("codex responses: no request to send")
	}
	if cred.AccessToken == "" || cred.AccountID == "" {
		return nil, apierr.Authentication("codex responses: no usable Codex credential; run `codex login`")
	}

	body, err := encodeRequest(req)
	if err != nil {
		return nil, err
	}

	// Each attempt owns its request context. Only a successful stream hands the
	// cancel to its body (see streamBody); every other return releases it here.
	reqCtx, cancel := context.WithCancel(ctx)
	handedOff := false
	defer func() {
		if !handedOff {
			cancel()
		}
	}()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL+Path, bytes.NewReader(body))
	if err != nil {
		return nil, apierr.Wrap(err, apierr.TypeAPI, "codex responses: build request")
	}
	httpReq.Header.Set(headerAuthorization, "Bearer "+cred.AccessToken)
	httpReq.Header.Set(wire.HeaderAccountID, cred.AccountID)
	httpReq.Header.Set(wire.HeaderOpenAIBeta, wire.OpenAIBetaValue)
	httpReq.Header.Set(wire.HeaderOriginator, wire.Originator)
	httpReq.Header.Set(headerContentType, contentTypeJSON)
	httpReq.Header.Set(headerAccept, acceptSSE)
	if sid := sessionID(req.PromptCacheKey); sid != "" {
		httpReq.Header.Set(headerSessionID, sid)
	}

	// Which TLS stack this request is about to go out on. It is read HERE rather
	// than captured by the server middleware at the top of the request: the
	// middleware only ever sees the Anthropic leg's transport, which is a
	// compile-time "std", while the Codex leg's is the only one that can flip.
	// Reading it immediately before Do also names the stack that actually
	// carried THIS request, so the request that trips a gate is logged as std
	// and its successor as utls.
	obs.SummaryFrom(ctx).SetTransport(c.tr.Kind())

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A refused TLS handshake is what a fingerprint gate looks like when
		// the peer never answers at all, so this path reports gates too.
		ue := classifyTransport(ctx, err)
		c.noteGate(ue)
		return nil, ue
	}

	rl := ParseRateLimits(resp.Header, c.now())
	c.observe(cred, rl)

	// The status the BACKEND gave, which is not always the status utraque
	// answers with: an upstream 200 whose body carries no events becomes a 502
	// downstream, and a 401 can lead to a same-account token retry. Recording
	// it here — the only layer that sees it — is what makes that difference
	// legible on the one request line.
	obs.SummaryFrom(ctx).SetUpstreamStatus(resp.StatusCode)

	// A 200 whose body is not an event stream is not a stream at all: it is a
	// Cloudflare interstitial, or a JSON error the backend served with the wrong
	// status. Handing either to the SSE translator would lose the upstream's own
	// diagnostic and surface as a bare "no events" 502, so fall through to
	// classification, which recognises the challenge markers AND parses the
	// structured error body.
	if resp.StatusCode == http.StatusOK && isStreamContentType(resp.Header.Get(headerContentType)) {
		handedOff = true
		return &Response{
			PublicDetail: attemptRedactor(cred, rejected...),
			Status:       resp.StatusCode,
			Header:       resp.Header.Clone(),
			RateLimits:   rl,
			Body:         &streamBody{rc: resp.Body, cancel: cancel, proto: resp.ProtoMajor},
		}, nil
	}

	raw := readBounded(resp.Body, c.maxErrorBody)
	_ = resp.Body.Close()

	ue := classifyResponse(resp.StatusCode, resp.Header, rl, raw, cred, rejected...)
	c.logFailure(ue, cred)
	c.noteGate(ue)
	return nil, ue
}

// noteGate tells the transport that the upstream answered a bot/TLS challenge
// rather than the API. Only a gate is reported: an auth failure, a rate limit
// or a 5xx says nothing about our TLS fingerprint, and switching stacks on one
// of those would trade a clear error for a confusing one.
//
// What the transport does with the report is its business — std and utls do
// nothing, auto switches to uTLS exactly once.
func (c *Client) noteGate(ue *UpstreamError) {
	if !ue.IsGate() || c.tr == nil {
		return
	}
	transport.ReportGate(c.tr)
}

// StreamWithRefresh acquires the initial snapshot for callers that have not
// already resolved one. It uses StreamPinned's same-account retry policy.
func (c *Client) StreamWithRefresh(ctx context.Context, src auth.CredentialSource, req *cschema.ResponsesRequest) (io.ReadCloser, error) {
	resp, err := c.StreamResponseWithRefresh(ctx, src, req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// StreamResponseWithRefresh is StreamWithRefresh with the response metadata.
func (c *Client) StreamResponseWithRefresh(ctx context.Context, src auth.CredentialSource, req *cschema.ResponsesRequest) (*Response, error) {
	if src == nil {
		return nil, apierr.API("codex responses: no credential source configured")
	}
	cred, err := src.Get(ctx)
	if err != nil {
		return nil, err
	}
	return c.StreamPinned(ctx, cred, src, req)
}

// StreamPinned opens the first attempt without re-reading the credential source.
func (c *Client) StreamPinned(ctx context.Context, cred auth.Credential, src auth.CredentialSource, req *cschema.ResponsesRequest) (*Response, error) {
	resp, err := c.StreamResponse(ctx, cred, req)
	if err == nil {
		return resp, nil
	}

	ue, ok := AsUpstream(err)
	if !ok || !ue.NeedsCredentialRefresh() || src == nil {
		return nil, err
	}

	// The token was rejected: mark it dead so Get must refresh, then try once.
	src.Invalidate(cred)
	fresh, gerr := src.Get(ctx)
	if gerr != nil {
		// File-backed acquisition can verify the local account even when its
		// OAuth refresh fails. Do not let a retryable refresh failure replay an
		// old prompt when we already know that the login changed.
		if scope := fresh.Scope(); scope != "" && scope != cred.Scope() {
			c.log.Info("codex login changed during refresh; not retrying",
				obs.HashAttr("original_account_scope", cred.Scope()), obs.HashAttr("current_account_scope", scope))
			return nil, apierr.InvalidRequest(loginChangedMessage)
		}
		return nil, gerr
	}
	// A rotated token can authorize the same account; a different login cannot
	// authorize replay of an already accepted prompt. Return a non-retryable
	// 400 so sending under the new login requires a new client request.
	if fresh.AccountID != cred.AccountID {
		c.log.Info("codex login changed during request; not retrying",
			obs.HashAttr("original_account", cred.AccountID), obs.HashAttr("current_account", fresh.AccountID))
		return nil, apierr.InvalidRequest(loginChangedMessage)
	}
	if fresh.AccessToken == cred.AccessToken {
		// Nothing changed, so the same call would fail the same way. Report the
		// original rejection rather than spending a second upstream request.
		c.log.Warn("codex rejected the credential and no refreshed token was available; not retrying")
		return nil, err
	}
	c.log.Info("codex rejected the credential; retrying once with a refreshed token",
		obs.HashAttr("account", fresh.AccountID))
	return c.streamAttempt(ctx, fresh, req, cred.AccessToken)
}

// observe hands the forwarded quota headers to the caller's hook, if any.
func (c *Client) observe(cred auth.Credential, rl RateLimits) {
	if c.onRateLimits == nil || rl.Empty() {
		return
	}
	c.onRateLimits(cred.Scope(), rl)
}

// logFailure records a classified failure. It logs the class, the upstream
// status, and the quota state — never the credential, and never the raw body.
func (c *Client) logFailure(ue *UpstreamError, cred auth.Credential) {
	attrs := []any{
		slog.String("class", string(ue.Class)),
		slog.Int("upstream_status", ue.Status),
		slog.Int("status", ue.HTTPStatus()),
		slog.String("transport", c.tr.Kind()),
		obs.HashAttr("account", cred.AccountID),
	}
	if !ue.RateLimits.Empty() {
		attrs = append(attrs, slog.Any("rate_limits", ue.RateLimits))
	}
	if ue.Class == ClassGate {
		c.log.Error("codex responses request was blocked by a bot/TLS gate", attrs...)
		return
	}
	c.log.Warn("codex responses request failed", attrs...)
}

// encodeRequest marshals req with stream forced on, without mutating the
// caller's struct.
func encodeRequest(req *cschema.ResponsesRequest) ([]byte, error) {
	out := *req // shallow copy: only the Stream flag differs
	out.Stream = true
	body, err := json.Marshal(&out)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.TypeAPI, "codex responses: encode request")
	}
	return body, nil
}

// readBounded reads at most max bytes, discarding a read error: a truncated or
// unreadable error body must not mask the status that caused it.
func readBounded(r io.Reader, max int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, max))
	return b
}

// streamBody is the upstream SSE body. Close is idempotent, and it is safe to
// call while another goroutine is blocked in Read: the stream translator does
// exactly that when it finishes a stream early, then waits for its reader. Close
// always cancels this request's own context first, which ends the request
// without touching the caller's context, and never waits for more upstream
// bytes. How it then reaches the transport body depends on the protocol.
//
// HTTP/1: Close never touches the net/http body while a Read on it is in
// flight. The cancel makes the transport abandon the connection, which ends the
// Read; Close waits for that Read to return and only then closes the body. A raw
// HTTP/1 response body does not survive a concurrent Close on Go 1.27.0/1.27.1:
// a body closed during a pending Read that then reaches end-of-body strands the
// Read until the pooled connection idles out (90s), because both wait on one
// per-connection handshake that releases only one of them. This relies on the
// transport ending a body Read when its request context is cancelled, as
// net/http does; a RoundTripper that ignored the context would make Close wait
// for more upstream bytes. The trade-off is that an HTTP/1 connection closed
// before end-of-body is not reused, as on Go 1.26 and earlier; a body read to
// EOF first still is.
//
// HTTP/2: Close closes the body straight away, even during a Read. Both HTTP/2
// clients in use (net/http's and x/net/http2 behind uTLS) support that: closing
// breaks the stream's read pipe, which ends the Read at once. Waiting for the
// Read instead would be unsafe, because on cancellation both write RST_STREAM
// before they break the pipe, so a peer that has stopped reading would hold the
// Read, and Close with it, until that write went through.
type streamBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	proto  int // resp.ProtoMajor; anything below 2 is joined as HTTP/1

	mu     sync.Mutex // HTTP/1 only: held for the whole of each rc.Read, and around rc.Close
	closed atomic.Bool

	once sync.Once
	err  error
}

// joinsRead reports whether Close must wait out an in-flight Read before it
// closes the transport body (HTTP/1), rather than closing it directly (HTTP/2).
func (b *streamBody) joinsRead() bool { return b.proto < 2 }

func (b *streamBody) Read(p []byte) (int, error) {
	if b.joinsRead() {
		b.mu.Lock()
		defer b.mu.Unlock()
	}
	if b.closed.Load() {
		return 0, http.ErrBodyReadAfterClose
	}
	return b.rc.Read(p)
}

func (b *streamBody) Close() error {
	b.once.Do(func() {
		// cancel must precede b.mu.Lock: on HTTP/1 it is what ends a Read
		// holding b.mu. On HTTP/2 it also lets the transport body's Close
		// return without waiting for the stream's own cleanup.
		b.cancel()
		if b.joinsRead() {
			b.mu.Lock()
			defer b.mu.Unlock()
		}
		b.closed.Store(true)
		b.err = b.rc.Close()
	})
	return b.err
}

// sessionID renders a conversation's prompt cache key as the UUID-shaped
// session_id header the Codex CLI sends. The backend is undocumented and may
// key cache affinity on the header rather than (or as well as) the body's
// prompt_cache_key, so the two name the SAME conversation: one hash, two
// spellings, and a request can never claim one identity in the header and a
// different one in the body.
//
// An empty key yields an empty id and the header is omitted, which is what the
// leg did before the key existed.
func sessionID(promptCacheKey string) string {
	digest, found := strings.CutPrefix(promptCacheKey, promptCacheKeyPrefix)
	if !found || len(digest) != 32 {
		return ""
	}
	// Only a key utraque minted can name a conversation, and only hex renders as
	// a UUID. Anything else yields no id and the header is simply omitted, which
	// is what the leg did before the key existed.
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return digest[0:8] + "-" + digest[8:12] + "-" + digest[12:16] + "-" + digest[16:20] + "-" + digest[20:32]
}
