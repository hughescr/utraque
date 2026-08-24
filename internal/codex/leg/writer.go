package leg

import "net/http"

// lazyWriter defers the status line until the first byte of the body exists.
//
// This is what preserves the plan's failure-mode-1 contract across the streaming
// path. The upstream having answered 200 does not yet mean the CLIENT can be
// answered 200: the body may carry no events at all, and the Translator only
// reports that after it has read to EOF. Committing "200 text/event-stream"
// before then would leave the leg holding an error it can no longer render —
// appending an envelope to a committed SSE response corrupts it, and the client
// would see a well-formed but empty stream instead of a real failure.
//
// So: headers are prepared and the status written on the first Write, and
// Started reports afterwards whether that ever happened.
type lazyWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	prepare func(http.Header)
	started bool
}

func newLazyWriter(w http.ResponseWriter, prepare func(http.Header)) *lazyWriter {
	return &lazyWriter{w: w, rc: http.NewResponseController(w), prepare: prepare}
}

// Write commits the response on its first call, then writes through.
func (lw *lazyWriter) Write(p []byte) (int, error) {
	lw.start()
	return lw.w.Write(p)
}

// Flush satisfies the error-less http.Flusher shape sse.FrameWriter looks for,
// so each translated frame is pushed to the client as it is produced rather than
// pooling in net/http's buffer.
//
// Before the first write there is deliberately nothing to flush: flushing would
// itself commit a 200 with no body, which is precisely the state lazyWriter
// exists to avoid.
func (lw *lazyWriter) Flush() {
	if !lw.started {
		return
	}
	_ = lw.rc.Flush()
}

// Started reports whether the status line has gone out — that is, whether the
// caller may still render an error envelope.
func (lw *lazyWriter) Started() bool { return lw.started }

func (lw *lazyWriter) start() {
	if lw.started {
		return
	}
	lw.started = true
	if lw.prepare != nil {
		lw.prepare(lw.w.Header())
	}
	lw.w.WriteHeader(http.StatusOK)
}
