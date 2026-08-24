package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// frames drains a Scanner over the given input into a slice, failing on a
// non-EOF error.
func frames(t *testing.T, input string) []Frame {
	t.Helper()
	sc := NewScanner(strings.NewReader(input))
	var out []Frame
	for sc.Scan() {
		f := sc.Frame()
		// Copy Data: the scanner may reuse its buffer across frames.
		out = append(out, Frame{Event: f.Event, Data: append([]byte(nil), f.Data...)})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan error: %v", err)
	}
	return out
}

func TestScanBasic(t *testing.T) {
	got := frames(t, "event: response.created\ndata: {\"id\":\"x\"}\n\n")
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1: %+v", len(got), got)
	}
	if got[0].Event != "response.created" {
		t.Errorf("event = %q", got[0].Event)
	}
	if string(got[0].Data) != `{"id":"x"}` {
		t.Errorf("data = %q", got[0].Data)
	}
}

func TestScanMultiLineData(t *testing.T) {
	got := frames(t, "data: line1\ndata: line2\ndata: line3\n\n")
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if want := "line1\nline2\nline3"; string(got[0].Data) != want {
		t.Errorf("data = %q, want %q", got[0].Data, want)
	}
}

func TestScanCRLF(t *testing.T) {
	got := frames(t, "event: e\r\ndata: d\r\n\r\n")
	if len(got) != 1 || got[0].Event != "e" || string(got[0].Data) != "d" {
		t.Fatalf("CRLF frame = %+v", got)
	}
}

func TestScanLoneCR(t *testing.T) {
	got := frames(t, "event: e\rdata: d\r\r")
	if len(got) != 1 || got[0].Event != "e" || string(got[0].Data) != "d" {
		t.Fatalf("CR frame = %+v", got)
	}
}

func TestScanComments(t *testing.T) {
	// A comment-only block dispatches nothing; a following real frame is intact.
	got := frames(t, ": this is a comment\n: another\n\ndata: real\n\n")
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1: %+v", len(got), got)
	}
	if string(got[0].Data) != "real" {
		t.Errorf("data = %q", got[0].Data)
	}
}

func TestScanNoSpaceAfterColon(t *testing.T) {
	got := frames(t, "data:no-space\n\n")
	if len(got) != 1 || string(got[0].Data) != "no-space" {
		t.Fatalf("frame = %+v", got)
	}
}

func TestScanOnlyOneLeadingSpaceStripped(t *testing.T) {
	got := frames(t, "data:  two-spaces\n\n")
	if string(got[0].Data) != " two-spaces" {
		t.Errorf("data = %q, want one leading space kept", got[0].Data)
	}
}

func TestScanEmptyDataField(t *testing.T) {
	got := frames(t, "data:\n\n")
	if len(got) != 1 || string(got[0].Data) != "" {
		t.Fatalf("frame = %+v", got)
	}
}

func TestScanFieldNoColon(t *testing.T) {
	// A bare "data" line is a field named data with empty value.
	got := frames(t, "data\n\n")
	if len(got) != 1 || string(got[0].Data) != "" {
		t.Fatalf("frame = %+v", got)
	}
}

func TestScanTrailingFrameNoBlankLine(t *testing.T) {
	// A stream that ends without the terminating blank line still yields the
	// accumulated frame — a truncated upstream must not lose its last event.
	got := frames(t, "event: e\ndata: d")
	if len(got) != 1 || got[0].Event != "e" || string(got[0].Data) != "d" {
		t.Fatalf("frame = %+v", got)
	}
}

func TestScanIDAndRetryIgnored(t *testing.T) {
	got := frames(t, "id: 42\nretry: 1000\nevent: e\ndata: d\n\n")
	if len(got) != 1 || got[0].Event != "e" || string(got[0].Data) != "d" {
		t.Fatalf("frame = %+v", got)
	}
}

func TestScanMultipleFrames(t *testing.T) {
	got := frames(t, "data: a\n\ndata: b\n\ndata: c\n\n")
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if string(got[i].Data) != want {
			t.Errorf("frame %d data = %q, want %q", i, got[i].Data, want)
		}
	}
}

func TestScanBlankLeadingLines(t *testing.T) {
	got := frames(t, "\n\n\ndata: a\n\n")
	if len(got) != 1 || string(got[0].Data) != "a" {
		t.Fatalf("frame = %+v", got)
	}
}

func TestScanLineTooLong(t *testing.T) {
	sc := NewScanner(strings.NewReader("data: " + strings.Repeat("x", 100) + "\n\n"))
	sc.SetMaxLineBytes(10)
	if sc.Scan() {
		t.Fatal("expected Scan to fail on an over-long line")
	}
	if !errors.Is(sc.Err(), ErrLineTooLong) {
		t.Errorf("err = %v, want ErrLineTooLong", sc.Err())
	}
}

// TestWriterRoundTrip asserts FrameWriter output parses back to the same frames.
func TestWriterRoundTrip(t *testing.T) {
	in := []Frame{
		{Event: "message_start", Data: []byte(`{"type":"message_start"}`)},
		{Event: "content_block_delta", Data: []byte("multi\nline\ndata")},
		{Event: "", Data: []byte("no-event")},
		{Event: "ping", Data: []byte("")},
	}
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	for _, f := range in {
		if err := fw.WriteFrame(f.Event, f.Data); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := fw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got := frames(t, buf.String())
	if len(got) != len(in) {
		t.Fatalf("round-trip got %d frames, want %d\n%s", len(got), len(in), buf.String())
	}
	for i := range in {
		if got[i].Event != in[i].Event {
			t.Errorf("frame %d event = %q, want %q", i, got[i].Event, in[i].Event)
		}
		if !bytes.Equal(got[i].Data, in[i].Data) {
			t.Errorf("frame %d data = %q, want %q", i, got[i].Data, in[i].Data)
		}
	}
}

func TestWriterFrameShape(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	if err := fw.WriteFrame("ping", []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = fw.Flush()
	if want := "event: ping\ndata: {\"type\":\"ping\"}\n\n"; buf.String() != want {
		t.Errorf("frame = %q, want %q", buf.String(), want)
	}
}

// countingFlusher records Flush calls to confirm FrameWriter forwards them.
type countingFlusher struct {
	io.Writer
	flushes int
}

func (c *countingFlusher) Flush() error { c.flushes++; return nil }

func TestWriterForwardsFlush(t *testing.T) {
	cf := &countingFlusher{Writer: &bytes.Buffer{}}
	fw := NewFrameWriter(cf)
	_ = fw.WriteFrame("e", []byte("d"))
	if err := fw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if cf.flushes != 1 {
		t.Errorf("downstream flushes = %d, want 1", cf.flushes)
	}
}

// plainFlusher has the standard library's error-less http.Flusher signature,
// which is NOT the sse.Flusher interface. NewFrameWriter must still recognise and
// forward to it, or real *http.ResponseWriter SSE frames never leave net/http's
// buffer.
type plainFlusher struct {
	io.Writer
	flushes int
}

func (c *plainFlusher) Flush() { c.flushes++ }

func TestWriterForwardsPlainHTTPFlush(t *testing.T) {
	pf := &plainFlusher{Writer: &bytes.Buffer{}}
	fw := NewFrameWriter(pf)
	_ = fw.WriteFrame("e", []byte("d"))
	if err := fw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if pf.flushes != 1 {
		t.Errorf("downstream flushes = %d, want 1", pf.flushes)
	}
}

// FuzzScanner asserts the scanner never panics and never loops forever on
// arbitrary bytes, and that whatever frames it emits re-serialise and re-parse
// to the identical frame sequence (a codec round-trip invariant).
func FuzzScanner(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"\n\n\n",
		"data: a\n\n",
		"event: e\ndata: d\n\n",
		"data: a\ndata: b\n\n",
		": comment\n\n",
		"data:no-space\n\n",
		"event: e\r\ndata: d\r\n\r\n",
		"id: 1\nretry: 5\n\n",
		"data: unterminated",
		"\r\r\r",
		"data: \x00\x01\x02\n\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		sc := NewScanner(bytes.NewReader(in))
		var got []Frame
		for sc.Scan() {
			fr := sc.Frame()
			got = append(got, Frame{Event: fr.Event, Data: append([]byte(nil), fr.Data...)})
			if len(got) > len(in)+16 {
				t.Fatalf("scanner emitted more frames (%d) than input bytes (%d): possible loop", len(got), len(in))
			}
		}
		// Err may be non-nil (e.g. an event: name containing a newline is
		// impossible, but a NUL is fine); a nil-or-not error must not panic.
		_ = sc.Err()

		// Round-trip: re-encode the emitted frames and re-parse. The second parse
		// must yield the identical frames. This catches any data payload the
		// writer cannot faithfully represent.
		var buf bytes.Buffer
		fw := NewFrameWriter(&buf)
		for _, fr := range got {
			// An event value may not contain a newline (SSE has no way to encode
			// it); the scanner can never produce one, so this is always safe.
			if strings.ContainsAny(fr.Event, "\r\n") {
				t.Fatalf("scanner produced an event with a line break: %q", fr.Event)
			}
			if err := fw.WriteFrame(fr.Event, fr.Data); err != nil {
				t.Fatalf("re-encode: %v", err)
			}
		}
		if err := fw.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		sc2 := NewScanner(&buf)
		var got2 []Frame
		for sc2.Scan() {
			fr := sc2.Frame()
			got2 = append(got2, Frame{Event: fr.Event, Data: append([]byte(nil), fr.Data...)})
		}
		if err := sc2.Err(); err != nil {
			t.Fatalf("re-parse error: %v", err)
		}
		if len(got) != len(got2) {
			t.Fatalf("round-trip frame count %d != %d", len(got), len(got2))
		}
		for i := range got {
			if got[i].Event != got2[i].Event || !bytes.Equal(got[i].Data, got2[i].Data) {
				t.Fatalf("round-trip frame %d differs:\n first: %+v\nsecond: %+v", i, got[i], got2[i])
			}
		}
	})
}
