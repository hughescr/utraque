package publicdetail

import "testing"

func TestReplace(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		values           []string
	}{
		{"end fragment", "bad abcdefghijklmnop", "bad [X]", []string{"abcdefghijklmnopQR"}},
		{"short fragment", "bad abcdefghijklmno", "bad abcdefghijklmno", []string{"abcdefghijklmnopQR"}},
		{"overlapping full value and suffix", "abcdefghijklmnopabcdefghijklmnopabcdefghijklmnop", "[X][X]", []string{"abcdefghijklmnopabcdefghijklmnop"}},
		{"full and fragment", "abcdefghijklmnopQR:abcdefghijklmnopQ", "[X]:[X]", []string{"abcdefghijklmnopQR"}},
		{"middle fragment", "abcdefghijklmnop!", "abcdefghijklmnop!", []string{"abcdefghijklmnopQR"}},
		{"plain and repeated", "key-one/key-two/key-one", "[X]/[X]/[X]", []string{"key-one", "key-two"}},
		{"skip tiny values", "a to cat", "a to cat", []string{"", "a", "to", "cat"}},
		{"contained value", "key-overlap/key-", "[X]/[X]", []string{"key-", "key-overlap"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Replace(tc.text, "[X]", tc.values...); got != tc.want {
				t.Errorf("Replace = %q, want %q", got, tc.want)
			}
		})
	}
}
