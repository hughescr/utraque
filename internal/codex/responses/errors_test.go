package responses

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/obs"
)

// Exercise StreamResponse, not just its parser: the redactor must receive the
// credential which signed this particular attempt, including a refreshed one.
func TestPublicResponseDetail(t *testing.T) {
	cred := testCred()
	cred.AccountID = "account/sk-inner123456789012"
	const shaped = "sk-unrelated123456789012"
	const prefix = "backend unavailable: "
	// Encode the first ASCII byte of each value so neither exact needle is
	// present on the wire. Matching before JSON decoding would miss both.
	escapeFirst := func(s string) string { return fmt.Sprintf(`\u%04x%s`, s[0], s[1:]) }
	escaped := `{"message":"` + prefix + escapeFirst(cred.AccessToken) + ` ` + escapeFirst(cred.AccountID) + `"}`
	if strings.Contains(escaped, cred.AccessToken) || strings.Contains(escaped, cred.AccountID) {
		t.Fatal("escaped fixture contains literal credential bytes")
	}
	for _, tc := range []struct {
		name, contentType, body, want string
	}{
		{"json", "application/json", `{"error":{"message":"` + prefix + cred.AccessToken + ` ` + cred.AccountID + ` ` + shaped + `","type":"` + cred.AccessToken + `","code":"` + cred.AccountID + `"}}`, prefix + strings.Repeat(obs.Redacted+" ", 2) + obs.Redacted},
		{"truncated JSON fallback", "application/json", `{"message":"` + cred.AccountID[:16], `{"message":"` + obs.Redacted},
		{"json escaped", "application/json", escaped, prefix + obs.Redacted + " " + obs.Redacted},
		{"plain", "text/plain", prefix + cred.AccountID + " " + cred.AccessToken, prefix + obs.Redacted + " " + obs.Redacted},
		// A byte cap or a rune cap applied before replacement loses part of the
		// exact needle, leaving a credential fragment visible to the caller.
		{"boundary", "application/json", `{"detail":"` + strings.Repeat("界", 501) + cred.AccessToken + `!"}`, strings.Repeat("界", 501) + obs.Redacted + "!"},
		{"shape at boundary", "text/plain", strings.Repeat("界", 500) + " " + shaped, strings.Repeat("界", 500) + " " + obs.Redacted},
		{"over-cap", "text/plain", strings.Repeat("界", 513), strings.Repeat("界", 511) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newClient(t, "https://example.invalid", func(o *Options) {
				o.Transport = funcTransport{&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})}}
			})
			_, err := client.StreamResponse(t.Context(), cred, testRequest())
			ue, ok := AsUpstream(err)
			if !ok || ue.Class != ClassServerError || ue.HTTPStatus() != 503 {
				t.Fatal("expected a classified 503 response")
			}
			wantPublic := "codex upstream error (HTTP 503)"
			if tc.want != "" {
				wantPublic += ": " + tc.want
			}
			if ue.UpstreamMessage != tc.want || !strings.HasSuffix(ue.Error(), wantPublic) || ue.APIError().Envelope().Error.Message != wantPublic {
				t.Error("typed error, log text and rendered envelope must carry the sanitized diagnostic")
			}
			if tc.name == "json" && (ue.UpstreamType != obs.Redacted || ue.UpstreamCode != obs.Redacted) {
				t.Error("typed upstream discriminators expose a credential")
			}
		})
	}
}

func TestPublicRedirectOmitsDestination(t *testing.T) {
	const dest = "https://other.invalid/private?ticket=opaque"
	ue := classifyResponse(http.StatusFound, http.Header{"Location": {dest}}, RateLimits{}, nil, testCred())
	msg := ue.APIError().Envelope().Error.Message
	if !strings.Contains(msg, "redirect") || !strings.Contains(msg, "302") || strings.Contains(msg, "other.invalid") || strings.Contains(msg, "ticket") {
		t.Fatalf("expected redirect status without destination: %q", msg)
	}
}

func TestPublicTransportErrorOmitsURL(t *testing.T) {
	const target = "https://private-user:private-password@example.invalid/private-path?ticket=opaque"
	for _, tc := range []struct {
		cause string
		class Class
		hint  string
	}{
		{"connection refused", ClassNetwork, "upstream"},
		{"remote error: tls: handshake failure", ClassGate, "uTLS"},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			cause := &url.Error{Op: "Post", URL: target, Err: errors.New(tc.cause)}
			ue := classifyTransport(t.Context(), cause)
			if ue.Class != tc.class || ue.HTTPStatus() != 502 || !errors.Is(ue, cause) {
				t.Fatal("lost transport classification or wrapped cause")
			}
			for _, msg := range []string{ue.Error(), ue.APIError().Envelope().Error.Message} {
				if !strings.Contains(msg, tc.hint) {
					t.Error("lost actionable classification")
				}
				for _, secret := range []string{"private-user", "private-password", "opaque"} {
					if strings.Contains(msg, secret) {
						t.Errorf("%s error included private URL components", tc.class)
						break
					}
				}
			}
		})
	}
}
