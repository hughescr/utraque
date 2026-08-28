package responses

import (
	"regexp"
	"testing"
)

var uuidShaped = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// The session id and the body's prompt_cache_key must name the SAME
// conversation. The backend is undocumented and may key cache affinity on
// either, so a request that claimed one identity in the header and another in
// the body could be routed away from its own cached prefix.
func TestSessionIDIsDerivedFromTheCacheKey(t *testing.T) {
	const key = "utq-403f0d685ee0bb48e9803f14586b1888"
	got := sessionID(key)
	if !uuidShaped.MatchString(got) {
		t.Fatalf("session id %q is not UUID-shaped; the Codex CLI sends a UUID here", got)
	}
	if got != sessionID(key) {
		t.Error("session id is not stable for one key")
	}
	if sessionID("utq-0000000000000000000000000000ffff") == got {
		t.Error("two cache keys produced the same session id")
	}
}

// Anything that is not a well-formed key yields no id, and the caller omits the
// header — which is what the leg did before the key existed.
func TestSessionIDIsEmptyForAnUnusableKey(t *testing.T) {
	for _, key := range []string{"", "utq-", "utq-tooshort", "403f0d685ee0bb48e9803f14586b1888", "utq-z03f0d685ee0bb48e9803f14586b18"} {
		if got := sessionID(key); got != "" {
			t.Errorf("sessionID(%q) = %q, want empty", key, got)
		}
	}
}
