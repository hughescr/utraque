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
	"sort"
	"strconv"
	"strings"
	"sync"
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
	"github.com/hughescr/utraque/internal/launchd"
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

	// Acquire the listening socket first: either the one launchd already holds
	// or a fresh bind of cfg.Listen. Doing it before anything else means a port
	// clash is reported before we have touched a credential file.
	lns, src, err := launchd.Listen(launchd.Options{
		SocketName: cfg.Launchd.SocketName,
		Addr:       cfg.Listen,
		Logger:     log,
	})
	if err != nil {
		return err
	}

	cfg.Idle = idlePolicy(cfg.Idle, src)

	timer := idle.New(cfg.Idle.Timeout, func() {
		log.Info("idle timeout reached; exiting so launchd can re-activate on the next request",
			slog.Duration("idle_timeout", cfg.Idle.Timeout))
		cancel()
	})

	// Trace dumps are off unless UTRAQUE_TRACE_DIR names a directory, and
	// turning them on prints a loud warning: a trace holds the conversation, not
	// just its shape.
	tracer, err := obs.TracerFromEnv(getenv, log)
	if err != nil {
		_ = launchd.CloseAll(lns)
		return err
	}

	a, err := newApp(cfg, log, timer, tracer)
	if err != nil {
		_ = launchd.CloseAll(lns)
		return err
	}

	// Fill the Codex catalog in the background so /healthz reports a real model
	// count on a daemon nobody has sent a request to yet. Nothing waits on it.
	a.warmCatalog(ctx)

	timer.Start()
	defer timer.Stop()

	return a.srv.ServeAll(ctx, lns...)
}

// idlePolicy decides the effective idle timeout from the configured one and how
// we got our socket.
//
// Self-exit is only safe when something can bring the daemon back, so it stays
// off by default on a manual start and turns on by default under launchd socket
// activation. An explicit UTRAQUE_IDLE_TIMEOUT always wins, in both directions:
// an operator can ask a hand-started proxy to exit when idle, and can ask a
// launchd-activated one never to.
func idlePolicy(in config.Idle, src launchd.Source) config.Idle {
	if in.Explicit || src != launchd.SourceLaunchd {
		return in
	}
	in.Timeout = config.DefaultLaunchdIdleTimeout
	return in
}

// app is the assembled process: the HTTP surface, plus the background work that
// belongs to a running daemon rather than to the handler wiring.
//
// The split exists so a test can build the exact production handler graph
// without also starting a goroutine that reaches for an upstream. warmCatalog
// is called by run and by nothing else, which is what keeps the test suite
// incapable of making a network request it did not ask for.
type app struct {
	srv  *server.Server
	warm func(context.Context)
}

// warmCatalog kicks off the startup catalog fetch. It returns immediately.
func (a *app) warmCatalog(ctx context.Context) {
	if a == nil || a.warm == nil {
		return
	}
	a.warm(ctx)
}

// newServer assembles the whole HTTP surface. It is the handler-only entry
// point tests use; run builds the full app.
//
// activity may be nil, which disables idle accounting.
func newServer(cfg config.Config, log *slog.Logger, activity server.ActivityTracker) (*server.Server, error) {
	a, err := newApp(cfg, log, activity, nil)
	if err != nil {
		return nil, err
	}
	return a.srv, nil
}

// newApp assembles the HTTP surface and the background work behind it. It is
// separate from run so tests can drive the exact wiring production uses against
// a fake upstream.
//
// tracer may be nil, which disables per-request trace dumps.
func newApp(cfg config.Config, log *slog.Logger, activity server.ActivityTracker, tracer *obs.Tracer) (*app, error) {
	trOpts := transport.Options{
		// Bound only the pre-first-byte wait. There is deliberately no overall
		// client timeout: an SSE stream may legitimately run for many minutes.
		ResponseHeaderTimeout: cfg.Limits.UpstreamIdleTimeout,
		DisableCompression:    true,
	}

	// The Anthropic leg always uses the standard library. It is the sanctioned
	// half — a transparent pass-through of the client's own credential to
	// api.anthropic.com — and nothing there fingerprint-gates anyone. Dressing
	// it up as Chrome would be dishonest for no benefit.
	tr := transport.NewStd(trOpts)

	// The Codex leg gets its own transport, because chatgpt.com is the only
	// endpoint that might ever fingerprint-gate us. Default auto: std now, uTLS
	// from the first gate onward. Separate transports also mean separate
	// connection pools, so a switch on the Codex leg cannot disturb an
	// in-flight Anthropic stream.
	codexTr, err := transport.New(cfg.Codex.Transport, trOpts, log)
	if err != nil {
		return nil, err
	}
	if codexTr.Kind() != transport.KindStd {
		log.Warn("the codex leg is starting on a non-standard TLS transport",
			slog.String("codex.transport", cfg.Codex.Transport),
			slog.String("kind", codexTr.Kind()))
	}

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
	//
	// It dials on the CODEX transport, not the default one: {base}/models and
	// {base}/responses are the same host, chatgpt.com, and that host is the whole
	// reason uTLS exists. On its own client the catalog would keep dialling std
	// after a gate had already flipped inference onto uTLS, so the picker would
	// serve no GPT rows and per-request effort clamping would stay stuck on the
	// compiled-in seed. The constructor copies the client and re-imposes both its
	// no-redirect policy and its own fetch timeout.
	cat := catalog.New(catalog.Options{
		BaseURL:       cfg.Codex.BaseURL,
		CachePath:     cfg.Codex.CachePath,
		ClientVersion: cfg.Codex.ClientVersion,
		HTTPClient:    codexTr.Client(),
		Logger:        log,
	})

	// routing.alias_overrides, applied before anything loads a catalog, since
	// LoadCatalog consults whatever overrides the registry already holds. This
	// is the escape hatch for a slug the alias grammar cannot parse, so a
	// newly-shipped irregular model is routable without a new build.
	reg := router.DefaultRegistry
	for _, ov := range cfg.Routing.AliasOverrides {
		reg.SetOverride(ov.Slug, ov.Codename, ov.Version, ov.Modifier)
		log.Info("registered a routing alias override",
			slog.String("slug", ov.Slug), slog.String("codename", ov.Codename),
			slog.String("version", ov.Version), slog.String("modifier", ov.Modifier))
	}

	// Every successful catalog read republishes the router's aliases. Without
	// this the registry stays on the compiled-in static seed forever: a retired
	// slug keeps resolving to a model the backend no longer serves, and a new
	// codename never resolves at all — while /healthz and the leg's own effort
	// clamping, which do read the live catalog, show the new one.
	loadAliases := newAliasLoader(reg, log)

	obsv := newCodexObserver()

	// Why a catalog read succeeded, failed, or was never attempted. The catalog
	// client itself only remembers what it HOLDS, so "models: 0" read the same
	// whether the backend served an empty list, the fetch failed, or nothing had
	// ever asked. That ambiguity was observed live and is what this records.
	catState := newCatalogState()

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
			Transport: codexTr,
			Logger:    log,
			// The quota windows the backend reports on every answer, success or
			// failure. Without this hook they were only ever seen on a failure.
			OnRateLimits: obsv.observeRateLimits,
		}),
		Catalog:         cat,
		OnCatalog:       loadAliases,
		OnUnknownEvents: obsv.observeUnknownEvents,
		Estimator:       tokens.Default(),
		UpstreamIdle:    cfg.Limits.UpstreamIdleTimeout,
		Logger:          log,
	}
	if credSource != nil {
		legOpts.Credentials = credSource
	}
	codexLeg, err := leg.New(legOpts)
	if err != nil {
		return nil, err
	}

	models, err := newDiscovery(cfg, tr, cat, credSource, loadAliases, catState, log)
	if err != nil {
		return nil, err
	}

	d := &dispatcher{anthropic: passthrough, codex: codexLeg}

	hr := &healthReporter{
		cat: cat, catState: catState, obs: obsv,
		anthTransport: tr, codexTransport: codexTr, tracer: tracer,
	}
	if credSource != nil {
		hr.auth = credSource
	}

	srv, err := server.New(server.Options{
		Config:        cfg,
		Logger:        log,
		Version:       version,
		Activity:      activity,
		HealthExtra:   hr.extra,
		TransportKind: tr.Kind,
		Tracer:        tracer,
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
	if err != nil {
		return nil, err
	}

	return &app{
		srv:  srv,
		warm: newCatalogWarmer(cat, credSource, loadAliases, catState, log),
	}, nil
}

// catalogWarmTimeout bounds the startup catalog fetch. It is generous, because
// nothing waits on it, and finite, because a hung fetch must not leave
// /healthz reporting "warming" forever.
const catalogWarmTimeout = 30 * time.Second

// newCatalogWarmer returns the startup catalog warm.
//
// This closes a real gap: the catalog is fetched lazily, so on a freshly
// started daemon /healthz reported "codex_catalog: models 0" — indistinguishable
// from a broken credential — until the first inference request or picker open
// happened to populate it. Warming on startup makes the model count a fact
// about the backend rather than a fact about whether anyone has used the proxy
// yet.
//
// Three properties are load-bearing:
//
//   - NON-BLOCKING. It returns immediately and the daemon serves regardless.
//     The Anthropic leg must never wait on the Codex catalog.
//   - FAILURE-TOLERANT. A failure is recorded and reported on /healthz; it is
//     never fatal, and the ordinary lazy paths still work afterwards.
//   - CREDENTIAL-SAFE. Like a picker open, it goes through the Peek gate: a
//     startup fetch is not a good enough reason to rotate the refresh token
//     that the user's own Codex CLI shares.
//
// It also detaches from the caller's context for cancellation while keeping its
// values, so a warm in flight does not hold up shutdown and does not die the
// instant run's context is replaced.
func newCatalogWarmer(cat *catalog.Client, credSource *auth.Source, loadAliases func([]cschema.Model), st *catalogState, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		if credSource == nil {
			st.unavailable("no codex credential is configured")
			return
		}
		if peek := credSource.Peek(); peek.State != auth.StateOK {
			st.unavailable(fmt.Sprintf("codex credential is %q", peek.State))
			return
		}
		st.begin()
		go func() {
			wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogWarmTimeout)
			defer cancel()

			cred, err := credSource.Get(wctx)
			if err != nil {
				st.fail(err)
				log.Warn("codex catalog warm skipped: no usable credential",
					slog.String("err", err.Error()))
				return
			}
			models, err := cat.Models(wctx, cred)
			if err != nil {
				st.fail(err)
				log.Warn("codex catalog warm failed; the catalog will be fetched on first use",
					slog.String("err", err.Error()))
				return
			}
			loadAliases(models)
			st.succeed(len(models))
			log.Info("codex catalog warmed at startup", slog.Int("models", len(models)))
		}()
	}
}

// newDiscovery builds the merged GET /v1/models handler that populates Claude
// Code's model picker.
//
// The Codex half is deliberately read through a Peek gate. discovery's contract
// is that opening a picker must never be a reason to rotate a credential, and
// auth.Source.Get can trigger a refresh and an auth.json write-back shared with
// the Codex CLI. A live token serves real rows; anything else serves none,
// which the handler already degrades to gracefully.
func newDiscovery(cfg config.Config, tr transport.Transport, cat *catalog.Client, credSource *auth.Source, loadAliases func([]cschema.Model), catState *catalogState, log *slog.Logger) (http.Handler, error) {
	anthCat, err := anthropic.NewCatalog(cfg.Anthropic.BaseURL, tr, anthropic.WithCatalogLogger(log))
	if err != nil {
		return nil, err
	}

	codexCat := discovery.CodexCatalogFunc(func(ctx context.Context) ([]cschema.Model, error) {
		if credSource == nil {
			err := errors.New("no codex credential is configured")
			catState.unavailable(err.Error())
			return nil, err
		}
		if st := credSource.Peek(); st.State != auth.StateOK {
			err := fmt.Errorf("codex credential is %q; not refreshing it just to open the model picker", st.State)
			catState.unavailable(err.Error())
			return nil, err
		}
		cred, err := credSource.Get(ctx)
		if err != nil {
			catState.fail(err)
			return nil, err
		}
		models, err := cat.Models(ctx, cred)
		if err != nil {
			// A picker open is the other place a catalog read happens, so it is
			// the other place a failure can be OBSERVED. Recording it here means
			// /healthz explains a stubbornly empty catalog even on a daemon whose
			// startup warm was never run.
			catState.fail(err)
			return nil, err
		}
		catState.succeed(len(models))
		// A picker open is the other place a live catalog is already in hand, so
		// it republishes the router's aliases too. Discovery derives the names it
		// advertises FROM the registry, so loading first is what stops a picker
		// offering yesterday's codenames.
		loadAliases(models)
		return models, nil
	})

	return discovery.New(discovery.Options{
		Anthropic: anthCat,
		Codex:     codexCat,
		Registry:  router.DefaultRegistry,
		Logger:    log,
	})
}

// newAliasLoader returns the hook that republishes the router's alias tiers from
// a live Codex model list.
//
// It is deliberately driven by reads that already happen — the leg's per-request
// effort-clamping lookup and a picker open — rather than by a timer: no extra
// upstream request, no extra goroutine, and no work at all until something
// actually needs the catalog. An unchanged list is a no-op, so the common case
// costs one fingerprint comparison rather than rebuilding three maps under the
// registry's write lock while every Resolve waits.
func newAliasLoader(reg *router.Registry, log *slog.Logger) func([]cschema.Model) {
	var (
		mu   sync.Mutex
		last string
	)
	return func(models []cschema.Model) {
		if len(models) == 0 {
			// Never clear the registry on an empty read: an empty catalog would
			// un-route every model, and "we could not see the catalog" is far
			// more likely than "Codex serves nothing".
			return
		}
		fp := catalogFingerprint(models)
		mu.Lock()
		defer mu.Unlock()
		if fp == last {
			return
		}
		catalog.PopulateRegistry(reg, models)
		last = fp
		log.Info("router aliases republished from the live codex catalog",
			slog.Int("models", len(models)),
			slog.Any("families", reg.Families()))
	}
}

// catalogFingerprint identifies a model list by exactly the data the alias
// tiers are derived from — the listed slugs and their priorities — so a catalog
// that only changed a display name does not churn the registry.
func catalogFingerprint(models []cschema.Model) string {
	entries := catalog.ListedEntries(models)
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.Slug+"#"+strconv.Itoa(e.Priority))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// codexObserver holds the non-secret operational state /healthz reports about
// the Codex leg: the quota windows the backend last named, and the running
// count of stream event types the translator did not recognise.
//
// Both are hooks off paths that already run, so observing costs nothing when
// nothing is happening. No token, account id or request body is ever held here.
type codexObserver struct {
	mu       sync.Mutex
	rl       responses.RateLimits
	seenRL   bool
	unknown  map[string]int
	streams  int64
	nowFn    func() time.Time
	observed time.Time
}

func newCodexObserver() *codexObserver {
	return &codexObserver{unknown: map[string]int{}, nowFn: time.Now}
}

// observeRateLimits records the quota headers of an upstream answer. It is
// called from the responses client on every response, success or failure.
func (o *codexObserver) observeRateLimits(rl responses.RateLimits) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rl, o.seenRL, o.observed = rl, true, o.nowFn()
}

// observeUnknownEvents accumulates the per-type counts of unrecognised Codex
// stream events. A non-zero count here is the early warning that the upstream
// protocol has drifted and the translator is silently dropping something.
func (o *codexObserver) observeUnknownEvents(counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.streams++
	for typ, n := range counts {
		o.unknown[typ] += n
	}
}

// quota renders the last-seen usage windows, or nil when the backend has not
// reported any yet.
func (o *codexObserver) quota() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.seenRL {
		return nil
	}
	out := map[string]any{"age_s": roundSeconds(o.nowFn().Sub(o.observed).Seconds())}
	if w := windowFields(o.rl.Primary); w != nil {
		out["primary"] = w
	}
	if w := windowFields(o.rl.Secondary); w != nil {
		out["secondary"] = w
	}
	if o.rl.HasRetryAfter {
		out["retry_after_s"] = roundSeconds(o.rl.RetryAfter.Seconds())
	}
	return out
}

// quotaHealth is quota with a "reported" flag, so codex_quota is ALWAYS present
// on /healthz. An absent block is easy to read as "quota is fine"; an explicit
// reported:false says the backend has not told us anything yet, which is a
// different and much less reassuring statement.
func (o *codexObserver) quotaHealth() map[string]any {
	q := o.quota()
	if q == nil {
		return map[string]any{"reported": false}
	}
	q["reported"] = true
	return q
}

func windowFields(w responses.Window) map[string]any {
	if !w.Reported() {
		return nil
	}
	out := map[string]any{}
	if w.HasUsedPercent {
		out["used_percent"] = w.UsedPercent
	}
	if w.HasWindowMinutes {
		out["window_minutes"] = w.WindowMinutes
	}
	if w.HasResetAfter {
		out["reset_after_s"] = roundSeconds(w.ResetAfter.Seconds())
	}
	return out
}

// unknownEvents renders the drift counters. Always present, so "zero" is a
// reported fact rather than an absence a reader has to interpret.
func (o *codexObserver) unknownEvents() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	byType := make(map[string]int, len(o.unknown))
	total := 0
	for typ, n := range o.unknown {
		byType[typ] = n
		total += n
	}
	return map[string]any{
		"unknown_events":        total,
		"unknown_event_types":   byType,
		"streams_with_unknowns": o.streams,
	}
}

// Values of /healthz's codex_catalog.state. They exist because "models: 0" on
// its own is several different situations wearing one face, and only some of
// them are worth acting on.
const (
	// catalogCold: nothing has tried to read the catalog yet.
	catalogCold = "cold"
	// catalogWarming: the startup fetch is in flight.
	catalogWarming = "warming"
	// catalogLoaded: a catalog is held and it names models.
	catalogLoaded = "loaded"
	// catalogEmpty: a fetch SUCCEEDED and the backend listed nothing. This is
	// the one that must never be confused with a failure.
	catalogEmpty = "empty"
	// catalogFailed: the last attempt errored; last_error says how.
	catalogFailed = "failed"
	// catalogUnavailable: no attempt is possible — no `codex login` on this
	// machine, or a credential not in a state worth spending a refresh on.
	catalogUnavailable = "unavailable"
)

// catalogState records WHY the catalog looks the way it does. The catalog
// client itself only knows what it HOLDS; this knows what happened when
// something last tried to fill it.
type catalogState struct {
	mu      sync.Mutex
	state   string
	err     string
	at      time.Time
	nowFn   func() time.Time
	attempt bool
}

func newCatalogState() *catalogState {
	return &catalogState{state: catalogCold, nowFn: time.Now}
}

func (s *catalogState) mark(state, err string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state, s.err, s.at, s.attempt = state, err, s.nowFn(), true
}

func (s *catalogState) begin() { s.mark(catalogWarming, "") }

func (s *catalogState) succeed(models int) {
	if models == 0 {
		s.mark(catalogEmpty, "")
		return
	}
	s.mark(catalogLoaded, "")
}

func (s *catalogState) fail(err error) {
	if err == nil {
		return
	}
	s.mark(catalogFailed, err.Error())
}

func (s *catalogState) unavailable(reason string) { s.mark(catalogUnavailable, reason) }

// read returns the recorded state, the last error message, and how long ago the
// last attempt was (negative when there has been none).
func (s *catalogState) read() (state, err string, age time.Duration) {
	if s == nil {
		return catalogCold, "", -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.attempt {
		return s.state, s.err, -1
	}
	return s.state, s.err, s.nowFn().Sub(s.at)
}

// healthReporter contributes the codex auth, catalog, quota, drift, transport
// and tracing fields to /healthz. It reads only cached/local state — a
// token-expiry decode from a file stat and the catalog's held snapshot — so a
// health poll never contacts the network and never reveals a token value.
type healthReporter struct {
	auth     *auth.Source    // nil when no credential file is configured
	cat      *catalog.Client // always set
	catState *catalogState   // always set
	obs      *codexObserver  // always set
	// The two legs hold SEPARATE transports and only the Codex one can ever
	// change stack, so both are reported. Reporting one number for "the"
	// transport was wrong in the only case that matters: after an auto flip,
	// the Anthropic leg is still std and the Codex leg is not.
	anthTransport  transport.Transport // always set
	codexTransport transport.Transport // always set
	tracer         *obs.Tracer         // nil unless tracing is on
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

	out := map[string]any{
		"codex_auth":    codex,
		"codex_catalog": h.catalogHealth(),
	}
	if h.obs != nil {
		out["codex_stream"] = h.obs.unknownEvents()
		// Always present, carrying reported:false until the backend has said
		// anything. An ABSENT quota block reads as "no quota problem", and the
		// whole point of reporting burn-down is to see it before it is one.
		out["codex_quota"] = h.obs.quotaHealth()
	}
	// Which TLS stack is in force, per leg. Under the auto transport the Codex
	// one can change mid-process, so both are read live rather than captured at
	// startup. "kind" names the CODEX stack: it is the only one that can flip,
	// and a single-valued field that could never change would be worthless.
	tr := map[string]any{}
	if h.anthTransport != nil {
		tr["anthropic"] = h.anthTransport.Kind()
	}
	if h.codexTransport != nil {
		tr["codex"] = h.codexTransport.Kind()
		tr["kind"] = h.codexTransport.Kind()
	}
	if len(tr) > 0 {
		out["transport"] = tr
	}
	// Tracing state is reported because a directory of prompts accumulating on
	// disk should never be a surprise. The path is not a secret; the contents
	// are, which is what the startup warning is about.
	trace := map[string]any{"enabled": h.tracer.Enabled()}
	if dir := h.tracer.Dir(); dir != "" {
		trace["dir"] = dir
	}
	out["trace"] = trace
	// The alias families the router currently routes. It is the quickest way to
	// see whether the live catalog has been loaded or the static seed is still
	// in force.
	out["codex_routing"] = map[string]any{"families": router.DefaultRegistry.Families()}
	return out
}

// catalogHealth renders codex_catalog: how many models are held, how old the
// snapshot is, and — the part that was missing — WHY it looks like that.
//
// The snapshot wins when it holds something, because a held catalog is a fact
// and a recorded attempt is only a memory of one. When it holds nothing, the
// recorded state is the whole answer: "empty" (the backend really listed
// nothing), "failed" (with last_error), "unavailable" (no credential to try
// with), "warming" (the startup fetch is in flight), or "cold" (nothing has
// tried yet).
func (h *healthReporter) catalogHealth() map[string]any {
	info := map[string]any{"models": 0, "loaded": false}

	var (
		n      int
		age    time.Duration
		loaded bool
	)
	if h.cat != nil {
		n, age, loaded = h.cat.Snapshot()
		info["models"] = n
		info["loaded"] = loaded
		if loaded {
			info["age_s"] = roundSeconds(age.Seconds())
		}
	}

	state, lastErr, sinceAttempt := h.catState.read()
	switch {
	case loaded && n > 0:
		state = catalogLoaded
	case loaded && n == 0:
		state = catalogEmpty
	}
	info["state"] = state
	if lastErr != "" {
		info["last_error"] = lastErr
	}
	if sinceAttempt >= 0 {
		info["last_attempt_age_s"] = roundSeconds(sinceAttempt.Seconds())
	}
	return info
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

	// The routing decision is the half of the request line only the dispatcher
	// knows. It goes onto the shared summary rather than a log line of its own,
	// so one request stays one record; the DEBUG line below carries the extra
	// detail (where the effort came from) that only matters when something has
	// already surprised you.
	sum := obs.SummaryFrom(ctx)
	sum.SetRoute(dec.Backend.String())
	sum.SetModels(dec.ClientModel, dec.UpstreamModel)
	sum.SetEffort(dec.Effort)
	sum.SetStream(p.Stream)
	// The dispatcher read the body, so it knows its real size — better than the
	// declared Content-Length the middleware had to settle for.
	sum.SetReqBytes(int64(len(raw)))

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
	sum := obs.SummaryFrom(ctx)
	if errors.Is(err, router.ErrClientGone) {
		// The caller hung up. Not a failure of ours, and it must not be counted
		// as one on the request line.
		sum.SetInterrupted(true)
	} else {
		sum.SetErr(err)
	}
	if errors.Is(err, router.ErrResponseStarted) || errors.Is(err, router.ErrClientGone) {
		log.LogAttrs(context.WithoutCancel(ctx), slog.LevelWarn,
			"leg failed after the response started", slog.String("err", err.Error()))
		return
	}
	log.LogAttrs(ctx, slog.LevelWarn, "leg failed", slog.String("err", err.Error()))
	_ = apierr.Write(w, err)
}
