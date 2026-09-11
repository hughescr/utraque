# Security

## Reporting a vulnerability

Please report security issues privately through GitHub's **Report a
vulnerability** button on the [Security
tab](https://github.com/hughescr/utraque/security/advisories/new), not as a
public issue.

Include what an attacker could reach and how to reproduce it. Please do not
include real tokens in the report — a redacted excerpt is enough.

## What this project handles

`utraque` sits in front of two paid subscriptions and, when configured, a
prepaid DeepSeek API balance, so the interesting surface is credential
handling:

- **Anthropic credential.** Never stored. The client sends its own OAuth
  credential on every request and the proxy forwards it unchanged. There is no
  Anthropic secret at rest anywhere in this project.
- **Codex credential.** Read from the Codex CLI's `auth.json`, refreshed when
  near expiry, and written back with a lock and an atomic rename so the Codex
  CLI's own state is never clobbered.
- **DeepSeek credential.** Owned by the proxy, from `DEEPSEEK_API_KEY` or the
  file named by `UTRAQUE_DEEPSEEK_API_KEY_FILE` (the explicit file wins). The
  key is sent only to the configured DeepSeek inference endpoint and the
  account-balance endpoint derived from the same base URL. Caller credentials
  are stripped before inference, and neither client follows redirects with the
  DeepSeek key attached.
- **Logging.** Redaction is by allowlist. Tokens are never logged in any form;
  `account_id` appears only as a hash prefix. A test drives the production
  logger at `debug` level and fails if token-shaped material reaches a log line.

## The loopback port is the real exposure

By default `utraque` listens on `127.0.0.1` with no authentication, and any
local process that can reach that port can spend the configured Codex
subscription or DeepSeek prepaid balance. Anthropic forwarding still requires
the caller to supply its own usable Anthropic bearer credential.

An unauthenticated loopback process can also call `/v1/utraque/providers`. Its
response can disclose locally recorded model and token history and the live
quota or balance data available from credentials supplied to the request or
configured in the process. A missing credential or unavailable source produces
an unavailable or partial section; it does not expose data that utraque could
not collect.

This is a deliberate default for a single-user machine, not a claim that it is
safe everywhere.

Set `UTRAQUE_LOCAL_TOKEN` to require a shared secret in an `X-Utraque-Token`
header on every route except `/healthz`, and configure the client to send it
(`ANTHROPIC_CUSTOM_HEADERS` for Claude Code). Do not expose the listener beyond
loopback. The provider-report endpoint rejects non-loopback callers even when
the listener and shared secret permit other routes from the network.

## Out of scope

The Codex backend is undocumented and may change or restrict access without
notice. Breakage caused by an upstream change is a bug, but not a security
vulnerability.
