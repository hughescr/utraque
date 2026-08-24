package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/catalog"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/router"
)

// fakeCred is the credential every test signs with. It is never real: the
// tests only ever talk to an httptest server, never chatgpt.com, and never read
// ~/.codex.
func fakeCred() auth.Credential {
	return auth.Credential{
		AccessToken: "fake-access-token",
		AccountID:   "fake-account-id",
		Exp:         time.Now().Add(time.Hour),
	}
}

// clock is a mockable time source safe for concurrent use (the background
// revalidation goroutine reads it).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeCatalog is a configurable fake of GET {base}/models. It records requests,
// asserts the required headers, and honours If-None-Match against its current
// etag.
type fakeCatalog struct {
	t *testing.T

	mu            sync.Mutex
	models        []schema.Model
	etag          string
	status304     bool // when true and If-None-Match matches, answer 304
	calls         int
	lastINM       string
	gotAuth       string
	gotAccount    string
	gotBeta       string
	gotOrigin     string
	requestSignal chan struct{}
}

func newFakeCatalog(t *testing.T, models []schema.Model, etag string) *fakeCatalog {
	return &fakeCatalog{t: t, models: models, etag: etag, requestSignal: make(chan struct{}, 16)}
}

func (f *fakeCatalog) set(models []schema.Model, etag string, status304 bool) {
	f.mu.Lock()
	f.models, f.etag, f.status304 = models, etag, status304
	f.mu.Unlock()
}

func (f *fakeCatalog) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCatalog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.lastINM = r.Header.Get("If-None-Match")
	f.gotAuth = r.Header.Get("Authorization")
	f.gotAccount = r.Header.Get("chatgpt-account-id")
	f.gotBeta = r.Header.Get("OpenAI-Beta")
	f.gotOrigin = r.Header.Get("originator")
	models, etag, want304 := f.models, f.etag, f.status304
	inm := f.lastINM
	f.mu.Unlock()

	defer func() {
		select {
		case f.requestSignal <- struct{}{}:
		default:
		}
	}()

	if r.Method != http.MethodGet || r.URL.Path != "/models" {
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "bad path", http.StatusNotFound)
		return
	}

	if want304 && inm != "" && inm == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(schema.ModelsResponse{Models: models})
}

// eventually polls cond until it holds or the deadline elapses.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func solModel() schema.Model {
	return schema.Model{Slug: "gpt-5.6-sol", Visibility: "list", ContextWindow: 400000,
		DefaultReasoningLevel: "low", Priority: 10}
}

func terraModel() schema.Model {
	return schema.Model{Slug: "gpt-5.6-terra", Visibility: "list", Priority: 8}
}

// --- fetch + header tests -------------------------------------------------

func TestFetchSendsRequiredHeadersAndParses(t *testing.T) {
	fake := newFakeCatalog(t, []schema.Model{solModel()}, `W/"v1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), Now: newClock().now})

	models, err := c.Models(context.Background(), fakeCred())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].Slug != "gpt-5.6-sol" {
		t.Fatalf("models = %+v", models)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.gotAuth != "Bearer fake-access-token" {
		t.Errorf("Authorization = %q", fake.gotAuth)
	}
	if fake.gotAccount != "fake-account-id" {
		t.Errorf("chatgpt-account-id = %q", fake.gotAccount)
	}
	if fake.gotBeta != "responses=experimental" {
		t.Errorf("OpenAI-Beta = %q", fake.gotBeta)
	}
	if fake.gotOrigin != "codex_cli_rs" {
		t.Errorf("originator = %q", fake.gotOrigin)
	}
}

func TestTTLFreshServesWithoutSecondCall(t *testing.T) {
	fake := newFakeCatalog(t, []schema.Model{solModel()}, `W/"v1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	clk := newClock()
	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), TTL: 300 * time.Second, Now: clk.now})

	if _, err := c.Models(context.Background(), fakeCred()); err != nil {
		t.Fatalf("first Models: %v", err)
	}
	// Well within TTL: no network call.
	clk.advance(10 * time.Second)
	if _, err := c.Models(context.Background(), fakeCred()); err != nil {
		t.Fatalf("second Models: %v", err)
	}
	if got := fake.callCount(); got != 1 {
		t.Errorf("call count = %d, want 1 (second Models should be served from the fresh cache)", got)
	}
}

func TestStaleWhileRevalidateServesStaleThenRefreshes(t *testing.T) {
	fake := newFakeCatalog(t, []schema.Model{solModel()}, `W/"v1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	clk := newClock()
	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), TTL: 60 * time.Second, Now: clk.now})

	first, err := c.Models(context.Background(), fakeCred())
	if err != nil {
		t.Fatalf("first Models: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first models len = %d, want 1", len(first))
	}

	// Go stale, and change what the server will return on revalidation.
	clk.advance(120 * time.Second)
	fake.set([]schema.Model{solModel(), terraModel()}, `W/"v2"`, false)

	// The stale read returns the OLD snapshot immediately.
	stale, err := c.Models(context.Background(), fakeCred())
	if err != nil {
		t.Fatalf("stale Models: %v", err)
	}
	if len(stale) != 1 {
		t.Errorf("stale read len = %d, want 1 (old snapshot served immediately)", len(stale))
	}

	// The background revalidation eventually installs the new snapshot.
	eventually(t, 2*time.Second, func() bool {
		m, err := c.Models(context.Background(), fakeCred())
		return err == nil && len(m) == 2
	})
	if got := fake.callCount(); got < 2 {
		t.Errorf("call count = %d, want >= 2 (initial + background revalidation)", got)
	}
}

func TestETag304ReusesModels(t *testing.T) {
	fake := newFakeCatalog(t, []schema.Model{solModel()}, `W/"v1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	clk := newClock()
	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), TTL: 60 * time.Second, Now: clk.now})

	if _, err := c.Models(context.Background(), fakeCred()); err != nil {
		t.Fatalf("first Models: %v", err)
	}

	// Go stale; server now answers 304 to a matching If-None-Match.
	clk.advance(120 * time.Second)
	fake.set([]schema.Model{solModel()}, `W/"v1"`, true)

	// Trigger background revalidation.
	if _, err := c.Models(context.Background(), fakeCred()); err != nil {
		t.Fatalf("stale Models: %v", err)
	}

	// After revalidation the snapshot is fresh again (Age below the TTL) and
	// the models were reused.
	eventually(t, 2*time.Second, func() bool { return c.Age() < 60*time.Second })

	fake.mu.Lock()
	inm := fake.lastINM
	fake.mu.Unlock()
	if inm != `W/"v1"` {
		t.Errorf("If-None-Match on revalidation = %q, want the held etag", inm)
	}
	m, err := c.Models(context.Background(), fakeCred())
	if err != nil || len(m) != 1 || m[0].Slug != "gpt-5.6-sol" {
		t.Errorf("after 304, models = %+v err=%v, want the reused snapshot", m, err)
	}
}

func TestUnauthorizedIsAuthenticationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), Now: newClock().now})
	_, err := c.Models(context.Background(), fakeCred())
	if err == nil {
		t.Fatal("want error on 401")
	}
	if got := err.Error(); !contains(got, "401") {
		t.Errorf("error = %q, want it to mention the 401 status", got)
	}
	// The credential must never appear in the surfaced error.
	if contains(err.Error(), "fake-access-token") || contains(err.Error(), "fake-account-id") {
		t.Errorf("error leaked credential material: %q", err.Error())
	}
}

// --- disk cache tests ------------------------------------------------------

func TestFetchWritesInteroperableDiskCache(t *testing.T) {
	fake := newFakeCatalog(t, []schema.Model{solModel()}, `W/"v1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "utraque-models_cache.json")
	clk := newClock()
	c := catalog.New(catalog.Options{
		BaseURL: srv.URL, HTTPClient: srv.Client(), Now: clk.now,
		CachePath: cachePath, ClientVersion: "utraque-test",
	})

	if _, err := c.Models(context.Background(), fakeCred()); err != nil {
		t.Fatalf("Models: %v", err)
	}

	b, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var cache schema.Cache
	if err := json.Unmarshal(b, &cache); err != nil {
		t.Fatalf("unmarshal cache: %v", err)
	}
	// Interop shape: the four Codex CLI keys are all present.
	if cache.ClientVersion != "utraque-test" {
		t.Errorf("ClientVersion = %q", cache.ClientVersion)
	}
	if cache.ETag != `W/"v1"` {
		t.Errorf("ETag = %q", cache.ETag)
	}
	if cache.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero")
	}
	if len(cache.Models) != 1 || cache.Models[0].Slug != "gpt-5.6-sol" {
		t.Errorf("Models = %+v", cache.Models)
	}

	// Safety: we wrote our OWN file under a temp dir, not the Codex CLI's.
	if filepath.Base(cachePath) == "models_cache.json" {
		t.Error("test must not target the Codex CLI's cache filename")
	}
}

func TestFreshDiskCacheServedWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "utraque-models_cache.json")
	clk := newClock()

	// Pre-write a cache dated exactly "now" so it is within TTL.
	cache := schema.Cache{
		ClientVersion: "x",
		ETag:          `W/"disk"`,
		FetchedAt:     clk.now(),
		Models:        []schema.Model{solModel()},
	}
	b, _ := json.Marshal(cache)
	if err := os.WriteFile(cachePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// An HTTP client that fails if used at all, proving the fresh disk cache
	// is served without any network call.
	c := catalog.New(catalog.Options{
		BaseURL:   "http://catalog.invalid",
		CachePath: cachePath,
		TTL:       300 * time.Second,
		Now:       clk.now,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network must not be used for a fresh disk cache")
		})},
	})

	models, err := c.Models(context.Background(), fakeCred())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].Slug != "gpt-5.6-sol" {
		t.Fatalf("models = %+v", models)
	}
	if c.Age() != 0 {
		t.Errorf("Age = %s, want 0 (fetched_at == now)", c.Age())
	}
}

// --- registry population tests --------------------------------------------

func TestPopulateRegistryTiersAndVisibility(t *testing.T) {
	reg := router.NewRegistry()
	models := []schema.Model{
		{Slug: "gpt-5.6-sol", Visibility: "list", Priority: 10},
		{Slug: "gpt-5.7-sol", Visibility: "list", Priority: 1}, // newer version wins bare despite lower priority
		{Slug: "gpt-5.5", Visibility: "list"},
		{Slug: "gpt-5.4-mini", Visibility: "list"},
		{Slug: "gpt-5.9-ghost", Visibility: "hide", Priority: 99}, // hidden -> not advertised at all
	}
	catalog.PopulateRegistry(reg, models)

	check := func(alias, want string) {
		t.Helper()
		if u, ok := reg.Resolve(alias); !ok || u != want {
			t.Errorf("Resolve(%q) = (%q, %v), want (%q, true)", alias, u, ok, want)
		}
	}
	// Version dominates the collision rule: the newer 5.7 wins the bare name.
	check("sol", "gpt-5.7-sol")
	check("sol-5.6", "gpt-5.6-sol")
	check("sol-5.7", "gpt-5.7-sol")
	check("gpt-5.6-sol", "gpt-5.6-sol") // raw always resolves
	check("5.5", "gpt-5.5")             // version-only bare alias
	check("5.4-mini", "gpt-5.4-mini")   // modifier, not codename

	// Hidden models are excluded from every advertised tier — including raw:
	// ListedEntries drops them before the registry ever sees them.
	if u, ok := reg.Resolve("ghost"); ok {
		t.Errorf(`Resolve("ghost") = %q, want not-found (model is hidden)`, u)
	}
	if u, ok := reg.Resolve("gpt-5.9-ghost"); ok {
		t.Errorf(`Resolve("gpt-5.9-ghost") = %q, want not-found (model is hidden)`, u)
	}

	fams := reg.Families()
	if !containsStr(fams, "sol") || !containsStr(fams, "5.5") || containsStr(fams, "ghost") {
		t.Errorf("Families = %v, want sol and 5.5 present, ghost absent", fams)
	}
}

func TestPopulateRegistryPriorityBreaksSameVersionTie(t *testing.T) {
	reg := router.NewRegistry()
	// Two irregular slugs the grammar can't parse, pinned via overrides to the
	// SAME codename+version, so only priority separates them for the bare name.
	reg.SetOverride("gpt-5.6-zed-a", "zed", "5.6", "")
	reg.SetOverride("gpt-5.6-zed-b", "zed", "5.6", "")
	models := []schema.Model{
		{Slug: "gpt-5.6-zed-a", Visibility: "list", Priority: 3},
		{Slug: "gpt-5.6-zed-b", Visibility: "list", Priority: 9},
	}
	catalog.PopulateRegistry(reg, models)

	if u, ok := reg.Resolve("zed"); !ok || u != "gpt-5.6-zed-b" {
		t.Errorf(`Resolve("zed") = (%q, %v), want ("gpt-5.6-zed-b", true) — higher priority wins the tie`, u, ok)
	}
}

func TestRefreshRegistryFromLiveCatalog(t *testing.T) {
	fake := newFakeCatalog(t, []schema.Model{solModel(), terraModel(),
		{Slug: "gpt-5.9-ghost", Visibility: "hide"}}, `W/"v1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), Now: newClock().now})
	reg := router.NewRegistry()

	if err := c.RefreshRegistry(context.Background(), fakeCred(), reg); err != nil {
		t.Fatalf("RefreshRegistry: %v", err)
	}
	if u, ok := reg.Resolve("sol"); !ok || u != "gpt-5.6-sol" {
		t.Errorf(`Resolve("sol") = (%q, %v), want gpt-5.6-sol`, u, ok)
	}
	if u, ok := reg.Resolve("terra"); !ok || u != "gpt-5.6-terra" {
		t.Errorf(`Resolve("terra") = (%q, %v), want gpt-5.6-terra`, u, ok)
	}
	if _, ok := reg.Resolve("ghost"); ok {
		t.Error("hidden model should not be advertised after RefreshRegistry")
	}
}

// TestRefreshRegistryBlocksForCurrentCatalogWhenStale proves RefreshRegistry
// installs the CURRENT catalog even when the held snapshot is stale — it must
// block on a real revalidation rather than publish the stale snapshot (which
// Models would serve immediately while revalidating in the background) and call
// that success.
func TestRefreshRegistryBlocksForCurrentCatalogWhenStale(t *testing.T) {
	fake := newFakeCatalog(t, []schema.Model{solModel()}, `W/"v1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	clk := newClock()
	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), TTL: 60 * time.Second, Now: clk.now})

	// Warm the cache with v1 (sol only).
	if _, err := c.Models(context.Background(), fakeCred()); err != nil {
		t.Fatalf("warm Models: %v", err)
	}

	// Go stale and change what the server advertises to sol+terra.
	clk.advance(120 * time.Second)
	fake.set([]schema.Model{solModel(), terraModel()}, `W/"v2"`, false)

	reg := router.NewRegistry()
	if err := c.RefreshRegistry(context.Background(), fakeCred(), reg); err != nil {
		t.Fatalf("RefreshRegistry: %v", err)
	}
	// terra is only in the CURRENT catalog; if RefreshRegistry had published the
	// stale v1 snapshot it would be absent.
	if _, ok := reg.Resolve("terra"); !ok {
		t.Error("RefreshRegistry installed a stale catalog: terra missing after a blocking refresh")
	}
}

// TestMissingETagOn200ClearsValidator proves a 200 without an ETag clears the
// held validator instead of retaining the previous one — otherwise a later 304
// for the old validator could wrongly mark the new body fresh.
func TestMissingETagOn200ClearsValidator(t *testing.T) {
	fake := newFakeCatalog(t, []schema.Model{solModel()}, `W/"e1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	clk := newClock()
	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), TTL: 60 * time.Second, Now: clk.now})

	// Cold fetch: client now holds etag e1.
	if _, err := c.Models(context.Background(), fakeCred()); err != nil {
		t.Fatalf("cold Models: %v", err)
	}

	// Go stale; server now returns a new body with NO ETag.
	clk.advance(120 * time.Second)
	fake.set([]schema.Model{solModel(), terraModel()}, "", false)
	if _, err := c.Models(context.Background(), fakeCred()); err != nil {
		t.Fatalf("stale Models: %v", err)
	}
	eventually(t, 2*time.Second, func() bool {
		m, err := c.Models(context.Background(), fakeCred())
		return err == nil && len(m) == 2
	})

	// A subsequent revalidation must carry NO If-None-Match: the no-ETag 200
	// cleared the validator (a fallback to e1 would be the bug).
	base := fake.callCount()
	clk.advance(120 * time.Second)
	_, _ = c.Models(context.Background(), fakeCred())
	eventually(t, 2*time.Second, func() bool { return fake.callCount() > base })
	fake.mu.Lock()
	inm := fake.lastINM
	fake.mu.Unlock()
	if inm != "" {
		t.Errorf("If-None-Match = %q after a no-ETag 200, want empty (validator must be cleared)", inm)
	}
}

// TestDiskCacheIgnoredOnClientVersionMismatch proves a disk cache written by a
// different client version is ignored and re-fetched, rather than served as if
// still valid after an upgrade.
func TestDiskCacheIgnoredOnClientVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "utraque-models_cache.json")
	clk := newClock()

	// A fresh (within-TTL) disk cache tagged with an OLD client version.
	cache := schema.Cache{ClientVersion: "old", ETag: `W/"d"`, FetchedAt: clk.now(), Models: []schema.Model{solModel()}}
	b, _ := json.Marshal(cache)
	if err := os.WriteFile(cachePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	fake := newFakeCatalog(t, []schema.Model{solModel(), terraModel()}, `W/"v2"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c := catalog.New(catalog.Options{
		BaseURL: srv.URL, HTTPClient: srv.Client(), Now: clk.now,
		CachePath: cachePath, ClientVersion: "new", TTL: 300 * time.Second,
	})

	models, err := c.Models(context.Background(), fakeCred())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("version-mismatched disk cache should be ignored and re-fetched; models = %d, want 2", len(models))
	}
	if got := fake.callCount(); got != 1 {
		t.Errorf("expected exactly one network fetch, got %d", got)
	}
}

// TestDiskCacheIgnoredWhenFetchedAtInFuture proves a future-dated fetched_at
// (a clock artefact) is distrusted and re-fetched rather than treated as
// perpetually fresh.
func TestDiskCacheIgnoredWhenFetchedAtInFuture(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "utraque-models_cache.json")
	clk := newClock()

	cache := schema.Cache{ETag: `W/"d"`, FetchedAt: clk.now().Add(time.Hour), Models: []schema.Model{solModel()}}
	b, _ := json.Marshal(cache)
	if err := os.WriteFile(cachePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	fake := newFakeCatalog(t, []schema.Model{solModel(), terraModel()}, `W/"v2"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c := catalog.New(catalog.Options{
		BaseURL: srv.URL, HTTPClient: srv.Client(), Now: clk.now,
		CachePath: cachePath, TTL: 300 * time.Second,
	})

	models, err := c.Models(context.Background(), fakeCred())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || fake.callCount() != 1 {
		t.Errorf("future-dated disk cache should be ignored and re-fetched; models=%d calls=%d", len(models), fake.callCount())
	}
}

// TestModelsReturnsDeepCopiedReasoningLevels proves a caller mutating a returned
// model's SupportedReasoningLevels cannot corrupt the cached snapshot other
// goroutines read.
func TestModelsReturnsDeepCopiedReasoningLevels(t *testing.T) {
	m := solModel()
	m.SupportedReasoningLevels = []schema.ReasoningLevel{{Effort: "low"}, {Effort: "high"}}
	fake := newFakeCatalog(t, []schema.Model{m}, `W/"v1"`)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	clk := newClock()
	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), TTL: 300 * time.Second, Now: clk.now})

	first, err := c.Models(context.Background(), fakeCred())
	if err != nil {
		t.Fatalf("first Models: %v", err)
	}
	first[0].SupportedReasoningLevels[0].Effort = "MUTATED"

	second, err := c.Models(context.Background(), fakeCred())
	if err != nil {
		t.Fatalf("second Models: %v", err)
	}
	if got := second[0].SupportedReasoningLevels[0].Effort; got != "low" {
		t.Errorf("caller mutation leaked into the cached snapshot: effort = %q, want low", got)
	}
}

// --- safety ---------------------------------------------------------------

// TestDefaultBaseURLIsCodexAndNoNetworkOnConstruction documents that the real
// endpoint is the default, and that constructing a client touches no network.
func TestDefaultBaseURLIsCodexAndNoNetworkOnConstruction(t *testing.T) {
	if catalog.DefaultBaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Errorf("DefaultBaseURL = %q", catalog.DefaultBaseURL)
	}
	// No BaseURL override, but we never call Models, so no request is made.
	c := catalog.New(catalog.Options{Now: newClock().now})
	if c.Age() != 0 {
		t.Errorf("Age on a never-fetched client = %s, want 0", c.Age())
	}
}

// --- helpers --------------------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestCatalogNeverFollowsRedirects: this request carries the Codex bearer
// token, the account id, and the CLI's originator headers. Following a redirect
// would replay all of them at whatever host the response named. The inference
// client and the OAuth refresh client already refuse; the catalog must too.
func TestCatalogNeverFollowsRedirects(t *testing.T) {
	var elsewhere atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the redirect target received an Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/models", http.StatusFound)
	}))
	defer srv.Close()

	c := catalog.New(catalog.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), Now: newClock().now})
	if _, err := c.Models(context.Background(), fakeCred()); err == nil {
		t.Fatal("a redirected catalog fetch reported success")
	} else if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error = %v, want it to name the redirect", err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the redirect target was contacted %d times; the credential must never be replayed", n)
	}
}
