package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/codex/catalog"
	"github.com/hughescr/utraque/internal/config"
)

func writeCodexVersionFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write Codex fixture: %v", err)
	}
	return path
}

func TestResolveCodexClientVersionDiscoversConfiguredExecutable(t *testing.T) {
	executable := writeCodexVersionFixture(t,
		"printf '%s\\n' 'harmless warning' >&2\nprintf '%s\\n' 'codex-cli 0.153.4'\n")
	cfg := config.Default()
	cfg.Codex.Executable = executable

	if err := resolveCodexClientVersion(context.Background(), &cfg); err != nil {
		t.Fatalf("resolveCodexClientVersion: %v", err)
	}
	if cfg.Codex.ClientVersion != "0.153.4" {
		t.Errorf("ClientVersion = %q, want 0.153.4", cfg.Codex.ClientVersion)
	}
}

func TestResolveCodexClientVersionPreservesOverrideWithoutExecuting(t *testing.T) {
	cfg := config.Default()
	cfg.Codex.ClientVersion = "0.200.1"
	cfg.Codex.Executable = filepath.Join(t.TempDir(), "missing-codex")

	if err := resolveCodexClientVersion(context.Background(), &cfg); err != nil {
		t.Fatalf("explicit override unexpectedly ran executable: %v", err)
	}
	if cfg.Codex.ClientVersion != "0.200.1" {
		t.Errorf("ClientVersion = %q, want explicit override", cfg.Codex.ClientVersion)
	}
}

func TestResolveCodexClientVersionRejectsMissingAndMalformedExecutable(t *testing.T) {
	for name, executable := range map[string]string{
		"missing":   filepath.Join(t.TempDir(), "missing-codex"),
		"malformed": writeCodexVersionFixture(t, "printf '%s\\n' 'codex-cli latest'\n"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Codex.Executable = executable
			err := resolveCodexClientVersion(context.Background(), &cfg)
			if err == nil {
				t.Fatal("resolveCodexClientVersion succeeded")
			}
			if !strings.Contains(err.Error(), config.EnvCodexClientVersion) {
				t.Errorf("error = %q, want explicit override guidance", err)
			}
			if cfg.Codex.ClientVersion != "" {
				t.Errorf("failure installed ClientVersion %q", cfg.Codex.ClientVersion)
			}
		})
	}
}

func TestDiscoverCodexClientVersionHonorsContextDeadline(t *testing.T) {
	executable := writeCodexVersionFixture(t, "while :; do :; done\n")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := discoverCodexClientVersion(ctx, executable)
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("timed-out version discovery took %s", elapsed)
	}
}

func TestDiscoverCodexClientVersionBoundsStdout(t *testing.T) {
	executable := writeCodexVersionFixture(t,
		"i=0\nwhile [ \"$i\" -lt 5000 ]; do printf x; i=$((i + 1)); done\n")
	_, err := discoverCodexClientVersion(context.Background(), executable)
	if err == nil || !strings.Contains(err.Error(), "more than 4096 bytes") {
		t.Fatalf("error = %v, want bounded-output error", err)
	}
}

func TestDetectedCodexClientVersionReachesCatalogRequest(t *testing.T) {
	executable := writeCodexVersionFixture(t, "printf '%s\\n' 'codex-cli 0.153.4'\n")
	cfg := config.Default()
	cfg.Codex.Executable = executable
	if err := resolveCodexClientVersion(context.Background(), &cfg); err != nil {
		t.Fatalf("resolveCodexClientVersion: %v", err)
	}

	var gotVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.URL.Query().Get("client_version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer upstream.Close()

	client := catalog.New(catalog.Options{
		BaseURL: upstream.URL, HTTPClient: upstream.Client(),
		ClientVersion: cfg.Codex.ClientVersion,
	})
	_, err := client.Models(context.Background(), auth.Credential{
		AccessToken: "test-token", AccountID: "test-account",
	})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotVersion != "0.153.4" {
		t.Errorf("client_version = %q, want detected version 0.153.4", gotVersion)
	}
}

// TestRunDiscoversCodexVersionBeforeCatalogWarm covers the production startup
// boundary end to end. The warmed request must carry the executable's version;
// testing the resolver and catalog client separately would not catch run
// forgetting to connect them.
func TestRunDiscoversCodexVersionBeforeCatalogWarm(t *testing.T) {
	dir := t.TempDir()
	executable := writeCodexVersionFixture(t, "printf '%s\\n' 'codex-cli 0.153.4'\n")
	authPath := filepath.Join(dir, "auth.json")
	authBody := fmt.Sprintf(`{"tokens":{"account_id":"test-account","access_token":%q,"refresh_token":"unused"}}`,
		jwtWithExp(time.Now().Add(time.Hour)))
	if err := os.WriteFile(authPath, []byte(authBody), 0o600); err != nil {
		t.Fatalf("write auth fixture: %v", err)
	}

	versionC := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		select {
		case versionC <- r.URL.Query().Get("client_version"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := map[string]string{
		"HOME":                     dir,
		config.EnvListen:           "127.0.0.1:0",
		config.EnvAnthropicBaseURL: upstream.URL,
		config.EnvCodexBaseURL:     upstream.URL,
		config.EnvCodexAuthFile:    authPath,
		config.EnvCodexCacheFile:   filepath.Join(dir, "models-cache.json"),
		config.EnvCodexExecutable:  executable,
	}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, func(key string) string { return env[key] }, io.Discard)
	}()

	select {
	case got := <-versionC:
		if got != "0.153.4" {
			t.Errorf("client_version = %q, want startup-detected version 0.153.4", got)
		}
	case err := <-done:
		t.Fatalf("run returned before catalog warm: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("startup catalog warm did not reach fake backend")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop after cancellation")
	}
}

// TestWarnDeprecatedEnvNamesOldAndNew captures the startup warning for a
// deprecated provider-report variable and asserts it names both the variable
// that was read and its replacement, and that nothing is logged when only the
// new names, or nothing, is set.
//
// deprecated: remove in the next release, with warnDeprecatedEnv.
func TestWarnDeprecatedEnvNamesOldAndNew(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	warnDeprecatedEnv(log, func(key string) string {
		return map[string]string{config.EnvProviderCacheTTL: "45s"}[key]
	})
	var rec struct {
		Level       string `json:"level"`
		Msg         string `json:"msg"`
		Deprecated  string `json:"deprecated"`
		Replacement string `json:"replacement"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode warning %q: %v", buf.String(), err)
	}
	if rec.Level != "WARN" || rec.Deprecated != config.EnvProviderCacheTTL || rec.Replacement != config.EnvProviderReportCacheTTL {
		t.Errorf("warning = %+v, want WARN naming %s and %s", rec, config.EnvProviderCacheTTL, config.EnvProviderReportCacheTTL)
	}
	if !strings.Contains(rec.Msg, "deprecated") {
		t.Errorf("msg = %q, want it to say the variable is deprecated", rec.Msg)
	}

	buf.Reset()
	warnDeprecatedEnv(log, func(key string) string {
		return map[string]string{config.EnvProviderReportCacheTTL: "45s", config.EnvProviderReportTimeout: "80s"}[key]
	})
	if buf.Len() != 0 {
		t.Errorf("the new names alone produced a warning: %s", buf.String())
	}
}
