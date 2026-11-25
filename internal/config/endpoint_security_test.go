package config_test

import (
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/config"
)

// credentialEndpointVars are the endpoints that carry a stored or caller
// credential: the Anthropic upstream (the caller's bearer), the DeepSeek
// inference base (the API key, and the balance URL derived from it), and the
// Codex backend and OAuth token endpoints (the access and refresh tokens).
var credentialEndpointVars = []string{
	config.EnvAnthropicBaseURL,
	config.EnvDeepSeekBaseURL,
	config.EnvCodexBaseURL,
	config.EnvCodexTokenURL,
}

// TestCredentialEndpointsRequireHTTPSExceptOnLoopback pins the transport rule:
// https everywhere, plain http only when the host is a loopback IP literal or
// exactly "localhost". Nothing is resolved, and a host that merely starts with
// a loopback-looking label is not loopback.
func TestCredentialEndpointsRequireHTTPSExceptOnLoopback(t *testing.T) {
	accepted := []string{
		"https://api.example.test",
		"https://api.example.test:8443/v1",
		"https://198.51.100.7/v1",
		"http://127.0.0.1:8411",
		"http://127.1.2.3/v1",
		"http://127.255.255.255",
		"http://[::1]:8411",
		"http://localhost:8411",
		"http://LocalHost/v1",
		"HTTP://127.0.0.1:8411",
	}
	rejected := []string{
		"http://deepseek.test/anthropic",
		"http://api.example.test",
		"http://127.evil.example",
		"http://127.0.0.1.evil",
		"http://127.0.0.1.nip.io",
		"http://localhost.evil.example",
		"http://localhost.",
		"http://notlocalhost",
		"http://127.1",
		"http://0.0.0.0:8411",
		"http://192.168.1.5:8411",
		"http://[::]:8411",
		"http://[::ffff:127.0.0.1]:8411",
		"http://[::1%25lo0]:8411",
	}
	for _, name := range credentialEndpointVars {
		t.Run(name, func(t *testing.T) {
			for _, raw := range accepted {
				if _, err := config.LoadFrom(envFrom(map[string]string{"HOME": "/home/tester", name: raw})); err != nil {
					t.Errorf("%s=%s rejected: %v", name, raw, err)
				}
			}
			for _, raw := range rejected {
				_, err := config.LoadFrom(envFrom(map[string]string{"HOME": "/home/tester", name: raw}))
				if err == nil {
					t.Errorf("%s=%s accepted; plain http to a non-loopback host must be refused", name, raw)
					continue
				}
				msg := err.Error()
				if !strings.Contains(msg, name) || !strings.Contains(msg, "https://") {
					t.Errorf("%s=%s: error %q does not name the variable and the https fix", name, raw, msg)
				}
				host := strings.TrimPrefix(raw, "http://")
				if strings.Contains(msg, host) {
					t.Errorf("%s=%s: error quotes the rejected URL: %q", name, raw, msg)
				}
			}
		})
	}
}

// TestEndpointErrorsNeverQuoteTheValue covers inputs whose text can carry a
// credential that the userinfo/query/fragment redaction never sees: an opaque
// URL ("https:<secret>"), a secret typed into the port position (which
// url.Parse quotes in its own error), and other unparseable shapes. Config
// errors are printed to stderr and launchd's log, so every one must be static.
func TestEndpointErrorsNeverQuoteTheValue(t *testing.T) {
	const canary = "CANARYc0nf1g7e5t"
	inputs := []string{
		"https:" + canary,
		"https:" + canary + "@api.example.test",
		"http:" + canary,
		"https://api.example.test:" + canary + "/v1",
		"https://api.example.test:" + canary,
		"https://[" + canary,
		"https://api.example.test/%zz" + canary,
		"%zz" + canary,
		"https://api.example.test/\x7f" + canary,
		"ftp://" + canary + ".example.test",
		"mailto:" + canary + "@example.test",
		"//" + canary + ".example.test/v1",
		"https:///" + canary,
		"http://" + canary + ".example.test",
		"http://user:" + canary + "@127.0.0.1",
		"https://127.0.0.1/?" + canary,
		"https://127.0.0.1/#" + canary,
	}
	for _, name := range credentialEndpointVars {
		for _, raw := range inputs {
			_, err := config.LoadFrom(envFrom(map[string]string{"HOME": "/home/tester", name: raw}))
			if err == nil {
				t.Errorf("%s=%q accepted", name, raw)
				continue
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("%s=%q: error quotes the value: %v", name, raw, err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: error %q does not name the variable", name, raw, err)
			}
		}
	}
}
