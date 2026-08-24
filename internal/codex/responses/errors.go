package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/hughescr/utraque/internal/apierr"
)

// Class is the coarse kind of an upstream failure. It is what the Codex leg
// switches on: whether to refresh the credential and retry once, whether a
// retry could ever help, and whether the failure is really a bot gate rather
// than an API answer.
type Class string

// The failure classes. Every *UpstreamError carries exactly one.
const (
	// ClassAuth is an upstream 401: the access token was rejected. The leg
	// should auth.Invalidate the credential and retry EXACTLY once.
	ClassAuth Class = "auth"
	// ClassRateLimit is an upstream 429: plan rate limit or quota exhausted.
	// Retry-After and the usage-window headers are carried on RateLimits.
	ClassRateLimit Class = "rate_limit"
	// ClassUpstream is an upstream 5xx: transient on the backend's side.
	ClassUpstream Class = "upstream"
	// ClassTerminal is a 4xx (other than 401/429) or an otherwise unusable
	// response: the request itself will never succeed as sent.
	ClassTerminal Class = "terminal"
	// ClassGate is a bot/TLS challenge (Cloudflare) rather than an API answer.
	// Retrying with the same TLS fingerprint cannot help; the fix is the uTLS
	// transport, which codex.transport=auto switches to on this class.
	ClassGate Class = "gate"
	// ClassNetwork is a transport-level failure with no HTTP response at all.
	ClassNetwork Class = "network"
	// ClassTimeout is a deadline expiring before the response headers arrived.
	ClassTimeout Class = "timeout"
	// ClassCanceled is the caller's context being cancelled (typically the
	// downstream client hanging up).
	ClassCanceled Class = "canceled"
)

// canceledStatus is the status pinned on a caller-cancelled request. 499 is
// nginx's non-standard "client closed request"; nothing is usually written to a
// client that has already gone away, but the status keeps the log honest about
// why the request ended.
const canceledStatus = 499

// UpstreamError is every failure of a Codex /responses call, typed so the leg
// can act on it and renderable so the client sees a well-formed Anthropic error
// envelope.
//
// It wraps an *apierr.Error, so apierr.From(err) and
// errors.As(err, &(*apierr.Error)(nil)) both find the rendering form, and it
// wraps the transport cause, so errors.Is(err, context.Canceled) still works.
type UpstreamError struct {
	// Class is the coarse kind of failure.
	Class Class
	// Status is the upstream HTTP status, or 0 when no response was received.
	Status int
	// UpstreamMessage is the human-readable message parsed out of the upstream
	// error body, empty when there was none (or it was an HTML challenge page).
	UpstreamMessage string
	// UpstreamType and UpstreamCode are OpenAI's own error discriminators when
	// the body carried them.
	UpstreamType string
	UpstreamCode string
	// RateLimits are the forwarded quota headers, populated on any response.
	RateLimits RateLimits

	api   *apierr.Error
	cause error
}

var _ error = (*UpstreamError)(nil)

func (e *UpstreamError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString("codex responses: ")
	b.WriteString(string(e.Class))
	if e.Status > 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	if e.api != nil {
		b.WriteString(": ")
		b.WriteString(e.api.Message)
	}
	if e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

// Unwrap exposes both the rendering form and the transport cause, so
// errors.As finds the *apierr.Error and errors.Is finds context.Canceled.
func (e *UpstreamError) Unwrap() []error {
	if e == nil {
		return nil
	}
	out := make([]error, 0, 2)
	if e.api != nil {
		out = append(out, e.api)
	}
	if e.cause != nil {
		out = append(out, e.cause)
	}
	return out
}

// APIError is the canonical renderable form: an Anthropic error envelope plus
// the HTTP status utraque should answer with.
func (e *UpstreamError) APIError() *apierr.Error {
	if e == nil || e.api == nil {
		return apierr.API("codex responses: unknown failure")
	}
	return e.api
}

// HTTPStatus is the status utraque should answer the client with. It is NOT
// always Status: a bot gate or an unexpected redirect is reported as 502
// because it is a gateway failure, not an answer the client can act on.
func (e *UpstreamError) HTTPStatus() int { return e.APIError().HTTPStatus() }

// Retryable reports whether re-sending the identical request could plausibly
// succeed. Auth failures are excluded: they need NeedsCredentialRefresh, not a
// blind retry. This package never acts on it — retry policy belongs to the leg.
func (e *UpstreamError) Retryable() bool {
	if e == nil {
		return false
	}
	switch e.Class {
	case ClassRateLimit, ClassUpstream, ClassNetwork, ClassTimeout:
		return true
	default:
		return false
	}
}

// NeedsCredentialRefresh reports the one documented retry hook: an upstream 401
// means the leg should auth.Invalidate the credential and retry exactly once
// (see Client.StreamWithRefresh, which does precisely that).
func (e *UpstreamError) NeedsCredentialRefresh() bool {
	return e != nil && e.Class == ClassAuth
}

// IsGate reports whether the upstream answered with a bot/TLS challenge.
func (e *UpstreamError) IsGate() bool { return e != nil && e.Class == ClassGate }

// RetryAfterDelay reports the upstream's Retry-After, when it sent one.
func (e *UpstreamError) RetryAfterDelay() (time.Duration, bool) {
	if e == nil {
		return 0, false
	}
	return e.RateLimits.RetryAfter, e.RateLimits.HasRetryAfter
}

// ApplyHeaders copies the forwarded rate-limit headers (including Retry-After)
// onto an outgoing response, preserving 429 semantics end to end.
func (e *UpstreamError) ApplyHeaders(dst http.Header) {
	if e == nil {
		return
	}
	e.RateLimits.Apply(dst)
}

// AsUpstream extracts the *UpstreamError from an error chain.
func AsUpstream(err error) (*UpstreamError, bool) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// ClassOf reports the class of err, or "" if it is not an upstream failure.
func ClassOf(err error) Class {
	if ue, ok := AsUpstream(err); ok {
		return ue.Class
	}
	return ""
}

// classifyResponse maps a non-streamable HTTP response onto a typed error. raw
// is the (bounded) response body, already read.
func classifyResponse(status int, h http.Header, rl RateLimits, raw []byte) *UpstreamError {
	msg, typ, code := parseUpstreamMessage(h.Get("Content-Type"), raw)
	e := &UpstreamError{
		Status:          status,
		UpstreamMessage: msg,
		UpstreamType:    typ,
		UpstreamCode:    code,
		RateLimits:      rl,
	}
	switch {
	case isGateResponse(status, h, raw, msg != ""):
		e.Class = ClassGate
		e.UpstreamMessage = "" // a challenge page carries no useful API message
		e.api = apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI, "%s", gateMessage(status))

	case status == http.StatusUnauthorized:
		e.Class = ClassAuth
		e.api = apierr.WithStatus(http.StatusUnauthorized, apierr.TypeAuthentication, "%s",
			withDetail("codex rejected the credential (HTTP 401); the access token needs refreshing — run `codex login` if this persists", msg))

	case status == http.StatusTooManyRequests:
		e.Class = ClassRateLimit
		base := "codex rate limit or quota exceeded (HTTP 429)"
		if rl.HasRetryAfter {
			base = fmt.Sprintf("%s; retry after %s", base, rl.RetryAfter)
		}
		e.api = apierr.WithStatus(http.StatusTooManyRequests, apierr.TypeRateLimit, "%s", withDetail(base, msg))

	case status >= 500:
		e.Class = ClassUpstream
		e.api = apierr.WithStatus(status, apierr.TypeForStatus(status), "%s",
			withDetail(fmt.Sprintf("codex upstream error (HTTP %d)", status), msg))

	case status >= 400:
		e.Class = ClassTerminal
		kind := apierr.TypeForStatus(status)
		if kind == apierr.TypeAPI {
			// An unmapped 4xx is the caller's fault, not ours.
			kind = apierr.TypeInvalidRequest
		}
		e.api = apierr.WithStatus(status, kind, "%s",
			withDetail(fmt.Sprintf("codex rejected the request (HTTP %d)", status), msg))

	case status >= 300:
		// The transport never follows a redirect (it would replay the bearer
		// token at whatever host upstream named), so a 3xx is a dead end.
		e.Class = ClassTerminal
		e.api = apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI,
			"codex returned an unexpected redirect (HTTP %d to %q); redirects are never followed",
			status, h.Get("Location"))

	default:
		e.Class = ClassTerminal
		e.api = apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI, "%s",
			withDetail(fmt.Sprintf("codex returned an unexpected response (HTTP %d, content-type %q) where an %s stream was expected",
				status, h.Get("Content-Type"), acceptSSE), msg))
	}
	return e
}

// classifyTransport maps a failure with no HTTP response onto a typed error.
func classifyTransport(ctx context.Context, err error) *UpstreamError {
	e := &UpstreamError{cause: err}
	switch {
	case errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)):
		e.Class = ClassCanceled
		e.api = apierr.WithStatus(canceledStatus, apierr.TypeAPI,
			"codex responses request was cancelled before the stream opened")
	case errors.Is(err, context.DeadlineExceeded) || (ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)):
		e.Class = ClassTimeout
		e.api = apierr.WithStatus(http.StatusGatewayTimeout, apierr.TypeTimeout,
			"codex responses request timed out before the stream opened")
	case looksLikeTLSGate(err):
		e.Class = ClassGate
		e.api = apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI, "%s", tlsGateMessage(err))
	default:
		e.Class = ClassNetwork
		e.api = apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI,
			"codex responses request could not reach the upstream: %s", oneLine(err.Error(), maxErrorMessage))
	}
	return e
}

// gateMessage is the actionable text for an HTTP-level bot challenge. It says
// plainly that a retry cannot help and names the fix, because the alternative
// (a bare 403) reads as "your account lost access" and sends the user hunting
// in the wrong place.
func gateMessage(status int) string {
	return fmt.Sprintf("codex upstream answered with a bot/TLS challenge instead of the API (HTTP %d, Cloudflare): "+
		"the request never reached the Codex backend, so retrying it unchanged cannot help. "+
		"The account itself is probably fine — confirm with the Codex CLI. "+
		"The fix is to present a browser TLS fingerprint via the uTLS transport; "+
		"in the default codex.transport=auto mode this failure has already switched the process onto it, so the next request uses it "+
		"(force it from the start with UTRAQUE_CODEX_TRANSPORT=utls)", status)
}

func tlsGateMessage(err error) string {
	return fmt.Sprintf("codex upstream refused the TLS connection, which is how a fingerprint-based bot gate looks from the client side: %s. "+
		"Retrying unchanged cannot help; the fix is the uTLS (browser-fingerprint) transport, "+
		"which codex.transport=auto has already switched to for the rest of this process "+
		"(force it from the start with UTRAQUE_CODEX_TRANSPORT=utls)", oneLine(err.Error(), maxErrorMessage))
}

// tlsGateMarkers are handshake failures that indicate the peer refused us
// rather than a routine network fault.
var tlsGateMarkers = []string{
	"tls: handshake failure",
	"tls: unrecognized name",
	"tls: no application protocol",
	"tls: bad record mac",
}

func looksLikeTLSGate(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range tlsGateMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// challengeMarkers are strings that only appear in a Cloudflare interstitial,
// never in an OpenAI API error body.
var challengeMarkers = []string{
	"just a moment",
	"cf-browser-verification",
	"__cf_chl",
	"cf_chl_opt",
	"attention required! | cloudflare",
	"enable javascript and cookies to continue",
	"checking your browser before accessing",
	"sorry, you have been blocked",
}

// isGateResponse decides whether a response is a bot challenge rather than an
// API answer.
//
// A marker in an HTML body is conclusive. A bare 403 carrying a cf-ray is
// treated as a gate ONLY when the body held no structured API message: the real
// backend sits behind Cloudflare, so cf-ray is present on legitimate answers
// too, and a genuine "you may not do that" 403 with a JSON error body must stay
// a permission error.
func isGateResponse(status int, h http.Header, raw []byte, hasStructuredMsg bool) bool {
	if looksHTML(h, raw) && hasChallengeMarker(raw) {
		return true
	}
	if hasStructuredMsg {
		return false
	}
	if status == http.StatusForbidden && h.Get("cf-ray") != "" {
		return true
	}
	if (status == http.StatusForbidden || status == http.StatusServiceUnavailable) &&
		strings.EqualFold(strings.TrimSpace(h.Get("Server")), "cloudflare") && looksHTML(h, raw) {
		return true
	}
	return false
}

func hasChallengeMarker(raw []byte) bool {
	probe := raw
	if len(probe) > 8<<10 {
		probe = probe[:8<<10]
	}
	lower := strings.ToLower(string(probe))
	for _, m := range challengeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// looksHTML reports whether the body is an HTML document, by content type or by
// its first non-space byte.
func looksHTML(h http.Header, raw []byte) bool {
	if isHTMLContentType(h.Get("Content-Type")) {
		return true
	}
	return bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("<"))
}

func isHTMLContentType(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/html")
}

// isStreamContentType reports whether a 200 may be handed to the SSE translator.
//
// We asked for text/event-stream, so that is what an answer looks like. An
// ABSENT content type is also accepted: it is legal on a chunked response and
// some intermediaries drop it, and refusing one would turn a working stream into
// a 502. Anything else — application/json, text/html — is an error body or an
// interstitial wearing a 200, and belongs in classifyResponse where its message
// can be read.
func isStreamContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return true
	}
	return strings.HasPrefix(ct, acceptSSE)
}

// errorBody covers the shapes the Codex backend and its edge return: OpenAI's
// {"error":{...}}, FastAPI's {"detail":...} (string or object), and a bare
// {"message":...}.
type errorBody struct {
	Error *struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
	Detail  json.RawMessage `json:"detail"`
	Message string          `json:"message"`
	Type    string          `json:"type"`
}

// parseUpstreamMessage pulls a human-readable message (and OpenAI's own type /
// code, when present) out of an error body. An HTML body yields nothing: its
// markup is noise at best and a challenge page at worst.
func parseUpstreamMessage(contentType string, raw []byte) (msg, typ, code string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", "", ""
	}
	if looksHTML(http.Header{"Content-Type": {contentType}}, trimmed) {
		return "", "", ""
	}

	var eb errorBody
	if err := json.Unmarshal(trimmed, &eb); err == nil {
		switch {
		case eb.Error != nil && strings.TrimSpace(eb.Error.Message) != "":
			return oneLine(eb.Error.Message, maxErrorMessage), eb.Error.Type, rawToString(eb.Error.Code)
		case len(eb.Detail) > 0:
			if d := detailMessage(eb.Detail); d != "" {
				return oneLine(d, maxErrorMessage), eb.Type, ""
			}
		case strings.TrimSpace(eb.Message) != "":
			return oneLine(eb.Message, maxErrorMessage), eb.Type, ""
		}
		// Valid JSON with no recognised message field: say nothing rather than
		// echo an arbitrary object back at the client.
		return "", eb.Type, ""
	}

	// Not JSON: a short plain-text body is still the best explanation we have.
	if strings.Contains(strings.ToLower(contentType), "text/plain") || len(trimmed) <= maxErrorMessage {
		return oneLine(string(trimmed), maxErrorMessage), "", ""
	}
	return "", "", ""
}

// detailMessage reads FastAPI's detail field, which is a string in the simple
// case and an object (or list of objects) carrying a message otherwise.
func detailMessage(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Message != "" {
			return obj.Message
		}
		return obj.Detail
	}
	return ""
}

func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// withDetail appends an upstream message to our own description.
func withDetail(base, msg string) string {
	if msg == "" {
		return base
	}
	return base + ": " + msg
}

// oneLine collapses whitespace and control characters and truncates to max
// runes, so an upstream body can never inject newlines into a log line or a
// huge blob into an error envelope. Truncation counts runes, never bytes, so a
// multi-byte character can't be sliced in half.
func oneLine(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	runes := 0
	for _, r := range s {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
			runes++
		}
		if max > 0 && runes >= max {
			return strings.TrimSpace(b.String()) + "…"
		}
		b.WriteRune(r)
		runes++
	}
	return b.String()
}
