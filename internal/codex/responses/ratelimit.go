package responses

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header families forwarded out of an upstream response. The Codex backend
// reports the subscription's rolling usage windows on x-codex-* headers, and
// (on a 429) how long to wait on Retry-After. utraque forwards them verbatim so
// /healthz and the request log can report the real quota state rather than
// guessing at it.
const (
	prefixCodex      = "x-codex-"
	prefixRateLimit  = "x-ratelimit-"
	headerRetryAfter = "Retry-After"

	suffixUsedPercent  = "used-percent"
	suffixWindow       = "window-minutes"
	suffixResetSeconds = "reset-after-seconds"

	scopePrimary   = "primary"
	scopeSecondary = "secondary"
)

// Window is one rolling usage window the Codex backend reports (the plan's
// "primary" and "secondary" quota windows). Each field has a companion Has flag
// because zero is a meaningful value: 0% used is not the same as "not reported".
type Window struct {
	// UsedPercent is how much of the window is consumed, 0-100.
	UsedPercent float64
	// HasUsedPercent reports whether UsedPercent was present and parseable.
	HasUsedPercent bool
	// WindowMinutes is the length of the rolling window.
	WindowMinutes int
	// HasWindowMinutes reports whether WindowMinutes was present and parseable.
	HasWindowMinutes bool
	// ResetAfter is how long until the window rolls over.
	ResetAfter time.Duration
	// HasResetAfter reports whether ResetAfter was present and parseable.
	HasResetAfter bool
}

// Reported reports whether the upstream said anything at all about this window.
func (w Window) Reported() bool {
	return w.HasUsedPercent || w.HasWindowMinutes || w.HasResetAfter
}

// LogValue renders the window for slog. It carries no credential material.
func (w Window) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, 3)
	if w.HasUsedPercent {
		attrs = append(attrs, slog.Float64("used_percent", w.UsedPercent))
	}
	if w.HasWindowMinutes {
		attrs = append(attrs, slog.Int("window_minutes", w.WindowMinutes))
	}
	if w.HasResetAfter {
		attrs = append(attrs, slog.Duration("reset_after", w.ResetAfter))
	}
	return slog.GroupValue(attrs...)
}

// RateLimits is the quota picture an upstream response carried: the raw headers
// to forward on, plus the parsed windows and Retry-After that /healthz and the
// request log want as numbers.
//
// Headers holds only the allowlisted families (x-codex-*, x-ratelimit-*,
// Retry-After) — never the whole upstream header set, so nothing unexpected is
// echoed to the client.
type RateLimits struct {
	// Headers are the forwarded upstream headers, canonicalised, values verbatim.
	Headers http.Header
	// Primary and Secondary are the parsed rolling usage windows.
	Primary   Window
	Secondary Window
	// RetryAfter is the parsed Retry-After delay (seconds form or HTTP-date
	// form, resolved against the client's clock). Never negative.
	RetryAfter time.Duration
	// HasRetryAfter reports whether RetryAfter was present and parseable.
	HasRetryAfter bool
	// RetryAfterRaw is the Retry-After header exactly as upstream sent it, so a
	// caller can forward the original token rather than a re-rendered one.
	RetryAfterRaw string
}

// ParseRateLimits extracts the forwardable rate-limit headers from h and parses
// the ones utraque reports numerically. now resolves an HTTP-date Retry-After.
func ParseRateLimits(h http.Header, now time.Time) RateLimits {
	var rl RateLimits
	for k, vs := range h {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, prefixCodex) && !strings.HasPrefix(lk, prefixRateLimit) && lk != "retry-after" {
			continue
		}
		if rl.Headers == nil {
			rl.Headers = make(http.Header, 8)
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		rl.Headers[http.CanonicalHeaderKey(k)] = cp
	}
	rl.Primary = parseWindow(h, scopePrimary)
	rl.Secondary = parseWindow(h, scopeSecondary)
	rl.RetryAfterRaw = strings.TrimSpace(h.Get(headerRetryAfter))
	if rl.RetryAfterRaw != "" {
		if d, ok := parseRetryAfter(rl.RetryAfterRaw, now); ok {
			rl.RetryAfter, rl.HasRetryAfter = d, true
		}
	}
	return rl
}

// parseRetryAfter accepts both RFC 9110 forms: delta-seconds and HTTP-date. A
// date already in the past yields zero rather than a negative delay.
func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(raw); err == nil {
		if now.IsZero() {
			now = time.Now()
		}
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func parseWindow(h http.Header, scope string) Window {
	var w Window
	if v := strings.TrimSpace(h.Get(prefixCodex + scope + "-" + suffixUsedPercent)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			w.UsedPercent, w.HasUsedPercent = f, true
		}
	}
	if v := strings.TrimSpace(h.Get(prefixCodex + scope + "-" + suffixWindow)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			w.WindowMinutes, w.HasWindowMinutes = n, true
		}
	}
	if v := strings.TrimSpace(h.Get(prefixCodex + scope + "-" + suffixResetSeconds)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 0 {
				n = 0
			}
			w.ResetAfter, w.HasResetAfter = time.Duration(n)*time.Second, true
		}
	}
	return w
}

// Empty reports whether the upstream forwarded no rate-limit headers at all.
func (r RateLimits) Empty() bool { return len(r.Headers) == 0 }

// Apply copies the forwarded headers onto an outgoing response. Multi-valued
// headers keep every value. It is how a 429's Retry-After and the usage-window
// headers reach the client unchanged.
func (r RateLimits) Apply(dst http.Header) {
	if dst == nil {
		return
	}
	for k, vs := range r.Headers {
		dst.Del(k)
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// LogValue renders the parsed quota state for slog. It carries no credential
// material — only percentages, window lengths, and delays.
func (r RateLimits) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, 3)
	if r.Primary.Reported() {
		attrs = append(attrs, slog.Any("primary", r.Primary))
	}
	if r.Secondary.Reported() {
		attrs = append(attrs, slog.Any("secondary", r.Secondary))
	}
	if r.HasRetryAfter {
		attrs = append(attrs, slog.Duration("retry_after", r.RetryAfter))
	}
	return slog.GroupValue(attrs...)
}
