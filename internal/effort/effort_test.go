package effort_test

import (
	"testing"

	"github.com/hughescr/utraque/internal/effort"
)

func TestKnownAndRank(t *testing.T) {
	// The canonical order, lowest to highest. Rank is the index; every
	// recognised level is Known.
	ordered := []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.Ultra}
	for i, l := range ordered {
		if !effort.Known(l) {
			t.Errorf("Known(%q) = false, want true", l)
		}
		if got := effort.Rank(l); got != i {
			t.Errorf("Rank(%q) = %d, want %d", l, got, i)
		}
	}
	// The wire spellings must not drift: they are what the Responses API and
	// the catalog use, and what a "-<level>" model suffix spells.
	want := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	for i, l := range ordered {
		if l.String() != want[i] {
			t.Errorf("level %d spells %q, want %q", i, l, want[i])
		}
	}
}

func TestUnknownLevelRanksBelowEverything(t *testing.T) {
	for _, l := range []effort.Level{"", "hgh", "minimal", "HIGH", "High "} {
		if effort.Known(l) {
			t.Errorf("Known(%q) = true, want false", l)
		}
		if got := effort.Rank(l); got != -1 {
			t.Errorf("Rank(%q) = %d, want -1", l, got)
		}
	}
}

func TestClamp(t *testing.T) {
	all := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	cases := []struct {
		name        string
		requested   effort.Level
		supported   []string
		wantApplied effort.Level
		wantClamped bool
	}{
		{"supported verbatim", effort.Max, all, effort.Max, false},
		{"empty request", "", all, "", false},
		{"no catalog data", effort.Ultra, nil, effort.Ultra, false},
		{"clamp down to highest supported", effort.Ultra, []string{"low", "medium", "high", "xhigh"}, effort.XHigh, true},
		{"clamp down skips a gap", effort.XHigh, []string{"low", "high", "ultra"}, effort.High, true},
		{"clamp up to the floor", effort.Low, []string{"medium", "high"}, effort.Medium, true},
		// An unrecognised request the model does not support verbatim ranks
		// below the floor: it clamps DOWN to the lowest supported level rather
		// than escalating to the model's max.
		{"unknown request clamps to floor", "hgh", []string{"medium", "high", "max"}, effort.Medium, true},
		// A token upstream added that utraque does not know yet is honoured
		// verbatim when the catalog says the model supports it.
		{"unknown request supported verbatim", "minimal", []string{"minimal", "low"}, "minimal", false},
		// Unknown supported tokens are skipped, not ranked.
		{"unknown supported tokens skipped", effort.Ultra, []string{"minimal", "high"}, effort.High, true},
		{"all supported tokens unknown", effort.High, []string{"minimal", "extreme"}, effort.High, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applied, clamped := effort.Clamp(c.requested, c.supported)
			if applied != c.wantApplied || clamped != c.wantClamped {
				t.Errorf("Clamp(%q, %v) = (%q, %v), want (%q, %v)",
					c.requested, c.supported, applied, clamped, c.wantApplied, c.wantClamped)
			}
		})
	}
}

func TestSourceValues(t *testing.T) {
	// These are the "effort_source" log values; they must not drift.
	cases := map[effort.Source]string{
		effort.SourceSuffix:  "suffix",
		effort.SourceBeta:    "anthropic-beta",
		effort.SourceConfig:  "config",
		effort.SourceCatalog: "catalog",
		effort.SourceNone:    "",
	}
	for s, want := range cases {
		if s.String() != want {
			t.Errorf("Source %q renders %q, want %q", s, s.String(), want)
		}
	}
}
