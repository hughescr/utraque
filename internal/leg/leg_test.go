package leg

import "testing"

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		id   ID
		want bool
	}{
		{Anthropic, true},
		{Codex, true},
		{DeepSeek, true},
		{Unknown, false},
		{"", false},
		{"openai", false},
		{"Anthropic", false},
	} {
		if got := tc.id.Valid(); got != tc.want {
			t.Errorf("ID(%q).Valid() = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestWireValues(t *testing.T) {
	// The string values are wire spellings: the provider report's "provider"
	// key, the observation's "source" key and usage history's "unknown".
	for _, tc := range []struct {
		id   ID
		want string
	}{
		{Anthropic, "anthropic"},
		{Codex, "codex"},
		{DeepSeek, "deepseek"},
		{Unknown, "unknown"},
	} {
		if got := tc.id.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.id, got, tc.want)
		}
	}
}
