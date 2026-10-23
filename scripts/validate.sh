#!/usr/bin/env bash

# The single source of truth for checks run locally, by CI, and by pre-push.

set -euo pipefail

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
group=${1:-all}

die() {
	printf 'validate.sh: %s\n' "$*" >&2
	exit 1
}

case "$group" in
	all|go|shell|hooks) ;;
	*) die "unknown group '$group' (expected: all, go, shell, or hooks)" ;;
esac
[ "$#" -le 1 ] || die "expected at most one group argument"

require_commands() {
	missing=""
	for command_name in "$@"; do
		if ! command -v "$command_name" >/dev/null 2>&1; then
			if [ -n "$missing" ]; then missing="$missing, "; fi
			missing="${missing}${command_name}"
		fi
	done
	[ -z "$missing" ] || die "missing required command(s): $missing"
}

preflight_go() {
	require_commands find go gofmt mktemp rm
}

preflight_shell() {
	require_commands bash find mktemp rm shellcheck
}

preflight_hooks() {
	preflight_go
	preflight_shell
	require_commands git
	regression="$repo/tests/pre-push-regression.sh"
	[ -x "$regression" ] || die "required hook regression is missing or not executable: $regression"
}

validate_go() {
	validation_tmp=$(mktemp -d "${TMPDIR:-/tmp}/utraque-validate.XXXXXX")
	trap 'rm -rf "$validation_tmp"' EXIT HUP INT TERM
	go_manifest="$validation_tmp/go-files"
	if ! find "$repo" \
		\( -type d \( -name .git -o -name vendor \) -prune \) -o \
		\( -type f -name '*.go' -print0 \) >"$go_manifest"; then
		die "could not discover Go source files"
	fi

	go_files=()
	while IFS= read -r -d '' file; do
		go_files+=("$file")
	done <"$go_manifest"
	[ "${#go_files[@]}" -gt 0 ] || die "no Go source files found"

	printf '%s\n' '==> gofmt'
	unformatted=$(gofmt -l "${go_files[@]}")
	if [ -n "$unformatted" ]; then
		printf '%s\n' 'not gofmt-clean:' "$unformatted" >&2
		return 1
	fi

	build_output="$validation_tmp/utraque"
	printf '%s\n' '==> go vet ./...'
	(cd "$repo" && CGO_ENABLED=1 go vet ./...)
	printf '%s\n' '==> go build ./cmd/utraque'
	(cd "$repo" && CGO_ENABLED=1 go build -o "$build_output" ./cmd/utraque)
	printf '%s\n' '==> go test ./...'
	(cd "$repo" && CGO_ENABLED=1 go test ./...)
	printf '%s\n' '==> go test -race ./...'
	(cd "$repo" && CGO_ENABLED=1 go test -race ./...)
	rm -rf "$validation_tmp"
	trap - EXIT HUP INT TERM
}

validate_shell() {
	validation_tmp=$(mktemp -d "${TMPDIR:-/tmp}/utraque-shell.XXXXXX")
	trap 'rm -rf "$validation_tmp"' EXIT HUP INT TERM
	shell_manifest="$validation_tmp/shell-files"
	: >"$shell_manifest"
	shell_files=()
	for directory in deploy scripts tests; do
		if [ -d "$repo/$directory" ]; then
			if ! find "$repo/$directory" -type f -name '*.sh' -print0 >>"$shell_manifest"; then
				die "could not discover shell files under $directory"
			fi
		fi
	done
	if [ -d "$repo/.githooks" ]; then
		if ! find "$repo/.githooks" -type f -print0 >>"$shell_manifest"; then
			die "could not discover version-controlled hooks"
		fi
	fi
	while IFS= read -r -d '' file; do
		shell_files+=("$file")
	done <"$shell_manifest"
	if [ "${#shell_files[@]}" -eq 0 ]; then
		die "no shell or hook files found"
	fi

	printf '%s\n' '==> bash -n'
	for file in "${shell_files[@]}"; do
		bash -n "$file"
	done
	printf '%s\n' '==> shellcheck'
	shellcheck "${shell_files[@]}"
	rm -rf "$validation_tmp"
	trap - EXIT HUP INT TERM
}

case "$group" in
	go)
		preflight_go
		validate_go
		;;
	shell)
		preflight_shell
		validate_shell
		;;
	hooks)
		preflight_hooks
		printf '%s\n' '==> pre-push regression'
		"$regression"
		;;
	all)
		preflight_hooks
		validate_go
		validate_shell
		printf '%s\n' '==> pre-push regression'
		"$regression"
		;;
esac
