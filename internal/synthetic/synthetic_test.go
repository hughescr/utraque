package synthetic_test

import (
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/synthetic"
)

func TestReasoningSignatureRoundTrip(t *testing.T) {
	const (
		id  = "rs_68a1f0c2"
		enc = "gAAAAABmZ+not/really+encrypted//but=="
	)
	sig := synthetic.EncodeReasoningSignature(id, enc)
	if sig == "" {
		t.Fatal("encode returned empty for a payload that should encode")
	}
	// The marker has to survive encoding: it is what stops the block reaching
	// Anthropic, which signs its own thinking blocks and rejects ours.
	if !strings.HasPrefix(sig, synthetic.Marker) {
		t.Errorf("signature does not carry the synthetic marker: %q", sig)
	}

	gotID, gotEnc, ok := synthetic.DecodeReasoningSignature(sig)
	if !ok {
		t.Fatalf("decode rejected its own encoding: %q", sig)
	}
	if gotID != id || gotEnc != enc {
		t.Errorf("round trip changed the payload: got (%q, %q), want (%q, %q)", gotID, gotEnc, id, enc)
	}
}

// An encrypted blob is the whole point of the payload, so an empty one must not
// produce a signature at all — the caller falls back to the payload-free form
// rather than emitting an envelope with nothing in it.
func TestReasoningSignatureRefusesEmptyContent(t *testing.T) {
	if sig := synthetic.EncodeReasoningSignature("rs_1", ""); sig != "" {
		t.Errorf("encode produced a signature for empty content: %q", sig)
	}
}

// Every one of these means "replay nothing". None is an error: a signature we
// cannot read is a cache miss, never a failed request.
func TestReasoningSignatureRejectsWhatItCannotReplay(t *testing.T) {
	cases := []struct {
		name string
		sig  string
	}{
		{"empty", ""},
		{"genuine anthropic signature", "ErUBCkYIBRgCKkDdT8v0aGVudGhpbmc="},
		{"ours, but the payload-free legacy form", synthetic.Marker + "resp_9-0"},
		{"ours, tagged, but not base64", synthetic.Marker + "r1.!!!not-base64!!!"},
		{"ours, tagged, base64 of non-JSON", synthetic.Marker + "r1.aGVsbG8"},
		{"ours, tagged, JSON with no encrypted content", synthetic.Marker + "r1.eyJpIjoicnNfMSJ9"},
		{"payload without the marker", "r1.eyJpIjoicnNfMSIsImUiOiJ4In0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, ok := synthetic.DecodeReasoningSignature(c.sig); ok {
				t.Errorf("decode accepted %q, which carries nothing replayable", c.sig)
			}
		})
	}
}
