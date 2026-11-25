package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/accountscope"
	"github.com/hughescr/utraque/internal/codex/catalog"
	"github.com/hughescr/utraque/internal/codex/wire"
	"github.com/hughescr/utraque/internal/config"
)

func TestWarmingDoesNotOverwriteFetchOutcome(t *testing.T) {
	s := newCatalogState()
	s.begin("scope-a")
	if state, _, age := s.read("scope-a"); state != catalogWarming || age >= 0 {
		t.Error("starting the warm is not a fetch completion")
	}
	f := catalog.CredentialAcquisitionFailed()
	s.mark("scope-a", catalogFailed, f)
	s.begin("scope-a")
	if _, failure, _ := s.read("scope-a"); failure != f {
		t.Error("starting the warm erased the last completed attempt")
	}
	// Another account's record reads as no attempt; an unknown account
	// contradicts nothing.
	if state, failure, age := s.read("scope-b"); state != catalogCold || failure != nil || age >= 0 {
		t.Error("another account's failure was shown")
	}
	if _, failure, _ := s.read(""); failure != f {
		t.Error("an unknown current account hid the record")
	}
}

// Use only fixture credentials. The upstream checks them before returning its
// fixed diagnostic; no server in this test is a general-purpose header echo.
func TestCatalogHealthFailureSurfaces(t *testing.T) {
	const account = "AcctFixture93QnR"
	const sig = "SignatureFixture37JpZ"
	const transportPath = "TransportFixture91CmX"
	access := jwtWithExp(time.Unix(4102444800, 0)) + "AccessFixture72HsK"
	for _, path := range []string{"warm", "picker", "revalidation"} {
		for _, tc := range []struct {
			code   string
			status int
		}{{"bad_request", 400}, {"redirect_refused", 307}, {"transport", 0}} {
			t.Run(path+"/"+tc.code, func(t *testing.T) {
				restoreRegistry(t)
				var requests atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Header.Get("Authorization") != "Bearer "+access || r.Header.Get(wire.HeaderAccountID) != account {
						t.Error("upstream did not receive the fixture credential")
					}
					switch tc.status {
					case 400:
						w.WriteHeader(400)
						_, _ = io.WriteString(w, "Bearer "+access+" account="+account)
					case 307:
						w.Header().Set("Location", "/signed?signature="+sig)
						w.WriteHeader(307)
					case 0:
						// EOF before response headers. The client's url.Error includes
						// the private fixture path even though no status exists.
						panic(http.ErrAbortHandler)
					}
				}))
				defer up.Close()
				env := newCodexEnvOpts(t, func(c *config.Config) {
					c.Codex.BaseURL = up.URL + "/" + transportPath
					c.LocalToken = "LocalFixture49DvT"
					writeAuthJSONWithAccount(t, c.Codex.AuthFile, access, "RefreshFixture25FqA", account)
					if path == "revalidation" {
						c.Codex.CacheFile = filepath.Join(t.TempDir(), "cache.json")
						if err := os.WriteFile(c.Codex.CacheFile, []byte(fmt.Sprintf(`{"account_scope":%q,"endpoint":%q,"fetched_at":"2001-01-01T00:00:00Z","models":[{"slug":"gpt-fixture"}]}`, accountscope.Key("codex", account), c.Codex.BaseURL)), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}, codexEnvOptions{CaptureLogs: true, Level: slog.LevelInfo})
				get := func(target string, full bool) string {
					r := httptest.NewRequest(http.MethodGet, target, nil)
					r.Host = "localhost"
					if full {
						r.Header.Set("X-Utraque-Token", "LocalFixture49DvT")
					}
					w := httptest.NewRecorder()
					env.app.srv.ServeHTTP(w, r)
					if w.Code != 200 {
						t.Fatalf("%s: status %d", target, w.Code)
					}
					return w.Body.String()
				}
				if path == "warm" {
					env.app.warmCatalog(t.Context())
				} else {
					get("/v1/models", true)
				}
				// Wait for the actual path's failure log; the stale picker returns
				// before the attempt completes. A cached read isn't a new attempt.
				logged := map[string]string{"warm": "catalog warm failed", "picker": "catalog read failed", "revalidation": "catalog background revalidation failed"}[path]
				deadline := time.Now().Add(3 * time.Second)
				for !strings.Contains(env.logs.String(), logged) {
					if time.Now().After(deadline) {
						t.Fatal("catalog failure was not logged")
					}
					time.Sleep(time.Millisecond)
				}
				before := requests.Load()
				full, limited := get("/healthz", true), get("/healthz", false)
				if before == 0 || before != requests.Load() {
					t.Fatal("expected a catalog attempt but no requests from either health poll")
				}
				var health struct {
					Catalog struct {
						Code    string   `json:"last_error_code"`
						Status  *int     `json:"last_error_status"`
						Message string   `json:"last_error"`
						Age     *float64 `json:"last_attempt_age_s"`
						Loaded  bool     `json:"loaded"`
					} `json:"codex_catalog"`
				}
				if err := json.Unmarshal([]byte(full), &health); err != nil {
					t.Fatal(err)
				}
				c := health.Catalog
				if c.Code != tc.code || c.Age == nil || c.Message == "" || c.Loaded != (path == "revalidation") {
					t.Errorf("incomplete failure classification: code=%q, age=%v, loaded=%v", c.Code, c.Age, c.Loaded)
				}
				if (tc.status == 0) != (c.Status == nil) || c.Status != nil && *c.Status != tc.status {
					t.Error("last_error_status does not match upstream")
				}
				if tc.status == 400 && (!strings.Contains(c.Message, "client_version") || !strings.Contains(c.Message, "UTRAQUE_CODEX_CLIENT_VERSION")) {
					t.Error("400 missing version hint")
				}
				for _, surface := range []string{full, limited, get("/v1/models", true), env.logs.String()} {
					for _, marker := range []string{"AccessFixture72HsK", account, sig, transportPath} {
						if strings.Contains(surface, marker) {
							t.Fatal("catalog surface contained a private fixture marker")
						}
					}
				}
			})
		}
	}
}
