package catalog_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/catalog"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
)

// perAccount serves each account a one-row list named after the account, with
// an ETag that is likewise the account's own. It fails the test if any request
// offers a validator other than its own account's.
func perAccount(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id := r.Header.Get("chatgpt-account-id")
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm != `"etag-`+id+`"` {
			t.Errorf("request for %q offered validator %s", id, inm)
		}
		w.Header().Set("ETag", `"etag-`+id+`"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"slug": id}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func credFor(account string) auth.Credential {
	c := fakeCred()
	c.AccountID = account
	return c
}

// A list held for A is never served to B, in memory or after a restart from
// the disk record: B's call fetches B's own list without offering A's ETag.
func TestAccountCatalog(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint("restart=", restart), func(t *testing.T) {
			var calls atomic.Int32
			srv := perAccount(t, &calls)
			a, b := credFor("acct-A"), credFor("acct-B")
			opts := catalog.Options{BaseURL: srv.URL, CacheFile: filepath.Join(t.TempDir(), "models.json")}
			c := catalog.New(opts)
			if _, err := c.Models(t.Context(), a); err != nil {
				t.Fatal(err)
			}
			if restart {
				c = catalog.New(opts)
			}
			for range 2 {
				models, err := c.Models(t.Context(), b)
				if err != nil || len(models) != 1 || models[0].Slug != "acct-B" {
					t.Fatalf("B was served %v (%v)", models, err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("%d fetches, want A's and one for B", calls.Load())
			}
			if n, _, loaded := c.Snapshot(a.Scope()); loaded || n != 0 {
				t.Error("health would show B's list as A's")
			}
			if n, _, loaded := c.Snapshot(b.Scope()); !loaded || n != 1 {
				t.Error("health does not show B's own list")
			}
		})
	}
}

// A failed fetch for B while A's list is held returns what a cold client
// returns, never A's rows, and leaves A's list in place for A.
func TestFailedFetchForAnotherAccountIsCold(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("chatgpt-account-id") != "acct-A" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"a-row"}]}`)
	}))
	defer srv.Close()
	var failures []string
	c := catalog.New(catalog.Options{BaseURL: srv.URL, OnFetch: func(scope string, _ int, f *catalog.Failure) {
		if f != nil {
			failures = append(failures, scope)
		}
	}})
	a, b := credFor("acct-A"), credFor("acct-B")
	if _, err := c.Models(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Models(t.Context(), b)
	if err == nil || rows != nil {
		t.Fatalf("B got %v, %v; want the cold error and no rows", rows, err)
	}
	if !slices.Equal(failures, []string{b.Scope()}) {
		t.Error("the failure was not reported under B's scope")
	}
	if rows, err := c.Models(t.Context(), a); err != nil || len(rows) != 1 || rows[0].Slug != "a-row" || calls.Load() != 2 {
		t.Error("B's failure disturbed A's held list")
	}
}

// A background revalidation for A that completes after B's list was committed
// is discarded: B's list stays held, and nothing is published or reported.
func TestLateBackgroundRevalidation(t *testing.T) {
	clk := newClock()
	a, b := credFor("acct-A"), credFor("acct-B")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id := r.Header.Get("chatgpt-account-id")
		if id == a.AccountID && r.Header.Get("If-None-Match") != "" {
			close(entered)
			<-release
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if id == b.AccountID && r.Header.Get("If-None-Match") != "" {
			t.Error("A's validator offered under B")
		}
		w.Header().Set("ETag", `"`+id+`"`)
		_, _ = fmt.Fprintf(w, `{"models":[{"slug":%q}]}`, id)
	}))
	defer srv.Close()
	logs := &syncBuffer{}
	var mu sync.Mutex
	var published []string
	var reported int
	c := catalog.New(catalog.Options{
		BaseURL: srv.URL, Now: clk.now, TTL: time.Minute,
		CacheFile: filepath.Join(t.TempDir(), "cache.json"),
		Logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnFetch:   func(string, int, *catalog.Failure) { mu.Lock(); reported++; mu.Unlock() },
		OnModels: func(m []cschema.CatalogModel) {
			mu.Lock()
			published = append(published, m[0].Slug)
			mu.Unlock()
		},
	})
	if _, err := c.Models(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	if rows, err := c.Models(t.Context(), a); err != nil || rows[0].Slug != a.AccountID {
		t.Fatal("A's stale list was not served")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("no background revalidation")
	}
	rows, err := c.Models(t.Context(), b)
	close(release)
	if err != nil || len(rows) != 1 || rows[0].Slug != b.AccountID {
		t.Fatalf("B was served %v (%v)", rows, err)
	}
	eventually(t, 2*time.Second, func() bool { return strings.Contains(logs.String(), "revalidation discarded") })
	if rows, err := c.Models(t.Context(), b); err != nil || rows[0].Slug != b.AccountID || calls.Load() != 3 {
		t.Fatal("the late revalidation replaced B's list")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(published, []string{a.AccountID, b.AccountID}) || reported != 2 {
		t.Fatalf("published %v, reported %d; the late revalidation must do neither", published, reported)
	}
}

// A foreground fetch for A that completes after B's commits: B never joins
// A's fetch and never receives A's rows. A's late result is a live answer to
// A's own request, so it is committed; B's next call then misses on scope and
// fetches B's list again, which corrects the snapshot and the disk record.
func TestLateForegroundFetchSelfCorrects(t *testing.T) {
	a, b := credFor("acct-A"), credFor("acct-B")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("chatgpt-account-id")
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = fmt.Fprintf(w, `{"models":[{"slug":%q}]}`, id)
	}))
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "cache.json")
	opts := catalog.Options{BaseURL: srv.URL, CacheFile: cache}
	c := catalog.New(opts)
	aDone := make(chan []cschema.CatalogModel, 1)
	go func() {
		rows, err := c.Models(t.Context(), a)
		if err != nil {
			t.Error(err)
		}
		aDone <- rows
	}()
	<-entered
	bDone := make(chan []cschema.CatalogModel, 1)
	go func() {
		rows, err := c.Models(t.Context(), b)
		if err != nil {
			t.Error(err)
		}
		bDone <- rows
	}()
	select {
	case rows := <-bDone:
		if len(rows) != 1 || rows[0].Slug != b.AccountID {
			t.Errorf("B was served %v", rows)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("B joined A's in-flight fetch")
	}
	close(release)
	if rows := <-aDone; len(rows) != 1 || rows[0].Slug != a.AccountID {
		t.Fatalf("A was served %v", rows)
	}
	for _, client := range []*catalog.Client{c, catalog.New(opts)} {
		rows, err := client.Models(t.Context(), b)
		if err != nil || len(rows) != 1 || rows[0].Slug != b.AccountID {
			t.Fatalf("B was served %v (%v) after A's late commit", rows, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("%d fetches, want A, B, and B once more after A's late commit", calls.Load())
	}
	var rec cschema.Cache
	if raw, err := os.ReadFile(cache); err != nil || json.Unmarshal(raw, &rec) != nil || rec.AccountScope != b.Scope() {
		t.Fatal("the disk record is not B's after the correction")
	}
}

func TestCacheDoesNotCrossEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	for _, model := range []string{"first", "second"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") != "" {
				t.Error("foreign endpoint received validator")
			}
			w.Header().Set("ETag", `"endpoint-specific"`)
			_, _ = fmt.Fprintf(w, `{"models":[{"slug":%q}]}`, model)
		}))
		defer srv.Close()
		rows, err := catalog.New(catalog.Options{BaseURL: srv.URL, CacheFile: path}).Models(t.Context(), fakeCred())
		if err != nil || len(rows) != 1 || rows[0].Slug != model {
			t.Fatal("wrong endpoint's catalog served")
		}
	}
}

func TestLegacyCacheIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(p, []byte(fmt.Sprintf(`{"fetched_at":%q,"models":[{"slug":"legacy"}]}`, time.Now().UTC().Format(time.RFC3339Nano))), 0600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeCatalog(t, nil, "")
	srv := httptest.NewServer(fake)
	defer srv.Close()
	rows, err := catalog.New(catalog.Options{BaseURL: srv.URL, CacheFile: p}).Models(t.Context(), fakeCred())
	if len(rows) != 0 || err != nil || fake.callCount() != 1 {
		t.Error("legacy cache was not a miss")
	}
}

// legacyCache is the pre-account-scope on-disk shape.
type legacyCache struct {
	ClientVersion string                 `json:"client_version"`
	ETag          string                 `json:"etag"`
	FetchedAt     time.Time              `json:"fetched_at"`
	Models        []cschema.CatalogModel `json:"models"`
}

// Unscoped, truncated or foreign records are a miss with no validator; the
// record written in their place is account- and endpoint-scoped, private, and
// still parses with the old decoder.
func TestDiskRecordCompatibility(t *testing.T) {
	for _, seed := range []string{
		`{"client_version":"","etag":"\"legacy\"","fetched_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","models":[{"slug":"legacy"}]}`,
		`{"account_scope":"codex:`,
		`not json at all`,
		`{"account_scope":"","endpoint":"","models":[{"slug":"legacy"}],"fetched_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`,
	} {
		t.Run(fmt.Sprintf("%.12s", seed), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "cache.json")
			if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("If-None-Match") != "" {
					t.Error("legacy validator offered")
				}
				w.Header().Set("ETag", `"new"`)
				_, _ = io.WriteString(w, `{"models":[{"slug":"new"}]}`)
			}))
			defer srv.Close()
			rows, err := catalog.New(catalog.Options{BaseURL: srv.URL, CacheFile: p}).Models(t.Context(), fakeCred())
			if err != nil || len(rows) != 1 || rows[0].Slug != "new" || calls.Load() != 1 {
				t.Fatalf("rows %v err %v calls %d", rows, err, calls.Load())
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(p)
			if err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("record mode %v (%v)", st.Mode().Perm(), err)
			}
			var old legacyCache
			if err := json.Unmarshal(raw, &old); err != nil || len(old.Models) != 1 || old.ETag != `"new"` {
				t.Fatalf("old decoder cannot parse new record: %v %s", err, raw)
			}
			var cur cschema.Cache
			_ = json.Unmarshal(raw, &cur)
			if cur.AccountScope != fakeCred().Scope() || cur.Endpoint != srv.URL {
				t.Fatalf("record not scoped: %s", raw)
			}
			rows, err = catalog.New(catalog.Options{BaseURL: srv.URL, CacheFile: p}).Models(t.Context(), fakeCred())
			if err != nil || len(rows) != 1 || calls.Load() != 1 {
				t.Fatalf("scoped record not reused: calls %d", calls.Load())
			}
			_, _ = catalog.New(catalog.Options{BaseURL: srv.URL, CacheFile: p}).Models(t.Context(), credFor("other"))
			if calls.Load() != 2 {
				t.Fatal("another account used a foreign record")
			}
		})
	}
}

func BenchmarkWarmAccountHit(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"slug":"one"}]}`)
	}))
	defer srv.Close()
	c := catalog.New(catalog.Options{BaseURL: srv.URL})
	path := filepath.Join(b.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"last_refresh":%q,"tokens":{"account_id":"fixture","access_token":"fixture-token"}}`, time.Now().Format(time.RFC3339))), 0600); err != nil {
		b.Fatal(err)
	}
	src, err := auth.New(auth.Options{Path: path, TokenURL: srv.URL})
	if err != nil {
		b.Fatal(err)
	}
	cred, err := src.Get(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := c.Models(b.Context(), cred); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Models(b.Context(), cred); err != nil {
			b.Fatal(err)
		}
	}
}
