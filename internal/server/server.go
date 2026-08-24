// Package server wires utraque's HTTP surface: a http.ServeMux carrying the
// Anthropic-compatible routes plus /healthz, the middleware chain (request id,
// response observation, panic recovery, idle-activity accounting, body limit,
// optional local shared-secret auth), and graceful shutdown.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/obs"
)

// Header names and well-known paths.
const (
	// LocalTokenHeader carries the optional loopback shared secret. It is a
	// dedicated header so the client's Authorization header, which holds the
	// user's Anthropic OAuth credential, passes through untouched.
	LocalTokenHeader = "X-Utraque-Token"

	// RequestIDHeader is honoured on inbound requests when it looks sane.
	RequestIDHeader = obs.RequestIDHeader

	// ResponseIDHeader is always set on our responses. It is distinct from
	// RequestIDHeader so a passthrough response can still carry Anthropic's
	// own X-Request-Id unmodified.
	ResponseIDHeader = "X-Utraque-Request-Id"

	// HealthPath is the local health endpoint.
	HealthPath = "/healthz"
)

// DefaultShutdownGrace is how long Serve drains in-flight requests.
const DefaultShutdownGrace = 25 * time.Second

// Routes are the backend handlers main mounts. A nil handler is not
// registered; a nil Passthrough means unmatched paths get a 404 envelope.
type Routes struct {
	Messages    http.Handler // POST /v1/messages
	CountTokens http.Handler // POST /v1/messages/count_tokens
	Models      http.Handler // GET  /v1/models
	Passthrough http.Handler // catch-all; in production this must not 404
}

// ActivityTracker is the slice of *idle.Timer the server depends on: Hold
// marks a request in flight and returns its release func.
type ActivityTracker interface {
	Hold() (release func())
}

// Options configures New. Only Config is required.
type Options struct {
	Config  config.Config
	Logger  *slog.Logger // default slog.Default()
	Routes  Routes
	Version string // reported by /healthz; default "0.0.0-dev"

	// Now is the clock, injectable for tests. Default time.Now.
	Now func() time.Time

	// Redactor governs which request headers reach the log.
	// Default obs.DefaultRedactor().
	Redactor *obs.Redactor

	// TransportKind reports the DEFAULT HTTP transport a request goes out on
	// ("std" or "utls"). It appears on every request line, because "which TLS
	// stack answered" is the first question once the Cloudflare fallback is in
	// play and a useful constant when it is not.
	//
	// It is only a default. The legs hold separate transports and a leg that
	// knows better overrides this by calling Summary.SetTransport at dispatch;
	// the Codex responses client does exactly that, which is what makes an
	// auto-transport flip visible on the request line. Wiring the flippable
	// transport in here instead would mislabel every Anthropic request.
	//
	// It is a func rather than a string because a transport can switch stacks
	// mid-process: a kind captured at construction would keep claiming "std"
	// long after every request had moved to uTLS.
	TransportKind func() string

	// Tracer, when enabled, dumps a per-request trace. Nil — the default, and
	// the default in production — writes nothing.
	Tracer *obs.Tracer

	// Activity, when set, is held for the duration of every non-exempt
	// request so the idle timer never fires mid-stream.
	Activity ActivityTracker

	// ActivityExempt reports requests that must not count as activity.
	// Default: /healthz, so polling health never keeps the daemon alive.
	ActivityExempt func(*http.Request) bool

	// AuthExempt reports requests that skip the local-token check.
	// Default: /healthz.
	AuthExempt func(*http.Request) bool

	// HealthExtra contributes additional /healthz fields. It may not
	// override status, version or uptime_s.
	HealthExtra func(context.Context) map[string]any

	ShutdownGrace time.Duration // default DefaultShutdownGrace
}

// Server is the assembled HTTP surface.
type Server struct {
	cfg            config.Config
	log            *slog.Logger
	version        string
	now            func() time.Time
	red            *obs.Redactor
	transportKind  func() string
	tracer         *obs.Tracer
	activity       ActivityTracker
	activityExempt func(*http.Request) bool
	authExempt     func(*http.Request) bool
	healthExtra    func(context.Context) map[string]any
	grace          time.Duration
	started        time.Time
	handler        http.Handler

	mu sync.Mutex
	hs *http.Server
}

var _ http.Handler = (*Server)(nil)

// IsHealth reports whether r targets the health endpoint. It is the default
// ActivityExempt and AuthExempt predicate.
func IsHealth(r *http.Request) bool { return r != nil && r.URL != nil && r.URL.Path == HealthPath }

// New validates the config and assembles the mux and middleware chain.
func New(opts Options) (*Server, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	s := &Server{
		cfg:            opts.Config,
		log:            opts.Logger,
		version:        opts.Version,
		now:            opts.Now,
		red:            opts.Redactor,
		transportKind:  opts.TransportKind,
		tracer:         opts.Tracer,
		activity:       opts.Activity,
		activityExempt: opts.ActivityExempt,
		authExempt:     opts.AuthExempt,
		healthExtra:    opts.HealthExtra,
		grace:          opts.ShutdownGrace,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.version == "" {
		s.version = "0.0.0-dev"
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.red == nil {
		s.red = obs.DefaultRedactor()
	}
	if s.activityExempt == nil {
		s.activityExempt = IsHealth
	}
	if s.authExempt == nil {
		s.authExempt = IsHealth
	}
	if s.grace <= 0 {
		s.grace = DefaultShutdownGrace
	}
	s.started = s.now()

	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, s.handleHealth)
	if h := opts.Routes.Messages; h != nil {
		mux.Handle("POST /v1/messages", h)
	}
	if h := opts.Routes.CountTokens; h != nil {
		mux.Handle("POST /v1/messages/count_tokens", h)
	}
	if h := opts.Routes.Models; h != nil {
		mux.Handle("GET /v1/models", h)
	}
	var root http.Handler = mux
	if h := opts.Routes.Passthrough; h != nil {
		mux.Handle("/", h)
		root = relayNonCanonical(mux, h)
	} else {
		mux.HandleFunc("/", handleNotFound)
	}

	s.handler = s.chain(root)
	return s, nil
}

// relayNonCanonical hands a request whose path http.ServeMux would answer with
// a local redirect to the catch-all instead.
//
// ServeMux canonicalizes: "/v1//messages" or a path with dot segments earns a
// 3xx of the mux's own making, which never reaches upstream and which, on a
// POST, makes the client re-send its body. Every unrecognised path must relay
// upstream unchanged, so utraque decides these itself rather than letting the
// mux answer them.
func relayNonCanonical(mux *http.ServeMux, passthrough http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CONNECT is exempt in ServeMux too: its target is never cleaned.
		if r.URL != nil && r.Method != http.MethodConnect {
			if p := r.URL.EscapedPath(); p != cleanPath(p) {
				passthrough.ServeHTTP(w, r)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// cleanPath mirrors net/http's own path canonicalization, which is what
// ServeMux compares against before deciding to redirect.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// chain wraps h outermost-first: request id, observation, panic recovery,
// idle activity, body limit, local auth.
func (s *Server) chain(h http.Handler) http.Handler {
	h = s.withAuth(h)
	h = s.withBodyLimit(h)
	h = s.withActivity(h)
	h = s.withRecover(h)
	h = s.withObserve(h)
	h = s.withRequestID(h)
	return h
}

// Handler returns the fully wrapped handler.
func (s *Server) Handler() http.Handler { return s.handler }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Config returns the validated configuration.
func (s *Server) Config() config.Config { return s.cfg }

// Logger returns the base logger.
func (s *Server) Logger() *slog.Logger { return s.log }

// Version returns the reported build version.
func (s *Server) Version() string { return s.version }

// Uptime is how long the server has existed.
func (s *Server) Uptime() time.Duration { return s.now().Sub(s.started) }

// Deadline derives a context bounded by the configured upstream idle timeout.
// Leg handlers use it so a hung upstream cannot pin a request forever.
func (s *Server) Deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if d := s.cfg.Limits.UpstreamIdleTimeout; d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return context.WithCancel(ctx)
}

func handleNotFound(w http.ResponseWriter, r *http.Request) {
	_ = apierr.Write(w, apierr.NotFound("no route for %s %s", r.Method, obs.SafePath(r.URL)))
}
