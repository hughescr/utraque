// Package catalog fetches and caches the Codex model catalog
// (GET {base}/models), the list utraque's router derives its short model
// aliases from. Each committed list is handed to Options.OnModels, which cmd
// uses to republish the router's aliases (registry.go adapts a model list into
// router aliases: PopulateRegistry).
//
// The catalog is cheap to hold and expensive to fetch, so this client layers
// three staleness controls over one in-memory snapshot:
//
//   - TTL: within Options.TTL of the last fetch the snapshot is served with no
//     network call at all.
//   - stale-while-revalidate: once past the TTL the stale snapshot is returned
//     IMMEDIATELY and a single background revalidation is kicked off, so a
//     caller never blocks on a refresh when it already has an answer.
//   - ETag: every fetch sends If-None-Match, so an unchanged catalog comes back
//     304 and the held models are reused (only fetched_at advances).
//
// The backend lists models per account, so the snapshot is tagged with the
// account scope (auth.Credential.Scope) of the credential that fetched it, and
// serves only callers presenting that same scope. A caller for any other
// account misses: it fetches with its own credential, offers no ETag (another
// account's validator must not revalidate another account's list), and on
// failure gets the error a cold client would, never the held rows.
//
// The snapshot is keyed by client_version as well as by time. The backend
// gates which models it lists on the client_version query parameter, so a
// catalog, and the ETag that validates it, describe the list for ONE client
// version. Each snapshot records the version it was fetched with. A fetch
// under a different version sends no If-None-Match, so a 304 can never
// revalidate the old version's list under the new one, and the disk cache is
// ignored on load when its client_version differs. The version itself is
// resolved before every fetch (Options.ClientVersionFunc), so a Codex CLI
// upgrade while utraque runs reaches the next revalidation.
//
// An optional on-disk cache lets a cold start serve the last-known catalog
// before the first successful fetch. The record carries the account scope and
// endpoint it was fetched for, and is used only for that same pair. Its file
// shape is interoperable with the Codex CLI's own models_cache.json, but
// utraque keeps its OWN file and never writes the CLI's.
//
// This package never logs a token or an account id in the clear; the credential
// it receives is used only to sign the request.
package catalog

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/codex/wire"
	"github.com/hughescr/utraque/internal/obs"
)

// Defaults used when Options leaves a field zero. The backend root and the
// request identity come from internal/codex/wire; the catalog lives at the
// root's "/models" subpath.
const (
	// DefaultTTL is how long a fetched catalog is served without revalidation.
	DefaultTTL = 300 * time.Second
	// backgroundRefreshTimeout bounds a stale-while-revalidate fetch, which
	// runs detached from any request context.
	backgroundRefreshTimeout = 30 * time.Second
	// maxBody caps how much of a catalog response body we read.
	maxBody = 8 << 20 // 8 MiB
	// maxClockSkew is how far into the future a disk cache's fetched_at may sit
	// before we distrust it as a clock artefact and re-fetch.
	maxClockSkew = 5 * time.Minute

	modelsPath     = "/models"
	queryClientVer = "client_version"
)

// Catalog is the read side the router and /healthz depend on: the current model
// list, and how old the held snapshot is.
type Catalog interface {
	// Models returns the current catalog, fetching or revalidating as the
	// staleness rules require. cred signs the request; it is never stored.
	Models(ctx context.Context, cred auth.Credential) ([]cschema.CatalogModel, error)
	// Age reports how long ago the held snapshot was fetched, or 0 if none has
	// been obtained yet.
	Age() time.Duration
}

// Options configures a Client. Only zero-valued fields are defaulted; a caller
// that wants the real endpoint can leave BaseURL empty.
type Options struct {
	// BaseURL is the Codex backend root. Defaults to wire.DefaultBaseURL.
	BaseURL string
	// CacheFile is utraque's own on-disk cache file. Empty disables disk
	// caching (memory only). It MUST NOT be the Codex CLI's models_cache.json:
	// utraque never overwrites the CLI's cache.
	CacheFile string
	// TTL is the fresh window. Defaults to DefaultTTL. A negative value is
	// treated as the default.
	TTL time.Duration
	// ClientVersion is sent as the client_version query parameter on every
	// models request — the real endpoint 400s the request outright without it
	// ("client_version" reported as a missing required query field) — and is
	// also recorded in the on-disk cache, which is keyed by it. It is NOT
	// defaulted here. With no ClientVersionFunc it is the fixed version for the
	// life of the client (an explicit override, or a test). With one, it is
	// only the seed: the version the disk cache is checked against and the one
	// /healthz reports before the first fetch. A caller that builds Options
	// directly must set it if the query parameter matters to what it is
	// asserting.
	ClientVersion string
	// ClientVersionFunc, when set, re-resolves the client version before every
	// fetch, so an upgraded Codex CLI is noticed without restarting utraque.
	// Production passes a discoverer that stats the Codex executable and only
	// re-runs `codex --version` when the file changed. It is called with the
	// fetch's context, at most once per fetch (fetches are collapsed by
	// singleflight), and never from /healthz. An empty result keeps the version
	// already in use, so a failed re-discovery never fails a fetch. It must be
	// safe for concurrent use.
	ClientVersionFunc ClientVersionFunc
	// HTTPClient performs the fetch. Defaults to a client with a modest
	// timeout. Tests inject one aimed at a fake server.
	HTTPClient *http.Client
	// Now supplies the current time. Defaults to time.Now. Tests override it.
	Now func() time.Time
	// Logger receives redacted operational logs. Defaults to slog.Default.
	Logger *slog.Logger
	// OnFetch observes actual fetch completions (including background refresh),
	// once per singleflight, never on a cache hit, with the account scope of
	// the credential that made the fetch. A nil failure means success. It is
	// not called for a background revalidation whose result was discarded.
	OnFetch func(scope string, models int, failure *Failure)
	// OnModels receives every list the client commits — a fetch, a 304, or the
	// disk record loaded on a cold start — in commit order. The rows are shared
	// with the snapshot and must not be modified.
	OnModels func([]cschema.CatalogModel)
}

// ClientVersionFunc reports the Codex client version the next fetch should
// send. See Options.ClientVersionFunc for the contract.
type ClientVersionFunc func(ctx context.Context) string

// Client is a caching catalog fetcher. It is safe for concurrent use.
type Client struct {
	baseURL   string
	cacheFile string
	ttl       time.Duration
	versionFn ClientVersionFunc
	http      *http.Client
	now       func() time.Time
	log       *slog.Logger
	onFetch   func(string, int, *Failure)
	onModels  func([]cschema.CatalogModel)

	// group collapses concurrent fetches (foreground + background) for one
	// account scope into one; the key is the scope, so a caller for one
	// account never joins, or is handed the result of, another account's fetch.
	group singleflight.Group
	// refreshing guards against launching more than one background
	// revalidation at a time.
	refreshing atomic.Bool
	// disk reads the disk record at most once per process, on the first miss,
	// for that caller's account only. A record for any other account is never
	// read later, so a second account's first call goes to the network.
	disk sync.Once
	// stageHook, when set (tests only), runs as a record starts to be staged,
	// where a slow filesystem would stall.
	stageHook func()

	// Lock graph: commitMu → mu, always in that order.
	//
	// commitMu serializes commits — a fetch outcome or the disk record — with
	// OnModels, OnFetch and the cache file's rename, so the published aliases,
	// the health record and the file follow the snapshot in commit order. mu
	// guards the request-visible state and is held only for in-memory reads
	// and writes; a warm hit takes only mu, for reading. No network call and
	// no file I/O other than that rename runs under either lock: the fetch, the
	// disk read, and the temp file's write and fsync all happen unlocked. The
	// callbacks take only their own leaf locks and never call back in here.
	commitMu sync.Mutex
	mu       sync.RWMutex
	st       state
	// clientVersion is the version in use: the seed from Options, replaced by
	// each non-empty ClientVersionFunc result. Guarded by mu.
	clientVersion string
}

// state is the in-memory snapshot. scope is the account scope of the
// credential it was fetched with (or the disk record's); clientVersion is the
// version it was fetched with: its etag validates the model list for that
// account and version only.
type state struct {
	scope         string
	models        []cschema.CatalogModel
	etag          string
	clientVersion string
	fetchedAt     time.Time
	loaded        bool
}

// holds reports whether the snapshot is a list fetched for scope.
func (s state) holds(scope string) bool { return s.loaded && s.scope == scope }

const defaultHTTPTimeout = 30 * time.Second

var _ Catalog = (*Client)(nil)

// New builds a Client. No network or disk access happens here.
func New(opts Options) *Client {
	c := &Client{
		baseURL:       opts.BaseURL,
		cacheFile:     opts.CacheFile,
		ttl:           opts.TTL,
		versionFn:     opts.ClientVersionFunc,
		clientVersion: opts.ClientVersion,
		http:          opts.HTTPClient,
		now:           opts.Now,
		log:           opts.Logger,
		onFetch:       opts.OnFetch,
		onModels:      opts.OnModels,
	}
	if c.baseURL == "" {
		c.baseURL = wire.DefaultBaseURL
	}
	if c.ttl <= 0 {
		c.ttl = DefaultTTL
	}
	// A redirect is NEVER followed. This request carries the Codex bearer token,
	// the account id, and the CLI's beta/originator headers; Go would replay all
	// of them at whatever host the response named, and keep the Authorization
	// header for any same-site hop. The inference client and the OAuth refresh
	// client already forbid it; the catalog must not be the one door left open.
	//
	// A caller-supplied client is COPIED rather than mutated, so the invariant
	// holds without reaching into something the caller still owns.
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultHTTPTimeout}
	} else {
		cp := *c.http
		c.http = &cp
	}
	rt := c.http.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.http.Transport = discardRedirectTarget{rt}
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// A catalog read is a bounded request/response, not a stream, so it always
	// gets an overall deadline. The proxy's shared transports deliberately carry
	// Timeout 0 — an overall deadline would cut a long SSE answer — so passing
	// one of those in must not silently make this fetch unbounded. The copy above
	// is what makes setting it safe.
	if c.http.Timeout <= 0 {
		c.http.Timeout = defaultHTTPTimeout
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c
}

// net/http parses Location before calling CheckRedirect and discards the
// response on a parse error. Drop a redirect's unused target at the transport
// boundary, so even that case is classified by its actual HTTP status.
type discardRedirectTarget struct{ http.RoundTripper }

func (t discardRedirectTarget) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(r)
	if resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Header.Del("Location")
	}
	return resp, err
}

// Models implements Catalog.
func (c *Client) Models(ctx context.Context, cred auth.Credential) ([]cschema.CatalogModel, error) {
	scope := cred.Scope()
	c.mu.RLock()
	st := c.st
	c.mu.RUnlock()

	if !st.holds(scope) {
		// A miss: nothing held, or another account's list. The first miss of
		// the process consults the disk record, for this caller's account only;
		// later misses, for any account, go straight to the network.
		c.disk.Do(func() { c.loadDisk(scope) })
		c.mu.RLock()
		st = c.st
		c.mu.RUnlock()
	}

	if st.holds(scope) {
		if c.now().Sub(st.fetchedAt) < c.ttl {
			return cloneModels(st.models), nil // fresh
		}
		// Stale: serve immediately, revalidate in the background. The request
		// ctx must not gate the background fetch — it will be cancelled the
		// moment this call returns.
		c.revalidateAsync(cred)
		return cloneModels(st.models), nil
	}

	// Cold, or held for another account: nothing this caller may be served,
	// so fetch synchronously with its own credential.
	ns, err := c.fetchShared(ctx, cred, false)
	if err != nil {
		return nil, err
	}
	return cloneModels(ns.models), nil
}

// Age implements Catalog.
func (c *Client) Age() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.st.loaded {
		return 0
	}
	return c.now().Sub(c.st.fetchedAt)
}

// Snapshot reports the currently held catalog without any network fetch, disk
// read or revalidation: how many models are held and how old the held snapshot
// is. scope is the account the caller considers current; a snapshot fetched
// for any other account reads as not loaded, and "" (current account unknown)
// accepts whatever is held. loaded is false when nothing has been obtained yet.
// It is what /healthz reads, so a health poll never touches the network or the
// disk and changes nothing.
func (c *Client) Snapshot(scope string) (models int, age time.Duration, loaded bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.st.loaded || scope != "" && c.st.scope != scope {
		return 0, 0, false
	}
	return len(c.st.models), c.now().Sub(c.st.fetchedAt), true
}

// ClientVersion reports the client_version the catalog is using: the one its
// most recent fetch sent, or, before any fetch, the seed from Options. Like
// Snapshot it never fetches and never re-resolves the version, so /healthz can
// read it for free.
func (c *Client) ClientVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientVersion
}

// resolveClientVersion asks ClientVersionFunc, when there is one, for the
// version the next fetch should send and records it as the version in use. An
// empty answer keeps the version already in use.
func (c *Client) resolveClientVersion(ctx context.Context) string {
	if c.versionFn != nil {
		if v := c.versionFn(ctx); v != "" {
			c.mu.Lock()
			c.clientVersion = v
			c.mu.Unlock()
			return v
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientVersion
}

// versionsDiffer reports whether a held snapshot's client version rules it out
// under the version in use. Only a difference between two KNOWN versions
// counts, so a version-less cache (a test fixture, an old file) still loads.
func versionsDiffer(held, current string) bool {
	return held != "" && current != "" && held != current
}

// revalidateAsync launches at most one background fetch. Its result is stored
// on success, unless the held snapshot has meanwhile become another account's;
// failures are observed and logged (never fatal) — the caller already has a
// stale-but-usable snapshot.
func (c *Client) revalidateAsync(cred auth.Credential) {
	if !c.refreshing.CompareAndSwap(false, true) {
		return // one already in flight
	}
	go func() {
		defer c.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), backgroundRefreshTimeout)
		defer cancel()
		if _, err := c.fetchShared(ctx, cred, true); err != nil {
			c.log.Warn("codex catalog background revalidation failed",
				slog.String("err", err.Error()))
		}
	}()
}

// fetchShared runs fetch under singleflight, keyed by the credential's account
// scope, so a foreground cold fetch and a background revalidation for the same
// account collapse to one HTTP round-trip. background is the initiating
// caller's kind; it decides the commit rule (see commit).
func (c *Client) fetchShared(ctx context.Context, cred auth.Credential, background bool) (state, error) {
	scope := cred.Scope()
	v, err, _ := c.group.Do(scope, func() (any, error) {
		ns, failure := c.fetch(ctx, cred, scope)
		// Write and fsync the record before taking commitMu, so a slow or hung
		// filesystem delays only this fetch, never another account's commit.
		var staged string
		if failure == nil {
			staged = c.stageDisk(ns)
		}
		if !c.commit(scope, ns, failure, staged, background) {
			_ = os.Remove(staged) // after commitMu is released
		}
		if failure != nil {
			return state{}, failure
		}
		return ns, nil
	})
	if err != nil {
		return state{}, err
	}
	return v.(state), nil
}

// commit applies one fetch outcome for scope under commitMu, and reports
// whether staged (if any) is gone, i.e. renamed into place.
//
// A foreground fetch always commits: it answers a live request made with that
// account's credential. So a slow fetch for account A that completes after a
// newer one for B leaves A's list held; B's next call then misses on scope and
// fetches B's list again. That costs one extra fetch and corrects itself, and
// at no point serves A's rows to B. A background revalidation for scope is
// discarded, unreported, when the held snapshot is no longer scope's: it was
// only refreshing a list that has since been replaced. The one qualification:
// a foreground caller that joins a flight STARTED by a background revalidation
// shares that flight's result, so it inherits the flight's discard rule.
func (c *Client) commit(scope string, ns state, failure *Failure, staged string, background bool) (installed bool) {
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	if background && c.st.scope != scope {
		c.log.Debug("codex catalog revalidation discarded: another account's list is now held")
		return staged == ""
	}
	installed = true
	if failure == nil {
		c.install(ns)
		installed = c.installDisk(staged)
	}
	if c.onFetch != nil {
		c.onFetch(scope, len(ns.models), failure)
	}
	return installed
}

// install publishes ns and makes it the held snapshot. The caller holds
// commitMu.
func (c *Client) install(ns state) {
	if c.onModels != nil {
		c.onModels(ns.models)
	}
	c.mu.Lock()
	c.st = ns
	c.mu.Unlock()
}

// fetch performs one conditional GET for scope's account. On 304 it reuses the
// held models; on 200 it replaces them.
func (c *Client) fetch(ctx context.Context, cred auth.Credential, scope string) (state, *Failure) {
	// Re-resolve first: an upgraded Codex CLI must reach THIS request, not the
	// one after it.
	clientVersion := c.resolveClientVersion(ctx)

	c.mu.RLock()
	prev := c.st
	c.mu.RUnlock()
	// Another account's snapshot is not this account's previous list: its
	// validator is never offered and its rows are never reused.
	hadPrev := prev.holds(scope)
	prevEtag, prevModels, prevVersion := prev.etag, prev.models, prev.clientVersion
	if !hadPrev {
		prevEtag = ""
	}

	// The held etag validates the list the backend served to prevVersion. The
	// backend gates models on client_version, so under a new version that
	// validator must not be offered: a 304 would keep the old version's list
	// (the one missing whatever the upgrade unlocked) and stamp it fresh.
	if hadPrev && prevEtag != "" && versionsDiffer(prevVersion, clientVersion) {
		c.log.Debug("codex client version changed since the held catalog was fetched; fetching it unconditionally",
			slog.String("cache_client_version", prevVersion),
			slog.String("client_version", clientVersion))
		prevEtag = ""
	}

	// client_version is a REQUIRED query parameter, not a header: the real
	// endpoint 400s the request outright without it (observed live: "field
	// required" at ('query', 'client_version')). It is parsed and re-encoded
	// through url.URL/url.Values rather than string-concatenated, so a future
	// BaseURL carrying its own query string (or a value needing escaping) is
	// handled correctly rather than silently producing a malformed request.
	reqURL, err := url.Parse(c.baseURL + modelsPath)
	if err != nil {
		return state{}, &Failure{reason: invalidRequest}
	}
	q := reqURL.Query()
	q.Set(queryClientVer, clientVersion)
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return state{}, &Failure{reason: invalidRequest}
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set(wire.HeaderAccountID, cred.AccountID)
	req.Header.Set(wire.HeaderOpenAIBeta, wire.OpenAIBetaValue)
	req.Header.Set(wire.HeaderOriginator, wire.Originator)
	req.Header.Set("Accept", "application/json")
	if prevEtag != "" {
		req.Header.Set("If-None-Match", prevEtag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return state{}, classifyIO(err, 0)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if !hadPrev || prevEtag == "" {
			// 304 to an unconditional request is a protocol surprise: we only
			// send If-None-Match with a held etag, and never across a client
			// version change. Treat it as unusable rather than reuse a list
			// the request did not validate.
			return state{}, &Failure{unexpectedNotModified, resp.StatusCode}
		}
		ns := state{scope: scope, models: prevModels, etag: prevEtag, clientVersion: prevVersion, fetchedAt: c.now(), loaded: true}
		c.log.Debug("codex catalog not modified", slog.Int("models", len(prevModels)))
		return ns, nil

	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return state{}, classifyIO(err, resp.StatusCode)
		}
		var parsed cschema.ModelsResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return state{}, &Failure{decode, resp.StatusCode}
		}
		// Use the response's own ETag, or none. Do NOT fall back to the previous
		// ETag: this 200 body may differ from what that validator described, and
		// pairing new models with a stale validator would let a later 304 for it
		// wrongly mark the new body fresh. No ETag simply means the next fetch is
		// unconditional.
		etag := resp.Header.Get("ETag")
		ns := state{scope: scope, models: parsed.Models, etag: etag, clientVersion: clientVersion, fetchedAt: c.now(), loaded: true}
		c.log.Info("fetched codex catalog",
			slog.Int("models", len(parsed.Models)),
			slog.String("client_version", clientVersion),
			obs.HashAttr("account", cred.AccountID))
		return ns, nil

	case http.StatusUnauthorized:
		return state{}, &Failure{credentialRejected, resp.StatusCode}
	case http.StatusBadRequest:
		return state{}, &Failure{badRequest, resp.StatusCode}
	default:
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return state{}, &Failure{redirectRefused, resp.StatusCode}
		}
		return state{}, &Failure{upstreamStatus, resp.StatusCode}
	}
}

// loadDisk installs the on-disk record as the held snapshot when it was
// written for scope's account at this endpoint and nothing is held yet, so a
// cold start has something to serve. The file is read with no lock held. A
// missing, unreadable, corrupt, unscoped or other-account record is ignored:
// the caller then fetches, and that fetch's record replaces it.
func (c *Client) loadDisk(scope string) {
	if c.cacheFile == "" {
		return
	}
	b, err := os.ReadFile(c.cacheFile)
	if err != nil {
		return
	}
	var cache cschema.Cache
	if err := json.Unmarshal(b, &cache); err != nil {
		return
	}
	if cache.AccountScope == "" || cache.AccountScope != scope || cache.Endpoint != c.baseURL {
		return // not this account's list at this endpoint
	}
	if len(cache.Models) == 0 && cache.FetchedAt.IsZero() {
		return // nothing useful
	}
	// A cache written under a different client version describes a different
	// list — the backend gates models on client_version — so ignore it and
	// re-fetch rather than serve a list a client upgrade may have invalidated.
	// This compares against the version in use (the seed, before any fetch),
	// deliberately without calling ClientVersionFunc. A version change that
	// lands after this load is still caught, by fetch dropping the etag.
	version := c.ClientVersion()
	if versionsDiffer(cache.ClientVersion, version) {
		c.log.Debug("ignoring codex catalog disk cache from a different client version",
			slog.String("cache_client_version", cache.ClientVersion),
			slog.String("client_version", version))
		return
	}
	// A future-dated fetched_at would read as perpetually fresh (negative age),
	// pinning a stale catalog until wall-clock time catches up. Reject anything
	// beyond a small skew allowance.
	if cache.FetchedAt.After(c.now().Add(maxClockSkew)) {
		c.log.Debug("ignoring codex catalog disk cache with a future fetched_at",
			slog.Time("fetched_at", cache.FetchedAt))
		return
	}
	heldVersion := cache.ClientVersion
	if heldVersion == "" {
		heldVersion = version
	}
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	if c.st.loaded {
		return // a fetch committed meanwhile; it is newer than the file
	}
	c.install(state{
		scope:         scope,
		models:        cache.Models,
		etag:          cache.ETag,
		clientVersion: heldVersion,
		fetchedAt:     cache.FetchedAt,
		loaded:        true,
	})
	c.log.Debug("loaded codex catalog from disk cache",
		slog.Int("models", len(cache.Models)),
		slog.Time("fetched_at", cache.FetchedAt))
}

// stageDisk writes the snapshot to a synced temp file beside CacheFile, if one
// is configured, and returns its name ("" when there is nothing to install).
// It takes no lock. Failure is logged and swallowed: the disk cache is an
// optimisation, not a requirement, and a write error must never fail a live
// request.
func (c *Client) stageDisk(ns state) string {
	if c.cacheFile == "" {
		return ""
	}
	if c.stageHook != nil {
		c.stageHook()
	}
	cache := cschema.Cache{
		AccountScope:  ns.scope,
		Endpoint:      c.baseURL,
		ClientVersion: ns.clientVersion,
		ETag:          ns.etag,
		FetchedAt:     ns.fetchedAt,
		Models:        ns.models,
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		c.log.Warn("codex catalog: encode disk cache", slog.String("err", err.Error()))
		return ""
	}
	data = append(data, '\n')
	dir := filepath.Dir(c.cacheFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.log.Warn("codex catalog: create cache dir", slog.String("err", err.Error()))
		return ""
	}
	tmp, err := os.CreateTemp(dir, ".models_cache-*.json.tmp")
	if err != nil {
		c.log.Warn("codex catalog: create temp cache", slog.String("err", err.Error()))
		return ""
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		c.log.Warn("codex catalog: write temp cache", slog.String("err", err.Error()))
		return ""
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		c.log.Warn("codex catalog: sync temp cache", slog.String("err", err.Error()))
		return ""
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		c.log.Warn("codex catalog: close temp cache", slog.String("err", err.Error()))
		return ""
	}
	return tmpName
}

// installDisk renames a staged record into place. The caller holds commitMu,
// so the file always ends as the record of the last committed snapshot. It
// reports whether the staged file is gone, so the caller knows whether to
// remove it once commitMu is released.
func (c *Client) installDisk(staged string) bool {
	if staged == "" {
		return true
	}
	if err := os.Rename(staged, c.cacheFile); err != nil {
		c.log.Warn("codex catalog: rename cache into place", slog.String("err", err.Error()))
		return false
	}
	return true
}

// cloneModels returns a deep copy of the model slice so a caller can hold,
// reorder, or mutate it without racing the client's own snapshot replacement.
// Each Model's SupportedReasoningLevels slice is copied too — a shallow copy
// would leave callers sharing (and able to mutate) the cached backing array,
// racing the goroutines that read the held snapshot.
func cloneModels(in []cschema.CatalogModel) []cschema.CatalogModel {
	if len(in) == 0 {
		return nil
	}
	out := make([]cschema.CatalogModel, len(in))
	copy(out, in)
	for i := range out {
		if len(out[i].SupportedReasoningLevels) > 0 {
			lv := make([]cschema.ReasoningLevel, len(out[i].SupportedReasoningLevels))
			copy(lv, out[i].SupportedReasoningLevels)
			out[i].SupportedReasoningLevels = lv
		}
	}
	return out
}
