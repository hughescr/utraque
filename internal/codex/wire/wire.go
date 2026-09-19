// Package wire declares the Codex CLI wire identity once: the backend root
// and the headers that make a request look like the Codex CLI's own, which
// the ChatGPT backend requires. The catalog client and the responses client
// both send exactly this set; keeping it here means the two can never drift
// apart, and nothing else in the tree needs to restate a value to describe
// it.
//
// The identity is honest: it is what the Codex CLI itself sends. No package
// invents a browser User-Agent or any other header on top of it.
//
// Dependency contract: wire imports only the standard library, so any
// package in the tree may import it without creating a cycle.
package wire

const (
	// DefaultBaseURL is the undocumented Codex backend root the Codex CLI
	// itself uses, for both the model catalog ("/models") and "/responses".
	// Clients accept an override so tests can aim at an httptest server; the
	// real host is never contacted by the test suite.
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"

	// HeaderAccountID names the header carrying the ChatGPT account id from
	// the Codex credential.
	HeaderAccountID = "chatgpt-account-id"

	// HeaderOpenAIBeta names the beta-features header; OpenAIBetaValue is
	// the value the Codex CLI sends on every backend request.
	HeaderOpenAIBeta = "OpenAI-Beta"
	// OpenAIBetaValue is the beta-features value that enables the Responses
	// API on the backend.
	OpenAIBetaValue = "responses=experimental"

	// HeaderOriginator names the client-identity header; Originator is the
	// Codex CLI's own identity, which utraque sends unchanged.
	HeaderOriginator = "originator"
	// Originator is the client identity the Codex CLI sends and utraque
	// forwards as its own. It is the only identity the Codex leg ever
	// presents; a transport switch changes the TLS handshake, never this.
	Originator = "codex_cli_rs"
)
