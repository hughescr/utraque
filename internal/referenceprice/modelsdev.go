// Package referenceprice reads public API reference prices without provider
// credentials. It is deliberately separate from quota and account readers.
package referenceprice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	DefaultURL              = "https://models.dev/api.json"
	DefaultTTL              = 5 * time.Minute
	defaultTimeout          = 5 * time.Second
	defaultFailureBackoff   = time.Minute
	defaultMaxResponseBytes = int64(8 << 20)

	SourceModelsDev = "models.dev"
	USDPerMillion   = "usd_per_million_tokens"
)

type Options struct {
	URL              string
	HTTPClient       *http.Client
	Timeout          time.Duration
	TTL              time.Duration
	FailureBackoff   time.Duration
	MaxResponseBytes int64
	Now              func() time.Time
}

// ModelPrice is one exact model id from its author provider's models.dev
// catalog. Provider and HasHigherTier are internal assembly metadata. Whether
// a model is a current routing candidate is not a price fact: the provider
// report attaches that after joining (providerreport.PriceRow).
type ModelPrice struct {
	Model      string   `json:"model"`
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`

	Provider      string `json:"-"`
	HasHigherTier bool   `json:"-"`
}

type Snapshot struct {
	Source      string       `json:"source"`
	ObservedAt  time.Time    `json:"observed_at"`
	Stale       bool         `json:"stale,omitempty"`
	Unit        string       `json:"unit"`
	Models      []ModelPrice `json:"models"`
	Assumptions []string     `json:"assumptions,omitempty"`
}

// ErrorCode classifies a catalog read failure.
type ErrorCode string

const (
	CodeConfiguration ErrorCode = "configuration_error"
	CodeUnavailable   ErrorCode = "unavailable"
	CodeTimeout       ErrorCode = "timeout"
	CodeTooLarge      ErrorCode = "response_too_large"
	CodeInvalidData   ErrorCode = "invalid_response"
)

type Error struct {
	Code      ErrorCode
	Retryable bool
}

func (e *Error) Error() string {
	if e == nil {
		return "reference pricing unavailable"
	}
	return fmt.Sprintf("reference pricing: %s", e.Code)
}

type cachedSnapshot struct {
	snapshot   Snapshot
	etag       string
	expiresAt  time.Time
	retryAfter time.Time
}

type Client struct {
	endpoint       string
	client         *http.Client
	ttl            time.Duration
	failureBackoff time.Duration
	maxBody        int64
	now            func() time.Time

	mu    sync.Mutex
	cache cachedSnapshot
	group singleflight.Group
}

func NewModelsDevClient(opts Options) (*Client, error) {
	if opts.URL == "" {
		opts.URL = DefaultURL
	}
	u, err := url.Parse(strings.TrimSpace(opts.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, &Error{Code: CodeConfiguration}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.FailureBackoff <= 0 {
		opts.FailureBackoff = defaultFailureBackoff
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = defaultMaxResponseBytes
	}
	if opts.MaxResponseBytes > 64<<20 {
		return nil, &Error{Code: CodeConfiguration}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	client := *opts.HTTPClient
	client.Timeout = opts.Timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{endpoint: u.String(), client: &client, ttl: opts.TTL,
		failureBackoff: opts.FailureBackoff, maxBody: opts.MaxResponseBytes, now: opts.Now}, nil
}

type readResult struct {
	snapshot Snapshot
	err      error
}

func (c *Client) Read(ctx context.Context) (Snapshot, error) {
	if c == nil {
		return Snapshot{}, &Error{Code: CodeConfiguration}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, classifyContext(err)
	}
	if snapshot, err, ok := c.cached(c.now().UTC()); ok {
		return snapshot, err
	}
	ch := c.group.DoChan("models.dev", func() (any, error) {
		if snapshot, err, ok := c.cached(c.now().UTC()); ok {
			return readResult{snapshot: snapshot, err: err}, nil
		}
		return c.refresh(), nil
	})
	select {
	case <-ctx.Done():
		return Snapshot{}, classifyContext(ctx.Err())
	case result := <-ch:
		if result.Err != nil {
			return Snapshot{}, &Error{Code: CodeUnavailable, Retryable: true}
		}
		value := result.Val.(readResult)
		return value.snapshot, value.err
	}
}

func (c *Client) cached(now time.Time) (Snapshot, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache.snapshot.Models) > 0 && now.Before(c.cache.expiresAt) {
		return cloneSnapshot(c.cache.snapshot), nil, true
	}
	if now.Before(c.cache.retryAfter) {
		err := &Error{Code: CodeUnavailable, Retryable: true}
		if len(c.cache.snapshot.Models) == 0 {
			return Snapshot{}, err, true
		}
		snapshot := cloneSnapshot(c.cache.snapshot)
		snapshot.Stale = true
		return snapshot, err, true
	}
	return Snapshot{}, nil, false
}

func (c *Client) refresh() readResult {
	c.mu.Lock()
	etag := c.cache.etag
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return c.failed(&Error{Code: CodeConfiguration})
	}
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return c.failed(&Error{Code: CodeTimeout, Retryable: true})
		}
		return c.failed(&Error{Code: CodeUnavailable, Retryable: true})
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.maxBody))
		return c.notModified()
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.maxBody))
		return c.failed(&Error{Code: CodeUnavailable, Retryable: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests})
	}
	if resp.ContentLength > c.maxBody {
		return c.failed(&Error{Code: CodeTooLarge})
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return c.failed(&Error{Code: CodeUnavailable, Retryable: true})
	}
	if int64(len(body)) > c.maxBody {
		return c.failed(&Error{Code: CodeTooLarge})
	}
	models, err := parseModelsDev(body)
	if err != nil {
		return c.failed(&Error{Code: CodeInvalidData})
	}
	now := c.now().UTC()
	snapshot := Snapshot{Source: SourceModelsDev, ObservedAt: now, Unit: USDPerMillion, Models: models}
	c.mu.Lock()
	c.cache = cachedSnapshot{snapshot: cloneSnapshot(snapshot), etag: safeETag(resp.Header.Get("ETag")), expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return readResult{snapshot: snapshot}
}

func (c *Client) notModified() readResult {
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache.snapshot.Models) == 0 {
		return c.failedLocked(&Error{Code: CodeInvalidData}, now)
	}
	c.cache.snapshot.ObservedAt = now
	c.cache.snapshot.Stale = false
	c.cache.expiresAt = now.Add(c.ttl)
	c.cache.retryAfter = time.Time{}
	return readResult{snapshot: cloneSnapshot(c.cache.snapshot)}
}

func (c *Client) failed(err error) readResult {
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failedLocked(err, now)
}

func (c *Client) failedLocked(err error, now time.Time) readResult {
	c.cache.retryAfter = now.Add(c.failureBackoff)
	if len(c.cache.snapshot.Models) == 0 {
		return readResult{err: err}
	}
	snapshot := cloneSnapshot(c.cache.snapshot)
	snapshot.Stale = true
	return readResult{snapshot: snapshot, err: err}
}

func classifyContext(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Retryable: true}
	}
	return &Error{Code: CodeUnavailable, Retryable: true}
}

type rawProvider struct {
	ID     string              `json:"id"`
	Models map[string]rawModel `json:"models"`
}

type rawModel struct {
	ID   string   `json:"id"`
	Cost *rawCost `json:"cost"`
}

type rawCost struct {
	Input      *float64          `json:"input"`
	Output     *float64          `json:"output"`
	CacheRead  *float64          `json:"cache_read"`
	CacheWrite *float64          `json:"cache_write"`
	Tiers      []json.RawMessage `json:"tiers"`
}

func parseModelsDev(data []byte) ([]ModelPrice, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	providers := []struct {
		catalog string
		report  string
	}{{"anthropic", "anthropic"}, {"openai", "codex"}, {"deepseek", "deepseek"}}
	models := make([]ModelPrice, 0, 64)
	for _, provider := range providers {
		raw, ok := root[provider.catalog]
		if !ok {
			return nil, fmt.Errorf("missing provider %s", provider.catalog)
		}
		var catalog rawProvider
		if err := json.Unmarshal(raw, &catalog); err != nil || catalog.ID != provider.catalog || len(catalog.Models) == 0 {
			return nil, fmt.Errorf("invalid provider %s", provider.catalog)
		}
		providerPrices := 0
		for key, model := range catalog.Models {
			id := model.ID
			if id == "" {
				id = key
			}
			if !validID(id) || model.Cost == nil || !validPrice(model.Cost.Input) || !validPrice(model.Cost.Output) {
				continue
			}
			if !validOptionalPrice(model.Cost.CacheRead) || !validOptionalPrice(model.Cost.CacheWrite) {
				continue
			}
			models = append(models, ModelPrice{Provider: provider.report, Model: id,
				Input: *model.Cost.Input, Output: *model.Cost.Output,
				CacheRead: cloneFloat(model.Cost.CacheRead), CacheWrite: cloneFloat(model.Cost.CacheWrite),
				HasHigherTier: len(model.Cost.Tiers) > 0})
			providerPrices++
		}
		if providerPrices == 0 {
			return nil, fmt.Errorf("provider %s has no valid prices", provider.catalog)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("no prices")
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider == models[j].Provider {
			return models[i].Model < models[j].Model
		}
		return models[i].Provider < models[j].Provider
	})
	return models, nil
}

func validID(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func validPrice(value *float64) bool {
	return value != nil && *value >= 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func validOptionalPrice(value *float64) bool { return value == nil || validPrice(value) }

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneSnapshot(in Snapshot) Snapshot {
	out := in
	out.Models = append([]ModelPrice(nil), in.Models...)
	for i := range out.Models {
		out.Models[i].CacheRead = cloneFloat(in.Models[i].CacheRead)
		out.Models[i].CacheWrite = cloneFloat(in.Models[i].CacheWrite)
	}
	out.Assumptions = append([]string(nil), in.Assumptions...)
	return out
}

func safeETag(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}
