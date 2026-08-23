package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Timeouts applied to the http.Server. There is deliberately no WriteTimeout:
// a streamed response may legitimately run for many minutes.
const (
	readHeaderTimeout = 20 * time.Second
	connIdleTimeout   = 120 * time.Second
)

// SignalContext returns a context cancelled on SIGINT or SIGTERM.
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// HTTPServer builds the *http.Server this Server should run behind. It is
// exported so cmd can attach a listener inherited from launchd.
func (s *Server) HTTPServer(ctx context.Context) *http.Server {
	return &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       connIdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		// Request contexts must survive shutdown so in-flight streams drain.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
}

// Serve runs on ln until ctx is cancelled, then drains gracefully.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := s.HTTPServer(ctx)
	s.mu.Lock()
	s.hs = hs
	s.mu.Unlock()
	s.log.Info("listening",
		slog.String("addr", ln.Addr().String()),
		slog.String("version", s.version),
		slog.Any("config", s.cfg),
	)
	return Drain(ctx, hs, ln, s.grace)
}

// ListenAndServe binds config.Listen and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Shutdown drains in-flight requests. It is a no-op before Serve.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	hs := s.hs
	s.mu.Unlock()
	if hs == nil {
		return nil
	}
	return hs.Shutdown(ctx)
}

// Drain is the graceful-shutdown helper: serve ln, and when ctx is cancelled
// stop accepting, wait up to grace for in-flight requests, then force-close.
// A clean shutdown returns nil rather than http.ErrServerClosed.
func Drain(ctx context.Context, hs *http.Server, ln net.Listener, grace time.Duration) error {
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	err := hs.Shutdown(sctx)
	<-errc // Serve always returns once Shutdown or Close has run.
	if err != nil {
		_ = hs.Close()
		return err
	}
	return nil
}
