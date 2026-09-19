package anthropic_test

import (
	"testing"

	"github.com/hughescr/utraque/internal/anthropic"
	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/synthetic"
)

func schemaThinkingBlock(text, sig string) aschema.ContentBlock {
	return aschema.ThinkingBlock(text, sig)
}

// The codec in internal/synthetic must stay strippable by the Anthropic leg's
// cheap gate: a payload-bearing signature carries the marker, so the
// allocation-free scan sees it before any parse.
func TestPayloadBearingSignatureTripsTheGate(t *testing.T) {
	sig := synthetic.EncodeReasoningSignature("rs_1", "blob")
	if !anthropic.HasSyntheticThinking([]byte(sig)) {
		t.Error("HasSyntheticThinking did not recognise a payload-bearing signature")
	}
}

// A payload-bearing signature must still be strippable by the Anthropic leg: it
// rides on a thinking block that Anthropic never signed, so letting one through
// would be a 400 on the next Claude turn.
func TestPayloadBearingSignatureIsStillSanitized(t *testing.T) {
	sig := synthetic.EncodeReasoningSignature("rs_1", "blob")
	blk := schemaThinkingBlock("weighing it up", sig)
	if !anthropic.IsSyntheticBlock(blk) {
		t.Error("a payload-bearing thinking block was not classified as ours")
	}
}
