package obs_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/obs"
)

func TestNewHandlerFormats(t *testing.T) {
	var buf bytes.Buffer
	for _, f := range []string{"", "json", "JSON", "text", " Text "} {
		buf.Reset()
		l, err := obs.NewLogger(&buf, slog.LevelInfo, f)
		if err != nil {
			t.Fatalf("format %q: %v", f, err)
		}
		l.Info("hello")
		if !strings.Contains(buf.String(), "hello") {
			t.Errorf("format %q produced %q", f, buf.String())
		}
	}
	if _, err := obs.NewHandler(&buf, slog.LevelInfo, "logfmt"); err == nil {
		t.Error("want an error for an unknown format")
	}
	if _, err := obs.NewLogger(&buf, slog.LevelInfo, "logfmt"); err == nil {
		t.Error("NewLogger must propagate the format error")
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l, err := obs.NewLogger(&buf, slog.LevelWarn, "json")
	if err != nil {
		t.Fatal(err)
	}
	l.Debug("nope-debug")
	l.Info("nope-info")
	l.Warn("yes-warn")
	s := buf.String()
	if strings.Contains(s, "nope") || !strings.Contains(s, "yes-warn") {
		t.Errorf("level filtering wrong: %s", s)
	}
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	if obs.LoggerFrom(ctx) != slog.Default() {
		t.Error("LoggerFrom must fall back to slog.Default")
	}
	//nolint:staticcheck // deliberately exercising the nil-context guard
	//lint:ignore SA1012 deliberately exercising LoggerFrom's nil-context guard
	if obs.LoggerFrom(nil) != slog.Default() {
		t.Error("LoggerFrom(nil) must fall back to slog.Default")
	}
	if obs.RequestIDFrom(ctx) != "" {
		t.Error("RequestIDFrom must be empty by default")
	}

	var buf bytes.Buffer
	l, err := obs.NewLogger(&buf, slog.LevelInfo, "json")
	if err != nil {
		t.Fatal(err)
	}
	withLog := obs.WithLogger(ctx, l)
	withID := obs.WithRequestID(withLog, "rid-1")
	if obs.LoggerFrom(withID) != l {
		t.Error("LoggerFrom did not round-trip")
	}
	if obs.RequestIDFrom(withID) != "rid-1" {
		t.Error("RequestIDFrom did not round-trip")
	}
	if obs.WithLogger(withID, nil) != withID {
		t.Error("WithLogger(nil) must be a no-op")
	}
	if obs.WithRequestID(withID, "") != withID {
		t.Error(`WithRequestID("") must be a no-op`)
	}
}

func TestRedactorHidesSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-ant-oat01-SUPERSECRET")
	h.Set("X-Api-Key", "sk-ant-api03-ALSOSECRET")
	h.Set("Cookie", "session=SECRETCOOKIE")
	h.Set("X-Utraque-Token", "LOCALSECRET")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "claude-cli/2.1.0")
	h.Add("Anthropic-Beta", "oauth-2025-04-20")
	h.Add("Anthropic-Beta", "context-1m-2025-08-07")
	h.Set("X-Stainless-Lang", "js")

	var buf bytes.Buffer
	l, err := obs.NewLogger(&buf, slog.LevelInfo, "json")
	if err != nil {
		t.Fatal(err)
	}
	l.LogAttrs(context.Background(), slog.LevelInfo, "req",
		slog.Attr{Key: "headers", Value: obs.DefaultRedactor().Header(h)})
	out := buf.String()

	for _, leak := range []string{"SUPERSECRET", "ALSOSECRET", "SECRETCOOKIE", "LOCALSECRET", "Bearer"} {
		if strings.Contains(out, leak) {
			t.Fatalf("redactor leaked %q: %s", leak, out)
		}
	}
	for _, keep := range []string{"application/json", "claude-cli/2.1.0", "oauth-2025-04-20", "context-1m-2025-08-07", `"js"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("redactor dropped the allowlisted value %q: %s", keep, out)
		}
	}
	for _, named := range []string{"authorization", "x-api-key", "cookie", "x-utraque-token"} {
		if !strings.Contains(out, named) {
			t.Errorf("withheld header %q should still be named: %s", named, out)
		}
	}
}

func TestRedactorAllowed(t *testing.T) {
	r := obs.DefaultRedactor()
	for _, ok := range []string{"content-type", "Content-Type", " anthropic-beta ", "x-stainless-anything"} {
		if !r.Allowed(ok) {
			t.Errorf("Allowed(%q) = false", ok)
		}
	}
	for _, no := range []string{"authorization", "x-api-key", "cookie", "x-utraque-token", "x-secret"} {
		if r.Allowed(no) {
			t.Errorf("Allowed(%q) = true", no)
		}
	}
	if obs.NewRedactor("authorization", "", "  ").Allowed("authorization") {
		t.Error("always-deny must beat an explicit allow")
	}

	var nilR *obs.Redactor
	if nilR.Allowed("content-type") {
		t.Error("a nil Redactor must allow nothing")
	}
	if len(nilR.Header(http.Header{"Content-Type": {"x"}}).Group()) != 0 {
		t.Error("a nil Redactor must render an empty group")
	}
	if len(obs.DefaultRedactor().Header(nil).Group()) != 0 {
		t.Error("an empty header set must render an empty group")
	}
}

func TestAttrsMatchesHeader(t *testing.T) {
	h := http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer x"}}
	attrs := obs.DefaultRedactor().Attrs(h)
	if len(attrs) != 2 {
		t.Fatalf("Attrs = %d attrs, want content-type plus redacted", len(attrs))
	}
}

func TestTruncation(t *testing.T) {
	h := http.Header{"Content-Type": {strings.Repeat("a", obs.MaxValueLen*2)}}
	g := obs.DefaultRedactor().Header(h).Group()
	if len(g) != 1 {
		t.Fatalf("want 1 attr, got %d", len(g))
	}
	if v := g[0].Value.String(); !strings.HasSuffix(v, "...(truncated)") {
		t.Errorf("value not truncated: len=%d", len(v))
	}
}

func TestHash(t *testing.T) {
	if obs.Hash("") != "" {
		t.Error(`Hash("") must be ""`)
	}
	got := obs.Hash("account-123")
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+8 {
		t.Fatalf("Hash = %q", got)
	}
	if strings.Contains(got, "account-123") {
		t.Error("Hash leaked its input")
	}
	if got == obs.Hash("account-124") {
		t.Error("Hash collides on distinct inputs")
	}
	if obs.HashAttr("k", "").Key != "" {
		t.Error("HashAttr must be empty for an empty value")
	}
	if a := obs.HashAttr("k", "v"); a.Key != "k" || a.Value.String() != obs.Hash("v") {
		t.Errorf("HashAttr = %v", a)
	}
}

func TestSafePath(t *testing.T) {
	if obs.SafePath(nil) != "" {
		t.Error("SafePath(nil) must be empty")
	}
	u, err := url.Parse("https://h/v1/messages?key=sk-leak")
	if err != nil {
		t.Fatal(err)
	}
	if got := obs.SafePath(u); got != "/v1/messages" {
		t.Errorf("SafePath = %q, want the query dropped", got)
	}
}
