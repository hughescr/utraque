# utraque

A local HTTP proxy that lets one Claude Code session use a Claude Max
subscription for Anthropic models, a ChatGPT/Codex subscription for OpenAI GPT
models, and a prepaid DeepSeek API balance side by side.

*One communicant, both subscriptions.*

## What it is

`utraque` is a local HTTP proxy you point Claude Code at. It routes by model
name: pick an Anthropic model and the request goes to `api.anthropic.com`
billed against your Claude Max subscription; pick an OpenAI GPT model
(bare aliases `sol`, `terra`, `luna`) and the request goes to OpenAI billed
against your ChatGPT/Codex subscription; pick `deepseek-flash` or
`deepseek-v4-pro` and the request uses your prepaid DeepSeek API key. All three
backends are addressed by their own model names inside one Claude Code session.

## Status

**The subscription legs are implemented and verified against the real
backends.** A `sol`
request returned a real OpenAI answer billed to the Codex subscription, and a
Claude request streamed a correct Anthropic SSE sequence, through one proxy in
one session. The DeepSeek leg is covered by hermetic contract tests; it has not
yet been included in the live-backend tripwire.

- **Anthropic leg.** A transparent passthrough: the request, including Claude
  Code's own OAuth credential, is forwarded byte-for-byte. The transport is
  transparent; one *client-side* behaviour is not, which is why
  `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1` is required — see *Install & run*.
- **Codex/GPT leg.** A GPT model reaches the Codex backend and streams back as
  Anthropic-shaped SSE — or as a single `MessagesResponse` when the client sends
  `stream:false` — billed against your Codex subscription. `GET /v1/models`
  serves the merged picker catalog, and `POST /v1/messages/count_tokens` is
  answered locally for GPT-routed models with the real o200k_base tokenizer
  (compiled in; nothing is downloaded), counting the request as it would go
  upstream — see *Token counts and the message_start seed*.
- **DeepSeek leg.** Uses DeepSeek's documented Anthropic-compatible endpoint
  with a separately configured API key. It accepts only documented model names,
  canonicalizes retired Flash aliases to `deepseek-flash`, and never forwards
  the caller's Anthropic OAuth credential, cookies, or local utraque token.
  `count_tokens` is estimated locally because DeepSeek does not document that
  endpoint.
- A missing Codex credential makes a GPT request answer `503`. Run `codex
  login`, or point `UTRAQUE_CODEX_AUTH_FILE` at a file that holds a token. A
  request racing the idle exit can also see `503`; under launchd a retry
  activates a fresh daemon.
- **The prompt cache is kept warm.** Reasoning items are replayed through the
  client, and every request carries a `prompt_cache_key` and a matching
  `session_id`. Without this the backend's cache stops matching at the first
  assistant turn and an agentic loop re-reads its whole conversation every turn.
  See *Prompt caching*.
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

A response body is relayed exactly as upstream encoded it, compression
included. So when a stream dies part-way — a dropped link, an upstream that
goes silent past the upstream idle timeout — `utraque` drops the connection rather than
closing the response tidily. A tidy close would tell the client it had received
the whole body, and a client that then failed to decompress the truncated
remains would report a corrupt response instead of the network fault it was. A
dropped connection is the honest signal, and every HTTP client already retries
one.

**Codex/GPT leg.** Rather than a metered API key, this leg
reads the Codex CLI's own login token from `~/.codex/auth.json`, refreshes it
when it's near expiry (writing the refreshed token back to that file safely,
so it never clobbers the Codex CLI's own state), and uses it to call OpenAI's
backend on your behalf. This bills usage against your ChatGPT/Codex
subscription, not a separate API key.

**DeepSeek leg.** This leg owns a prepaid API key and sends it only to
DeepSeek's documented Anthropic-format endpoint. Set `DEEPSEEK_API_KEY` for a
manually started process, or put the plain key in a private file and set
`UTRAQUE_DEEPSEEK_API_KEY_FILE` to its path. The explicit file wins over the
environment value and a missing, unreadable, or empty file is a startup error.
DeepSeek does not publish a standard `~/.deepseek.json` credential location,
so utraque does not guess one.

The compatibility endpoint documents some Anthropic fields as ignored. Utraque
allows performance hints such as `cache_control` and thinking
`budget_tokens`, but rejects requirements whose meaning would otherwise be
silently lost: unsupported content blocks, Pro image input, `top_k`,
non-default `service_tier`, MCP/container requests, structured `output_config`, forced
serial tool calls, and text citations. Because DeepSeek ignores
`tool_result.is_error`, utraque removes that flag and prefixes failed tool output
with `[tool error]` so ordinary tool failures keep their meaning across turns.
Claude Code's client-side tool search returns discovered names as nested
`tool_reference` blocks, which [DeepSeek's compatibility table](https://api-docs.deepseek.com/guides/anthropic_api/)
does not document. Utraque rewrites each reference as an explicit text marker
only when the same request still includes that tool's supported top-level
`input_schema`, and removes `defer_loading` from referenced definitions because
DeepSeek has no documented deferral mechanism. The model therefore receives
both the discovery result and the callable definition. Missing definitions and
unknown content-block types remain request errors rather than being forwarded.

DeepSeek also validates every tool schema `pattern` as a regular expression
before the model runs, in a dialect that is not quite the one Anthropic
clients write for. Utraque translates each pattern to the DeepSeek dialect
before forwarding, so a tool such as Claude Code's Artifact tool — whose
`^[^\0]*$` DeepSeek rejects outright — no longer fails every request on a
DeepSeek route. See *Tool schemas on a GPT or DeepSeek route* for the rules.

## Install & run

### Prerequisites

- **Go 1.27** to build. The macOS launchd deployment additionally needs cgo
  (`CGO_ENABLED=1`, the default) and the Xcode Command Line Tools: adopting
  launchd's socket calls `launch_activate_socket(3)` through a small Darwin
  shim, and a `CGO_ENABLED=0` build cannot do it — it would bind
  `UTRAQUE_LISTEN` itself and collide with the socket launchd already holds.
- **Claude Code**, signed in to the Claude Max account the Anthropic leg should
  bill.
- **The Codex CLI**, signed in to the ChatGPT/Codex account the GPT leg should
  bill. Run `codex login` before the first GPT request, or it answers `503`:
  `utraque` holds no credential of its own to fall back on.
- **A DeepSeek API key** is optional. Without one, DeepSeek models are not
  advertised in `/v1/models` and a typed DeepSeek request returns `503`.
- **No Anthropic API key in the environment.** An `ANTHROPIC_API_KEY` or
  `ANTHROPIC_AUTH_TOKEN` left in a shell profile displaces the subscription
  OAuth credential and quietly bills metered API usage instead — the one thing
  this project exists to avoid.

### Quickstart

From nothing to a GPT model answering inside Claude Code:

```sh
git clone https://github.com/hughescr/utraque.git
cd utraque
go build -o bin/utraque ./cmd/utraque

codex login                    # if you have not already
unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN

./bin/utraque &                # or let launchd do it — see below
curl -sf http://127.0.0.1:8317/healthz | python3 -m json.tool | head -20
#   codex_auth.state should read "ok"; if it says "missing", run codex login

export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
export _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1

claude --model terra-high      # a GPT model, billed to the Codex subscription
```

Inside that session, `/model` switches back to any Claude model, which bills
the Max subscription. One session, both subscriptions.

### About those build commands

Both name the **`./cmd/utraque` package**, not `./...`, and that distinction
matters: `go build ./...` over a multi-package module compiles everything as a
check and then *discards* the binaries, so it produces nothing you can run —
use it to verify the tree, not to install it. `-o` chooses where the binary
lands; `go install ./cmd/utraque` puts it in `$GOBIN` (`$GOPATH/bin`, usually
`~/go/bin`) instead.

### Version stamping

A build from a git checkout self-stamps: Go's `-buildvcs` default embeds the
checkout's revision and whether the tree was modified, and `utraque` turns
that into a version like `1.0.0+0ef929d` (or `1.0.0+0ef929d.dirty` for an
uncommitted tree) with no build flags required. That string is what
`/healthz`'s `version` field reports, what the startup "listening" log line
carries, and what `utraque --version` (or `-version`) prints to stdout before
exiting — the flag is handled before config loads or Codex is contacted, so it
works even with no Codex CLI installed.

`-ldflags "-X main.version=<string>"` overrides the self-stamped value
outright, for a packaged build (a Homebrew formula, a container image) that
wants to pin an exact string instead of trusting the embedded revision:

```sh
go build -ldflags "-X main.version=1.0.0" -o bin/utraque ./cmd/utraque
```

### Pointing Claude Code at it

| Variable | Why |
| --- | --- |
| `ANTHROPIC_BASE_URL=http://127.0.0.1:8317` | **Required.** Sends every request through `utraque`. Match `UTRAQUE_LISTEN`. |
| `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1` | **Required.** See below — without it every Claude model silently loses most of its context window. |
| `CLAUDE_CODE_MAX_CONTEXT_TOKENS` | Optional, and **GPT-only**. See below. |
| `ANTHROPIC_CUSTOM_HEADERS="X-Utraque-Token: <token>"` | Required **only** if you set `UTRAQUE_LOCAL_TOKEN`. Without it Claude Code cannot authenticate to your own proxy. |
| `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` | Optional, and inert alongside the variable above — the two are mutually exclusive. See *Using a GPT route*. |

#### `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL` — do not skip this

The HTTP forwarding is transparent, but pointing Claude Code at a local address
changes a decision it makes **client-side**, before any request is sent. Claude
Code treats any `ANTHROPIC_BASE_URL` whose host is not `api.anthropic.com` as
third-party, and third-party Claude models get a 200,000-token default instead
of their real window — 1,000,000 for the current Opus, Sonnet and Fable models.

The loss is worse than a smaller window. Once a session's live context is past
that clamp, auto-compaction has to summarise a history larger than the limit it
is compacting to; that request fails, the context never shrinks, and compaction
re-fires immediately, forever. Setting this variable restores the client's
native window handling. It is accurate rather than a trick: `utraque` really is
a transparent pass-through to `api.anthropic.com` carrying the client's own
credential.

#### `CLAUDE_CODE_MAX_CONTEXT_TOKENS` — GPT routes only

Claude Code applies this value **only when the resolved model name does not
start with `claude-`**. It sizes the GPT routes and can never change a Claude
model's window, so it is not an alternative to the variable above — the two do
not overlap.

One value covers every GPT route, and every model the live Codex catalog now
lists reports the same window: 272,000 tokens for `gpt-5.6-sol`,
`gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`, `gpt-5.4` and `gpt-5.4-mini`. Set
`272000`, and leave it unset if you only use Claude models.

If a narrower model ever reappears in the catalog, size this to the smallest
route you actually use rather than the largest. An early compaction costs
context you can re-establish, while a session allowed to grow past its real
window draws a hard "too long" error from the backend and breaks the
conversation outright.

#### If you turn the local token on

With `UTRAQUE_LOCAL_TOKEN` set, every request must carry it back in the
`X-Utraque-Token` header — a dedicated header, so the client's `Authorization`
passes through untouched — and `/healthz` is the only exempt route. Claude Code
can send it:

```sh
export ANTHROPIC_CUSTOM_HEADERS="X-Utraque-Token: $(< ~/.utraque-token)"
```

Because `/healthz` is exempt, a healthy-looking probe does **not** prove the
token is wired. Verify with a real request.

With no Anthropic API key set in the environment, Claude Code's own Max
subscription OAuth credential remains the active credential and is forwarded
untouched, as described above.

### Using a GPT route

Three ways in, in the order most people want them:

**1. An agent definition** — the durable option. A file in `.claude/agents/`:

```md
---
name: gpt-terra-high
description: Cross-family substantive work, or review of Claude's own output.
model: terra-high
effort: high
---
```

**2. At launch or in-session** — `claude --model terra-high`, or `/model` and
type the name.

Any of these name forms resolve: a bare alias (`sol`, `terra`, `luna`), a
version pin (`sol-5.6`), an effort suffix (`sol-high`, `sol-5.6-ultra`), or the
raw upstream slug (`gpt-5.6-sol`). Effort composes with any of them and is
clamped to what that model actually supports.

**3. The `/model` picker** — *not available if you took the required context
window fix, and you should.* The two settings are mutually exclusive. Claude
Code gates gateway discovery on four conditions, and the third is the same
predicate `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL` flips:

```js
if(!CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY) return false;
if(providerKind() !== "firstParty")            return false;
if(assumeFirstPartyBaseURL())                  return false;   // <-- here
if(!ANTHROPIC_BASE_URL)                        return false;
return true;
```

Telling the client to treat this proxy as first-party also tells it there is no
gateway to interrogate, so it never calls `GET /v1/models` and no GPT rows
appear in the picker — whatever `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY` is
set to. That is the right trade: the picker is a convenience, while the context
clamp costs 800,000 tokens on every Claude model and can hang a long session.

Typed names and agent frontmatter never depended on discovery and are
unaffected, which is why they are listed first. The merged catalog is still
served and is still useful to any client that does ask for it.

### Tool schemas on a GPT or DeepSeek route

The Codex and DeepSeek backends both validate every tool's parameter schema
before a model runs, and each accepts its own regex dialect, narrower or
merely different from the patterns Anthropic clients send. A tool whose
pattern uses a form the backend rejects fails every request on that route
with `Invalid schema for function ... is not a 'regex'`. Claude Code's
Artifact tool trips both: its `field` pattern has lookahead and a `\p{Cc}`
Unicode class, which Codex rejects (lookahead currently reports a more
specific `Invalid JSON schema: regex lookaround is not supported`), and its
`file_paths` pattern has a `\0`, which DeepSeek rejects.

Both legs run the same schema walker, parameterized by a per-backend dialect
(`internal/toolschema`). Each pattern is either rewritten to the spelling the
backend accepts or, where no compatible spelling exists, dropped from the
schema with its original text appended to the property's description so the
constraint remains in the tool declaration. A dropped pattern is no longer
enforced by backend schema validation, so callers or tool implementations that
require enforcement must validate the input themselves. An untouched schema is
forwarded with its content unchanged: the leg re-encodes the request with Go's
`encoding/json`, which keeps key order and number spelling but compacts
whitespace and escape spellings, so what changes is the formatting, never the
constraint.

**Codex dialect.** Unicode property classes become explicit codepoint ranges
from Go's own Unicode tables, `(?<name>…)` becomes `(?P<name>…)`, `\k<name>`
becomes `(?P=name)`, `\z` becomes `\Z`, and `\x{HHHH}` becomes `\uHHHH`.
Lookaround (`(?=...)`, `(?!...)`, `(?<=...)`, and `(?<!...)`) is dropped
because Codex schema validation rejects it. Atomic groups, possessive
quantifiers, `\Q…\E`, POSIX classes, and a property-class expansion that would
run to hundreds of ranges are also dropped.

**DeepSeek dialect.** The engine is a Rust one with fancy-regex extensions, so
it accepts most of what Codex rejects — lookaround, atomic groups, possessive
quantifiers, `\p{…}` by name, `(?<name>…)`, `\k<name>`, POSIX classes, `\G`,
and the `x` and `u` inline flags all pass through untouched. What it rejects
was established empirically, one pattern per request against the live
endpoint: `\0` (read as a backreference outside a class and refused inside
one) is rewritten to `\x00`, and `\0` followed by octal digits to the matching
`\xHH`; `\Z` becomes `\z` (the reverse of the Codex rewrite); `\p{^Name}`
becomes `\P{Name}`; and inside a bracket expression a bare `[` is escaped,
because the engine reads it as a nested class, as is each `&` or `~` that is
followed by another, which it reads as a set operator. `\C`, `\R`, `\Q…\E`,
`\N{…}`, `\o{…}`, `\g<…>`, unknown letter escapes such as `\y`, and a numbered
backreference inside a class are dropped.

The translation log line for either leg names each rewritten node as
`rewritten_patterns` and each dropped one as `dropped_patterns`, in
`Tool.properties.field` form.


### Unattended, on demand (macOS)

`utraque` does not need to be a resident daemon. launchd can hold the listening
socket and start the process only when a connection actually arrives; after an
idle hour `utraque` exits and launchd starts it again on the next request, which
the client never notices.

```sh
go build -o bin/utraque ./cmd/utraque
(umask 077; openssl rand -hex 16 > ~/.utraque-token)
deploy/install.sh --local-token-file ~/.utraque-token --codex-executable codex
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.hughescr.utraque.plist
```

`--local-token-file` is optional (as is `--local-token`, which takes the value
directly) and writes `UTRAQUE_LOCAL_TOKEN` into the plist; pair it with
`ANTHROPIC_CUSTOM_HEADERS` as shown above, and if you omit it understand what
you are choosing — without it any local process can spend the configured Codex
subscription or DeepSeek prepaid balance and read whatever local usage history
and available provider quota or balance data the provider report can collect.
Anthropic requests and live Anthropic usage readings still require the caller's
own bearer credential.

`deploy/install.sh` writes `~/Library/LaunchAgents/com.hughescr.utraque.plist`,
creates `~/Library/LaunchAgents` and `~/Library/Logs/utraque` if they are
missing, and prints the commands; it never runs `launchctl` unless you pass
`--load`.
Remove it again with `deploy/uninstall.sh --unload`. See
[`deploy/README.md`](deploy/README.md) for the options, how to verify it, and
what to check when it misbehaves.

Started any other way — `go run ./cmd/utraque`, a terminal, your own supervisor —
`utraque` binds `UTRAQUE_LISTEN` itself and never self-exits, because nothing
would be there to bring it back.

## Configuration

Configuration is environment variables only — every one `UTRAQUE_`-prefixed,
except the Codex CLI's own `CODEX_HOME` and DeepSeek's conventional
`DEEPSEEK_API_KEY`. An empty value counts as unset, so a
default cannot be overridden to the empty string. Anything invalid fails at
startup with a named error rather than being quietly ignored.

This is the whole surface.

### Server

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_LISTEN` | `127.0.0.1:8317` | The `host:port` to bind. Also the address `ANTHROPIC_BASE_URL` must name. |
| `UTRAQUE_LOCAL_TOKEN` | *(none)* | Optional loopback shared secret, required in `X-Utraque-Token` on every request except `/healthz`. Recommended on: without it, any local process can spend the configured Codex subscription or DeepSeek balance and read whatever provider-report data is available. Anthropic operations still require the caller's own bearer credential. |
| `UTRAQUE_MAX_BODY_BYTES` | `67108864` (64 MiB) | Largest request body accepted. |
| `UTRAQUE_UPSTREAM_IDLE_TIMEOUT` | `120s` | Bounds the wait for an upstream's first byte **and** silence within a stream, so a stalled SSE response cannot pin a request forever. There is deliberately no overall request timeout: a legitimate stream can run for many minutes. |
| `UTRAQUE_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `UTRAQUE_LOG_FORMAT` | `json` | `json` \| `text`. |

### Anthropic leg

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_ANTHROPIC_BASE_URL` | `https://api.anthropic.com` | Where the passthrough forwards. Rejected at startup if it carries userinfo, a query or a fragment — a credential must never ride in a configured URL. |

### DeepSeek leg

| Variable | Default | What it does |
| --- | --- | --- |
| `DEEPSEEK_API_KEY` | *(none)* | Conventional DeepSeek API key variable. Never forwarded from the incoming request and never rendered in config or logs. |
| `UTRAQUE_DEEPSEEK_API_KEY_FILE` | *(none)* | Path to a plain private key file. A leading `~/` is expanded. When set, it wins over `DEEPSEEK_API_KEY` and any read failure is fatal rather than falling back to a different credential. For launchd, add this variable to the plist because launchd does not inherit the shell environment. |
| `UTRAQUE_DEEPSEEK_BASE_URL` | `https://api.deepseek.com/anthropic` | DeepSeek's documented Anthropic-format endpoint. The override exists for hermetic testing and private compatible gateways; in normal use, leave it alone. Credential-bearing URLs are rejected. |

### Codex/GPT leg

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_CODEX_AUTH_FILE` | `$CODEX_HOME/auth.json`, else `~/.codex/auth.json` | The Codex login token — the same file the Codex CLI reads and writes. A leading `~/` is expanded. |
| `CODEX_HOME` | *(unset)* | The Codex CLI's own variable, honoured unprefixed so pointing both tools at one directory works. |
| `UTRAQUE_CODEX_CACHE_FILE` | `<user cache dir>/utraque/models_cache.json` | `utraque`'s **own** catalog cache. Never the Codex CLI's `models_cache.json`. An empty value counts as unset, so the default applies; the catalog runs memory-only only when no user cache directory can be determined. |
| `UTRAQUE_CODEX_BASE_URL` | `https://chatgpt.com/backend-api/codex` | The backend root used for both the model catalog and inference. It exists so the test suite can aim the leg at a fake upstream; in normal use, leave it alone. |
| `UTRAQUE_CODEX_TOKEN_URL` | `https://auth.openai.com/oauth/token` | Where a refresh token is exchanged. |
| `UTRAQUE_CODEX_REFRESH_SKEW` | `2m` | Refresh pre-emptively once the access token is this close to expiry. |
| `UTRAQUE_CODEX_LOCK_TIMEOUT` | `10s` | How long a refresh waits for the cross-process advisory lock on `auth.json` before giving up, so `utraque` and the Codex CLI never clobber each other. |
| `UTRAQUE_CODEX_TRANSPORT` | `auto` | `auto` \| `std` \| `utls`. See *Transport* below. A typo is a startup error, never a silent fallback. |
| `UTRAQUE_CODEX_CLIENT_VERSION` | detected from `UTRAQUE_CODEX_EXECUTABLE --version` | Sent as the `client_version` query parameter on every model-catalog request. Discovery is bounded and requires `codex-cli <semantic-version>` on stdout; startup fails if discovery cannot supply a real version, because stale versions can hide newly released models. Set this explicitly to bypass executable discovery. |

The discovered Codex version is fixed for the life of the utraque process. A
Codex CLI upgrade is reflected the next time utraque starts; an explicit
`UTRAQUE_CODEX_CLIENT_VERSION` bypasses discovery and remains in force until the
override is changed or removed.

### Routing

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_ROUTING_ALIAS_OVERRIDES` | *(none)* | Comma-separated `<slug>=<codename>:<version>[:<modifier>]` entries, e.g. `gpt-5.3-codex-spark=spark:5.3`. The short-name grammar assumes a slug looks like `gpt-<version>[-<one tail token>]`; anything else needs an override to be reachable by a short name. It is the escape hatch for a newly-shipped irregular slug, so a model becomes routable without a new build. A malformed entry fails startup — a typo here means a model that silently does not route. |

### Unattended operation

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_IDLE_TIMEOUT` | `1h` under launchd, off otherwise | How long the process may sit idle before self-exiting, as a Go duration. Setting it wins in both directions, and `0` means never exit. A request still running holds the timer open, so a long streamed answer can never be cut off by an idle exit. A request that arrives in the instant after the deadline fires is answered `503` rather than started, since the drain that has already begun could not see it through; under launchd the retry restarts the daemon. |
| `UTRAQUE_LAUNCHD_SOCKET` | `Listener` | The `Sockets` key in the plist whose descriptors `utraque` adopts. Must match the plist. |

### Provider reporting

| Variable | Default | What it does |
| --- | --- | --- |
| `UTRAQUE_CCUSAGE_EXECUTABLE` | *(none)* | Direct path to an installed native `ccusage` binary. When set, this takes precedence over the package runner. Homebrew users can set `/opt/homebrew/bin/ccusage` after `brew install ccusage`. Utraque never downloads or updates it. |
| `UTRAQUE_CCUSAGE_RUNNER` | `bunx` | Package runner used when no native executable is set. |
| `UTRAQUE_CCUSAGE_VERSION` | `latest` | `ccusage` package version requested through the runner. The resolved version is recorded in each report. |
| `UTRAQUE_CODEX_EXECUTABLE` | `codex` | Codex executable queried once at startup for the model-catalog client version, then used for the report's isolated, short-lived app-server query. It uses the same credential source as inference. Under launchd, configure an absolute path because its `PATH` is intentionally narrow. |
| `UTRAQUE_PROVIDER_REPORT_CACHE_TTL` | `30s` | Lifetime of one coherent local-history and live-quota snapshot. Was `UTRAQUE_PROVIDER_CACHE_TTL`, which is still read as a deprecated alias for one release: it applies only when the new name is unset, and using it logs a WARN line naming both. |
| `UTRAQUE_PROVIDER_REPORT_TIMEOUT` | `90s` | Overall deadline for an on-demand report collection. Was `UTRAQUE_PROVIDER_TIMEOUT`, a deprecated alias on the same terms. |
| `UTRAQUE_CLAUDE_PLAN` | *(none)* | Optional operator-supplied Claude plan label. It is reported as configured metadata, not provider-confirmed data. |
| `UTRAQUE_CLAUDE_PLAN_MULTIPLIER` | *(none)* | Optional positive operator-supplied plan multiplier. There is deliberately no assumed default. |

The startup `listening` line's `config` group renders the two timing values
as `provider_report.cache_ttl` and `provider_report.timeout` (they were
`reporting.cache_ttl` and `reporting.timeout` before the variables were
renamed to match).

Errors about these two values name a variable, and which one depends on the
kind of error. A value that does not parse as a duration is reported against
the variable that was actually read, so `UTRAQUE_PROVIDER_CACHE_TTL=forever`
fails with `config: UTRAQUE_PROVIDER_CACHE_TTL: time: invalid duration
"forever"` and points at the line to fix. A value that parses but is not
positive fails validation of the loaded configuration, which no longer knows
where the value came from, so it is always reported against the canonical
name: `UTRAQUE_PROVIDER_TIMEOUT=0` fails with `config:
UTRAQUE_PROVIDER_REPORT_TIMEOUT must be positive`, the variable you should be
setting anyway. (Both validation errors named the old variables before the
rename.)

The native ccusage executable avoids a Bun/Node runtime dependency. The default
remains `bunx ccusage@latest` for installations that do not set a native path.
ccusage availability is checked on the report request. Codex availability is
checked at startup unless `UTRAQUE_CODEX_CLIENT_VERSION` is set explicitly.
This startup requirement applies even when you intend to use only non-Codex
routes. A launchd host that deliberately has no Codex executable must add the
explicit override to the generated plist as described in
[`deploy/README.md`](deploy/README.md#codex-free-launchd-hosts).

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
is never the starting point. The three inference legs use two transports: Anthropic
and DeepSeek share one connection pool, while Codex has the other, so a switch on
the Codex side cannot disturb an in-flight Anthropic or DeepSeek stream. The
Codex model catalog dials on the Codex transport too — `{base}/models` and
`{base}/responses` are the same host — so a flip carries the picker and the
effort clamping with it instead of leaving them gated. `/healthz` reports **both**
transports (`anthropic`, shared with DeepSeek, is always `std`; `codex` is the one
that can move), read fresh each time, because `auto` can change it mid-process.
The per-request `transport` field is recorded by the leg that dispatched the
request, immediately before it goes out, so the request that trips a gate reads
`std` and its successor reads `utls`. The startup configuration record, and
the `WARN` written when the Codex leg starts on a non-standard transport, log
the configured MODE (`auto`, `std`, or `utls`) as `codex.transport_mode` (it
was `codex.transport`), which is distinct from that per-request `transport`
key and from `/healthz`'s live kind.

## Logging and traces

One structured line per request (`msg` = `request`), on stderr (launchd
captures it), carrying `request_id`, `method`, `path`, `status`, `req_bytes`,
`resp_bytes`, `ttfb_ms`, `total_ms`, `route` (`anthropic`, `codex`,
`deepseek`, or `discovery`), `client_model`, `upstream_model`,
`effort_requested`, `effort_applied`, `stream`, `upstream_status`,
`output_tokens`, `input_tokens`, `cache_read_input_tokens`,
`cache_creation_input_tokens`, `estimated_input_tokens`, `stop_reason`,
`interrupted`, `transport`, `err` when there was one, and a `headers` group
holding the four allowlisted request headers by value and, under
`headers_withheld`, the names of every other header that arrived (see
*Redaction* below).

`effort_requested` is the reasoning effort the client asked for: the
`-<level>` suffix on the model name, and nothing else. The router parses only
the suffix, so a name without one logs an EMPTY `effort_requested` — the key is
present on every routed request, Anthropic and DeepSeek included, and an empty
value means "the client asked for none", never a default the router resolved
(the `anthropic-beta`, config and catalog fallbacks are applied later, by the
Codex translator, and show up in `effort_applied`). Only a request that never
routed, such as a `/healthz` poll, lacks the key. `effort_applied` is the
effort actually sent upstream after those fallbacks and the catalog clamp; only
the Codex leg knows it, so it is absent on Anthropic and DeepSeek lines and on
a Codex request that failed before translation, and once known it is emitted
even when empty (a translation that sent no reasoning effort at all). So an
unsuffixed `sol` request reads `effort_requested="" effort_applied="low"` when
the catalog default is `low`. (Both replaced a single `effort` field that meant
the requested value before translation and the applied value after it.)

At DEBUG the dispatcher also writes a `routed` record carrying `route`,
`client_model`, `upstream_model`, `effort_requested` (the suffix value, empty
when the name had none), `effort_source` (`suffix` when a suffix was parsed,
otherwise `""` — the router never resolves the other sources) and `stream`.
(`route` was `backend`, and `effort_requested` was `effort`, before the
log-schema change that aligned the record with the request line.) The Codex
leg's own DEBUG records are listed under *Prompt caching* (the translation
record) and below (the stream record).

Two of those fields earn their place by being *differences*. `upstream_status`
is the status the BACKEND gave, which is not always the one you were answered
with — an upstream 200 whose body carried no events becomes a 502 downstream,
and an upstream 401 becomes a refresh and a retry. `interrupted` separates "you
hung up" from "it broke", so a cancelled turn never reads as an incident.

On the Codex and DeepSeek legs, `input_tokens`, `cache_read_input_tokens` and
`cache_creation_input_tokens` are the three components of the prompt under
Anthropic semantics, and the whole prompt is their sum. `input_tokens` is the
UNCACHED part billed at full price, `cache_read_input_tokens` the part served
from the cache, and `cache_creation_input_tokens` the part written into it, so
the hit rate is `cache_read_input_tokens / (input_tokens +
cache_read_input_tokens + cache_creation_input_tokens)`. The DeepSeek leg
reports all three from the upstream's own usage block. The Responses API
reports its `input_tokens` inclusive of the cached count and reports no
cache-write count at all; the translator subtracts the cached part out, so the
Codex leg's logged counts do not double count it, and its
`cache_creation_input_tokens` is the `0` the client was told. In a healthy
agentic loop the cached count tracks the prompt as the conversation grows and
`input_tokens` stays small. A cached count that stays FLAT while `input_tokens`
climbs means the replayed history has stopped matching what the model saw, and
every turn is paying full price for the whole conversation — see *Prompt
caching*.

`estimated_input_tokens` is the prompt count utraque computed locally and
seeded into `message_start` before the backend reported the real usage. It is
designed to be a lower bound of the billed prompt, so the invariant to watch is
`estimated_input_tokens <= input_tokens + cache_read_input_tokens`; a line that
breaks it is one on which ccusage's per-message dedup could have kept the seed
instead of the truth — see *Token counts and the message_start seed*.

**Redaction is by allowlist.** Exactly four request headers may be logged with
their values — `anthropic-version`, `anthropic-beta`, `content-type`,
`user-agent`. Every other header is named but never valued — the header group
lists them under `headers_withheld`, the same key the trace manifest uses (it
was `redacted` before the log-schema change) — so the shape of a request stays
debuggable without its contents being disclosed. "Redacted", and the
`[REDACTED]` marker, are reserved for a VALUE that was substituted: a withheld
header was never valued in the first place. `Authorization`,
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
`UTRAQUE_TRACE_DIR` writes `<stem>.request.json` for every request, and — for a
Codex request that got as far as opening a stream — `<stem>.upstream.sse` and
`<stem>.downstream.sse` beside it (a non-streaming answer lands in
`<stem>.downstream.json`). An Anthropic passthrough, a `/healthz` poll, a
`/v1/models` open, or a Codex request that failed before the stream opened leave
the manifest alone. The same redaction is applied, manifest included. They are
source material for test fixtures: the bytes received and the bytes sent, side
by side, can turn a translation bug into a reproducible case. **A trace holds
the prompt text and the model's output in the clear**, which is why enabling it
logs a loud `WARN` at startup.

A caller may supply `X-Request-Id`; when accepted, it is logged as
`request_id`, and a filesystem-safe form of the id plus a short hash names the
trace files: the `<stem>` is the id with every byte outside `[A-Za-z0-9_-]`
replaced by `_`, a hyphen, and the first eight hex characters of the id's
SHA-256, so `req/1` traces to `req_1-388947b8.request.json` while `req.1`,
which sanitises to the same `req_1`, gets a different hash and its own files.
(Without the hash, the second of two such requests would overwrite the first's
dump.) The manifest's `request_id` is the raw id; a trace-write failure is
logged with `request_id` (raw) and `trace_stem` side by side. Utraque echoes
the chosen request id in the separate `X-Utraque-Request-Id` response header,
leaving a passthrough response's `X-Request-Id` available to Anthropic. An id
that is itself credential-shaped is refused and a generated one used instead.
That is a backstop and not a guarantee: an opaque high-entropy string is
exactly what a request id looks like.

**Other per-request records.** The Codex leg writes one record per stream at
DEBUG (`codex stream translated`) or, when the translator met event types it
does not recognise, at INFO (`codex stream carried unrecognised event types`),
carrying `upstream_model`, `effort_requested`, `effort_applied`, `started`,
`terminated`, `errored` and, on the INFO form, `unknown_events` (the total) and
`unknown_event_types` (the per-type map) — the same spelling `/healthz`'s
`codex_stream` block uses. (Before the log-schema change `unknown_events` was
the map, and the record carried no effort.) On this record and the translation
record, `effort_requested` is the level the translator chose by precedence
(suffix, then `anthropic-beta`, config and the catalog default) before
clamping, so it equals the request line's suffix value whenever there was a
suffix and names the fallback otherwise; `effort_applied` is the level sent,
the same value the request line carries. Both are always present on the
stream record, empty when the translation sent no effort. A Codex call that
fails before its response starts is logged with `class` (`auth`, `rate_limit`,
`server_error`, `terminal`, `gate`, `network`, `timeout`), `upstream_status`
and `status`; `server_error` — an upstream 5xx — was spelled `upstream` before
the log-schema change. The error TEXT of such a failure, which is what the
request line's `err` and the trace manifest's `summary.err` carry, still reads
`codex responses: upstream (HTTP 5xx): …`; only the `class` attribute was
renamed. The Anthropic leg's sanitizer logs `stripped synthetic thinking
blocks` with `thinking_blocks_removed` (an int; it was `dropped`).

Every error the proxy produces itself, and every Codex or catalog failure it
renders from an upstream status, is an Anthropic error envelope whose `type`
is one of the values on the published
[Claude API errors](https://platform.claude.com/docs/en/api/errors) page: 400
`invalid_request_error`, 401 `authentication_error`, 402 `billing_error`, 403
`permission_error`, 404 `not_found_error`, 409 `conflict_error`, 413
`request_too_large`, 429 `rate_limit_error`, 500 `api_error`, 504
`timeout_error` (official, not a utraque extension) and 529
`overloaded_error`. A 402 and a 409 rendered from an upstream status are
`billing_error` and `conflict_error`; both were `api_error` (and, on a Codex
rejection, `invalid_request_error`) before the taxonomy was checked against
that page.

## Prompt caching

The Codex backend caches a prompt prefix and bills the cached part at a
discount. The cache is automatic — nothing turns it on — but it only pays out
while the conversation utraque replays still matches the token sequence the
model actually saw, and that sequence includes the **reasoning items** the model
emitted. Replay an assistant turn without them and the prefix diverges at the
first assistant message, so the hit stops there and never grows again: every
later turn re-reads the whole conversation at full price.

That is not hypothetical. Before this was fixed, real sessions ran at an 11.7%
cache hit rate — 246M prompt tokens in total, of which 28.7M were cached — while
the Codex CLI on the same backend runs at 95–99%. (Those figures were measured
when the log still carried the Responses API's inclusive `input_tokens`; under
today's Anthropic-semantics log the same sessions would read 217M uncached
against 28.7M cached, and the rate is `cached / (input + cached)` — the same
11.7%.) The tell was a `cache_read_input_tokens` pinned at exactly the same
number, turn after turn, while the prompt climbed from 56k to 113k.

Three things keep the prefix matching.

**Reasoning replay.** Every request asks for
`include: ["reasoning.encrypted_content"]`. Under `store:false` — which utraque
always sends, because it keeps no conversation on the backend — that encrypted
blob is the only form of a reasoning item that can be replayed at all. utraque
is stateless per request, so the blob has to come back from the client: it rides
in the signature of the synthetic thinking block utraque already mints for every
reasoning item, the client replays that block in the next turn's history, and
the request translator turns it back into a reasoning input item. The
Anthropic-leg sanitizer strips these blocks before they could reach Anthropic,
which signs its own thinking blocks and rejects one it did not issue.

A blob that does not come back is a cache miss, never a broken request. A
thinking block from a Claude turn, or one of ours minted before its encrypted
content arrived, is dropped exactly as before and counted as
`reasoning_unreplayable` on the translation log line.

**`prompt_cache_key`.** The backend routes a request to whichever machine holds
its cached prefix by hashing the prompt's opening tokens, and spills elsewhere
when one prefix draws too much concurrent traffic — which is what a fleet of
subagents looks like. The key makes that routing explicit. It hashes exactly the
part of a request that does not change as a conversation grows: the model, the
instructions, the tool declarations, and the opening input item.

**The client's billing header is dropped.** Claude Code prepends a system block
carrying its own billing metadata for Anthropic, including a `cch` value that
changes on every turn. It sits first, so left in it would change the opening
tokens of every prompt and invalidate the cached prefix from its first token —
no amount of reasoning replay further down would matter. It is addressed to
Anthropic's billing system and means nothing to the Codex backend, so the Codex
leg drops it and records `system:billing-header` as a drop. The Anthropic leg is
untouched: that is a byte-for-byte passthrough and the header reaches Anthropic
exactly as Claude Code wrote it.

**Serial tool calls.** The Codex leg sends `parallel_tool_calls:false` when a
request contains any tool in its built-in mutating-tools set or when the client
sets `tool_choice.disable_parallel_tool_use`. Its `MutatingTools` override is
programmatic only; there is no environment or configuration setting for it. The
DEBUG translation record (`translated a Messages request for the codex
backend`) says why with `parallel_tool_calls_reason` (`""` when the field was
left unset, `client_flag`, `tool_trigger`, or `both`) and, when a tool
triggered it, `parallel_tool_calls_triggers` (the sorted tool names). The
older bool `parallel_tool_calls_disabled` is still emitted beside them for one
release and will then be removed. The record also carries `upstream_model`,
`effort_requested`, `effort_applied` (it was `effort`), `effort_source`, and
`effort_clamped`, plus applicable `reasoning_replayed`,
`reasoning_unreplayable`, `dropped_params` (the Anthropic parameter names the
backend ignores) and `dropped_system_blocks` (the `system:*` markers; the two
were one `dropped` list), `orphaned_tool_results`, `dropped_images`,
`rewritten_patterns`, and `dropped_patterns`.

**`session_id`.** Derived from the same hash, so the header and the body always
name the same conversation. The Codex CLI sends one, the backend is
undocumented, and a request must never claim one identity in the header and a
different one in the body.

## Token counts and the message_start seed

Two numbers on the Codex leg are computed locally rather than reported by the
backend: the `usage.input_tokens` seeded into `message_start` before the
upstream has said anything, and the answer to `POST /v1/messages/count_tokens`,
which the Codex API does not offer and which drives Claude Code's context bar.
Both come from the real GPT tokenizer, o200k_base, compiled into the binary
(the vocabulary is embedded Go source; nothing is fetched at runtime).

**Why the seed must be a lower bound.** Claude Code writes one transcript line
per content block, all sharing one `message.id`. The first line carries the
`message_start` seed; the last carries the real usage from `message_delta`.
ccusage dedups lines sharing an id by keeping the one with the LARGER total
(`input + cache_create + cache_read + output`). A seed that overshoots the real
prompt by more than the answer's size therefore wins the dedup, and that turn's
cache reads and output vanish from the report. (The previous chars/4 estimate
did exactly this on short-answer turns.) So the seed is counted to stay at or
below what the backend bills:

- **Only text that actually goes upstream is counted** — the instructions,
  each message part, each function call's name and arguments, each function
  result, each tool's name, description and the prose inside its parameter
  schema (property names, descriptions, enum values, types; never the JSON
  punctuation or schema keywords, which the backend does not show the model).
  It is counted from the request AFTER translation, so the dropped billing
  header, dropped thinking text and the rest are already gone.
- **Each field is tokenized separately and the counts summed.** The backend
  frames every field in its own turn or declaration, so a per-field count is
  the faithful model.
- **No framing overheads are added.** The backend adds its own — role and turn
  markers, tool namespace syntax, a hidden preamble — and guessing at them
  could only push the count over.
- **Opaque blobs are skipped.** Encrypted reasoning items, image data URLs,
  anything base64: tokenizing ciphertext would overshoot wildly, and the
  backend counts the real content utraque cannot see.

**The consequence.** The count is knowingly low, and in one case low by a lot:
a session with heavy reasoning replay carries thousands of reasoning tokens
per turn inside the encrypted items, so `count_tokens` — and the context bar
it drives — reads a few percent low in such sessions. That is accepted. A low
bar is harmless; a seed that wins the dedup is not.

**Cost.** The count runs concurrently with the upstream request, never ahead
of it: `message_start` waits for it only if the tokenizer is slower than the
backend's first event, which on a warm cache it never is, and a request whose
client has gone away does not wait at all. Per-field counts are memoized in a
bounded in-process cache keyed by content hash, because an agentic loop
resends the same history every turn — a 400 KB prompt costs ~35 ms cold and
~55 µs on the next turn.

The work is bounded per field. The tokenizer library is quadratic in the
length of one regex piece — an unbroken run of letters, one repeated symbol,
or a block of blank lines — and a 400 KB run took nearly a minute. So a run
of one character class 1 KB or longer is never handed to it: the run, widened
to the nearest guaranteed piece boundaries, is charged the provable floor of
one token per 128 bytes (the longest o200k token), and everything else is
counted exactly. Ordinary prose, code and JSON never contain such a run; for
the rare field that does, the count reads low, which is the accepted
direction. A 400 KB field of such runs counts in ~250 ms at worst, and a
single 400 KB run in under a millisecond.

### Testing the round trip without spending quota

The one part of this that no unit test can settle is whether a real client
replays the synthetic thinking blocks with their signatures intact. That trip is
client → utraque → client and never reaches OpenAI, so `cmd/fakecodex` stands in
for the backend and answers it for free:

```
go run ./cmd/fakecodex -listen 127.0.0.1:8411
UTRAQUE_CODEX_BASE_URL=http://127.0.0.1:8411 utraque
# then drive a normal session against any gpt-* route
```

The blob size matters when testing this. Real `encrypted_content` runs about
1–11KB (measured across Codex CLI rollouts, mean ~2.5KB), so a stub that hands
out a short string proves nothing about whether a client returns a realistic one
intact. `-blob-bytes` sets the size and `-tool-turns` sets how many reasoning
items pile up in one conversation; the defaults are a realistic 3000 bytes and a
single tool call.

Measured with a real Claude Code session: 735 reasoning items totalling 2.2MB
came back in one request byte-for-byte, and single blobs of 100KB — an order of
magnitude past anything the real backend issues — survived intact. Nothing in
the chain truncates or rewrites a signature.

It prints a verdict per turn. `ROUND TRIP OK` means the client carried the
encrypted content back and utraque replayed it. `reasoning_replayed=0` turn
after turn on a growing conversation means the client is not carrying the blocks
back, and the stateless design does not hold.

## Short model names

Short names (`sol`, `sol-5.6`, `sol-high`) are derived from the model list the
Codex backend itself serves, not from a table compiled into the binary. Every
successful catalog read — the per-request lookup that clamps reasoning effort,
and a picker open — republishes them, so a codename OpenAI ships today starts
resolving as soon as anything reads the catalog, and a retired slug stops.
Until the first read succeeds, a compiled-in seed applies. Raw `gpt-*` slugs
always route regardless.

A model name that resolves to no leg at all is answered locally with a 404
`not_found_error` whose message lists the accepted model names — the
Anthropic globs, the DeepSeek ids, `gpt-*` and the bare aliases currently in
force — so the reply says what would have worked:

```json
{"type":"error","error":{"type":"not_found_error","message":"model \"banana\" not recognised; accepted model names: claude-*, anthropic-*, deepseek-flash, deepseek-v4-pro, gpt-*, 5.4, 5.4-mini, 5.5, luna, sol, terra"}}
```

(The message read "known route families" before the list was renamed: it
mixes globs, exact ids and bare aliases, and "family" now means nothing in
the router.) When the aliases are republished from the live catalog, the
INFO line `router aliases republished from the live codex catalog` carries
them as `bare_aliases` (was `families`).

## Health

`GET /healthz` is answered locally and never contacts any upstream. It
reports process status, version and uptime, plus, for the Codex leg:

- `codex_auth` — `state`: the credential state (`ok` / `stale` / `missing`),
  `reason` when it is `stale` (`expiring` for a token at or past expiry,
  `invalidated` when the backend rejected it; omitted otherwise), and
  `expires_in_s`, the seconds until the access token expires. The token value
  never appears. (`state` was `status`, and there was no `reason`, before the
  healthz revision that aligned it with `codex_catalog.state`.)
- `codex_catalog` — how many models the held snapshot holds, how old it is, and
  `state`: **why** it looks like that. A bare `models: 0` is several different
  situations wearing one face, so they are named — `loaded`, `empty` (a fetch
  succeeded and the backend really listed nothing), `failed` (with
  `last_error`), `unavailable` (no `codex login` here), `warming`, or `cold`.
  The catalog is also warmed in the background at startup, so the count is a
  fact about the backend rather than a fact about whether anyone has used the
  proxy yet.
- `codex_routing` — `bare_aliases`, the bare (rolling) aliases the router
  currently resolves: the quickest way to see whether the live catalog has
  been loaded or the compiled-in seed is still in force. (The key was
  `families` before the same revision.)
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

And, for the DeepSeek leg:

- `deepseek` — `configured`: whether the leg was built at all, which it is
  exactly when a DeepSeek API key was configured. Without it a `deepseek-*`
  request is answered 503 "deepseek leg is not configured"; this is that
  state, visible before the first request. The DeepSeek leg rides the
  Anthropic transport, so it has no entry of its own under `transport`.

## Provider report

`GET /v1/utraque/providers` returns schema version 1 JSON for Anthropic,
Codex, and DeepSeek. It is restricted to loopback clients. When
`UTRAQUE_LOCAL_TOKEN` is configured, the ordinary server middleware also
requires the matching `X-Utraque-Token`; without that optional setting, a
loopback caller needs no local-auth header. The caller must supply its Claude
OAuth bearer credential for the Anthropic
usage reading; utraque sends that bearer only to Anthropic's official usage
endpoint and does not capture it from OAuth files, the environment, or a
keychain.

```sh
curl -sS \
  -H 'Authorization: Bearer <claude-oauth-token>' \
  http://127.0.0.1:8317/v1/utraque/providers
```

Add `-H 'X-Utraque-Token: <local-token>'` when `UTRAQUE_LOCAL_TOKEN` is
configured.

Every response is `Cache-Control: no-store`. Utraque collects 30 inclusive UTC
dates of local `ccusage` history while reading each provider's live quota once.
The schema-v1 field remains named `quota_after` for compatibility; the one live
reading does not imply that it was taken after local-history collection. The
current collector never produces `quota_before` or `paired_measurement`; it sets
only `quota_after`. The collector records
`paired_quota_measurement_unavailable` on every fresh Anthropic build, never
the result of a paired measurement. Freshness post-processing may replace it
with `quota_window_reset_after_collection` or `cached_measurement_expired`.
Cached responses retain each source's original timestamp and
identify cached and stale sources explicitly. Providers fail independently, so a
DeepSeek balance can still be returned when local history fails, and local
history can still be returned when a live provider reading fails. A previous
complete snapshot may accompany a partial attempt as a separate stale object.

`last_success` on a provider is per attempt: it means this collection had at
least one successful quota or history section, including a partial collection.
`last_complete_snapshot.last_success` instead records the time a complete
measurement succeeded. `last_attempt` is normally the collection end time; for a
quota rate-limit error with a recorded real upstream attempt it uses that attempt
time, which can predate `collection_started_at` during a cooldown. That same
`quota_after` error then also carries the attempt time as `attempted_at`, so the
two meanings of `last_attempt` can be told apart; `attempted_at` is omitted from
every other error.

In schema version 1, `source` has a field-specific value space: `quota_after`
uses the leg name; `history.source` is `ccusage`; `models[].source` is the
ccusage agent label of the local log (such as `claude`, `codex`, or
`opencode`);
`history.blocks[].source` is always `claude`, because blocks are collected from
the Claude log only; `reference_prices.source` is `models.dev`; and
`configured_plan.source` is `configured`.

An abbreviated response looks like this:

```json
{
  "schema_version": 1,
  "generated_at": "2026-09-11T12:00:00Z",
  "collection_started_at": "2026-09-11T11:59:58Z",
  "collection_ended_at": "2026-09-11T12:00:00Z",
  "providers": [
    {
      "provider": "anthropic",
      "status": "ok",
      "source_freshness": {"cached": false, "stale": false, "age_seconds": 0},
      "quota_after": {
        "source": "anthropic",
        "collected_at": "2026-09-11T12:00:00Z",
        "quotas": [{"id": "five_hour", "used_percent": 31.5, "unit": "percent_0_100"}]
      },
      "history": {
        "source": "ccusage",
        "coverage": "local_only",
        "cost_basis": "calculated_api_reference_usd",
        "unit_prices_available": false,
        "unit_price_unavailable_reason": "ccusage_does_not_report_unit_prices"
      },
      "reference_prices": {
        "source": "models.dev",
        "observed_at": "2026-09-11T12:00:00Z",
        "unit": "usd_per_million_tokens",
        "models": [
          {"model": "claude-haiku-4-5", "input": 1, "output": 5, "cache_read": 0.1, "cache_write": 1.25, "eligible": true}
        ],
        "assumptions": ["cache_write_5m"]
      }
    }
  ]
}
```

The endpoint returns `200` even when one provider is partial or unavailable;
each provider carries its own status and classified errors. It returns the
normal local-auth `401` for a missing or wrong `X-Utraque-Token` when that
optional protection is configured, `403` for a non-loopback caller, and
`405` for methods other than GET or HEAD. Times are RFC 3339 UTC, durations and ages are seconds, percentages use
`percent_0_100`, token fields are counts, and provider balances retain decimal
strings plus their three-letter currency.

History is always labelled `local_only`; it is not whole-account coverage.
Costs are `calculated_api_reference_usd`, not subscription charges or prepaid
deductions. `ccusage` does not expose a current unit-price catalog, so per-model
effective rates are weighted historical observations and remain unavailable
when any included usage is unpriced.

Each provider may also carry `reference_prices`, an independent public
models.dev snapshot denominated in USD per million tokens. The `codex`
reference-price section carries the OpenAI API list prices that models.dev labels
`openai`. Model ids are exact author-catalog ids. `eligible: true` identifies
current selectable candidates:
the held live Codex routing catalog (or its startup seed), the built-in current
Claude fallback list, and the two DeepSeek routes when configured. A model seen
in local history is included with `eligible: false` when it is no longer in
that candidate set, which keeps old usage interpretable without making a
retired cheap model the estimate target. The Claude candidate flag therefore
reflects the fallback list rather than any credential-scoped live Anthropic
catalog. Optional cache fields are omitted when the source has no price for
them; `cache_write_5m` and `base_tier` state which source price was selected.

The public catalog read carries no provider or caller credential. Utraque caps
it at 8 MiB and five seconds, coalesces concurrent reads, revalidates its
five-minute cache with ETag, and returns its last good snapshot as `stale: true`
when a refresh fails. A catalog failure is reported under the
`reference_prices` section and does not block quota or history collection.

Remaining-token figures are conditional estimates. A DeepSeek USD balance can
be divided by a fully priced historical workload rate, with future price and
workload assumptions stated in the result. Other currencies remain null. The
routine report does not emit a Claude five-hour estimate because doing so would
require a second live quota request to bracket local-history collection. Its
calibration field instead reports `paired_quota_measurement_unavailable`; the
schema retains the older paired-measurement fields for compatibility.

When a provider quota endpoint returns `429`, utraque honors its `Retry-After`
time for that provider account. During that cooldown, reports return the safe
`rate_limited` classification and retry time without contacting the endpoint
again. Missing or unusable retry guidance uses a bounded increasing backoff.

`/healthz` remains network-free and contains none of this financial or usage
detail. Report failures do not affect inference routes.

## The model picker (merged `/v1/models`)

`utraque` serves its own `GET /v1/models`, merging Anthropic's model list with
the Codex models it can route to and, when configured, two static DeepSeek rows.
Set `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` in
the client's environment to turn discovery on — but note that it has no effect
while `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL` is set, which the Claude leg
needs. *Using a GPT route* explains the trade; this section describes what is
served to a client that does ask.

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
one. Note also that Claude Code will not request this catalog at all in the
recommended setup: `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1` disables its
gateway discovery, as *Using a GPT route* explains. The Anthropic half is served
from the static list in normal use, which is why the fallback, not the upstream
read, is the load-bearing path.

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
| `raw` | One row per raw upstream slug: `anthropic-compat.gpt-5.6-sol` |
| `off` | No Codex rows. GPT names still route when typed or set in agent frontmatter — they just don't appear in the picker. |

An id template that could not produce a filter-passing id is rejected at
startup rather than silently yielding a picker with no GPT rows in it.

### DeepSeek models

When a DeepSeek key is configured, discovery adds
`anthropic-compat.deepseek-flash` and
`anthropic-compat.deepseek-v4-pro`. The prefix exists solely to survive Claude
Code's model filter; the router sends the canonical model name upstream and
puts that same canonical name in non-stream responses and streamed
`message_start` events. These ids also resolve without the in-memory picker
registry, so a chosen model remains usable after the daemon restarts.

## Tests

```sh
go test -race ./...          # the whole suite; hermetic
```

**The default suite contacts nothing.** Every upstream in it is an
`httptest` server and every credential is a throwaway written under
`t.TempDir()`. The real `chatgpt.com`, the real `auth.openai.com`, the real
`api.anthropic.com`, the real `api.deepseek.com` and the real
`~/.codex/auth.json` are never read, written or contacted by `go test ./...`,
and the leak test drives the production logger at `debug` to prove no
token-shaped material reaches a log line.

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

## Is this allowed?

As far as we can tell, yes — for personal use of your own two subscriptions.
The two legs rest on different kinds of evidence, so they are worth separating.

**The Anthropic leg is documented behaviour.** Claude Code's own docs describe
pointing `ANTHROPIC_BASE_URL` at a gateway while a subscription login stays
active:

> Claude Code checks these plan requirements only when it connects to the
> Anthropic API directly. If you point `ANTHROPIC_BASE_URL` at an
> [LLM gateway](https://code.claude.com/docs/en/llm-gateway#subscriptions-and-gateways)
> and your saved claude.ai login stays the active credential, Claude Code
> doesn't check your plan's usage credits.

— [Model configuration](https://code.claude.com/docs/en/model-config). That is
exactly this arrangement: the client stays genuine Claude Code, the proxy holds
no Anthropic secret, and the subscription's own limits and billing apply.

**The OpenAI leg is endorsed in public but not written into the terms.** OpenAI
has repeatedly pointed people at this pattern. Romain Huet, OpenAI's Head of
Developer Experience,
[said in March 2026](https://x.com/romainhuet/status/2038699202834841962):

> "We want people to be able to use Codex, and their ChatGPT subscription,
> wherever they like! That means in the app, in the terminal, but also in
> JetBrains, Xcode, OpenCode, Pi, and now Claude Code."

More directly, Thibault Sottiaux, who led Codex, published a five-minute recipe
in July 2026 for pointing Claude Code at GPT-5.6 Sol through a third-party
translating proxy on a ChatGPT subscription —
[the same architecture as this project](https://x.com/thsottiaux/status/2076119366647894371).

The line OpenAI does draw is between personal reuse and resale: Codex
leadership has called out "sub2api" setups, which pool one subscription and
re-serve it as shared API traffic, as unsupported, while naming sign-in through
official or open-source clients as the supported path. `utraque` is single-user
by construction and has no multi-tenancy of any kind.

**What is missing is a written guarantee.** Asked directly whether "Sign in with
ChatGPT" is permitted from a forked or custom client, an OpenAI maintainer
[confirmed only that forking is fine under the Apache licence](https://github.com/openai/codex/discussions/8338)
and declined to answer the credential question. The endpoint is undocumented and
could change or be restricted without notice. Use your own subscription, for
yourself, and take that risk knowingly.

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
