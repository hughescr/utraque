// Package proxyhdr declares the X-Utraque-* header names once. These are
// the headers utraque itself defines on the caller-to-proxy hop: the loopback
// shared secret, the proxy-minted request id, the debug headers that say
// which leg served a request and what it was translated to, and the token
// count method. Each leg decides which of them it sets; the whole prefix
// is reserved. This package also identifies fields nominated by Connection
// so legs that copy peer headers agree on which names are hop-by-hop.
//
// None of these headers may cross the trust boundary: the token is
// utraque's own credential and the request id is minted here, so the legs
// strip them from what they forward and from what they relay back.
//
// Dependency contract: proxyhdr imports only the standard library, so any
// package in the tree may import it without creating a cycle.
package proxyhdr

import (
	"net/http"
	"strings"
)

// IsReserved reports whether name belongs to the proxy, including extensions
// unknown to this build. Test source fields before copying, not the assembled
// destination: a middleware or the leg itself may have minted headers there.
func IsReserved(name string) bool {
	const prefix = "X-Utraque-"
	return len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix)
}

// ConnectionTokens returns the canonical field names nominated by this
// message. Range over keys since middleware may supply noncanonical spellings
// alongside the canonical Connection key. The usual no-nominations case
// allocates no set.
func ConnectionTokens(h http.Header) map[string]struct{} {
	var names map[string]struct{}
	for k, values := range h {
		if !strings.EqualFold(k, "Connection") {
			continue
		}
		for _, line := range values {
			for tok := range strings.SplitSeq(line, ",") {
				tok = strings.TrimSpace(tok)
				if tok == "" {
					continue
				}
				if names == nil {
					names = make(map[string]struct{})
				}
				names[http.CanonicalHeaderKey(tok)] = struct{}{}
			}
		}
	}
	return names
}

const (
	// LocalToken carries the optional loopback shared secret. It is a
	// dedicated header so the client's Authorization header, which holds the
	// user's upstream credential, passes through untouched.
	LocalToken = "X-Utraque-Token"

	// RequestID is always set on the proxy's responses and carries the id
	// the access log records. It is distinct from X-Request-Id so a
	// passthrough response can still carry Anthropic's own X-Request-Id
	// unmodified.
	RequestID = "X-Utraque-Request-Id"

	// Route names the leg that served the request. The Codex and DeepSeek legs
	// set it; its absence on an Anthropic response is part of that leg's
	// passthrough contract.
	Route = "X-Utraque-Route"

	// Model names the upstream slug the request was translated to. The Codex
	// and DeepSeek legs set it beside Route.
	Model = "X-Utraque-Model"

	// TokenCountMethod says how a /count_tokens result was obtained when it
	// is a local estimate rather than an upstream count. The DeepSeek and
	// Codex legs set it.
	TokenCountMethod = "X-Utraque-Token-Count-Method"
)
