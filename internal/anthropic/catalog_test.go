package anthropic_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/anthropic"
	"github.com/hughescr/utraque/internal/transport"
)

// Every test here talks to an httptest server. None contacts api.anthropic.com
// and none reads a credential file; the tokens below are literals.
const (
	fakeBearer = "Bearer not-a-real-token"
	fakeAPIKey = "sk-ant-not-a-real-key"
)

func newCatalog(t *testing.T, h http.HandlerFunc, opts ...anthropic.CatalogOption) (*anthropic.CatalogClient, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	c, err := anthropic.NewCatalog(srv.URL, transport.NewStd(transport.DefaultOptions()), opts...)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return c, &hits
}

func writeModels(w http.ResponseWriter, models ...anthropic.CatalogModel) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": models})
}

func TestCatalogReadsTheModelList(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotAccept atomic.Value
	var gotBetas atomic.Value
	c, hits := newCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotQuery.Store(r.URL.RawQuery)
		gotAuth.Store(r.Header.Get("Authorization"))
		gotAccept.Store(r.Header.Get("Accept"))
		gotBetas.Store(r.Header.Values("Anthropic-Beta"))
		writeModels(w,
			anthropic.CatalogModel{ID: "claude-opus-5", DisplayName: "Opus 5", Type: "model", CreatedAt: "2026-05-01"},
			anthropic.CatalogModel{ID: "", DisplayName: "junk"},
		)
	})

	cred := anthropic.Credential{
		Authorization: fakeBearer,
		Beta:          []string{"oauth-2025-04-20", "context-1m-2025-08-07"},
	}
	models, err := c.Models(t.Context(), cred)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	if got := gotPath.Load(); got != anthropic.CatalogPath {
		t.Errorf("path = %v, want %q", got, anthropic.CatalogPath)
	}
	if got := gotQuery.Load(); got != "limit=1000" {
		t.Errorf("query = %v, want limit=1000", got)
	}
	if got := gotAuth.Load(); got != fakeBearer {
		t.Errorf("Authorization = %v, want the caller's own header", got)
	}
	if got := gotAccept.Load(); got != "application/json" {
		t.Errorf("Accept = %v", got)
	}
	// Repeated anthropic-beta values must stay separate header lines.
	if got, want := gotBetas.Load(), cred.Beta; !reflect.DeepEqual(got, want) {
		t.Errorf("Anthropic-Beta = %v, want %v as separate values", got, want)
	}

	if len(models) != 1 {
		t.Fatalf("models = %+v, want the one row with a non-empty id", models)
	}
	if models[0].ID != "claude-opus-5" || models[0].DisplayName != "Opus 5" {
		t.Errorf("model = %+v", models[0])
	}
}

func TestCatalogSendsTheAPIKeyWhenThereIsNoBearer(t *testing.T) {
	var gotKey, gotAuth, gotVersion atomic.Value
	c, _ := newCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("X-Api-Key"))
		gotAuth.Store(r.Header.Get("Authorization"))
		gotVersion.Store(r.Header.Get("Anthropic-Version"))
		writeModels(w)
	})

	if _, err := c.Models(t.Context(), anthropic.Credential{APIKey: fakeAPIKey, Version: "2026-01-01"}); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if got := gotKey.Load(); got != fakeAPIKey {
		t.Errorf("X-Api-Key = %v", got)
	}
	if got := gotAuth.Load(); got != "" {
		t.Errorf("Authorization = %v, want empty when only an api key was supplied", got)
	}
	if got := gotVersion.Load(); got != "2026-01-01" {
		t.Errorf("Anthropic-Version = %v, want the caller's own value", got)
	}
}

func TestCatalogWithoutACredentialNeverContactsUpstream(t *testing.T) {
	c, hits := newCatalog(t, func(w http.ResponseWriter, r *http.Request) { writeModels(w) })

	_, err := c.Models(t.Context(), anthropic.Credential{})
	if !errors.Is(err, anthropic.ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
	if hits.Load() != 0 {
		t.Errorf("hits = %d, want 0", hits.Load())
	}
	// And it must not be negative-cached: the next request may well carry one.
	if _, cached := c.NegativeCachedUntil(); cached {
		t.Error("a missing credential must not open a negative-cache window")
	}
}

func TestCatalogNegativeCachesFailuresAndExpiresThem(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)

	now := time.Now()
	clock := func() time.Time { return now }

	c, hits := newCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeModels(w, anthropic.CatalogModel{ID: "claude-opus-5", DisplayName: "Opus 5"})
	}, anthropic.WithCatalogNow(func() time.Time { return clock() }),
		anthropic.WithCatalogNegativeTTL(60*time.Second))

	cred := anthropic.Credential{Authorization: fakeBearer}

	if _, err := c.Models(t.Context(), cred); err == nil {
		t.Fatal("expected the 401 to surface as an error")
	}
	for range 5 {
		if _, err := c.Models(t.Context(), cred); err == nil {
			t.Fatal("expected the cached failure to keep surfacing")
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 — the failure must be cached", hits.Load())
	}
	until, cached := c.NegativeCachedUntil()
	if !cached {
		t.Fatal("expected a live negative-cache window")
	}
	if want := now.Add(60 * time.Second); !until.Equal(want) {
		t.Errorf("negative cache until %v, want %v", until, want)
	}

	// Past the window, it asks again — and recovers.
	now = now.Add(61 * time.Second)
	fail.Store(false)
	models, err := c.Models(t.Context(), cred)
	if err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
	if _, cached := c.NegativeCachedUntil(); cached {
		t.Error("a success must clear the negative-cache window")
	}
}

func TestCatalogCachesSuccessForTheTTL(t *testing.T) {
	now := time.Now()
	c, hits := newCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		writeModels(w, anthropic.CatalogModel{ID: "claude-opus-5", DisplayName: "Opus 5"})
	}, anthropic.WithCatalogNow(func() time.Time { return now }),
		anthropic.WithCatalogTTL(5*time.Minute))

	cred := anthropic.Credential{Authorization: fakeBearer}
	for range 4 {
		if _, err := c.Models(t.Context(), cred); err != nil {
			t.Fatalf("Models: %v", err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1 within the TTL", hits.Load())
	}

	now = now.Add(6 * time.Minute)
	if _, err := c.Models(t.Context(), cred); err != nil {
		t.Fatalf("Models after TTL: %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2 after the TTL lapsed", hits.Load())
	}
}

func TestCatalogCallersCannotMutateTheCache(t *testing.T) {
	c, _ := newCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		writeModels(w, anthropic.CatalogModel{ID: "claude-opus-5", DisplayName: "Opus 5"})
	})
	cred := anthropic.Credential{Authorization: fakeBearer}

	first, err := c.Models(t.Context(), cred)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	first[0].DisplayName = "tampered"

	second, err := c.Models(t.Context(), cred)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if second[0].DisplayName != "Opus 5" {
		t.Errorf("cache was mutated through a returned slice: %+v", second[0])
	}
}

func TestCatalogTreatsARedirectAsAFailure(t *testing.T) {
	c, _ := newCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		// Following this would re-send the caller's bearer token to another
		// host, so it must never be followed.
		w.Header().Set("Location", "https://example.invalid/v1/models")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})

	_, err := c.Models(t.Context(), anthropic.Credential{Authorization: fakeBearer})
	if !errors.Is(err, anthropic.ErrRedirected) {
		t.Fatalf("err = %v, want ErrRedirected", err)
	}
}

func TestCatalogRejectsAMalformedBody(t *testing.T) {
	c, _ := newCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": "not an array"`))
	})
	if _, err := c.Models(t.Context(), anthropic.Credential{Authorization: fakeBearer}); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestCredentialFromRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", " "+fakeBearer+" ")
	r.Header.Set("X-Api-Key", fakeAPIKey)
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Add("Anthropic-Beta", "oauth-2025-04-20")
	r.Header.Add("Anthropic-Beta", "")
	r.Header.Add("Anthropic-Beta", " context-1m-2025-08-07 ")

	cred := anthropic.CredentialFromRequest(r)
	if cred.Authorization != fakeBearer {
		t.Errorf("Authorization = %q, want trimmed %q", cred.Authorization, fakeBearer)
	}
	if cred.APIKey != fakeAPIKey {
		t.Errorf("APIKey = %q", cred.APIKey)
	}
	if cred.Version != "2023-06-01" {
		t.Errorf("Version = %q", cred.Version)
	}
	want := []string{"oauth-2025-04-20", "context-1m-2025-08-07"}
	if !reflect.DeepEqual(cred.Beta, want) {
		t.Errorf("Beta = %v, want %v (empties dropped, values trimmed)", cred.Beta, want)
	}
	if !cred.Present() {
		t.Error("Present() = false")
	}

	if anthropic.CredentialFromRequest(nil).Present() {
		t.Error("a nil request must not yield a present credential")
	}
	if (anthropic.Credential{Version: "2023-06-01"}).Present() {
		t.Error("a version alone is not a credential")
	}
}

func TestNewCatalogRejectsBadBaseURLs(t *testing.T) {
	tr := transport.NewStd(transport.DefaultOptions())
	for _, bad := range []string{"", "ftp://api.anthropic.com", "/no-scheme", "https://"} {
		if _, err := anthropic.NewCatalog(bad, tr); err == nil {
			t.Errorf("NewCatalog(%q) should have failed", bad)
		}
	}
	if _, err := anthropic.NewCatalog("https://api.anthropic.com", nil); err == nil {
		t.Error("NewCatalog with a nil transport should have failed")
	}

	c, err := anthropic.NewCatalog("https://user:secret@api.anthropic.com/base/?q=1#frag", tr)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	// Userinfo, query and fragment must be stripped: a credential must never
	// ride in a configured URL.
	if got, want := c.BaseURL(), "https://api.anthropic.com/base"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

// TestCatalogDoesNotServeOneCallersListToAnother: utraque holds no Anthropic
// secret of its own — every catalog read is signed by whichever credential the
// caller sent this request. The cache is therefore keyed on that credential.
// Harmless on a single-user loopback, wrong the moment the local token is
// shared, which is exactly the configuration local_token exists to support.
func TestCatalogDoesNotServeOneCallersListToAnother(t *testing.T) {
	var seen atomic.Value // last Authorization observed
	c, hits := newCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seen.Store(auth)
		writeModels(w, anthropic.CatalogModel{ID: "claude-for-" + auth, DisplayName: auth})
	}, anthropic.WithCatalogTTL(time.Hour))

	alice := anthropic.Credential{Authorization: "Bearer alice-token"}
	bob := anthropic.Credential{Authorization: "Bearer bob-token"}

	got, err := c.Models(t.Context(), alice)
	if err != nil {
		t.Fatalf("Models(alice): %v", err)
	}
	if len(got) != 1 || got[0].ID != "claude-for-Bearer alice-token" {
		t.Fatalf("alice got %+v", got)
	}

	got, err = c.Models(t.Context(), bob)
	if err != nil {
		t.Fatalf("Models(bob): %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2: bob must not be served alice's cached list", hits.Load())
	}
	if len(got) != 1 || got[0].ID != "claude-for-Bearer bob-token" {
		t.Errorf("bob got %+v, want a list fetched with bob's own credential", got)
	}

	// The cache is one slot keyed on the credential, not a per-caller map: it
	// holds only the most recent caller's answer, so alice re-fetches with her
	// own credential rather than being handed bob's. Bounded, and never wrong.
	if _, err := c.Models(t.Context(), alice); err != nil {
		t.Fatalf("Models(alice, again): %v", err)
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3", hits.Load())
	}
}
