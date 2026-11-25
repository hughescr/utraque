package deepseek

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/proxyhdr"
	"github.com/hughescr/utraque/internal/publicdetail"
	"github.com/hughescr/utraque/internal/relay"
)

const (
	maxErrorBody  = 64 << 10
	maxErrorRunes = 512

	// errorBodyReadDeadline bounds the whole read of an upstream error body.
	// The idle timer only measures the gap between bytes, so an upstream that
	// trickles a byte at a time would otherwise hold the request indefinitely
	// for the sake of a diagnostic. When it passes, the fixed per-status
	// message goes out instead.
	errorBodyReadDeadline = 10 * time.Second
)

// upstreamErrorType is the Anthropic error type for an upstream HTTP status.
// Any 4xx without a specific mapping is the caller's request being refused, so
// it is invalid_request_error rather than apierr's api_error default.
func upstreamErrorType(status int) apierr.ErrorType {
	typ := apierr.TypeForStatus(status)
	if typ == apierr.TypeAPI && status >= 400 && status < 500 {
		return apierr.TypeInvalidRequest
	}
	return typ
}

func (l *Leg) publicError(resp *http.Response, idle *relay.UpstreamIdleGuard, identity bool) *apierr.Error {
	status := resp.StatusCode
	e := apierr.WithStatus(status, upstreamErrorType(status), "deepseek upstream returned HTTP %d", status)
	if !identity || resp.ContentLength > maxErrorBody {
		return e
	}
	raw, ok := l.readErrorBody(resp, idle)
	if !ok {
		return e
	}
	typ, message, ok := errorDetail(raw)
	if !ok {
		return e
	}
	l.setPublicDetail(e, typ, message)
	return e
}

// readErrorBody reads a complete error body of at most maxErrorBody bytes. It
// reports false for anything else: a read error, a body that hit the limit
// (at the limit with unknown length there is no proof of EOF, so don't read a
// probe byte and don't accept a valid-looking prefix as the whole body), a
// short body, or a read still going at errorBodyReadDeadline.
func (l *Leg) readErrorBody(resp *http.Response, idle *relay.UpstreamIdleGuard) ([]byte, bool) {
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1) // Buffered: a read that outlives the deadline must not block.
	go func() {
		raw, err := io.ReadAll(io.LimitReader(idle.Wrap(resp.Body), maxErrorBody))
		done <- result{raw, err}
	}()
	timer := time.NewTimer(l.errorBodyDeadline)
	defer timer.Stop()
	select {
	case res := <-done:
		raw := res.raw
		if res.err != nil || (len(raw) == maxErrorBody && resp.ContentLength != maxErrorBody) ||
			(resp.ContentLength > 0 && int64(len(raw)) != resp.ContentLength) {
			return nil, false
		}
		return raw, true
	case <-timer.C:
		_ = resp.Body.Close() // Unblocks the reader; the goroutine then exits.
		return nil, false
	}
}

// errorDetail extracts the only two fields of an upstream error body that are
// ever relayed. Two shapes are accepted: Anthropic's {"type":"error","error":
// {"type","message"}}, and the OpenAI shape {"error":{"message","type","code"}}
// that a gateway in front of the vendor may use, which has no top-level type.
// The returned type is empty for the OpenAI shape, whose "type" string is not
// an Anthropic error type; the HTTP status decides that instead.
func errorDetail(raw []byte) (typ, message string, ok bool) {
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || strings.TrimSpace(body.Error.Message) == "" {
		return "", "", false
	}
	switch body.Type {
	case aschema.EventError:
		return body.Error.Type, body.Error.Message, true
	case "":
		return "", body.Error.Message, true
	default:
		return "", "", false
	}
}

// setPublicDetail puts a scrubbed, capped upstream message in e, and the
// upstream's error type too when it is a known Anthropic one.
func (l *Leg) setPublicDetail(e *apierr.Error, upstreamType, message string) {
	// Only a known Anthropic type round-trips through the status mapping. An
	// arbitrary upstream string must not become an unredacted output field.
	if typ := apierr.ErrorType(upstreamType); upstreamType != "" && apierr.TypeForStatus(apierr.StatusFor(typ)) == typ {
		e.Type = typ
	}
	msg := obs.Scrub(publicdetail.Replace(message, obs.Redacted, l.apiKey))
	n := 0
	for i := range msg {
		if n == maxErrorRunes {
			msg = msg[:i]
			break
		}
		n++
	}
	e.Message = msg
}

// A generated error envelope has its own representation headers. Only retry,
// quota and support-correlation metadata survive from the upstream response.
func (l *Leg) copyErrorHeaders(dst, src http.Header) {
	hop := proxyhdr.ConnectionTokens(src)
	for name, values := range src {
		name = http.CanonicalHeaderKey(name)
		if _, ok := hop[name]; ok || len(values) == 0 {
			continue
		}
		var valid func(string) bool
		switch {
		case name == "Retry-After":
			valid = validRetryAfter
		case strings.HasPrefix(name, "X-Ratelimit-"), strings.HasPrefix(name, "Anthropic-Ratelimit-"):
			valid = validQuotaValue
		case name == "Request-Id", name == "X-Request-Id":
			valid = validSupportID
		default:
			continue
		}
		if !slices.ContainsFunc(values, func(v string) bool {
			return !valid(v) || (len(l.apiKey) >= 4 && strings.Contains(v, l.apiKey))
		}) {
			dst[name] = slices.Clone(values)
		}
	}
}

func validRetryAfter(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	if strings.Trim(v, "0123456789") == "" {
		return true
	}
	_, err := http.ParseTime(v)
	return err == nil
}

func validQuotaValue(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	if strings.Trim(v, "0123456789.eE+-") == "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(f, 0) && f >= 0 {
			return true
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return true
	}
	d, err := time.ParseDuration(v)
	return err == nil && d >= 0
}

func validSupportID(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for i := range len(v) {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return obs.Scrub(v) == v
}
