#!/usr/bin/env bash

# Regression coverage for deploy/install.sh's loopback check: which --node
# values may be installed without a shared secret.
#
# Part 1 asks the installer's own classifier through --check-node, which
# touches nothing, so the accept/reject table runs on every host (including
# the Ubuntu CI job). Part 2 runs the whole installer against a disposable
# HOME and never passes --load, so it only writes a plist under that HOME; a
# launchctl stub that fails the test sits first on PATH to prove nothing is
# loaded. Part 2 needs macOS (the installer lints the plist with plutil) and
# is skipped, with a note, where plutil is absent.

set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
installer="$root/deploy/install.sh"

tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/utraque-install-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp_root"' EXIT HUP INT TERM

tests_run=0
failures=0

ok() { printf 'ok %d - %s\n' "$tests_run" "$1"; }
not_ok() {
	printf 'not ok %d - %s\n' "$tests_run" "$1"
	failures=$((failures + 1))
}

finish() {
	printf '1..%d\n' "$tests_run"
	if [ "$failures" -ne 0 ]; then
		printf '# %d of %d failed\n' "$failures" "$tests_run" >&2
		exit 1
	fi
	exit 0
}

# ---------------------------------------------------------------------------
# Part 1: the classification table, on every host.
# ---------------------------------------------------------------------------

loopback_nodes=(
	127.0.0.1 127.1.2.3 127.255.255.255 127.0.0.0
	localhost LocalHost LOCALHOST ::1 0:0:0:0:0:0:0:1
)
# Anchoring cases: each of these CONTAINS a loopback literal but is not one.
# Dropping the ^ or $ from the dotted-quad pattern, or letting command
# substitution eat a trailing newline, lets one of them through.
non_loopback_nodes=(
	127.attacker.example 127.0.0.1.evil 127.256.0.1 127.0.0.256 127.1 127.0
	127.0.0 127.00.0.1 127.0.0.01 localhost.evil.example notlocalhost
	0.0.0.0 192.168.1.5 ::2 :: utraque.example.test
	evil.127.0.0.1 x127.0.0.1 a127.0.0.1 127.0.0.1x 127.0.0.1. .127.0.0.1
	" 127.0.0.1" "127.0.0.1 " " 127.0.0.1 " " localhost" "localhost "
	$'127.0.0.1\n' $'\n127.0.0.1' $'127.0.0.1\nevil.example' $'localhost\n'
	""
)

check_node() {
	local node=$1 want=$2 rc=0
	tests_run=$((tests_run + 1))
	"$installer" --check-node "$node" >/dev/null 2>&1 || rc=$?
	if [ "$want" = yes ] && [ "$rc" -eq 0 ]; then
		ok "$(printf 'classifier: %q is loopback' "$node")"
	elif [ "$want" = no ] && [ "$rc" -eq 1 ]; then
		ok "$(printf 'classifier: %q is not loopback' "$node")"
	else
		not_ok "$(printf 'classifier: %q wanted loopback=%s, --check-node exited %d' "$node" "$want" "$rc")"
	fi
}

for node in "${loopback_nodes[@]}"; do
	check_node "$node" yes
done
for node in "${non_loopback_nodes[@]}"; do
	check_node "$node" no
done

# --check-node must not run the installer: with no binary on the machine and a
# HOME that does not exist it still answers, and creates nothing under it.
tests_run=$((tests_run + 1))
side_effect_home="$tmp_root/side-effect-home"
if env -i HOME="$side_effect_home" PATH="/usr/bin:/bin" "$installer" --check-node 127.0.0.1 >/dev/null 2>&1 &&
	[ ! -e "$side_effect_home" ]; then
	ok "--check-node has no side effects"
else
	not_ok "--check-node touched HOME or failed without a binary"
fi

tests_run=$((tests_run + 1))
if "$installer" --check-node >/dev/null 2>&1; then
	not_ok "--check-node without a value succeeded"
else
	ok "--check-node without a value is an error"
fi

# ---------------------------------------------------------------------------
# Part 2: the full installer, macOS only.
# ---------------------------------------------------------------------------

if ! command -v plutil >/dev/null 2>&1; then
	printf '# SKIP full-install cases: install.sh needs plutil (macOS)\n'
	finish
fi

stub_bin="$tmp_root/bin"
mkdir -p "$stub_bin"
launchctl_log="$tmp_root/launchctl-called"
cat >"$stub_bin/launchctl" <<EOF
#!/bin/sh
echo "\$*" >>"$launchctl_log"
exit 97
EOF
chmod 700 "$stub_bin/launchctl"

fake_binary="$tmp_root/utraque"
printf '#!/bin/sh\nexit 0\n' >"$fake_binary"
chmod 700 "$fake_binary"

token_file="$tmp_root/token"
printf 'canary-install-token-5Fh2\n' >"$token_file"
chmod 600 "$token_file"

# run_install HOME [args...]: the installer in a clean environment, with no
# inherited UTRAQUE_LOCAL_TOKEN and the launchctl stub first on PATH.
run_install() {
	local home=$1
	shift
	env -i HOME="$home" PATH="$stub_bin:/usr/bin:/bin:/usr/sbin:/sbin" TMPDIR="$tmp_root" \
		"$installer" --binary "$fake_binary" "$@"
}

# expect NODE ACCEPTED_WITHOUT_TOKEN
expect() {
	local node=$1 loopback=$2 home out plist
	tests_run=$((tests_run + 1))
	home=$(mktemp -d "$tmp_root/home.XXXXXX")
	plist="$home/Library/LaunchAgents/com.hughescr.utraque.plist"
	if out=$(run_install "$home" --node "$node" 2>&1); then
		if [ "$loopback" = yes ]; then
			ok "--node $node is loopback: accepted without a token"
		else
			not_ok "--node $node is not loopback but was accepted without a token"
			printf '%s\n' "$out" | sed 's/^/#   /'
		fi
	else
		if [ "$loopback" = yes ]; then
			not_ok "--node $node is loopback but was refused without a token"
			printf '%s\n' "$out" | sed 's/^/#   /'
		elif ! printf '%s' "$out" | grep -q 'is not loopback'; then
			not_ok "--node $node was refused for the wrong reason"
			printf '%s\n' "$out" | sed 's/^/#   /'
		elif [ -e "$plist" ]; then
			not_ok "--node $node was refused but a plist was written anyway"
		else
			ok "--node $node is not loopback: refused without a token"
		fi
	fi

	if [ "$loopback" = no ]; then
		tests_run=$((tests_run + 1))
		home=$(mktemp -d "$tmp_root/home.XXXXXX")
		plist="$home/Library/LaunchAgents/com.hughescr.utraque.plist"
		if ! out=$(run_install "$home" --node "$node" --local-token-file "$token_file" 2>&1); then
			not_ok "--node $node with a token was refused"
			printf '%s\n' "$out" | sed 's/^/#   /'
		elif ! printf '%s' "$out" | grep -q 'WARNING: --node'; then
			not_ok "--node $node with a token was accepted without the network-exposure warning"
		elif [ -z "$(find "$plist" -perm 600 2>/dev/null)" ]; then
			not_ok "--node $node with a token: plist is missing or not mode 600"
		else
			ok "--node $node with a token: accepted with a warning, plist mode 600"
		fi
	fi
}

for node in 127.0.0.1 127.1.2.3 127.255.255.255 localhost LocalHost LOCALHOST ::1 0:0:0:0:0:0:0:1; do
	expect "$node" yes
done
for node in 127.attacker.example 127.0.0.1.evil 127.256.0.1 127.0.0.256 127.1 127.0.0 \
	127.00.0.1 127.0.0.01 localhost.evil.example notlocalhost 0.0.0.0 192.168.1.5 \
	::2 :: utraque.example.test evil.127.0.0.1; do
	expect "$node" no
done

# The printed ANTHROPIC_BASE_URL hint: a DNS name, which may resolve to
# loopback where utraque refuses it as a Host, gets one extra line pointing at
# http://127.0.0.1:PORT; loopback spellings and IP literals do not.
expect_hint() {
	local node=$1 want=$2 home out note
	tests_run=$((tests_run + 1))
	home=$(mktemp -d "$tmp_root/home.XXXXXX")
	note=0
	if ! out=$(run_install "$home" --node "$node" --port 8317 --local-token-file "$token_file" 2>&1); then
		not_ok "--node $node: install failed while checking the base-URL hint"
		printf '%s\n' "$out" | sed 's/^/#   /'
		return
	fi
	if printf '%s\n' "$out" | grep -qF "use http://127.0.0.1:8317 (or http://localhost:8317)"; then
		note=1
	fi
	if ! printf '%s\n' "$out" | grep -qF "ANTHROPIC_BASE_URL=http://"; then
		not_ok "--node $node: no ANTHROPIC_BASE_URL hint printed"
	elif [ "$want" = yes ] && [ "$note" = 1 ]; then
		ok "--node $node: hint carries the loopback-address note"
	elif [ "$want" = no ] && [ "$note" = 0 ]; then
		ok "--node $node: hint printed without a loopback-address note"
	else
		not_ok "--node $node: loopback-address note present=$note, wanted $want"
		printf '%s\n' "$out" | sed 's/^/#   /'
	fi
}

expect_hint utraque.example.test yes
for node in 127.0.0.1 localhost LocalHost ::1 192.168.1.5; do
	expect_hint "$node" no
done

# The characters --node permits are a fixed set; anything else is refused
# before classification, with a different message and no plist.
for node in " 127.0.0.1 " $'127.0.0.1\n'; do
	tests_run=$((tests_run + 1))
	home=$(mktemp -d "$tmp_root/home.XXXXXX")
	if out=$(run_install "$home" --node "$node" 2>&1); then
		not_ok "$(printf '--node %q was accepted' "$node")"
	elif [ -e "$home/Library/LaunchAgents/com.hughescr.utraque.plist" ]; then
		not_ok "$(printf '--node %q was refused but a plist was written' "$node")"
	else
		ok "$(printf -- '--node %q is refused with no plist' "$node")"
	fi
done

# The default node, no token: the plist is still written mode 600.
tests_run=$((tests_run + 1))
home=$(mktemp -d "$tmp_root/home.XXXXXX")
plist="$home/Library/LaunchAgents/com.hughescr.utraque.plist"
if out=$(run_install "$home" 2>&1) && [ -n "$(find "$plist" -perm 600 2>/dev/null)" ]; then
	ok "default install writes the plist mode 600"
else
	not_ok "default install did not write a mode-600 plist"
	printf '%s\n' "$out" | sed 's/^/#   /'
fi

tests_run=$((tests_run + 1))
if [ -e "$launchctl_log" ]; then
	not_ok "launchctl was invoked: $(cat "$launchctl_log")"
else
	ok "launchctl was never invoked"
fi

finish
