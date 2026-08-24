# Running utraque on demand with launchd

utraque is not meant to be a resident daemon. On macOS, launchd binds and owns
the listening socket, and starts utraque only when a connection actually
arrives. After an idle period utraque exits; launchd keeps the socket and starts
it again on the next request. Nothing is running between sessions, and the
client never notices the gap.

```
  Claude Code ──connect──▶ 127.0.0.1:8317   (socket owned by launchd, always up)
                                │
                                │ first connection
                                ▼
                          launchd starts utraque
                                │
                                │ launch_activate_socket("Listener")
                                ▼
                          utraque adopts the socket and serves
                                │
                                │ 1h with no request
                                ▼
                          utraque exits; launchd keeps the socket
```

## What is in here

| File | What it is |
|---|---|
| `com.hughescr.utraque.plist.template` | The launchd agent, with `@PLACEHOLDER@` values |
| `install.sh` | Renders the template into `~/Library/LaunchAgents/` |
| `uninstall.sh` | Removes it again |

The label is `com.hughescr.utraque`, and the plist installs as
`~/Library/LaunchAgents/com.hughescr.utraque.plist`. It is a **LaunchAgent**
(per-user), not a LaunchDaemon: utraque reads the Codex credential out of your
own `~/.codex/auth.json` and must run as you.

## Install

```sh
go build -o bin/utraque ./cmd/utraque
deploy/install.sh
```

`install.sh` writes the plist and prints the `launchctl` command to run. It does
not load anything on its own — loading an agent changes what runs on your
machine, so that stays an explicit step:

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.hughescr.utraque.plist
```

Pass `--load` if you would rather `install.sh` did the bootstrap for you (it
boots out any previous copy first).

Re-running `install.sh` is safe: identical input rewrites nothing and says
`unchanged`.

### Options worth knowing

This is a runnable command, not an annotated one — a comment after a line
continuation ends the command, so the options below carry their notes in prose:

```sh
openssl rand -hex 16 > ~/.utraque-token
chmod 600 ~/.utraque-token

deploy/install.sh \
  --binary /usr/local/bin/utraque \
  --port 8317 \
  --node localhost \
  --idle 1h \
  --local-token-file ~/.utraque-token \
  --log-level info --log-format json
```

`--binary` defaults to `./bin/utraque`, then whatever is on `$PATH`. `--port` is
the port launchd binds. `--node localhost` binds both loopback families.
`--idle` takes a Go duration, and `0` means never self-exit.

A shared secret is strongly recommended. Without one, **any** local process can
spend both of your subscriptions through the loopback port. With one, callers
must send `X-Utraque-Token`, and the plist — which holds the secret — is written
mode `600`. Prefer `--local-token-file` (or `-` to read stdin, or the
`UTRAQUE_LOCAL_TOKEN` environment variable): `--local-token` still works, but an
argv value is visible in `ps` for as long as the script runs.

`--node localhost` makes launchd bind both `127.0.0.1` and `[::1]`, so it does
not matter which one the client resolves to; utraque serves every descriptor
launchd hands over. Use `--node 127.0.0.1` for IPv4 only. Anything off loopback
— `0.0.0.0` above all — exposes both subscriptions to your network, so
`install.sh` refuses it outright unless you also supply a shared secret, and
warns even then. An IPv6 literal is accepted in bare form and bracketed for you
where it has to be.

## Verify

```sh
launchctl print gui/$(id -u)/com.hughescr.utraque   # launchd's view of the job
curl -s http://localhost:8317/healthz | jq          # this request is what starts it
tail -f ~/Library/Logs/utraque/utraque.log
```

The first `curl` is the interesting one: before it, `launchctl print` shows a job
with no PID, and the port still answers because launchd is holding it. After it,
the job has a PID and the log shows `inherited the listening socket from
launchd`.

To watch the whole cycle, install with `--idle 30s`, make one request, wait, and
watch the process disappear from `launchctl print` while `curl` keeps working.

## Point Claude Code at it

```sh
export ANTHROPIC_BASE_URL=http://localhost:8317
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
```

## Uninstall

```sh
deploy/uninstall.sh --unload
```

Or, by hand:

```sh
launchctl bootout gui/$(id -u)/com.hughescr.utraque
rm ~/Library/LaunchAgents/com.hughescr.utraque.plist
```

Removing the plist alone does not stop a loaded agent — launchd keeps the job,
and the socket, until it is booted out.

## How it works, and what to change if it breaks

**Socket adoption.** Pure Go cannot ask launchd for the inherited descriptors:
`launch_activate_socket(3)` is a C entry point with no syscall equivalent. So
`internal/launchd` carries a darwin-only cgo shim that calls it, converts each
descriptor with `net.FileListener`, and serves them all. One `Sockets` entry can
produce several descriptors (one per address family), which is why utraque
accepts a set of listeners rather than one.

**Fallback.** When `launch_activate_socket` reports `ESRCH` ("not managed by
launchd") or `ENOENT` ("no socket by that name") — the normal answers for a
manual start — utraque binds `UTRAQUE_LISTEN` itself and logs why. `go run
./cmd/utraque` therefore behaves exactly as it did before socket activation
existed, and so does a `CGO_ENABLED=0` build, which cannot make the call at all.
Any *other* failure is fatal rather than falling back: if launchd really does
hold the socket, binding the same address ourselves would only collide with it.

**Idle exit and streaming.** The idle timer is held open for the whole of every
request, so a streamed answer that says nothing for an hour cannot trigger an
exit mid-stream. When the timer does fire it cancels the serving context, which
stops accepting and then waits up to 25 seconds for in-flight responses to
finish. `ExitTimeOut` in the plist is 30 seconds so launchd cannot `SIGKILL`
through that drain.

**Idle default.** utraque only defaults to a 1h self-exit when launchd handed it
the socket. Started by hand it defaults to never exiting, because there would be
nothing to bring it back. `UTRAQUE_IDLE_TIMEOUT` overrides both directions;
`0` means never.

**Re-activation latency.** `ThrottleInterval` is 1 second. launchd's 10-second
default would stall the first request after an idle exit behind the throttle.

### Common problems

| Symptom | Cause |
|---|---|
| `Bootstrap failed: 5: Input/output error` | A copy is already loaded — `launchctl bootout` first |
| Port answers, nothing ever starts | `Sockets` key name and `UTRAQUE_LAUNCHD_SOCKET` disagree |
| `launchd: this process holds no socket by that name` in the log | Same disagreement, seen from utraque's side; it fell back to its own bind |
| Job respawns constantly | utraque is exiting at startup — read the log; `launchctl print` shows the last exit status |
| Requests fail after a code change | launchd runs the path recorded in the plist; rebuild in place or re-run `install.sh` |
