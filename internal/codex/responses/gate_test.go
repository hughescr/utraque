package responses

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/transport"
)

// These tests wire the real auto transport to a fake upstream on loopback and
// assert the one behaviour that connects the two packages: a gate-class failure
// — and only a gate-class failure — switches the process onto uTLS. Nothing
// here contacts chatgpt.com, and no real credential is read.

// gateHandler answers every request with a Cloudflare challenge, which is what
// a fingerprint gate looks like from the client side.
func gateHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-ray", "8f0a1b2c3d4e5f60-SJC")
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, cloudflareChallenge)
	})
}

func TestGateSwitchesTheAutoTransportExactlyOnce(t *testing.T) {
	srv := httptest.NewServer(gateHandler())
	defer srv.Close()

	tr := transport.NewAuto(transport.DefaultOptions(), quietLogger())
	c := newClient(t, srv.URL, func(o *Options) { o.Transport = tr })

	if tr.Kind() != transport.KindStd {
		t.Fatalf("Kind() = %q before any request, want %q", tr.Kind(), transport.KindStd)
	}

	for i := range 3 {
		body, err := c.Stream(context.Background(), testCred(), testRequest())
		if err == nil {
			_ = body.Close()
			t.Fatalf("request %d: want a gate error, got a stream", i)
		}
		ue, ok := AsUpstream(err)
		if !ok || !ue.IsGate() {
			t.Fatalf("request %d: want a gate error, got %v", i, err)
		}
		if tr.Kind() != transport.KindUTLS {
			t.Errorf("request %d: Kind() = %q after a gate, want %q", i, tr.Kind(), transport.KindUTLS)
		}
	}

	// Exactly once: the first gate consumed the one switch this process gets,
	// so a further report has nothing left to do.
	if transport.ReportGate(tr) {
		t.Error("ReportGate switched again; the switch must happen at most once per process")
	}
}

func TestNonGateFailuresLeaveTheTransportAlone(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantClass   Class
	}{
		{"401 rejected token", http.StatusUnauthorized, "application/json",
			`{"error":{"message":"invalid token"}}`, ClassAuth},
		{"429 plan quota", http.StatusTooManyRequests, "application/json",
			`{"error":{"message":"rate limited"}}`, ClassRateLimit},
		{"500 backend fault", http.StatusInternalServerError, "application/json",
			`{"error":{"message":"oops"}}`, ClassUpstream},
		{"400 bad request", http.StatusBadRequest, "application/json",
			`{"error":{"message":"unknown model"}}`, ClassTerminal},
		// A real API error served from behind Cloudflare is the trap: the
		// cf-ray header is present but the answer came from the backend, so
		// the TLS fingerprint is demonstrably fine.
		{"403 account error behind cloudflare", http.StatusForbidden, "application/json",
			`{"error":{"message":"forbidden for this account"}}`, ClassTerminal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("cf-ray", "8f0a1b2c3d4e5f60-SJC")
				w.Header().Set("Server", "cloudflare")
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			tr := transport.NewAuto(transport.DefaultOptions(), quietLogger())
			c := newClient(t, srv.URL, func(o *Options) { o.Transport = tr })

			body, err := c.Stream(context.Background(), testCred(), testRequest())
			if err == nil {
				_ = body.Close()
				t.Fatal("want an error, got a stream")
			}
			ue, ok := AsUpstream(err)
			if !ok {
				t.Fatalf("not an *UpstreamError: %v", err)
			}
			if ue.Class != tc.wantClass {
				t.Fatalf("Class = %q, want %q", ue.Class, tc.wantClass)
			}
			if ue.IsGate() {
				t.Fatal("this case must not classify as a gate")
			}
			if tr.Kind() != transport.KindStd {
				t.Errorf("Kind() = %q, want %q: only a gate may change the TLS fingerprint",
					tr.Kind(), transport.KindStd)
			}
			// The one switch must still be available for a real gate later.
			if !transport.ReportGate(tr) {
				t.Error("the transport had already switched, so a non-gate failure consumed the switch")
			}
		})
	}
}

// TestSuccessLeavesTheTransportAlone is the base case: the live proxy runs on
// std and must keep running on std.
func TestSuccessLeavesTheTransportAlone(t *testing.T) {
	srv, _ := sseServer(t, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	defer srv.Close()

	tr := transport.NewAuto(transport.DefaultOptions(), quietLogger())
	c := newClient(t, srv.URL, func(o *Options) { o.Transport = tr })

	body, err := c.Stream(context.Background(), testCred(), testRequest())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()

	if tr.Kind() != transport.KindStd {
		t.Errorf("Kind() = %q after a successful stream, want %q", tr.Kind(), transport.KindStd)
	}
}

// TestFixedTransportsIgnoreAGate: an operator who pinned std or utls chose it,
// and a gate must not silently override that choice.
func TestFixedTransportsIgnoreAGate(t *testing.T) {
	srv := httptest.NewServer(gateHandler())
	defer srv.Close()

	for _, tr := range []transport.Transport{
		transport.NewStd(transport.DefaultOptions()),
		transport.NewUTLS(transport.DefaultOptions()),
	} {
		want := tr.Kind()
		c := newClient(t, srv.URL, func(o *Options) { o.Transport = tr })
		body, err := c.Stream(context.Background(), testCred(), testRequest())
		if err == nil {
			_ = body.Close()
			t.Fatalf("%s: want a gate error, got a stream", want)
		}
		if tr.Kind() != want {
			t.Errorf("%s transport changed to %q after a gate", want, tr.Kind())
		}
	}
}

// TestTheRequestLineFollowsTheTransportFlip pins the signal that makes a flip
// visible at all. The stack is recorded by THIS client at dispatch, not by the
// server middleware (which only ever sees the Anthropic leg's fixed std
// transport), so the gated request reads "std" and its successor reads "utls".
func TestTheRequestLineFollowsTheTransportFlip(t *testing.T) {
	srv := httptest.NewServer(gateHandler())
	defer srv.Close()

	tr := transport.NewAuto(transport.DefaultOptions(), quietLogger())
	c := newClient(t, srv.URL, func(o *Options) { o.Transport = tr })

	kinds := make([]string, 0, 2)
	for i := range 2 {
		sum := obs.NewSummary()
		ctx := obs.WithSummary(context.Background(), sum)
		body, err := c.Stream(ctx, testCred(), testRequest())
		if err == nil {
			_ = body.Close()
			t.Fatalf("request %d: want a gate error, got a stream", i)
		}
		kind, _ := sum.Fields()["transport"].(string)
		kinds = append(kinds, kind)
	}

	if kinds[0] != transport.KindStd {
		t.Errorf("the gated request logged transport %q, want %q: it really did go out on std",
			kinds[0], transport.KindStd)
	}
	if kinds[1] != transport.KindUTLS {
		t.Errorf("the request after the flip logged transport %q, want %q",
			kinds[1], transport.KindUTLS)
	}
}
