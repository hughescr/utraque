// Package catalog fetches and caches the Codex model catalog
// (GET {base}/models), the list utraque's router derives its short model
// aliases from.
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
// An optional on-disk cache lets a cold start serve the last-known catalog
// before the first successful fetch. Its file shape is interoperable with the
// Codex CLI's own models_cache.json, but utraque keeps its OWN file and never
// writes the CLI's.
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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/obs"
)

// Defaults used when Options leaves a field zero.
const (
	// DefaultBaseURL is the Codex backend root; the catalog lives at its
	// "/models" subpath. Override in tests to point at an httptest server.
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
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

	modelsPath        = "/models"
	headerAccountID   = "chatgpt-account-id"
	headerOpenAIBeta  = "OpenAI-Beta"
	openAIBetaValue   = "responses=experimental"
	headerOriginator  = "originator"
	originatorValue   = "codex_cli_rs"
	fetchSingleflight = "models"
)

// Catalog is the read side the router and /healthz depend on: the current model
// list, and how old the held snapshot is.
type Catalog interface {
	// Models returns the current catalog, fetching or revalidating as the
	// staleness rules require. cred signs the request; it is never stored.
	Models(ctx context.Context, cred auth.Credential) ([]schema.Model, error)
	// Age reports how long ago the held snapshot was fetched, or 0 if none has
	// been obtained yet.
	Age() time.Duration
}

// Options configures a Client. Only zero-valued fields are defaulted; a caller
// that wants the real endpoint can leave BaseURL empty.
type Options struct {
	// BaseURL is the Codex backend root. Defaults to DefaultBaseURL.
	BaseURL string
	// CachePath is utraque's own on-disk cache file. Empty disables disk
	// caching (memory only). It MUST NOT be the Codex CLI's models_cache.json:
	// utraque never overwrites the CLI's cache.
	CachePath string
	// TTL is the fresh window. Defaults to DefaultTTL. A negative value is
	// treated as the default.
	TTL time.Duration
	// ClientVersion is recorded in the on-disk cache for interop/debugging.
	ClientVersion string
	// HTTPClient performs the fetch. Defaults to a client with a modest
	// timeout. Tests inject one aimed at a fake server.
	HTTPClient *http.Client
	// Now supplies the current time. Defaults to time.Now. Tests override it.
	Now func() time.Time
	// Logger receives redacted operational logs. Defaults to slog.Default.
	Logger *slog.Logger
}

// Client is a caching catalog fetcher. It is safe for concurrent use.
type Client struct {
	baseURL       string
	cachePath     string
	ttl           time.Duration
	clientVersion string
	http          *http.Client
	now           func() time.Time
	log           *slog.Logger

	// group collapses concurrent fetches (foreground + background) into one.
	group singleflight.Group
	// refreshing guards against launching more than one background
	// revalidation at a time.
	refreshing atomic.Bool

	mu        sync.RWMutex
	st        state
	diskTried bool
}

// state is the in-memory snapshot.
type state struct {
	models    []schema.Model
	etag      string
	fetchedAt time.Time
	loaded    bool
}

const defaultHTTPTimeout = 30 * time.Second

var _ Catalog = (*Client)(nil)

// New builds a Client. No network or disk access happens here.
func New(opts Options) *Client {
	c := &Client{
		baseURL:       opts.BaseURL,
		cachePath:     opts.CachePath,
		ttl:           opts.TTL,
		clientVersion: opts.ClientVersion,
		http:          opts.HTTPClient,
		now:           opts.Now,
		log:           opts.Logger,
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
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
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c
}

// Models implements Catalog.
func (c *Client) Models(ctx context.Context, cred auth.Credential) ([]schema.Model, error) {
	c.ensureDiskLoaded()

	c.mu.RLock()
	st := c.st
	c.mu.RUnlock()

	if st.loaded {
		if c.now().Sub(st.fetchedAt) < c.ttl {
			return cloneModels(st.models), nil // fresh
		}
		// Stale: serve immediately, revalidate in the background. The request
		// ctx must not gate the background fetch — it will be cancelled the
		// moment this call returns.
		c.revalidateAsync(cred)
		return cloneModels(st.models), nil
	}

	// Cold: nothing to serve, so fetch synchronously.
	ns, err := c.fetchShared(ctx, cred)
	if err != nil {
		return nil, err
	}
	return cloneModels(ns.models), nil
}

// Age implements Catalog.
func (c *Client) Age() time.Duration {
	c.ensureDiskLoaded()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.st.loaded {
		return 0
	}
	return c.now().Sub(c.st.fetchedAt)
}

// Snapshot reports the currently held catalog without any network fetch or
// revalidation: how many models are held and how old the held snapshot is.
// loaded is false when nothing has been obtained yet (neither from the disk
// cache nor a fetch). It is what /healthz reads so a health poll never touches
// the network — at most it triggers the one-time disk-cache load Age also does.
func (c *Client) Snapshot() (models int, age time.Duration, loaded bool) {
	c.ensureDiskLoaded()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.st.loaded {
		return 0, 0, false
	}
	return len(c.st.models), c.now().Sub(c.st.fetchedAt), true
}

// revalidateAsync launches at most one background fetch. Its result is stored
// on success and simply logged (never fatal) on failure — the caller already
// has a stale-but-usable snapshot.
func (c *Client) revalidateAsync(cred auth.Credential) {
	if !c.refreshing.CompareAndSwap(false, true) {
		return // one already in flight
	}
	go func() {
		defer c.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), backgroundRefreshTimeout)
		defer cancel()
		if _, err := c.fetchShared(ctx, cred); err != nil {
			c.log.Warn("codex catalog background revalidation failed",
				slog.String("err", err.Error()))
		}
	}()
}

// fetchShared runs fetch under singleflight so a foreground cold fetch and a
// background revalidation collapse to one HTTP round-trip.
func (c *Client) fetchShared(ctx context.Context, cred auth.Credential) (state, error) {
	v, err, _ := c.group.Do(fetchSingleflight, func() (any, error) {
		return c.fetch(ctx, cred)
	})
	if err != nil {
		return state{}, err
	}
	return v.(state), nil
}

// fetch performs one conditional GET and commits the result. On 304 it reuses
// the held models; on 200 it replaces them.
func (c *Client) fetch(ctx context.Context, cred auth.Credential) (state, error) {
	c.mu.RLock()
	prevEtag := c.st.etag
	prevModels := c.st.models
	hadPrev := c.st.loaded
	c.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+modelsPath, nil)
	if err != nil {
		return state{}, apierr.Wrap(err, apierr.TypeAPI, "codex catalog: build request")
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set(headerAccountID, cred.AccountID)
	req.Header.Set(headerOpenAIBeta, openAIBetaValue)
	req.Header.Set(headerOriginator, originatorValue)
	req.Header.Set("Accept", "application/json")
	if prevEtag != "" {
		req.Header.Set("If-None-Match", prevEtag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return state{}, apierr.Wrap(err, apierr.TypeAPI, "codex catalog: request failed")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if !hadPrev {
			// 304 with nothing cached is a protocol surprise (we would only
			// send If-None-Match with a held etag). Treat as unusable.
			return state{}, apierr.API("codex catalog returned 304 with no cached catalog to reuse")
		}
		ns := state{models: prevModels, etag: prevEtag, fetchedAt: c.now(), loaded: true}
		c.commit(ns)
		c.log.Debug("codex catalog not modified", slog.Int("models", len(prevModels)))
		return ns, nil

	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return state{}, apierr.Wrap(err, apierr.TypeAPI, "codex catalog: read body")
		}
		var parsed schema.ModelsResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return state{}, apierr.Wrap(err, apierr.TypeAPI, "codex catalog: decode body")
		}
		// Use the response's own ETag, or none. Do NOT fall back to the previous
		// ETag: this 200 body may differ from what that validator described, and
		// pairing new models with a stale validator would let a later 304 for it
		// wrongly mark the new body fresh. No ETag simply means the next fetch is
		// unconditional.
		etag := resp.Header.Get("ETag")
		ns := state{models: parsed.Models, etag: etag, fetchedAt: c.now(), loaded: true}
		c.commit(ns)
		c.log.Info("fetched codex catalog",
			slog.Int("models", len(parsed.Models)),
			obs.HashAttr("account", cred.AccountID))
		return ns, nil

	case http.StatusUnauthorized:
		// Signal to the caller that the credential needs refreshing; carry no
		// token material, only the status.
		return state{}, apierr.Authentication("codex catalog rejected the credential (HTTP 401); the access token may need refreshing")

	default:
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			// Reached here because CheckRedirect refused to follow it. Say so,
			// rather than letting it read as an ordinary upstream failure.
			return state{}, apierr.WithStatus(http.StatusBadGateway, apierr.TypeAPI,
				"codex catalog returned an unexpected redirect (HTTP %d to %q); redirects are never followed",
				resp.StatusCode, resp.Header.Get("Location"))
		}
		kind := apierr.TypeForStatus(resp.StatusCode)
		return state{}, apierr.WithStatus(resp.StatusCode, kind,
			"codex catalog request failed (HTTP %d)", resp.StatusCode)
	}
}

// commit stores the new snapshot and best-effort writes the disk cache.
func (c *Client) commit(ns state) {
	c.mu.Lock()
	c.st = ns
	c.mu.Unlock()
	c.writeDisk(ns)
}

// ensureDiskLoaded loads the on-disk cache once, if present, so a cold start has
// something to serve. A missing/unreadable/corrupt file is ignored — it just
// means the first Models call fetches synchronously.
func (c *Client) ensureDiskLoaded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.diskTried || c.st.loaded {
		c.diskTried = true
		return
	}
	c.diskTried = true
	if c.cachePath == "" {
		return
	}
	b, err := os.ReadFile(c.cachePath)
	if err != nil {
		return
	}
	var cache schema.Cache
	if err := json.Unmarshal(b, &cache); err != nil {
		return
	}
	if len(cache.Models) == 0 && cache.FetchedAt.IsZero() {
		return // nothing useful
	}
	// A cache written by a different client version may describe a different
	// protocol/catalog shape; ignore it and re-fetch rather than serve a list a
	// client upgrade may have invalidated. (Only gate when both versions are
	// known, so a version-less test cache still loads.)
	if cache.ClientVersion != "" && c.clientVersion != "" && cache.ClientVersion != c.clientVersion {
		c.log.Debug("ignoring codex catalog disk cache from a different client version",
			slog.String("cache_client_version", cache.ClientVersion),
			slog.String("client_version", c.clientVersion))
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
	c.st = state{
		models:    cache.Models,
		etag:      cache.ETag,
		fetchedAt: cache.FetchedAt,
		loaded:    true,
	}
	c.log.Debug("loaded codex catalog from disk cache",
		slog.Int("models", len(cache.Models)),
		slog.Time("fetched_at", cache.FetchedAt))
}

// writeDisk atomically writes the snapshot to CachePath, if configured. Failure
// is logged and swallowed: the disk cache is an optimisation, not a
// requirement, and a write error must never fail a live request.
func (c *Client) writeDisk(ns state) {
	if c.cachePath == "" {
		return
	}
	cache := schema.Cache{
		ClientVersion: c.clientVersion,
		ETag:          ns.etag,
		FetchedAt:     ns.fetchedAt,
		Models:        ns.models,
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		c.log.Warn("codex catalog: encode disk cache", slog.String("err", err.Error()))
		return
	}
	data = append(data, '\n')
	dir := filepath.Dir(c.cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.log.Warn("codex catalog: create cache dir", slog.String("err", err.Error()))
		return
	}
	tmp, err := os.CreateTemp(dir, ".models_cache-*.json.tmp")
	if err != nil {
		c.log.Warn("codex catalog: create temp cache", slog.String("err", err.Error()))
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		c.log.Warn("codex catalog: write temp cache", slog.String("err", err.Error()))
		return
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		c.log.Warn("codex catalog: sync temp cache", slog.String("err", err.Error()))
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		c.log.Warn("codex catalog: close temp cache", slog.String("err", err.Error()))
		return
	}
	if err := os.Rename(tmpName, c.cachePath); err != nil {
		_ = os.Remove(tmpName)
		c.log.Warn("codex catalog: rename cache into place", slog.String("err", err.Error()))
		return
	}
}

// cloneModels returns a deep copy of the model slice so a caller can hold,
// reorder, or mutate it without racing the client's own snapshot replacement.
// Each Model's SupportedReasoningLevels slice is copied too — a shallow copy
// would leave callers sharing (and able to mutate) the cached backing array,
// racing the goroutines that read the held snapshot.
func cloneModels(in []schema.Model) []schema.Model {
	if len(in) == 0 {
		return nil
	}
	out := make([]schema.Model, len(in))
	copy(out, in)
	for i := range out {
		if len(out[i].SupportedReasoningLevels) > 0 {
			lv := make([]schema.ReasoningLevel, len(out[i].SupportedReasoningLevels))
			copy(lv, out[i].SupportedReasoningLevels)
			out[i].SupportedReasoningLevels = lv
		}
	}
	return out
}

// currentModels returns a catalog current as of now: it serves the in-memory
// snapshot only while it is within the fresh TTL, and otherwise BLOCKS on a
// synchronous fetch. Unlike Models it never returns a stale snapshot with a
// detached background refresh — a caller about to install the result into the
// live router (see RefreshRegistry) must not publish a stale list and report
// success. On fetch failure it returns the error and no models.
func (c *Client) currentModels(ctx context.Context, cred auth.Credential) ([]schema.Model, error) {
	c.ensureDiskLoaded()
	c.mu.RLock()
	st := c.st
	c.mu.RUnlock()
	if st.loaded && c.now().Sub(st.fetchedAt) < c.ttl {
		return cloneModels(st.models), nil
	}
	ns, err := c.fetchShared(ctx, cred)
	if err != nil {
		return nil, err
	}
	return cloneModels(ns.models), nil
}
