package deepseek

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/obs"
)

const relayCanary = "OpaqueCanary0812"

type scriptedResponse func(*http.Request) (*http.Response, error)

func (rt scriptedResponse) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func relayFixture(t *testing.T, status int, headers http.Header, body io.Reader, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	client := &http.Client{Transport: scriptedResponse(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(body), ContentLength: -1}, nil
	})}
	leg, err := New("https://fixture.invalid/anthropic", relayCanary, followingTransport{client})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := callLeg(t, leg, "deepseek-flash", `{"model":"deepseek-flash","messages":[{"role":"user","content":"test"}]}`, stream, false, nil)
	if err != nil {
		if err := apierr.Write(rec, err); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

func TestPublicErrorEnvelope(t *testing.T) {
	const body = `{"type":"error","extra":"opaque field","error":{"type":"vendor-specific","message":"check OpaqueCanary0812 or ` + `\` + `u004fpaqueCanary0812, Authorization: Bearer abcdefghijklmnop"}}`
	for _, tc := range []struct {
		status int
		kind   string
	}{
		{400, "invalid_request_error"}, {401, "authentication_error"}, {402, "billing_error"},
		{403, "permission_error"}, {404, "not_found_error"}, {413, "request_too_large"},
		{429, "rate_limit_error"}, {500, "api_error"}, {503, "overloaded_error"}, {529, "overloaded_error"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			rec := relayFixture(t, tc.status, http.Header{"Content-Type": {"text/plain"}}, strings.NewReader(body), false)
			want := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"check [REDACTED] or [REDACTED], Authorization: [REDACTED]"}}`+"\n", tc.kind)
			if rec.Code != tc.status || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != want {
				t.Errorf("got %d %q %s; want %d JSON %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String(), tc.status, want)
			}
		})
	}
}

func TestPublicErrorRedactsBeforeRuneLimit(t *testing.T) {
	prefix := strings.Repeat("界", 508)
	body := `{"type":"error","error":{"type":"api_error","message":"` + prefix + relayCanary + `"}}`
	rec := relayFixture(t, 503, nil, strings.NewReader(body), false)
	// A recognised type is preserved even if its usual status differs. Neither
	// a partial credential nor a broken multibyte rune may reach the envelope.
	want := fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":%q}}`+"\n", prefix+obs.Redacted[:4])
	if rec.Code != 503 || rec.Body.String() != want {
		t.Fatalf("unexpected capped response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublicErrorFallback(t *testing.T) {
	const valid = `{"type":"error","error":{"type":"api_error","message":"benign"}}`
	for _, tc := range []struct{ name, body, encoding string }{
		{"incomplete", `{"type":"error","error":{"message":"OpaqueCanary`, ""},
		{"not json", "provider failure OpaqueCanary", ""},
		{"empty message", `{"type":"error","error":{"message":"   "}}`, ""},
		{"wrong envelope", `{"type":"message","error":{"message":"opaque"}}`, ""},
		{"unexpected encoding", valid, "gzip"},
		{"8 MiB, complete-looking prefix", valid + strings.Repeat(" ", (8<<20)-len(valid)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := strings.NewReader(tc.body)
			rec := relayFixture(t, 429, http.Header{"Content-Encoding": {tc.encoding}}, reader, false)
			if n := reader.Size() - int64(reader.Len()); n > 64<<10 {
				t.Errorf("read %d bytes; limit is 64 KiB", n)
			}
			want := `{"type":"error","error":{"type":"rate_limit_error","message":"deepseek upstream returned HTTP 429"}}` + "\n"
			if rec.Code != 429 || rec.Body.String() != want {
				t.Errorf("got %d body length %d, want fixed fallback", rec.Code, rec.Body.Len())
			}
		})
	}
}

func TestPublicErrorMetadata(t *testing.T) {
	const date = "Mon, 02 Jan 2006 15:04:05 GMT"
	const reset = "2026-11-30T15:04:05.433Z"
	for _, tc := range []struct {
		name, field string
		values      []string
		keep        bool
	}{
		{"retry seconds", "Retry-After", []string{"0"}, true},
		{"retry date", "Retry-After", []string{date}, true},
		{"retry rejects negative", "Retry-After", []string{"-1"}, false},
		{"retry rejects token", "Retry-After", []string{relayCanary}, false},
		{"quota decimals", "X-Ratelimit-Remaining", []string{"0", "4.5"}, true},
		{"quota instant", "Anthropic-Ratelimit-Tokens-Reset", []string{reset}, true},
		{"quota duration", "X-Ratelimit-Reset-Requests", []string{"6m0s"}, true},
		{"quota invalid second value", "X-Ratelimit-Remaining", []string{"5", relayCanary}, false},
		{"quota negative", "X-Ratelimit-Remaining", []string{"-1"}, false},
		{"quota non-finite", "X-Ratelimit-Remaining", []string{"NaN"}, false},
		{"quota too long", "X-Ratelimit-Limit", []string{strings.Repeat("1", 65)}, false},
		{"request id", "Request-Id", []string{"req-187"}, true},
		{"x request id", "X-Request-Id", []string{"req-177"}, true},
		{"request id space", "Request-Id", []string{"bad id"}, false},
		{"request id echoed attempt key", "Request-Id", []string{relayCanary}, false},
		{"request id token shape", "Request-Id", []string{"sk-ant-api03-abcdefghijklmnopqrstuvw"}, false},
		{"request id too long", "Request-Id", []string{strings.Repeat("a", 129)}, false},
		{"no upstream cache directives", "Cache-Control", []string{"public"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{http.CanonicalHeaderKey(tc.field): tc.values}
			rec := relayFixture(t, 429, h, strings.NewReader("not json"), false)
			got := rec.Result().Header.Values(tc.field)
			if (!tc.keep && len(got) != 0) || (tc.keep && strings.Join(tc.values, "|") != strings.Join(got, "|")) {
				t.Errorf("%s: got %q, inputs %q, keep=%v", tc.field, got, tc.values, tc.keep)
			}
		})
	}
	h := http.Header{"Connection": {"other", "foo, ReTrY-AfTeR"}, "Retry-After": {"2"}}
	if got := relayFixture(t, 429, h, strings.NewReader("not json"), false).Result().Header.Get("Retry-After"); got != "" {
		t.Errorf("copied Connection-nominated retry header: %q", got)
	}
}

func TestPublicInBandError(t *testing.T) {
	body := "event: error\ndata: " + `{"type":"error","private":"drop","error":{"type":"OpaqueCanary0812","message":"check OpaqueCanary0812"}}` + "\n\n"
	rec := relayFixture(t, 200, http.Header{"Content-Type": {"text/event-stream"}}, strings.NewReader(body), true)
	want := "event: error\ndata: " + `{"type":"error","error":{"type":"api_error","message":"check [REDACTED]"}}` + "\n\n"
	if rec.Code != 200 || rec.Body.String() != want {
		t.Error("in-band error did not contain only safe fields")
	}
}

func TestPublicErrorPolicyLeavesSuccessBytesAlone(t *testing.T) {
	// The successful payload deliberately contains the canary: error-message
	// scrubbing must never become a filter over completions or stream frames.
	msg := `{"content":[{"text":"` + relayCanary + `","type":"text"}],"model":"deepseek-flash","type":"message"}`
	stream := "event: message_start\ndata: {\"message\":{\"model\":\"deepseek-flash\"},\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\", \"text\":\"" + relayCanary + "\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		kind, body string
		stream     bool
	}{{"application/json", msg, false}, {"text/event-stream", stream, true}} {
		t.Run(tc.kind, func(t *testing.T) {
			rec := relayFixture(t, 200, http.Header{"Content-Type": {tc.kind}}, strings.NewReader(tc.body), tc.stream)
			if rec.Code != 200 || rec.Body.String() != tc.body {
				t.Errorf("success bytes changed: %d %q, want %q", rec.Code, rec.Body.String(), tc.body)
			}
		})
	}
}
