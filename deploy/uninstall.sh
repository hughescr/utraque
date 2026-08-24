#!/usr/bin/env bash
#
# uninstall.sh — remove utraque's launchd agent plist.
#
# Like install.sh, it prints what it does and does NOT run launchctl unless you
# pass --unload. Removing the plist alone does not stop a loaded agent: launchd
# keeps the job (and the socket) until it is booted out, so the usual order is
# --unload, or bootout by hand first.
#
# Idempotent: running it twice reports that there was nothing left to do.
#
# Usage: deploy/uninstall.sh [options]
#   --unload      run the launchctl bootout before removing the plist
#   --keep-logs   leave ~/Library/Logs/utraque in place (the default)
#   --purge-logs  delete ~/Library/Logs/utraque too
#   -h, --help    this text

set -euo pipefail

LABEL="com.hughescr.utraque"

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
plist="$HOME/Library/LaunchAgents/$LABEL.plist"
log_dir="$HOME/Library/Logs/utraque"

do_unload=0
purge_logs=0

say() { printf '%s\n' "$*"; }
die() { printf 'uninstall.sh: %s\n' "$*" >&2; exit 1; }
usage() { sed -n '2,/^set -euo/{/^set -euo/d;s/^#//;s/^ //;p;}' "${BASH_SOURCE[0]}"; }

while [ $# -gt 0 ]; do
	case "$1" in
		--unload)     do_unload=1; shift ;;
		--keep-logs)  purge_logs=0; shift ;;
		--purge-logs) purge_logs=1; shift ;;
		-h|--help)    usage; exit 0 ;;
		*)            die "unknown option $1 (try --help)" ;;
	esac
done

domain="gui/$(id -u)"
loaded=0
if launchctl print "$domain/$LABEL" >/dev/null 2>&1; then loaded=1; fi

say "utraque launchd agent"
say "  label   $LABEL"
say "  plist   $plist"
if [ "$loaded" -eq 1 ]; then
	say "  state   loaded in $domain"
else
	say "  state   not loaded"
fi
say ""

if [ "$do_unload" -eq 1 ]; then
	# Ask launchd to boot the job out unconditionally and read its answer,
	# rather than deciding from the `launchctl print` above. `print` fails for
	# reasons other than "not loaded", and treating any failure as "nothing to
	# do" would go on to remove the plist while the job — and the socket it
	# holds — were still live, leaving nothing to reinstall from.
	say "+ launchctl bootout $domain/$LABEL"
	set +e
	bootout_output=$(launchctl bootout "$domain/$LABEL" 2>&1)
	bootout_rc=$?
	set -e
	case "$bootout_rc" in
		0)
			say "booted out. launchd has released the listening socket." ;;
		3|113)
			# ESRCH / "Could not find specified service": it really was not loaded.
			say "nothing to boot out (launchd does not have this job)" ;;
		*)
			if [ -n "$bootout_output" ]; then say "$bootout_output"; fi
			die "launchctl bootout failed (exit $bootout_rc); the plist was left in place so you can retry" ;;
	esac
elif [ "$loaded" -eq 1 ]; then
	say "The agent is still loaded and launchd still holds the port."
	say "Nothing has been unloaded. To stop it, run:"
	say ""
	say "    launchctl bootout $domain/$LABEL"
	say ""
fi

if [ -f "$plist" ]; then
	rm -f "$plist"
	say "removed  $plist"
else
	say "absent   $plist (nothing to remove)"
fi

if [ "$purge_logs" -eq 1 ]; then
	if [ -d "$log_dir" ]; then
		rm -rf "$log_dir"
		say "removed  $log_dir"
	else
		say "absent   $log_dir"
	fi
elif [ -d "$log_dir" ]; then
	say "kept     $log_dir (pass --purge-logs to delete it)"
fi

say ""
say "Nothing outside launchd was touched: your ~/.codex/auth.json, the utraque"
say "binary and any ANTHROPIC_BASE_URL you set elsewhere are all still there."
say "Reinstall with: $here/install.sh"
