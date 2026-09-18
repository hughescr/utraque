package tokens

import (
	"fmt"
	"hash/maphash"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer/codec"

	"github.com/hughescr/utraque/internal/codex/schema"
)

// O200kName is what the exact estimator reports as its Name.
const O200kName = "o200k_base"

// O200kBase is the exact Estimator: github.com/tiktoken-go/tokenizer's
// o200k_base codec, whose vocabulary is compiled into the binary (a Go map
// literal generated from the published encoding), so it works offline and
// contacts nothing at runtime.
//
// Every string field is counted separately and summed, with no framing
// overheads and every opaque blob skipped — the lower-bound rule in the
// package comment. Per-field counts are memoized (see memo), because an
// agentic loop resends the same history on every turn: a 200k-token prompt
// is tokenized from scratch once, and each later turn costs only the hash of
// the fields it repeats plus the tokenization of the ones it adds.
type O200kBase struct {
	codec *codec.Codec
	memo  *memo
	// maxPiece overrides maxPieceBytes when > 0, for tests.
	maxPiece int
}

var _ RequestEstimator = (*O200kBase)(nil)

// The codec is built once per process: codec.NewO200kBase compiles the split
// regexp on every call, so building it per Codex() call would be a needless
// few milliseconds on every construction site — and every test.
//
// The codec subpackage is used directly rather than tokenizer.Get because Get
// references every encoding the module ships, and the linker then keeps all
// five vocabularies (~12 MB of binary) for the one this leg needs (~7 MB).
var (
	o200kOnce  sync.Once
	o200kCodec *codec.Codec
	o200kErr   error
	o200kMemo  = newMemo(memoGenerationEntries)
)

// O200k returns the shared exact estimator, or the tokenizer's initialisation
// error. Callers that want the fallback applied for them use Codex.
func O200k() (*O200kBase, error) {
	o200kOnce.Do(func() {
		defer func() {
			// The vocabulary and regexp are compiled in, so construction cannot
			// fail at runtime short of a build defect; a panic there is turned
			// into the error the fallback path expects.
			if r := recover(); r != nil {
				o200kErr = fmt.Errorf("utraque/tokens: o200k_base tokenizer: %v", r)
			}
		}()
		o200kCodec = codec.NewO200kBase()
	})
	if o200kErr != nil {
		return nil, o200kErr
	}
	return &O200kBase{codec: o200kCodec, memo: o200kMemo}, nil
}

// Name reports the encoding.
func (e *O200kBase) Name() string { return O200kName }

// EstimateString counts the tokens in one string: exactly, except that a run
// of one rune class maxPieceBytes or longer is charged its floor (see count).
// It goes through the memo like every field, so a caller counting the same
// text repeatedly pays for the tokenization once.
func (e *O200kBase) EstimateString(s string) int {
	if s == "" {
		return 0
	}
	if n, ok := e.memo.get(s); ok {
		return n
	}
	n := e.count(s)
	e.memo.put(s, n)
	return n
}

// count tokenizes s with no memoization, in time linear in len(s).
//
// The codec is quadratic in two places, both per "piece" of the o200k split
// regex. Its byte-pair merge rescans and compacts a piece's part list on
// every merge, so one unbroken run of letters or symbols costs the square of
// its length (100 KB of "a": 3.6 s; 400 KB: 57 s). And its regex engine, on
// a region of mixed spaces and newlines, rescans the whole remaining region
// for every two-byte piece it cuts (100 KB of " \n": 4 s). Ordinary text
// never notices: the regex cuts prose, code and JSON into words, short digit
// groups and punctuation runs, and nothing long survives. But a tool result
// can carry a block of thousands of blank lines, a kilobyte of one repeated
// symbol or a file with no line breaks at all, and message_start waited on
// every one of them.
//
// So the string is scanned once for runs of a single rune class — letters,
// digits, everything else — at least maxPieceBytes long. A run that long is
// exactly the input the codec cannot bound, so it is not given one: the run,
// widened to the nearest positions that are provably piece boundaries (see
// region), is charged the floor its byte length implies, and the text
// between such regions is handed to the codec in whole spans, which it counts
// exactly as it would have in place. A string with no long run — every
// string an ordinary session sends — is one codec call, as before.
func (e *O200kBase) count(s string) int {
	limit := e.maxPiece
	if limit <= 0 {
		limit = maxPieceBytes
	}
	total, spanStart := 0, 0
	prevClass, prevStart := classNone, 0
	for i := 0; i < len(s); {
		c, end := runEnd(s, i)
		if end-i < limit || c == classDigit {
			prevClass, prevStart, i = c, i, end
			continue
		}
		ra, re := region(s, c, i, end, prevClass, prevStart)
		// The previous region ended at a cut the backward walk cannot
		// cross, so ra >= spanStart; the clamp is insurance against a
		// slice panic costing a request should that reasoning ever slip.
		ra = max(ra, spanStart)
		total += e.countWhole(s[spanStart:ra]) + floorTokens(re-ra)
		spanStart, i = re, re
		// re is a boundary no piece crosses, so whatever follows starts
		// fresh there: no earlier run can lend it a leading rune.
		prevClass = classNone
	}
	return total + e.countWhole(s[spanStart:])
}

// region widens the long run [a, b) of class c to [ra, re), the smallest
// enclosing byte range whose ends the split regex is guaranteed to cut at,
// so the pieces inside it are whole, and the spans on either side of it
// split exactly as they did with it in place.
//
// A piece stays inside one class run — a run of letters, a run of one to
// three digits, or a run of anything else (punctuation, symbols, whitespace,
// newlines) — with two exceptions, both in the letter alternatives:
// `[^\r\n\p{L}\p{N}]?` lets a letter piece begin with ONE rune of the run
// before it, and `(?i:'s|'t|'re|'ve|'m|'ll|'d)?` lets it end with an
// apostrophe and up to two letters of the run after that — which leaves the
// rest of that letter run to start a new piece two letters in, so with a
// lone apostrophe between two letter runs NEITHER run boundary is a cut.
//
// Forward from a long run, then: a non-letter run takes the letter run after
// it (the lead rune), and any run takes every apostrophe-plus-letters that
// follows (contractions, however many chain). The end of a letter run not
// followed by an apostrophe, and the end of a non-letter run not followed by
// letters, are cuts the regex must make.
//
// Backward from a long LETTER run: its lead may be the last rune of the
// preceding non-letter run, so that run is taken whole — and if that run is
// a lone apostrophe after letters, it may be a contraction of them, so the
// letter run before it is taken too, and the walk repeats. It stops at a
// non-letter run that is not a lone apostrophe, at a digit run (digits lead
// nothing and contract with nothing), or at the start. A long non-letter run
// of two or more runes starts at a cut already: its first rune is followed
// by more of the run, so it can neither lead a letter piece nor contract
// with the letters before it. A lone apostrophe — a run the default limit
// never treats as long, but a small one does — gets the same walk. Only the
// immediately preceding run is known to the caller; the walk finds the rest
// with prevRun.
func region(s string, c, a, b, prevClass, prevStart int) (ra, re int) {
	ra, re = a, b
	switch {
	case c == classLetter && prevClass == classOther:
		ra = contractionStart(s, prevStart, a)
	case c == classOther && b == a+1 && s[a] == '\'' && prevClass == classLetter:
		// The run is a lone apostrophe: it may be contracting the letters
		// before it, and it takes the letters after it below.
		ra = contractionStart(s, a, b)
	}
	if c != classLetter {
		if nc, ne := runEnd(s, re); nc == classLetter {
			re = ne
		}
	}
	for re < len(s) && s[re] == '\'' {
		nc, ne := runEnd(s, re+1)
		if nc != classLetter {
			break
		}
		re = ne
	}
	return ra, re
}

// contractionStart walks back from the non-letter run starting at ra and
// followed by the letter run starting at letters: while that run is a lone
// apostrophe after letters, the piece may reach back over those letters and
// whatever leads them, so the walk takes the letter run and the non-letter
// run before it and looks again. It returns the first start that is a cut.
func contractionStart(s string, ra, letters int) int {
	for ra > 0 && letters == ra+1 && s[ra] == '\'' {
		lc, ls := prevRun(s, ra)
		if lc != classLetter {
			break // after digits or nothing: the apostrophe leads
		}
		ra = ls
		if ls == 0 {
			break
		}
		pc, ps := prevRun(s, ls)
		if pc != classOther {
			break // digits before the letters: a cut at ls
		}
		ra, letters = ps, ls
	}
	return ra
}

// Rune classes for the run scan. A run is a maximal sequence of runes of one
// class; combining marks (\p{M}) extend a letter or symbol run rather than
// starting one, as they do in the regex, but never join a digit run in
// either direction, since the digit alternative admits digits only: marks
// after a digit open the run that follows them, and marks before a digit
// are a symbol run of their own (the regex takes them as one letter-class
// piece, then the digits as theirs). That last rule matters for count's
// digit exemption: a digit run must hold nothing but digits, or the marks
// hidden inside it would reach the codec as one unbounded piece.
const (
	classNone = iota
	classLetter
	classDigit
	classOther
	classMark // prevRun only; runEnd folds marks into the run they extend
)

// runEnd classifies the run starting at byte i of s and returns its class
// and the byte offset just past it. i must be less than len(s). ASCII takes
// a table-free fast path; everything else is decoded and classified by the
// same Unicode categories the regex uses (\p{L}, \p{N}, \p{M}). An invalid
// byte classifies as the U+FFFD the regex engine (and json.Marshal, on the
// way upstream) turns it into: a symbol.
func runEnd(s string, i int) (class, end int) {
	class = classNone
	for end = i; end < len(s); {
		var c int
		size := 1
		if b := s[end]; b < utf8.RuneSelf {
			switch {
			case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z':
				c = classLetter
			case '0' <= b && b <= '9':
				c = classDigit
			default:
				c = classOther
			}
		} else {
			var r rune
			r, size = utf8.DecodeRuneInString(s[end:])
			switch {
			case unicode.IsLetter(r):
				c = classLetter
			case unicode.IsNumber(r):
				c = classDigit
			case unicode.IsMark(r) && class != classDigit:
				c = class // extends whatever run this is, or opens one
			default:
				c = classOther
			}
		}
		if c != class && class != classNone {
			break
		}
		if c == classDigit && end > i && class == classNone {
			break // marks so far: they do not open a digit run
		}
		class = c
		end += size
	}
	if class == classNone {
		class = classOther // only marks: the symbol alternative takes them
	}
	return class, end
}

// classOf is runEnd's classification of one rune, with marks as their own
// class so a backward walk can decide whose run they are in.
func classOf(r rune) int {
	switch {
	case unicode.IsLetter(r):
		return classLetter
	case unicode.IsNumber(r):
		return classDigit
	case unicode.IsMark(r):
		return classMark
	default:
		return classOther
	}
}

// lastRune decodes the rune ending at byte end of s, and its class; end
// must be positive.
func lastRune(s string, end int) (class, size int) {
	r, size := utf8.DecodeLastRuneInString(s[:end])
	return classOf(r), size
}

// marksBefore returns the byte offset of the first of the run of combining
// marks ending at end (end itself if there are none).
func marksBefore(s string, end int) int {
	for end > 0 {
		c, size := lastRune(s, end)
		if c != classMark {
			break
		}
		end -= size
	}
	return end
}

// prevRun is runEnd's mirror: the class and start of the run that ends at
// byte end of s, which must be a run boundary (end > 0). It follows runEnd's
// rules for marks exactly — a mark extends the run of the non-mark rune
// before it, except after a digit or at the start of the string, where the
// marks open the letter or symbol run that follows them; and a digit run
// holds no marks at all, so a digit run's start is simply its first digit.
func prevRun(s string, end int) (class, start int) {
	start = end
	c, size := lastRune(s, start)
	if c == classMark {
		// Trailing marks: the rune before them decides.
		p := marksBefore(s, start)
		if p == 0 {
			return classOther, 0 // only marks
		}
		pc, psize := lastRune(s, p)
		if pc == classDigit {
			return classOther, p // marks after a digit are their own run
		}
		start, c, size = p, pc, psize
	}
	class = c
	start -= size
	if class == classDigit {
		for start > 0 {
			c, size = lastRune(s, start)
			if c != classDigit {
				break
			}
			start -= size
		}
		return class, start
	}
	for start > 0 {
		c, size = lastRune(s, start)
		if c == class {
			start -= size
			continue
		}
		if c != classMark {
			break
		}
		p := marksBefore(s, start)
		if p == 0 {
			return class, 0 // leading marks open this run
		}
		pc, psize := lastRune(s, p)
		switch pc {
		case class:
			start = p - psize // the marks and their rune are in this run
		case classDigit:
			return class, p // marks after a digit open this run
		default:
			return class, start // the marks belong to the run before
		}
	}
	return class, start
}

// countWhole is one codec.Count call, exact for a span whose pieces are all
// short. codec.Count reports an error only for a regexp failure, which the
// generated matcher has not produced for any input tried (megabyte runs of
// every class included); should it ever, the span is charged its floor
// rather than nothing, which is still a lower bound and closer to the truth.
func (e *O200kBase) countWhole(s string) int {
	if s == "" {
		return 0
	}
	n, err := e.codec.Count(s)
	if err != nil || n < 0 {
		return floorTokens(len(s))
	}
	return n
}

// maxPieceBytes is the longest run of one rune class the codec is asked to
// handle. Pieces are at most a run plus a leading rune and a contraction, so
// a 1 KB run bounds the merge at ~0.4 ms per piece and the whitespace rescan
// at ~0.6 ms per region; a field made entirely of such runs, the worst
// case, counts at roughly 1 µs per byte against 60 ns for prose — bounded,
// and well under the time the upstream takes to say anything. Runs at or
// over the limit are charged floorTokens instead. Legitimate runs this long
// are rare — a run of a thousand blank lines, a kilobyte of one repeated
// symbol, a CJK sentence of more than three hundred characters with no
// punctuation — and for them the floor undercounts, which is the accepted
// direction: at most a few dozen tokens per kilobyte for whitespace and
// symbol runs, more for CJK.
const maxPieceBytes = 1024

// maxTokenBytes is the byte length of the longest token in o200k_base: the
// 128-space run. Derived from the vocabulary compiled into
// github.com/tiktoken-go/tokenizer v0.8.1 (TestMaxTokenBytesMatchesVocabulary
// pins the version); every token is at most this long, so a byte range that
// whole tokens tile cannot hold fewer than ceil(n / maxTokenBytes) of them.
const maxTokenBytes = 128

// floorTokens is the provable lower bound on the token count of n bytes.
func floorTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + maxTokenBytes - 1) / maxTokenBytes
}

// EstimateRequest counts the request under the lower-bound rule.
func (e *O200kBase) EstimateRequest(req *cschema.ResponsesRequest) int {
	v := exactVisitor{est: e}
	walkFields(req, &v)
	return v.total
}

// exactVisitor sums per-field counts. Item and Image charge nothing: the
// backend's framing and an image's tile cost are both unknowable here and the
// count must stay a lower bound.
type exactVisitor struct {
	est   *O200kBase
	total int
}

func (v *exactVisitor) Text(s string) { v.total += v.est.EstimateString(s) }
func (v *exactVisitor) Image()        {}
func (v *exactVisitor) Item()         {}

// Tool charges one declaration: name, description and the schema's prose (see
// schemaText). The whole declaration is memoized as one unit, keyed by the
// hash of its three fields, because tool definitions repeat verbatim on every
// turn of a session and their schemas are the one place the walk allocates
// (the JSON has to be decoded to find the prose inside it).
func (v *exactVisitor) Tool(t *cschema.Tool) {
	if n, ok := v.est.memo.getTool(t); ok {
		v.total += n
		return
	}
	n := v.est.count(t.Name) + v.est.count(t.Description)
	schemaText(t.Parameters, func(s string) { n += v.est.count(s) })
	v.est.memo.putTool(t, n)
	v.total += n
}

// memoGenerationEntries caps one memo generation. The cache holds between one
// and two generations, so at most twice this many entries live at once. An
// entry is a 16-byte key and an int in a Go map, roughly 40 bytes with map
// overhead, so the ceiling is about 5 MB. A Claude Code session contributes
// one entry per message part, tool call, tool result and tool declaration —
// a long session is a few thousand — so the cap comfortably covers dozens of
// concurrent sessions before anything is evicted, and nothing is ever
// re-tokenized while its session is still active.
const memoGenerationEntries = 1 << 16

// memo is a bounded, concurrency-safe cache of per-field token counts.
//
// Keys are a 64-bit content hash plus the field's byte length, not the field
// text, so the cache's memory does not scale with the size of the prompts it
// has seen. The hash is hash/maphash (the runtime's own, seeded per process),
// chosen for speed over collision resistance: a collision would mis-count one
// field of an estimate, which the 64-bit space plus the length guard makes a
// once-in-the-life-of-the-universe event for a cache this size, and the input
// comes from the local client, not an adversary.
//
// Eviction is generational rather than LRU. Inserts go to the young map; when
// it reaches its cap, it becomes the old map and the previous old map is
// dropped. A lookup checks young then old, and an old hit is copied forward so
// a field still in use survives the next rotation. This is O(1) with no
// per-access allocation and no list to maintain, at the cost of evicting in
// coarse batches, which is fine for a cache whose misses cost microseconds.
type memo struct {
	mu    sync.Mutex
	cap   int
	young map[memoKey]int
	old   map[memoKey]int
}

type memoKey struct {
	hash uint64
	len  int
}

var memoSeed = maphash.MakeSeed()

func newMemo(capacity int) *memo {
	return &memo{cap: capacity, young: make(map[memoKey]int, capacity/4)}
}

func keyOf(s string) memoKey {
	return memoKey{hash: maphash.String(memoSeed, s), len: len(s)}
}

// toolKey hashes a tool declaration as one unit. The fields are fed to the
// hash with a separator so ("ab","c") and ("a","bc") differ; the length guard
// is the sum of the field lengths.
func toolKey(t *cschema.Tool) memoKey {
	var h maphash.Hash
	h.SetSeed(memoSeed)
	_, _ = h.WriteString(t.Name)
	_ = h.WriteByte(0)
	_, _ = h.WriteString(t.Description)
	_ = h.WriteByte(0)
	_, _ = h.Write(t.Parameters)
	return memoKey{hash: h.Sum64(), len: len(t.Name) + len(t.Description) + len(t.Parameters)}
}

func (m *memo) get(s string) (int, bool)            { return m.lookup(keyOf(s)) }
func (m *memo) put(s string, n int)                 { m.store(keyOf(s), n) }
func (m *memo) getTool(t *cschema.Tool) (int, bool) { return m.lookup(toolKey(t)) }
func (m *memo) putTool(t *cschema.Tool, n int)      { m.store(toolKey(t), n) }

func (m *memo) lookup(k memoKey) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.young[k]; ok {
		return n, true
	}
	if n, ok := m.old[k]; ok {
		m.insertLocked(k, n)
		return n, true
	}
	return 0, false
}

func (m *memo) store(k memoKey, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.insertLocked(k, n)
}

func (m *memo) insertLocked(k memoKey, n int) {
	if len(m.young) >= m.cap {
		m.old = m.young
		m.young = make(map[memoKey]int, m.cap/4)
	}
	m.young[k] = n
}

// Len reports the number of entries currently held, for tests.
func (m *memo) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.young) + len(m.old)
}
