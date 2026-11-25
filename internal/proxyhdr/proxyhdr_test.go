package proxyhdr_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/proxyhdr"
)

// IsReserved is documented to take any spelling: callers range over raw map
// keys, which middleware can supply in any case.
func TestIsReservedIgnoresCase(t *testing.T) {
	for name, want := range map[string]bool{
		"X-Utraque-Foo":  true,
		"x-utraque-foo":  true,
		"X-UTRAQUE-FOO":  true,
		"x-UtRaQuE-":     true,
		"X-Utraque-":     true,
		"X-Utraque":      false,
		"x-utraque":      false,
		"X-Utraquex-Foo": false,
		"Y-Utraque-Foo":  false,
		"":               false,
	} {
		if got := proxyhdr.IsReserved(name); got != want {
			t.Errorf("IsReserved(%q) = %t, want %t", name, got, want)
		}
	}
}

// TestNamesAreCanonicalAndPrefixed pins the header names: they are external
// (clients and log readers match on them), so the values are restated here,
// and each must already be in canonical form so header lookups by constant
// and by canonicalised key agree.
func TestNamesAreCanonicalAndPrefixed(t *testing.T) {
	for name, tc := range map[string]struct{ got, want string }{
		"LocalToken":       {proxyhdr.LocalToken, "X-Utraque-Token"},
		"RequestID":        {proxyhdr.RequestID, "X-Utraque-Request-Id"},
		"Route":            {proxyhdr.Route, "X-Utraque-Route"},
		"Model":            {proxyhdr.Model, "X-Utraque-Model"},
		"TokenCountMethod": {proxyhdr.TokenCountMethod, "X-Utraque-Token-Count-Method"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got, tc.want)
		}
		if !strings.HasPrefix(tc.got, "X-Utraque-") {
			t.Errorf("%s = %q lacks the X-Utraque- prefix", name, tc.got)
		}
		if canon := http.CanonicalHeaderKey(tc.got); canon != tc.got {
			t.Errorf("%s = %q is not canonical (%q)", name, tc.got, canon)
		}
	}
}
