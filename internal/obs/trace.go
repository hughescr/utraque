package obs

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
	"sync"
)

// EnvTraceDir turns on per-request trace dumps and names the directory they are
// written to. It is deliberately its OWN environment variable rather than a
// log level: raising the log level should never start writing prompt text to
// disk, and turning tracing on should be an act with a name.
const EnvTraceDir = "UTRAQUE_TRACE_DIR"

// TraceWarning is the startup notice tracing prints. It is a WARN, not an
// INFO, because a directory of conversations in the clear is a standing risk
// for as long as it exists.
const TraceWarning = "REQUEST TRACING IS ENABLED: trace dumps contain PROMPT TEXT and model output in the clear. " +
	"Credentials are redacted, the conversation is NOT. Unset " + EnvTraceDir + " to turn this off."

// Trace file suffixes. Three files per request, named by request id, so a
// trace can be replayed as a test fixture:
//
//	<id>.request.json    the inbound request: method, path, allowlisted
//	                     headers, and the body as it was received
//	<id>.upstream.sse    the raw upstream stream, byte for byte
//	<id>.downstream.sse  the translated stream this proxy wrote back
//	<id>.downstream.json a non-streaming answer, which is a body and not a
//	                     stream, so it is not pretended to be one
const (
	SuffixRequest        = ".request.json"
	SuffixUpstream       = ".upstream.sse"
	SuffixDownstream     = ".downstream.sse"
	SuffixDownstreamJSON = ".downstream.json"
)

// Tracer writes per-request trace dumps into a directory. A nil *Tracer is a
// disabled tracer and every method tolerates it, so a call site never branches
// on whether tracing is on.
type Tracer struct {
	dir string
	log *slog.Logger
	red *Redactor
}

// NewTracer builds a Tracer writing into dir, creating it if needed, and logs
// the loud warning. An empty dir returns a nil Tracer and no error: tracing off
// is not a failure.
func NewTracer(dir string, log *slog.Logger) (*Tracer, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	log.Warn(TraceWarning, slog.String("trace_dir", dir))
	return &Tracer{dir: dir, log: log, red: DefaultRedactor()}, nil
}

// TracerFromEnv builds a Tracer from EnvTraceDir.
func TracerFromEnv(getenv func(string) string, log *slog.Logger) (*Tracer, error) {
	if getenv == nil {
		return nil, nil
	}
	return NewTracer(getenv(EnvTraceDir), log)
}

// Enabled reports whether traces are being written.
func (t *Tracer) Enabled() bool { return t != nil && t.dir != "" }

// Dir is the trace directory, or "" when tracing is off. It is not a secret.
func (t *Tracer) Dir() string {
	if t == nil {
		return ""
	}
	return t.dir
}

// Begin opens a trace for one request. It returns nil when tracing is off.
func (t *Tracer) Begin(id string) *Trace {
	if !t.Enabled() {
		return nil
	}
	name := safeFileID(id)
	if name == "" {
		return nil
	}
	return &Trace{tracer: t, id: name, meta: traceMeta{RequestID: id}}
}

// safeFileID reduces a request id to something that can only ever name a file
// inside the trace directory. A caller-supplied X-Request-Id reaches this
// function, so path separators and dot segments must not survive it.
func safeFileID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if len(id) > 128 {
		id = id[:128]
	}
	var b strings.Builder
	for i := range len(id) {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if strings.Trim(out, "_") == "" {
		return ""
	}
	return out
}

// traceMeta is the <id>.request.json shape. It doubles as a test fixture
// manifest: everything needed to replay the request is here, and nothing that
// would be unsafe to commit.
type traceMeta struct {
	RequestID string              `json:"request_id"`
	Method    string              `json:"method,omitempty"`
	Path      string              `json:"path,omitempty"`
	Headers   map[string][]string `json:"headers,omitempty"`
	Withheld  []string            `json:"headers_withheld,omitempty"`
	Status    int                 `json:"status,omitempty"`
	Summary   map[string]any      `json:"summary,omitempty"`
	Body      json.RawMessage     `json:"body,omitempty"`
	BodyRaw   string              `json:"body_raw,omitempty"`
}

// Trace is one request's dump. Its methods are safe on a nil receiver and safe
// for concurrent use: the upstream tee and the downstream writer run on
// different goroutines.
type Trace struct {
	tracer *Tracer
	id     string

	mu       sync.Mutex
	meta     traceMeta
	closed   bool
	files    []*os.File
	pendings []*scrubWriter
}

type traceKey struct{}

// WithTrace attaches tr to ctx. A nil tr is a no-op, so the context of an
// untraced request carries nothing.
func WithTrace(ctx context.Context, tr *Trace) context.Context {
	if tr == nil {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, tr)
}

// TraceFrom returns the in-flight request's Trace, or nil.
func TraceFrom(ctx context.Context) *Trace {
	if ctx == nil {
		return nil
	}
	tr, _ := ctx.Value(traceKey{}).(*Trace)
	return tr
}

// SetRequest records the inbound request line and its ALLOWLISTED headers.
// Withheld headers are named but never valued, exactly as in the log: a trace
// is a debugging aid, not an exemption from the redaction rule.
func (t *Trace) SetRequest(method, path string, h http.Header) {
	if t == nil {
		return
	}
	kept := map[string][]string{}
	var withheld []string
	for name, vals := range h {
		lower := strings.ToLower(name)
		if !t.tracer.red.Allowed(lower) {
			withheld = append(withheld, lower)
			continue
		}
		cp := make([]string, len(vals))
		for i, v := range vals {
			cp[i] = Scrub(v)
		}
		kept[lower] = cp
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.meta.Method, t.meta.Path = method, path
	if len(kept) > 0 {
		t.meta.Headers = kept
	}
	if len(withheld) > 0 {
		t.meta.Withheld = withheld
	}
}

// SetBody records the request body with credential-shaped material scrubbed.
// The body is prompt text: this is the part the startup warning is about.
func (t *Trace) SetBody(body []byte) {
	if t == nil || len(body) == 0 {
		return
	}
	clean := ScrubBytes(body)
	t.mu.Lock()
	defer t.mu.Unlock()
	if json.Valid(clean) {
		t.meta.Body = json.RawMessage(clean)
		return
	}
	t.meta.BodyRaw = string(clean)
}

// SetStatus records the status the client was answered with.
func (t *Trace) SetStatus(code int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.meta.Status = code
}

// SetSummary attaches the request line's fields to the manifest, so a trace
// explains itself without a log to correlate it against.
func (t *Trace) SetSummary(sum *Summary) {
	if t == nil || sum == nil {
		return
	}
	f := sum.Fields()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.meta.Summary = f
}

// TeeUpstream wraps the upstream body so every byte read from it is also
// written to <id>.upstream.sse. Closing the returned ReadCloser closes the
// original; the trace file is closed by Close.
func (t *Trace) TeeUpstream(rc io.ReadCloser) io.ReadCloser {
	if t == nil || rc == nil {
		return rc
	}
	sw := t.openScrubbed(SuffixUpstream)
	if sw == nil {
		return rc
	}
	return &teeReadCloser{rc: rc, w: sw}
}

// TeeDownstream wraps the client-facing writer so every translated byte is also
// written to <id>.downstream.sse.
//
// The wrapper re-exposes Flush because sse.FrameWriter type-switches on it to
// decide whether frames stream or pool in a buffer; a tee that swallowed Flush
// would silently turn true-incremental translation back into one lump.
func (t *Trace) TeeDownstream(w io.Writer) io.Writer {
	if t == nil || w == nil {
		return w
	}
	sw := t.openScrubbed(SuffixDownstream)
	if sw == nil {
		return w
	}
	return &teeFlushWriter{w: w, trace: sw}
}

// WriteDownstream records a non-streaming answer body.
func (t *Trace) WriteDownstream(body []byte) {
	if t == nil || len(body) == 0 {
		return
	}
	if f := t.open(SuffixDownstreamJSON); f != nil {
		_, _ = f.Write(ScrubBytes(body))
	}
}

// Close writes the manifest and closes every stream file. It is idempotent.
func (t *Trace) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	meta := t.meta
	files := t.files
	pendings := t.pendings
	t.files, t.pendings = nil, nil
	t.mu.Unlock()

	// Flush any held tail first: a stream that did not end on a newline would
	// otherwise lose its last partial line, which is exactly the frame a
	// truncation bug leaves behind.
	for _, sw := range pendings {
		sw.flushTail()
	}
	for _, f := range files {
		_ = f.Close()
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.tracer.log.Warn("trace: encoding the request manifest failed",
			slog.String("request_id", t.id), slog.String("err", err.Error()))
		return
	}
	data = append(data, '\n')
	path := filepath.Join(t.tracer.dir, t.id+SuffixRequest)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.tracer.log.Warn("trace: writing the request manifest failed",
			slog.String("request_id", t.id), slog.String("err", err.Error()))
	}
}

// openScrubbed opens a trace file behind a scrubbing writer, registering both
// so Close flushes and closes them.
func (t *Trace) openScrubbed(suffix string) *scrubWriter {
	f := t.open(suffix)
	if f == nil {
		return nil
	}
	sw := newScrubWriter(f)
	t.mu.Lock()
	t.pendings = append(t.pendings, sw)
	t.mu.Unlock()
	return sw
}

// open creates one trace file, remembering it so Close can close it. A failure
// is logged once and yields nil: tracing must never break a live request.
func (t *Trace) open(suffix string) *os.File {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	path := filepath.Join(t.tracer.dir, t.id+suffix)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.tracer.log.Warn("trace: opening a trace file failed",
			slog.String("path", path), slog.String("err", err.Error()))
		return nil
	}
	t.files = append(t.files, f)
	return f
}

// scrubWriter applies Scrub to everything written to a trace file. It is
// line-oriented rather than chunk-oriented so a credential split across two
// SSE writes is still matched: bytes are held until a newline arrives.
type scrubWriter struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

// maxScrubHold bounds the held tail, so a stream with no newline at all cannot
// buffer the whole conversation in memory.
const maxScrubHold = 1 << 20

func newScrubWriter(w io.Writer) *scrubWriter { return &scrubWriter{w: w} }

func (s *scrubWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	// Flush every complete line; hold the tail until its newline arrives.
	if i := bytes.LastIndexByte(s.buf, '\n'); i >= 0 {
		if _, err := s.w.Write(ScrubBytes(s.buf[:i+1])); err != nil {
			return 0, err
		}
		s.buf = append(s.buf[:0], s.buf[i+1:]...)
	} else if len(s.buf) > maxScrubHold {
		if _, err := s.w.Write(ScrubBytes(s.buf)); err != nil {
			return 0, err
		}
		s.buf = s.buf[:0]
	}
	return len(p), nil
}

// flushTail writes whatever never ended in a newline.
func (s *scrubWriter) flushTail() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return
	}
	_, _ = s.w.Write(ScrubBytes(s.buf))
	s.buf = s.buf[:0]
}

// teeReadCloser mirrors reads into a trace writer.
type teeReadCloser struct {
	rc io.ReadCloser
	w  io.Writer
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		_, _ = t.w.Write(p[:n])
	}
	return n, err
}

func (t *teeReadCloser) Close() error { return t.rc.Close() }

// teeFlushWriter mirrors writes into a trace writer while preserving the
// Flush() shape the SSE writer looks for.
type teeFlushWriter struct {
	w     io.Writer
	trace io.Writer
}

func (t *teeFlushWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		_, _ = t.trace.Write(p[:n])
	}
	return n, err
}

// Flush forwards to the wrapped writer when it can flush. The trace file is
// not flushed here: it is closed at the end of the request, and an fsync per
// SSE frame would make tracing change the timing it exists to reveal.
func (t *teeFlushWriter) Flush() {
	if f, ok := t.w.(interface{ Flush() }); ok {
		f.Flush()
	}
}
