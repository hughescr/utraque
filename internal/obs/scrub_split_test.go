package obs

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

// splitCanary is the value-shaped text the generated inputs put where a secret
// goes. A chunked write may emit it only where scrubbing the whole input does.
const splitCanary = "K9q"

// splitVocab holds pieces per scrub alternative: Bearer, JWT, sk- keys, JSON
// secret fields, and query parameters.
var splitVocab = [][]string{
	{"Bearer", "bearer", "BEARER", " ", "  ", "\t", "\n", "\r\n", "abcd", "K9qK9qK9q", "=", "x", "!", "-", ".", "~"},
	{"eyJ", "K9qK9qK9q", "abcdefgh", ".", "_", "-", "\n", " ", "x", "eyJK9qK9q.K9qK9qK9q"},
	{"sk-", "SK-", "ſk-", "K9qK9qK9qK9q", "abc", ".", "_", " ", "\n", "x", "sk"},
	{`"password"`, `"secret"`, `"authorization"`, `"access_token"`, `"Password"`, ":", " ", "\n", "\r\n", `"`, `\`, `\"`, "K9qK9q", "}", "{", ",", "x", "é"},
	{"access_token=", "api_key=", "client_secret=", "Refresh_Token=", "&", "'", "\"", " ", "\n", "K9qK9q", "x", "=", "?"},
}

func splitGen(r *rand.Rand, alt, pieces int) []byte {
	var b strings.Builder
	v := splitVocab[alt]
	for range pieces {
		if r.IntN(8) == 0 { // noise from another alternative
			o := splitVocab[r.IntN(len(splitVocab))]
			b.WriteString(o[r.IntN(len(o))])
			continue
		}
		b.WriteString(v[r.IntN(len(v))])
	}
	return []byte(b.String())
}

// splitRun writes src in the pieces splits describe, then flushes the tail.
func splitRun(src []byte, splits []int, limit int) ([]byte, bool) {
	var out bytes.Buffer
	w := newScrubWriter(&out)
	w.limit = limit
	prev := 0
	for _, s := range append(splits, len(src)) {
		s = min(s, len(src))
		if s < prev {
			continue
		}
		_, _ = w.Write(src[prev:s])
		prev = s
	}
	w.flushTail()
	return out.Bytes(), w.omitted
}

// splitCheck holds the writer to its contract: however src is chunked, the
// output equals scrubbing src whole, or, once an unresolvable suffix forces
// omission, a prefix of that followed by the marker and nothing private.
func splitCheck(t *testing.T, src []byte, splits []int, limit int) {
	t.Helper()
	whole := ScrubBytes(src)
	got, omitted := splitRun(src, splits, limit)
	if !omitted {
		if !bytes.Equal(got, whole) {
			t.Fatalf("limit=%d splits=%v\nsrc  =%q\nwhole=%q\ngot  =%q", limit, splits, src, whole, got)
		}
		return
	}
	prefix, ok := bytes.CutSuffix(got, []byte(Redacted))
	if !ok || !bytes.HasPrefix(whole, prefix) {
		t.Fatalf("omitted, but output is not a prefix of whole + marker\nlimit=%d splits=%v\nsrc  =%q\nwhole=%q\ngot  =%q", limit, splits, src, whole, got)
	}
	if !bytes.Contains(whole, []byte(splitCanary)) && bytes.Contains(got, []byte(splitCanary)) {
		t.Fatal("a chunked write disclosed what a whole scrub hides")
	}
}

// Short inputs, small hold limits (so cuts happen), every single split point
// and byte-at-a-time.
func TestScrubWriterEverySplitPoint(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for alt := range splitVocab {
		for range 400 {
			src := splitGen(r, alt, 2+r.IntN(10))
			limit := startOverlap + 1 + r.IntN(40)
			splitCheck(t, src, nil, limit)
			all := make([]int, 0, len(src))
			for i := 1; i < len(src); i++ {
				splitCheck(t, src, []int{i}, limit)
				all = append(all, i)
			}
			splitCheck(t, src, all, limit)
		}
	}
}

func TestScrubWriterRandomLongChunks(t *testing.T) {
	n := 3000
	if testing.Short() {
		n = 300
	}
	r := rand.New(rand.NewPCG(3, 5))
	for alt := range splitVocab {
		for range n {
			src := splitGen(r, alt, 20+r.IntN(200))
			limit := startOverlap + 1 + r.IntN(300)
			var splits []int
			for p := 0; p < len(src); {
				p += 1 + r.IntN(1+r.IntN(2*limit))
				splits = append(splits, p)
			}
			splitCheck(t, src, splits, limit)
		}
	}
}

// A tail that never ends in a newline, after a cut that kept a synthetic
// word-context byte, is flushed without that byte.
func TestScrubWriterFlushTailAfterCut(t *testing.T) {
	src := []byte(strings.Repeat("word", 40)) // word characters only, no LF
	got, omitted := splitRun(src, []int{len(src) - 3}, startOverlap+8)
	if omitted || !bytes.Equal(got, ScrubBytes(src)) {
		t.Fatalf("got %q, want %q", got, ScrubBytes(src))
	}
}

func TestScrubWriterOmissionIsTerminal(t *testing.T) {
	var out bytes.Buffer
	w := newScrubWriter(&out)
	w.limit = startOverlap + 10
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	_, _ = w.Write([]byte("hello\n"))
	w.flushTail()
	_, _ = w.Write([]byte(`"password": "` + strings.Repeat(splitCanary, 200)))
	if !w.omitted {
		t.Fatal("expected omission")
	}
	before := out.Len()
	_, _ = w.Write([]byte("\"\nclean line\n"))
	w.flushTail()
	if out.Len() != before {
		t.Fatalf("omitted state was exited: %q", out.String())
	}
}

type failNext struct {
	buf   bytes.Buffer
	fails int
}

func (f *failNext) Write(p []byte) (int, error) {
	if f.fails > 0 {
		f.fails--
		return 0, errors.New("disk full")
	}
	return f.buf.Write(p)
}

// A failed write may have dropped the bytes that introduced a secret. The
// writer then emits nothing more, so a later write that succeeds cannot put
// the secret's continuation on disk with no introducer before it.
func TestScrubWriterFailsClosedAfterWriteError(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"held-record", "partial ", "rest\nnext\n"},
		{"multiline-json", `{"password":` + "\n", `"K9qK9q"}` + "\nnext\n"},
		{"tail-dropped-held", "a", "b\n" + `"password": "K9qK9q`},
		{"tail-dropped-empty", "", "b\n" + `"password": "K9qK9q`},
		{"bearer-dropped", "a", "b\nAuthorization: Bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &failNext{}
			w := newScrubWriter(f)
			_, _ = w.Write([]byte(tc.first))
			f.fails = 1
			if _, err := w.Write([]byte(tc.second)); err == nil || f.fails != 0 {
				t.Fatalf("the failing write was not reported (err %v)", err)
			}
			before := f.buf.Len()
			_, _ = w.Write([]byte(" K9qK9qK9qK9q\"}\nafter\n"))
			w.flushTail()
			if f.buf.Len() != before {
				t.Errorf("wrote %q after a failed write", f.buf.String()[before:])
			}
			if bytes.Contains(f.buf.Bytes(), []byte(splitCanary)) {
				t.Errorf("secret tail disclosed after a failed trace write: %q", f.buf.String())
			}
		})
	}
}

func BenchmarkScrubWriterThroughput(b *testing.B) {
	const total = 4 << 20
	for _, tc := range []struct{ name, chunk string }{
		{"NoLF-plain-32K", strings.Repeat("x", 32<<10)},
		{"NoLF-bearer-dense-4K", strings.Repeat("y Bearer abcdefghij ", 4<<10/20)},
		{"NoLF-openjson-4K", `"password":"` + strings.Repeat("x", 4<<10-12)},
		{"SSE-delta", strings.Repeat("event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`+"\n\n", 32)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			chunk := []byte(tc.chunk)
			b.SetBytes(total)
			for b.Loop() {
				w := newScrubWriter(io.Discard)
				for range total / len(chunk) {
					_, _ = w.Write(chunk)
				}
				w.flushTail()
			}
		})
	}
}
