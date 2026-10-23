# Contributing

Thanks for looking. `utraque` is a small, single-purpose proxy, and the bar for
changes is mostly about not breaking two things that are hard to debug from a
bug report: the credential handling and the streaming translation.

## Getting set up

```sh
git clone https://github.com/hughescr/utraque.git
cd utraque
go build -o bin/utraque ./cmd/utraque
go test ./...
```

Go 1.27. On macOS, the launchd path also needs cgo (the default) and the Xcode
Command Line Tools.

The complete validator also needs Git, Bash, and ShellCheck. Install ShellCheck
with `brew install shellcheck` on macOS or
`sudo apt-get install shellcheck` on Ubuntu. Race-enabled cgo builds require a
native C toolchain: Xcode Command Line Tools on macOS or `build-essential` on
Ubuntu.

## Before opening a pull request

```sh
./scripts/validate.sh
```

The script locates the repository from its own path, so it works from any
directory. With no argument (or with `all`) it runs the `go`, `shell`, and
`hooks` groups in that order and exits on the first failure. Unknown groups and
extra arguments are errors, and a missing required tool fails instead of
silently skipping its check. CI runs `go` on both macOS and Linux; its Ubuntu
shell job runs `shell` and `hooks`.

The `go` group checks every recursive `*.go` file except files under `.git` or
`vendor`, forces `CGO_ENABLED=1`, then vets, builds `./cmd/utraque` into a
disposable output, and runs ordinary and race-enabled tests. It never formats
source or replaces `bin/utraque`. The `shell` group recursively finds `*.sh`
under `deploy`, `scripts`, and `tests`, plus every file under `.githooks`; it
runs `bash -n` separately on each file before ShellCheck. The `hooks` group
exercises real pushes against disposable local repositories and bare remotes.

To enable the version-controlled pre-push hook in this repository, run:

```sh
./scripts/install-hooks.sh
```

The installer refuses to replace another configured hooks path or any
executable hook already present under `.git/hooks`. `core.hooksPath` is stored
in the repository's shared config, so the installer also refuses a repository
with linked worktrees; integrate those worktrees' hooks manually instead. The
hook accepts only a clean, fully tracked checkout of `HEAD`, then runs the
complete validator before Git updates the remote. Check `git status` yourself
before running the validator directly; the clean-HEAD rule is enforced by the
pre-push hook, not by `validate.sh`.

## The test suite contacts nothing

`go test ./...` is hermetic by design: every upstream is an `httptest` server
and every credential is a throwaway written under `t.TempDir()`. The real
`chatgpt.com`, `auth.openai.com`, `api.anthropic.com`, `api.deepseek.com` and
your own `~/.codex/auth.json` are never read, written, or contacted.

The tests that *do* talk to the real backends are behind a build tag and need
your own subscription credentials:

```sh
go test -tags live -run TestLiveContract ./...
```

CI never runs those. Run them yourself when you change the Codex leg, since
they are the tripwire for upstream protocol drift.

## Two areas that need extra care

**`internal/codex/auth`.** This reads, refreshes, and writes back the Codex
CLI's own `auth.json`. Getting it wrong logs a user out of their own Codex CLI.
Any change here must preserve the flock, the re-read under lock, the atomic
temp-write plus rename, and the passthrough of unknown JSON keys. There are
tests for cross-process and concurrent refresh; keep them passing.

**`internal/translate/stream`.** A state machine converting Codex SSE into
Anthropic SSE incrementally. It is covered by golden files, a grammar-invariant
checker, a prefix-truncation property test (every prefix of every input stream
must still produce valid output), and a sink-equivalence test asserting the
streaming and aggregating sinks never diverge. New event handling should come
with a fixture.

If you add a request-translation case, build the fixture from a request the
real client actually sent. Hand-written payloads have hidden a real bug here
before: they did not carry the mid-conversation system messages Claude Code
sends, so the translator shipped broken and the whole GPT leg failed on the
first request from a real client, with a green test suite.

Trace dumps are source material, not fixtures to copy whole. Promote the
`body` from `<id>.request.json` to a request corpus
`<case>.anthropic.input.json` input and its translated result to
`<case>.responses.golden.json`; map `<id>.upstream.sse` to a stream corpus
`<case>.responses.input.sse` input and `<id>.downstream.sse` to its
`<case>.anthropic.golden.sse` golden. Remove trace metadata and any sensitive prompt
material before committing.

## Logging

Redaction is by allowlist — only named headers may be logged, tokens never in
any form, and `account_id` only as a hash prefix. There is a test that drives
the production logger at `debug` and asserts no token-shaped material reaches a
log line. Do not add a log statement that prints a whole request or header map.

## Releasing

`cmd/utraque/main.go` defines `const releaseVersion`. A build from a git
checkout self-stamps that with the revision (see the README's *Version
stamping*), so cutting a release is just:

1. Bump `releaseVersion` in `cmd/utraque/main.go`.
2. Commit it.
3. Tag `vX.Y.Z` — Go module tags carry the `v` prefix even though
   `releaseVersion` itself does not.
4. Push the tag.

## Reporting bugs

Include the `/healthz` output with any token values removed, the redacted
request line from the log, and what you expected instead. If it involves the
Codex leg, say which model name you used and whether it was typed, set in agent
frontmatter, or chosen from the picker.
