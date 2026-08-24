// Command utraque is a local HTTP proxy that lets one Claude Code session
// reach two subscriptions: Anthropic models pass through to api.anthropic.com
// on the caller's own OAuth credential, and GPT models route to the Codex
// backend on the credential the Codex CLI already holds. Both legs are live;
// this file only assembles them, so the wiring stays readable and every
// behaviour is testable in the package that owns it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hughescr/utraque/internal/anthropic"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/catalog"
	"github.com/hughescr/utraque/internal/codex/leg"
	"github.com/hughescr/utraque/internal/codex/responses"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/discovery"
	"github.com/hughescr/utraque/internal/idle"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/server"
	"github.com/hughescr/utraque/internal/tokens"
	"github.com/hughescr/utraque/internal/transport"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// betaHeader carries the OAuth capability flags. It may legitimately appear
// several times; the passthrough relays each value as its own header line and
// only the router's (currently unused) signal parsing wants them joined.
const betaHeader = "anthropic-beta"

func main() {
	if err := run(context.Background(), os.Getenv, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "utraque: %v\n", err)
		os.Exit(1)
	}
}

// run is main's testable body: load config, build the logger, assemble the
// server, arm the idle timer, and serve until a signal or the idle timeout
// asks us to stop.
func run(ctx context.Context, getenv func(string) string, stderr io.Writer) error {
	cfg, err := config.LoadFrom(getenv)
	if err != nil {
		return err
	}

	log, err := obs.NewLogger(stderr, cfg.SlogLevel(), cfg.Log.Format)
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	// Two cancellation sources feed one context: SIGINT/SIGTERM from the
	// operator or launchd, and the idle timer's self-exit. Either one starts
	// the same graceful drain.
	ctx, stopSignals := server.SignalContext(ctx)
	defer stopSignals()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	timer := idle.New(cfg.Idle.Timeout, func() {
		log.Info("idle timeout reached; exiting so launchd can re-activate on the next request",
			slog.Duration("idle_timeout", cfg.Idle.Timeout))
		cancel()
	})

	srv, err := newServer(cfg, log, timer)
	if err != nil {
		return err
	}

	timer.Start()
	defer timer.Stop()

	return srv.ListenAndServe(ctx)
}

// newServer assembles the whole HTTP surface. It is separate from run so tests
// can drive the exact handler wiring production uses, against a fake upstream.
//
// activity may be nil, which disables idle accounting.
func newServer(cfg config.Config, log *slog.Logger, activity server.ActivityTracker) (*server.Server, error) {
	tr := transport.NewStd(transport.Options{
		// Bound only the pre-first-byte wait. There is deliberately no overall
		// client timeout: an SSE stream may legitimately run for many minutes.
		ResponseHeaderTimeout: cfg.Limits.UpstreamIdleTimeout,
		DisableCompression:    true,
	})

	passthrough, err := anthropic.New(cfg.Anthropic.BaseURL, tr,
		anthropic.WithLogger(log),
		anthropic.WithMaxBodyBytes(cfg.Limits.MaxBodyBytes),
		// ResponseHeaderTimeout above only bounds the wait for the first byte.
		// This bounds silence *within* a stream too, so a stalled SSE response
		// cannot pin a request, a connection and an idle-timer hold forever.
		anthropic.WithUpstreamIdleTimeout(cfg.Limits.UpstreamIdleTimeout),
	)
	if err != nil {
		return nil, err
	}

	// Codex auth + catalog. The auth source is only built when a credential
	// file path is configured. LoadFrom always resolves one; a bare
	// config.Default() (used by tests) leaves it empty, in which case the codex
	// leg has no credential and /healthz reports auth "missing" — rather than
	// New failing at construction on auth.ErrNoPath.
	var credSource *auth.Source
	if cfg.Codex.AuthFile != "" {
		credSource, err = auth.New(auth.Options{
			Path:        cfg.Codex.AuthFile,
			TokenURL:    cfg.Codex.TokenURL,
			ClientID:    cfg.Codex.ClientID,
			RefreshSkew: cfg.Codex.RefreshSkew,
			LockTimeout: cfg.Codex.LockTimeout,
			Logger:      log,
		})
		if err != nil {
			return nil, err
		}
	}

	// The catalog client is always built; an empty CachePath just makes it
	// memory-only. It performs no network or disk I/O until first used, and
	// /healthz only ever reads its held snapshot (never triggering a fetch).
	cat := catalog.New(catalog.Options{
		BaseURL:       cfg.Codex.BaseURL,
		CachePath:     cfg.Codex.CachePath,
		ClientVersion: version,
		Logger:        log,
	})

	// The Codex inference leg. It is built even without a credential source:
	// "no `codex login` here" is a state it reports per request, with an
	// actionable message, rather than a construction failure that would take
	// the working Anthropic leg down with it.
	//
	// Only assign the interface field from a non-nil concrete source: a nil
	// *auth.Source stored in an interface is a non-nil interface value, which
	// would defeat the leg's own nil check.
	legOpts := leg.Options{
		Client: responses.New(responses.Options{
			BaseURL:   cfg.Codex.BaseURL,
			Transport: tr,
			Logger:    log,
		}),
		Catalog:      cat,
		Estimator:    tokens.Default(),
		UpstreamIdle: cfg.Limits.UpstreamIdleTimeout,
		Logger:       log,
	}
	if credSource != nil {
		legOpts.Credentials = credSource
	}
	codexLeg, err := leg.New(legOpts)
	if err != nil {
		return nil, err
	}

	models, err := newDiscovery(cfg, tr, cat, credSource, log)
	if err != nil {
		return nil, err
	}

	d := &dispatcher{anthropic: passthrough, codex: codexLeg}

	hr := &healthReporter{cat: cat}
	if credSource != nil {
		hr.auth = credSource
	}

	return server.New(server.Options{
		Config:      cfg,
		Logger:      log,
		Version:     version,
		Activity:    activity,
		HealthExtra: hr.extra,
		Routes: server.Routes{
			Messages:    http.HandlerFunc(d.messages),
			CountTokens: http.HandlerFunc(d.countTokens),
			Models:      models,
			// Everything else — /v1/organizations/..., whatever Claude Code
			// reaches for next — relays upstream unchanged. None of it may 404
			// locally.
			Passthrough: passthrough,
		},
	})
}

// newDiscovery builds the merged GET /v1/models handler that populates Claude
// Code's model picker.
//
// The Codex half is deliberately read through a Peek gate. discovery's contract
// is that opening a picker must never be a reason to rotate a credential, and
// auth.Source.Get can trigger a refresh and an auth.json write-back shared with
// the Codex CLI. A live token serves real rows; anything else serves none,
// which the handler already degrades to gracefully.
func newDiscovery(cfg config.Config, tr transport.Transport, cat *catalog.Client, credSource *auth.Source, log *slog.Logger) (http.Handler, error) {
	anthCat, err := anthropic.NewCatalog(cfg.Anthropic.BaseURL, tr, anthropic.WithCatalogLogger(log))
	if err != nil {
		return nil, err
	}

	codexCat := discovery.CodexCatalogFunc(func(ctx context.Context) ([]cschema.Model, error) {
		if credSource == nil {
			return nil, errors.New("no codex credential is configured")
		}
		if st := credSource.Peek(); st.State != auth.StateOK {
			return nil, fmt.Errorf("codex credential is %q; not refreshing it just to open the model picker", st.State)
		}
		cred, err := credSource.Get(ctx)
		if err != nil {
			return nil, err
		}
		return cat.Models(ctx, cred)
	})

	return discovery.New(discovery.Options{
		Anthropic: anthCat,
		Codex:     codexCat,
		Logger:    log,
	})
}

// healthReporter contributes the codex auth and catalog fields to /healthz. It
// reads only cached/local state — a token-expiry decode from a file stat and
// the catalog's held snapshot — so a health poll never contacts the network and
// never reveals a token value.
type healthReporter struct {
	auth *auth.Source    // nil when no credential file is configured
	cat  *catalog.Client // always set
}

func (h *healthReporter) extra(context.Context) map[string]any {
	codex := map[string]any{"status": auth.StateMissing}
	if h.auth != nil {
		st := h.auth.Peek()
		codex["status"] = st.State
		if st.HasExpiry {
			// Whole seconds; may be negative for an already-expired token. The
			// token value itself is never included.
			codex["expires_in_s"] = int64(st.ExpiresIn / time.Second)
		}
	}

	catInfo := map[string]any{"models": 0}
	if h.cat != nil {
		n, age, loaded := h.cat.Snapshot()
		catInfo["models"] = n
		if loaded {
			catInfo["age_s"] = roundSeconds(age.Seconds())
		}
	}

	return map[string]any{
		"codex_auth":    codex,
		"codex_catalog": catInfo,
	}
}

// roundSeconds keeps a duration-in-seconds to millisecond precision so the
// health JSON stays short.
func roundSeconds(s float64) float64 { return math.Round(s*1000) / 1000 }

// dispatcher reads the request body once, peeks the two fields that decide
// routing, and hands the whole thing to the leg the router picked. Both legs
// own their own responses; the dispatcher only chooses between them and renders
// what neither of them could.
type dispatcher struct {
	anthropic router.Leg
	codex     router.Leg
}

// peeked is the minimal shape the dispatcher needs out of a Messages or
// CountTokens body. Every other field is left in Raw and forwarded untouched.
type peeked struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// legCall selects which router.Leg method a dispatched request invokes, so the
// two routes share one dispatch body.
type legCall func(router.Leg, http.ResponseWriter, *http.Request, *router.Request) error

func callMessages(l router.Leg, w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	return l.Messages(w, r, rq)
}

func callCountTokens(l router.Leg, w http.ResponseWriter, r *http.Request, rq *router.Request) error {
	return l.CountTokens(w, r, rq)
}

func (d *dispatcher) messages(w http.ResponseWriter, r *http.Request) {
	d.dispatch(w, r, callMessages)
}

func (d *dispatcher) countTokens(w http.ResponseWriter, r *http.Request) {
	d.dispatch(w, r, callCountTokens)
}

func (d *dispatcher) dispatch(w http.ResponseWriter, r *http.Request, call legCall) {
	ctx := r.Context()
	log := obs.LoggerFrom(ctx)

	raw, err := server.ReadBody(r)
	if err != nil {
		_ = apierr.Write(w, err)
		return
	}

	var p peeked
	if err := json.Unmarshal(raw, &p); err != nil {
		_ = apierr.Write(w, apierr.Wrap(err, apierr.TypeInvalidRequest,
			"request body must be a JSON object carrying a \"model\" field"))
		return
	}

	dec, err := router.Resolve(p.Model, strings.Join(r.Header.Values(betaHeader), ","))
	if err != nil {
		log.LogAttrs(ctx, slog.LevelInfo, "unroutable model", slog.String("model", p.Model))
		_ = apierr.Write(w, err)
		return
	}

	log.LogAttrs(ctx, slog.LevelDebug, "routed",
		slog.String("backend", dec.Backend.String()),
		slog.String("client_model", dec.ClientModel),
		slog.String("upstream_model", dec.UpstreamModel),
		slog.String("effort", dec.Effort),
		slog.String("effort_source", dec.EffortSource),
		slog.Bool("stream", p.Stream),
	)

	rq := &router.Request{Raw: raw, Model: p.Model, Stream: p.Stream, Dec: dec, Log: log}

	switch dec.Backend {
	case router.BackendAnthropic:
		if err := call(d.anthropic, w, r, rq); err != nil {
			d.fail(w, r, err)
		}
	case router.BackendCodex:
		if d.codex == nil {
			// Defensive: newServer always builds the leg. A nil interface here
			// would panic, and a panic is a much worse answer than a 503.
			_ = apierr.Write(w, apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeAPI,
				"codex leg is not configured"))
			return
		}
		if err := call(d.codex, w, r, rq); err != nil {
			d.fail(w, r, err)
		}
	default:
		_ = apierr.Write(w, apierr.API("router returned an unknown backend %q", dec.Backend))
	}
}

// fail renders a leg's error, unless the leg already committed a status line —
// appending an error envelope to a half-sent body would corrupt it.
func (d *dispatcher) fail(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	log := obs.LoggerFrom(ctx)
	if errors.Is(err, router.ErrResponseStarted) || errors.Is(err, router.ErrClientGone) {
		log.LogAttrs(context.WithoutCancel(ctx), slog.LevelWarn,
			"leg failed after the response started", slog.String("err", err.Error()))
		return
	}
	log.LogAttrs(ctx, slog.LevelWarn, "leg failed", slog.String("err", err.Error()))
	_ = apierr.Write(w, err)
}
