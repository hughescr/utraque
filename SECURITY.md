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
- **Codex credential.** Read from the Codex CLI's `auth.json` and refreshed when
  near expiry. The refreshed file is written to a temporary file in the same
  directory and renamed over the original, under utraque's own lock file
  (`auth.json.lock`), keeping the file's unknown keys, mode and group (if the
  group cannot be kept, the group and other bits are narrowed so no new group
  gains access). A writer that does not take that lock is detected
  best-effort, by a changed size, modification time or (on macOS and Linux)
  file identity, and utraque then declines to overwrite its write; a small
  window remains between that last check and the rename. The same comparison
  decides whether the cached login is still the file's: on macOS and Linux a
  replaced file is always noticed, while an in-place rewrite that keeps both
  size and modification time is not. When the mode permits group or other read or write
  access, utraque logs a warning when it first sees that mode (and again if
  the mode changes) and `/healthz` reports `mode_warning`; run `chmod 600` on
  the file. Each accepted request uses one credential snapshot from lookup to
  send. After a rejection that calls for a credential refresh (a 401; a
  challenge page with status 401 is a 502 and refreshes nothing) it is retried
  once, and only with a new token for the same account; if the stored login now
  belongs to a different account (you ran `codex login` mid-request), the
  request is not re-sent (its first attempt went out under the original login
  and was rejected) and the client gets an HTTP 400 `invalid_request_error`
  asking it to resubmit. What is guaranteed is that once a request has a login
  it is never re-sent under a different account, so a prompt accepted under one
  account is never re-sent under another account's billing and retention. What
  is not guaranteed is that a request starting at the very moment of a login
  switch uses the new login: it uses the login utraque read for it, which during
  a switch can be the previous one (a credential load already in progress is
  shared with requests that arrive while it runs). The model catalog, its
  on-disk cache, and the quota and catalog status on `/healthz` are tagged with
  the account that fetched them (the cache holds a SHA-256 derived from the
  account id, not the id) and are not reused for another account; the new
  account's list is fetched by the next request that actually consults the
  catalog under that login (a `count_tokens` call or an unroutable model name
  does not). Short model names published from the previous account's catalog
  keep routing after a switch: the bare names are replaced when a non-empty list
  for the new account is published, and the decorated picker names
  (`anthropic-compat.` rows) are replaced or dropped when the picker is next
  opened. A successful but empty list publishes no names. Such a request is sent
  under the current login, and the upstream rejects a model that account lacks.
  A file read mid-rewrite authorises nothing.
- **DeepSeek credential.** Owned by the proxy, from `DEEPSEEK_API_KEY` or the
  file named by `UTRAQUE_DEEPSEEK_API_KEY_FILE` (the explicit file wins). The
  key is sent only to the configured DeepSeek inference endpoint and the
  account-balance endpoint derived from the same base URL. Caller credentials
  are stripped before inference, and neither client follows redirects with the
  DeepSeek key attached. No child process receives it: `codex --version`, the
  Codex app-server query and `ccusage` each run with an explicit environment
  rather than utraque's own.
- **Upstream endpoints.** Every URL a credential is sent to
  (`UTRAQUE_ANTHROPIC_BASE_URL`, `UTRAQUE_DEEPSEEK_BASE_URL`,
  `UTRAQUE_CODEX_BASE_URL`, `UTRAQUE_CODEX_TOKEN_URL`) must be `https`, or plain
  `http` on `127.0.0.0/8`, `[::1]` or `localhost`. Startup fails otherwise,
  naming the variable and never echoing the value.
- **Reserved headers.** The `X-Utraque-*` namespace belongs to the proxy: it is
  never forwarded upstream, and an upstream-supplied header in it never reaches
  the client, on every leg.
- **Logging.** Request headers are logged by allowlist; every other logged
  string is scrubbed for credential shapes. Tokens are never logged in any form;
  a logged `account_id` attribute is only a hash prefix. A test drives the
  production logger at `debug` level and fails if token-shaped material reaches
  a log line.
  Normal logs carry request metadata, not body content. Debug-level records add
  diagnostic metadata taken from the request (tool names, schema paths), and a
  request's error summary can carry scrubbed, upstream-derived text. Opt-in
  trace files (`UTRAQUE_TRACE_DIR`) hold the whole conversation, scrubbed only
  for credential shapes.
- **Upstream error text.** Text from an upstream error reaches the client and
  the log only after the leg's own credential (and, for Codex, the account id)
  is removed, credential-shaped text is scrubbed, and the upstream detail is
  capped at 512 runes (the cap covers that detail, not the fixed text utraque
  renders around it). Exact-match removal applies to credential values of at
  least 4 bytes. That holds for HTTP error responses and for errors inside an
  already-open stream: Codex `response.failed` and `error` events, and a
  DeepSeek SSE `error` event, which is rebuilt with only its type and message.
  A DeepSeek non-streaming `2xx` response whose JSON body is itself an error
  object is rebuilt the same way, with its status unchanged. A message ending
  in the first 16 or more bytes of the credential has that fragment removed.
  Known limits:
  - It covers error messages and logs. A successful completion is relayed
    unscrubbed, and an opt-in trace is scrubbed for credential shapes only.
  - Removal of a known credential matches its exact text; a transformed or
    encoded copy is caught only if it has a credential's shape.

## The loopback port is the real exposure

By default `utraque` listens on `127.0.0.1` with no authentication, and any
local process that can reach that port can spend the configured Codex
subscription or DeepSeek prepaid balance. Anthropic forwarding still requires
the caller to supply its own usable Anthropic bearer credential. Local
processes are trusted by default; a web page in your browser is not.

Every route refuses a request that carries an `Origin` header, or a
`Sec-Fetch-Site` other than `same-origin` or `none`, with a 403
`permission_error`. Unless the request arrived on a listener known to be
non-loopback, the `Host` header must also be
`localhost`, an address in `127.0.0.0/8`, or `::1`; no other name is accepted,
and no name is ever resolved, because trusting a name for its DNS answer would
let whoever controls that DNS rebind it. A listener of unknown kind is checked
like a loopback one. A
page in your browser therefore cannot spend the subscriptions or read the
provider report, whether by a cross-site request or by DNS rebinding. This is
not a substitute for the local token against other local processes, which send
neither header. A client that reaches utraque by another name (a reverse proxy,
a container using `host.docker.internal`, an `/etc/hosts` alias) is refused.

An unauthenticated loopback process can also call the provider report
(`/utraque/providers/v2`, `/utraque/providers/v1`, or the deprecated alias
`/v1/utraque/providers`). Its response can disclose locally recorded model
and token history and the live quota or balance data available from
credentials supplied to the request or configured in the process. A missing
credential or unavailable source produces an unavailable or partial section;
it does not expose data that utraque could not collect.

This is a deliberate default for a single-user machine, not a claim that it is
safe everywhere.

Set `UTRAQUE_LOCAL_TOKEN` to require a shared secret in an `X-Utraque-Token`
header on every route except `/healthz`, and configure the client to send it
(`ANTHROPIC_CUSTOM_HEADERS` for Claude Code). Generate it randomly, for example
with `openssl rand -hex 16`: the startup log shows a short fingerprint of it
(the first four bytes of an unsalted SHA-256), which cannot be reversed for a
random token but can be guess-checked for a weak one. With a token configured,
an unauthenticated `/healthz` request gets only `status`, `version` and
`uptime_s`; the rest needs the token.

Do not expose the listener beyond loopback. The binary enforces the minimum: it
classifies every listener by its bound address at startup, and a wildcard,
non-loopback or unclassifiable listener without `UTRAQUE_LOCAL_TOKEN` stops
startup with a message naming the variable. With a token it serves and logs one
warning. The token then protects against unauthenticated spending, not against
someone who can read the plain-HTTP traffic: on a shared network they can read
the token and the caller's Anthropic bearer credential off the wire.

The provider-report endpoint checks the direct peer address and rejects
non-loopback callers even when the listener and shared secret permit other
routes from the network. Behind a reverse proxy on the same host every caller
looks like loopback, so set a token if one fronts utraque.

## Out of scope

The Codex backend is undocumented and may change or restrict access without
notice. Breakage caused by an upstream change is a bug, but not a security
vulnerability.
