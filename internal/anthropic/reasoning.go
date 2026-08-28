package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// This file defines how a Codex reasoning item survives a round trip through
// the client.
//
// The Responses backend caches a prompt prefix only while the replayed
// conversation matches the token sequence the model actually saw, and that
// sequence INCLUDES the reasoning items the model emitted. Replaying an
// assistant turn without them diverges at the first assistant message, which
// caps the cache hit at whatever precedes it — the instructions, the tools and
// the opening user message — for the rest of the conversation.
//
// utraque is stateless per request, so the blob has to come back from the
// client. It rides in the signature of the synthetic thinking block utraque
// already mints for every reasoning item (see SyntheticThinkingMarker): the
// client replays that block in the next turn's history, and the request
// translator turns it back into a reasoning input item. The Anthropic-leg
// sanitizer strips these blocks before they can reach Anthropic, so the
// fabricated signature is never presented to a backend that would reject it.
//
// A signature that does not carry a payload — one minted before this existed,
// or one for a reasoning item whose stream broke before its encrypted content
// arrived — decodes as "not present" and the block is dropped exactly as it
// was before. Losing a blob costs a cache miss, never correctness.

// reasoningSigTag follows the marker and identifies the payload encoding. It
// distinguishes a payload-bearing signature from the older
// "<response-id>-<output-index>" form, which carried no data and must keep
// decoding as absent rather than as a corrupt payload.
const reasoningSigTag = "r1."

// reasoningPayload is what a synthetic signature carries. The field names are
// single letters because this is encoded into every assistant turn of every
// conversation and the encrypted content is already several kilobytes.
type reasoningPayload struct {
	// ID is the reasoning item's own id ("rs_..."), replayed verbatim.
	ID string `json:"i,omitempty"`
	// Enc is the item's encrypted_content: an opaque blob the backend issues
	// only when the request asked for it via include, and the only part of a
	// reasoning item that can be replayed under store:false.
	Enc string `json:"e"`
}

// EncodeReasoningSignature builds the signature for a thinking block minted
// from a reasoning item carrying encrypted content. An empty enc returns "",
// so a caller can fall back to a payload-free signature rather than emitting an
// empty envelope.
func EncodeReasoningSignature(id, enc string) string {
	if enc == "" {
		return ""
	}
	b, err := json.Marshal(reasoningPayload{ID: id, Enc: enc})
	if err != nil {
		// The struct is two strings; Marshal cannot fail. Returning empty keeps
		// the caller on its payload-free path if it somehow does.
		return ""
	}
	return SyntheticThinkingMarker + reasoningSigTag + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeReasoningSignature recovers the reasoning item a signature carries. ok
// is false for any signature that is not one of ours, does not carry a payload,
// or does not decode — every one of which means "replay nothing", never an
// error. A payload whose encrypted content is empty is also rejected: an empty
// blob would serialise a reasoning item the backend cannot use.
func DecodeReasoningSignature(sig string) (id, enc string, ok bool) {
	rest, found := strings.CutPrefix(sig, SyntheticThinkingMarker)
	if !found {
		return "", "", false
	}
	body, found := strings.CutPrefix(rest, reasoningSigTag)
	if !found {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", "", false
	}
	var p reasoningPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", false
	}
	if p.Enc == "" {
		return "", "", false
	}
	return p.ID, p.Enc, true
}
