// Command utraque is a local HTTP proxy that lets one Claude Code session
// reach two subscriptions: Anthropic models pass through to api.anthropic.com
// on the caller's own OAuth credential, and GPT models route to the Codex
// backend. This build implements the Anthropic leg; the Codex leg answers with
// a clear 503 stub until phase 5 lands.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/hughescr/utraque/internal/anthropic"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/idle"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/server"
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

	d := &dispatcher{anthropic: passthrough}

	return server.New(server.Options{
		Config:   cfg,
		Logger:   log,
		Version:  version,
		Activity: activity,
		Routes: server.Routes{
			Messages:    http.HandlerFunc(d.messages),
			CountTokens: http.HandlerFunc(d.countTokens),
			// Everything else — /v1/models, /v1/organizations/..., whatever
			// Claude Code reaches for next — relays upstream unchanged. None
			// of it may 404 locally.
			Passthrough: passthrough,
		},
	})
}

// dispatcher reads the request body once, peeks the two fields that decide
// routing, and hands the whole thing to the leg the router picked.
type dispatcher struct {
	anthropic router.Leg

	// codex is nil until the Codex leg lands; nil means "answer the 503 stub".
	codex router.Leg
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
			_ = apierr.Write(w, codexNotImplemented(dec))
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
	if errors.Is(err, anthropic.ErrResponseStarted) || errors.Is(err, anthropic.ErrClientGone) {
		log.LogAttrs(context.WithoutCancel(ctx), slog.LevelWarn,
			"leg failed after the response started", slog.String("err", err.Error()))
		return
	}
	log.LogAttrs(ctx, slog.LevelWarn, "leg failed", slog.String("err", err.Error()))
	_ = apierr.Write(w, err)
}

// codexNotImplemented is the phase-1 Codex stub. It is a 503 rather than a 501
// so the client reads it as "this backend cannot serve you right now" — the
// same shape it will see later when the Codex leg exists but is unhealthy —
// and never as a malformed request it should stop retrying.
func codexNotImplemented(dec router.Decision) *apierr.Error {
	return apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeAPI,
		"codex leg not yet implemented: model %q routes to upstream model %q, which utraque cannot serve yet",
		dec.ClientModel, dec.UpstreamModel)
}
