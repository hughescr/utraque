#!/usr/bin/env bash
#
# install.sh — write utraque's launchd agent plist.
#
# It renders deploy/com.hughescr.utraque.plist.template into
# ~/Library/LaunchAgents/com.hughescr.utraque.plist and stops. It prints every
# file it touches and every launchctl command you will want, but it does NOT
# run launchctl unless you pass --load: loading an agent changes what is running
# on your machine, and that stays your decision.
#
# Idempotent: re-running with the same options reports "unchanged" and rewrites
# nothing.
#
# Usage: deploy/install.sh [options]
#   --binary PATH        utraque binary to run (default: ./bin/utraque, then $PATH)
#   --port PORT          port launchd binds (default: 8317)
#   --node HOST          address launchd binds (default: localhost, both loopback families)
#   --idle DURATION      idle self-exit, Go duration; 0 never exits (default: 1h)
#   --local-token TOKEN  UTRAQUE_LOCAL_TOKEN shared secret for the loopback port
#                        (visible in ps while this script runs; prefer the file
#                        or environment forms below)
#   --local-token-file F read the shared secret from F, or from stdin for '-'
#                        (also read from $UTRAQUE_LOCAL_TOKEN if neither is given)
#   --codex-home PATH    CODEX_HOME to write into the plist. launchd does NOT
#                        inherit your shell's CODEX_HOME, so pass this if you
#                        keep Codex somewhere other than ~/.codex -- otherwise
#                        the agent reads ~/.codex/auth.json and reports a
#                        missing credential (default: omitted)
#   --log-level LEVEL    debug|info|warn|error (default: info)
#   --log-format FORMAT  json|text (default: json)
#   --load               after writing, run the launchctl bootstrap for you
#   --force              rewrite the plist even when it is already identical
#   -h, --help           this text

set -euo pipefail

LABEL="com.hughescr.utraque"
SOCKET_NAME="Listener"

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd -- "$here/.." && pwd)
template="$here/$LABEL.plist.template"

agents_dir="$HOME/Library/LaunchAgents"
plist="$agents_dir/$LABEL.plist"
log_dir="$HOME/Library/Logs/utraque"
stdout_path="$log_dir/utraque.log"
stderr_path="$log_dir/utraque.log"

binary=""
port="8317"
node="localhost"
idle="1h"
local_token=""
local_token_file=""
codex_home=""
log_level="info"
log_format="json"
do_load=0
force=0

die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

usage() { sed -n '2,/^set -euo/{/^set -euo/d;s/^#//;s/^ //;p;}' "${BASH_SOURCE[0]}"; }

while [ $# -gt 0 ]; do
	case "$1" in
		--binary)      binary="${2:-}"; shift 2 ;;
		--port)        port="${2:-}"; shift 2 ;;
		--node)        node="${2:-}"; shift 2 ;;
		--idle)        idle="${2:-}"; shift 2 ;;
		--local-token) local_token="${2:-}"; shift 2 ;;
		--local-token-file) local_token_file="${2:-}"; shift 2 ;;
		--codex-home)  codex_home="${2:-}"; shift 2 ;;
		--log-level)   log_level="${2:-}"; shift 2 ;;
		--log-format)  log_format="${2:-}"; shift 2 ;;
		--load)        do_load=1; shift ;;
		--force)       force=1; shift ;;
		-h|--help)     usage; exit 0 ;;
		*)             die "unknown option $1 (try --help)" ;;
	esac
done

[ -f "$template" ] || die "template not found: $template"

# Resolve the binary: an explicit path, then the repo build, then $PATH.
if [ -z "$binary" ]; then
	if [ -x "$repo/bin/utraque" ]; then
		binary="$repo/bin/utraque"
	elif command -v utraque >/dev/null 2>&1; then
		binary=$(command -v utraque)
	elif command -v go >/dev/null 2>&1 && [ -x "$(go env GOBIN 2>/dev/null)/utraque" ]; then
		# `go install ./cmd/utraque` lands here, and this directory is often not
		# on PATH on a fresh Go setup, so command -v above would miss it.
		binary="$(go env GOBIN)/utraque"
	elif command -v go >/dev/null 2>&1 && [ -x "$(go env GOPATH 2>/dev/null)/bin/utraque" ]; then
		binary="$(go env GOPATH)/bin/utraque"
	else
		die "no utraque binary found. Build one with 'go build -o bin/utraque ./cmd/utraque', or install one with 'go install ./cmd/utraque', or pass --binary PATH."
	fi
fi
case "$binary" in
	/*) ;;
	*)  binary="$(cd -- "$(dirname -- "$binary")" && pwd)/$(basename -- "$binary")" ;;
esac
[ -x "$binary" ] || die "not an executable file: $binary"

# Every rendered value is validated HERE. utraque validates its own
# configuration before it reaches launchd's socket, so a value launchd accepts
# but utraque rejects makes the daemon exit 1 on every activation — and with
# ThrottleInterval 1 in the template, that is a respawn loop once a second.
# Stopping now is much easier to debug than reading launchd's log to find out.
case "$port" in
	''|*[!0-9]*) die "--port must be a number, got '$port'" ;;
esac
if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
	die "--port must be between 1 and 65535, got '$port'"
fi

[ -n "$node" ] || die "--node must not be empty"
case "$node" in
	*[!A-Za-z0-9.:_-]*) die "--node must be a hostname or an IP literal, got '$node'" ;;
esac

# UTRAQUE_LISTEN is parsed with net.SplitHostPort, so an IPv6 literal has to be
# bracketed. Without this, '--node ::1' renders '::1:8317', which utraque
# rejects at startup — the respawn loop above, from an address that is fine.
# SockNodeName in the plist takes the bare form, so only the env var changes.
case "$node" in
	*:*) listen="[$node]:$port" ;;
	*)   listen="$node:$port" ;;
esac

# A Go duration, which is what config.LoadFrom parses. '0' means never exit.
if ! printf '%s' "$idle" | grep -Eq '^(0|([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+)$'; then
	die "--idle must be a Go duration such as 90s, 1h or 1h30m (or 0 to never self-exit), got '$idle'"
fi

case "$log_level" in
	debug|info|warn|error) ;;
	*) die "--log-level must be debug|info|warn|error, got '$log_level'" ;;
esac
case "$log_format" in
	json|text) ;;
	*) die "--log-format must be json|text, got '$log_format'" ;;
esac

# The shared secret, in decreasing order of how much of it ends up in `ps` and
# in your shell history: a file (or stdin), the environment, then argv.
if [ -n "$local_token_file" ]; then
	[ -z "$local_token" ] || die "pass --local-token or --local-token-file, not both"
	if [ "$local_token_file" = "-" ]; then
		local_token=$(cat)
	else
		[ -r "$local_token_file" ] || die "cannot read --local-token-file: $local_token_file"
		local_token=$(cat -- "$local_token_file")
	fi
	local_token=${local_token%%$'\n'*}
	[ -n "$local_token" ] || die "--local-token-file gave an empty token"
elif [ -z "$local_token" ] && [ -n "${UTRAQUE_LOCAL_TOKEN:-}" ]; then
	local_token="$UTRAQUE_LOCAL_TOKEN"
fi

# Binding off loopback publishes both subscriptions to the network. With no
# shared secret that is unauthenticated spending by anything that can route to
# this machine, so it is refused rather than warned about. A token makes it a
# deliberate choice, which is the user's to make.
case "$node" in
	localhost|Localhost|LOCALHOST|127.*|::1|0:0:0:0:0:0:0:1) ;;
	*)
		[ -n "$local_token" ] || die "--node '$node' is not loopback, so the port would be reachable from your network with no authentication at all. Pass a shared secret too (--local-token-file, or \$UTRAQUE_LOCAL_TOKEN), or bind loopback."
		say "WARNING: --node '$node' is not loopback. Both subscriptions will be reachable from your network, gated only by the shared secret." ;;
esac

# launchd re-executes the recorded path, so a binary inside a build directory
# that gets cleaned leaves the agent broken until the next install.
case "$binary" in
	*/bin/utraque)
		say "note: the agent will run $binary — reinstall if you move or clean that path" ;;
esac

xml_escape() {
	printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

# Optional entries appended to EnvironmentVariables, tab-indented to match.
extra_env=""
add_env() {
	extra_env="$extra_env		<key>$1</key>
		<string>$(xml_escape "$2")</string>
"
}
if [ -n "$local_token" ]; then add_env "UTRAQUE_LOCAL_TOKEN" "$local_token"; fi
if [ -n "$codex_home" ]; then add_env "CODEX_HOME" "$codex_home"; fi

template_text=$(cat "$template")
rendered="$template_text"
substitute() {
	rendered="${rendered//@$1@/$2}"
}
substitute LABEL             "$(xml_escape "$LABEL")"
substitute PROGRAM           "$(xml_escape "$binary")"
substitute SOCKET_NAME       "$(xml_escape "$SOCKET_NAME")"
substitute SOCK_NODE         "$(xml_escape "$node")"
substitute SOCK_PORT         "$(xml_escape "$port")"
substitute LISTEN            "$(xml_escape "$listen")"
substitute IDLE_TIMEOUT      "$(xml_escape "$idle")"
substitute LOG_LEVEL         "$(xml_escape "$log_level")"
substitute LOG_FORMAT        "$(xml_escape "$log_format")"
substitute STDOUT            "$(xml_escape "$stdout_path")"
substitute STDERR            "$(xml_escape "$stderr_path")"
substitute EXTRA_ENVIRONMENT "$extra_env"

say "utraque launchd agent"
say "  label      $LABEL"
say "  binary     $binary"
say "  socket     $SOCKET_NAME -> $listen (launchd binds it; utraque adopts it)"
say "  idle exit  $idle"
say "  logs       $stdout_path"
if [ -n "$local_token" ]; then
	say "  local auth UTRAQUE_LOCAL_TOKEN is set (the plist is written mode 600)"
else
	say "  local auth none — any local process can spend both subscriptions through this port"
fi
say ""

# Validate before touching anything installed: a malformed plist that launchd
# refuses is much harder to debug than a script that stops here.
tmp=$(mktemp "${TMPDIR:-/tmp}/utraque-plist.XXXXXX")
chmod 600 "$tmp"
backup=""
staged=""
cleanup() { rm -f "$tmp" ${staged:+"$staged"} ${backup:+"$backup"}; }
trap cleanup EXIT
printf '%s\n' "$rendered" > "$tmp"
if ! plutil -lint "$tmp" >/dev/null; then
	die "the rendered plist is not valid; nothing was installed"
fi

mkdir -p "$agents_dir"
say "ensured  $agents_dir"
mkdir -p "$log_dir"
say "ensured  $log_dir"

if [ -f "$plist" ] && [ "$force" -eq 0 ] && cmp -s "$tmp" "$plist"; then
	# Re-assert the mode even when the contents are identical: the file may hold
	# UTRAQUE_LOCAL_TOKEN and something else may have loosened it since.
	chmod 600 "$plist"
	say "unchanged $plist (mode 600)"
else
	action="wrote"
	if [ -f "$plist" ]; then
		action="updated"
		# Kept only so --load can put the working agent back if the new plist
		# fails to bootstrap. Removed by the EXIT trap either way.
		backup=$(mktemp "${TMPDIR:-/tmp}/utraque-plist-prev.XXXXXX")
		chmod 600 "$backup"
		cat "$plist" > "$backup"
	fi
	# Stage inside the target directory and rename over the destination: the
	# rename is atomic, it cannot be redirected by a symlink sitting at $plist,
	# and the mode is right BEFORE any content exists rather than a chmod later
	# — which, with a token in the file, was a world-readable window.
	staged=$(mktemp "$agents_dir/.$LABEL.plist.XXXXXX")
	chmod 600 "$staged"
	cat "$tmp" > "$staged"
	mv -f "$staged" "$plist"
	staged=""
	say "$action  $plist (mode 600)"
fi
say ""

domain="gui/$(id -u)"
if [ "$do_load" -eq 1 ]; then
	say "loading the agent (--load was given)"
	booted_out=0
	if launchctl print "$domain/$LABEL" >/dev/null 2>&1; then
		say "+ launchctl bootout $domain/$LABEL"
		launchctl bootout "$domain/$LABEL" || true
		booted_out=1
	fi
	say "+ launchctl bootstrap $domain $plist"
	if ! launchctl bootstrap "$domain" "$plist"; then
		# The bootout above already stopped whatever was working. Put it back
		# rather than leaving the machine with no agent at all.
		if [ -n "$backup" ] && [ -f "$backup" ]; then
			say "bootstrap failed; restoring the previous plist and reloading it"
			cat "$backup" > "$plist"
			chmod 600 "$plist"
			if launchctl bootstrap "$domain" "$plist"; then
				say "the previous agent is loaded again; the new plist was not installed"
			else
				say "the previous agent could not be reloaded either — NOTHING is loaded now"
			fi
		elif [ "$booted_out" -eq 1 ]; then
			# No backup exists because the plist on disk was already identical --
			# but an agent WAS running and the bootout above stopped it. The
			# installed plist is still the right one, so just load it again.
			say "bootstrap failed; the plist was unchanged, so reloading it as-is"
			if launchctl bootstrap "$domain" "$plist"; then
				say "the previous agent is loaded again"
			else
				say "the previous agent could not be reloaded either -- NOTHING is loaded now"
			fi
		else
			say "nothing was loaded before this, so nothing was rolled back"
		fi
		die "launchctl bootstrap failed"
	fi
	say ""
	say "loaded. launchd now holds $listen; the first request starts utraque."
else
	say "Nothing has been loaded. To start it, run:"
	say ""
	say "    launchctl bootstrap $domain $plist"
	say ""
	say "If a previous version is already loaded, bootout first:"
	say ""
	say "    launchctl bootout $domain/$LABEL"
	say "    launchctl bootstrap $domain $plist"
fi
say ""
say "Then, to confirm it works:"
say ""
say "    launchctl print $domain/$LABEL      # launchd's view of the job"
say "    curl -s http://$listen/healthz      # first request; this is what starts utraque"
say "    tail -f $stdout_path"
say ""
say "Point Claude Code at it with:  ANTHROPIC_BASE_URL=http://$listen"
say "Uninstall with:               $here/uninstall.sh"
