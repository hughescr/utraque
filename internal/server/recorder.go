package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// ResponseInfo is the per-request response summary captured by the observe
// middleware. Its accessors are safe for concurrent use.
type ResponseInfo struct {
	mu     sync.Mutex
	status int
	bytes  int64
	ttfb   time.Duration
	wrote  bool
}

// Status is the response status, or 0 if nothing has been written yet.
func (i *ResponseInfo) Status() int {
	if i == nil {
		return 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status
}

// Bytes is how many body bytes have been written.
func (i *ResponseInfo) Bytes() int64 {
	if i == nil {
		return 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.bytes
}

// TTFB is the delay from request start to the first header or body byte.
func (i *ResponseInfo) TTFB() time.Duration {
	if i == nil {
		return 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ttfb
}

// Wrote reports whether the response has begun.
func (i *ResponseInfo) Wrote() bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.wrote
}

func (i *ResponseInfo) mark(status int, at time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.wrote {
		return
	}
	i.wrote = true
	i.status = status
	i.ttfb = at
}

func (i *ResponseInfo) add(n int64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.bytes += n
}

type infoKey struct{}

func withInfo(ctx context.Context, i *ResponseInfo) context.Context {
	return context.WithValue(ctx, infoKey{}, i)
}

// InfoFrom returns the response summary for the in-flight request, or nil.
func InfoFrom(ctx context.Context) *ResponseInfo {
	if ctx == nil {
		return nil
	}
	i, _ := ctx.Value(infoKey{}).(*ResponseInfo)
	return i
}

// recorder wraps a ResponseWriter to capture status, byte count and TTFB. It
// exposes Unwrap so http.ResponseController reaches the real writer, which is
// what keeps SSE flushing and write deadlines working through the chain. It
// deliberately does NOT implement io.ReaderFrom, so io.Copy in the passthrough
// leg routes through Write and the logged byte count stays honest.
type recorder struct {
	http.ResponseWriter
	now   func() time.Time
	start time.Time
	info  ResponseInfo
}

func newRecorder(w http.ResponseWriter, now func() time.Time, start time.Time) *recorder {
	return &recorder{ResponseWriter: w, now: now, start: start}
}

func (rc *recorder) WriteHeader(code int) {
	// 1xx are informational; net/http permits several before the real status.
	if code >= 100 && code < 200 {
		rc.ResponseWriter.WriteHeader(code)
		return
	}
	rc.info.mark(code, rc.now().Sub(rc.start))
	rc.ResponseWriter.WriteHeader(code)
}

func (rc *recorder) Write(b []byte) (int, error) {
	rc.info.mark(http.StatusOK, rc.now().Sub(rc.start))
	n, err := rc.ResponseWriter.Write(b)
	if n > 0 {
		rc.info.add(int64(n))
	}
	return n, err
}

func (rc *recorder) Flush() {
	_ = http.NewResponseController(rc.ResponseWriter).Flush()
}

func (rc *recorder) Unwrap() http.ResponseWriter { return rc.ResponseWriter }

func (rc *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rc.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("server: %T does not support hijacking", rc.ResponseWriter)
	}
	return h.Hijack()
}
