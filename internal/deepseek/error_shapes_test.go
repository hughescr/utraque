package deepseek

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/relay"
	"github.com/hughescr/utraque/internal/transport"
)

const errorFixtureBody = `{"model":"deepseek-flash","messages":[{"role":"user","content":"test"}]}`

// statusKinds is the Anthropic error type for each status the error path is
// exercised at.
var statusKinds = []struct {
	status int
	kind   string
}{
	{400, "invalid_request_error"}, {401, "authentication_error"}, {402, "billing_error"},
	{403, "permission_error"}, {404, "not_found_error"}, {413, "request_too_large"},
	{429, "rate_limit_error"}, {500, "api_error"}, {503, "overloaded_error"}, {529, "overloaded_error"},
}

// The OpenAI shape has no top-level type. Its message goes through the same
// scrubbing as the Anthropic shape; its type string is never relayed.
func TestPublicErrorOpenAIShape(t *testing.T) {
	const body = `{"error":{"message":"Insufficient Balance: check OpaqueCanary0812, Authorization: Bearer abcdefghijklmnop","type":"overloaded_error","param":null,"code":"invalid_request_error"},"extra":"opaque field"}`
	for _, tc := range statusKinds {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			rec := relayFixture(t, tc.status, http.Header{"Content-Type": {"text/plain"}}, strings.NewReader(body), false)
			want := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"Insufficient Balance: check [REDACTED], Authorization: [REDACTED]"}}`+"\n", tc.kind)
			if rec.Code != tc.status || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != want {
				t.Errorf("got %d %q %s; want %d JSON %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String(), tc.status, want)
			}
		})
	}
}

func TestPublicErrorOpenAIShapeCapsRunes(t *testing.T) {
	prefix := strings.Repeat("界", 508)
	body := `{"error":{"message":"` + prefix + relayCanary + `","type":"x","code":402}}`
	rec := relayFixture(t, 402, nil, strings.NewReader(body), false)
	want := fmt.Sprintf(`{"type":"error","error":{"type":"billing_error","message":%q}}`+"\n", prefix+obs.Redacted[:4])
	if rec.Code != 402 || rec.Body.String() != want {
		t.Fatalf("unexpected capped response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublicErrorOtherShapesFallBack(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"string error", `{"error":"Insufficient Balance OpaqueCanary0812"}`},
		{"empty message", `{"error":{"message":"  ","type":"x"}}`},
		{"no message", `{"error":{"type":"x","code":1}}`},
		{"no error object", `{"message":"Insufficient Balance OpaqueCanary0812"}`},
		{"foreign top-level type", `{"type":"message","error":{"message":"opaque"}}`},
		{"top-level type not a string", `{"type":7,"error":{"message":"opaque"}}`},
		{"message not a string", `{"error":{"message":{"nested":"opaque"}}}`},
		{"array", `[{"error":{"message":"opaque"}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := relayFixture(t, 402, nil, strings.NewReader(tc.body), false)
			want := `{"type":"error","error":{"type":"billing_error","message":"deepseek upstream returned HTTP 402"}}` + "\n"
			if rec.Code != 402 || rec.Body.String() != want {
				t.Errorf("got %d %s, want fixed fallback", rec.Code, rec.Body.String())
			}
		})
	}
}

// A 4xx the status mapping does not know is still the caller's request being
// refused, so the envelope says so. Mapped statuses keep their own type.
func TestPublicErrorUnmappedStatuses(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   string
	}{
		{405, "invalid_request_error"}, {410, "invalid_request_error"}, {418, "invalid_request_error"},
		{422, "invalid_request_error"}, {451, "invalid_request_error"}, {499, "invalid_request_error"},
		{409, "conflict_error"}, {408, "timeout_error"},
		{500, "api_error"}, {502, "api_error"}, {599, "api_error"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			rec := relayFixture(t, tc.status, nil, strings.NewReader("not json"), false)
			want := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"deepseek upstream returned HTTP %d"}}`+"\n", tc.kind, tc.status)
			if rec.Code != tc.status || rec.Body.String() != want {
				t.Errorf("fallback: got %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, want)
			}
			// With detail, an OpenAI-shaped body still takes its type from the status.
			rec = relayFixture(t, tc.status, nil, strings.NewReader(`{"error":{"message":"specific","type":"vendor"}}`), false)
			want = fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"specific"}}`+"\n", tc.kind)
			if rec.Code != tc.status || rec.Body.String() != want {
				t.Errorf("detail: got %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, want)
			}
		})
	}
}

// serverLeg is a Leg against a real local server with injected timeouts.
func serverLeg(t *testing.T, url string, idle, total time.Duration) *Leg {
	t.Helper()
	l, err := New(url, relayCanary, transport.NewStd(transport.DefaultOptions()), WithUpstreamIdleTimeout(idle))
	if err != nil {
		t.Fatal(err)
	}
	l.errorBodyDeadline = total
	return l
}

func timedErrorCall(t *testing.T, l *Leg) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	start := time.Now()
	rec, err := callLeg(t, l, "deepseek-flash", errorFixtureBody, false, false, nil)
	elapsed := time.Since(start)
	if err != nil {
		if err := apierr.Write(rec, err); err != nil {
			t.Fatal(err)
		}
	}
	return rec, elapsed
}

const (
	overloadedFallback = `{"type":"error","error":{"type":"overloaded_error","message":"deepseek upstream returned HTTP 503"}}` + "\n"
	detailedBody       = `{"type":"error","error":{"type":"api_error","message":"complete and slow"}}`
)

// An error body that never arrives ends at the idle timeout, not at the
// total deadline (set far out here), with the fixed message.
func TestErrorBodyStallEndsAtIdleTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	rec, elapsed := timedErrorCall(t, serverLeg(t, upstream.URL, 300*time.Millisecond, time.Minute))
	if rec.Code != 503 || rec.Body.String() != overloadedFallback {
		t.Errorf("got %d %s, want the fixed 503 fallback", rec.Code, rec.Body.String())
	}
	if elapsed > 5*time.Second {
		t.Errorf("a stalled error body held the request for %s; the idle timeout is 300ms", elapsed)
	}
}

// Bytes that keep arriving inside the idle window keep the read alive: the
// body below takes longer in total than the idle timeout, so it is only read
// whole if the read re-arms the idle timer on every byte.
func TestErrorBodyReadRearmsIdleTimer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fl := w.(http.Flusher)
		fl.Flush()
		for _, part := range []string{detailedBody[:20], detailedBody[20:40], detailedBody[40:]} {
			_, _ = io.WriteString(w, part)
			fl.Flush()
			time.Sleep(200 * time.Millisecond)
		}
	}))
	defer upstream.Close()
	rec, elapsed := timedErrorCall(t, serverLeg(t, upstream.URL, 500*time.Millisecond, time.Minute))
	want := `{"type":"error","error":{"type":"api_error","message":"complete and slow"}}` + "\n"
	if rec.Code != 503 || rec.Body.String() != want {
		t.Errorf("got %d %s (after %s), want the detail", rec.Code, rec.Body.String(), elapsed)
	}
}

// An upstream that trickles a byte at a time never trips the idle timer; the
// total deadline is what ends the read.
func TestErrorBodyTrickleEndsAtTotalDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fl := w.(http.Flusher)
		fl.Flush()
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"never `)
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
				if _, err := io.WriteString(w, "x"); err != nil {
					return
				}
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()
	const total = 400 * time.Millisecond
	rec, elapsed := timedErrorCall(t, serverLeg(t, upstream.URL, time.Minute, total))
	if rec.Code != 503 || rec.Body.String() != overloadedFallback {
		t.Errorf("got %d %s, want the fixed 503 fallback", rec.Code, rec.Body.String())
	}
	if elapsed < total || elapsed > 5*time.Second {
		t.Errorf("trickling body ended after %s; want about the %s total deadline", elapsed, total)
	}
}

func TestErrorBodyDeadlineConstant(t *testing.T) {
	if errorBodyReadDeadline != 10*time.Second {
		t.Errorf("errorBodyReadDeadline = %s, want 10s", errorBodyReadDeadline)
	}
	l, err := New("https://fixture.invalid/anthropic", relayCanary, transport.NewStd(transport.DefaultOptions()))
	if err != nil {
		t.Fatal(err)
	}
	if l.errorBodyDeadline != errorBodyReadDeadline {
		t.Errorf("a new Leg reads error bodies for %s, want %s", l.errorBodyDeadline, errorBodyReadDeadline)
	}
}

// A 2xx whose body is an Anthropic error object is answered through the error
// pipeline with the upstream's status; nothing else from the body survives.
func TestSuccessStatusErrorObjectIsSanitised(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			"anthropic shape",
			`{"type":"error","private":"drop","error":{"type":"overloaded_error","message":"check OpaqueCanary0812","extra":"drop"}}`,
			`{"type":"error","error":{"type":"overloaded_error","message":"check [REDACTED]"}}`,
		},
		{
			"escaped type",
			`{"type":"error","error":{"type":"vendor","message":"hello OpaqueCanary0812"}}`,
			`{"type":"error","error":{"type":"api_error","message":"hello [REDACTED]"}}`,
		},
		{
			"no usable message",
			`{"type":"error","error":{"type":"overloaded_error"}}`,
			`{"type":"error","error":{"type":"api_error","message":"deepseek upstream returned an error object"}}`,
		},
		{
			"no error object",
			`{"type":"error"}`,
			`{"type":"error","error":{"type":"api_error","message":"deepseek upstream returned an error object"}}`,
		},
	} {
		for _, status := range []int{200, 201} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, status), func(t *testing.T) {
				rec := relayFixture(t, status, http.Header{"Content-Type": {"application/json"}, "Retry-After": {"3"}}, strings.NewReader(tc.body), false)
				if rec.Code != status || rec.Body.String() != tc.want+"\n" || strings.Contains(rec.Body.String(), "model") {
					t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body.String(), status, tc.want)
				}
				if rec.Header().Get("Retry-After") != "3" || rec.Header().Get("Content-Type") != "application/json" {
					t.Errorf("error metadata headers = %v", rec.Header())
				}
			})
		}
	}
}

// Only a top-level "error" type is an error object. Success bodies, including
// ones that merely contain an error-typed value deeper down, are relayed with
// the model rewrite and nothing more. The bodies are already compact with
// sorted keys, so the rewrite's re-encoding leaves their bytes alone.
func TestSuccessStatusBodiesStayByteIdentical(t *testing.T) {
	for _, body := range []string{
		`{"content":[{"text":"` + relayCanary + `","type":"text"}],"model":"deepseek-flash","type":"message"}`,
		`{"content":[{"text":"x","type":"error"}],"model":"deepseek-flash","type":"message"}`,
		`{"error":{"message":"` + relayCanary + `"},"model":"deepseek-flash","type":"message"}`,
		`{"model":"deepseek-flash","type":"message_error"}`,
	} {
		rec := relayFixture(t, 200, http.Header{"Content-Type": {"application/json"}}, strings.NewReader(body), false)
		if rec.Code != 200 || rec.Body.String() != body {
			t.Errorf("success body changed: %d %q, want %q", rec.Code, rec.Body.String(), body)
		}
	}
}

// jsonResponse without the model rewrite (no route uses it today: count_tokens
// is estimated locally) relays a body byte for byte unless it is an error object.
func TestJSONResponseWithoutRewrite(t *testing.T) {
	leg, err := New("https://fixture.invalid/anthropic", relayCanary, transport.NewStd(transport.DefaultOptions()))
	if err != nil {
		t.Fatal(err)
	}
	call := func(status int, body string) *httptest.ResponseRecorder {
		t.Helper()
		resp := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1}
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
		_, idle := relay.WithUpstreamIdleDeadline(req.Context(), 0)
		rec := httptest.NewRecorder()
		if err := leg.jsonResponse(rec, req, idle, resp, "deepseek-flash", false); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	for _, body := range []string{
		`{ "type": "message_count",  "input_tokens":  3 }`,
		`{"input_tokens":3,"nested":{"type":"error","message":"` + relayCanary + `"}}`,
		`{"input_tokens":3}` + "\n",
	} {
		if rec := call(200, body); rec.Code != 200 || rec.Body.String() != body {
			t.Errorf("body changed: %d %q, want %q", rec.Code, rec.Body.String(), body)
		}
	}
	rec := call(200, `{"type":"error","error":{"type":"api_error","message":"hi OpaqueCanary0812"}}`)
	want := `{"type":"error","error":{"type":"api_error","message":"hi [REDACTED]"}}` + "\n"
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("error object: got %d %q, want %q", rec.Code, rec.Body.String(), want)
	}
}
