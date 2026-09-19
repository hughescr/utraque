package models_test

import (
	"testing"

	"github.com/hughescr/utraque/internal/deepseek/models"
)

func TestCanonical(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wantID string
		wantOK bool
	}{
		{"deepseek-flash", "deepseek-flash", true},
		{"deepseek-v4-pro", "deepseek-v4-pro", true},
		{"deepseek-v4-flash", "deepseek-flash", true},
		{"deepseek-v4-flash-vision-exp", "deepseek-flash", true},
		{"deepseek-chat", "", false},
		{"DeepSeek-Flash", "", false}, // byte-exact: callers lowercase first
		{"", "", false},
	} {
		id, ok := models.Canonical(tc.name)
		if id != tc.wantID || ok != tc.wantOK {
			t.Errorf("Canonical(%q) = %q, %v; want %q, %v", tc.name, id, ok, tc.wantID, tc.wantOK)
		}
	}
}

func TestLookupCapabilities(t *testing.T) {
	flash, ok := models.Lookup("deepseek-v4-flash-vision-exp")
	if !ok || flash.ID != "deepseek-flash" || !flash.SupportsImages {
		t.Errorf("Lookup(vision alias) = %+v, %v; want deepseek-flash with image support", flash, ok)
	}
	pro, ok := models.Lookup("deepseek-v4-pro")
	if !ok || pro.SupportsImages {
		t.Errorf("Lookup(deepseek-v4-pro) = %+v, %v; want a model without image support", pro, ok)
	}
	if _, ok := models.Lookup("nope"); ok {
		t.Error("Lookup(nope) reported a match")
	}
}

// TestModelsListingIsStable pins the listing order and the strings the picker
// rows, the 404 body and the response-model check are built from.
func TestModelsListingIsStable(t *testing.T) {
	got := models.Models()
	want := []struct{ id, display string }{
		{"deepseek-flash", "DeepSeek V4.1 Flash"},
		{"deepseek-v4-pro", "DeepSeek V4 Pro 0813"},
	}
	if len(got) != len(want) {
		t.Fatalf("Models() = %+v, want %d entries", got, len(want))
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].DisplayName != w.display {
			t.Errorf("Models()[%d] = %+v, want id %q display %q", i, got[i], w.id, w.display)
		}
	}
}

func TestModelsReturnsACopy(t *testing.T) {
	first := models.Models()
	first[0].ID = "mutated"
	first[0].Aliases[0] = "mutated-alias"
	if again := models.Models(); again[0].ID != "deepseek-flash" || again[0].Aliases[0] != "deepseek-v4-flash" {
		t.Errorf("Models() shares state with an earlier result: %+v", again[0])
	}
}

// TestNamesAreUnique guards the catalog against an alias that shadows an id
// or is listed twice, which would make Canonical order-dependent.
func TestNamesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, m := range models.Models() {
		for _, name := range append([]string{m.ID}, m.Aliases...) {
			if prev, dup := seen[name]; dup {
				t.Errorf("%q listed under both %q and %q", name, prev, m.ID)
			}
			seen[name] = m.ID
		}
		if m.DisplayName == "" {
			t.Errorf("%q has no display name", m.ID)
		}
	}
}
