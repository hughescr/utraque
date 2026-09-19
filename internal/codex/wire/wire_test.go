package wire_test

import (
	"testing"

	"github.com/hughescr/utraque/internal/codex/wire"
)

// TestIdentityIsTheCodexCLIs pins the wire values: the backend accepts this
// exact identity, and a stray edit here would change every Codex request.
func TestIdentityIsTheCodexCLIs(t *testing.T) {
	for name, tc := range map[string]struct{ got, want string }{
		"DefaultBaseURL":   {wire.DefaultBaseURL, "https://chatgpt.com/backend-api/codex"},
		"HeaderAccountID":  {wire.HeaderAccountID, "chatgpt-account-id"},
		"HeaderOpenAIBeta": {wire.HeaderOpenAIBeta, "OpenAI-Beta"},
		"OpenAIBetaValue":  {wire.OpenAIBetaValue, "responses=experimental"},
		"HeaderOriginator": {wire.HeaderOriginator, "originator"},
		"Originator":       {wire.Originator, "codex_cli_rs"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got, tc.want)
		}
	}
}
