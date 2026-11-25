package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/server"
)

// anyWindow returns an n-byte window of secret that occurs in haystack, or "".
// A prefix or suffix disclosed on its own (a "last 8 characters" diagnostic)
// passes a whole-value check but not this one.
func anyWindow(haystack, secret string, n int) string {
	for i := 0; i+n <= len(secret); i++ {
		if strings.Contains(haystack, secret[i:i+n]) {
			return secret[i : i+n]
		}
	}
	return ""
}

// Unlike the bare server's health handler, the application installs a health
// reporter that inspects the stored Codex credential. Exercise its projection
// with actual synthetic auth files, not a stand-in HealthExtra callback.
//
// Request full health detail: catalog failures, auth state and DeepSeek
// configuration must not disclose a credential or account id. Checking only
// the limited liveness document would never exercise those projections.
func FuzzConfiguredHealthRedaction(f *testing.F) {
	// Every secret carries private chunks of at least 8 bytes at its start and
	// end, so disclosing only one end of it is detected. Random-looking, so no
	// 8-byte window can occur in the public body by accident.
	const (
		accessStart, accessEnd   = "Aq7Kx2MaVn", "W9pD4r6Tby"
		refreshStart, refreshEnd = "Rm3Lc8Yu0e", "Bj5Fs1Ng7o"
		accountStart, accountEnd = "Ct6Zt2Xq9d", "Ek4Aw8Ui3h"
		localStart, localEnd     = "Lz1Qp5Hv8c", "Yd0Mn6Gs2x"
		keyStart, keyEnd         = "Dk9Wr3Jb7f", "Pe4Tu1Zo5a"
	)
	const account = accountStart + "m1ddl3Acc0untId" + accountEnd
	cfg := config.Default()
	cfg.Codex.AuthFile = filepath.Join(f.TempDir(), "auth.json")
	cfg.LocalToken = localStart + "m1ddl3L0calT0k3n" + localEnd
	cfg.DeepSeek.APIKey = keyStart + "m1ddl3Pr0v1d3rK3y" + keyEnd
	const offline = "http://127.0.0.1:0"
	cfg.Anthropic.BaseURL, cfg.Codex.BaseURL, cfg.Codex.TokenURL, cfg.DeepSeek.BaseURL = offline, offline, offline, offline
	var redirect atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if redirect.Load() {
			w.Header().Set("Location", "/signed?sig="+account)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, account, accessEnd)
	}))
	f.Cleanup(up.Close)
	cfg.Codex.BaseURL = up.URL
	srv, err := newServer(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		f.Fatal(err)
	}
	f.Add("refresh-canary", false, uint8(1))
	f.Add("\x00\xff\"/\\&=", true, uint8(1))
	f.Add("", true, uint8(0))
	f.Add("", false, uint8(0))
	f.Add("signed-location", false, uint8(2))
	f.Add("switch-after-failed-fetch", false, uint8(4))
	f.Fuzz(func(t *testing.T, detail string, expired bool, failure uint8) {
		if len(detail) > 4096 {
			t.Skip()
		}
		chunks := []string{accessStart, accessEnd, refreshStart, refreshEnd, accountStart, accountEnd, localStart, localEnd, keyStart, keyEnd}
		for _, chunk := range chunks {
			if strings.Contains(detail, chunk) {
				t.Skip() // a fuzzer copying a fixture chunk is not a disclosure
			}
		}
		exp, state := time.Unix(4102444800, 0), "ok" // 2100-01-01
		if expired {
			exp, state = time.Unix(1, 0), "stale"
		}
		jwt := jwtWithExp(exp)
		access := jwt + accessStart + hex.EncodeToString([]byte(detail)) + accessEnd
		refresh := refreshStart + detail + refreshEnd
		refreshJSON, err := json.Marshal(refresh)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"tokens":{"account_id":%q,"access_token":%q,"refresh_token":%s}}`, account, access, refreshJSON)
		if err := os.WriteFile(cfg.Codex.AuthFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cfg.Codex.AuthFile, 0o640); err != nil {
			t.Fatal(err)
		}
		var wantFailure string
		if mode := failure % 3; mode != 0 {
			restoreRegistry(t)
			redirect.Store(mode == 2)
			picker := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			picker.Host = "localhost"
			picker.Header.Set("X-Utraque-Token", cfg.LocalToken)
			srv.ServeHTTP(httptest.NewRecorder(), picker)
			switch {
			case expired:
				wantFailure = "no_credential"
			case mode == 2:
				wantFailure = "redirect_refused"
			default:
				wantFailure = "bad_request"
			}
		}
		switched := !expired && failure%3 != 0 && failure&4 != 0
		if switched {
			body = strings.Replace(body, account, account+"-second", 1)
			if err := os.WriteFile(cfg.Codex.AuthFile, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			wantFailure = ""
		}
		r := httptest.NewRequest(http.MethodGet, server.HealthPath, nil)
		r.Host = "localhost"
		r.Header.Set("X-Utraque-Token", cfg.LocalToken)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		var result struct {
			Catalog struct {
				Code string `json:"last_error_code"`
			} `json:"codex_catalog"`
			Codex struct {
				State   string
				Mode    string `json:"file_mode"`
				Warning string `json:"mode_warning"`
			} `json:"codex_auth"`
			DeepSeek struct{ Configured bool } `json:"deepseek"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || result.Codex.State != state || !result.DeepSeek.Configured {
			t.Fatal("health did not report the configured credentials' state")
		}
		if result.Codex.Mode != "0640" || result.Codex.Warning != auth.PermissiveModeWarning {
			t.Fatal("health did not project the mode and fixed warning")
		}
		if (switched || wantFailure != "") && result.Catalog.Code != wantFailure {
			t.Fatalf("catalog failure code = %q, want %q", result.Catalog.Code, wantFailure)
		}
		got := w.Body.String()
		for _, marker := range []string{accessStart + hex.EncodeToString([]byte(detail)) + accessEnd, refresh, account, cfg.DeepSeek.APIKey, cfg.LocalToken} {
			if strings.Contains(got, marker) {
				t.Fatal("configured health response retained private credential material")
			}
		}
		for _, chunk := range chunks {
			if strings.Contains(got, chunk) {
				t.Fatalf("configured health response disclosed part of a credential (chunk %q)", chunk[:2]+"...")
			}
		}
		// The fixed secrets: no 8-byte window of them may appear anywhere, which
		// also covers a first-N or last-N character hint in a reason or detail
		// field. The access token's JWT header and payload segments count too.
		for name, secret := range map[string]string{
			"local token": cfg.LocalToken, "deepseek key": cfg.DeepSeek.APIKey, "codex account id": account,
			"access token (jwt part)": jwt, "access token (tail)": accessEnd, "refresh token (head)": refreshStart,
		} {
			if anyWindow(got, secret, min(8, len(secret))) != "" {
				t.Fatalf("configured health response disclosed a fragment of the %s", name)
			}
		}
	})
}
