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
#   --codex-home PATH    CODEX_HOME for the agent (default: inherit/none)
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
	else
		die "no utraque binary found. Build one with 'go build -o bin/utraque ./cmd/utraque', or pass --binary PATH."
	fi
fi
case "$binary" in
	/*) ;;
	*)  binary="$(cd -- "$(dirname -- "$binary")" && pwd)/$(basename -- "$binary")" ;;
esac
[ -x "$binary" ] || die "not an executable file: $binary"

case "$port" in
	''|*[!0-9]*) die "--port must be a number, got '$port'" ;;
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
substitute LISTEN            "$(xml_escape "$node:$port")"
substitute IDLE_TIMEOUT      "$(xml_escape "$idle")"
substitute LOG_LEVEL         "$(xml_escape "$log_level")"
substitute LOG_FORMAT        "$(xml_escape "$log_format")"
substitute STDOUT            "$(xml_escape "$stdout_path")"
substitute STDERR            "$(xml_escape "$stderr_path")"
substitute EXTRA_ENVIRONMENT "$extra_env"

say "utraque launchd agent"
say "  label      $LABEL"
say "  binary     $binary"
say "  socket     $SOCKET_NAME -> $node:$port (launchd binds it; utraque adopts it)"
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
trap 'rm -f "$tmp"' EXIT
printf '%s\n' "$rendered" > "$tmp"
if ! plutil -lint "$tmp" >/dev/null; then
	die "the rendered plist is not valid; nothing was installed"
fi

mkdir -p "$agents_dir"
say "ensured  $agents_dir"
mkdir -p "$log_dir"
say "ensured  $log_dir"

if [ -f "$plist" ] && [ "$force" -eq 0 ] && cmp -s "$tmp" "$plist"; then
	say "unchanged $plist"
else
	action="wrote"
	if [ -f "$plist" ]; then action="updated"; fi
	cat "$tmp" > "$plist"
	chmod 600 "$plist"
	say "$action  $plist (mode 600)"
fi
say ""

domain="gui/$(id -u)"
if [ "$do_load" -eq 1 ]; then
	say "loading the agent (--load was given)"
	if launchctl print "$domain/$LABEL" >/dev/null 2>&1; then
		say "+ launchctl bootout $domain/$LABEL"
		launchctl bootout "$domain/$LABEL" || true
	fi
	say "+ launchctl bootstrap $domain $plist"
	launchctl bootstrap "$domain" "$plist"
	say ""
	say "loaded. launchd now holds $node:$port; the first request starts utraque."
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
say "    curl -s http://$node:$port/healthz  # first request; this is what starts utraque"
say "    tail -f $stdout_path"
say ""
say "Point Claude Code at it with:  ANTHROPIC_BASE_URL=http://$node:$port"
say "Uninstall with:               $here/uninstall.sh"
