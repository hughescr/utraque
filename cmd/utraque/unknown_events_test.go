package main

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestUnknownEventBucketsAcrossStreams(t *testing.T) {
	o := newCodexObserver()
	o.observeUnknownEvents(nil)
	// Counts previously bucketed at the source must survive the merge.
	o.observeUnknownEvents(map[string]int{"(malformed)": 2, "(other)": 3})
	want := map[string]int{"(malformed)": 2, "(other)": 3}
	for i := range 80 {
		name := fmt.Sprintf("future.v2_%d", i)
		o.observeUnknownEvents(map[string]int{name: 2})
		if i < 62 {
			want[name] = 2
		} else {
			want["(other)"] += 2
		}
	}
	// The lifetime boundary must classify keys even if handed an unclassified map.
	o.observeUnknownEvents(map[string]int{"future.v2_0": 4, "future\nname": 5, strings.Repeat("z", 65): 6})
	want["future.v2_0"] += 4
	want["(malformed)"] += 11
	health := o.unknownEvents()
	got := health["unknown_event_types"].(map[string]int)
	if !maps.Equal(got, want) {
		t.Errorf("health labels differ: %d keys, malformed=%d, other=%d; want %d, %d, %d", len(got), got["(malformed)"], got["(other)"], len(want), want["(malformed)"], want["(other)"])
	}
	if health["unknown_events"] != 180 || health["streams_with_unknowns"] != int64(82) {
		t.Errorf("health totals: events=%v streams=%v; want 180 events in 82 affected streams", health["unknown_events"], health["streams_with_unknowns"])
	}
}
