package deepseek

import (
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/transport"
)

// canaryBasePassword is a distinctive userinfo password; no error may echo it.
const canaryBasePassword = "BaseUrlPwCanary-92ac"

// TestNewRefusesBaseURLWithUserinfoQueryOrFragment: the DeepSeek API key is
// sent to the base URL, so one that embeds credentials, a query or a fragment
// (which could smuggle a second credential or redirect target) must be refused
// at construction, and the refusal must not echo the URL back.
func TestNewRefusesBaseURLWithUserinfoQueryOrFragment(t *testing.T) {
	cases := map[string]string{
		"userinfo":                "https://u:" + canaryBasePassword + "@host.invalid/anthropic",
		"userinfo no password":    "https://" + canaryBasePassword + "@host.invalid/anthropic",
		"userinfo http":           "http://u:" + canaryBasePassword + "@127.0.0.1:9/anthropic",
		"query":                   "https://host.invalid/anthropic?k=" + canaryBasePassword,
		"empty forced query":      "https://host.invalid/anthropic?",
		"fragment":                "https://host.invalid/anthropic#" + canaryBasePassword,
		"userinfo and query":      "https://u:" + canaryBasePassword + "@host.invalid/anthropic?k=v",
		"userinfo, bad scheme":    "ftp://u:" + canaryBasePassword + "@host.invalid/anthropic",
		"userinfo, whitespace ok": "  https://u:" + canaryBasePassword + "@host.invalid/anthropic  ",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(raw, testAPIKey, transport.NewStd(transport.DefaultOptions()))
			if err == nil {
				t.Fatalf("New accepted %q", raw)
			}
			if strings.Contains(err.Error(), canaryBasePassword) {
				t.Errorf("error echoed the credential text: %q", err.Error())
			}
		})
	}
}

// TestNewParseErrorDoesNotEchoBaseURLCredentials: a base URL that carries
// credentials AND fails url.Parse must not have those credentials echoed in
// New's error either (the error is printed at startup and may reach logs).
func TestNewParseErrorDoesNotEchoBaseURLCredentials(t *testing.T) {
	raw := "https://u:" + canaryBasePassword + "@host.invalid:notaport/anthropic"
	_, err := New(raw, testAPIKey, transport.NewStd(transport.DefaultOptions()))
	if err == nil {
		t.Fatalf("New accepted %q", raw)
	}
	if strings.Contains(err.Error(), canaryBasePassword) {
		t.Errorf("error echoed the credential text: %q", err.Error())
	}
}

// TestNewAcceptsPlainBaseURL guards the refusal test against passing only
// because New rejects everything.
func TestNewAcceptsPlainBaseURL(t *testing.T) {
	l, err := New("https://host.invalid/anthropic/", testAPIKey, transport.NewStd(transport.DefaultOptions()))
	if err != nil {
		t.Fatalf("New rejected a plain base URL: %v", err)
	}
	if got := l.BaseURL(); got != "https://host.invalid/anthropic" {
		t.Errorf("BaseURL=%q", got)
	}
}
