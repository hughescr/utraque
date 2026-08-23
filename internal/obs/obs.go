// Package obs holds utraque's observability plumbing: slog handler
// construction, per-request logger and request-id propagation, and
// allowlist-based header redaction. Only headers on the allowlist are ever
// logged with their values; everything else is reported by name only, and
// credentials are never logged in any form.
//
// obs imports nothing outside the standard library. That is why Hash below
// duplicates config.Redact's few lines rather than importing config: obs sits
// under every other package and must stay dependency-free.
package obs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Log formats accepted by NewHandler.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// RequestIDHeader is the inbound header honoured as a caller-supplied id.
const RequestIDHeader = "X-Request-Id"

// MaxValueLen caps how much of a header value is logged.
const MaxValueLen = 512

// NewHandler builds the slog handler for the given level and format.
func NewHandler(w io.Writer, level slog.Level, format string) (slog.Handler, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", FormatJSON:
		return slog.NewJSONHandler(w, opts), nil
	case FormatText:
		return slog.NewTextHandler(w, opts), nil
	default:
		return nil, fmt.Errorf("obs: unknown log format %q", format)
	}
}

// NewLogger builds a *slog.Logger over NewHandler.
func NewLogger(w io.Writer, level slog.Level, format string) (*slog.Logger, error) {
	h, err := NewHandler(w, level, format)
	if err != nil {
		return nil, err
	}
	return slog.New(h), nil
}

type ctxKey int

const (
	ctxKeyLogger ctxKey = iota
	ctxKeyRequestID
)

// WithLogger attaches a request-scoped logger to ctx.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// LoggerFrom returns the request-scoped logger, or slog.Default.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}

// WithRequestID attaches the request id to ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestIDFrom returns the request id, or "".
func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// defaultAllow is the fixed set of header names whose values may be logged.
var defaultAllow = []string{
	"accept",
	"accept-encoding",
	"accept-language",
	"anthropic-beta",
	"anthropic-version",
	"cache-control",
	"connection",
	"content-encoding",
	"content-length",
	"content-type",
	"te",
	"user-agent",
	"x-request-id",
}

// defaultAllowPrefixes matches families of harmless client-telemetry headers.
var defaultAllowPrefixes = []string{"x-stainless-"}

// alwaysDeny can never be allowlisted, whatever a caller passes to NewRedactor.
// This is belt-and-braces against a future edit to defaultAllow.
var alwaysDeny = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"set-cookie",
	"x-api-key",
	"anthropic-auth-token",
	"openai-api-key",
	"x-utraque-token",
}

// Redactor decides which request headers may be logged. It is immutable after
// construction and safe for concurrent use.
type Redactor struct {
	allow    map[string]struct{}
	prefixes []string
	deny     map[string]struct{}
}

// NewRedactor builds a Redactor allowing exactly the named headers (case
// insensitive) plus the default safe prefixes. Names on the always-deny list
// are dropped even when passed in.
func NewRedactor(allow ...string) *Redactor {
	r := &Redactor{
		allow:    make(map[string]struct{}, len(allow)),
		prefixes: slices.Clone(defaultAllowPrefixes),
		deny:     make(map[string]struct{}, len(alwaysDeny)),
	}
	for _, n := range alwaysDeny {
		r.deny[n] = struct{}{}
	}
	for _, n := range allow {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if _, bad := r.deny[n]; bad {
			continue
		}
		r.allow[n] = struct{}{}
	}
	return r
}

// DefaultRedactor returns the standard allowlist.
func DefaultRedactor() *Redactor { return NewRedactor(defaultAllow...) }

// Allowed reports whether a header's value may be logged.
func (r *Redactor) Allowed(name string) bool {
	if r == nil {
		return false
	}
	n := strings.ToLower(strings.TrimSpace(name))
	if _, bad := r.deny[n]; bad {
		return false
	}
	if _, ok := r.allow[n]; ok {
		return true
	}
	for _, p := range r.prefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// Header renders h as a slog group holding the allowlisted headers plus a
// "redacted" attr naming, but never valuing, everything withheld.
func (r *Redactor) Header(h http.Header) slog.Value {
	if r == nil || len(h) == 0 {
		return slog.GroupValue()
	}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)

	attrs := make([]slog.Attr, 0, len(h)+1)
	withheld := make([]string, 0, len(h))
	for _, name := range names {
		lower := strings.ToLower(name)
		if !r.Allowed(lower) {
			withheld = append(withheld, lower)
			continue
		}
		vals := h.Values(name)
		switch len(vals) {
		case 0:
		case 1:
			attrs = append(attrs, slog.String(lower, truncate(vals[0])))
		default:
			out := make([]string, len(vals))
			for i, v := range vals {
				out[i] = truncate(v)
			}
			attrs = append(attrs, slog.Any(lower, out))
		}
	}
	if len(withheld) > 0 {
		attrs = append(attrs, slog.Any("redacted", withheld))
	}
	return slog.GroupValue(attrs...)
}

// Attrs is Header as a flat slice, for callers assembling their own group.
func (r *Redactor) Attrs(h http.Header) []slog.Attr { return r.Header(h).Group() }

func truncate(s string) string {
	if len(s) <= MaxValueLen {
		return s
	}
	return s[:MaxValueLen] + "...(truncated)"
}

// Hash fingerprints a value so it can be correlated in logs without being
// disclosed. The empty string maps to the empty string.
func Hash(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

// HashAttr is Hash as a slog.Attr, empty when s is empty.
func HashAttr(key, s string) slog.Attr {
	if s == "" {
		return slog.Attr{}
	}
	return slog.String(key, Hash(s))
}

// SafePath renders a request URL's path without its query string.
func SafePath(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.EscapedPath()
}
