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

**Both legs are implemented and verified against the real backends.** A `sol`
request returned a real OpenAI answer billed to the Codex subscription, and a
Claude request streamed a correct Anthropic SSE sequence, through one proxy in
one session.

- **Anthropic leg.** A transparent passthrough: Claude Code behaves exactly as
  if it were talking to `api.anthropic.com` directly.
- **Codex/GPT leg.** A GPT model reaches the Codex backend and streams back as
  Anthropic-shaped SSE — or as a single `MessagesResponse` when the client sends
  `stream:false` — billed against your Codex subscription. `GET /v1/models`
  serves the merged picker catalog, and `POST /v1/messages/count_tokens` is
  answered locally for GPT-routed models.
- A GPT request answers `503` only when there is no Codex credential to spend.
  Run `codex login`, or point `UTRAQUE_CODEX_AUTH_FILE` at a file that holds
  one.
- **It runs unattended.** launchd holds the listening socket and starts
  `utraque` on the first connection; it exits after an idle hour and launchd
  re-activates it on the next request. See *Unattended, on demand* below.
- **One redacted line per request**, an optional trace dump, and a `/healthz`
  that explains itself. See *Logging and traces* and *Health*.
- **A uTLS fallback transport** is wired but idle: the Codex leg dials on the
  standard library and switches to a Chrome-shaped TLS handshake only if the
  upstream ever answers with a bot/TLS gate. None has been observed.

The one standing risk is not in this code: the Codex backend is undocumented and
can add stream event types without notice. `utraque` counts anything it does not
recognise and reports it on `/healthz`, and the live contract test (see
*Tests*) is the tripwire that fails loudly when it happens.

## How it works / credentials

**Anthropic leg.** This is a transparent passthrough.
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

```sh
# Build a binary at ./bin/utraque:
go build -o bin/utraque ./cmd/utraque

# …or install it onto your PATH, at $GOBIN (typically ~/go/bin/utraque):
go install ./cmd/utraque
```

Both name the **`./cmd/utraque` package**, not `./...`, and that distinction
matters: `go build ./...` over a multi-package module compiles everything as a
check and then *discards* the binaries, so it produces nothing you can run —
use it to verify the tree, not to install it. `-o` chooses where the binary
lands; `go install` puts it in `$GOBIN` (`$GOPATH/bin`, usually `~/go/bin`).

Then start it and point Claude Code at it:

```sh
./bin/utraque &                       # or let launchd do it — see below
export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
```

### Pointing Claude Code at it

| Variable | Why |
| --- | --- |
| `ANTHROPIC_BASE_URL=http://127.0.0.1:8317` | Sends every request through `utraque`. Match `UTRAQUE_LISTEN`. |
| `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` | Turns on `GET /v1/models`, so the GPT models appear in the `/model` picker. Without it the picker shows only the client's built-in Claude list — GPT names still route when typed or set in agent frontmatter. |

With no Anthropic API key set in the environment, Claude Code's own Max
subscription OAuth credential remains the active credential and is forwarded
untouched, as described above.

If you set `UTRAQUE_LOCAL_TOKEN`, every request must carry it back in the
`X-Utraque-Token` header — a dedicated header, so the client's `Authorization`
header passes through untouched — and `/healthz` is the only route exempt. That
means the client has to be able to add a custom header; check yours before
turning the token on, or you will lock yourself out of your own proxy.

### Unattended, on demand (macOS)

`utraque` does not need to be a resident daemon. launchd can hold the listening
socket and start the process only when a connection actually arrives; after an
idle hour `utraque` exits and launchd starts it again on the next request, which
the client never notices.

```sh
go build -o bin/utraque ./cmd/utraque
openssl rand -hex 16 > ~/.utraque-token && chmod 600 ~/.utraque-token
deploy/install.sh --local-token-file ~/.utraque-token
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.hughescr.utraque.plist
```

`--local-token` is optional and writes `UTRAQUE_LOCAL_TOKEN` into the plist; omit
it if your client cannot send the `X-Utraque-Token` header, and understand what
you are choosing — without it any local process can spend both subscriptions
through the loopback port.

`deploy/install.sh` only writes `~/Library/LaunchAgents/com.hughescr.utraque.plist`
and prints the commands; it never runs `launchctl` unless you pass `--load`.
Remove it again with `deploy/uninstall.sh --unload`. See
[`deploy/README.md`](deploy/README.md) for the options, how to verify it, and
what to check when it misbehaves.

Started any other way — `go run ./cmd/utraque`, a terminal, your own supervisor —
`utraque` binds `UTRAQUE_LISTEN` itself and never self-exits, because nothing
would be there to bring it back.

## Configuration

Configuration is environment variables only — every one `UTRAQUE_`-prefixed,
except the Codex CLI's own `CODEX_HOME`, which is read unprefixed on purpose so
both tools find the one `auth.json`. An empty value counts as unset, so a
default cannot be overridden to the empty string. Anything invalid fails at
startup with a named error rather than being quietly ignored.

This is the whole surface.

### Server

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_LISTEN` | `127.0.0.1:8317` | The `host:port` to bind. Also the address `ANTHROPIC_BASE_URL` must name. |
| `UTRAQUE_LOCAL_TOKEN` | *(none)* | Optional loopback shared secret, required in `X-Utraque-Token` on every request except `/healthz`. Recommended on, since any local process could otherwise spend both subscriptions through the loopback port. |
| `UTRAQUE_MAX_BODY_BYTES` | `67108864` (64 MiB) | Largest request body accepted. |
| `UTRAQUE_UPSTREAM_IDLE_TIMEOUT` | `120s` | Bounds the wait for an upstream's first byte **and** silence within a stream, so a stalled SSE response cannot pin a request forever. There is deliberately no overall request timeout: a legitimate stream can run for many minutes. |
| `UTRAQUE_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `UTRAQUE_LOG_FORMAT` | `json` | `json` \| `text`. |

### Anthropic leg

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_ANTHROPIC_BASE_URL` | `https://api.anthropic.com` | Where the passthrough forwards. Rejected at startup if it carries userinfo, a query or a fragment — a credential must never ride in a configured URL. |

### Codex/GPT leg

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_CODEX_AUTH_FILE` | `$CODEX_HOME/auth.json`, else `~/.codex/auth.json` | The Codex login token — the same file the Codex CLI reads and writes. A leading `~/` is expanded. |
| `CODEX_HOME` | *(unset)* | The Codex CLI's own variable, honoured unprefixed so pointing both tools at one directory works. |
| `UTRAQUE_CODEX_CACHE_FILE` | `<user cache dir>/utraque/models_cache.json` | `utraque`'s **own** catalog cache. Never the Codex CLI's `models_cache.json`. Empty disables it and the catalog runs memory-only. |
| `UTRAQUE_CODEX_BASE_URL` | `https://chatgpt.com/backend-api/codex` | The backend root used for both the model catalog and inference. It exists so the test suite can aim the leg at a fake upstream; in normal use, leave it alone. |
| `UTRAQUE_CODEX_TOKEN_URL` | `https://auth.openai.com/oauth/token` | Where a refresh token is exchanged. |
| `UTRAQUE_CODEX_REFRESH_SKEW` | `2m` | Refresh pre-emptively once the access token is this close to expiry. |
| `UTRAQUE_CODEX_LOCK_TIMEOUT` | `10s` | How long a refresh waits for the cross-process advisory lock on `auth.json` before giving up, so `utraque` and the Codex CLI never clobber each other. |
| `UTRAQUE_CODEX_TRANSPORT` | `auto` | `auto` \| `std` \| `utls`. See *Transport* below. A typo is a startup error, never a silent fallback. |
| `UTRAQUE_CODEX_CLIENT_VERSION` | `0.148.0` | Sent as the `client_version` query parameter on every model-catalog request. The real endpoint rejects the request outright (HTTP 400) without it, so an empty value is a startup error rather than a silent, permanently-failing catalog fetch. |

### Routing

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_ROUTING_ALIAS_OVERRIDES` | *(none)* | Comma-separated `<slug>=<codename>:<version>[:<modifier>]` entries, e.g. `gpt-5.3-codex-spark=spark:5.3`. The short-name grammar assumes a slug looks like `gpt-<version>[-<one tail token>]`; anything else needs an override to be reachable by a short name. It is the escape hatch for a newly-shipped irregular slug, so a model becomes routable without a new build. A malformed entry fails startup — a typo here means a model that silently does not route. |

### Unattended operation

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_IDLE_TIMEOUT` | `1h` under launchd, off otherwise | How long the process may sit idle before self-exiting, as a Go duration. Setting it wins in both directions, and `0` means never exit. A request still running holds the timer open, so a long streamed answer can never be cut off by an idle exit. A request that arrives in the instant after the deadline fires is answered `503` rather than started, since the drain that has already begun could not see it through; under launchd the retry restarts the daemon. |
| `UTRAQUE_LAUNCHD_SOCKET` | `Listener` | The `Sockets` key in the plist whose descriptors `utraque` adopts. Must match the plist. |

### Observability

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_TRACE_DIR` | *(off)* | Writes per-request trace dumps here. See [Logging and traces](#logging-and-traces): a trace holds the conversation, so it has its own switch rather than being reachable by raising the log level. |

Not configurable by environment today: the model picker's own knobs
(`catalog_mode`, the alias-emission strategy, the id template). They are
code-level options with working defaults — the ones described under *The
model picker* — and nothing reads an environment variable for them yet. Do not
go looking for a `UTRAQUE_DISCOVERY_*` key; there isn't one.

## Transport

`UTRAQUE_CODEX_TRANSPORT` chooses which TLS stack the **Codex leg** dials
`chatgpt.com` with. The Anthropic leg is always the standard library: it is the
sanctioned half, nothing there fingerprint-gates anyone, and dressing it up as a
browser would be dishonest for no benefit.

| Value | Behaviour |
| --- | --- |
| `auto` (default) | Start on the standard library; switch to uTLS once, permanently, the first time the upstream answers with a bot/TLS gate. No cost while no gate exists, no outage if one appears. |
| `std` | Standard library only. The stack the whole proxy was built and live-verified against, and the only one that honours `HTTP_PROXY`/`HTTPS_PROXY`. |
| `utls` | Always present a Chrome-shaped TLS ClientHello. Only the handshake differs — no forged browser headers, and the `originator` stays honestly `codex_cli_rs`. |

A hand-rolled TLS stack is a strictly larger attack surface, which is why uTLS
is never the starting point. The two legs hold separate transports and therefore
separate connection pools, so a switch on the Codex side cannot disturb an
in-flight Anthropic stream. The Codex model catalog dials on the Codex transport
too — `{base}/models` and `{base}/responses` are the same host — so a flip
carries the picker and the effort clamping with it instead of leaving them
gated. `/healthz` reports **both** legs (`anthropic` is always `std`; `codex` is
the one that can move), read fresh each time, because `auto` can change it
mid-process. The per-request `transport` field is recorded by the leg that
dispatched the request, immediately before it goes out, so the request that
trips a gate reads `std` and its successor reads `utls`.

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
`x-api-key`, `access_token`, `refresh_token` and `id_token` cannot be logged:
the slog handler is wrapped in a scrubber that blanks any attribute whose key
names a credential — including under a namespacing prefix, so `codex_token` is
blanked by the same rule as `token`, while a count like `output_tokens` is not —
and rewrites any credential-shaped value (a bearer token, a JWT, an `sk-` key, a
token field in a JSON body or query string) before it can reach the output. The
Codex `account_id` appears only as a hash prefix.

Be precise about what that buys. The header layer is a true allowlist: a header
not on the list of four is never valued, whatever it holds. The attribute layer
is a denylist over names plus a shape-matching backstop over values, so a call
site that both invented an un-denied key *and* put a credential of an
unrecognised shape under it would get through. No call site does, and the tests
say so; it is a rule enforced at the edge, not a type system.

Request and response **bodies** are never logged, at any level. One thing that
does come off an upstream response is the first 512 characters of its error
body, which becomes the request line's `err` — it goes through the scrubber like
every other string, on the log path and on the trace path alike.

**Trace dumps** are the exception, and they are behind their own switch. Setting
`UTRAQUE_TRACE_DIR` writes `<id>.request.json` for every request, and — for a
Codex request that got as far as opening a stream — `<id>.upstream.sse` and
`<id>.downstream.sse` beside it (a non-streaming answer lands in
`<id>.downstream.json`). An Anthropic passthrough, a `/healthz` poll, a
`/v1/models` open, or a Codex request that failed before the stream opened leave
the manifest alone. The same redaction is applied, manifest included. They double
as test fixtures: the bytes received and the bytes sent, side by side, turn a
translation bug into a reproducible case. **A trace holds the prompt text and the
model's output in the clear**, which is why enabling it logs a loud `WARN` at
startup.

A caller-supplied `X-Request-Id` is echoed back, logged, and used to name the
trace files, so an id that is itself credential-shaped is refused and a
generated one used instead. That is a backstop and not a guarantee: an opaque
high-entropy string is exactly what a request id looks like.

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
  recognise, by type (`unknown_events`, `unknown_event_types`,
  `streams_with_unknowns`). A non-zero count is the early warning that the
  upstream protocol has drifted; the live contract test below is the deliberate
  version of the same check.
- `transport` — which HTTP transport is in force per leg (`anthropic`, always
  `std`; `codex`, which `kind` repeats because it is the only one that can
  change), read live, since the auto transport can switch stacks mid-process.
- `trace` — whether per-request trace dumps are being written, and where. A
  directory of conversations accumulating on disk should never be a surprise.

## The model picker (merged `/v1/models`)

`utraque` serves its own `GET /v1/models`, merging Anthropic's model list with
the Codex models it can route to, so both subscriptions show up in Claude
Code's `/model` picker. Set `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` in
the client's environment to turn discovery on.

The options named in this section — `catalog_mode`, the emission strategy, the
id template — are code-level defaults today. None of them reads an
environment variable yet, so what a build serves is what you get.

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

## Tests

```sh
go test -race ./...          # the whole suite; hermetic
```

**The default suite contacts nothing.** Every upstream in it is an
`httptest` server and every credential is a throwaway written under
`t.TempDir()`. The real `chatgpt.com`, the real `auth.openai.com`, the real
`api.anthropic.com` and the real `~/.codex/auth.json` are never read, written or
contacted by `go test ./...`, and the leak test drives the production logger at
`debug` to prove no token-shaped material reaches a log line.

### The live contract test

Two files are excluded from that run by a build tag, because they do the one
thing the suite otherwise refuses to do — talk to the real backends:

```sh
go test -tags live ./...                          # both, plus the hermetic suite again
go test -tags live -run TestLiveContract ./...    # only the contract smoke test
go test -tags live -run 'TestLive(UTLS|Std)' ./...  # only the transport reachability check
```

The build **tag** is the gate, not the name. (One hermetic test is called
`TestLiveCatalogRepublishesTheRouterAliases` — "live" there means live catalog
*data* off a fake backend — which is why the real ones carry their own
prefixes.)

- `cmd/utraque/live_test.go` — **the upstream-drift tripwire.** One real request
  per leg. For the Codex leg it captures the raw upstream SSE (via a trace dump
  into `t.TempDir()`) and asserts the set of event types the backend actually
  sent is a **subset of the translator's mapping table**,
  `stream.HandledEventTypes()`. A new or renamed Codex event type fails the test
  by name and says what is being silently dropped. It cross-checks `/healthz`'s
  own drift counters, and asserts the translated stream is a well-formed
  Anthropic SSE sequence.
- `internal/transport/live_test.go` — checks that both TLS stacks still reach
  the Codex edge and get an API answer rather than a challenge page. It sends no
  credential; a `401` is a pass.

It spends real quota and reads the real `~/.codex/auth.json`, so it is a
deliberate act, not part of CI. Run `codex login` first. The Codex case fails
loudly (rather than skipping) when there is no usable credential, because a
tripwire that quietly declines to fire is worse than no tripwire. The Anthropic
case needs a credential you supply — `UTRAQUE_LIVE_ANTHROPIC_TOKEN` (sent as a
bearer token) or `ANTHROPIC_API_KEY` (sent as `x-api-key`) — and skips when
neither is set. `UTRAQUE_LIVE_CODEX_MODEL` and `UTRAQUE_LIVE_ANTHROPIC_MODEL`
override which model each case asks for, so a rename upstream does not need a
new build.

What to do when the tripwire fires: add a case for each named event type to
`Translator.handle` and list it in `handledEventTypes`
(`internal/translate/stream/translator.go`). The hermetic
`TestHandledEventTypesMatchTheDispatchSwitch` keeps that list and the dispatch
switch from drifting apart, so the table can be trusted as the thing the live
test compares against.

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
