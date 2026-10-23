#!/usr/bin/env bash

# Configure this checkout to use its version-controlled hooks without silently
# replacing another hook setup.

set -euo pipefail

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)

die() {
	printf 'install-hooks.sh: %s\n' "$*" >&2
	exit 1
}

command -v git >/dev/null 2>&1 || die "missing required command: git"
git_root=$(git -C "$repo" rev-parse --show-toplevel 2>/dev/null) || die "not inside a Git working tree: $repo"
[ "$git_root" = "$repo" ] || die "script directory does not match Git root: $git_root"
[ -x "$repo/.githooks/pre-push" ] || die "missing executable hook: $repo/.githooks/pre-push"

if configured=$(git -C "$repo" config --get core.hooksPath 2>/dev/null); then
	case "$configured" in
		.githooks|"$repo/.githooks")
			printf '%s\n' 'Git hooks already use .githooks; no change needed.'
			exit 0
			;;
		*)
			origin=$(git -C "$repo" config --show-origin --get core.hooksPath 2>/dev/null || printf '%s' "$configured")
			die "core.hooksPath is already configured ($origin); left it unchanged"
			;;
	esac
fi

hooks_dir=$(git -C "$repo" rev-parse --git-path hooks)
case "$hooks_dir" in
	/*) ;;
	*) hooks_dir="$repo/$hooks_dir" ;;
esac
conflicts=""
for hook in "$hooks_dir"/*; do
	[ -f "$hook" ] || continue
	[ -x "$hook" ] || continue
	case "$hook" in
		*.sample) continue ;;
	esac
	if [ -n "$conflicts" ]; then conflicts="$conflicts, "; fi
	conflicts="${conflicts}$(basename -- "$hook")"
done
[ -z "$conflicts" ] || die "executable hooks already exist in $hooks_dir ($conflicts); left them and core.hooksPath unchanged"

git -C "$repo" config --local core.hooksPath .githooks
printf '%s\n' 'Configured this checkout to use .githooks.'
