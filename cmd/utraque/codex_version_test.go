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
	"sync"
	"sync/atomic"
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

	probe, err := resolveCodexClientVersion(context.Background(), &cfg, nil)
	if err != nil {
		t.Fatalf("resolveCodexClientVersion: %v", err)
	}
	if cfg.Codex.ClientVersion != "0.153.4" {
		t.Errorf("ClientVersion = %q, want 0.153.4", cfg.Codex.ClientVersion)
	}
	if probe == nil || probe.versionFunc() == nil {
		t.Error("a discovered version must come with a probe that keeps it current")
	}
}

func TestResolveCodexClientVersionPreservesOverrideWithoutExecuting(t *testing.T) {
	cfg := config.Default()
	cfg.Codex.ClientVersion = "0.200.1"
	cfg.Codex.Executable = filepath.Join(t.TempDir(), "missing-codex")

	probe, err := resolveCodexClientVersion(context.Background(), &cfg, nil)
	if err != nil {
		t.Fatalf("explicit override unexpectedly ran executable: %v", err)
	}
	if cfg.Codex.ClientVersion != "0.200.1" {
		t.Errorf("ClientVersion = %q, want explicit override", cfg.Codex.ClientVersion)
	}
	// No probe means no version func, so the catalog has nothing that could
	// stat or run the executable later either.
	if probe != nil || probe.versionFunc() != nil {
		t.Error("an explicit override must not produce a re-resolving probe")
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
			probe, err := resolveCodexClientVersion(context.Background(), &cfg, nil)
			if probe != nil {
				t.Error("a failed discovery returned a probe")
			}
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
	if _, err := resolveCodexClientVersion(context.Background(), &cfg, nil); err != nil {
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
	// The warm is detached from run's context and writes the disk cache after
	// the response, so wait for that write; otherwise it can race TempDir's
	// cleanup once run returns.
	cacheFile := env[config.EnvCodexCacheFile]
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(cacheFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup catalog warm never wrote its disk cache")
		}
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

// --- runtime re-resolution ------------------------------------------------

// installCountingCodex writes an executable at path that records every run in
// counter and answers --version with output. It is written beside path and
// renamed into place, the way a package manager replaces a file, so an
// "upgrade" is a new file (new inode) rather than an edit of the old one.
func installCountingCodex(t *testing.T, path, counter, output string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nprintf 'run\\n' >> %q\nprintf '%%s\\n' %q\n", counter, output)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(script), 0o700); err != nil {
		t.Fatalf("write Codex fixture: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("install Codex fixture: %v", err)
	}
}

// versionRuns counts how many times a counting fixture has been executed.
func versionRuns(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read run counter: %v", err)
	}
	return strings.Count(string(b), "run\n")
}

// relink atomically re-points a symlink, as `brew upgrade` does.
func relink(t *testing.T, target, link string) {
	t.Helper()
	tmp := link + ".tmp"
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatalf("rename symlink: %v", err)
	}
}

// logRecords decodes a JSON log buffer into one map per line.
func logRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func recordsWithMsgPrefix(recs []map[string]any, prefix string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// TestCodexVersionProbeRunsVersionOnlyWhenTheExecutableChanges covers the
// install shapes a real Codex CLI arrives in. Re-checking an unchanged
// executable must cost a stat, never a subprocess; an upgrade — a replaced
// file, or a re-pointed symlink — must be noticed on the next check and
// logged once as old -> new.
func TestCodexVersionProbeRunsVersionOnlyWhenTheExecutableChanges(t *testing.T) {
	cases := []struct {
		name string
		// install lays out v1 and returns the configured executable plus an
		// upgrade func that installs v2.
		install func(t *testing.T, dir, counter string) (executable string, upgrade func())
	}{
		{
			name: "absolute path replaced in place",
			install: func(t *testing.T, dir, counter string) (string, func()) {
				exe := filepath.Join(dir, "codex")
				installCountingCodex(t, exe, counter, "codex-cli 0.155.0-alpha.9")
				return exe, func() { installCountingCodex(t, exe, counter, "codex-cli 0.155.0") }
			},
		},
		{
			name: "bare name found on PATH",
			install: func(t *testing.T, dir, counter string) (string, func()) {
				bin := filepath.Join(dir, "bin")
				if err := os.Mkdir(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				exe := filepath.Join(bin, "codex")
				installCountingCodex(t, exe, counter, "codex-cli 0.155.0-alpha.9")
				return "codex", func() { installCountingCodex(t, exe, counter, "codex-cli 0.155.0") }
			},
		},
		{
			// bin/codex -> opt/codex -> cellar/<version>/codex, like Homebrew.
			// The upgrade only re-points the inner link; the outer path and its
			// own link never change.
			name: "symlink chain re-pointed",
			install: func(t *testing.T, dir, counter string) (string, func()) {
				for _, d := range []string{"bin", "opt", "cellar/0.155.0-alpha.9", "cellar/0.155.0"} {
					if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				installCountingCodex(t, filepath.Join(dir, "cellar/0.155.0-alpha.9/codex"), counter, "codex-cli 0.155.0-alpha.9")
				installCountingCodex(t, filepath.Join(dir, "cellar/0.155.0/codex"), counter, "codex-cli 0.155.0")
				relink(t, "../cellar/0.155.0-alpha.9/codex", filepath.Join(dir, "opt/codex"))
				relink(t, "../opt/codex", filepath.Join(dir, "bin/codex"))
				return filepath.Join(dir, "bin/codex"), func() {
					relink(t, "../cellar/0.155.0/codex", filepath.Join(dir, "opt/codex"))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			counter := filepath.Join(dir, "runs")
			exe, upgrade := tc.install(t, dir, counter)

			logs := &syncBuffer{}
			cfg := config.Default()
			cfg.Codex.Executable = exe
			probe, err := resolveCodexClientVersion(context.Background(), &cfg, slog.New(slog.NewJSONHandler(logs, nil)))
			if err != nil {
				t.Fatalf("resolveCodexClientVersion: %v", err)
			}
			if cfg.Codex.ClientVersion != "0.155.0-alpha.9" {
				t.Fatalf("startup version = %q", cfg.Codex.ClientVersion)
			}
			if got := versionRuns(t, counter); got != 1 {
				t.Fatalf("startup ran --version %d times, want 1", got)
			}

			for range 3 {
				if got := probe.ClientVersion(context.Background()); got != "0.155.0-alpha.9" {
					t.Errorf("unchanged executable: version = %q", got)
				}
			}
			if got := versionRuns(t, counter); got != 1 {
				t.Errorf("an unchanged executable ran --version again: %d runs, want 1", got)
			}

			upgrade()
			if got := probe.ClientVersion(context.Background()); got != "0.155.0" {
				t.Errorf("after upgrade: version = %q, want 0.155.0", got)
			}
			if got := probe.ClientVersion(context.Background()); got != "0.155.0" {
				t.Errorf("after upgrade, re-checked: version = %q, want 0.155.0", got)
			}
			if got := versionRuns(t, counter); got != 2 {
				t.Errorf("--version ran %d times, want exactly 2 (startup + one after the upgrade)", got)
			}

			changed := recordsWithMsgPrefix(logRecords(t, logs.String()), "codex client version changed")
			if len(changed) != 1 {
				t.Fatalf("want exactly one version-change log line, got %v", changed)
			}
			if rec := changed[0]; rec["level"] != "INFO" ||
				rec["previous_client_version"] != "0.155.0-alpha.9" || rec["client_version"] != "0.155.0" {
				t.Errorf("version-change log = %v, want INFO naming 0.155.0-alpha.9 -> 0.155.0", rec)
			}
		})
	}
}

// TestCodexVersionProbeKeepsLastGoodVersionOnFailure: once a version is known,
// no runtime discovery failure may take it away. Each failure warns once, and
// a broken executable is not re-run on every check.
func TestCodexVersionProbeKeepsLastGoodVersionOnFailure(t *testing.T) {
	cases := []struct {
		name     string
		breakIt  func(t *testing.T, exe, counter string)
		wantRuns int // total --version runs after two checks of the broken executable
		wantWarn string
	}{
		{
			name: "malformed output",
			breakIt: func(t *testing.T, exe, counter string) {
				installCountingCodex(t, exe, counter, "codex-cli latest")
			},
			wantRuns: 2,
			wantWarn: "codex client version discovery failed",
		},
		{
			name: "executable removed",
			breakIt: func(t *testing.T, exe, _ string) {
				if err := os.Remove(exe); err != nil {
					t.Fatal(err)
				}
			},
			wantRuns: 1,
			wantWarn: "codex client version re-check failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			counter := filepath.Join(dir, "runs")
			exe := filepath.Join(dir, "codex")
			installCountingCodex(t, exe, counter, "codex-cli 0.155.0")

			logs := &syncBuffer{}
			cfg := config.Default()
			cfg.Codex.Executable = exe
			probe, err := resolveCodexClientVersion(context.Background(), &cfg, slog.New(slog.NewJSONHandler(logs, nil)))
			if err != nil {
				t.Fatalf("resolveCodexClientVersion: %v", err)
			}

			tc.breakIt(t, exe, counter)
			for range 2 {
				if got := probe.ClientVersion(context.Background()); got != "0.155.0" {
					t.Errorf("version = %q, want the last good 0.155.0", got)
				}
			}
			if got := versionRuns(t, counter); got != tc.wantRuns {
				t.Errorf("--version ran %d times, want %d", got, tc.wantRuns)
			}
			warns := recordsWithMsgPrefix(logRecords(t, logs.String()), tc.wantWarn)
			if len(warns) != 1 {
				t.Fatalf("want exactly one %q warning, got %v (all logs: %s)", tc.wantWarn, warns, logs.String())
			}
			if warns[0]["level"] != "WARN" || warns[0]["client_version"] != "0.155.0" {
				t.Errorf("warning = %v, want WARN naming the kept version", warns[0])
			}
		})
	}
}

// TestCodexVersionProbeReRunsAfterMaxAge covers the backstop for an install
// whose visible file never changes on upgrade: an unchanged fingerprint is
// trusted for codexVersionMaxAge and no longer.
func TestCodexVersionProbeReRunsAfterMaxAge(t *testing.T) {
	var runs atomic.Int64
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	probe := newCodexVersionProbe("codex", slog.New(slog.DiscardHandler))
	probe.stat = func(string) (exeFingerprint, error) { return exeFingerprint{path: "/bin/codex"}, nil }
	probe.run = func(context.Context, string) (string, error) {
		runs.Add(1)
		return "0.155.0", nil
	}
	probe.now = func() time.Time { return now }

	if _, err := probe.discoverInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(codexVersionMaxAge - time.Second)
	probe.ClientVersion(context.Background())
	if got := runs.Load(); got != 1 {
		t.Errorf("runs within max age = %d, want 1", got)
	}
	now = now.Add(2 * time.Second)
	probe.ClientVersion(context.Background())
	if got := runs.Load(); got != 2 {
		t.Errorf("runs past max age = %d, want 2", got)
	}
}

// TestCodexVersionProbeSerialisesConcurrentChecks: catalog fetches can come
// from a request and a background revalidation at once. A changed executable
// must still be run exactly once, not once per concurrent caller.
func TestCodexVersionProbeSerialisesConcurrentChecks(t *testing.T) {
	var runs atomic.Int64
	var gen atomic.Int64
	probe := newCodexVersionProbe("codex", slog.New(slog.DiscardHandler))
	probe.stat = func(string) (exeFingerprint, error) {
		return exeFingerprint{path: "/bin/codex", ino: uint64(gen.Load())}, nil
	}
	probe.run = func(context.Context, string) (string, error) {
		runs.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the window a racing caller would use
		return fmt.Sprintf("0.155.%d", gen.Load()), nil
	}
	if _, err := probe.discoverInitial(context.Background()); err != nil {
		t.Fatal(err)
	}

	gen.Store(1)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if got := probe.ClientVersion(context.Background()); got != "0.155.1" {
				t.Errorf("version = %q, want 0.155.1", got)
			}
		})
	}
	wg.Wait()
	if got := runs.Load(); got != 2 {
		t.Errorf("--version ran %d times, want 2 (startup + one for the change)", got)
	}
}

// catalogClock is a settable time source safe to read from the catalog's
// background revalidation goroutine.
type catalogClock struct{ ns atomic.Int64 }

func newCatalogClock() *catalogClock {
	c := &catalogClock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}

func (c *catalogClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *catalogClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// TestCodexUpgradeReachesTheNextCatalogFetch drives the probe and the catalog
// together, the way production wires them, through the observed failure: a
// daemon that started on one Codex CLI version keeps running across an
// upgrade. The backend here answers 304 to the old ETag whatever the client
// version, so reusing that validator would keep the old model list — which is
// exactly how the stale list survived. The cases also pin the other two
// outcomes: an unchanged executable keeps revalidating conditionally without
// a subprocess, and a failed re-discovery keeps the last good version and
// still fetches.
func TestCodexUpgradeReachesTheNextCatalogFetch(t *testing.T) {
	cases := []struct {
		name        string
		change      func(t *testing.T, exe, counter string)
		wantVersion string
		wantINM     string
		wantModels  int
		wantRuns    int
		wantLog     string
	}{
		{
			name: "upgraded",
			change: func(t *testing.T, exe, counter string) {
				installCountingCodex(t, exe, counter, "codex-cli 0.155.0")
			},
			wantVersion: "0.155.0", wantINM: "", wantModels: 3, wantRuns: 2,
			wantLog: "codex client version changed",
		},
		{
			name:        "unchanged",
			change:      func(*testing.T, string, string) {},
			wantVersion: "0.155.0-alpha.9", wantINM: `W/"old"`, wantModels: 1, wantRuns: 1,
		},
		{
			name: "re-discovery fails",
			change: func(t *testing.T, exe, counter string) {
				installCountingCodex(t, exe, counter, "not a version")
			},
			wantVersion: "0.155.0-alpha.9", wantINM: `W/"old"`, wantModels: 1, wantRuns: 2,
			wantLog: "codex client version discovery failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			counter := filepath.Join(dir, "runs")
			exe := filepath.Join(dir, "codex")
			installCountingCodex(t, exe, counter, "codex-cli 0.155.0-alpha.9")

			type seen struct{ version, inm string }
			var (
				mu       sync.Mutex
				requests []seen
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				first := len(requests) == 0
				requests = append(requests, seen{r.URL.Query().Get("client_version"), r.Header.Get("If-None-Match")})
				mu.Unlock()
				if r.Header.Get("If-None-Match") == `W/"old"` {
					w.Header().Set("ETag", `W/"old"`)
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if first {
					w.Header().Set("ETag", `W/"old"`)
					_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-5.5","visibility":"list"}]}`)
					return
				}
				w.Header().Set("ETag", `W/"new"`)
				_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-5.5","visibility":"list"},{"slug":"gpt-6-sol","visibility":"list"},{"slug":"gpt-6-luna","visibility":"list"}]}`)
			}))
			defer upstream.Close()

			logs := &syncBuffer{}
			log := slog.New(slog.NewJSONHandler(logs, nil))
			cfg := config.Default()
			cfg.Codex.Executable = exe
			probe, err := resolveCodexClientVersion(context.Background(), &cfg, log)
			if err != nil {
				t.Fatalf("resolveCodexClientVersion: %v", err)
			}

			clk := newCatalogClock()
			cacheFile := filepath.Join(dir, "models_cache.json")
			cat := catalog.New(catalog.Options{
				BaseURL: upstream.URL, HTTPClient: upstream.Client(), CacheFile: cacheFile,
				ClientVersion: cfg.Codex.ClientVersion, ClientVersionFunc: probe.versionFunc(),
				TTL: time.Minute, Now: clk.now, Logger: log,
			})
			cred := auth.Credential{AccessToken: "test-token", AccountID: "test-account"}
			if m, err := cat.Models(context.Background(), cred); err != nil || len(m) != 1 {
				t.Fatalf("first Models = %d models, err %v", len(m), err)
			}

			tc.change(t, exe, counter)
			clk.advance(2 * time.Minute)
			// Stale: served immediately, revalidated in the background.
			if _, err := cat.Models(context.Background(), cred); err != nil {
				t.Fatalf("stale Models: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				mu.Lock()
				n := len(requests)
				mu.Unlock()
				_, age, _ := cat.Snapshot("")
				if n >= 2 && age < time.Minute {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("revalidation never completed (%d requests)", n)
				}
				time.Sleep(5 * time.Millisecond)
			}

			mu.Lock()
			second := requests[1]
			mu.Unlock()
			if second.version != tc.wantVersion {
				t.Errorf("revalidation client_version = %q, want %q", second.version, tc.wantVersion)
			}
			if second.inm != tc.wantINM {
				t.Errorf("revalidation If-None-Match = %q, want %q", second.inm, tc.wantINM)
			}
			if n, _, _ := cat.Snapshot(""); n != tc.wantModels {
				t.Errorf("held models = %d, want %d", n, tc.wantModels)
			}
			if got := cat.ClientVersion(); got != tc.wantVersion {
				t.Errorf("catalog ClientVersion() = %q, want %q", got, tc.wantVersion)
			}
			if got := versionRuns(t, counter); got != tc.wantRuns {
				t.Errorf("--version ran %d times, want %d", got, tc.wantRuns)
			}
			// The disk write follows the in-memory commit, so poll for the one
			// this revalidation made (its fetched_at), not the first fetch's.
			var diskVersion string
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				var disk struct {
					ClientVersion string    `json:"client_version"`
					FetchedAt     time.Time `json:"fetched_at"`
				}
				if raw, err := os.ReadFile(cacheFile); err == nil && json.Unmarshal(raw, &disk) == nil && disk.FetchedAt.Equal(clk.now()) {
					if diskVersion = disk.ClientVersion; diskVersion == tc.wantVersion {
						break
					}
				}
			}
			if diskVersion != tc.wantVersion {
				t.Errorf("disk cache client_version = %q, want %q", diskVersion, tc.wantVersion)
			}
			if tc.wantLog != "" && len(recordsWithMsgPrefix(logRecords(t, logs.String()), tc.wantLog)) != 1 {
				t.Errorf("want one %q log line; logs:\n%s", tc.wantLog, logs.String())
			}
		})
	}
}

// TestExplicitClientVersionOverrideNeverTouchesTheExecutable: an override is
// fixed for the life of the process. Nothing may run the executable — not at
// startup and not before a later fetch — even when that executable changes.
func TestExplicitClientVersionOverrideNeverTouchesTheExecutable(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	exe := filepath.Join(dir, "codex")
	installCountingCodex(t, exe, counter, "codex-cli 0.155.0")

	var (
		mu       sync.Mutex
		versions []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		versions = append(versions, r.URL.Query().Get("client_version"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Codex.Executable = exe
	cfg.Codex.ClientVersion = "0.200.1"
	probe, err := resolveCodexClientVersion(context.Background(), &cfg, nil)
	if err != nil {
		t.Fatalf("resolveCodexClientVersion: %v", err)
	}
	fn := probe.versionFunc()
	if fn != nil {
		t.Fatal("an explicit override must give the catalog no version func to call")
	}

	clk := newCatalogClock()
	cat := catalog.New(catalog.Options{
		BaseURL: upstream.URL, HTTPClient: upstream.Client(),
		ClientVersion: cfg.Codex.ClientVersion, ClientVersionFunc: fn,
		TTL: time.Minute, Now: clk.now, Logger: slog.New(slog.DiscardHandler),
	})
	cred := auth.Credential{AccessToken: "test-token", AccountID: "test-account"}
	if _, err := cat.Models(context.Background(), cred); err != nil {
		t.Fatalf("Models: %v", err)
	}
	installCountingCodex(t, exe, counter, "codex-cli 0.300.0")
	clk.advance(2 * time.Minute)
	if _, err := cat.Models(context.Background(), cred); err != nil {
		t.Fatalf("stale Models: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(versions)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("revalidation never reached the backend")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, v := range versions {
		if v != "0.200.1" {
			t.Errorf("request %d client_version = %q, want the override", i, v)
		}
	}
	if got := versionRuns(t, counter); got != 0 {
		t.Errorf("the override path ran the executable %d times", got)
	}
}

// TestRunRediscoversCodexVersionBeforeALaterCatalogFetch protects the
// production wiring end to end: run must hand the probe to newApp, and newApp
// must hand it to the catalog. Testing the probe and the catalog separately
// would stay green if either link were dropped. The startup warm is answered
// 500, so nothing is held and the next catalog read fetches again at once —
// no clock is needed to make a second fetch happen.
func TestRunRediscoversCodexVersionBeforeALaterCatalogFetch(t *testing.T) {
	// The picker read republishes the process-wide aliases from this fake
	// catalog; put the static seed back so later tests do not route "sol" to it.
	restoreRegistry(t)
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	executable := filepath.Join(dir, "codex")
	installCountingCodex(t, executable, counter, "codex-cli 0.155.0-alpha.9")
	authPath := filepath.Join(dir, "auth.json")
	authBody := fmt.Sprintf(`{"tokens":{"account_id":"test-account","access_token":%q,"refresh_token":"unused"}}`,
		jwtWithExp(time.Now().Add(time.Hour)))
	if err := os.WriteFile(authPath, []byte(authBody), 0o600); err != nil {
		t.Fatalf("write auth fixture: %v", err)
	}

	var catalogReads atomic.Int64
	versionC := make(chan string, 64)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		versionC <- r.URL.Query().Get("client_version")
		if catalogReads.Add(1) == 1 {
			http.Error(w, "warm fails on purpose", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6-sol","visibility":"list"}]}`)
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuffer{}
	env := map[string]string{
		"HOME":                     dir,
		config.EnvListen:           "127.0.0.1:0",
		config.EnvLogFormat:        "json",
		config.EnvAnthropicBaseURL: upstream.URL,
		config.EnvCodexBaseURL:     upstream.URL,
		config.EnvCodexAuthFile:    authPath,
		config.EnvCodexCacheFile:   filepath.Join(dir, "models-cache.json"),
		config.EnvCodexExecutable:  executable,
	}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, func(key string) string { return env[key] }, logs)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("run did not stop after cancellation")
		}
	}()

	select {
	case got := <-versionC:
		if got != "0.155.0-alpha.9" {
			t.Fatalf("startup warm client_version = %q, want 0.155.0-alpha.9", got)
		}
	case err := <-done:
		t.Fatalf("run returned before catalog warm: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("startup catalog warm did not reach fake backend")
	}

	// The daemon's address is only in its "listening" log line.
	var addr string
	for deadline := time.Now().Add(10 * time.Second); addr == ""; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("run never logged its listening address:\n%s", logs.String())
		}
		for _, rec := range recordsWithMsgPrefix(logRecords(t, logs.String()), "listening") {
			addr, _ = rec["addr"].(string)
		}
	}

	installCountingCodex(t, executable, counter, "codex-cli 0.155.0")

	// A picker open reads the catalog. Until the failed warm has been
	// recorded, a read can join it and share its error, so keep opening the
	// picker until a second catalog request reaches the backend.
	var second string
	for deadline := time.Now().Add(10 * time.Second); second == ""; {
		if time.Now().After(deadline) {
			t.Fatal("no catalog read after startup reached the backend")
		}
		resp, err := http.Get("http://" + addr + "/v1/models")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		select {
		case second = <-versionC:
		case <-time.After(50 * time.Millisecond):
		}
	}
	if second != "0.155.0" {
		t.Errorf("client_version after the upgrade = %q, want 0.155.0: the running daemon did not re-resolve it", second)
	}
	if got := versionRuns(t, counter); got != 2 {
		t.Errorf("--version ran %d times, want 2 (startup + once after the upgrade)", got)
	}
	if len(recordsWithMsgPrefix(logRecords(t, logs.String()), "codex client version changed")) != 1 {
		t.Errorf("want one version-change log line; logs:\n%s", logs.String())
	}
}

// --- probe environment ----------------------------------------------------

// probeEnvCanaries are secrets the daemon's own environment can hold, by name
// and value. None may reach the `codex --version` child.
var probeEnvCanaries = map[string]string{
	"DEEPSEEK_API_KEY":         "canary-deepseek-7Hq2",
	"UTRAQUE_LOCAL_TOKEN":      "canary-local-token-9Zr4",
	"ANTHROPIC_CUSTOM_HEADERS": "x-utraque-token: canary-header-3Kp8",
	"OPENAI_API_KEY":           "canary-openai-5Wd1",
	"GITHUB_TOKEN":             "canary-github-2Mx6",
	"NODE_OPTIONS":             "--require=/canary/preload-4Lc9.js",
	// A prefix family is no licence for a credential: mise really does read
	// MISE_GITHUB_TOKEN.
	"MISE_GITHUB_TOKEN": "canary-mise-token-8Nb3",
	"NVM_AUTH_KEY":      "canary-nvm-key-1Qa7",
}

// setProbeEnv gives the test process a realistic daemon environment: the
// canaries above plus a private HOME, a PATH, and version-manager variables
// that must pass through. It returns the values the child should see.
func setProbeEnv(t *testing.T, path string) (home string) {
	t.Helper()
	for k, v := range probeEnvCanaries {
		t.Setenv(k, v)
	}
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", path)
	t.Setenv("VOLTA_HOME", filepath.Join(home, ".volta"))
	t.Setenv("NVM_DIR", filepath.Join(home, ".nvm"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("UNLISTED_SETTING", "canary-unlisted-6Ty5")
	return home
}

// writeEnvDumpingCodex installs a codex that writes its whole environment to
// dump and then answers --version. It uses absolute paths only, so it works
// whatever PATH the child is given.
func writeEnvDumpingCodex(t *testing.T, path, dump, version string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\n/usr/bin/env > %q\nprintf '%%s\\n' %q\n", dump, "codex-cli "+version)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(script), 0o700); err != nil {
		t.Fatalf("write Codex fixture: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("install Codex fixture: %v", err)
	}
}

func readEnvDump(t *testing.T, dump string) (raw string, vars map[string]string) {
	t.Helper()
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the fake codex did not run: %v", err)
	}
	vars = map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vars[k] = v
		}
	}
	return string(b), vars
}

// assertProbeEnv checks one dumped child environment: no canary by name or
// value, and the real HOME, PATH and version-manager settings present.
func assertProbeEnv(t *testing.T, dump, home, path string) {
	t.Helper()
	raw, vars := readEnvDump(t, dump)
	for k, v := range probeEnvCanaries {
		if _, ok := vars[k]; ok {
			t.Errorf("%s reached the codex --version child", k)
		}
		if strings.Contains(raw, v) {
			t.Errorf("the value of %s reached the codex --version child", k)
		}
	}
	if _, ok := vars["UNLISTED_SETTING"]; ok || strings.Contains(raw, "canary-unlisted-6Ty5") {
		t.Error("a variable outside the allow-list reached the child")
	}
	for k, want := range map[string]string{
		"HOME":       home,
		"PATH":       path,
		"VOLTA_HOME": filepath.Join(home, ".volta"),
		"NVM_DIR":    filepath.Join(home, ".nvm"),
		"CODEX_HOME": filepath.Join(home, ".codex"),
	} {
		if got := vars[k]; got != want {
			t.Errorf("child %s = %q, want the daemon's %q", k, got, want)
		}
	}
}

// TestCodexVersionProbeGetsOnlyAllowListedEnvironment: the probe runs a
// user-installed executable, often a Node script behind a version-manager
// shim, so it needs HOME, PATH and the version manager's settings — and
// nothing else from a daemon environment that can hold the local token, a
// DeepSeek key or a header string carrying either.
func TestCodexVersionProbeGetsOnlyAllowListedEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := "/usr/bin:/bin"
	home := setProbeEnv(t, path)
	exe := filepath.Join(dir, "codex")
	dump := filepath.Join(dir, "env-initial")
	writeEnvDumpingCodex(t, exe, dump, "0.160.0")

	cfg := config.Default()
	cfg.Codex.Executable = exe
	probe, err := resolveCodexClientVersion(context.Background(), &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("resolveCodexClientVersion: %v", err)
	}
	if cfg.Codex.ClientVersion != "0.160.0" {
		t.Fatalf("ClientVersion = %q", cfg.Codex.ClientVersion)
	}
	assertProbeEnv(t, dump, home, path)

	// The runtime re-probe after an upgrade spawns the same way.
	redump := filepath.Join(dir, "env-reprobe")
	writeEnvDumpingCodex(t, exe, redump, "0.161.0")
	if got := probe.ClientVersion(context.Background()); got != "0.161.0" {
		t.Fatalf("re-probed version = %q, want 0.161.0", got)
	}
	assertProbeEnv(t, redump, home, path)
}

func TestCodexProbeEnvFilter(t *testing.T) {
	in := []string{
		"HOME=/h", "PATH=/p", "USER=u", "LOGNAME=u", "SHELL=/bin/zsh", "TMPDIR=/t",
		"LANG=C", "LC_ALL=C", "LC_CTYPE=UTF-8", "CODEX_HOME=/c",
		"XDG_CONFIG_HOME=/x1", "XDG_DATA_HOME=/x2", "XDG_CACHE_HOME=/x3", "XDG_STATE_HOME=/x4",
		"SSL_CERT_FILE=/s1", "SSL_CERT_DIR=/s2",
		"NVM_DIR=/n", "NVM_BIN=/nb", "VOLTA_HOME=/v", "FNM_DIR=/f", "FNM_MULTISHELL_PATH=/fm",
		"ASDF_DIR=/a", "ASDF_DATA_DIR=/ad", "MISE_DATA_DIR=/m", "NODENV_ROOT=/nr",
		"N_PREFIX=/np", "PNPM_HOME=/pn", "BUN_INSTALL=/b", "NODE_PATH=/nm",
		// Dropped: secrets, utraque's and the providers' own settings, code
		// injection, near-misses on the allow-list, and malformed entries.
		"UTRAQUE_LOCAL_TOKEN=x", "UTRAQUE_LISTEN=x", "DEEPSEEK_API_KEY=x", "OPENAI_API_KEY=x",
		"OPENAI_BASE_URL=x", "ANTHROPIC_CUSTOM_HEADERS=x", "ANTHROPIC_BASE_URL=x",
		"GITHUB_TOKEN=x", "AWS_SECRET_ACCESS_KEY=x", "NPM_TOKEN=x", "NODE_OPTIONS=x",
		"MISE_GITHUB_TOKEN=x", "NVM_AUTH_KEY=x", "VOLTA_SECRET=x", "ASDF_PASSWORD=x",
		"NVMRC=x", "NVM=x", "VOLTA=x", "N_PREFIXES=x", "NODE_PATHS=x", "PNPM_HOME2=x",
		"XDG_RUNTIME_DIR=x", "LC_MESSAGES=x", "HOMEBREW_PREFIX=x", "HTTPS_PROXY=x",
		"DYLD_INSERT_LIBRARIES=x", "LD_PRELOAD=x", "BASH_ENV=x", "ENV=x",
		"home=/lower", "NOEQUALS", "=leading",
	}
	got := codexProbeEnv(in)
	want := in[:29]
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("codexProbeEnv kept\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestCodexVersionProbeRunsAVersionManagerShim: the allow-list must still let
// a real install work. The shim found on PATH execs a script under HOME whose
// `#!/usr/bin/env fakenode` line finds its interpreter through PATH again,
// the way an npm-installed codex behind nvm, Volta or fnm runs.
func TestCodexVersionProbeRunsAVersionManagerShim(t *testing.T) {
	dir := t.TempDir()
	shims := filepath.Join(dir, "shims")
	nodeBin := filepath.Join(dir, "node", "bin")
	for _, d := range []string{shims, nodeBin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := shims + ":" + nodeBin + ":/usr/bin:/bin"
	home := setProbeEnv(t, path)
	toolchain := filepath.Join(home, ".volta", "tools", "codex")
	if err := os.MkdirAll(toolchain, 0o700); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{
		filepath.Join(shims, "codex"): "#!/bin/sh\n" +
			"[ -n \"$VOLTA_HOME\" ] || { echo 'shim: VOLTA_HOME unset' >&2; exit 3; }\n" +
			"exec \"$HOME/.volta/tools/codex/codex.js\" \"$@\"\n",
		filepath.Join(toolchain, "codex.js"): "#!/usr/bin/env fakenode\n",
		filepath.Join(nodeBin, "fakenode"): "#!/bin/sh\n" +
			"[ \"$2\" = --version ] || exit 4\n" +
			"printf '%s\\n' 'codex-cli 0.162.1'\n",
	} {
		if err := os.WriteFile(file, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Default()
	cfg.Codex.Executable = "codex"
	if _, err := resolveCodexClientVersion(context.Background(), &cfg, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("resolveCodexClientVersion through a shim: %v", err)
	}
	if cfg.Codex.ClientVersion != "0.162.1" {
		t.Errorf("ClientVersion = %q, want 0.162.1", cfg.Codex.ClientVersion)
	}
}

// TestCodexVersionProbeFailureNeverQuotesChildOutput: a broken or wrapped
// codex can print anything, including an environment dump or a token, and the
// startup failure goes to stderr and launchd's log while the runtime failure
// goes to the log. Both describe the output's shape, never its text, and the
// startup failure names both escape hatches.
func TestCodexVersionProbeFailureNeverQuotesChildOutput(t *testing.T) {
	const canary = "sk-CANARYprobe0utput"
	for name, output := range map[string]string{
		"extra field":      "codex-cli 0.163.0 " + canary,
		"token as version": "codex-cli " + canary,
		"wrong tool":       canary + " 0.163.0",
		"env dump":         "OPENAI_API_KEY=" + canary + "\nHOME=/x",
		"control bytes":    "codex-cli \x1b[31m" + canary,
	} {
		t.Run(name, func(t *testing.T) {
			exe := writeCodexVersionFixture(t, fmt.Sprintf("printf '%%s\\n' %q\nprintf '%%s\\n' %q >&2\n", output, canary))
			cfg := config.Default()
			cfg.Codex.Executable = exe
			_, err := resolveCodexClientVersion(context.Background(), &cfg, slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("malformed output accepted")
			}
			msg := err.Error()
			if strings.Contains(msg, "CANARY") || strings.Contains(msg, "\x1b") {
				t.Errorf("startup error quotes the child's output: %q", msg)
			}
			for _, hatch := range []string{config.EnvCodexClientVersion, config.EnvCodexExecutable} {
				if !strings.Contains(msg, hatch) {
					t.Errorf("startup error %q does not name %s", msg, hatch)
				}
			}
		})
	}

	t.Run("runtime re-probe", func(t *testing.T) {
		dir := t.TempDir()
		counter := filepath.Join(dir, "runs")
		exe := filepath.Join(dir, "codex")
		installCountingCodex(t, exe, counter, "codex-cli 0.163.0")
		logs := &syncBuffer{}
		cfg := config.Default()
		cfg.Codex.Executable = exe
		probe, err := resolveCodexClientVersion(context.Background(), &cfg, slog.New(slog.NewJSONHandler(logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		installCountingCodex(t, exe, counter, "codex-cli "+canary)
		if got := probe.ClientVersion(context.Background()); got != "0.163.0" {
			t.Errorf("version = %q, want the last good one", got)
		}
		if !strings.Contains(logs.String(), "codex client version discovery failed") {
			t.Fatalf("no failure warning logged: %s", logs.String())
		}
		if strings.Contains(logs.String(), "CANARY") {
			t.Errorf("runtime warning quotes the child's output: %s", logs.String())
		}
	})
}
