package config_test

import (
	"net/url"
	"testing"

	"github.com/hughescr/utraque/internal/config"
)

// FuzzRedactURL checks what may survive, rather than blacklisting particular
// query names. Paths and opaque scheme data are not credential fields.
func FuzzRedactURL(f *testing.F) {
	for _, seed := range []string{
		"", "https://example.invalid/v1?", "//canary%40name:p%2Fass@example.invalid/a%2Fb#note",
		"https://example.invalid/?key=first&key=second", "./relative?key=value#fragment",
		"opaque:some-data?query", "https://canary:password@[bad", "/%zz", "\x00\xff",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := config.RedactURL(raw)
		u, err := url.Parse(raw)
		if err != nil {
			if got != "invalid-url" {
				t.Fatal("unparseable URL did not produce the invalid-url sentinel")
			}
		} else {
			public := url.URL{Scheme: u.Scheme, Opaque: u.Opaque, OmitHost: u.OmitHost, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
			if got != public.String() {
				t.Fatal("redacted URL differs from public components")
			}
		}

		// Even mutations that do not themselves parse exercise all four
		// credential-bearing components, including encoded delimiters.
		const marker = "OnlyInCredential9f7b362d"
		secret := marker + raw
		withCreds := url.URL{
			Scheme: "https", Host: "example.invalid", Path: "/v1",
			User: url.UserPassword(secret, secret), RawQuery: url.Values{"arbitrary": {secret, secret}}.Encode(), Fragment: secret,
		}
		if got := config.RedactURL(withCreds.String()); got != "https://example.invalid/v1" {
			t.Fatal("URL retained credentials or lost its public endpoint")
		}
	})
}
