package effort_test

import (
	"testing"

	"github.com/hughescr/utraque/internal/effort"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in     string
		want   effort.Level
		wantOK bool
	}{
		{"low", effort.Low, true},
		{"medium", effort.Medium, true},
		{"high", effort.High, true},
		{"xhigh", effort.XHigh, true},
		{"max", effort.Max, true},
		{"ultra", effort.Ultra, true},
		// Case and whitespace are forgiven: the token came off the wire.
		{"  High ", effort.High, true},
		{"MAX", effort.Max, true},
		// Anything else names no effort rather than buying some tier.
		{"", "", false},
		{"hgh", "", false},
		{"none", "", false},
		{"extreme", "", false},
	}
	for _, c := range cases {
		got, ok := effort.Parse(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("Parse(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestSourceBodyValueAndLogName(t *testing.T) {
	if effort.SourceBody.String() != "body" {
		t.Errorf("SourceBody renders %q, want body", effort.SourceBody.String())
	}
	// LogName is the effort_source value on the per-request line.
	cases := map[effort.Source]string{
		effort.SourceSuffix:  "suffix",
		effort.SourceBody:    "body",
		effort.SourceBeta:    "anthropic-beta",
		effort.SourceConfig:  "config",
		effort.SourceCatalog: "default",
		effort.SourceNone:    "default",
	}
	for s, want := range cases {
		if got := s.LogName(); got != want {
			t.Errorf("Source %q LogName = %q, want %q", s, got, want)
		}
	}
}
