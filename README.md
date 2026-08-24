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
- **Both legs answer.** A GPT model reaches the Codex backend and streams back
  as Anthropic-shaped SSE — or as a single `MessagesResponse` when the client
  sends `stream:false` — billed against your Codex subscription.
  `GET /v1/models` serves the merged picker catalog, and
  `POST /v1/messages/count_tokens` is answered locally for GPT-routed models.
- A GPT request answers `503` only when there is no Codex credential to spend.
  Run `codex login`, or point `UTRAQUE_CODEX_AUTH_FILE` at a file that holds
  one.
- **It runs unattended.** launchd holds the listening socket and starts
  `utraque` on the first connection; it exits after an idle hour and launchd
  re-activates it on the next request. See *Unattended, on demand* below.

Still outstanding: observability polish and the uTLS transport for a possible
Cloudflare fingerprint gate. See the project's internal plan for the full
phased roadmap; each phase is independently useful.

## How it works / credentials

**Anthropic leg (working today).** This is a transparent passthrough.
Claude Code sends its own Claude Max subscription OAuth credential on every
request; `utraque` forwards that request byte-for-byte to
`api.anthropic.com`, including the `Authorization` header and any repeated
`anthropic-beta` headers. The proxy stores no Anthropic secret of its own —
the credential lives entirely in the client and passes through untouched.

**Codex/GPT leg.** Rather than a metered API key, this leg
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

### Unattended, on demand (macOS)

`utraque` does not need to be a resident daemon. launchd can hold the listening
socket and start the process only when a connection actually arrives; after an
idle hour `utraque` exits and launchd starts it again on the next request, which
the client never notices.

```sh
go build -o bin/utraque ./cmd/utraque
deploy/install.sh --local-token "$(openssl rand -hex 16)"
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.hughescr.utraque.plist
```

`deploy/install.sh` only writes `~/Library/LaunchAgents/com.hughescr.utraque.plist`
and prints the commands; it never runs `launchctl` unless you pass `--load`.
Remove it again with `deploy/uninstall.sh --unload`. See
[`deploy/README.md`](deploy/README.md) for the options, how to verify it, and
what to check when it misbehaves.

Started any other way — `go run ./cmd/utraque`, a terminal, your own supervisor —
`utraque` binds `UTRAQUE_LISTEN` itself and never self-exits, because nothing
would be there to bring it back.

## Configuration

Configuration is via `UTRAQUE_`-prefixed environment variables (a TOML file
is also planned). Knobs that exist today:

- **Listen address** — the host:port `utraque` binds to.
- **Local token** — an optional shared secret required on incoming requests,
  recommended on, since any local process could otherwise spend both
  subscriptions through the loopback port.
- **Anthropic base URL** — the upstream Anthropic endpoint the passthrough
  leg forwards to (defaults to `api.anthropic.com`).
- **Codex base URL** (`UTRAQUE_CODEX_BASE_URL`) — the Codex backend root used
  for both the model catalog and inference. It exists so the test suite can
  aim the leg at a fake upstream; in normal use, leave it alone.
- **Codex auth file** (`UTRAQUE_CODEX_AUTH_FILE`) — where the Codex login
  token lives. Defaults to `$CODEX_HOME/auth.json`, else `~/.codex/auth.json`,
  which is the same file the Codex CLI reads and writes.
- **Alias overrides** (`UTRAQUE_ROUTING_ALIAS_OVERRIDES`) — a comma-separated
  list of `<slug>=<codename>:<version>[:<modifier>]` entries, e.g.
  `gpt-5.3-codex-spark=spark:5.3`. The short-name grammar assumes a slug looks
  like `gpt-<version>[-<one tail token>]`; anything else needs an override to
  be reachable by a short name. It is the escape hatch for a newly-shipped
  irregular slug, so a model becomes routable without a new build.
- **Limits** — request body size and similar guardrails enforced by the
  server middleware.
- **Idle timeout** (`UTRAQUE_IDLE_TIMEOUT`) — how long the process may sit idle
  before self-exiting, as a Go duration. Under launchd socket activation this
  defaults to `1h`; started by hand it defaults to off, since nothing would
  bring it back. Setting it wins in both directions, and `0` means never exit.
  A request that is still running holds the timer open, so a long streamed
  answer can never be cut off by an idle exit.
- **launchd socket** (`UTRAQUE_LAUNCHD_SOCKET`) — the `Sockets` key in the plist
  whose descriptors `utraque` adopts. Default `Listener`; it must match the
  plist.
- **Trace directory** (`UTRAQUE_TRACE_DIR`) — off unless set. See
  [Logging and traces](#logging-and-traces): a trace holds the conversation, so
  it has its own switch rather than being reachable by raising the log level.

## Logging and traces

One structured line per request, on stderr (launchd captures it), carrying
`request_id`, `method`, `path`, `status`, `req_bytes`, `resp_bytes`, `ttfb_ms`,
`total_ms`, `route`, `client_model`, `upstream_model`, `effort`, `stream`,
`upstream_status`, `output_tokens`, `stop_reason`, `interrupted`, `transport`,
and `err` when there was one.

Two of those fields earn their place by being *differences*. `upstream_status`
is the status the BACKEND gave, which is not always the one you were answered
with — an upstream 200 whose body carried no events becomes a 502 downstream,
and an upstream 401 becomes a refresh and a retry. `interrupted` separates "you
hung up" from "it broke", so a cancelled turn never reads as an incident.

**Redaction is by allowlist.** Exactly four request headers may be logged with
their values — `anthropic-version`, `anthropic-beta`, `content-type`,
`user-agent`. Every other header is named but never valued, so the shape of a
request stays debuggable without its contents being disclosed. `Authorization`,
`x-api-key`, `access_token`, `refresh_token` and `id_token` are unloggable *by
construction*: the slog handler is wrapped in a scrubber that blanks any
attribute whose key names a credential and rewrites any credential-shaped value
(a bearer token, a JWT, an `sk-` key, a token field in a JSON body or query
string) before it can reach the output. The Codex `account_id` appears only as
a hash prefix. Request and response bodies are never logged, at any level.

**Trace dumps** are the one exception, and they are behind their own switch.
Setting `UTRAQUE_TRACE_DIR` writes three files per request —
`<id>.request.json`, `<id>.upstream.sse` and `<id>.downstream.sse` (a
non-streaming answer lands in `<id>.downstream.json`) — with the same redaction
applied. They double as test fixtures: the bytes received and the bytes sent,
side by side, turn a translation bug into a reproducible case. **A trace holds
the prompt text and the model's output in the clear**, which is why enabling it
logs a loud `WARN` at startup.

## Short model names

Short names (`sol`, `sol-5.6`, `sol-high`) are derived from the model list the
Codex backend itself serves, not from a table compiled into the binary. Every
successful catalog read — the per-request lookup that clamps reasoning effort,
and a picker open — republishes them, so a codename OpenAI ships today starts
resolving as soon as anything reads the catalog, and a retired slug stops.
Until the first read succeeds, a compiled-in seed applies. Raw `gpt-*` slugs
always route regardless.

## Health

`GET /healthz` is answered locally and never contacts either upstream. It
reports process status, version and uptime, plus, for the Codex leg:

- `codex_auth` — the credential state (`ok` / `stale` / `missing`) and the
  seconds until the access token expires. The token value never appears.
- `codex_catalog` — how many models the held snapshot holds, how old it is, and
  `state`: **why** it looks like that. A bare `models: 0` is several different
  situations wearing one face, so they are named — `loaded`, `empty` (a fetch
  succeeded and the backend really listed nothing), `failed` (with
  `last_error`), `unavailable` (no `codex login` here), `warming`, or `cold`.
  The catalog is also warmed in the background at startup, so the count is a
  fact about the backend rather than a fact about whether anyone has used the
  proxy yet.
- `codex_routing` — the short-name route families the router currently
  resolves: the quickest way to see whether the live catalog has been loaded or
  the compiled-in seed is still in force.
- `codex_quota` — the rolling usage windows the backend reports on its own
  response headers, with the age of that reading, so subscription burn-down is
  visible. Always present, carrying `reported: false` until the backend has
  said anything: an absent quota block reads as "quota is fine".
- `codex_stream` — how many Codex stream events the translator did not
  recognise, by type. A non-zero count is the early warning that the upstream
  protocol has drifted.
- `transport` — which HTTP transport is in force (`std` or `utls`), read live,
  since the auto transport can switch stacks mid-process.
- `trace` — whether per-request trace dumps are being written, and where. A
  directory of conversations accumulating on disk should never be a surprise.

## The model picker (merged `/v1/models`)

`utraque` serves its own `GET /v1/models`, merging Anthropic's model list with
the Codex models it can route to, so both subscriptions show up in Claude
Code's `/model` picker. Set `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` in
the client's environment to turn discovery on.

Everything in this section is built to the client's actual behaviour, verified
against the Claude Code binary rather than inferred:

- The client fetches `GET {base}/v1/models?limit=1000` with a **3-second
  timeout** and treats **any redirect as a hard failure**. `utraque` therefore
  never redirects on this route and answers within an internal **1.5s
  deadline**, falling back rather than running late.
- It reads only `id` and `display_name`, and **discards any id that does not
  match `/(claude|anthropic)/i`** — a case-insensitive, unanchored substring
  test.
- The id is sent back verbatim as the request's `model` when a row is picked,
  so every id `utraque` advertises is registered in the router's alias registry
  and is guaranteed to route.
- An empty list always beats an error: every failure path still returns HTTP
  200 with a well-formed `{"data":[…]}` body.

### Anthropic models

`catalog_mode` picks where the Claude rows come from:

| Mode | Behaviour |
| --- | --- |
| `merge` (default) | Read Anthropic's own catalog using the credential on the incoming request, then union it with a built-in static list so nothing is missing. |
| `upstream` | Only what the upstream read returned. If it fails, no Claude rows. |
| `static` | Never contact Anthropic. |

A failed or refused upstream read is **negative-cached for ~60s**, so a
credential that cannot read that endpoint costs one slow picker open, not every
one. Note that Claude Code only attempts gateway discovery at all when
`ANTHROPIC_AUTH_TOKEN` or an API key is set — a plain subscription OAuth
session sends neither, so in normal use the Claude half is served from the
static list. That is the designed outcome, which is why the fallback, not the
upstream read, is the load-bearing path.

### Codex/GPT models

Codex models are advertised under a configurable id template, defaulting to the
prefixed compat form **`anthropic-compat.{alias}`** — chosen because it passes
both today's "contains" filter and a plausible future "starts-with" one. Only
models the Codex catalog marks `visibility: "list"` are offered unless
`include_hidden` is set.

Four emission strategies, alias emission **on by default**:

| Strategy | Emits |
| --- | --- |
| `template` (default) | The rolling and pinned aliases: `anthropic-compat.sol`, `anthropic-compat.sol-5.6` |
| `effort_variants` | The above plus one row per supported reasoning effort: `anthropic-compat.sol-high`, `anthropic-compat.sol-5.6-ultra` |
| `passthrough` | One row per raw upstream slug: `anthropic-compat.gpt-5.6-sol` |
| `off` | No Codex rows. GPT names still route when typed or set in agent frontmatter — they just don't appear in the picker. |

An id template that could not produce a filter-passing id is rejected at
startup rather than silently yielding a picker with no GPT rows in it.

### 1M-context rows

Because `utraque` is a gateway, Claude Code cannot verify that a model supports
a 1M context window, so it does **not** offer the "(1M context)" picker entry
it would offer on a direct connection. The capability is there; the row is not.

So `utraque` emits the row itself: for each natively-1M Claude model it adds a
second entry whose id carries the `[1m]` marker, e.g.

```
claude-sonnet-5        Sonnet 5
claude-sonnet-5[1m]    Sonnet 5 (1M context)
```

Picking the second one makes the client send the `context-1m-2025-08-07` beta,
treat the window as 1,000,000 tokens for auto-compaction, and strip the `[1m]`
marker back off before putting the model in the request body — so the proxy
receives an ordinary `claude-sonnet-5` request carrying the long-context beta,
which the passthrough forwards untouched.

The default set matches the client's own native-1M list (Sonnet 5, Fable 5,
Opus 5, Opus 4.7, Opus 4.8) and is configurable, as is the whole feature. Pair
it with `CLAUDE_CODE_MAX_CONTEXT_TOKENS` / auto-compaction settings if you want
those models to compact at their true window rather than a conservative
default.

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
