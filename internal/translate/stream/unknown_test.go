package stream_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/translate/stream"
)

func TestUnknownEventBucketsAtSource(t *testing.T) {
	var body strings.Builder
	add := func(name string) {
		data, err := json.Marshal(map[string]string{"type": name})
		if err != nil {
			t.Fatal(err)
		}
		body.WriteString(frameOf("", string(data)))
	}
	want := map[string]int{}
	// Fill with well-formed names before either synthetic bucket is needed.
	for i := range 80 {
		name := fmt.Sprintf("future.v2_%d", i)
		if i == 0 {
			name = strings.Repeat("a", 64)
		}
		add(name)
		if i < 62 {
			want[name]++
		} else {
			want["(other)"]++
		}
	}
	add("future.v2_1") // still attributed after the map has filled
	want["future.v2_1"]++
	for _, name := range []string{"", "1first", "a-b", "aB", "a\nb", "é", strings.Repeat("a", 65), "(other)", "(malformed)"} {
		add(name)
		want["(malformed)"]++
	}
	body.WriteString(frameOf("bad fallback", "{}"))
	body.WriteString(frameOf("ignored", "{"))
	want["(malformed)"] += 2
	body.WriteString(frameOf("response.completed", "{}"))

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	res, err := stream.New(stream.Options{Logger: logger}).Run(t.Context(), strings.NewReader(body.String()), &recordingSink{})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(res.UnknownEvents, want) {
		t.Errorf("per-stream counts differ: %d keys, malformed=%d, other=%d; want %d, %d, %d", len(res.UnknownEvents), res.UnknownEvents["(malformed)"], res.UnknownEvents["(other)"], len(want), want["(malformed)"], want["(other)"])
	}
	// Compare decoded labels, so JSON-escaping a control character cannot hide it.
	seen := map[string]int{}
	dec := json.NewDecoder(&logs)
	for {
		var record struct{ Msg, Type string }
		err := dec.Decode(&record)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if record.Msg == "unknown Codex stream event type" {
			seen[record.Type]++
		}
	}
	if len(seen) != len(want) {
		t.Errorf("debug log has %d distinct labels, want %d", len(seen), len(want))
	}
	for name := range want {
		if seen[name] != 1 {
			t.Errorf("debug records for bucket %q = %d, want 1", name, seen[name])
		}
	}
}
