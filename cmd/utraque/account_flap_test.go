package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/config"
)

// aOnlyCatalog is the fake catalog with one alias no compiled-in route
// supplies: "aonly" resolves only once this catalog is published.
var aOnlyCatalog = strings.ReplaceAll(fakeCatalogBody, "gpt-6.2-sol", "gpt-9-aonly")

func sendModel(t *testing.T, env *codexEnv, model string) (int, string) {
	t.Helper()
	r := post(t, env.front.URL+"/v1/messages", fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"test"}]}`, model), nil)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, string(body)
}

func pollHealth(t *testing.T, env *codexEnv) {
	t.Helper()
	r, err := http.Get(env.front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
}

// The Codex CLI may rewrite auth.json in place, so a reader can catch it
// empty, truncated or naming no account. That is not a login change: once the
// file reads as A again, A's catalog-only alias still routes, no catalog is
// fetched again, and no request is refused as a login change.
func TestUnreadableAuthKeepsCatalogRoutes(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, partial := range []struct{ name, body string }{
			{"empty", ""},
			{"truncated", `{"tokens":{"account_id":"acct-fa`},
			{"not-json", "\x00\x00\x00"},
			{"no-account", `{"tokens":{}}`},
			{"blank-account", `{"tokens":{"account_id":"  ","access_token":"x"}}`},
		} {
			t.Run(fmt.Sprintf("disk=%v/%s", disk, partial.name), func(t *testing.T) {
				restoreRegistry(t)
				env := newCodexEnv(t, func(c *config.Config) {
					if disk {
						c.Codex.CacheFile = filepath.Join(t.TempDir(), "models.json")
					}
				})
				env.codex.catalogBody = aOnlyCatalog
				if s, _ := sendModel(t, env, "sol"); s != 200 {
					t.Fatal("could not prime A's catalog")
				}
				if s, _ := sendModel(t, env, "aonly"); s != 200 {
					t.Fatal("A's catalog-only alias did not route")
				}
				good, err := os.ReadFile(env.authPath)
				if err != nil {
					t.Fatal(err)
				}
				st, err := os.Stat(env.authPath)
				if err != nil {
					t.Fatal(err)
				}
				fetches, posts := env.codex.catalog.Load(), env.codex.calls.Load()
				if err := os.WriteFile(env.authPath, []byte(partial.body), 0o600); err != nil {
					t.Fatal(err)
				}
				pollHealth(t, env) // lands in the rewrite window
				// A request that reads the file now gets the ordinary
				// credential failure: not a 404, not the login-changed 400,
				// and nothing is sent.
				status, body := sendModel(t, env, "aonly")
				if status == 200 || status == 400 || status == 404 || strings.Contains(body, "login changed") || env.codex.calls.Load() != posts {
					t.Errorf("request during the rewrite: HTTP %d %s", status, body)
				}
				if err := os.WriteFile(env.authPath, good, 0o600); err != nil {
					t.Fatal(err)
				}
				// Same size and mtime as before: only the content tells.
				if err := os.Chtimes(env.authPath, st.ModTime(), st.ModTime()); err != nil {
					t.Fatal(err)
				}
				var got []int
				for range 3 {
					s, _ := sendModel(t, env, "aonly")
					got = append(got, s)
				}
				pollHealth(t, env)
				s, _ := sendModel(t, env, "aonly")
				got = append(got, s)
				for _, s := range got {
					if s != 200 {
						t.Fatalf("unchanged login lost a routable alias: %v", got)
					}
				}
				if f := env.codex.catalog.Load(); f != fetches {
					t.Errorf("the flap cost %d catalog fetch(es)", f-fetches)
				}
			})
		}
	}
}

// A delete-and-recreate shows a reader a missing file for an instant. The
// alias still routes once the file is back.
func TestMomentaryAbsenceKeepsCatalogRoutes(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, nil)
	env.codex.catalogBody = aOnlyCatalog
	if s, _ := sendModel(t, env, "aonly"); s != 404 || env.codex.calls.Load() != 0 {
		t.Fatal("a catalog-only alias routed, or reached upstream, before any catalog loaded")
	}
	if s, _ := sendModel(t, env, "sol"); s != 200 {
		t.Fatal("could not prime A's catalog")
	}
	good, err := os.ReadFile(env.authPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(env.authPath); err != nil {
		t.Fatal(err)
	}
	pollHealth(t, env)
	if err := os.WriteFile(env.authPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, _ := sendModel(t, env, "aonly"); s != 200 {
		t.Fatalf("one missing read lost the alias: HTTP %d", s)
	}
}

// A request already accepted completes under its pinned login while the file
// flaps underneath it.
func TestInFlightRequestSurvivesAuthFlap(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, nil)
	env.codex.catalogBody = aOnlyCatalog
	entered, release := make(chan struct{}), make(chan struct{})
	env.codex.respond = func(_ *testing.T, w http.ResponseWriter, _ *http.Request, n int64) {
		if n == 2 {
			close(entered)
			<-release
		}
		writeSSE(w, solStream)
	}
	if s, _ := sendModel(t, env, "sol"); s != 200 {
		t.Fatal("could not prime A's catalog")
	}
	good, err := os.ReadFile(env.authPath)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { s, _ := sendModel(t, env, "aonly"); done <- s }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("request did not reach upstream")
	}
	if err := os.WriteFile(env.authPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pollHealth(t, env)
	if err := os.WriteFile(env.authPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	pollHealth(t, env)
	close(release)
	if s := <-done; s != 200 {
		t.Fatalf("in-flight request failed across a flap: HTTP %d", s)
	}
}

// The accepted tradeoff of an unscoped route table, pinned so that changing it
// is a deliberate decision: after a genuine A → B switch, a name only A's
// catalog listed still routes, and the request is sent under B's login (where
// the real upstream rejects a model B lacks). That same request loads B's
// catalog, so from then on the alias set is B's: A's alias is a 404 with no
// upstream call and B's own alias routes.
func TestPreviousAccountAliasRoutesUnderCurrentLogin(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprint("disk=", disk), func(t *testing.T) {
			restoreRegistry(t)
			env := newCodexEnvOpts(t, func(c *config.Config) {
				if disk {
					c.Codex.CacheFile = filepath.Join(t.TempDir(), "models.json")
				}
			}, codexEnvOptions{CaptureLogs: true})
			env.codex.catalogBody = aOnlyCatalog
			env.codex.catalogFor = func(account string) string {
				if account == "account-B" {
					return strings.ReplaceAll(fakeCatalogBody, "gpt-5.5", "gpt-9-bonly")
				}
				return ""
			}
			if s, _ := sendModel(t, env, "aonly"); s != 404 {
				t.Fatal("aonly routed before A's catalog loaded")
			}
			if s, _ := sendModel(t, env, "sol"); s != 200 {
				t.Fatal("could not prime A's catalog")
			}
			fetches := env.codex.catalog.Load()
			writeAuthJSONWithAccount(t, env.authPath, jwtWithExp(time.Now().Add(8*time.Hour)), "refresh-B", "account-B")

			if s, body := sendModel(t, env, "aonly"); s != 200 {
				t.Fatalf("A's alias after the switch: HTTP %d %s", s, body)
			}
			_, account, _ := env.codex.lastIdentity(t)
			if account != "account-B" || !strings.Contains(env.codex.lastBody(t), `"gpt-9-aonly"`) {
				t.Fatalf("A's alias was sent to %q, body %s; want gpt-9-aonly under account-B", account, env.codex.lastBody(t))
			}
			if env.codex.catalog.Load() != fetches+1 {
				t.Fatal("the first request under B did not load B's catalog")
			}

			posts := env.codex.calls.Load()
			if s, _ := sendModel(t, env, "aonly"); s != 404 || env.codex.calls.Load() != posts {
				t.Error("A's alias outlived B's catalog load, or a 404 reached upstream")
			}
			if s, _ := sendModel(t, env, "bonly"); s != 200 {
				t.Error("B's own alias does not route")
			}
			if _, account, _ := env.codex.lastIdentity(t); account != "account-B" {
				t.Errorf("B's alias was sent to %q", account)
			}
			r, err := http.Get(env.front.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			var h struct {
				Routing struct {
					Bare []string `json:"bare_aliases"`
				} `json:"codex_routing"`
			}
			if err := json.NewDecoder(r.Body).Decode(&h); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(h.Routing.Bare, "aonly") || !slices.Contains(h.Routing.Bare, "bonly") {
				t.Errorf("bare_aliases %v are not B's", h.Routing.Bare)
			}
			for _, raw := range []string{"acct-fake", "account-B"} {
				if strings.Contains(env.logs.String(), raw) {
					t.Errorf("log carries the raw account id %q", raw)
				}
			}
		})
	}
}
