package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/obs"
)

// maxRequestIDLen bounds a caller-supplied request id.
const maxRequestIDLen = 128

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(ResponseIDHeader, id)
		ctx := obs.WithRequestID(r.Context(), id)
		ctx = obs.WithLogger(ctx, s.log.With(slog.String("request_id", id)))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sanitizeRequestID accepts a short, printable, space-free ASCII id.
func sanitizeRequestID(id string) string {
	if id == "" || len(id) > maxRequestIDLen {
		return ""
	}
	for i := range len(id) {
		if id[i] < 0x21 || id[i] > 0x7e {
			return ""
		}
	}
	return id
}

func newRequestID() string { return strings.ToLower(rand.Text()) }

// withObserve wraps the ResponseWriter to capture status, bytes and TTFB, and
// emits EXACTLY ONE structured access-log line per request.
//
// One line, not several, is the point. This middleware can see the request
// line, the sizes and the timings; only the layers below know the route, the
// models, the effort, the upstream status and how the answer ended. They
// contribute through the obs.Summary carried in the context, so a reader gets a
// single record per request rather than a scattering of half-lines to correlate
// by hand. Every Summary method is nil-safe, so a leg driven without a server
// around it still works and simply contributes nothing.
//
// This is also where a trace dump is opened when tracing is on: the request id
// already exists (withRequestID is outermost) and the deferred Close runs after
// every layer below has finished writing.
func (s *Server) withObserve(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := newRecorder(w, s.now, start)

		sum := obs.NewSummary()
		if s.transportKind != nil {
			sum.SetTransport(s.transportKind())
		}
		// A declared Content-Length is the honest byte count even for a body
		// the handler streams rather than buffers. A leg that reads the body
		// itself overrides this with what it actually read.
		sum.SetReqBytes(r.ContentLength)

		ctx := withInfo(r.Context(), &rec.info)
		ctx = obs.WithSummary(ctx, sum)

		trace := s.tracer.Begin(obs.RequestIDFrom(ctx))
		if trace != nil {
			defer trace.Close()
			trace.SetRequest(r.Method, obs.SafePath(r.URL), r.Header)
			ctx = obs.WithTrace(ctx, trace)
		}

		r = r.WithContext(ctx)

		defer func() {
			total := s.now().Sub(start)
			// A context already cancelled when the handler returns means the
			// client hung up. Legs mark deliberate interrupts too; the flag
			// only ever latches on.
			if r.Context().Err() != nil {
				sum.SetInterrupted(true)
			}
			status := rec.info.Status()

			attrs := make([]slog.Attr, 0, 24)
			attrs = append(attrs,
				slog.String("method", r.Method),
				slog.String("path", obs.SafePath(r.URL)),
				slog.Int("status", status),
				slog.Int64("resp_bytes", rec.info.Bytes()),
				slog.Float64("ttfb_ms", obs.Millis(rec.info.TTFB())),
				slog.Float64("total_ms", obs.Millis(total)),
			)
			attrs = append(attrs, sum.Attrs()...)
			attrs = append(attrs, slog.Attr{Key: "headers", Value: s.red.Header(r.Header)})
			obs.LoggerFrom(ctx).LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "request", attrs...)

			trace.SetStatus(status)
			trace.SetSummary(sum)
		}()

		next.ServeHTTP(rec, r)
	})
}

// withRecover turns a panic into a 500 error envelope, unless the response has
// already begun, in which case it logs and lets the connection break rather
// than appending garbage to a half-sent body.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rv := recover()
			if rv == nil {
				return
			}
			if err, ok := rv.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rv) // net/http's own signal; do not swallow it
			}
			ctx := r.Context()
			obs.LoggerFrom(ctx).LogAttrs(context.WithoutCancel(ctx), slog.LevelError,
				"panic recovered",
				slog.Any("panic", rv),
				slog.String("stack", string(debug.Stack())),
			)
			if info := InfoFrom(ctx); info != nil && info.Wrote() {
				return
			}
			_ = apierr.Write(w, apierr.API("internal error"))
		}()
		next.ServeHTTP(w, r)
	})
}

// withActivity holds the idle timer open for the life of a non-exempt request.
func (s *Server) withActivity(next http.Handler) http.Handler {
	if s.activity == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.activityExempt != nil && s.activityExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		release := s.activity.Hold()
		defer release()
		next.ServeHTTP(w, r)
	})
}

// withBodyLimit caps request bodies at limits.max_body_bytes. A declared
// Content-Length over the cap is rejected up front; an undeclared or lying one
// is caught by MaxBytesReader when the handler reads (see ReadBody).
func (s *Server) withBodyLimit(next http.Handler) http.Handler {
	maxBytes := s.cfg.Limits.MaxBodyBytes
	if maxBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBytes {
			_ = apierr.Write(w, apierr.RequestTooLarge(
				"request body of %d bytes exceeds the %d byte limit", r.ContentLength, maxBytes))
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// withAuth enforces the optional loopback shared secret. Any local process
// could otherwise spend both subscriptions through the loopback port.
func (s *Server) withAuth(next http.Handler) http.Handler {
	want := []byte(s.cfg.LocalToken)
	if len(want) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authExempt != nil && s.authExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get(LocalTokenHeader))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			ctx := r.Context()
			obs.LoggerFrom(ctx).LogAttrs(context.WithoutCancel(ctx), slog.LevelWarn,
				"local token rejected",
				slog.String("path", obs.SafePath(r.URL)),
				slog.Bool("presented", len(got) > 0),
			)
			_ = apierr.Write(w, apierr.Authentication(
				"missing or invalid %s header", LocalTokenHeader))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ReadBody reads a request body already capped by withBodyLimit, converting an
// over-limit read into a request_too_large error. Route handlers should use it
// instead of io.ReadAll so the client always sees an Anthropic error envelope.
func ReadBody(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, apierr.RequestTooLarge(
				"request body exceeds the %d byte limit", mbe.Limit)
		}
		return nil, apierr.Wrap(err, apierr.TypeInvalidRequest, "reading request body")
	}
	return b, nil
}
