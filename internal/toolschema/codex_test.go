package toolschema

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// artifactFieldPattern is the pattern that produced the first real rejection:
// Claude Code's Artifact tool, "field" parameter.
const artifactFieldPattern = `^(?!__.*__$)[^\p{Cc}\p{Cf}\p{Zl}\p{Zp}"\\./[\]]{1,200}$`

func TestCodexPatternUnchanged(t *testing.T) {
	unchanged := []string{
		`^[0-9a-f]{32}$`,
		`^[A-Za-z0-9_\-.~:@+]{1,200}$`,
		`^(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,4})$`,
		`^wf_[a-z0-9-]{6,}$`,
		`(?i)^abc$`,
		`(?is:a.b)c`,
		`(?P<name>\w+)(?P=name)`,
		`\x41\u0041`,
		`[^"\\./[\]]`,
		`[]a]`,
		`[^]a]`,
		`a+?b*?c??`,
		`^\.?$`,
		`(a)\1`,
		`\Aabc\Z`,
		`(?#note)a`,
		`^[A-Za-z0-9_=-]{1,4096}$`, // past Go's repeat limit; the twin is clamped
		`a{2000}b{2000,}`,
		`a{`,
		`a{x}`,
	}
	for _, p := range unchanged {
		got, ok := translate(Codex, p)
		if !ok {
			t.Errorf("%q: want accepted", p)
		} else if got != p {
			t.Errorf("%q: want unchanged, got %q", p, got)
		}
	}
}

func TestCodexPatternRewritten(t *testing.T) {
	cases := map[string]string{
		`\p{Zl}`:              `[\u2028]`,
		`\P{Zl}`:              `[^\u2028]`,
		`\p{^Zl}`:             `[^\u2028]`,
		`[\p{Zl}\p{Zp}]`:      `[\u2028\u2029]`,
		`[^\P{Zl}]`:           `[^\x00-\u2027\u2029-\U0010ffff]`,
		`\p{Cc}`:              `[\x00-\x1f\x7f-\x9f]`,
		`(?<name>x)`:          `(?P<name>x)`,
		`(?<n>a)\k<n>`:        `(?P<n>a)(?P=n)`,
		`\Aabc\z`:             `\Aabc\Z`,
		`\x{41}\x{1F600}`:     `\x41\U0001f600`,
		`[\x{2028}-\x{2029}]`: `[\u2028-\u2029]`,
	}
	for in, want := range cases {
		got, ok := translate(Codex, in)
		if !ok {
			t.Errorf("%q: want accepted", in)
		} else if got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestCodexPatternDropped(t *testing.T) {
	dropped := map[string]string{
		artifactFieldPattern:   "lookahead in the Artifact pattern",
		`(?=a)b`:               "positive lookahead",
		`(?!a)b`:               "negative lookahead",
		`(?<=a)b`:              "positive lookbehind",
		`(?<!a)b`:              "negative lookbehind",
		`^(?!\.)(?!.*\.\.)a+$`: "multiple lookaheads",
		`\p{L}+`:               "letter category: too many ranges",
		`[\p{Nd}]`:             "decimal digits: too many ranges",
		`\p{Nope}`:             "unknown property",
		`\p{`:                  "unterminated property",
		`\p`:                   "bare \\p",
		`(?>ab)`:               "atomic group",
		`\Qa.b\E`:              "quoted span",
		`[[:alpha:]]+`:         "POSIX class",
		`\C`:                   "any byte",
		`\Ga`:                  "match-start anchor",
		`a++b`:                 "possessive quantifier",
		`(?x)a b`:              "verbose flag",
		`(?)`:                  "empty flag group",
		`[abc`:                 "unbalanced bracket",
		`a{2,1}`:               "bad repetition",
		`a{4096,10}`:           "bad repetition past the twin's clamp",
		`abc\`:                 "trailing backslash",
		`(?<n`:                 "unterminated named group",
		`\k<n`:                 "unterminated named backreference",
		`\x{zz}`:               "bad hex escape",
		`\x{110000}`:           "codepoint past MaxRune",
		`[\1]`:                 "backreference inside class",
	}
	for p, why := range dropped {
		if got, ok := translate(Codex, p); ok {
			t.Errorf("%q (%s): want dropped, got %q", p, why, got)
		}
	}
}

func TestCodexPatternKeepsLiteralLookaroundText(t *testing.T) {
	unchanged := []string{
		`\(?=literal`,
		`\(\?=literal`,
		`[(?=]`,
	}
	for _, p := range unchanged {
		got, ok := translate(Codex, p)
		if !ok {
			t.Errorf("%q: want accepted", p)
		} else if got != p {
			t.Errorf("%q: want unchanged, got %q", p, got)
		}
	}
}

// TestToPythonPatternCompilesInPython keeps the legacy rewrite spellings
// syntactically valid in Python. It is not a Codex-validator contract;
// lookaround is separately rejected before a pattern is emitted. It is skipped
// when no python3 is on PATH.
func TestCodexPatternCompilesInPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	inputs := []string{
		`\p{Zl}`, `\P{Zl}`, `[\p{Zl}\p{Zp}]`, `[^\P{Zl}]`, `\p{Cc}`, `\p{Cf}`,
		`(?<name>x)`, `(?<n>a)\k<n>`, `\Aabc\z`, `\x{41}\x{1F600}`,
		`(a)\1`, `(?i)^abc$`, `(?is:a.b)c`,
	}
	var patterns []string
	for _, in := range inputs {
		out, ok := translate(Codex, in)
		if !ok {
			t.Fatalf("%q: want accepted", in)
		}
		patterns = append(patterns, out)
	}
	blob, _ := json.Marshal(patterns)
	script := `import json,re,sys
bad=[]
for p in json.load(sys.stdin):
    try: re.compile(p)
    except re.error as e: bad.append(f"{p!r}: {e}")
print("\n".join(bad))
sys.exit(1 if bad else 0)`
	cmd := exec.Command(py, "-c", script)
	cmd.Stdin = strings.NewReader(string(blob))
	outb, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python rejected translated patterns:\n%s", outb)
	}
}

func TestCodexSanitizeUntouchedIsIdentical(t *testing.T) {
	raw := json.RawMessage(`{"type":"object",   "properties": {"id": {"type":"string","pattern":"^[0-9a-f]{32}$"}}}`)
	out, res := Sanitize(Codex, raw)
	if !res.Empty() {
		t.Fatalf("result = %+v, want empty", res)
	}
	if string(out) != string(raw) {
		t.Fatalf("bytes changed on an untouched schema:\n%s\n%s", raw, out)
	}
	for _, bad := range []json.RawMessage{nil, json.RawMessage(`[]`), json.RawMessage(`not json`)} {
		out, res := Sanitize(Codex, bad)
		if string(out) != string(bad) || !res.Empty() {
			t.Errorf("%q: want passthrough, got %q %+v", bad, out, res)
		}
	}
}

func TestCodexSanitizeRewrites(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"field": {"type": "string", "description": "the field.", "pattern": "^(?!__.*__$)[^\\p{Cc}]{1,200}$"},
			"bare": {"type": "string", "pattern": "\\p{L}+"},
			"keep": {"type": "string", "pattern": "^[a-z]+$"},
			"list": {"type": "array", "items": {"type": "object", "properties": {"doc": {"type": "string", "pattern": "(?<d>x)"}}}},
			"tuple": {"type": "array", "items": [{"type": "string", "pattern": "(?>x)"}]},
			"either": {"anyOf": [{"type": "string"}, {"type": "string", "pattern": "\\p{Zl}"}]}
		},
		"$defs": {"D": {"type": "string", "pattern": "[[:digit:]]"}},
		"patternProperties": {"^x": {"type": "string", "pattern": "a++"}},
		"examples": [{"pattern": "\\p{L}"}]
	}`)
	out, res := Sanitize(Codex, raw)
	want := Result{
		Rewritten: []string{"properties.either.anyOf.1", "properties.list.items.properties.doc"},
		Dropped:   []string{"$defs.D", "patternProperties.^x", "properties.bare", "properties.field", "properties.tuple.items.0"},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("result = %+v\nwant     %+v", res, want)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	props := got["properties"].(map[string]any)
	field := props["field"].(map[string]any)
	if _, ok := field["pattern"]; ok {
		t.Errorf("field.pattern not removed")
	}
	if d := field["description"]; d != `the field. Must match the regular expression: ^(?!__.*__$)[^\p{Cc}]{1,200}$` {
		t.Errorf("field.description = %q", d)
	}
	bare := props["bare"].(map[string]any)
	if _, ok := bare["pattern"]; ok {
		t.Errorf("bare.pattern not removed")
	}
	if d := bare["description"]; d != `Must match the regular expression: \p{L}+` {
		t.Errorf("bare.description = %q", d)
	}
	tuple := props["tuple"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if d := tuple["description"]; d != `Must match the regular expression: (?>x)` {
		t.Errorf("tuple item description = %q", d)
	}
	if p := props["keep"].(map[string]any)["pattern"]; p != "^[a-z]+$" {
		t.Errorf("keep.pattern = %q, want kept", p)
	}
	// Data-carrying keywords are not schemas and must not be rewritten.
	ex := got["examples"].([]any)[0].(map[string]any)
	if ex["pattern"] != `\p{L}` {
		t.Errorf("examples[0] was rewritten: %v", ex)
	}
}
