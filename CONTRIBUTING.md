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

## Before opening a pull request

```sh
gofmt -l ./cmd ./internal     # must print nothing
go vet ./...
go test -race ./...
```

CI runs exactly these on macOS and Linux, plus `shellcheck` over the deploy
scripts. If you touch `deploy/*.sh`, run `shellcheck` locally too.

## The test suite contacts nothing

`go test ./...` is hermetic by design: every upstream is an `httptest` server
and every credential is a throwaway written under `t.TempDir()`. The real
`chatgpt.com`, `auth.openai.com`, `api.anthropic.com` and your own
`~/.codex/auth.json` are never read, written, or contacted.

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
before: they did not carry the multi-block system array and mid-conversation
system messages that Claude Code sends, and the translator was broken for
months without any test noticing.

## Logging

Redaction is by allowlist — only named headers may be logged, tokens never in
any form, and `account_id` only as a hash prefix. There is a test that drives
the production logger at `debug` and asserts no token-shaped material reaches a
log line. Do not add a log statement that prints a whole request or header map.

## Reporting bugs

Include the `/healthz` output with any token values removed, the redacted
request line from the log, and what you expected instead. If it involves the
Codex leg, say which model name you used and whether it was typed, set in agent
frontmatter, or chosen from the picker.
