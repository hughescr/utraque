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
	return s.ServeAll(ctx, ln)
}

// ServeAll runs on every listener until ctx is cancelled, then drains them all
// through one shutdown.
//
// It takes a slice because launchd hands over one descriptor per address family
// it bound for a Sockets entry: an entry with no SockNodeName produces both an
// IPv4 and an IPv6 socket, and accepting on only one of them would leave
// connections to the other hanging with nothing to answer them.
func (s *Server) ServeAll(ctx context.Context, lns ...net.Listener) error {
	if len(lns) == 0 {
		return errors.New("server: ServeAll needs at least one listener")
	}
	hs := s.HTTPServer(ctx)
	s.mu.Lock()
	s.hs = hs
	s.mu.Unlock()

	addrs := make([]string, len(lns))
	for i, ln := range lns {
		addrs[i] = ln.Addr().String()
	}
	s.log.Info("listening",
		slog.String("addr", addrs[0]),
		slog.Any("addrs", addrs),
		slog.String("version", s.version),
		slog.Any("config", s.cfg),
	)
	return DrainAll(ctx, hs, lns, s.grace)
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

// Drain is the graceful-shutdown helper for a single listener.
func Drain(ctx context.Context, hs *http.Server, ln net.Listener, grace time.Duration) error {
	return DrainAll(ctx, hs, []net.Listener{ln}, grace)
}

// DrainAll serves every listener, and when ctx is cancelled stops accepting,
// waits up to grace for in-flight requests, then force-closes. A clean shutdown
// returns nil rather than http.ErrServerClosed.
//
// The idle timeout arrives here as a cancelled ctx, which is precisely why the
// wait exists: a streaming answer that is mid-flight when the daemon decides to
// exit finishes writing before the process goes away. (internal/idle will not
// fire at all while a request is in flight, so this is the second line of
// defence, covering a SIGTERM from launchd or an operator.)
func DrainAll(ctx context.Context, hs *http.Server, lns []net.Listener, grace time.Duration) error {
	if len(lns) == 0 {
		return errors.New("server: DrainAll needs at least one listener")
	}
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func() { errc <- hs.Serve(ln) }()
	}

	outstanding := len(lns)
	var serveErr error
	select {
	case err := <-errc:
		outstanding--
		// One listener stopped on its own. Even a clean stop is fatal to the
		// daemon: the address it held is no longer served, and under socket
		// activation launchd would keep handing connections to a socket nobody
		// accepts on. Shut the rest down and report.
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
	}

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	err := hs.Shutdown(sctx)
	// Shutdown (or Close) closes every listener registered with hs, so each
	// remaining Serve returns.
	for range outstanding {
		<-errc
	}
	if err != nil {
		_ = hs.Close()
		return err
	}
	return serveErr
}
