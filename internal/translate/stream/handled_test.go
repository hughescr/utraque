package stream_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/translate/stream"
)

// The mapping table published by HandledEventTypes is what the LIVE contract
// smoke test compares a real Codex stream against. A table that had drifted
// away from the dispatch switch would make that tripwire lie in the dangerous
// direction — claiming an event type is understood when handle() actually drops
// it — so these two tests hold the list and the switch together, hermetically.

// TestHandledEventTypesMatchTheDispatchSwitch drives one frame of every listed
// type through a Translator and fails on any that the translator counts as
// unknown. Unknown-counting happens in exactly one place: handle's default
// branch. So "not counted" means "the switch has a case for it".
func TestHandledEventTypesMatchTheDispatchSwitch(t *testing.T) {
	types := stream.HandledEventTypes()
	if len(types) == 0 {
		t.Fatal("HandledEventTypes is empty; the tripwire would pass on any stream at all")
	}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			res := runOneEvent(t, typ)
			if n := res.UnknownEvents[typ]; n != 0 {
				t.Errorf("%q is in HandledEventTypes but handle() counted it as unknown %d time(s): "+
					"the mapping table and the dispatch switch have drifted apart", typ, n)
			}
			if !stream.Handles(typ) {
				t.Errorf("Handles(%q) = false for a type HandledEventTypes lists", typ)
			}
		})
	}
}

// TestAnUnlistedEventTypeIsCountedAsUnknown is the negative control: without
// it, a translator that never counted anything would satisfy the test above.
func TestAnUnlistedEventTypeIsCountedAsUnknown(t *testing.T) {
	const invented = "response.some_future_thing.delta"
	if stream.Handles(invented) {
		t.Fatalf("Handles(%q) = true for an invented type", invented)
	}
	res := runOneEvent(t, invented)
	if res.UnknownEvents[invented] == 0 {
		t.Errorf("an unlisted event type was not counted as unknown: %v", res.UnknownEvents)
	}
}

// runOneEvent plays a minimal stream — response.created, then one frame of typ —
// through a Translator and returns the Result. Errors are ignored on purpose:
// several handled types (response.failed, error) legitimately end the stream,
// and only the unknown-event counters are under test here.
func runOneEvent(t *testing.T, typ string) stream.Result {
	t.Helper()
	body := frameOf(cschema.EventResponseCreated,
		`{"type":"`+cschema.EventResponseCreated+`","response":{"id":"resp_handled"}}`) +
		frameOf(typ, fmt.Sprintf(`{"type":%q}`, typ))
	tr := stream.New(stream.Options{Model: "sol"})
	res, _ := tr.Run(context.Background(), strings.NewReader(body), &recordingSink{})
	return res
}

func frameOf(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}
