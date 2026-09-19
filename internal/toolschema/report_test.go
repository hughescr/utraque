package toolschema

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"
)

// TestReportAddLabelsNodesByTool: the "<tool>.<path>" rule has one owner. A
// root-level pattern is labelled by the tool name alone, and tools are kept
// in the order they were added.
func TestReportAddLabelsNodesByTool(t *testing.T) {
	var r Report
	if !r.Empty() || r.LogAttrs() != nil {
		t.Fatal("the zero Report must be empty and log nothing")
	}
	r.Add("Artifact", Result{Rewritten: []string{"properties.file_paths.items"}})
	r.Add("Read", Result{})
	r.Add("Quoted", Result{Rewritten: []string{"properties.a", "properties.b"}, Dropped: []string{"", "properties.q"}})
	want := Report{
		Rewritten: []string{"Artifact.properties.file_paths.items", "Quoted.properties.a", "Quoted.properties.b"},
		Dropped:   []string{"Quoted", "Quoted.properties.q"},
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("report = %+v\nwant     %+v", r, want)
	}
	if r.Empty() {
		t.Error("Empty() = true for a populated report")
	}
}

// TestReportLogAttrsShape pins the two log keys and their order, rendered
// through a real JSON handler the way both legs emit them: rewritten first,
// dropped second, and neither key when its list is empty.
func TestReportLogAttrsShape(t *testing.T) {
	cases := []struct {
		name string
		r    Report
		want string
	}{
		{"both", Report{Rewritten: []string{"A.x"}, Dropped: []string{"B"}},
			`{"msg":"m","rewritten_patterns":["A.x"],"dropped_patterns":["B"]}`},
		{"rewritten only", Report{Rewritten: []string{"A.x", "A.y"}},
			`{"msg":"m","rewritten_patterns":["A.x","A.y"]}`},
		{"dropped only", Report{Dropped: []string{"B"}},
			`{"msg":"m","dropped_patterns":["B"]}`},
		{"empty", Report{}, `{"msg":"m"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
					return slog.Attr{}
				}
				return a
			}})
			slog.New(h).LogAttrs(context.Background(), slog.LevelInfo, "m", tc.r.LogAttrs()...)
			got := bytes.TrimSpace(buf.Bytes())
			if string(got) != tc.want {
				t.Errorf("log line = %s\nwant       %s", got, tc.want)
			}
			if !json.Valid(got) {
				t.Errorf("log line is not JSON: %s", got)
			}
		})
	}
}
