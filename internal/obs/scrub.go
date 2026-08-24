package obs

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted is what replaces anything credential-shaped.
const Redacted = "[REDACTED]"

// deniedAttrKeys are attribute keys whose value is never logged, whatever it
// holds. The allowlist governs HEADERS; this governs everything a call site
// might name directly, so `slog.String("access_token", tok)` is impossible to
// write into a log even by accident.
//
// Keys are compared after lowercasing and folding '-' to '_', so
// "Refresh-Token", "refresh_token" and "REFRESH_TOKEN" are one key. A key is
// denied too when a denied name is its trailing underscore-separated segment
// group: "codex_token" is denied by "token", "upstream_client_secret" by
// "client_secret". Namespacing a field with a prefix is the obvious way to
// write the mistake this list exists to make unwritable, and an exact-match
// denylist did not stop it. Matching the TAIL rather than any substring is what
// keeps "output_tokens" — plural, and a count — a perfectly loggable field.
var deniedAttrKeys = map[string]struct{}{
	"access_token":  {},
	"refresh_token": {},
	"id_token":      {},
	"authorization": {},
	"api_key":       {},
	"apikey":        {},
	"client_secret": {},
	"cookie":        {},
	"credential":    {},
	"password":      {},
	"secret":        {},
	"token":         {},
	"x_api_key":     {},
	"bearer":        {},
}

// hashedAttrKeys name values that are allowed through only as a hash prefix.
// The Codex account id is the whole population: it is useful for correlating
// two log lines to one subscription and useless to anyone who cannot already
// see the account, so it is fingerprinted rather than withheld outright. A
// value that is already a hash is left alone.
var hashedAttrKeys = map[string]struct{}{
	"account":            {},
	"account_id":         {},
	"chatgpt_account_id": {},
}

// scrubRE matches the shapes a credential takes in this codebase's blast
// radius. It is a backstop, not the primary defence — the primary defence is
// that nothing ever passes a token to a logger — so it errs towards matching:
//
//   - "Bearer <anything>" — the Anthropic OAuth credential the client presents
//     and the Codex access token utraque signs with.
//   - a JWT — the Codex access token is one, and its "exp" is decoded all over
//     the auth package.
//   - an sk- key — Anthropic and OpenAI API keys, including sk-ant-oat01-*.
//   - a JSON or query-string field named for a token — auth.json bodies and
//     OAuth token-endpoint exchanges.
var scrubRE = regexp.MustCompile(strings.Join([]string{
	`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`,
	`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}(?:\.[A-Za-z0-9_-]*)?`,
	`\bsk-[A-Za-z0-9._-]{12,}`,
	`(?i)"(?:access_token|refresh_token|id_token|api_key|client_secret|authorization|password|secret)"\s*:\s*"[^"]*"`,
	`(?i)\b(?:access_token|refresh_token|id_token|api_key|client_secret)=[^&\s"']+`,
}, "|"))

// minScrubLen skips strings too short to hold any of the shapes above.
const minScrubLen = 8

// Scrub replaces credential-shaped substrings of s with Redacted. A JSON field
// or query parameter keeps its NAME — only the value is replaced — so a scrubbed
// trace dump is still parseable JSON.
func Scrub(s string) string {
	if len(s) < minScrubLen {
		return s
	}
	return scrubRE.ReplaceAllStringFunc(s, redactMatch)
}

// ScrubBytes is Scrub over a byte slice. It returns b unchanged (not a copy)
// when there is nothing to redact.
func ScrubBytes(b []byte) []byte {
	if len(b) < minScrubLen {
		return b
	}
	if !scrubRE.Match(b) {
		return b
	}
	return scrubRE.ReplaceAllFunc(b, func(m []byte) []byte {
		return []byte(redactMatch(string(m)))
	})
}

// redactMatch replaces one match, preserving the field name when the match was
// a `"key": "value"` or `key=value` pair.
func redactMatch(m string) string {
	if strings.HasPrefix(m, `"`) {
		if i := strings.IndexByte(m, ':'); i > 0 {
			return m[:i+1] + ` "` + Redacted + `"`
		}
	}
	if !strings.HasPrefix(strings.ToLower(m), "bearer") {
		if i := strings.IndexByte(m, '='); i > 0 {
			return m[:i+1] + Redacted
		}
	}
	return Redacted
}

// DeniedAttrKey reports whether an attribute with this key may never carry a
// value into the log. A denied name matches the whole key or its trailing
// underscore-separated segments, so a namespaced "codex_token" is denied and a
// merely similar "output_tokens" is not.
func DeniedAttrKey(key string) bool {
	k := normalizeAttrKey(key)
	if _, bad := deniedAttrKeys[k]; bad {
		return true
	}
	for i := 0; i < len(k); i++ {
		if k[i] != '_' {
			continue
		}
		if _, bad := deniedAttrKeys[k[i+1:]]; bad {
			return true
		}
	}
	return false
}

// HashedAttrKey reports whether an attribute with this key is logged only as a
// hash prefix.
func HashedAttrKey(key string) bool {
	_, h := hashedAttrKeys[normalizeAttrKey(key)]
	return h
}

func normalizeAttrKey(key string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "-", "_")
}

// scrubHandler is a slog.Handler middleware that rewrites every record on its
// way to the real handler: an attr whose KEY names a credential loses its
// value, and every string-shaped value is passed through Scrub.
//
// This is what makes the redaction rule structural rather than a convention.
// obs cannot stop another package from calling slog with the wrong thing; it
// can make the wrong thing not reach the output.
type scrubHandler struct{ next slog.Handler }

var _ slog.Handler = scrubHandler{}

// NewScrubHandler wraps next. A nil next yields a nil handler, so callers can
// pass a handler through unconditionally.
func NewScrubHandler(next slog.Handler) slog.Handler {
	if next == nil {
		return nil
	}
	if _, already := next.(scrubHandler); already {
		return next
	}
	return scrubHandler{next: next}
}

func (h scrubHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h scrubHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(scrubAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h scrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return scrubHandler{next: h.next.WithAttrs(scrubAttrs(attrs))}
}

func (h scrubHandler) WithGroup(name string) slog.Handler {
	return scrubHandler{next: h.next.WithGroup(name)}
}

func scrubAttrs(in []slog.Attr) []slog.Attr {
	if len(in) == 0 {
		return in
	}
	out := make([]slog.Attr, len(in))
	for i, a := range in {
		out[i] = scrubAttr(a)
	}
	return out
}

func scrubAttr(a slog.Attr) slog.Attr {
	if DeniedAttrKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	if HashedAttrKey(a.Key) {
		v := a.Value.Resolve()
		if v.Kind() == slog.KindString {
			s := v.String()
			if s != "" && !strings.HasPrefix(s, hashPrefix) {
				return slog.String(a.Key, Hash(s))
			}
		}
		return slog.Attr{Key: a.Key, Value: v}
	}
	a.Value = scrubValue(a.Value)
	return a
}

func scrubValue(v slog.Value) slog.Value {
	// Resolve first: a LogValuer's rendered attrs are what actually reach the
	// output, so scrubbing before resolution would miss them entirely.
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(Scrub(v.String()))
	case slog.KindGroup:
		return slog.GroupValue(scrubAttrs(v.Group())...)
	case slog.KindAny:
		return scrubAny(v)
	default:
		return v
	}
}

// scrubAny handles the value shapes a call site actually produces with
// slog.Any: an error, a string slice, a raw byte slice, a Stringer, or a string
// map. A map is rewritten in place — same keys, same shape — because a nested
// key naming a credential is otherwise invisible to the top-level key check.
// Anything else (a map of counters, a bool slice) is left alone: rewriting it
// would change the JSON shape a reader depends on, and none of those shapes can
// hold a credential the string paths would not already have caught.
func scrubAny(v slog.Value) slog.Value {
	switch x := v.Any().(type) {
	case error:
		if x == nil {
			return v
		}
		return slog.StringValue(Scrub(x.Error()))
	case []byte:
		return slog.StringValue(Scrub(string(x)))
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = Scrub(s)
		}
		return slog.AnyValue(out)
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, s := range x {
			out[k] = scrubMapValue(k, s)
		}
		return slog.AnyValue(out)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, av := range x {
			if s, ok := av.(string); ok {
				out[k] = scrubMapValue(k, s)
				continue
			}
			out[k] = av
		}
		return slog.AnyValue(out)
	case fmt.Stringer:
		if x == nil {
			return v
		}
		return slog.StringValue(Scrub(x.String()))
	default:
		return v
	}
}

// scrubMapValue applies the attr rules to one entry of a logged map: a denied
// key loses its value outright, everything else is scrubbed by shape.
func scrubMapValue(key, val string) string {
	if DeniedAttrKey(key) {
		return Redacted
	}
	return Scrub(val)
}
