//go:build live

// This file is excluded from every default test run. It is the ONLY test in the
// package that touches a real host, and it exists as a manual tripwire:
//
//	go test -tags live -run TestLive ./internal/transport/...
//
// Nothing here sends a credential. Each case makes one unauthenticated request
// and looks only at whether the TLS handshake completed and whether the answer
// came from the API or from a bot gate. A 401 is a PASS: it means the request
// reached the Codex backend, which is exactly what is being measured.

package transport

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const liveCodexURL = "https://chatgpt.com/backend-api/codex/responses"

// TestLiveUTLSReachesCodex checks that a Chrome-shaped ClientHello is accepted
// by chatgpt.com's edge: the handshake completes, HTTP/2 is negotiated, and the
// response is a real API answer rather than a challenge page.
func TestLiveUTLSReachesCodex(t *testing.T) {
	assertNotGated(t, NewUTLS(DefaultOptions()), KindUTLS)
}

// TestLiveStdReachesCodex is the control. While this passes, the uTLS transport
// is not needed and codex.transport=auto correctly stays on std. The day it
// starts failing while TestLiveUTLSReachesCodex still passes is the day the
// automatic switch earns its keep.
func TestLiveStdReachesCodex(t *testing.T) {
	assertNotGated(t, NewStd(DefaultOptions()), KindStd)
}

func assertNotGated(t *testing.T, tr Transport, wantKind string) {
	t.Helper()
	if tr.Kind() != wantKind {
		t.Fatalf("Kind() = %q, want %q", tr.Kind(), wantKind)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// No Authorization header: this measures the edge, not the account.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, liveCodexURL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("originator", honestOriginator)

	resp, err := tr.Client().Do(req)
	if err != nil {
		t.Fatalf("%s transport could not complete the request (a refused handshake is how a fingerprint gate looks): %v", wantKind, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	t.Logf("%s transport: %s %d, cf-ray=%q, content-type=%q",
		wantKind, resp.Proto, resp.StatusCode, resp.Header.Get("cf-ray"), resp.Header.Get("Content-Type"))

	// A challenge page is HTML, whatever status it carries.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("%s transport was answered with an HTML page (content-type %q): this is a bot gate, not an API answer", wantKind, ct)
	}
	for _, marker := range []string{"cf-browser-verification", "Just a moment", "you have been blocked"} {
		if strings.Contains(string(body), marker) {
			t.Errorf("%s transport was answered with a challenge page (matched %q)", wantKind, marker)
		}
	}
	// 401 is the expected answer to an unauthenticated request and proves the
	// call reached the backend.
	if resp.StatusCode == http.StatusForbidden {
		t.Errorf("%s transport got a bare 403, which is the usual shape of a fingerprint gate", wantKind)
	}
}
