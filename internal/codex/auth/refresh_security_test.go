//go:build unix

package auth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
)

// writeStaleAuthFile writes a fake auth.json under the test's TempDir whose
// access token is about to expire, so Get must refresh.
func writeStaleAuthFile(t *testing.T, refreshToken string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	body := authJSON{
		access:    makeJWT(time.Now().Add(5 * time.Second)), // stale => forces a refresh
		refresh:   refreshToken,
		accountID: "acct-sec",
	}.bytes()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write fake auth.json: %v", err)
	}
	return path
}

// TestRefreshDefaultClientNeverFollowsRedirects: the refresh POST carries the
// refresh token, so the default (production) HTTP client must treat a 3xx as
// the final response and never replay the request to the Location target.
func TestRefreshDefaultClientNeverFollowsRedirects(t *testing.T) {
	const canary = "REDIRECT-CANARY-REFRESH-TOKEN"

	var victimHits atomic.Int64
	var victimSawToken atomic.Bool
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		victimHits.Add(1)
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if strings.Contains(string(b), canary) || strings.Contains(r.URL.String(), canary) {
			victimSawToken.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"x"}`)
	}))
	t.Cleanup(victim.Close)

	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			victimHits.Store(0)
			victimSawToken.Store(false)
			var tokenHits atomic.Int64
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenHits.Add(1)
				http.Redirect(w, r, victim.URL+"/steal", code)
			}))
			t.Cleanup(tokenSrv.Close)

			s, err := New(Options{
				Path:     writeStaleAuthFile(t, canary),
				TokenURL: tokenSrv.URL,
				ClientID: "app_TEST",
				// HTTPClient deliberately NOT set: exercise the production default.
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = s.Get(context.Background())
			if err == nil {
				t.Error("a redirect from the token endpoint must not yield a credential")
			}
			if got := tokenHits.Load(); got != 1 {
				t.Errorf("token endpoint hits = %d, want 1", got)
			}
			if got := victimHits.Load(); got != 0 {
				t.Errorf("redirect target was contacted %d time(s); the refresh POST was followed", got)
			}
			if victimSawToken.Load() {
				t.Error("redirect target received the refresh token")
			}
			if err != nil && strings.Contains(err.Error(), canary) {
				t.Errorf("error leaked the refresh token: %q", err.Error())
			}
		})
	}
}

// TestRefreshErrorNeverEchoesEndpointBody: an OAuth error body may echo the
// presented refresh token; no failing status may let any of it reach the error
// text, the client-facing error JSON, or the logs.
func TestRefreshErrorNeverEchoesEndpointBody(t *testing.T) {
	const refreshCanary = "REFRESH-TOKEN-CANARY-8f3a"
	const bodyCanary = "BODY-CANARY-5c1e-endpoint-detail"

	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
	} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				_ = b
				w.WriteHeader(status)
				// Echo the presented refresh token verbatim, as a hostile or sloppy
				// endpoint might, plus a canary of its own.
				_, _ = fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"token %s rejected: %s","refresh_token":"%s"}`,
					refreshCanary, bodyCanary, refreshCanary)
			}))
			t.Cleanup(srv.Close)

			var logBuf bytes.Buffer
			s, err := New(Options{
				Path:       writeStaleAuthFile(t, refreshCanary),
				TokenURL:   srv.URL,
				ClientID:   "app_TEST",
				HTTPClient: srv.Client(),
				Logger:     slogTextLogger(&logBuf),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, getErr := s.Get(context.Background())
			if getErr == nil {
				t.Fatal("expected an error for a failing token endpoint")
			}

			rec := httptest.NewRecorder()
			if err := apierr.Write(rec, getErr); err != nil {
				t.Fatalf("apierr.Write: %v", err)
			}
			wire := rec.Body.String()
			if wire == "" {
				t.Fatal("apierr.Write produced no bytes")
			}

			surfaces := map[string]string{
				"err.Error()":  getErr.Error(),
				"apierr.Write": wire,
				"log output":   logBuf.String(),
			}
			for name, text := range surfaces {
				for _, leak := range []string{refreshCanary, bodyCanary, "invalid_grant", "error_description"} {
					if strings.Contains(text, leak) {
						t.Errorf("%s leaked %q: %q", name, leak, text)
					}
				}
			}
		})
	}
}
