package toolschema

import (
	"fmt"
	"strings"
	"unicode"
)

// Codex is the dialect of the Codex backend's schema validator, which accepts
// a compatibility subset of Python re syntax.
//
// Rewritten:
//   - \p{Name}, \P{Name}, \pL: Unicode property classes. Expanded into
//     explicit codepoint ranges from Go's unicode tables. A class whose
//     expansion is large (\p{L} is hundreds of ranges) is dropped rather than
//     bloating the prompt.
//   - (?<name>...): the JS/.NET/Go named group becomes (?P<name>...).
//   - \k<name>: named backreference becomes (?P=name).
//   - \z: absolute end of text becomes \Z.
//   - \x{HHHH}: braced hex escape becomes \uHHHH or \UHHHHHHHH.
//
// Dropped (known unsupported by the backend, or too large to emit):
//   - lookaround: (?=...), (?!...), (?<=...), (?<!...)
//   - \Q...\E, [[:alpha:]], \C, \G
//   - (?>...) atomic groups and possessive quantifiers
//   - inline flags other than i, m, s
//
// Kept as-is: numbered backreferences, \A, \Z, (?P<name>...), (?P=name),
// (?#comment), and non-capturing groups.
var Codex Dialect = codexDialect{}

// maxPropertyRanges caps how many codepoint ranges a single \p{...} may expand
// to before the pattern is dropped instead. The Artifact tool's classes (Cc,
// Cf, Zl, Zp) expand to a few dozen; a whole letter category is hundreds.
const maxPropertyRanges = 64

type codexDialect struct{}

func (codexDialect) escape(w *writer, s string, inClass bool) (int, bool) {
	switch s[1] {
	case 'p', 'P':
		return codexProperty(w, s, inClass)
	case 'k':
		// \k<name> -> (?P=name). A backreference is a literal for the Go check.
		if inClass || !strings.HasPrefix(s[2:], "<") {
			return 0, false
		}
		end := strings.IndexByte(s, '>')
		if end < 0 {
			return 0, false
		}
		w.split("(?P="+s[3:end]+")", "x")
		return end + 1, true
	case 'x':
		if !strings.HasPrefix(s[2:], "{") {
			w.both(s[:2])
			return 2, true
		}
		cp, n, ok := bracedHex(s)
		if !ok {
			return 0, false
		}
		w.split(pyCodepoint(cp), goCodepoint(cp))
		return n, true
	case 'u', 'U':
		// \uHHHH and \UHHHHHHHH are wire spellings Go's parser lacks.
		cp, n, ok := fixedHex(s)
		if !ok {
			return 0, false
		}
		w.split(s[:n], goCodepoint(cp))
		return n, true
	case 'z', 'Z':
		w.split(`\Z`, `\z`)
		return 2, true
	case 'Q', 'E', 'C', 'G':
		return 0, false
	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if inClass {
			return 0, false // octal-vs-backreference ambiguity; not worth it
		}
		w.split(s[:2], "x")
		return 2, true
	default:
		w.both(s[:2])
		return 2, true
	}
}

// codexProperty expands \p{Name}, \P{Name}, \pL or \PL at the start of s into
// explicit codepoint ranges. Outside a class the expansion is wrapped in its
// own [...] (negated for \P); inside a class the ranges are emitted inline,
// and \P is expanded to the complement so the enclosing class stays correct.
func codexProperty(w *writer, s string, inClass bool) (int, bool) {
	negate := s[1] == 'P'
	name, n, ok := propertySpan(s)
	if !ok {
		return 0, false
	}
	if strings.HasPrefix(name, "^") {
		negate = !negate
		name = name[1:]
	}
	tbl := lookupUnicodeTable(name)
	if tbl == nil {
		return 0, false
	}
	ranges := tableRanges(tbl)
	if inClass && negate {
		ranges = complementRanges(ranges)
	}
	if len(ranges) > maxPropertyRanges {
		return 0, false
	}
	var py, gochk strings.Builder
	if !inClass {
		py.WriteByte('[')
		gochk.WriteByte('[')
		if negate {
			py.WriteByte('^')
			gochk.WriteByte('^')
		}
	}
	for _, r := range ranges {
		py.WriteString(pyCodepoint(r[0]))
		gochk.WriteString(goCodepoint(r[0]))
		if r[1] != r[0] {
			py.WriteByte('-')
			gochk.WriteByte('-')
			py.WriteString(pyCodepoint(r[1]))
			gochk.WriteString(goCodepoint(r[1]))
		}
	}
	if !inClass {
		py.WriteByte(']')
		gochk.WriteByte(']')
	}
	w.split(py.String(), gochk.String())
	return n, true
}

// group handles a "(?" construct. The Codex schema validator rejects
// lookaround, so any of its four forms makes the enclosing pattern fall back
// to a description. The JS-style named group is respelled for the backend.
func (codexDialect) group(w *writer, s string) (int, bool) {
	rest := s[2:]
	switch {
	case strings.HasPrefix(rest, ":"), strings.HasPrefix(rest, "P<"):
		w.both("(?")
		return 2, true
	case strings.HasPrefix(rest, "#"):
		// A comment group; Python keeps it, Go's parser has no such form.
		end := strings.IndexByte(s, ')')
		if end < 0 {
			return 0, false
		}
		w.split(s[:end+1], "")
		return end + 1, true
	case strings.HasPrefix(rest, "P="):
		end := strings.IndexByte(s, ')')
		if end < 0 {
			return 0, false
		}
		w.split(s[:end+1], "x")
		return end + 1, true
	case strings.HasPrefix(rest, "="), strings.HasPrefix(rest, "!"),
		strings.HasPrefix(rest, "<="), strings.HasPrefix(rest, "<!"):
		return 0, false
	case strings.HasPrefix(rest, "<"):
		w.both("(?P<")
		return 3, true
	}
	// Inline flags: only the ones every engine shares.
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case 'i', 'm', 's':
			continue
		case ')', ':':
			if j == 0 {
				return 0, false
			}
			w.both(s[:2+j+1])
			return 2 + j + 1, true
		default:
			return 0, false
		}
	}
	return 0, false
}

func (codexDialect) possessive(*writer) bool { return false }

func (codexDialect) posixClass() bool { return false }

func (codexDialect) classLiteral(w *writer, s string) { w.both(s[:1]) }

// lookupUnicodeTable resolves a \p{...} name the way RE2 does: general
// categories first (including the one-letter groups), then scripts, then the
// binary properties, plus "Any".
func lookupUnicodeTable(name string) *unicode.RangeTable {
	if name == "Any" {
		return &unicode.RangeTable{R32: []unicode.Range32{{Lo: 0, Hi: unicode.MaxRune, Stride: 1}}}
	}
	if t, ok := unicode.Categories[name]; ok {
		return t
	}
	if t, ok := unicode.Scripts[name]; ok {
		return t
	}
	if t, ok := unicode.Properties[name]; ok {
		return t
	}
	return nil
}

// tableRanges flattens a RangeTable into sorted, non-overlapping [lo,hi]
// pairs, expanding strided entries into individual codepoints.
func tableRanges(t *unicode.RangeTable) [][2]rune {
	var out [][2]rune
	for _, r := range t.R16 {
		out = appendRange(out, rune(r.Lo), rune(r.Hi), rune(r.Stride))
	}
	for _, r := range t.R32 {
		out = appendRange(out, rune(r.Lo), rune(r.Hi), rune(r.Stride))
	}
	return out
}

func appendRange(out [][2]rune, lo, hi, stride rune) [][2]rune {
	if stride == 1 {
		return append(out, [2]rune{lo, hi})
	}
	for c := lo; c <= hi; c += stride {
		out = append(out, [2]rune{c, c})
	}
	return out
}

// complementRanges returns the codepoints in [0, MaxRune] not covered by
// ranges, which must be sorted and non-overlapping.
func complementRanges(ranges [][2]rune) [][2]rune {
	var out [][2]rune
	next := rune(0)
	for _, r := range ranges {
		if r[0] > next {
			out = append(out, [2]rune{next, r[0] - 1})
		}
		next = r[1] + 1
	}
	if next <= unicode.MaxRune {
		out = append(out, [2]rune{next, unicode.MaxRune})
	}
	return out
}

// pyCodepoint spells a codepoint as a Python re escape: \xHH, \uHHHH or
// \UHHHHHHHH. Escapes are used even for printable ASCII so the result is safe
// inside a character class without any ']' or '-' special-casing.
func pyCodepoint(c rune) string {
	switch {
	case c < 0x100:
		return fmt.Sprintf(`\x%02x`, c)
	case c < 0x10000:
		return fmt.Sprintf(`\u%04x`, c)
	default:
		return fmt.Sprintf(`\U%08x`, c)
	}
}
