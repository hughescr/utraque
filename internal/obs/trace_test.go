package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/obs"
)

// Tracing must be off unless the operator turned it on by name. A missing or
// empty UTRAQUE_TRACE_DIR is not a configuration error, it is the default.
func TestTracerDisabledByDefault(t *testing.T) {
	tr, err := obs.TracerFromEnv(func(string) string { return "" }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Enabled() || tr.Dir() != "" {
		t.Error("tracing must be off when the env var is unset")
	}
	// Every method has to tolerate the disabled tracer, or every call site
	// would need a branch.
	trace := tr.Begin("req-1")
	if trace != nil {
		t.Fatal("a disabled tracer must not begin a trace")
	}
	trace.SetRequest("POST", "/v1/messages", http.Header{})
	trace.SetBody([]byte("{}"))
	trace.SetStatus(200)
	trace.SetSummary(obs.NewSummary())
	trace.WriteDownstream([]byte("{}"))
	trace.Close()
	if got := trace.TeeUpstream(io.NopCloser(strings.NewReader("x"))); got == nil {
		t.Error("TeeUpstream on a nil trace must pass the reader through")
	}
	var buf bytes.Buffer
	if trace.TeeDownstream(&buf) != io.Writer(&buf) {
		t.Error("TeeDownstream on a nil trace must pass the writer through")
	}
}

// Turning tracing on must be loud, because the directory it creates fills with
// conversations.
func TestTracerWarnsAtStartup(t *testing.T) {
	l, buf := newBufLogger(t)
	dir := t.TempDir()
	tr, err := obs.TracerFromEnv(func(k string) string {
		if k == obs.EnvTraceDir {
			return dir
		}
		return ""
	}, l)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Enabled() || tr.Dir() != dir {
		t.Fatalf("tracer not enabled for %q", dir)
	}
	out := buf.String()
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("the trace notice must be a WARN: %s", out)
	}
	for _, want := range []string{"PROMPT TEXT", obs.EnvTraceDir} {
		if !strings.Contains(out, want) {
			t.Errorf("the trace warning omits %q: %s", want, out)
		}
	}
}

func TestTraceWritesThreeRedactedFiles(t *testing.T) {
	dir := t.TempDir()
	tr, err := obs.NewTracer(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer "+fakeJWT)
	h.Set("Content-Type", "application/json")
	h.Set("X-Api-Key", "sk-ant-api03-ABCDEFGHIJKLMNOP")

	trace := tr.Begin("req-abc")
	if trace == nil {
		t.Fatal("Begin returned nil for an enabled tracer")
	}
	ctx := obs.WithTrace(context.Background(), trace)
	if obs.TraceFrom(ctx) != trace {
		t.Fatal("WithTrace did not round-trip")
	}

	trace.SetRequest("POST", "/v1/messages", h)
	trace.SetBody([]byte(`{"model":"sol","messages":[{"role":"user","content":"the prompt"}]}`))

	upstream := trace.TeeUpstream(io.NopCloser(strings.NewReader(
		"event: response.created\ndata: {\"token\":\"" + fakeJWT + "\"}\n\n")))
	if _, err := io.Copy(io.Discard, upstream); err != nil {
		t.Fatal(err)
	}

	var client bytes.Buffer
	down := trace.TeeDownstream(&client)
	if _, err := io.WriteString(down, "event: message_start\ndata: {\"x\":1}\n\n"); err != nil {
		t.Fatal(err)
	}

	sum := obs.NewSummary()
	sum.SetRoute("codex")
	sum.SetStopReason("end_turn")
	trace.SetSummary(sum)
	trace.SetStatus(200)
	trace.Close()
	trace.Close() // idempotent

	read := func(suffix string) string {
		b, err := os.ReadFile(filepath.Join(dir, "req-abc"+suffix))
		if err != nil {
			t.Fatalf("read %s: %v", suffix, err)
		}
		return string(b)
	}

	manifest := read(obs.SuffixRequest)
	var meta struct {
		RequestID string              `json:"request_id"`
		Method    string              `json:"method"`
		Path      string              `json:"path"`
		Headers   map[string][]string `json:"headers"`
		Withheld  []string            `json:"headers_withheld"`
		Status    int                 `json:"status"`
		Summary   map[string]any      `json:"summary"`
	}
	if err := json.Unmarshal([]byte(manifest), &meta); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, manifest)
	}
	if meta.RequestID != "req-abc" || meta.Method != "POST" || meta.Path != "/v1/messages" || meta.Status != 200 {
		t.Errorf("manifest identity wrong: %+v", meta)
	}
	if _, ok := meta.Headers["content-type"]; !ok {
		t.Errorf("the allowlisted header is missing: %s", manifest)
	}
	if _, ok := meta.Headers["authorization"]; ok {
		t.Errorf("a withheld header carried a value into the trace: %s", manifest)
	}
	if len(meta.Withheld) == 0 {
		t.Errorf("withheld headers should still be named: %s", manifest)
	}
	if meta.Summary["route"] != "codex" {
		t.Errorf("the summary did not reach the manifest: %s", manifest)
	}
	// The prompt IS in the trace — that is the point, and the reason for the
	// startup warning. The credentials are not.
	if !strings.Contains(manifest, "the prompt") {
		t.Errorf("the trace should hold the prompt: %s", manifest)
	}

	upstreamDump, downstreamDump := read(obs.SuffixUpstream), read(obs.SuffixDownstream)
	if !strings.Contains(downstreamDump, "message_start") {
		t.Errorf("downstream dump missing: %s", downstreamDump)
	}
	if client.String() != "event: message_start\ndata: {\"x\":1}\n\n" {
		t.Errorf("the tee altered what the client received: %q", client.String())
	}
	for name, dump := range map[string]string{"manifest": manifest, "upstream": upstreamDump} {
		if strings.Contains(dump, fakeJWT) || strings.Contains(dump, "ABCDEFGHIJKLMNOP") {
			t.Errorf("%s trace leaked credential material: %s", name, dump)
		}
	}
}

// A caller-supplied X-Request-Id reaches the filename, so it must not be able
// to name a path outside the trace directory.
func TestTraceIDCannotEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	tr, err := obs.NewTracer(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../../etc/passwd", "a/b/c", "..", "."} {
		trace := tr.Begin(id)
		if trace == nil {
			continue
		}
		trace.SetBody([]byte(`{}`))
		trace.Close()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "/") || strings.HasPrefix(e.Name(), "..") {
			t.Errorf("a trace file escaped its name: %q", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "etc", "passwd")); err == nil {
		t.Error("a trace wrote outside its directory")
	}
}

// A non-streaming answer is a body, and is traced as one.
func TestTraceNonStreamingBody(t *testing.T) {
	dir := t.TempDir()
	tr, err := obs.NewTracer(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	trace := tr.Begin("agg-1")
	trace.WriteDownstream([]byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}]}`))
	trace.Close()

	b, err := os.ReadFile(filepath.Join(dir, "agg-1"+obs.SuffixDownstreamJSON))
	if err != nil {
		t.Fatalf("non-streaming dump missing: %v", err)
	}
	if !strings.Contains(string(b), `"msg_1"`) {
		t.Errorf("non-streaming dump wrong: %s", b)
	}
}
