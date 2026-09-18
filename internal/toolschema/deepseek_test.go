package toolschema

import (
	"encoding/json"
	"reflect"
	"testing"
)

// artifactFilePathsPattern is the pattern that produced the first real
// DeepSeek rejection: Claude Code's Artifact tool, "file_paths" items.
const artifactFilePathsPattern = `^[^\0]*$`

// The accepted forms are the rows of the empirical acceptance matrix probed
// one request at a time against the live endpoint. Each must reach the wire
// byte-identical: an over-stripped pattern loses a constraint for nothing.
func TestDeepSeekPatternUnchanged(t *testing.T) {
	unchanged := []string{
		// escapes that both engines share
		`\x7f`, `\U0001f600`, `\u0041`, `[\u0041]`, `\x{e9}`, `\x{1f600}`, `[\x{41}-\x{5a}]`,
		`\d\w\s\D\W\S`, `[\d\w\s]`, `\h`, `\H`, `[\h]`, `\e`, `[\e]`, `[\x00-\e]`, `\a`, `\f`, `\v`, `\n\t\r`,
		`\/`, `\-`, `\.`, `\'`, `\"`, `\ `, `\<`, `a/b`, "a\nb", `\cA`, `[\cA]`,
		// property classes are not expanded
		`\p{L}`, `\p{Lu}`, `\pL`, `\P{L}`, `\p{Cc}`, `[\p{L}\d]`, `[\p{L}\x00]`, `\p{Alphabetic}`,
		`\p{Cc}\p{Cf}\p{Zl}\p{Zp}`, `[\[\]]`, `[^"\\./\[\]]{1,200}`, `[a\&\&b]`, `[a&b~c]`,
		// groups and backreferences
		`(?<name>a)`, `(?P<name>a)`, `(?P<name>a)(?P=name)`, `(?<n>a)(?P=n)`, `(?<name>a)\k<name>`,
		`(?i)(?<n>a)\k<n>`, `(a)\1`, `(a|b)`, `(?#comment)a`,
		// lookaround, atomic and possessive forms
		`(?=a)b`, `(?!a)b`, `(?<=a)b`, `(?<!a)b`, `(?=\x00)a`, `^(?!\.)(?!.*\.\.)a+$`,
		`(?>a)`, `a++`, `a*+`, `a?+`, `a{2}+`,
		// anchors
		`\A`, `\z`, `\b`, `\B`, `\G`, `a\Kb`, `[\z]`, `[\A]`, `[\b]`, `[\G]`,
		// inline flags
		`(?i)abc`, `(?m)^a$`, `(?s)a.b`, `(?x) a b`, `(?u)a`, `(?U)a`, `(?i:abc)`, `(?i-s:a)`, `(?-i)a`, `(?x:a b)`,
		// bracket expressions
		`[[:alpha:]]`, `[[:^alpha:]]`, `[a-z-]`, `[]a]`, `[^]a]`, `[\x00-\x1f\x7f]`, `^[^\x00]*$`,
		// repetition
		`a{2,3}`, `a{,3}`, `a{2,}`, `^[A-Za-z0-9_=-]{1,4096}$`, `a{2000}b{2000,}`, `a{`, `a{x}`,
		// everyday schema patterns
		`^[0-9a-f]{32}$`,
		`^[A-Za-z0-9_\-.~:@+]{1,200}$`,
		`^(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,4})$`,
		`^wf_[a-z0-9-]{6,}$`,
	}
	for _, p := range unchanged {
		got, ok := translate(DeepSeek, p)
		if !ok {
			t.Errorf("%q: want accepted", p)
		} else if got != p {
			t.Errorf("%q: want unchanged, got %q", p, got)
		}
	}
}

func TestDeepSeekPatternRewritten(t *testing.T) {
	cases := map[string]string{
		artifactFilePathsPattern: `^[^\x00]*$`,
		`\0`:                     `\x00`,
		`a\0b`:                   `a\x00b`,
		`[\0-\x1f]`:              `[\x00-\x1f]`,
		`\00`:                    `\x00`,
		`\012`:                   `\x0a`,
		`\0123`:                  `\x0a3`,
		`\08`:                    `\x008`,
		`\077`:                   `\x3f`,
		`\0{2}`:                  `\x00{2}`,
		`\Aabc\Z`:                `\Aabc\z`,
		`\p{^L}`:                 `\P{L}`,
		`\P{^L}`:                 `\p{L}`,
		`[\p{^L}]`:               `[\P{L}]`,
		artifactFieldPattern:     `^(?!__.*__$)[^\p{Cc}\p{Cf}\p{Zl}\p{Zp}"\\./\[\]]{1,200}$`,
		`[a[b]]`:                 `[a\[b]]`,
		`[[]`:                    `[\[]`,
		`[a&&b~~c]`:              `[a\&&b\~~c]`,
		`[&&&]`:                  `[\&\&&]`,
		`[a-z&&[^c]]`:            `[a-z\&&\[^c]]`,
		`[[:alpha:][]`:           `[[:alpha:]\[]`,
	}
	for in, want := range cases {
		got, ok := translate(DeepSeek, in)
		if !ok {
			t.Errorf("%q: want accepted", in)
		} else if got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestDeepSeekPatternDropped(t *testing.T) {
	dropped := map[string]string{
		`\C`:              "any byte",
		`\R`:              "linebreak",
		`\Qa.b\E`:         "quoted span",
		`\y`:              "unknown letter escape",
		`\q`:              "unknown letter escape",
		`\N{DASH}`:        "named codepoint",
		`\o{101}`:         "braced octal",
		`(a)\g<1>`:        "subroutine call",
		`[\1]`:            "backreference inside class",
		`[\0\1]`:          "backreference inside class, after a rewritten NUL",
		`[\k<n>]`:         "named backreference inside class",
		`[\K]`:            "keep-out inside class",
		`\p{`:             "unterminated property",
		`\p{}`:            "empty property",
		`\p`:              "bare \\p",
		`(?)`:             "empty flag group",
		`(?i-)`:           "dangling flag negation",
		`(?i-s-m)`:        "two flag negations",
		`(?q)`:            "unknown flag",
		`(?P>n)`:          "named subroutine",
		`[abc`:            "unbalanced bracket",
		`[[:alpha:]`:      "unbalanced bracket after POSIX class",
		`[[:alpha]`:       "unterminated POSIX class",
		`a{2,1}`:          "bad repetition",
		`a{4096,10}`:      "bad repetition past the twin's clamp",
		`abc\`:            "trailing backslash",
		`(?<n`:            "unterminated named group",
		`\k<n`:            "unterminated named backreference",
		`\x{zz}`:          "bad hex escape",
		`\x{110000}`:      "codepoint past MaxRune",
		`\u00`:            "short \\u escape",
		`\c1`:             "control escape without a letter",
		`(?=a`:            "unbalanced lookahead",
		`(?=(a)b`:         "unbalanced paren inside lookahead",
		`(?<=a`:           "unbalanced lookbehind",
		`(?>a`:            "unbalanced atomic group",
		`(?=a))`:          "extra close after lookahead",
		`(?i:a`:           "unbalanced flag group",
		`(?x:a`:           "unbalanced flag group with no Go twin flag",
		`\é`:              "non-ASCII escape",
		`(?=a)b` + `)`:    "stray close paren",
		`a**`:             "double star",
		`(?<n>a)\k<n>(?=`: "unterminated trailing lookahead",
	}
	for p, why := range dropped {
		if got, ok := translate(DeepSeek, p); ok {
			t.Errorf("%q (%s): want dropped, got %q", p, why, got)
		}
	}
}

func TestDeepSeekPatternKeepsLiteralLookaroundText(t *testing.T) {
	unchanged := []string{
		`\(?=literal`,
		`\(\?=literal`,
		`[(?=]`,
	}
	for _, p := range unchanged {
		got, ok := translate(DeepSeek, p)
		if !ok {
			t.Errorf("%q: want accepted", p)
		} else if got != p {
			t.Errorf("%q: want unchanged, got %q", p, got)
		}
	}
}

// TestDeepSeekSanitizeArtifactFilePaths is the contract for the schema shape
// that failed: the Artifact tool's file_paths array, whose item pattern
// carries \0. The wire form gets \x00 and nothing else changes.
func TestDeepSeekSanitizeArtifactFilePaths(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"file_paths":{"type":"array","items":{"type":"string","minLength":1,"maxLength":1024,"pattern":"^[^\\0]*$"},"maxItems":25,"minItems":1},"url":{"type":"string","maxLength":512}}}`)
	before := string(raw)
	out, res := Sanitize(DeepSeek, raw)
	want := Result{Rewritten: []string{"properties.file_paths.items"}}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("result = %+v\nwant     %+v", res, want)
	}
	if string(raw) != before {
		t.Errorf("input schema was mutated:\n got %s\nwant %s", raw, before)
	}
	const wantOut = `{"properties":{"file_paths":{"items":{"maxLength":1024,"minLength":1,"pattern":"^[^\\x00]*$","type":"string"},"maxItems":25,"minItems":1,"type":"array"},"url":{"maxLength":512,"type":"string"}},"type":"object"}`
	if string(out) != wantOut {
		t.Errorf("wire schema:\n got %s\nwant %s", out, wantOut)
	}
}

func TestDeepSeekSanitizeUntouchedIsIdentical(t *testing.T) {
	// The Codex dialect drops this email pattern (lookahead); the DeepSeek
	// dialect passes it through, so the schema stays byte-identical.
	raw := json.RawMessage(`{"type":"object",   "properties": {"to": {"type":"string","pattern":"^(?!\\.)(?!.*\\.\\.)[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9.-]+$"}}}`)
	out, res := Sanitize(DeepSeek, raw)
	if !res.Empty() {
		t.Fatalf("result = %+v, want empty", res)
	}
	if string(out) != string(raw) {
		t.Fatalf("bytes changed on an untouched schema:\n%s\n%s", raw, out)
	}
}

func TestDeepSeekSanitizeDropsIntoDescription(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","description":"An a.","pattern":"\\Qa.b\\E"},"b":{"type":"string","pattern":"\\R"}}}`)
	out, res := Sanitize(DeepSeek, raw)
	want := Result{Dropped: []string{"properties.a", "properties.b"}}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("result = %+v\nwant     %+v", res, want)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	props := got["properties"].(map[string]any)
	a := props["a"].(map[string]any)
	if _, ok := a["pattern"]; ok {
		t.Errorf("a.pattern not removed")
	}
	if d := a["description"]; d != `An a. Must match the regular expression: \Qa.b\E` {
		t.Errorf("a.description = %q", d)
	}
	b := props["b"].(map[string]any)
	if _, ok := b["pattern"]; ok {
		t.Errorf("b.pattern not removed")
	}
	if d := b["description"]; d != `Must match the regular expression: \R` {
		t.Errorf("b.description = %q", d)
	}
}
