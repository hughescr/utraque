# utraque

A local HTTP proxy that lets one Claude Code session use a Claude Max
subscription for Anthropic models and a ChatGPT/Codex subscription for OpenAI
GPT models, side by side.

*One communicant, both subscriptions.*

## What it is

`utraque` is a local HTTP proxy you point Claude Code at. It routes by model
name: pick an Anthropic model and the request goes to `api.anthropic.com`
billed against your Claude Max subscription; pick an OpenAI GPT model
(native names `sol`, `terra`, `luna`) and the request goes to OpenAI billed
against your ChatGPT/Codex subscription. Both legs are addressed by native
model names inside a single Claude Code session — no metered pay-per-token
API key is used on either leg.

## Status

Under active development. Honestly, right now:

- The Anthropic passthrough and router foundation work: Claude Code can be
  pointed at `utraque` today and it behaves exactly as if talking to
  `api.anthropic.com` directly.
- The Codex/GPT leg is **not yet implemented**. Requests that route to a GPT
  model currently get a `503` stub response.

The Codex leg is being built out in phases — Codex auth handling, the model
catalog, request translation, and finally the streaming translator that turns
OpenAI's Responses API into Anthropic-shaped SSE. See the project's internal
plan for the full phased roadmap; each phase is intended to be independently
useful, ending with unattended operation via launchd.

## How it works / credentials

**Anthropic leg (working today).** This is a transparent passthrough.
Claude Code sends its own Claude Max subscription OAuth credential on every
request; `utraque` forwards that request byte-for-byte to
`api.anthropic.com`, including the `Authorization` header and any repeated
`anthropic-beta` headers. The proxy stores no Anthropic secret of its own —
the credential lives entirely in the client and passes through untouched.

**Codex/GPT leg (in progress).** Rather than a metered API key, this leg
reads the Codex CLI's own login token from `~/.codex/auth.json`, refreshes it
when it's near expiry (writing the refreshed token back to that file safely,
so it never clobbers the Codex CLI's own state), and uses it to call OpenAI's
backend on your behalf. This bills usage against your ChatGPT/Codex
subscription, not a separate API key.

## Install & run

Requires Go 1.27.

```
go build ./...
```

Run the resulting `utraque` binary, then point Claude Code at it by setting
`ANTHROPIC_BASE_URL` to the proxy's listen address. With no Anthropic API key
set in the environment, Claude Code's own Max subscription OAuth credential
remains the active credential and is forwarded as described above.

Unattended operation — installing `utraque` as a macOS launchd service with
socket activation and idle self-exit, so it starts on first connection and
shuts itself down after a period of inactivity — is planned but not yet
implemented. For now, run the binary directly in a terminal or your own
process supervisor.

## Configuration

Configuration is via `UTRAQUE_`-prefixed environment variables (a TOML file
is also planned). Knobs that exist today:

- **Listen address** — the host:port `utraque` binds to.
- **Local token** — an optional shared secret required on incoming requests,
  recommended on, since any local process could otherwise spend both
  subscriptions through the loopback port.
- **Anthropic base URL** — the upstream Anthropic endpoint the passthrough
  leg forwards to (defaults to `api.anthropic.com`).
- **Limits** — request body size and similar guardrails enforced by the
  server middleware.
- **Idle timeout** — how long the process may sit idle before self-exiting
  (relevant once launchd socket activation lands).

## A note on terms of service

The Anthropic leg follows Anthropic's own documented gateway behavior: a
saved Claude Code login remains the active credential when the client is
pointed at a custom base URL, so its usage limits and billing apply as
normal. This half is officially sanctioned.

The Codex/GPT leg is different. It uses an undocumented endpoint via the
Codex CLI's own login token, replaying the same requests the Codex CLI
itself would make. This is not part of any published API and OpenAI could
change or restrict it without notice. It is intended for personal use of
your own subscription, not for pooling or reselling access. Use it with that
understanding.

## License

Apache-2.0. See [LICENSE.md](LICENSE.md) and [NOTICE](NOTICE).

## On the name

*Utraque* is Latin, from *sub utraque specie* — "under both kinds." It was
the rallying phrase of the **Utraquists**, a movement within the Hussite
reform in 15th-century Bohemia who insisted that lay communicants receive
the Eucharist in both kinds: not just the bread, as the Catholic Church of
the time gave the laity, but the wine as well, chalice included. The chalice
became their emblem, and the demand for communion *sub utraque specie* was
written into the Four Articles of Prague in 1420, one of the founding
documents of the wider Hussite schism that followed the execution of Jan
Hus.

The allusion is meant fairly literally: one communicant — you, the user of a
single Claude Code session — receiving in both kinds, both subscriptions,
neither one withheld.
