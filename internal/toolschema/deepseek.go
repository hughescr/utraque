package toolschema

import (
	"fmt"
	"strconv"
	"strings"
)

// DeepSeek is the dialect of DeepSeek's Anthropic-compatible endpoint, which
// validates each pattern with a Rust regex engine carrying fancy-regex
// extensions (backtracking, lookaround, backreferences). The rules were
// established empirically, one pattern per request against the live endpoint.
//
// Rewritten:
//   - \0, \0o, \0oo: the NUL (and octal) escape becomes \xHH. The bare form is
//     read as a backreference outside a class and rejected inside one, so
//     Claude Code's Artifact tool ("^[^\0]*$") fails every request without
//     this. \00 is NUL and \012 is \x0a, never a NUL followed by a digit.
//   - \Z: end of text becomes \z. Note the direction is the reverse of Codex.
//   - \p{^Name}, \P{^Name}: the caret negation is respelled as \P{Name} and
//     \p{Name}, the only negation the engine accepts.
//   - [ inside a bracket expression is escaped, as is each & or ~ that is
//     followed by another. The engine reads an unescaped [ as a nested class
//     (so "[^./[\]]" is unterminated, which is the Artifact tool's "field"
//     pattern) and && or ~~ as set operators, where every other engine reads
//     literals.
//
// Dropped (rejected by the engine):
//   - \C, \R, \Q...\E, \N{...}, \o{...}, \g<...>, and any unknown letter
//     escape such as \y
//   - a numbered backreference inside a class
//
// Kept as-is: lookaround, atomic groups, possessive quantifiers, \p{...}
// with any other name (names are not validated locally; the engine's table is
// wider than Go's), (?<name>...), (?P<name>...), (?P=name), \k<name>,
// numbered backreferences, [[:alpha:]], \G, \K (outside a class), \A, \z,
// \b, \B, \h, \e, \cX, \x{...}, \uHHHH, \UHHHHHHHH, (?#comment), and the
// inline flags i, m, s, x, u and U, with or without a "-" negation.
var DeepSeek Dialect = deepseekDialect{}

type deepseekDialect struct{}

func (deepseekDialect) Name() string { return "deepseek" }

func (deepseekDialect) escape(w *writer, s string, inClass bool) (int, bool) {
	c := s[1]
	switch c {
	case 'p', 'P':
		name, n, ok := propertySpan(s)
		if !ok {
			return 0, false
		}
		wire := s[:n]
		if strings.HasPrefix(name, "^") {
			letter := byte('P')
			if c == 'P' {
				letter = 'p'
			}
			wire = `\` + string(letter) + "{" + name[1:] + "}"
		}
		// The engine's property table is wider than Go's, so the twin sees a
		// literal rather than a name Go might not know.
		w.split(wire, "x")
		return n, true
	case 'k':
		if inClass || !strings.HasPrefix(s[2:], "<") {
			return 0, false
		}
		end := strings.IndexByte(s, '>')
		if end < 0 {
			return 0, false
		}
		w.split(s[:end+1], "x")
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
		w.split(s[:n], goCodepoint(cp))
		return n, true
	case 'u', 'U':
		cp, n, ok := fixedHex(s)
		if !ok {
			return 0, false
		}
		w.split(s[:n], goCodepoint(cp))
		return n, true
	case 'z', 'Z':
		w.split(`\z`, classNeutral(inClass, `\z`))
		return 2, true
	case 'A', 'b', 'B':
		w.split(s[:2], classNeutral(inClass, s[:2]))
		return 2, true
	case 'G':
		// Match-start anchor: zero-width, like Go's \b.
		w.split(`\G`, classNeutral(inClass, `\b`))
		return 2, true
	case 'K':
		// Keep-out marker: zero-width, no Go twin, meaningless in a class.
		if inClass {
			return 0, false
		}
		w.split(`\K`, "")
		return 2, true
	case 'e':
		w.split(`\e`, `\x{1b}`)
		return 2, true
	case 'h', 'H':
		// Horizontal whitespace; any class-like escape keeps the twin honest.
		w.split(s[:2], `\s`)
		return 2, true
	case 'c':
		if len(s) < 3 || !isASCIILetter(s[2]) {
			return 0, false
		}
		w.split(s[:3], goCodepoint(rune(s[2]&0x1f)))
		return 3, true
	case '0':
		n := 2
		for n < 4 && n < len(s) && s[n] >= '0' && s[n] <= '7' {
			n++
		}
		v, _ := strconv.ParseUint(s[1:n], 8, 8)
		w.both(fmt.Sprintf(`\x%02x`, v))
		return n, true
	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if inClass {
			return 0, false // the engine rejects a backreference in a class
		}
		w.split(s[:2], "x")
		return 2, true
	case 'a', 'f', 'n', 'r', 't', 'v', 'd', 'D', 'w', 'W', 's', 'S':
		w.both(s[:2])
		return 2, true
	default:
		if c >= 0x80 || isASCIILetter(c) || (c >= '0' && c <= '9') {
			// \C, \R, \Q, \E, \N, \o, \g and every other letter escape.
			return 0, false
		}
		w.both(s[:2]) // escaped punctuation or space
		return 2, true
	}
}

// classNeutral returns the Go twin of an assertion escape: outside a class the
// given spelling, inside one a literal, because Go rejects an assertion in a
// class while the engine accepts it.
func classNeutral(inClass bool, outside string) string {
	if inClass {
		return "x"
	}
	return outside
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// group handles a "(?" construct. Lookaround and atomic groups are accepted
// by the engine and become plain groups in the twin, so an unbalanced paren
// inside them is still caught while the wire form is untouched.
func (deepseekDialect) group(w *writer, s string) (int, bool) {
	rest := s[2:]
	switch {
	case strings.HasPrefix(rest, ":"), strings.HasPrefix(rest, "P<"):
		w.both("(?")
		return 2, true
	case strings.HasPrefix(rest, "#"):
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
	case strings.HasPrefix(rest, "<="), strings.HasPrefix(rest, "<!"):
		w.split(s[:4], "(?:")
		return 4, true
	case strings.HasPrefix(rest, "="), strings.HasPrefix(rest, "!"), strings.HasPrefix(rest, ">"):
		w.split(s[:3], "(?:")
		return 3, true
	case strings.HasPrefix(rest, "<"):
		w.both("(?<")
		return 3, true
	}
	// Inline flags. Go lacks x and u, so the twin omits them; a group that
	// carried nothing else is an empty flag group or a plain group.
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case 'i', 'm', 's', 'x', 'u', 'U', '-':
			continue
		case ')', ':':
			flags := rest[:j]
			if flags == "" || strings.HasSuffix(flags, "-") || strings.Count(flags, "-") > 1 {
				return 0, false
			}
			twin := strings.NewReplacer("x", "", "u", "").Replace(flags)
			twin = strings.TrimSuffix(twin, "-")
			switch {
			case twin != "":
				w.split(s[:2+j+1], "(?"+twin+string(rest[j]))
			case rest[j] == ':':
				w.split(s[:2+j+1], "(?:")
			default:
				w.split(s[:2+j+1], "")
			}
			return 2 + j + 1, true
		default:
			return 0, false
		}
	}
	return 0, false
}

// possessive keeps the quantifier's trailing '+' on the wire and omits it
// from the twin, which Go would read as a nested repetition.
func (deepseekDialect) possessive(w *writer) bool {
	w.split("+", "")
	return true
}

func (deepseekDialect) posixClass() bool { return true }

// classLiteral escapes the bytes the engine treats as class syntax where the
// source engines see literals. Only a doubled & or ~ is an operator, so a
// lone one stays as it is and the common "[\-.~:@+]" stays byte-identical.
// A doubled - is also a set operator there, but escaping - would turn a
// range into two literals, and no source engine gives a doubled - a useful
// meaning, so it is left alone.
func (deepseekDialect) classLiteral(w *writer, s string) {
	c := s[0]
	switch {
	case c == '[', (c == '&' || c == '~') && len(s) > 1 && s[1] == c:
		w.both(`\` + s[:1])
	default:
		w.both(s[:1])
	}
}
