// Package anthropic is the byte-faithful passthrough leg. It relays the
// caller's request to api.anthropic.com without holding an Anthropic
// credential of its own, so the caller's own subscription pays for the call.
package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/transport"
)

// DefaultMaxBodyBytes bounds a body this leg reads itself (the catch-all
// path). When the router has already read the body into Request.Raw the limit
// has been applied upstream by the server middleware.
const DefaultMaxBodyBytes int64 = 64 << 20

const copyBufferSize = 32 << 10

// ErrResponseStarted wraps any failure that happens after the status line and
// headers have gone out. A caller must not try to render an error envelope on
// top of it — the bytes are already committed.
//
// It is an alias for router.ErrResponseStarted, not a distinct sentinel: both
// legs fail this way, so the dispatcher tests one value and errors.Is matches
// whichever name the caller reaches for.
var ErrResponseStarted = router.ErrResponseStarted

// ErrClientGone wraps a failure caused by the caller disconnecting. Like
// ErrResponseStarted it is the shared router sentinel under a local name.
var ErrClientGone = router.ErrClientGone

// hopByHop is the RFC 7230 §6.1 connection-scoped header set. These describe
// the caller-to-proxy connection and must never be relayed to upstream (nor
// relayed back down from it).
var hopByHop = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {}, // non-standard, but HTTP/1 clients still emit it
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// utraque's own loopback headers. They describe the caller-to-proxy hop and
// must never cross the trust boundary:
//
//   - localTokenHeader is the shared secret that authorizes a local process to
//     spend the user's subscriptions. Forwarding it would disclose utraque's
//     own credential to a third party on every single request.
//   - responseIDHeader is minted by the proxy; letting an upstream response
//     overwrite it would make the client-visible id disagree with the logs.
//
// The names are duplicated from internal/server rather than imported to keep
// this leg free of a dependency on the server package; passthrough_test asserts
// the two stay in step.
const (
	localTokenHeader = "X-Utraque-Token"
	responseIDHeader = "X-Utraque-Request-Id"
)

// IsHopByHop reports whether name is a connection-scoped header. name is
// canonicalized before the lookup.
func IsHopByHop(name string) bool {
	_, ok := hopByHop[http.CanonicalHeaderKey(name)]
	return ok
}

// Option configures a Leg.
type Option func(*Leg)

// WithLogger sets the leg's logger. The default discards.
func WithLogger(log *slog.Logger) Option {
	return func(l *Leg) {
		if log != nil {
			l.log = log
		}
	}
}

// WithMaxBodyBytes bounds bodies this leg reads itself. Zero or negative
// restores DefaultMaxBodyBytes.
func WithMaxBodyBytes(n int64) Option {
	return func(l *Leg) {
		if n <= 0 {
			n = DefaultMaxBodyBytes
		}
		l.maxBody = n
	}
}

// WithSanitizer enables or disables the synthetic-thinking sanitizer. It is on
// by default. Off means every body is forwarded byte-for-byte, unconditionally.
func WithSanitizer(on bool) Option {
	return func(l *Leg) { l.sanitize = on }
}

// WithUpstreamIdleTimeout bounds how long the leg waits on a silent upstream —
// both before the response headers arrive and, rolling, between body reads
// once they have. Zero or negative disables it.
//
// This is deliberately an *idle* bound and not an overall deadline: an SSE
// stream may legitimately run for many minutes, but a stream that has produced
// nothing for the whole period is hung, and without this the request, its
// goroutine, its connection and its idle-timer hold live forever.
func WithUpstreamIdleTimeout(d time.Duration) Option {
	return func(l *Leg) {
		if d < 0 {
			d = 0
		}
		l.upstreamIdle = d
	}
}

// Leg is the byte-faithful Anthropic passthrough. It forwards the caller's
// method, path, query, body, and headers to the configured base URL, holding
// no Anthropic credential of its own: the caller's Authorization bearer token
// and anthropic-beta values ride through untouched, which is what keeps the
// request billed to the caller's own subscription.
//
// Leg satisfies router.Leg and http.Handler; the http.Handler form is the
// catch-all for every unrecognised path under the base URL.
type Leg struct {
	base         *url.URL
	tr           transport.Transport
	log          *slog.Logger
	maxBody      int64
	sanitize     bool
	upstreamIdle time.Duration
}

var (
	_ router.Leg   = (*Leg)(nil)
	_ http.Handler = (*Leg)(nil)
)

// New builds a passthrough leg targeting baseURL (e.g.
// "https://api.anthropic.com"). Any userinfo, query, or fragment on baseURL is
// dropped — a credential must never ride in a configured URL — and a trailing
// slash is trimmed so path joining stays exact.
func New(baseURL string, tr transport.Transport, opts ...Option) (*Leg, error) {
	if tr == nil {
		return nil, errors.New("utraque/anthropic: nil transport")
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("utraque/anthropic: parse base url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("utraque/anthropic: base url must be http or https, got %q", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("utraque/anthropic: base url has no host: %q", baseURL)
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""

	l := &Leg{
		base:     u,
		tr:       tr,
		log:      slog.New(slog.DiscardHandler),
		maxBody:  DefaultMaxBodyBytes,
		sanitize: true,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// BaseURL returns the normalized upstream base URL.
func (l *Leg) BaseURL() string { return l.base.String() }

// Messages forwards POST /v1/messages.
func (l *Leg) Messages(w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	return l.forward(w, r, rq)
}

// CountTokens forwards POST /v1/messages/count_tokens. It sanitizes too: a
// count_tokens body replays the same conversation history, synthetic thinking
// blocks included.
func (l *Leg) CountTokens(w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	return l.forward(w, r, rq)
}

// ServeHTTP is the catch-all. Claude Code reaches for assorted endpoints under
// the base URL and none of them may 404 locally, so anything unrecognised is
// relayed upstream unchanged.
func (l *Leg) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := l.forward(w, r, nil); err != nil {
		if errors.Is(err, ErrResponseStarted) || errors.Is(err, ErrClientGone) {
			l.log.WarnContext(r.Context(), "anthropic passthrough failed after response started",
				"path", r.URL.Path, "err", err)
			return
		}
		l.log.WarnContext(r.Context(), "anthropic passthrough failed",
			"path", r.URL.Path, "err", err)
		_ = apierr.Write(w, err)
	}
}

func (l *Leg) forward(w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	body, err := l.requestBody(r, rq)
	if err != nil {
		return err
	}

	// The one deliberate exception to byte-purity. A single bytes.Contains over
	// the raw body costs nanoseconds and only ever trips on a mixed-model
	// replay, where a thinking block we minted for a GPT turn would otherwise
	// be sent to Anthropic and rejected for a bogus signature.
	if l.sanitize && HasSyntheticThinking(body) {
		clean, rep, serr := SanitizeWithReport(body)
		switch {
		case serr != nil:
			// Fail open: an unparseable body is still the caller's business.
			l.log.WarnContext(r.Context(), "synthetic-thinking sanitizer skipped",
				"path", r.URL.Path, "err", serr)
		case rep.Changed:
			l.log.DebugContext(r.Context(), "stripped synthetic thinking blocks",
				"path", r.URL.Path, "dropped", rep.Dropped,
				"bytes_before", len(body), "bytes_after", len(clean))
			if rep.HeadlessToolUse {
				// See SanitizeMessages: unrepairable here, and the likely cause
				// of an otherwise baffling 400 from Anthropic.
				l.log.WarnContext(r.Context(),
					"an assistant turn kept its tool_use after losing a synthetic thinking block; "+
						"Anthropic may reject this request if extended thinking is enabled",
					"path", r.URL.Path)
			}
			body = clean
		}
	}

	// idle bounds a silent upstream. It cancels only this leg's derived
	// context, so the caller's own cancellation stays distinguishable from a
	// timeout when the error is classified below.
	ctx, idle := l.withIdleDeadline(r.Context())
	defer idle.stop()

	req, err := http.NewRequestWithContext(ctx, r.Method, l.upstreamURL(r.URL), bytes.NewReader(body))
	if err != nil {
		return apierr.Wrap(err, apierr.TypeInvalidRequest, "build upstream request")
	}
	copyRequestHeaders(req.Header, r.Header)
	// A caller that sent no User-Agent must reach upstream with none, not with
	// net/http's default "Go-http-client/..." — that is a header we invented.
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = nil
	}
	// Leave req.Host empty so the Host header derives from the upstream URL.
	// Relaying the caller's Host (127.0.0.1:8317) would fail TLS/SNI routing.
	req.Host = ""
	req.ContentLength = int64(len(body))
	if len(body) == 0 {
		req.Body = http.NoBody
	}

	resp, err := l.tr.Client().Do(req)
	if err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", ErrClientGone, ctxErr)
		}
		return l.upstreamError(err, idle.fired())
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	rc := http.NewResponseController(w)
	// Push the status line and headers out before the first body byte so an SSE
	// client can start reading immediately.
	flush(rc)

	buf := make([]byte, copyBufferSize)
	if _, err := io.CopyBuffer(&flushWriter{w: w, rc: rc}, idle.wrap(resp.Body), buf); err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w: %w", ErrResponseStarted, ErrClientGone, ctxErr)
		}
		if idle.fired() {
			return fmt.Errorf("%w: upstream stream went silent for %s: %w",
				ErrResponseStarted, l.upstreamIdle, err)
		}
		return fmt.Errorf("%w: streaming upstream response: %w", ErrResponseStarted, err)
	}
	return nil
}

// upstreamError classifies a failed round trip. A timeout is a 504
// timeout_error rather than the blanket 502: the client's retry policy for
// "upstream is slow" differs from "upstream is broken".
func (l *Leg) upstreamError(err error, idleFired bool) error {
	if idleFired {
		e := apierr.Wrap(scrubURLError(err), apierr.TypeTimeout,
			"upstream sent nothing for %s", l.upstreamIdle)
		return e
	}
	var ne net.Error
	if (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return apierr.Wrap(scrubURLError(err), apierr.TypeTimeout, "upstream request timed out")
	}
	e := apierr.Wrap(scrubURLError(err), apierr.TypeAPI, "upstream request failed")
	e.Status = http.StatusBadGateway
	return e
}

// scrubURLError strips the query string (and any userinfo) from the URL
// net/http stamps into a *url.Error. That URL is the caller's own, verbatim,
// and reaches the log through the error's cause; a query-string credential
// must not end up there. Go redacts only the userinfo password.
func scrubURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safe := ue.URL
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.User = nil
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		u.RawFragment = ""
		safe = u.String()
	} else {
		safe = "invalid-url"
	}
	return &url.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}

// idleGuard arms the rolling upstream-idle deadline for one round trip. The
// zero value (no timeout configured) is inert: stop, fired and wrap all become
// no-ops, so forward needs no branches.
type idleGuard struct {
	timeout time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	tripped atomic.Bool
}

func (l *Leg) withIdleDeadline(parent context.Context) (context.Context, *idleGuard) {
	g := &idleGuard{timeout: l.upstreamIdle}
	if l.upstreamIdle <= 0 {
		return parent, g
	}
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	g.timer = time.AfterFunc(l.upstreamIdle, func() {
		g.tripped.Store(true)
		cancel()
	})
	return ctx, g
}

func (g *idleGuard) stop() {
	if g.timer != nil {
		g.timer.Stop()
	}
	if g.cancel != nil {
		g.cancel()
	}
}

func (g *idleGuard) fired() bool { return g.tripped.Load() }

// wrap restarts the countdown on every byte read from the upstream body, which
// is what makes the bound an idle one rather than a total one.
func (g *idleGuard) wrap(r io.Reader) io.Reader {
	if g.timer == nil {
		return r
	}
	return &idleResetReader{r: r, g: g}
}

type idleResetReader struct {
	r io.Reader
	g *idleGuard
}

func (ir *idleResetReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 && !ir.g.tripped.Load() {
		ir.g.timer.Reset(ir.g.timeout)
	}
	return n, err
}

func (l *Leg) requestBody(r *http.Request, rq *router.Request) ([]byte, error) {
	if rq != nil && rq.Raw != nil {
		return rq.Raw, nil
	}
	if r.Body == nil {
		return nil, nil
	}
	limit := l.maxBody
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	b, err := io.ReadAll(&io.LimitedReader{R: r.Body, N: limit + 1})
	if err != nil {
		// An over-limit chunked body surfaces here as *http.MaxBytesError from
		// the server's own MaxBytesReader. It is a 413, exactly as the routed
		// path reports it — not a generic 400.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, apierr.RequestTooLarge("request body exceeds the %d byte limit", mbe.Limit)
		}
		return nil, apierr.Wrap(err, apierr.TypeInvalidRequest, "read request body")
	}
	if int64(len(b)) > limit {
		return nil, apierr.RequestTooLarge("request body exceeds %d bytes", limit)
	}
	return b, nil
}

// upstreamURL joins the caller's escaped path and raw query onto the base URL
// without ever re-encoding them: the string is built by concatenation and
// re-parsed, so percent-escapes survive exactly as the caller wrote them.
func (l *Leg) upstreamURL(in *url.URL) string {
	path := in.EscapedPath()
	if path == "" {
		path = "/"
	}
	var b strings.Builder
	b.WriteString(l.base.String())
	b.WriteString(path)
	if in.RawQuery != "" || in.ForceQuery {
		b.WriteByte('?')
		b.WriteString(in.RawQuery)
	}
	return b.String()
}

// copyRequestHeaders relays every caller header except the connection-scoped
// set, the headers named in the caller's own Connection token list, and
// Content-Length (the sanitizer may have changed the length; the outbound
// length comes from Request.ContentLength).
//
// Values are copied as a slice, never joined. That is what preserves repeated
// anthropic-beta headers as distinct values — joining them into one
// comma-separated line changes the OAuth capability signal upstream reads.
func copyRequestHeaders(dst, src http.Header) {
	drop := connectionTokens(src)
	for k, vv := range src {
		ck := http.CanonicalHeaderKey(k)
		if IsHopByHop(ck) || ck == "Content-Length" || ck == localTokenHeader {
			continue
		}
		if _, ok := drop[ck]; ok {
			continue
		}
		dst[ck] = slices.Clone(vv)
	}
}

// copyResponseHeaders relays upstream response headers minus the
// connection-scoped set. Content-Length is kept: the body is copied verbatim,
// so upstream's length still describes it.
func copyResponseHeaders(dst, src http.Header) {
	drop := connectionTokens(src)
	for k, vv := range src {
		ck := http.CanonicalHeaderKey(k)
		if IsHopByHop(ck) || ck == responseIDHeader {
			continue
		}
		if _, ok := drop[ck]; ok {
			continue
		}
		dst[ck] = slices.Clone(vv)
	}
}

// connectionTokens returns the canonicalized header names listed in Connection,
// which RFC 7230 also makes hop-by-hop for this message.
func connectionTokens(h http.Header) map[string]struct{} {
	out := map[string]struct{}{}
	for _, line := range h.Values("Connection") {
		for _, tok := range strings.Split(line, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			out[http.CanonicalHeaderKey(tok)] = struct{}{}
		}
	}
	return out
}

// flushWriter flushes after every write so SSE frames reach the caller as they
// arrive rather than at the 2 KiB buffer boundary or handler return. It
// deliberately does not implement io.ReaderFrom, which would let io.Copy hand
// the body straight to the ResponseWriter and skip the flushes.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err != nil {
		return n, err
	}
	// A real flush failure means the downstream connection is gone. Reporting
	// it stops the copy loop; swallowing it would keep the proxy draining an
	// upstream stream — and holding the idle timer open — for nobody.
	if ferr := fw.rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
		return n, ferr
	}
	return n, nil
}

// flush best-effort flushes; a ResponseWriter that cannot flush is not an error.
// Used only for the headers, before any body byte exists to lose.
func flush(rc *http.ResponseController) {
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		_ = err
	}
}
