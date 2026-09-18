package tokens

import (
	"math/rand/v2"
	"runtime/debug"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hughescr/utraque/internal/codex/schema"
)

// boundaryCorpus exercises the places a cut around a floored region could
// go wrong: the leading rune a letter piece may borrow, apostrophe
// contractions, the whitespace lookahead, combining marks, digits beside
// letters, multibyte text and invalid UTF-8.
var boundaryCorpus = []string{
	"",
	"hello",
	"hello world",
	"hello   world",
	"hello   \n   world",
	"  \n\n  x",
	"trailing   ",
	"   leading",
	"don't won't I'M they're we've you'll he'd it's",
	"1234567 89 0",
	"a1b22c333d4444",
	"--- === ### ///\n\n",
	"path/to/file.go:12:3",
	"tab\tsep\r\nwin\rold\n",
	"日本語のテキストと English mixed 文",
	"élève ́́ café 1́abc -́abc",
	"emoji 🎉🎉 and symbols ©®",
	"ABCdef ABC DEF abc'S abc'sdef abc's'd abc'-",
	"\xff\xfe invalid utf8 \xc0",
	"٣٤٥ arabic digits",
	// Contractions beside long runs, and the chains they can form.
	"it'saaaa x'it'saaa it'sx'llaaa 1'saaa -'saaa  'saaa 'saaa a' ' it'ſaaa",
	// Marks beside apostrophes, digits and the string's ends.
	"it'\u0301saaa \u0301'saaa 1\u0301'saaa a\u0301'saaa 1\u03012 \u03011 a\u03012\u0301b \u0301",
	"\u0301\u0301", "\u0301x", "1\u0301", "x\u0301", "'",
	// Marks before digits are their own run, never part of the digit run.
	"\u03011", "\u0301\u030112", "1\u0301\u03012", "\u0301\u030112\u0301x", "a\u0301\u03011",
	strings.Repeat("\u0301", 300) + "1",
	strings.Repeat("word ", 300),
	strings.Repeat("=", 300) + "\n" + strings.Repeat("-", 300),
	strings.Repeat("\n", 300) + "x" + strings.Repeat(" ", 300) + "y",
}

// TestPrevRunMirrorsRunEnd: walking the runs backward must reproduce the
// forward scan exactly, marks and all, or a region's backward walk could
// start inside a piece.
func TestPrevRunMirrorsRunEnd(t *testing.T) {
	for _, s := range boundaryCorpus {
		for i := 0; i < len(s); {
			c, end := runEnd(s, i)
			if pc, ps := prevRun(s, end); pc != c || ps != i {
				t.Errorf("%q: run [%d,%d) class %d, prevRun(%d) says [%d,%d) class %d", s, i, end, c, end, ps, end, pc)
			}
			i = end
		}
	}
}

// TestCountIsExactWithoutLongRuns: with nothing to floor, count is one codec
// call and must equal the codec.
func TestCountIsExactWithoutLongRuns(t *testing.T) {
	e := fresh(t, 16)
	for _, s := range boundaryCorpus {
		want, _ := e.codec.Count(s)
		if got := e.count(s); got != want {
			t.Errorf("count(%q) = %d, codec says %d", s, got, want)
		}
	}
}

// TestRegionCutsAreExact is the load-bearing property of the fast path: a cut
// at a region boundary must not change how the text on either side splits.
// With the limit forced down so that every run of a few bytes becomes a
// region, the spans between regions are the pieces around every kind of
// boundary in the corpus; each is counted by the codec on its own and must
// agree with the codec's count of the same span in place — which is
// checked through Encode's token boundaries, since a piece boundary is a
// token boundary and the codec's regex is not exported.
func TestRegionCutsAreExact(t *testing.T) {
	e := fresh(t, 16)
	for _, s := range boundaryCorpus {
		checkRegionCuts(t, e, s)
	}
}

// checkRegionCuts takes each non-digit run of s in turn as the one long run
// — with the runs before it short, as count would see them — and asserts
// the region built around it is cut where the codec cuts.
func checkRegionCuts(t *testing.T, e *O200kBase, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		return // the codec re-encodes invalid bytes; offsets differ
	}
	_, toks, err := e.codec.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	// tokensEnding[off] is how many tokens end at or before byte off, for
	// every token boundary off.
	tokensEnding := map[int]int{0: 0}
	off := 0
	for n, tok := range toks {
		off += len(tok)
		tokensEnding[off] = n + 1
	}
	prevClass, prevStart := classNone, 0
	for i := 0; i < len(s); {
		c, end := runEnd(s, i)
		if c == classDigit {
			prevClass, prevStart, i = c, i, end
			continue
		}
		ra, re := region(s, c, i, end, prevClass, prevStart)
		prevClass, prevStart, i = c, i, end
		before, okA := tokensEnding[ra]
		through, okE := tokensEnding[re]
		if !okA || !okE {
			t.Errorf("%q: region [%d,%d) %q is not token-aligned; tokens %q", s, ra, re, s[ra:re], toks)
			return
		}
		// Token-aligned is necessary; what the count relies on is that the
		// prefix before the region and the suffix after it count the same
		// on their own as they did in place. (The region itself is never
		// counted on its own — it is floored — and a whitespace region cut
		// off from the digit after it would not split the same anyway.)
		if n, _ := e.codec.Count(s[:ra]); n != before {
			t.Errorf("%q: the prefix before region [%d,%d) counts %d on its own, %d in place", s, ra, re, n, before)
		}
		if n, _ := e.codec.Count(s[re:]); n != len(toks)-through {
			t.Errorf("%q: the suffix after region [%d,%d) counts %d on its own, %d in place", s, ra, re, n, len(toks)-through)
		}
	}
}

// TestRegionCutsAreExactOnRandomText drives the same checks over thousands
// of random strings from an alphabet chosen to hit every boundary rule:
// letters of both cases, digits, apostrophes, combining marks, whitespace,
// newlines, symbols and CJK, in every adjacency.
func TestRegionCutsAreExactOnRandomText(t *testing.T) {
	shared := fresh(t, 16)
	alphabet := []string{"a", "b", "S", "L", "l", "d", "1", "7", "'", "\u0301", "\u0302", " ", "\n", "\t", "-", "/", ".", "\u00e9", "\u017f", "\u65e5", "\u672c", "\u0663", "\u3099",
		// Stems whose contractions BPE merges into one token, so a cut
		// between stem and apostrophe is not even token-aligned.
		"don", "can", "won", "I", "we", "you", "they", "t", "m", "s", "re", "ve", "ll"}
	rng := rand.New(rand.NewPCG(1, 2))
	limits := []int{1, 2, 3, 5}
	ests := make([]*O200kBase, len(limits))
	for i, limit := range limits {
		ests[i] = &O200kBase{codec: shared.codec, memo: newMemo(16), maxPiece: limit}
	}
	for n := 0; n < 4000; n++ {
		var sb strings.Builder
		for k := rng.IntN(12); k >= 0; k-- {
			sb.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		s := sb.String()
		checkRegionCuts(t, shared, s)
		want, _ := shared.codec.Count(s)
		for i, e := range ests {
			if got := e.count(s); got > want {
				t.Errorf("limit %d: count(%q) = %d exceeds the codec's %d", limits[i], s, got, want)
			}
		}
	}
}

// TestLowLimitStaysALowerBound: whatever the limit, floored regions never
// push the count over the codec's.
func TestLowLimitStaysALowerBound(t *testing.T) {
	shared := fresh(t, 16)
	for _, limit := range []int{1, 2, 3, 5, 8} {
		e := &O200kBase{codec: shared.codec, memo: newMemo(16), maxPiece: limit}
		for _, s := range boundaryCorpus {
			want, _ := e.codec.Count(s)
			if got := e.count(s); got > want {
				t.Errorf("limit %d: count(%q) = %d exceeds the codec's %d", limit, s, got, want)
			}
		}
	}
}

// TestLongRunsAreFloored: a long run is charged its floor, widened to the
// piece boundaries around it, the text beyond exactly, and the total never
// exceeds the true count.
func TestLongRunsAreFloored(t *testing.T) {
	e := fresh(t, 16)
	exact := func(s string) int {
		n, err := e.codec.Count(s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	aa := strings.Repeat("a", 4096)
	cases := []struct {
		name string
		s    string
		want int
	}{
		// "hello", then the region " aaaa…" (the space may lead the letter
		// piece, so the whole preceding run is in it), then " world".
		{"letters", "hello " + aa + " world", exact("hello") + floorTokens(4097) + exact(" world")},
		// A long non-letter run takes the letter run after it.
		{"blank lines", "x" + strings.Repeat("\n", 5000) + "yz w", exact("x") + floorTokens(5002) + exact(" w")},
		// A contraction after a long run is part of the region.
		{"contraction", "x " + aa + "'s ok", exact("x") + floorTokens(4096+1+2) + exact(" ok")},
		// Digits bound a region on both sides.
		{"digits", "12" + aa + "34", exact("12") + floorTokens(4096) + exact("34")},
		// Marks before digits are a run of their own — long, it is floored;
		// the digits after it are not swept into a digit run with it.
		{"marks before digits", strings.Repeat("\u0301", 2048) + "12", floorTokens(4096) + exact("12")},
		{"marks between digits", "7" + strings.Repeat("\u0301", 2048) + "12", exact("7") + floorTokens(4096) + exact("12")},
		// Two runs with a word between: the first run's region takes the
		// space and the word (the space could lead the word's piece), the
		// second takes its leading space.
		{"two runs", strings.Repeat("=", 2000) + " and " + strings.Repeat("-", 2000),
			floorTokens(2004) + floorTokens(2001)},
		// A contraction before the run reaches back over the letters it
		// may contract with; a chain of them reaches back over the chain.
		{"contraction before", "it's" + aa + " ok", floorTokens(4+4096) + exact(" ok")},
		{"contraction chain", "it'sx'll" + aa, floorTokens(8 + 4096)},
		// Digits stop the walk: nothing contracts with or leads from them.
		{"digits stop the walk", "9it's" + aa, exact("9") + floorTokens(4+4096)},
		// Whitespace with newlines, the regex engine's own quadratic case.
		{"mixed whitespace", "a" + strings.Repeat(" \n", 3000) + "b", exact("a") + floorTokens(6001)},
		// Invalid bytes beside a run count by their own length, a lower
		// bound on the three-byte U+FFFD they become upstream; the one
		// before the letters is in the region, the one after is the
		// codec's.
		{"invalid utf8", "\xff " + strings.Repeat("a", 3000) + " \xfe", floorTokens(3002) + exact(" \ufffd")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.count(c.s)
			if got != c.want {
				t.Errorf("count = %d, want %d", got, c.want)
			}
			if truth := exact(c.s); got > truth {
				t.Errorf("count %d exceeds the codec's %d: not a lower bound", got, truth)
			}
		})
	}
}

// TestOrdinaryTextHasNoLongRuns: prose, code and JSON never trip a region,
// so the common case stays one codec call.
func TestOrdinaryTextHasNoLongRuns(t *testing.T) {
	req := syntheticPrompt(400_000)
	v := &collectVisitor{}
	walkFields(req, v)
	for _, s := range v.fields {
		for i := 0; i < len(s); {
			c, end := runEnd(s, i)
			if end-i >= maxPieceBytes && c != classDigit {
				t.Fatalf("a %d-byte run of class %d in an ordinary field", end-i, c)
			}
			i = end
		}
	}
}

type collectVisitor struct{ fields []string }

func (c *collectVisitor) Text(s string) { c.fields = append(c.fields, s) }
func (c *collectVisitor) Image()        {}
func (c *collectVisitor) Item()         {}
func (c *collectVisitor) Tool(t *cschema.Tool) {
	c.fields = append(c.fields, t.Name, t.Description)
}

// TestLongRunCountsInBoundedTime is the regression guard for both quadratic
// paths: 400 KB of one letter took 57 s through the codec and 100 KB of
// " \n" took 4 s; each must now be a linear scan and a floor. The marks
// cases guard the digit exemption: a run of combining marks before a digit
// once classified as one digit run, bypassed the cap, and reached the codec
// as one 200 KB piece (40 KB took 565 ms, growing quadratically).
func TestLongRunCountsInBoundedTime(t *testing.T) {
	e := fresh(t, 16)
	marks := strings.Repeat("\u0301", 200_000)
	for _, s := range []string{
		strings.Repeat("a", 400_000),
		strings.Repeat("A", 400_000),
		strings.Repeat(" \n", 200_000),
		strings.Repeat("  \n", 100_000),
		strings.Repeat("-", 400_000),
		strings.Repeat("7", 400_000), // digits never form long pieces
		marks + "1",
		"1" + marks + "1",
	} {
		start := time.Now()
		got := e.count(s)
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("counting %.4q… (%d bytes) took %v", s, len(s), d)
		}
		switch {
		case s[0] == '7':
			if got < len(s)/3 {
				t.Errorf("digits: count = %d, want at least one per three", got)
			}
		case strings.HasSuffix(s, "1"):
			// The marks are floored, each digit beside them is exact.
			digits := strings.Count(s, "1")
			if want := floorTokens(len(marks)) + digits; got != want {
				t.Errorf("%.4q…: count = %d, want the floor %d plus %d digits", s, got, want, digits)
			}
		default:
			if want := floorTokens(len(s)); got != want {
				t.Errorf("%.4q…: count = %d, want the floor %d", s, got, want)
			}
		}
	}
}

// TestMaxTokenBytesMatchesVocabulary pins the module version maxTokenBytes was
// derived from, and shows the longest token really is reachable. Bumping the
// tokenizer means re-deriving the constant from the new vocabulary source.
func TestMaxTokenBytesMatchesVocabulary(t *testing.T) {
	e := fresh(t, 16)
	_, toks, err := e.codec.Encode(strings.Repeat(" ", 4*maxTokenBytes))
	if err != nil {
		t.Fatal(err)
	}
	longest := 0
	for _, tok := range toks {
		longest = max(longest, len(tok))
	}
	if longest != maxTokenBytes {
		t.Errorf("longest token in a space run is %d bytes, maxTokenBytes is %d", longest, maxTokenBytes)
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info")
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/tiktoken-go/tokenizer" {
			if dep.Version != "v0.8.1" {
				t.Errorf("tokenizer is %s; maxTokenBytes was derived from v0.8.1's vocabulary, re-derive it", dep.Version)
			}
			return
		}
	}
	t.Error("tokenizer module not in build info")
}

// BenchmarkEstimateStringUnbrokenRun is the worst case the run scan exists
// for: fields the split regex cannot cut short. "letters" and "blank" are
// one region each; "worst" is 400 KB of runs just under maxPieceBytes, so
// every one of them goes through the codec's quadratic merge and, for the
// whitespace ones, its quadratic rescan.
func BenchmarkEstimateStringUnbrokenRun(b *testing.B) {
	var worst strings.Builder
	for worst.Len() < 400_000 {
		worst.WriteString(strings.Repeat("a", maxPieceBytes-1))
		worst.WriteString(strings.Repeat(" \n", (maxPieceBytes-1)/2))
	}
	for _, c := range []struct{ name, s string }{
		{"letters", strings.Repeat("a", 400_000)},
		{"blank", strings.Repeat(" \n", 200_000)},
		{"worst", worst.String()},
	} {
		b.Run(c.name, func(b *testing.B) {
			shared := fresh(b, 16)
			b.SetBytes(int64(len(c.s)))
			for i := 0; i < b.N; i++ {
				e := &O200kBase{codec: shared.codec, memo: newMemo(16)}
				e.EstimateString(c.s)
			}
		})
	}
}
