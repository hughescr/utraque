package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/hughescr/utraque/internal/config"
)

const (
	codexVersionTimeout = 5 * time.Second
	codexVersionMaxText = 4 << 10
)

var codexSemver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)

// resolveCodexClientVersion fills the catalog's required client_version from
// the configured Codex executable. An explicit environment/config override
// wins and avoids starting a subprocess.
func resolveCodexClientVersion(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("discover Codex client version: nil config")
	}
	if cfg.Codex.ClientVersion != "" {
		return nil
	}

	version, err := discoverCodexClientVersion(ctx, cfg.Codex.Executable)
	if err != nil {
		return fmt.Errorf("discover Codex client version with %s --version: %w; install/configure Codex or set %s explicitly",
			config.EnvCodexExecutable, err, config.EnvCodexClientVersion)
	}
	cfg.Codex.ClientVersion = version
	return nil
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
