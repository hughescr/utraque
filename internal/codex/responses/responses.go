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
// StreamWithRefresh invalidates the credential and retries EXACTLY once on an
// upstream 401, because only this package can tell a 401 from any other 4xx.
//
// A bot/TLS gate (a Cloudflare challenge rather than an API answer) is detected
// and typed as ClassGate with an actionable message. Switching to a browser TLS
// fingerprint is phase 8; this package only names the problem.
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
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/transport"
)

// Wire constants. The header set is exactly what the Codex CLI sends; the
// originator stays honestly `codex_cli_rs` rather than impersonating a browser.
const (
	// DefaultBaseURL is the Codex backend root. Override in tests to point at
	// an httptest server; the real host is never contacted by the test suite.
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
	// Path is the responses endpoint under the base URL.
	Path = "/responses"

	headerAuthorization = "Authorization"
	headerAccountID     = "chatgpt-account-id"
	headerOpenAIBeta    = "OpenAI-Beta"
	headerOriginator    = "originator"
	headerContentType   = "Content-Type"
	headerAccept        = "Accept"

	openAIBetaValue = "responses=experimental"
	originatorValue = "codex_cli_rs"
	acceptSSE       = "text/event-stream"
	contentTypeJSON = "application/json"

	// defaultMaxErrorBody bounds how much of a failing response we read. An
	// error body is small; a challenge page is not, and neither may be
	// unbounded.
	defaultMaxErrorBody = 64 << 10
	// maxErrorMessage bounds (in runes) how much upstream text reaches an error
	// envelope or a log line.
	maxErrorMessage = 512
)

// Options configures a Client. Every field is optional.
type Options struct {
	// BaseURL is the Codex backend root. Defaults to DefaultBaseURL.
	BaseURL string
	// Transport supplies the HTTP client. Defaults to the standard no-redirect
	// transport. A redirect is never followed: it would replay the caller's
	// bearer token at whatever host the upstream named.
	Transport transport.Transport
	// MaxErrorBody bounds the bytes read from a failing response. Defaults to
	// defaultMaxErrorBody.
	MaxErrorBody int64
	// OnRateLimits, when set, is called with the forwarded quota headers of
	// every upstream response — success or failure — so /healthz and the
	// request log can report the real usage window. It must not block.
	OnRateLimits func(RateLimits)
	// Now supplies the current time, used to resolve an HTTP-date Retry-After.
	// Defaults to time.Now.
	Now func() time.Time
	// Logger receives redacted operational logs. Defaults to slog.Default.
	Logger *slog.Logger
}

// Client posts to the Codex responses endpoint. It is safe for concurrent use.
type Client struct {
	baseURL      string
	http         *http.Client
	transportKnd string
	maxErrorBody int64
	onRateLimits func(RateLimits)
	now          func() time.Time
	log          *slog.Logger
}

// Streamer is the surface the Codex leg depends on. Stream opens the upstream
// SSE stream for an already-resolved credential; StreamWithRefresh resolves the
// credential itself and drives the single 401 retry.
type Streamer interface {
	Stream(ctx context.Context, cred auth.Credential, req *schema.ResponsesRequest) (io.ReadCloser, error)
	StreamWithRefresh(ctx context.Context, src auth.CredentialSource, req *schema.ResponsesRequest) (io.ReadCloser, error)
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
		c.baseURL = DefaultBaseURL
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
	c.http = tr.Client()
	c.transportKnd = tr.Kind()
	return c
}

// Response is an opened upstream stream plus the metadata that came with its
// headers. Body is the raw SSE stream; the caller owns it and MUST Close it.
type Response struct {
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
func (c *Client) Stream(ctx context.Context, cred auth.Credential, req *schema.ResponsesRequest) (io.ReadCloser, error) {
	resp, err := c.StreamResponse(ctx, cred, req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// StreamResponse is Stream plus the response metadata (status, headers, and the
// forwarded rate-limit headers) the leg reports on /healthz.
func (c *Client) StreamResponse(ctx context.Context, cred auth.Credential, req *schema.ResponsesRequest) (*Response, error) {
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+Path, bytes.NewReader(body))
	if err != nil {
		return nil, apierr.Wrap(err, apierr.TypeAPI, "codex responses: build request")
	}
	httpReq.Header.Set(headerAuthorization, "Bearer "+cred.AccessToken)
	httpReq.Header.Set(headerAccountID, cred.AccountID)
	httpReq.Header.Set(headerOpenAIBeta, openAIBetaValue)
	httpReq.Header.Set(headerOriginator, originatorValue)
	httpReq.Header.Set(headerContentType, contentTypeJSON)
	httpReq.Header.Set(headerAccept, acceptSSE)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, classifyTransport(ctx, err)
	}

	rl := ParseRateLimits(resp.Header, c.now())
	c.observe(rl)

	// A 200 whose body is not an event stream is not a stream at all: it is a
	// Cloudflare interstitial, or a JSON error the backend served with the wrong
	// status. Handing either to the SSE translator would lose the upstream's own
	// diagnostic and surface as a bare "no events" 502, so fall through to
	// classification, which recognises the challenge markers AND parses the
	// structured error body.
	if resp.StatusCode == http.StatusOK && isStreamContentType(resp.Header.Get(headerContentType)) {
		return &Response{
			Status:     resp.StatusCode,
			Header:     resp.Header.Clone(),
			RateLimits: rl,
			Body:       &streamBody{rc: resp.Body},
		}, nil
	}

	raw := readBounded(resp.Body, c.maxErrorBody)
	_ = resp.Body.Close()

	ue := classifyResponse(resp.StatusCode, resp.Header, rl, raw)
	c.logFailure(ue, cred)
	return nil, ue
}

// StreamWithRefresh resolves a credential from src, opens the stream, and — on
// an upstream 401 and only then — invalidates that credential and retries
// EXACTLY once with a freshly refreshed one. This is the one retry the plan
// sanctions, and it lives here because only this package can tell a 401 from
// any other failure.
func (c *Client) StreamWithRefresh(ctx context.Context, src auth.CredentialSource, req *schema.ResponsesRequest) (io.ReadCloser, error) {
	resp, err := c.StreamResponseWithRefresh(ctx, src, req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// StreamResponseWithRefresh is StreamWithRefresh with the response metadata.
func (c *Client) StreamResponseWithRefresh(ctx context.Context, src auth.CredentialSource, req *schema.ResponsesRequest) (*Response, error) {
	if src == nil {
		return nil, apierr.API("codex responses: no credential source configured")
	}
	cred, err := src.Get(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.StreamResponse(ctx, cred, req)
	if err == nil {
		return resp, nil
	}

	ue, ok := AsUpstream(err)
	if !ok || !ue.NeedsCredentialRefresh() {
		return nil, err
	}

	// The token was rejected: mark it dead so Get must refresh, then try once.
	src.Invalidate(cred)
	fresh, gerr := src.Get(ctx)
	if gerr != nil {
		// A refresh failure is more actionable than the 401 that provoked it.
		return nil, gerr
	}
	// The request is signed by BOTH the token and the account id, so either one
	// changing makes the retry a different call. Comparing only the token would
	// decline a retry that a corrected or rotated account id would have fixed.
	if fresh.AccessToken == cred.AccessToken && fresh.AccountID == cred.AccountID {
		// Nothing changed, so the same call would fail the same way. Report the
		// original rejection rather than spending a second upstream request.
		c.log.Warn("codex rejected the credential and no refreshed token was available; not retrying")
		return nil, err
	}
	c.log.Info("codex rejected the credential; retrying once with a refreshed token",
		obs.HashAttr("account", fresh.AccountID))
	return c.StreamResponse(ctx, fresh, req)
}

// observe hands the forwarded quota headers to the caller's hook, if any.
func (c *Client) observe(rl RateLimits) {
	if c.onRateLimits == nil || rl.Empty() {
		return
	}
	c.onRateLimits(rl)
}

// logFailure records a classified failure. It logs the class, the upstream
// status, and the quota state — never the credential, and never the raw body.
func (c *Client) logFailure(ue *UpstreamError, cred auth.Credential) {
	attrs := []any{
		slog.String("class", string(ue.Class)),
		slog.Int("upstream_status", ue.Status),
		slog.Int("status", ue.HTTPStatus()),
		slog.String("transport", c.transportKnd),
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
func encodeRequest(req *schema.ResponsesRequest) ([]byte, error) {
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

// streamBody makes Close idempotent so a leg that closes on both the happy path
// and a deferred cleanup cannot double-close the upstream connection.
type streamBody struct {
	rc   io.ReadCloser
	once sync.Once
	err  error
}

func (b *streamBody) Read(p []byte) (int, error) { return b.rc.Read(p) }

func (b *streamBody) Close() error {
	b.once.Do(func() { b.err = b.rc.Close() })
	return b.err
}
