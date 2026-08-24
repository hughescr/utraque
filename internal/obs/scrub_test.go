package obs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/obs"
)

// A realistic-looking Codex access token: the JWT shape the auth package
// decodes an "exp" out of.
const fakeJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
	"eyJleHAiOjIwMDAwMDAwMDAsInN1YiI6ImZha2UifQ." +
	"c2lnbmF0dXJlLXRoYXQtaXMtbm90LXJlYWw"

func newBufLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l, err := obs.NewLogger(&buf, slog.LevelDebug, "json")
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

func TestScrubRedactsTokenShapes(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		leak  string
		keeps string
	}{
		{"bearer", "Authorization: Bearer sk-ant-oat01-SUPERSECRETVALUE", "SUPERSECRETVALUE", ""},
		{"jwt", "token is " + fakeJWT + " expiring soon", "eyJleHAiOjIwMDAwMDAwMDAs", "expiring soon"},
		{"sk key", "key sk-ant-api03-ABCDEFGHIJKLMNOP failed", "ABCDEFGHIJKLMNOP", "failed"},
		{"json field", `{"access_token":"` + fakeJWT + `","other":"kept"}`, fakeJWT, `"other"`},
		{"query field", "POST /token?refresh_token=rt_abcdefghijklmnop&x=1", "rt_abcdefghijklmnop", "x=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := obs.Scrub(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Errorf("Scrub leaked %q: %s", tc.leak, got)
			}
			if !strings.Contains(got, obs.Redacted) {
				t.Errorf("Scrub did not mark a redaction: %s", got)
			}
			if tc.keeps != "" && !strings.Contains(got, tc.keeps) {
				t.Errorf("Scrub dropped the surrounding context %q: %s", tc.keeps, got)
			}
		})
	}
}

// A scrubbed JSON body must still be JSON: a trace dump is meant to be replayed
// as a fixture, and a redaction that broke the syntax would make it unusable.
func TestScrubKeepsJSONParseable(t *testing.T) {
	in := []byte(`{"tokens":{"access_token":"` + fakeJWT + `","refresh_token":"rt-x"},"model":"sol"}`)
	got := string(obs.ScrubBytes(in))
	if strings.Contains(got, fakeJWT) {
		t.Fatalf("ScrubBytes leaked the token: %s", got)
	}
	for _, want := range []string{`"access_token":`, `"model":"sol"`} {
		if !strings.Contains(got, want) {
			t.Errorf("ScrubBytes dropped %q: %s", want, got)
		}
	}
}

func TestScrubHandlerDeniesKeysWhateverTheValue(t *testing.T) {
	l, buf := newBufLogger(t)
	l.Info("creds",
		slog.String("access_token", "plainlookingvalue"),
		slog.String("refresh_token", "another"),
		slog.String("id_token", "third"),
		slog.String("Authorization", "whatever"),
		slog.String("api_key", "fourth"),
		slog.String("model", "sol"),
	)
	out := buf.String()
	for _, leak := range []string{"plainlookingvalue", "another", "third", "whatever", "fourth"} {
		if strings.Contains(out, leak) {
			t.Errorf("a denied key leaked its value %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"model":"sol"`) {
		t.Errorf("an ordinary attr was lost: %s", out)
	}
}

// A denied name is denied under a namespacing prefix too. This is the gap an
// exact-match denylist left: "token" was denied, "codex_token" was not, and a
// refresh token has no shape the regex backstop recognises — so a plausible
// call site put one in the clear through BOTH layers.
func TestDeniedKeysMatchNamespacedNames(t *testing.T) {
	denied := []string{
		"codex_token", "client_access_token", "upstream_client_secret",
		"Codex-Refresh-Token", "x_api_key", "the_password", "my_cookie",
	}
	for _, k := range denied {
		if !obs.DeniedAttrKey(k) {
			t.Errorf("DeniedAttrKey(%q) = false, want true", k)
		}
	}
	// The tail must be a whole segment group, so ordinary fields survive. These
	// are real fields on the request line and losing them would be a regression
	// in the opposite direction.
	allowed := []string{
		"output_tokens", "input_tokens", "tokenizer", "secretary",
		"account_id", "transport", "stop_reason", "req_bytes",
	}
	for _, k := range allowed {
		if obs.DeniedAttrKey(k) {
			t.Errorf("DeniedAttrKey(%q) = true, want false", k)
		}
	}
}

// The same rule has to hold through the handler, since that is the layer a call
// site actually goes through.
func TestScrubHandlerDeniesNamespacedKeys(t *testing.T) {
	l, buf := newBufLogger(t)
	l.Info("creds",
		slog.String("codex_token", "rt_LFRDkQ3mSecretRefreshTokenValue0123456789"),
		slog.Any("meta", map[string]string{
			"refresh_token": "rt_MAPSECRETVALUE0123456789",
			"model":         "sol",
		}),
		slog.Int("output_tokens", 42),
	)
	out := buf.String()
	for _, leak := range []string{"rt_LFRDkQ3mSecret", "rt_MAPSECRETVALUE"} {
		if strings.Contains(out, leak) {
			t.Errorf("a namespaced or nested denied key leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"output_tokens":42`) {
		t.Errorf("a count field was wrongly redacted: %s", out)
	}
	if !strings.Contains(out, `"model":"sol"`) {
		t.Errorf("an ordinary map entry was lost: %s", out)
	}
}

// The account id is correlatable but not disclosable, so it is hashed rather
// than withheld — and a value that is already a hash is left alone.
func TestScrubHandlerHashesAccountIDs(t *testing.T) {
	l, buf := newBufLogger(t)
	l.Info("acct",
		slog.String("account_id", "acct-real-12345"),
		slog.String("account", obs.Hash("acct-real-12345")),
	)
	out := buf.String()
	if strings.Contains(out, "acct-real-12345") {
		t.Fatalf("account id logged in the clear: %s", out)
	}
	if n := strings.Count(out, obs.Hash("acct-real-12345")); n != 2 {
		t.Errorf("want both account fields as the same hash, got %d occurrences: %s", n, out)
	}
}

// Groups, errors and LogValuers are the shapes real call sites use, so the
// scrubber has to reach inside all of them.
func TestScrubHandlerReachesNestedValues(t *testing.T) {
	l, buf := newBufLogger(t)
	l.Info("nested",
		slog.Group("headers", slog.String("authorization", "Bearer "+fakeJWT)),
		slog.Any("err", errors.New("refresh failed for Bearer "+fakeJWT)),
		slog.Any("values", []string{"ok", "sk-ant-api03-ABCDEFGHIJKLMNOP"}),
		slog.Any("body", []byte(`{"access_token":"`+fakeJWT+`"}`)),
	)
	out := buf.String()
	for _, leak := range []string{fakeJWT, "ABCDEFGHIJKLMNOP", "Bearer ey"} {
		if strings.Contains(out, leak) {
			t.Errorf("scrub handler leaked %q: %s", leak, out)
		}
	}
}

// WithAttrs and WithGroup must keep the wrapping: a logger derived once at
// startup and used forever is the common case, and it must not shed the
// scrubber on the way.
func TestScrubHandlerSurvivesDerivation(t *testing.T) {
	l, buf := newBufLogger(t)
	derived := l.With(slog.String("access_token", "bound-at-startup")).WithGroup("g")
	derived.Info("hello", slog.String("authorization", "Bearer "+fakeJWT))
	out := buf.String()
	for _, leak := range []string{"bound-at-startup", fakeJWT} {
		if strings.Contains(out, leak) {
			t.Errorf("a derived logger leaked %q: %s", leak, out)
		}
	}
}

func TestSummaryIsNilSafeAndRendersFields(t *testing.T) {
	var nilSum *obs.Summary
	nilSum.SetRoute("codex")
	nilSum.SetModels("sol", "gpt-5.6-sol")
	nilSum.SetInterrupted(true)
	if nilSum.Attrs() != nil || nilSum.Route() != "" {
		t.Error("a nil Summary must render nothing")
	}
	if obs.SummaryFrom(context.Background()) != nil {
		t.Error("SummaryFrom must be nil when nothing attached one")
	}

	sum := obs.NewSummary()
	ctx := obs.WithSummary(context.Background(), sum)
	if obs.SummaryFrom(ctx) != sum {
		t.Fatal("WithSummary did not round-trip")
	}
	sum.SetRoute("codex")
	sum.SetModels("sol", "gpt-5.6-sol")
	sum.SetEffort("high")
	sum.SetStream(true)
	sum.SetReqBytes(128)
	sum.SetReqBytes(-1) // ignored: an undeclared Content-Length is not a size
	sum.SetUpstreamStatus(200)
	sum.SetOutputTokens(42)
	sum.SetStopReason("end_turn")
	sum.SetTransport("std")
	sum.SetInterrupted(false) // never latches off
	sum.SetErr(nil)

	got := map[string]any{}
	for _, a := range sum.Attrs() {
		got[a.Key] = a.Value.Any()
	}
	for k, want := range map[string]any{
		"route": "codex", "client_model": "sol", "upstream_model": "gpt-5.6-sol",
		"effort": "high", "stream": true, "req_bytes": int64(128),
		"upstream_status": int64(200), "output_tokens": int64(42), "stop_reason": "end_turn",
		"interrupted": false, "transport": "std",
	} {
		if got[k] != want {
			t.Errorf("summary[%q] = %v, want %v", k, got[k], want)
		}
	}
	if _, present := got["err"]; present {
		t.Error("a nil error must not produce an err field")
	}
}
