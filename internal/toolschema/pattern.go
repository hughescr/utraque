// Package toolschema rewrites a tool's JSON Schema for a backend that
// validates every "pattern" keyword as a regular expression before any model
// runs. Anthropic accepts the schema untouched, so this only surfaces on a
// route whose backend runs its own regex parser: Codex (Python re, in a
// compatibility subset) and DeepSeek (a Rust engine with fancy-regex
// extensions). A pattern the backend rejects fails the whole request with
// HTTP 400, so one tool with a rich regex blocks every turn of the session.
//
// The fix is a per-pattern compatibility translation, parameterized by a
// Dialect that knows what one backend accepts. Most of a regex is literal text
// that every engine reads the same way; the known disagreements are
// concentrated in a handful of escape and group forms. Each one is either
// rewritten to a spelling accepted by the backend, or the pattern is dropped
// from the schema and its text appended to the property's description. That
// keeps the constraint visible in the tool declaration without sending a
// pattern the backend rejects.
//
// A translated pattern is finally checked for well-formedness by parsing a
// Go-syntax twin of it, which catches unbalanced brackets and bad repetitions
// that would fail on every engine. Where a backend accepts a form Go's parser
// lacks (lookaround, possessive quantifiers, \G, ...), the twin carries a
// neutral stand-in in its place: an empty group where the form was a group,
// so the enclosing parens are still balanced, and a literal where it was an
// atom, so a following quantifier still has an operand.
//
// Only syntax is checked. A backreference naming a group that does not
// exist (\3 with two groups, \k<name> with no such group) is a well-formed
// pattern that the backend may still reject; that is not detected here.
//
// The dialect rules are documented on Codex and DeepSeek.
package toolschema

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// PathSep joins the segments of a Result entry: "<tool>.<json path>" names
// the schema node, e.g. "Artifact.properties.field".
const PathSep = "."

// NodePath names a schema node for a log line: "<tool>.<path>", or just the
// tool name when path is "" (a pattern on the schema root).
func NodePath(tool, path string) string {
	if path == "" {
		return tool
	}
	return tool + PathSep + path
}

// PatternNote is the sentence appended to a property description when its
// pattern is dropped, so the model still sees the constraint.
const PatternNote = "Must match the regular expression: "

// Result reports what Rewrite did to one schema: the JSON paths (relative to
// the schema root) of every node whose pattern was rewritten for backend
// compatibility, and of every node whose pattern was dropped. Both lists are
// sorted.
type Result struct {
	Rewritten []string
	Dropped   []string
}

// Empty reports whether the schema was left untouched.
func (r Result) Empty() bool {
	return len(r.Rewritten) == 0 && len(r.Dropped) == 0
}

// Report aggregates, across every tool in one request, what Rewrite did:
// each entry names a schema node as "<tool>.<json path>" (see NodePath).
// Rewritten holds the nodes whose pattern was translated for backend
// compatibility; Dropped holds the nodes whose pattern could not be sent
// compatibly and was removed, with its text folded into the node's
// description so the constraint remains in the tool declaration. Entries are
// in tool order, then in each tool's Result order. The zero value is an empty
// report.
type Report struct {
	Rewritten []string
	Dropped   []string
}

// Add folds one tool's Result into the report, prefixing each path with the
// tool's name.
func (r *Report) Add(tool string, res Result) {
	for _, p := range res.Rewritten {
		r.Rewritten = append(r.Rewritten, NodePath(tool, p))
	}
	for _, p := range res.Dropped {
		r.Dropped = append(r.Dropped, NodePath(tool, p))
	}
}

// Empty reports whether no tool schema was touched.
func (r Report) Empty() bool {
	return len(r.Rewritten) == 0 && len(r.Dropped) == 0
}

// LogAttrs renders the report for a log line: a rewritten_patterns attribute
// when anything was rewritten, then a dropped_patterns attribute when
// anything was dropped, and nothing for an empty report. Both legs append
// these to their per-request translation line, so the keys are spelled once.
func (r Report) LogAttrs() []slog.Attr {
	var attrs []slog.Attr
	if len(r.Rewritten) > 0 {
		attrs = append(attrs, slog.Any("rewritten_patterns", r.Rewritten))
	}
	if len(r.Dropped) > 0 {
		attrs = append(attrs, slog.Any("dropped_patterns", r.Dropped))
	}
	return attrs
}

// Dialect describes one backend's regular-expression syntax: which forms it
// rejects, and the spelling it wants for the forms it accepts under a
// different name. The two implementations are Codex and DeepSeek.
type Dialect interface {
	// escape translates the escape sequence at the start of s, which begins
	// with a backslash and has at least one byte after it. It writes to w and
	// returns how many bytes of s it consumed, or false when the escape is one
	// the backend rejects.
	escape(w *writer, s string, inClass bool) (int, bool)
	// group translates the "(?" construct at the start of s the same way.
	group(w *writer, s string) (int, bool)
	// possessive handles a '+' that directly follows a quantifier. It returns
	// false when the backend rejects possessive quantifiers.
	possessive(w *writer) bool
	// posixClass reports whether [:name:] inside a bracket expression is
	// accepted; when it is, the span is passed through verbatim.
	posixClass() bool
	// classLiteral writes the unescaped byte at the start of s, found inside
	// a bracket expression, respelled if the backend would read it as an
	// operator. The rest of s is there to see a doubled operator coming.
	classLiteral(w *writer, s string)
}

// Rewrite returns a copy of raw with every "pattern" keyword translated for
// d, or removed where no compatible form exists. When nothing needs to change
// the input bytes are returned as-is: the caller re-encodes the request with
// encoding/json, which keeps the schema's key order and number spelling but
// compacts whitespace and escape spellings, so "untouched" means unchanged
// content, not identical bytes. A schema that is not a JSON object is passed
// through untouched: the backend will reject it with a clearer error than
// anything this function could add.
func Rewrite(d Dialect, raw json.RawMessage) (json.RawMessage, Result) {
	var res Result
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
	rewriteNode(d, obj, "", &res)
	if res.Empty() {
		return raw, res
	}
	sort.Strings(res.Rewritten)
	sort.Strings(res.Dropped)
	out, err := json.Marshal(obj)
	if err != nil {
		// Unreachable for a value that came out of json.Unmarshal, but the
		// original is a better fallback than an empty schema.
		return raw, Result{}
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

// rewriteNode rewrites node in place and records what it did in res.
// path is the dotted JSON path to node ("" at root).
func rewriteNode(d Dialect, node map[string]any, path string, res *Result) {
	if p, ok := node["pattern"].(string); ok {
		switch out, ok := translate(d, p); {
		case !ok:
			delete(node, "pattern")
			desc, _ := node["description"].(string)
			note := PatternNote + p
			if desc == "" {
				node["description"] = note
			} else {
				node["description"] = desc + " " + note
			}
			res.Dropped = append(res.Dropped, path)
		case out != p:
			node["pattern"] = out
			res.Rewritten = append(res.Rewritten, path)
		}
	}
	join := func(key string) string {
		if path == "" {
			return key
		}
		return path + PathSep + key
	}
	for _, k := range schemaSingleChild {
		if child, ok := node[k].(map[string]any); ok {
			rewriteNode(d, child, join(k), res)
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
				rewriteNode(d, child, join(k)+PathSep+strconv.Itoa(i), res)
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
				rewriteNode(d, child, join(k)+PathSep+name, res)
			}
		}
	}
}

// translate applies d's compatibility rewrites to p. It returns the
// translated pattern (identical to p when nothing needed changing) and false
// when p uses a form the backend rejects or is malformed.
func translate(d Dialect, p string) (string, bool) {
	w := &writer{}
	if !w.run(d, p) {
		return "", false
	}
	if _, err := syntax.Parse(w.chk.String(), syntax.Perl); err != nil {
		return "", false
	}
	return w.out.String(), true
}

// writer emits two spellings of a pattern side by side: out goes on the wire,
// and chk is a Go-syntax twin used only to check well-formedness. They differ
// where Go's parser rejects a form emitted on the wire and in the spelling of
// hexadecimal escapes.
type writer struct {
	out, chk strings.Builder
}

func (w *writer) both(s string) {
	w.out.WriteString(s)
	w.chk.WriteString(s)
}

func (w *writer) split(wire, gochk string) {
	w.out.WriteString(wire)
	w.chk.WriteString(gochk)
}

// run walks p once, handing every escape and "(?" group to d and echoing
// everything else to both spellings. It returns false as soon as d rejects a
// form or the pattern is malformed in a way every engine would reject.
func (w *writer) run(d Dialect, p string) bool {
	inClass := false // inside [...]: ( is literal, escapes may differ
	q := qNone       // what quantifier the previous token was
	for i := 0; i < len(p); i++ {
		c := p[i]
		// Every branch but the quantifier ones ends a quantifier run.
		prev := q
		q = qNone
		switch {
		case c == '\\':
			if i+1 >= len(p) {
				return false // trailing backslash: malformed everywhere
			}
			n, ok := d.escape(w, p[i:], inClass)
			if !ok {
				return false
			}
			i += n - 1
		case inClass:
			switch {
			case c == ']':
				inClass = false
				w.both("]")
			case c == '[' && strings.HasPrefix(p[i:], "[:"):
				// A POSIX class such as [[:alpha:]]. Its span is echoed whole
				// so its own closing ']' is not mistaken for the bracket's.
				end := strings.Index(p[i:], ":]")
				if end < 0 || !d.posixClass() {
					return false
				}
				w.both(p[i : i+end+2])
				i += end + 1
			default:
				d.classLiteral(w, p[i:])
			}
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
			n, ok := d.group(w, p[i:])
			if !ok {
				return false
			}
			i += n - 1
		case c == '*' || c == '+' || c == '?' || c == '{':
			n, ok := w.quantifier(d, p[i:], prev, &q)
			if !ok {
				return false
			}
			i += n - 1
		default:
			w.both(string(c))
		}
	}
	return !inClass
}

// quantState records what the previous token was, so a '?' or '+' after a
// quantifier is read as its lazy or possessive suffix rather than as a
// quantifier of its own, and a quantifier after a possessive one is rejected.
type quantState int

const (
	qNone       quantState = iota // not a quantifier
	qGreedy                       // *, +, ? or {n,m}
	qLazy                         // a greedy quantifier plus ?
	qPossessive                   // a greedy or lazy quantifier plus +
)

// quantifier handles the *, +, ? or { at the start of s, given what the
// previous token was, and sets q for the next token. It returns how many
// bytes it consumed.
//
// A quantifier directly after a possessive one is rejected here because the
// twin cannot show it: the possessive '+' has no Go spelling and is omitted,
// so a following quantifier would look valid to Go while every backend
// rejects it. Any other stacking (a**, a+{2}) reaches the twin as written,
// and Go rejects it as a nested repetition. A lazy quantifier followed by a
// possessive '+' is accepted by the DeepSeek engine and passed through; the
// Codex dialect drops every possessive form anyway.
func (w *writer) quantifier(d Dialect, s string, prev quantState, q *quantState) (int, bool) {
	c := s[0]
	if prev == qPossessive {
		if c != '{' || isRepeat(s) {
			return 0, false
		}
	}
	switch {
	case c == '+' && (prev == qGreedy || prev == qLazy):
		if !d.possessive(w) {
			return 0, false
		}
		*q = qPossessive
		return 1, true
	case c == '?' && prev == qGreedy:
		w.both("?")
		*q = qLazy
		return 1, true
	case c == '{':
		return w.repeat(s, q)
	default:
		w.both(string(c))
		*q = qGreedy
		return 1, true
	}
}

// maxTwinRepeat is Go's regexp/syntax repeat-count limit.
const maxTwinRepeat = 1000

// isRepeat reports whether s starts with a well-formed {n}, {n,} or {n,m}.
func isRepeat(s string) bool {
	_, _, n := repeatBounds(s)
	return n > 0
}

// repeatBounds parses the {n}, {n,} or {n,m} at the start of s, returning the
// two decimal bounds (hi is "" for {n} and {n,}), the length of the span, and
// false when s does not start with a well-formed repeat.
func repeatBounds(s string) (lo, hi string, n int) {
	end := strings.IndexByte(s, '}')
	if end < 0 {
		return "", "", 0
	}
	lo, hi, hasComma := strings.Cut(s[1:end], ",")
	if lo == "" || !isDigits(lo) || (hasComma && hi != "" && !isDigits(hi)) {
		return "", "", 0
	}
	if !hasComma {
		hi = lo
	}
	return lo, hi, end + 1
}

// repeat handles a '{' at the start of s. A well-formed {n}, {n,} or {n,m}
// goes to the wire verbatim and to the twin with each count clamped to Go's
// limit of 1000, which the backends exceed (the Artifact tool has {1,4096}).
// Clamping would hide an inverted {n,m} from the twin, so that is rejected
// here first. Anything else is a literal brace and echoed to both. It returns
// how many bytes it consumed.
func (w *writer) repeat(s string, q *quantState) (int, bool) {
	lo, hi, n := repeatBounds(s)
	if n == 0 {
		w.both("{")
		return 1, true
	}
	if hi != "" && compareDecimal(lo, hi) > 0 {
		return 0, false
	}
	twin := "{" + clampRepeat(lo)
	if _, rest, hasComma := strings.Cut(s[1:n-1], ","); hasComma {
		twin += "," + clampRepeat(rest)
	}
	twin += "}"
	w.split(s[:n], twin)
	*q = qGreedy
	return n, true
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// compareDecimal orders two unsigned decimal strings of any length.
func compareDecimal(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}

// clampRepeat returns the decimal count s, or Go's limit when s exceeds it.
// An empty s (the open upper bound of {n,}) is returned as-is.
func clampRepeat(s string) string {
	if s == "" {
		return s
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > maxTwinRepeat {
		return strconv.Itoa(maxTwinRepeat)
	}
	return s
}

// propertySpan measures the \p{Name}, \P{Name}, \pL or \PL at the start of s,
// returning the name (with any leading ^ negation marker intact) and the
// number of bytes the escape occupies.
func propertySpan(s string) (name string, n int, ok bool) {
	switch {
	case len(s) > 2 && s[2] == '{':
		end := strings.IndexByte(s, '}')
		if end < 0 || end == 3 {
			return "", 0, false
		}
		return s[3:end], end + 1, true
	case len(s) > 2:
		return s[2:3], 3, true
	default:
		return "", 0, false
	}
}

// bracedHex parses the \x{HHHH} at the start of s.
func bracedHex(s string) (cp rune, n int, ok bool) {
	end := strings.IndexByte(s, '}')
	if end < 0 {
		return 0, 0, false
	}
	v, err := strconv.ParseUint(s[3:end], 16, 32)
	if err != nil || v > unicode.MaxRune {
		return 0, 0, false
	}
	return rune(v), end + 1, true
}

// fixedHex parses the \uHHHH or \UHHHHHHHH at the start of s.
func fixedHex(s string) (cp rune, n int, ok bool) {
	width := 4
	if s[1] == 'U' {
		width = 8
	}
	if len(s) < 2+width {
		return 0, 0, false
	}
	v, err := strconv.ParseUint(s[2:2+width], 16, 32)
	if err != nil || v > unicode.MaxRune {
		return 0, 0, false
	}
	return rune(v), 2 + width, true
}

// goCodepoint spells a codepoint for the Go syntax check.
func goCodepoint(c rune) string {
	return fmt.Sprintf(`\x{%x}`, c)
}
