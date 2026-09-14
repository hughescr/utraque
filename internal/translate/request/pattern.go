package request

import (
	"encoding/json"
	"fmt"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The Codex backend validates every function tool's parameters as a JSON
// Schema before any model runs, and it accepts only a compatibility subset of
// regular-expression syntax. A pattern it rejects fails the whole request with
// HTTP 400, so one tool with a rich regex blocks every turn of the session.
// Anthropic accepts the same schema untouched, which is why this only surfaces
// on the codex route.
//
// The fix is a per-pattern compatibility translation. Most of a regex is
// literal text that every engine reads the same way; the known disagreements
// are concentrated in a handful of escape and group forms. Each one is either
// rewritten to a spelling accepted by the backend, or the pattern is dropped
// from the schema and its text appended to the property's description. That
// keeps the constraint visible in the tool declaration without sending a
// pattern the backend rejects.
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
//
// The translated pattern is finally checked for well-formedness by parsing a
// Go-syntax twin of it, which catches unbalanced brackets and bad repetitions
// that would fail on every engine.

// maxPropertyRanges caps how many codepoint ranges a single \p{...} may expand
// to before the pattern is dropped instead. The Artifact tool's classes (Cc,
// Cf, Zl, Zp) expand to a few dozen; a whole letter category is hundreds.
const maxPropertyRanges = 64

// droppedPatternSep joins the segments of a Metadata.DroppedPatterns or
// RewrittenPatterns entry: "<tool>.<json path>" names the schema node, e.g.
// "Artifact.properties.field".
const droppedPatternSep = "."

// patternNote is the sentence appended to a property description when its
// pattern is dropped, so the model still sees the constraint.
const patternNote = "Must match the regular expression: "

// schemaPatternResult reports what sanitizeToolSchema did to one schema:
// the JSON paths (relative to the schema root) of every node whose pattern was
// rewritten for backend compatibility, and of every node whose pattern was
// dropped. Both lists are sorted.
type schemaPatternResult struct {
	Rewritten []string
	Dropped   []string
}

func (r schemaPatternResult) empty() bool {
	return len(r.Rewritten) == 0 && len(r.Dropped) == 0
}

// sanitizeToolSchema returns a copy of raw with every "pattern" keyword
// translated for backend compatibility, or removed where no compatible form
// exists. When nothing needs to change the input bytes are returned as-is so
// the wire form of an untouched tool stays byte-identical. A schema that is not
// a JSON object is passed through untouched: the backend will reject it with a
// clearer error than anything this function could add.
func sanitizeToolSchema(raw json.RawMessage) (json.RawMessage, schemaPatternResult) {
	var res schemaPatternResult
	if len(raw) == 0 {
		return raw, res
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, res
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return raw, res
	}
	sanitizeSchemaNode(obj, "", &res)
	if res.empty() {
		return raw, res
	}
	sort.Strings(res.Rewritten)
	sort.Strings(res.Dropped)
	out, err := json.Marshal(obj)
	if err != nil {
		// Unreachable for a value that came out of json.Unmarshal, but the
		// original is a better fallback than an empty schema.
		return raw, schemaPatternResult{}
	}
	return out, res
}

// schemaChildKeys lists the JSON Schema keywords whose value is itself a
// schema (single) or a list/map of schemas, so the walk can find every node a
// "pattern" keyword may sit on. Unknown keywords are not descended into: a
// "pattern" inside "examples" or "default" is data, not a constraint.
var (
	schemaSingleChild = []string{"items", "additionalProperties", "additionalItems", "contains", "not", "if", "then", "else", "propertyNames"}
	schemaListChild   = []string{"anyOf", "oneOf", "allOf", "prefixItems", "items"}
	schemaMapChild    = []string{"properties", "patternProperties", "definitions", "$defs", "dependentSchemas"}
)

// sanitizeSchemaNode rewrites node in place and records what it did in res.
// path is the dotted JSON path to node ("" at root).
func sanitizeSchemaNode(node map[string]any, path string, res *schemaPatternResult) {
	if p, ok := node["pattern"].(string); ok {
		switch py, ok := toPythonPattern(p); {
		case !ok:
			delete(node, "pattern")
			desc, _ := node["description"].(string)
			note := patternNote + p
			if desc == "" {
				node["description"] = note
			} else {
				node["description"] = desc + " " + note
			}
			res.Dropped = append(res.Dropped, path)
		case py != p:
			node["pattern"] = py
			res.Rewritten = append(res.Rewritten, path)
		}
	}
	join := func(key string) string {
		if path == "" {
			return key
		}
		return path + droppedPatternSep + key
	}
	for _, k := range schemaSingleChild {
		if child, ok := node[k].(map[string]any); ok {
			sanitizeSchemaNode(child, join(k), res)
		}
	}
	// "items" appears in both lists: it is a single schema in modern drafts
	// and a tuple-form list in draft-04.
	for _, k := range schemaListChild {
		list, ok := node[k].([]any)
		if !ok {
			continue
		}
		for i, v := range list {
			if child, ok := v.(map[string]any); ok {
				sanitizeSchemaNode(child, join(k)+droppedPatternSep+strconv.Itoa(i), res)
			}
		}
	}
	for _, k := range schemaMapChild {
		m, ok := node[k].(map[string]any)
		if !ok {
			continue
		}
		for name, v := range m {
			if child, ok := v.(map[string]any); ok {
				sanitizeSchemaNode(child, join(k)+droppedPatternSep+name, res)
			}
		}
	}
}

// toPythonPattern applies the known backend-compatibility rewrites to p. It
// returns the translated pattern (identical to p when nothing needed changing)
// and false when p uses an unsupported form or is malformed.
func toPythonPattern(p string) (string, bool) {
	w := patternWriter{}
	if !w.translate(p) {
		return "", false
	}
	if _, err := syntax.Parse(w.chk.String(), syntax.Perl); err != nil {
		return "", false
	}
	return w.out.String(), true
}

// patternWriter emits two spellings of a pattern side by side: out goes on the
// wire, and chk is a Go-syntax twin used only to check well-formedness. They
// differ where Go's parser rejects a form emitted on the wire and in the
// spelling of hexadecimal escapes (\uHHHH on the wire, \x{HHHH} in Go).
type patternWriter struct {
	out, chk strings.Builder
}

func (w *patternWriter) both(s string) {
	w.out.WriteString(s)
	w.chk.WriteString(s)
}

func (w *patternWriter) split(py, gochk string) {
	w.out.WriteString(py)
	w.chk.WriteString(gochk)
}

func (w *patternWriter) translate(p string) bool {
	inClass := false // inside [...]; \p expands inline, ( is literal
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\':
			if i+1 >= len(p) {
				return false // trailing backslash: malformed everywhere
			}
			n, ok := w.escape(p[i:], inClass)
			if !ok {
				return false
			}
			i += n - 1
		case inClass:
			if c == ']' {
				inClass = false
			} else if c == '[' && strings.HasPrefix(p[i:], "[:") {
				return false // POSIX class such as [[:alpha:]]
			}
			w.both(string(c))
		case c == '[':
			inClass = true
			w.both("[")
			// A ']' immediately after '[' or '[^' is a literal, not a close.
			j := i + 1
			if j < len(p) && p[j] == '^' {
				w.both("^")
				j++
			}
			if j < len(p) && p[j] == ']' {
				w.both("]")
				j++
			}
			i = j - 1
		case c == '(' && i+1 < len(p) && p[i+1] == '?':
			n, ok := w.group(p[i:])
			if !ok {
				return false
			}
			i += n - 1
		case c == '+' && i > 0 && strings.IndexByte("+*?}", p[i-1]) >= 0:
			return false // possessive quantifier
		default:
			w.both(string(c))
		}
	}
	return !inClass
}

// escape handles the escape sequence at the start of s (which begins with a
// backslash and has at least one byte after it) and returns how many bytes it
// consumed.
func (w *patternWriter) escape(s string, inClass bool) (int, bool) {
	switch s[1] {
	case 'p', 'P':
		return w.property(s, inClass)
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
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return 0, false
		}
		cp, err := strconv.ParseUint(s[3:end], 16, 32)
		if err != nil || cp > unicode.MaxRune {
			return 0, false
		}
		w.split(pyCodepoint(rune(cp)), goCodepoint(rune(cp)))
		return end + 1, true
	case 'u', 'U':
		// \uHHHH and \UHHHHHHHH are wire spellings Go's parser lacks.
		width := 4
		if s[1] == 'U' {
			width = 8
		}
		if len(s) < 2+width {
			return 0, false
		}
		cp, err := strconv.ParseUint(s[2:2+width], 16, 32)
		if err != nil || cp > unicode.MaxRune {
			return 0, false
		}
		w.split(s[:2+width], goCodepoint(rune(cp)))
		return 2 + width, true
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

// property expands \p{Name}, \P{Name}, \pL or \PL at the start of s into
// explicit codepoint ranges. Outside a class the expansion is wrapped in its
// own [...] (negated for \P); inside a class the ranges are emitted inline,
// and \P is expanded to the complement so the enclosing class stays correct.
func (w *patternWriter) property(s string, inClass bool) (int, bool) {
	negate := s[1] == 'P'
	var name string
	n := 0
	switch {
	case len(s) > 2 && s[2] == '{':
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return 0, false
		}
		name = s[3:end]
		n = end + 1
	case len(s) > 2:
		name = s[2:3]
		n = 3
	default:
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

// group handles a "(?" construct at the start of s and returns how many bytes
// it consumed. The Codex schema validator rejects lookaround, so any of its
// four forms makes the enclosing pattern fall back to a description. The
// JS-style named group is respelled for the backend.
func (w *patternWriter) group(s string) (int, bool) {
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

// goCodepoint spells a codepoint for the Go syntax check.
func goCodepoint(c rune) string {
	return fmt.Sprintf(`\x{%x}`, c)
}
