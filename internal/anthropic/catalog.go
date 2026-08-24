package anthropic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/transport"
)

// Defaults for the upstream model-catalog client.
const (
	// CatalogPath is Anthropic's model-list endpoint, relative to the base URL.
	CatalogPath = "/v1/models"

	// DefaultCatalogLimit is the page size asked for. Anthropic caps the
	// parameter at 1000, which is also what Claude Code itself requests.
	DefaultCatalogLimit = 1000

	// DefaultCatalogTTL is how long a successful catalog is reused without
	// asking upstream again. Model lists change on the order of weeks; a picker
	// open must not become an upstream round-trip every time.
	DefaultCatalogTTL = 5 * time.Minute

	// DefaultCatalogNegativeTTL is how long a *failure* is remembered. The plan
	// calls for roughly a minute: long enough that a proxy whose credential
	// cannot read this endpoint stops paying the latency on every picker open,
	// short enough that a re-login recovers quickly.
	DefaultCatalogNegativeTTL = 60 * time.Second

	// DefaultAnthropicVersion is sent when the caller supplied no
	// anthropic-version of its own. It matches what Claude Code sends.
	DefaultAnthropicVersion = "2023-06-01"

	// maxCatalogBody caps how much of a catalog response is read.
	maxCatalogBody = 4 << 20 // 4 MiB
)

// ErrNoCredential means the inbound request carried nothing that could
// authenticate an upstream catalog read. It is NOT an upstream failure and is
// deliberately not negative-cached: the very next request may well carry one.
//
// This is the common case in practice. Claude Code only attempts gateway model
// discovery when ANTHROPIC_AUTH_TOKEN or an API key is set; a subscription
// OAuth session sends neither, so the merged catalog is normally built from the
// static list rather than from upstream.
var ErrNoCredential = errors.New("utraque/anthropic: no client credential on the request")

// ErrRedirected means upstream answered with a 3xx. utraque never follows one —
// doing so would re-send the caller's bearer token to whatever host upstream
// named — so a redirect is a hard failure here, exactly as it is for the client.
var ErrRedirected = errors.New("utraque/anthropic: upstream redirected the catalog request")

// Credential is the caller's own Anthropic credential, lifted from the inbound
// request. utraque holds no Anthropic secret of its own: whatever authenticates
// the catalog read is what the client sent us this request, and it is used and
// discarded.
type Credential struct {
	// Authorization is the raw Authorization header value ("Bearer ...").
	Authorization string
	// APIKey is the raw x-api-key header value.
	APIKey string
	// Beta holds the anthropic-beta values, each preserved as its own entry so
	// they are re-sent as separate header lines rather than re-joined.
	Beta []string
	// Version is the anthropic-version header value, if the caller sent one.
	Version string
}

// CredentialFromRequest lifts the caller's credential headers off r. It copies;
// nothing references r afterwards.
func CredentialFromRequest(r *http.Request) Credential {
	if r == nil {
		return Credential{}
	}
	var c Credential
	c.Authorization = strings.TrimSpace(r.Header.Get("Authorization"))
	c.APIKey = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	c.Version = strings.TrimSpace(r.Header.Get("Anthropic-Version"))
	for _, v := range r.Header.Values("Anthropic-Beta") {
		if v = strings.TrimSpace(v); v != "" {
			c.Beta = append(c.Beta, v)
		}
	}
	return c
}

// Present reports whether the credential can authenticate an upstream read.
func (c Credential) Present() bool { return c.Authorization != "" || c.APIKey != "" }

// cacheKey is a stable, non-reversible fingerprint of the material that
// authenticates the upstream read. The catalog cache is keyed on it so a list
// fetched with one caller's credential is never served to a different one:
// utraque holds no Anthropic secret of its own, so each caller's answer is
// theirs alone. Harmless on a single-user loopback, wrong the moment the local
// token is shared.
func (c Credential) cacheKey() string {
	sum := sha256.Sum256([]byte(c.Authorization + "\x00" + c.APIKey))
	return hex.EncodeToString(sum[:])
}

// apply writes the credential onto an outbound request.
func (c Credential) apply(req *http.Request) {
	switch {
	case c.Authorization != "":
		req.Header.Set("Authorization", c.Authorization)
	case c.APIKey != "":
		req.Header.Set("X-Api-Key", c.APIKey)
	}
	version := c.Version
	if version == "" {
		version = DefaultAnthropicVersion
	}
	req.Header.Set("Anthropic-Version", version)
	// Repeated anthropic-beta values stay separate header lines, never re-joined
	// — the same rule the passthrough leg follows.
	for _, v := range c.Beta {
		req.Header.Add("Anthropic-Beta", v)
	}
	req.Header.Set("Accept", "application/json")
}

// CatalogModel is one row of Anthropic's model list. Only ID and DisplayName
// are consumed by the client's picker; CreatedAt and Type are carried so the
// merged catalog can echo the real API's shape.
type CatalogModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Type        string `json:"type,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// catalogPage is the GET /v1/models body.
type catalogPage struct {
	Data []CatalogModel `json:"data"`
}

// Catalog is the read side internal/discovery depends on.
type Catalog interface {
	// Models returns Anthropic's own model list, read with the caller's
	// credential. It returns ErrNoCredential when cred cannot authenticate.
	Models(ctx context.Context, cred Credential) ([]CatalogModel, error)
}

// CatalogOption configures a CatalogClient.
type CatalogOption func(*CatalogClient)

// WithCatalogLogger sets the logger. The default discards.
func WithCatalogLogger(log *slog.Logger) CatalogOption {
	return func(c *CatalogClient) {
		if log != nil {
			c.log = log
		}
	}
}

// WithCatalogTTL sets how long a successful catalog is reused. Zero or negative
// restores DefaultCatalogTTL.
func WithCatalogTTL(d time.Duration) CatalogOption {
	return func(c *CatalogClient) {
		if d <= 0 {
			d = DefaultCatalogTTL
		}
		c.ttl = d
	}
}

// WithCatalogNegativeTTL sets how long a failure is remembered. Zero or negative
// restores DefaultCatalogNegativeTTL.
func WithCatalogNegativeTTL(d time.Duration) CatalogOption {
	return func(c *CatalogClient) {
		if d <= 0 {
			d = DefaultCatalogNegativeTTL
		}
		c.negTTL = d
	}
}

// WithCatalogNow injects the clock. Tests use it to age the caches without
// sleeping.
func WithCatalogNow(now func() time.Time) CatalogOption {
	return func(c *CatalogClient) {
		if now != nil {
			c.now = now
		}
	}
}

// CatalogClient reads Anthropic's own GET /v1/models with the caller's
// credential, caching both success and failure.
//
// Whether a subscription OAuth credential is actually accepted on this endpoint
// is unconfirmed, which is the whole reason the failure path is a first-class,
// cheap, cached outcome rather than an exception: discovery must be able to ask,
// be told no, and fall back to the static list without paying for the question
// again on the next picker open.
//
// It is safe for concurrent use.
type CatalogClient struct {
	base   *url.URL
	tr     transport.Transport
	log    *slog.Logger
	ttl    time.Duration
	negTTL time.Duration
	now    func() time.Time

	mu sync.Mutex
	// models/goodUntil hold the last success; err/badUntil hold the last
	// failure. At most one of the two windows is meaningful at a time: a
	// success clears the negative cache and vice versa.
	//
	// credKey fingerprints the credential both windows belong to. A read by a
	// different caller misses and fetches with its own credential rather than
	// being served someone else's answer. One slot, not a per-caller map: a map
	// would grow with every distinct credential, and utraque's normal shape is a
	// single loopback caller, so a miss on a credential change costs one fetch.
	models    []CatalogModel
	goodUntil time.Time
	err       error
	badUntil  time.Time
	credKey   string
}

var _ Catalog = (*CatalogClient)(nil)

// NewCatalog builds a catalog client against baseURL (e.g.
// "https://api.anthropic.com"). Userinfo, query and fragment are stripped — a
// credential must never ride in a configured URL — and a trailing slash is
// trimmed so path joining stays exact.
func NewCatalog(baseURL string, tr transport.Transport, opts ...CatalogOption) (*CatalogClient, error) {
	if tr == nil {
		return nil, errors.New("utraque/anthropic: nil transport")
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("utraque/anthropic: parse catalog base url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("utraque/anthropic: catalog base url must be http or https, got %q", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("utraque/anthropic: catalog base url has no host: %q", baseURL)
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""

	c := &CatalogClient{
		base:   u,
		tr:     tr,
		log:    slog.New(slog.DiscardHandler),
		ttl:    DefaultCatalogTTL,
		negTTL: DefaultCatalogNegativeTTL,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// BaseURL returns the normalized upstream base URL.
func (c *CatalogClient) BaseURL() string { return c.base.String() }

// Models implements Catalog.
func (c *CatalogClient) Models(ctx context.Context, cred Credential) ([]CatalogModel, error) {
	if !cred.Present() {
		return nil, ErrNoCredential
	}
	key := cred.cacheKey()
	if models, err, hit := c.cached(key); hit {
		return models, err
	}

	models, err := c.fetch(ctx, cred)
	if err != nil {
		c.rememberFailure(key, err)
		return nil, err
	}
	c.rememberSuccess(key, models)
	return cloneCatalog(models), nil
}

// cached returns a live cache entry, positive or negative, for the credential
// fingerprint key. hit=false means the caller must fetch — including when the
// held entry belongs to a different credential.
func (c *CatalogClient) cached(key string) (models []CatalogModel, err error, hit bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credKey != key {
		return nil, nil, false
	}
	if now.Before(c.badUntil) && c.err != nil {
		return nil, c.err, true
	}
	if now.Before(c.goodUntil) && c.models != nil {
		return cloneCatalog(c.models), nil, true
	}
	return nil, nil, false
}

func (c *CatalogClient) rememberSuccess(key string, models []CatalogModel) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credKey = key
	c.models = cloneCatalog(models)
	c.goodUntil = now.Add(c.ttl)
	c.err, c.badUntil = nil, time.Time{}
}

func (c *CatalogClient) rememberFailure(key string, err error) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credKey = key
	c.err = err
	c.badUntil = now.Add(c.negTTL)
	// A stale success is deliberately dropped rather than served past its TTL:
	// this list decides what a user is offered, and offering models the account
	// may no longer have is worse than falling back to the static list.
	c.models, c.goodUntil = nil, time.Time{}
	c.log.Debug("anthropic catalog read failed; negative-caching",
		slog.String("err", err.Error()), slog.Duration("for", c.negTTL))
}

// NegativeCachedUntil reports when the current negative-cache window ends, and
// whether one is in force. Exposed for /healthz and tests.
func (c *CatalogClient) NegativeCachedUntil() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil || !c.now().Before(c.badUntil) {
		return time.Time{}, false
	}
	return c.badUntil, true
}

// ResetCache clears both caches. Exposed for tests and for a future explicit
// "re-check upstream" control.
func (c *CatalogClient) ResetCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models, c.goodUntil = nil, time.Time{}
	c.err, c.badUntil = nil, time.Time{}
	c.credKey = ""
}

func (c *CatalogClient) fetch(ctx context.Context, cred Credential) ([]CatalogModel, error) {
	target := *c.base
	target.Path = c.base.Path + CatalogPath
	target.RawQuery = url.Values{"limit": {fmt.Sprint(DefaultCatalogLimit)}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.TypeAPI, "anthropic catalog: build request")
	}
	cred.apply(req)

	resp, err := c.tr.Client().Do(req)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.TypeAPI, "anthropic catalog: request failed")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCatalogBody))
		_ = resp.Body.Close()
	}()

	// The transport never follows redirects, so a 3xx arrives here intact.
	// Treat it as a failure rather than chasing it with the caller's token.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrRedirected, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		kind := apierr.TypeForStatus(resp.StatusCode)
		return nil, apierr.WithStatus(resp.StatusCode, kind,
			"anthropic catalog request failed (HTTP %d)", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBody))
	if err != nil {
		return nil, apierr.Wrap(err, apierr.TypeAPI, "anthropic catalog: read body")
	}
	var page catalogPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, apierr.Wrap(err, apierr.TypeAPI, "anthropic catalog: decode body")
	}

	out := make([]CatalogModel, 0, len(page.Data))
	for _, m := range page.Data {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		out = append(out, m)
	}
	c.log.Debug("read anthropic catalog", slog.Int("models", len(out)))
	return out, nil
}

func cloneCatalog(in []CatalogModel) []CatalogModel {
	if in == nil {
		return nil
	}
	out := make([]CatalogModel, len(in))
	copy(out, in)
	return out
}
