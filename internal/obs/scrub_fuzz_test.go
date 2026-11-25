package obs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"
)

const (
	writerHead = "HeadCanary8245"
	writerTail = "TailCanary7390"
)

func writerFields(middle string) []string {
	value := writerHead + middle + writerTail
	return []string{"Bearer " + value, "eyJ" + writerHead + middle + "." + writerTail,
		"sk-" + value, `{"password":"` + value + `"}`, "?access_token=" + value}
}

func TestScrubWriterStraddle(t *testing.T) {
	// Replay the original 1 MiB/8 KiB failure once per alternative. Explore
	// offsets at the new candidate cut with a small limit, including through
	// an introducer and before its token is long enough to qualify.
	const small = 256
	for i, field := range writerFields(strings.Repeat("x", 100)) {
		for _, tc := range []struct{ limit, cut, offset int }{
			{maxScrubHold, maxScrubHold + 1 - (8 << 10), 20},
			{small, small - startOverlap, 1}, {small, small - startOverlap, 5},
			{small, small - startOverlap, 20}, {small, small - startOverlap, 80},
		} {
			t.Run(fmt.Sprintf("shape%d/limit%d-cut%d", i, tc.limit, tc.offset), func(t *testing.T) {
				line := strings.Repeat(" ", tc.cut-tc.offset) + field
				line += strings.Repeat(" ", max(0, tc.limit+1-len(line)))
				var out bytes.Buffer
				w := newScrubWriter(&out)
				w.limit = tc.limit
				_, _ = w.Write([]byte(line))
				_, _ = w.Write([]byte("\npublic-after\n"))
				w.flushTail()
				if bytes.Contains(out.Bytes(), []byte(writerHead)) || bytes.Contains(out.Bytes(), []byte(writerTail)) || !bytes.Contains(out.Bytes(), []byte(Redacted)) || !strings.HasSuffix(out.String(), "\npublic-after\n") {
					t.Fatal("forced cut disclosed a private chunk or lost a control")
				}
			})
		}
	}
}

// Each uvarint chooses the next Write length minus one. An empty plan writes
// the remainder at once; any partition is representable, including byte writes.
func chunkTrace(t *testing.T, src, plan []byte, limit int) []byte {
	t.Helper()
	var out bytes.Buffer
	w := newScrubWriter(&out)
	w.limit = limit
	whole := ScrubBytes(src)
	for len(src) > 0 {
		n := len(src)
		if len(plan) > 0 {
			u, k := binary.Uvarint(plan)
			if k <= 0 {
				u, k = uint64(plan[0]), 1
			}
			plan = plan[k:]
			n = 1 + int(u%uint64(n))
		}
		if wrote, err := w.Write(src[:n]); err != nil || wrote != n || cap(w.buf) > limit {
			t.Fatalf("Write = %d, %v; buffer cap = %d, limit = %d", wrote, err, cap(w.buf), limit)
		}
		src = src[n:]
	}
	w.flushTail()
	if w.omitted {
		prefix, ok := bytes.CutSuffix(out.Bytes(), []byte(Redacted))
		if !ok || !bytes.HasPrefix(whole, prefix) {
			t.Fatal("omission did not preserve the safely scrubbed prefix")
		}
	} else if !bytes.Equal(out.Bytes(), whole) {
		t.Fatal("stream differs from whole-input scrub without an omission")
	}
	return out.Bytes()
}

func TestScrubWriterPreservesWordBoundary(t *testing.T) {
	const limit = 128
	// The cut is immediately after a word byte. Treating the carried query
	// name as beginning-of-string creates a false match that consumes Bearer
	// but leaves its credential behind, unlike the unsplit stream.
	for _, line := range []string{
		strings.Repeat("x", limit-startOverlap) + "api_key=Bearer " + writerTail + strings.Repeat(" ", 40),
		strings.Repeat("x", limit-startOverlap) + "ſk-" + writerTail + strings.Repeat(" ", limit*2),
	} {
		got := chunkTrace(t, []byte(line+"\n"), nil, limit)
		if bytes.Contains(got, []byte(writerTail)) {
			t.Fatal("a synthetic word boundary hid the actual introducer")
		}
	}
}

func TestScrubWriterRechecksAfterCut(t *testing.T) {
	const limit = 128
	first := strings.Repeat("z", limit-30) + "\"secret\": \"ab\n"
	second := "cd\"\n" + strings.Repeat("w", 40)
	next := "\npublic.\n"
	input := []byte(first + second + strings.Repeat(next, 60))
	plan := append([]byte{byte(len(first) - 1), byte(len(second) - 1)}, bytes.Repeat([]byte{byte(len(next) - 1)}, 60)...)
	if !bytes.Equal(chunkTrace(t, input, plan, limit), ScrubBytes(input)) {
		t.Fatal("closed multiline field suppressed later independent records")
	}
}

func TestScrubWriterSplitFoldedRune(t *testing.T) {
	const limit = 128
	// A folded UTF-8 letter in an unqualified JWT segment can be split by
	// the cap itself. The missing dot and payload arrive after the refill.
	for _, letter := range []string{"ſ", "K"} {
		for i := 1; i < len(letter); i++ {
			in := []byte("eyJ" + writerHead + strings.Repeat("x", limit-3-len(writerHead)-i) + letter + "." + writerTail + "\n")
			got := chunkTrace(t, in, nil, limit)
			if bytes.Contains(got, []byte(writerHead)) || bytes.Contains(got, []byte(writerTail)) {
				t.Fatal("partial UTF-8 disqualified an extendable JWT segment")
			}
		}
	}
}

func TestScrubWriterOrdinaryLongLine(t *testing.T) {
	for _, line := range []string{"the bearer of news, ", `{"schema":"secret"} `, "eyJnear-miss/", "sk-xyz ", "api_key=canary&"} {
		input := []byte(line + strings.Repeat("x", 400) + "\npublic after\n")
		if got := chunkTrace(t, input, nil, 128); !bytes.Equal(got, ScrubBytes(input)) {
			t.Fatal("a closed or disqualified introducer suppressed the remainder")
		}
	}
}

func TestScrubWriterMultilineJSON(t *testing.T) {
	for _, first := range []string{`{"password":"}`, `{"password":  "}`, `{"password":"x\"}`} {
		in := []byte(first + "\r\n" + writerTail + `"}` + "\n")
		// Flush precisely at the apparent end of the first object.
		got := chunkTrace(t, in, []byte{byte(len(first) + 1)}, 128)
		if !bytes.Equal(got, ScrubBytes(in)) || bytes.Contains(got, []byte(writerTail)) {
			t.Fatal("an opening or escaped quote before } is not a record boundary")
		}
	}
}

func TestScrubWriterUnresolvableSuffix(t *testing.T) {
	const limit = 128
	pad := strings.Repeat("x", limit*3)
	for _, field := range append(writerFields(pad), "Bearer"+strings.Repeat(" ", limit*3)+writerTail,
		`"password": "`+writerHead+pad+`\`+"\n"+writerTail+`"`) {
		in := []byte("public: " + field + "\nnext line\n")
		for _, plan := range [][]byte{nil, bytes.Repeat([]byte{0}, len(in))} {
			got := chunkTrace(t, in, plan, limit)
			if !bytes.Contains(got, []byte("public: ")) || !bytes.HasSuffix(got, []byte(Redacted)) || bytes.Contains(got, []byte(writerHead)) || bytes.Contains(got, []byte(writerTail)) {
				t.Fatal("unresolvable overlong suffix did not fail closed")
			}
		}
	}
}

func FuzzScrubWriterChunking(f *testing.F) {
	for i := range 5 {
		f.Add([]byte("arbitrary\x00\xff\nnext"), []byte{95, 0, 10, 0}, byte(i), uint16(0), uint16(60+i))
		f.Add([]byte("multi\nline"), bytes.Repeat([]byte{0}, 180), byte(i|8), uint16(0), uint16(70+i))
	}
	f.Fuzz(func(t *testing.T, data, plan []byte, shape byte, size, pad uint16) {
		limit := 96 + int(size)%1024
		middle := strings.Repeat("x", int(pad)%(3*limit))
		if shape&8 != 0 {
			middle += strings.Repeat("x", limit)
		}
		field := writerFields(middle)[int(shape)%5]
		if shape&16 != 0 {
			field = `"password"` + "\n:\n" + `"` + writerHead + middle + `\` + "\n" + writerTail + `"`
		}
		input := append([]byte(strings.Repeat("!", int(pad)%(2*limit))+" "+field+"\n"), data...)
		got := chunkTrace(t, input, plan, limit)
		for _, private := range []string{writerHead, writerTail} {
			if !bytes.Contains(data, []byte(private)) && bytes.Contains(got, []byte(private)) {
				t.Fatal("planted value survived chunking")
			}
		}
		if len(input) <= limit && !bytes.Equal(got, ScrubBytes(input)) {
			t.Fatal("bounded stream differs from scrubbing the unsplit input")
		}

		// Independently framed records, unlike an opening JSON quote or
		// Bearer separator continued on the next line. Check byte equality,
		// not just absence of a canary, including the final partial record.
		var lines, want bytes.Buffer
		for _, part := range bytes.Split(data, []byte{'\n'}) {
			for len(part) > 0 {
				n := min(len(part), (limit-40)/2)
				line := []byte("data: " + hex.EncodeToString(part[:n]) + " Bearer " + writerHead + "\n")
				lines.Write(line)
				want.Write(ScrubBytes(line))
				part = part[n:]
			}
		}
		lines.WriteString("tail sk-" + writerTail)
		want.Write(ScrubBytes([]byte("tail sk-" + writerTail)))
		if !bytes.Equal(chunkTrace(t, lines.Bytes(), plan, limit), want.Bytes()) {
			t.Fatal("independent records differ from the line-by-line oracle")
		}
	})
}

func BenchmarkTraceScrubStream(b *testing.B) {
	for _, tc := range []struct{ name, chunk string }{
		{"NoLF", strings.Repeat("x", 32<<10)},
		{"NoLFTokens", strings.Repeat("x", 32<<10) + " Bearer TestCanary1846 "},
		{"SSE", strings.Repeat("data: {\"text\":\"hello Bearer TestCanary1846\"}\n\n", 32)},
		{"Delta", strings.Repeat("event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`+"\n\n", 32)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			p := []byte(tc.chunk)
			n := (4 << 20) / len(p)
			b.SetBytes(int64(n * len(p)))
			b.ReportAllocs()
			for b.Loop() {
				w := newScrubWriter(io.Discard)
				for range n {
					_, _ = w.Write(p)
				}
				w.flushTail()
			}
		})
	}
}

// FuzzScrubCredential inserts a known credential into arbitrary surroundings.
// Printable private chunks at both ends catch partial disclosure even when a
// value is quoted or percent-encoded. Only the field name or token introducer
// needs to be literal: Scrub is a shape matcher, not a recursive decoder.
func FuzzScrubCredential(f *testing.F) {
	jsonKeys := []string{"access_token", "refresh_token", "id_token", "api_key", "client_secret", "authorization", "password", "secret"}
	queryKeys := jsonKeys[:5]
	rawStart := 3 + len(jsonKeys) + len(queryKeys)
	variants := rawStart + len(jsonKeys)
	for i := range variants {
		f.Add("debug: other=1;\x00\xff", `local/with=padding+and\slashes`, byte(i), uint32(0xa553), uint16(i))
	}
	f.Add(`{"note":"request"}`, "", byte(0), uint32(0), uint16(0))
	f.Add("?x=one&next=two", " @/#&=\t\n", byte(15), uint32(0xffff), uint16(99))
	f.Add("note", "cut", byte(rawStart), uint32(1), uint16(0))
	f.Add("note", "cut", byte(rawStart), uint32(2), uint16(2))
	f.Fuzz(func(t *testing.T, surrounding, value string, variant byte, casing uint32, pos uint16) {
		sum := sha256.Sum256([]byte(value))
		chunks := []string{hex.EncodeToString(sum[:6]), hex.EncodeToString(sum[6:12])}
		variant = byte(int(variant) % variants)
		mixed := func(key string) string {
			b := []byte(key)
			for i, c := range b {
				if c >= 'a' && c <= 'z' && casing&(1<<i) != 0 {
					b[i] -= 'a' - 'A'
				}
			}
			return string(b)
		}
		// Map only out-of-alphabet bytes, so realistic seeds keep their
		// punctuation and the fuzzer controls the interior of each token.
		token := func(extra string) string {
			return strings.Map(func(c rune) rune {
				if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(extra, c) {
					return c
				}
				return '-'
			}, value)
		}
		var field, jsonKey string
		switch {
		case variant == 0:
			field = mixed("bearer") + "\t" + chunks[0] + token("._~+/=-") + chunks[1]
		case variant == 1:
			field = "eyJ" + chunks[0] + "." + chunks[1] + "." + token("_-")
		case variant == 2:
			field = "sk-" + chunks[0] + token("._-") + chunks[1]
		case int(variant)-3 < len(jsonKeys):
			quoted, err := json.Marshal(chunks[0] + value + chunks[1])
			if err != nil {
				t.Fatal(err)
			}
			jsonKey = mixed(jsonKeys[variant-3])
			space := []string{"", " ", "\r\n", "\t"}
			field = `"` + jsonKey + `"` + space[casing%4] + ":" + space[pos%4] + string(quoted)
		case int(variant) < rawStart:
			field = mixed(queryKeys[int(variant)-3-len(jsonKeys)]) + "=" + url.QueryEscape(chunks[0]+value+chunks[1])
		default:
			// Not marshalled: even malformed escapes, including a literal LF,
			// must not make a credential visible in prose or truncated logs.
			control := []byte{'\n', 0, '\r', '\t'}[pos%4]
			ending := []string{`" end`, ``, `\`}[casing%3]
			field = `"` + mixed(jsonKeys[int(variant)-rawStart]) + `": "` + chunks[0] + token("-") + `\` + string(control) + chunks[1] + ending
		}
		// An explicit non-word boundary before each token is part of the
		// documented grammar. '&' also terminates a query value.
		cut := int(pos) % (len(surrounding) + 1)
		n := 1 + int(pos)%3
		var in string
		if jsonKey != "" {
			// A JSON field should really be a field, not text inside an
			// unrelated unterminated string in the fuzzed prefix.
			before, _ := json.Marshal(surrounding[:cut])
			after, _ := json.Marshal(surrounding[cut:])
			in = `{"before":` + string(before) + strings.Repeat(","+field, n) + `,"after":` + string(after) + "}"
		} else if int(variant) >= rawStart {
			in = surrounding[:cut] + " & " + field // a truncated shape must really end at EOF
		} else {
			in = surrounding[:cut] + strings.Repeat("&"+field, n) + "&" + surrounding[cut:]
		}
		got := Scrub(in)
		if string(ScrubBytes([]byte(in))) != got {
			t.Fatal("byte and string scrubbers disagree on credential")
		}
		for _, chunk := range chunks {
			if !strings.Contains(surrounding, chunk) && strings.Contains(got, chunk) {
				t.Fatalf("credential variant %d retained a private chunk", variant)
			}
		}
		if jsonKey != "" {
			var fields map[string]string
			if json.Unmarshal([]byte(got), &fields) != nil || fields[jsonKey] != Redacted {
				t.Fatal("scrubbed object lost its JSON field or redaction marker")
			}
		}
	})
}
