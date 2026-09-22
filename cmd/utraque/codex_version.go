package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hughescr/utraque/internal/codex/catalog"
	"github.com/hughescr/utraque/internal/config"
)

const (
	codexVersionTimeout = 5 * time.Second
	codexVersionMaxText = 4 << 10
	// codexVersionMaxAge bounds how long an unchanged executable fingerprint
	// is trusted before `--version` is run again anyway. The fingerprint is
	// the primary signal; this is the backstop for an install whose visible
	// file does not change on upgrade (a wrapper script that execs a package
	// runner, say). It is well above the catalog TTL on purpose: at the TTL
	// every revalidation would spawn a subprocess and the fingerprint would
	// save nothing.
	codexVersionMaxAge = time.Hour
)

var codexSemver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)

// resolveCodexClientVersion fills the catalog's required client_version from
// the configured Codex executable. An explicit environment/config override
// wins and avoids starting a subprocess.
//
// The returned probe is how the catalog keeps the version current after
// startup: it is nil when the version is an explicit override (which never
// changes and never touches the executable), and otherwise already holds the
// version just discovered, which is also written into cfg.Codex.ClientVersion
// as the catalog's seed. The nil-ness of the probe, not cfg, is what records
// whether the version was explicit: after this call both look the same in cfg.
func resolveCodexClientVersion(ctx context.Context, cfg *config.Config, log *slog.Logger) (*codexVersionProbe, error) {
	if cfg == nil {
		return nil, errors.New("discover Codex client version: nil config")
	}
	if cfg.Codex.ClientVersion != "" {
		return nil, nil
	}

	probe := newCodexVersionProbe(cfg.Codex.Executable, log)
	version, err := probe.discoverInitial(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover Codex client version with %s --version: %w; install/configure Codex or set %s explicitly",
			config.EnvCodexExecutable, err, config.EnvCodexClientVersion)
	}
	cfg.Codex.ClientVersion = version
	return probe, nil
}

// codexVersionProbe re-resolves the Codex CLI version while utraque runs.
//
// Discovery used to happen once, at startup, so a Codex CLI upgraded under a
// long-running daemon kept being reported to the catalog as the old version,
// and the backend kept withholding the models gated on the new one. Running
// `codex --version` before every catalog fetch would fix that at the price of
// a subprocess per fetch, so the probe fingerprints the executable instead —
// resolved path, symlink target, size, mtime, device and inode — and only
// re-runs `--version` when that changes (or codexVersionMaxAge passes).
//
// One mutex serialises the whole check, subprocess included, so concurrent
// callers never spawn duplicate `--version` runs: a caller that waited finds
// the fingerprint already refreshed and returns at the cost of one stat.
type codexVersionProbe struct {
	executable string
	log        *slog.Logger
	// run and stat are the probe's two contacts with the system; tests swap
	// them to count invocations or to fail on demand.
	run  func(ctx context.Context, executable string) (string, error)
	stat func(executable string) (exeFingerprint, error)
	now  func() time.Time

	mu          sync.Mutex
	version     string         // last good version; never cleared by a failure
	fp          exeFingerprint // fingerprint of the last --version attempt
	probedAt    time.Time      // when that attempt ran
	statFailing bool           // the last stat failed; warn once per streak
}

func newCodexVersionProbe(executable string, log *slog.Logger) *codexVersionProbe {
	if log == nil {
		log = slog.Default()
	}
	return &codexVersionProbe{
		executable: executable,
		log:        log,
		run:        discoverCodexClientVersion,
		stat:       fingerprintExecutable,
		now:        time.Now,
	}
}

// versionFunc adapts the probe to the catalog's version source. A nil probe
// (the explicit-override case) yields a nil func, so the catalog keeps its
// fixed version and nothing is ever stat'ed or run.
func (p *codexVersionProbe) versionFunc() catalog.ClientVersionFunc {
	if p == nil {
		return nil
	}
	return p.ClientVersion
}

// discoverInitial runs the startup discovery. Unlike ClientVersion it
// reports failure, because with no version at all there is nothing to fall
// back to and startup should say so.
func (p *codexVersionProbe) discoverInitial(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A stat failure is not fatal here: running the executable is the real
	// test, and it produces the better error. The zero fingerprint just means
	// the first runtime check re-runs --version once.
	fp, _ := p.stat(p.executable)
	version, err := p.run(ctx, p.executable)
	if err != nil {
		return "", err
	}
	p.version, p.fp, p.probedAt = version, fp, p.now()
	return version, nil
}

// ClientVersion returns the Codex client version the next catalog fetch should
// send. It is a catalog.ClientVersionFunc: cheap when the executable is
// unchanged (one stat), and it never fails — a discovery failure keeps the
// last good version and logs a warning.
func (p *codexVersionProbe) ClientVersion(ctx context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	fp, err := p.stat(p.executable)
	if err != nil {
		// Mid-upgrade the executable can briefly vanish. Keep what worked and
		// let the next fetch try again, warning once rather than per fetch.
		if !p.statFailing {
			p.log.Warn("codex client version re-check failed; keeping the last good version",
				slog.String("client_version", p.version),
				slog.String("err", err.Error()))
		}
		p.statFailing = true
		return p.version
	}
	p.statFailing = false
	if fp == p.fp && p.now().Sub(p.probedAt) < codexVersionMaxAge {
		return p.version
	}

	version, err := p.run(ctx, fp.path)
	if err != nil {
		if ctx.Err() != nil {
			// The caller gave up; that says nothing about the executable, so do
			// not record this attempt against its fingerprint.
			return p.version
		}
		// Recording the fingerprint means a broken executable is retried when
		// it changes (or the max age passes), not on every fetch, so neither
		// the subprocess nor this warning repeats per revalidation.
		p.fp, p.probedAt = fp, p.now()
		p.log.Warn("codex client version discovery failed; keeping the last good version",
			slog.String("client_version", p.version),
			slog.String("err", err.Error()))
		return p.version
	}
	p.fp, p.probedAt = fp, p.now()
	if version != p.version {
		p.log.Info("codex client version changed; the model catalog will be re-fetched with it",
			slog.String("previous_client_version", p.version),
			slog.String("client_version", version))
		p.version = version
	}
	return p.version
}

// exeFingerprint identifies one installed executable file cheaply enough to
// check before every catalog fetch. Homebrew and npm install Codex behind a
// symlink chain that an upgrade re-points, so the resolved target is part of
// it; size, mtime and device/inode catch a file replaced in place (npm
// extracts fresh files with a fixed mtime, so the inode is what moves there).
// Every field is comparable, so == is the whole comparison.
type exeFingerprint struct {
	path    string // the executable as found on PATH, when given a bare name
	target  string // path with every symlink resolved
	size    int64
	modTime int64 // Unix nanoseconds, so == compares instants
	dev     uint64
	ino     uint64
}

func fingerprintExecutable(executable string) (exeFingerprint, error) {
	path := executable
	if !strings.ContainsRune(executable, filepath.Separator) {
		found, err := exec.LookPath(executable)
		if err != nil {
			return exeFingerprint{}, err
		}
		path = found
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return exeFingerprint{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return exeFingerprint{}, err
	}
	fp := exeFingerprint{
		path:    path,
		target:  target,
		size:    info.Size(),
		modTime: info.ModTime().UnixNano(),
	}
	fp.dev, fp.ino = fileIdentity(info)
	return fp, nil
}

func discoverCodexClientVersion(ctx context.Context, executable string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, codexVersionTimeout)
	defer cancel()

	var stdout, stderr boundedBuffer
	stdout.max = codexVersionMaxText
	stderr.max = codexVersionMaxText
	cmd := exec.CommandContext(runCtx, executable, "--version")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	if runCtx.Err() != nil {
		return "", fmt.Errorf("version command: %w", runCtx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("version command failed: %w", err)
	}
	if stdout.truncated {
		return "", fmt.Errorf("version command produced more than %d bytes on stdout", codexVersionMaxText)
	}
	return parseCodexVersion(stdout.String())
}

func parseCodexVersion(output string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[0] != "codex-cli" || !codexSemver.MatchString(fields[1]) {
		return "", fmt.Errorf("unexpected version output %q; want %q", strings.TrimSpace(output), "codex-cli <semantic-version>")
	}
	return fields[1], nil
}

// boundedBuffer drains subprocess output without allowing a broken executable
// to grow memory without limit. It intentionally reports successful writes so
// the process remains drainable until it exits or the context kills it.
type boundedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.max - b.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return n, nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }
