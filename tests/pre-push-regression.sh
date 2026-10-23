#!/usr/bin/env bash

# End-to-end regression coverage for the repository validation entrypoint and
# version-controlled pre-push hook. Every repository and remote created here is
# disposable and local; the test never contacts a network remote.

set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
git_local_env=$(git -C "$root" rev-parse --local-env-vars)
for variable in $git_local_env; do
	unset "$variable"
done
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null

tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/utraque-pre-push.XXXXXX") || exit 1
trap 'rm -rf "$tmp_root"' EXIT HUP INT TERM
mkdir -p "$tmp_root/git-template"
export GIT_TEMPLATE_DIR="$tmp_root/git-template"
export GOCACHE="$tmp_root/go-cache"
export GOMODCACHE="$tmp_root/go-mod-cache"
mkdir -p "$GOCACHE" "$GOMODCACHE"

tests_run=0

fail() {
	printf 'not ok %d - %s\n' "$tests_run" "$*" >&2
	exit 1
}

pass() {
	printf 'ok %d - %s\n' "$tests_run" "$1"
}

begin_test() {
	tests_run=$((tests_run + 1))
	printf '# %s\n' "$1"
}

write_valid_go() {
	file=$1
	cat >"$file" <<'EOF'
package main

func main() {}
EOF
}

write_valid_shell() {
	file=$1
	cat >"$file" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'valid deployment fixture\n'
EOF
	chmod +x "$file"
}

write_invalid_shell() {
	file=$1
	cat >"$file" <<'EOF'
#!/usr/bin/env bash
if true; then
	printf 'missing fi\n'
EOF
	chmod +x "$file"
}

copy_validation_artifacts() {
	destination=$1
	mkdir -p "$destination/scripts" "$destination/.githooks"
	if [ -x "$root/scripts/validate.sh" ]; then
		cp -p "$root/scripts/validate.sh" "$destination/scripts/validate-source.sh"
		cat >"$destination/scripts/validate.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [ "$#" -gt 0 ]; then
	exec "$here/validate-source.sh" "$@"
fi
"$here/validate-source.sh" go
exec "$here/validate-source.sh" shell
EOF
		chmod +x "$destination/scripts/validate.sh"
	fi
	if [ -x "$root/.githooks/pre-push" ]; then
		cp -p "$root/.githooks/pre-push" "$destination/.githooks/pre-push"
	fi
}

init_fixture() {
	working=$1
	remote=$2
	git init --bare -b main "$remote" >/dev/null || fail "could not initialize bare remote"
	git init -b main "$working" >/dev/null || fail "could not initialize working repository"
	git -C "$working" config user.name "Utraque Hook Test"
	git -C "$working" config user.email "hook-test@example.invalid"
	git -C "$working" config core.hooksPath .githooks
	git -C "$working" remote add origin "$remote"

	mkdir -p "$working/cmd/utraque" "$working/deploy/nested"
	cat >"$working/go.mod" <<'EOF'
module example.invalid/hookfixture

go 1.27
EOF
	write_valid_go "$working/main.go"
	write_valid_go "$working/cmd/utraque/main.go"
	write_valid_shell "$working/deploy/install.sh"
	write_valid_shell "$working/deploy/nested/check.sh"
	copy_validation_artifacts "$working"
	git -C "$working" add .
	git -C "$working" commit -m "valid initial fixture" >/dev/null || fail "could not commit initial fixture"
}

remote_oid() {
	remote=$1
	ref=$2
	git --git-dir="$remote" rev-parse --verify "$ref" 2>/dev/null || printf '%s\n' absent
}

capture_push() {
	output_file=$1
	shift
	if git "$@" >"$output_file" 2>&1; then
		push_rc=0
	else
		push_rc=$?
	fi
}

assert_blocked_without_remote_change() {
	description=$1
	before=$2
	after=$3
	rc=$4
	details=""
	if [ "$rc" -eq 0 ]; then
		details="push exited 0"
	fi
	if [ "$after" != "$before" ]; then
		if [ -n "$details" ]; then details="$details; "; fi
		details="${details}remote changed from $before to $after"
	fi
	if [ -n "$details" ]; then
		fail "$description was not blocked: $details"
	fi
}

test_real_pre_push_enforcement() {
	begin_test "real pre-push enforcement protects the remote"
	working="$tmp_root/enforcement-work"
	remote="$tmp_root/enforcement.git"
	push_log="$tmp_root/push.log"
	init_fixture "$working" "$remote"

	# A valid control push establishes that the local transport and fixture are
	# healthy before any rejection is attributed to the hook.
	capture_push "$push_log" -C "$working" push -u origin main
	[ "$push_rc" -eq 0 ] || fail "valid control push failed: $(cat "$push_log")"
	baseline=$(remote_oid "$remote" refs/heads/main)
	[ "$baseline" = "$(git -C "$working" rev-parse HEAD)" ] || fail "valid control did not update remote main"

	# This is syntactically valid Go but genuinely fails gofmt. A missing hook is
	# silent in Git, so the regression's initial red state is the observable bad
	# outcome: the push succeeds and advances the remote.
	cat >"$working/main.go" <<'EOF'
package main
func main(){println("unformatted")}
EOF
	git -C "$working" add main.go
	git -C "$working" commit -m "unformatted Go" >/dev/null || fail "could not commit unformatted Go"
	capture_push "$push_log" -C "$working" push origin main
	after=$(remote_oid "$remote" refs/heads/main)
	assert_blocked_without_remote_change "unformatted Go push" "$baseline" "$after" "$push_rc"

	write_valid_go "$working/main.go"
	git -C "$working" add main.go
	git -C "$working" commit -m "format Go" >/dev/null || fail "could not commit formatted Go"
	capture_push "$push_log" -C "$working" push origin main
	[ "$push_rc" -eq 0 ] || fail "valid formatted control push failed: $(cat "$push_log")"
	baseline=$(remote_oid "$remote" refs/heads/main)

	# Keep a valid first deployment script and put the syntax error only in a
	# nested second script. This catches both non-recursive discovery and the
	# incorrect `bash -n first.sh second.sh` form, which parses only first.sh.
	write_invalid_shell "$working/deploy/nested/check.sh"
	git -C "$working" add deploy/nested/check.sh
	git -C "$working" commit -m "malformed nested deployment shell" >/dev/null || fail "could not commit malformed shell"
	capture_push "$push_log" -C "$working" push origin main
	after=$(remote_oid "$remote" refs/heads/main)
	assert_blocked_without_remote_change "nested malformed shell push" "$baseline" "$after" "$push_rc"

	write_valid_shell "$working/deploy/nested/check.sh"
	git -C "$working" add deploy/nested/check.sh
	git -C "$working" commit -m "fix nested deployment shell" >/dev/null || fail "could not commit shell fix"
	capture_push "$push_log" -C "$working" push origin main
	[ "$push_rc" -eq 0 ] || fail "valid shell control push failed: $(cat "$push_log")"
	baseline=$(remote_oid "$remote" refs/heads/main)

	# The hook validates the exact commit being pushed. It must not claim that a
	# valid HEAD was checked while tracked or untracked working-tree state could
	# have influenced the validator.
	cat >"$working/feature.go" <<'EOF'
package main

const feature = true
EOF
	git -C "$working" add feature.go
	git -C "$working" commit -m "valid tracked dirty-state candidate" >/dev/null || fail "could not commit dirty-state candidate"
	printf '\n// local-only dirty state\n' >>"$working/main.go"
	capture_push "$push_log" -C "$working" push origin main
	after=$(remote_oid "$remote" refs/heads/main)
	assert_blocked_without_remote_change "tracked dirty-tree push" "$baseline" "$after" "$push_rc"
	grep -Eqi 'dirty|tracked|working tree' "$push_log" || fail "tracked dirty-tree rejection did not explain the dirty checkout"
	write_valid_go "$working/main.go"
	capture_push "$push_log" -C "$working" push origin main
	[ "$push_rc" -eq 0 ] || fail "clean push after tracked dirty rejection failed: $(cat "$push_log")"
	baseline=$(remote_oid "$remote" refs/heads/main)

	cat >"$working/more.go" <<'EOF'
package main

const more = true
EOF
	git -C "$working" add more.go
	git -C "$working" commit -m "valid untracked-state candidate" >/dev/null || fail "could not commit untracked-state candidate"
	printf 'untracked state\n' >"$working/untracked.txt"
	capture_push "$push_log" -C "$working" push origin main
	after=$(remote_oid "$remote" refs/heads/main)
	assert_blocked_without_remote_change "untracked-tree push" "$baseline" "$after" "$push_rc"
	grep -Eqi 'untracked|dirty|working tree' "$push_log" || fail "untracked-tree rejection did not explain the untracked file"
	rm -f "$working/untracked.txt"
	capture_push "$push_log" -C "$working" push origin main
	[ "$push_rc" -eq 0 ] || fail "clean push after untracked rejection failed: $(cat "$push_log")"
	current_head=$(git -C "$working" rev-parse HEAD)

	# An annotated tag's update OID is the tag object, so the hook has to peel it
	# before comparing it with HEAD. A tag of HEAD is valid; a tag of an older
	# commit still represents a non-HEAD code update and must be rejected.
	git -C "$working" tag -a current-head -m "current HEAD" HEAD
	capture_push "$push_log" -C "$working" push origin refs/tags/current-head
	[ "$push_rc" -eq 0 ] || fail "annotated tag of HEAD was rejected: $(cat "$push_log")"
	[ "$(git --git-dir="$remote" rev-parse 'refs/tags/current-head^{}')" = "$current_head" ] || fail "annotated HEAD tag did not reach the remote"
	git -C "$working" tag -a stale-tag -m "stale tag" "$baseline"
	capture_push "$push_log" -C "$working" push origin refs/tags/stale-tag
	stale_tag_after=$(remote_oid "$remote" refs/tags/stale-tag)
	assert_blocked_without_remote_change "annotated non-HEAD tag push" absent "$stale_tag_after" "$push_rc"

	# A named local ref can point somewhere other than HEAD. Rejecting it avoids
	# validating one tree and pushing another.
	git -C "$working" branch stale "$baseline"
	capture_push "$push_log" -C "$working" push origin stale:refs/heads/stale
	stale_after=$(remote_oid "$remote" refs/heads/stale)
	assert_blocked_without_remote_change "non-HEAD code push" absent "$stale_after" "$push_rc"
	grep -q 'HEAD' "$push_log" || fail "non-HEAD rejection did not explain the HEAD-only rule"
	capture_push "$push_log" -C "$working" push origin HEAD:refs/heads/atomic-valid stale:refs/heads/atomic-stale
	atomic_valid_after=$(remote_oid "$remote" refs/heads/atomic-valid)
	atomic_stale_after=$(remote_oid "$remote" refs/heads/atomic-stale)
	assert_blocked_without_remote_change "mixed HEAD/non-HEAD multi-ref push" absent "$atomic_valid_after" "$push_rc"
	[ "$atomic_stale_after" = absent ] || fail "mixed multi-ref rejection still created the stale remote ref"

	# A deletion has no local code tree to validate and must remain usable even
	# while the checkout is dirty.
	git -C "$working" branch deletable HEAD
	capture_push "$push_log" -C "$working" push origin deletable:refs/heads/deletable
	[ "$push_rc" -eq 0 ] || fail "setup push for deletion failed: $(cat "$push_log")"
	printf 'dirty during deletion\n' >"$working/untracked.txt"
	capture_push "$push_log" -C "$working" push origin --delete deletable
	[ "$push_rc" -eq 0 ] || fail "deletion-only push was rejected: $(cat "$push_log")"
	[ "$(remote_oid "$remote" refs/heads/deletable)" = absent ] || fail "deletion-only push left the remote ref in place"
	rm -f "$working/untracked.txt"

	pass "real pre-push enforcement protects the remote"
}

test_validator_contract() {
	begin_test "validator groups are recursive, isolated, and complete by default"
	working="$tmp_root/validator-work"
	remote="$tmp_root/validator.git"
	log="$tmp_root/validator.log"
	init_fixture "$working" "$remote"
	validator="$working/scripts/validate.sh"
	[ -x "$validator" ] || fail "fixture has no executable scripts/validate.sh"
	mkdir -p "$working/bin"
	printf 'deployed binary sentinel\n' >"$working/bin/utraque"

	(cd "$working/deploy" && "$validator" shell) >"$log" 2>&1 || fail "valid shell group failed outside the repository root: $(cat "$log")"
	real_go=$(command -v go) || fail "test host lacks go"
	go_log="$tmp_root/go-invocations.log"
	go_shim="$tmp_root/go-shim"
	mkdir -p "$go_shim"
	cat >"$go_shim/go" <<'EOF'
#!/bin/sh
printf '%s|%s\n' "${CGO_ENABLED-unset}" "$*" >>"$GO_INVOCATIONS"
exec "$REAL_GO" "$@"
EOF
	chmod +x "$go_shim/go"
	(cd "$working" && GO_INVOCATIONS="$go_log" REAL_GO="$real_go" PATH="$go_shim:$PATH" "$validator" go) >"$log" 2>&1 || fail "valid Go group failed: $(cat "$log")"
	grep -q '^1|vet ./\.\.\.$' "$go_log" || fail "Go validation did not force CGO_ENABLED=1 for go vet ./..."
	grep -Eq '^1\|build -o .+ ./cmd/utraque$' "$go_log" || fail "Go validation did not build ./cmd/utraque with CGO_ENABLED=1 into a separate output"
	grep -q '^1|test ./\.\.\.$' "$go_log" || fail "Go validation did not force CGO_ENABLED=1 for go test ./..."
	grep -q '^1|test -race ./\.\.\.$' "$go_log" || fail "Go validation did not force CGO_ENABLED=1 for go test -race ./..."
	[ "$(cat "$working/bin/utraque")" = "deployed binary sentinel" ] || fail "Go validation overwrote bin/utraque"
	(cd "$working" && "$validator") >"$log" 2>&1 || fail "valid default validation failed: $(cat "$log")"

	write_invalid_shell "$working/deploy/nested/check.sh"
	set +e
	(cd "$working" && "$validator" shell) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "shell group accepted a malformed recursive deploy script"
	(cd "$working" && "$validator" go) >"$log" 2>&1 || fail "Go group was coupled to shell validation: $(cat "$log")"
	set +e
	(cd "$working" && "$validator") >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "default validation omitted shell checks"
	write_valid_shell "$working/deploy/nested/check.sh"

	cat >"$working/main.go" <<'EOF'
package main
func main(){println("unformatted")}
EOF
	(cd "$working" && "$validator" shell) >"$log" 2>&1 || fail "shell group was coupled to Go validation: $(cat "$log")"
	set +e
	(cd "$working" && "$validator" go) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "Go group accepted unformatted Go"
	set +e
	(cd "$working" && "$validator") >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "default validation omitted Go checks"

	pass "validator groups are recursive, isolated, and complete by default"
}

make_path_shim() {
	shim=$1
	mkdir -p "$shim"
	shift
	for command_name in "$@"; do
		command_path=$(command -v "$command_name") || fail "test host lacks $command_name"
		ln -s "$command_path" "$shim/$command_name"
	done
}

test_validator_errors_are_early() {
	begin_test "validator rejects bad arguments and missing dependencies early"
	working="$tmp_root/errors-work"
	remote="$tmp_root/errors.git"
	log="$tmp_root/errors.log"
	marker="$tmp_root/validator-command-ran"
	init_fixture "$working" "$remote"
	validator="$working/scripts/validate.sh"
	[ -x "$validator" ] || fail "fixture has no executable scripts/validate.sh"

	marker_bin="$tmp_root/marker-bin"
	mkdir -p "$marker_bin"
	cat >"$marker_bin/go" <<'EOF'
#!/bin/sh
: >"$VALIDATOR_MARKER"
exit 99
EOF
	cp "$marker_bin/go" "$marker_bin/gofmt"
	cp "$marker_bin/go" "$marker_bin/shellcheck"
	chmod +x "$marker_bin/go" "$marker_bin/gofmt" "$marker_bin/shellcheck"
	set +e
	(cd "$working" && VALIDATOR_MARKER="$marker" PATH="$marker_bin:$PATH" "$validator" unknown) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "unknown validator group succeeded"
	[ ! -e "$marker" ] || fail "unknown validator group ran validation commands before rejecting the argument"
	set +e
	(cd "$working" && VALIDATOR_MARKER="$marker" PATH="$marker_bin:$PATH" "$validator" go extra) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "validator accepted an extra argument"
	[ ! -e "$marker" ] || fail "validator ran validation commands before rejecting an extra argument"

	shim="$tmp_root/no-shellcheck-bin"
	make_path_shim "$shim" dirname find mktemp rm
	cat >"$shim/bash" <<'EOF'
#!/bin/sh
if [ "${1:-}" = -n ]; then
	: >"$VALIDATOR_MARKER"
fi
exec /bin/bash "$@"
EOF
	chmod +x "$shim/bash"
	set +e
	(cd "$working" && VALIDATOR_MARKER="$marker" PATH="$shim" /bin/bash "$validator" shell) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "shell group succeeded without shellcheck"
	[ ! -e "$marker" ] || fail "shell group began syntax checks before reporting missing shellcheck"
	grep -q 'shellcheck' "$log" || fail "missing-shellcheck error did not name shellcheck"

	pass "validator rejects bad arguments and missing dependencies early"
}

init_installer_fixture() {
	working=$1
	git init -b main "$working" >/dev/null || fail "could not initialize installer repository"
	mkdir -p "$working/scripts" "$working/.githooks" "$working/.git/hooks"
	[ -x "$root/scripts/install-hooks.sh" ] || fail "repository has no executable scripts/install-hooks.sh"
	[ -x "$root/.githooks/pre-push" ] || fail "repository has no executable .githooks/pre-push"
	cp -p "$root/scripts/install-hooks.sh" "$working/scripts/install-hooks.sh"
	cp -p "$root/.githooks/pre-push" "$working/.githooks/pre-push"
	cat >"$working/.git/hooks/pre-push.sample" <<'EOF'
#!/bin/sh
exit 0
EOF
	chmod +x "$working/.git/hooks/pre-push.sample"
}

test_installer_conflicts() {
	begin_test "hook installer is idempotent and preserves existing hook choices"
	log="$tmp_root/installer.log"

	plain="$tmp_root/install-plain"
	init_installer_fixture "$plain"
	(cd "$plain" && scripts/install-hooks.sh) >"$log" 2>&1 || fail "installer rejected sample-only default hooks: $(cat "$log")"
	[ "$(git -C "$plain" config --local --get core.hooksPath)" = .githooks ] || fail "installer did not configure .githooks"
	(cd "$plain" && scripts/install-hooks.sh) >"$log" 2>&1 || fail "installer was not idempotent: $(cat "$log")"

	default_conflict="$tmp_root/install-default-conflict"
	init_installer_fixture "$default_conflict"
	cat >"$default_conflict/.git/hooks/pre-commit" <<'EOF'
#!/bin/sh
printf 'existing hook\n'
EOF
	chmod +x "$default_conflict/.git/hooks/pre-commit"
	set +e
	(cd "$default_conflict" && scripts/install-hooks.sh) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "installer bypassed an existing executable default hook"
	grep -q 'existing hook' "$default_conflict/.git/hooks/pre-commit" || fail "installer changed the existing default hook"
	[ -z "$(git -C "$default_conflict" config --local --get core.hooksPath 2>/dev/null)" ] || fail "installer configured hooksPath despite a default-hook conflict"

	path_conflict="$tmp_root/install-path-conflict"
	init_installer_fixture "$path_conflict"
	git -C "$path_conflict" config core.hooksPath custom-hooks
	set +e
	(cd "$path_conflict" && scripts/install-hooks.sh) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "installer replaced a different configured core.hooksPath"
	[ "$(git -C "$path_conflict" config --local --get core.hooksPath)" = custom-hooks ] || fail "installer changed the conflicting core.hooksPath"

	empty_conflict="$tmp_root/install-empty-conflict"
	init_installer_fixture "$empty_conflict"
	git -C "$empty_conflict" config core.hooksPath ''
	set +e
	(cd "$empty_conflict" && scripts/install-hooks.sh) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "installer replaced an explicitly empty core.hooksPath"
	git -C "$empty_conflict" config --local --get core.hooksPath >/dev/null || fail "installer removed the explicitly empty core.hooksPath"
	[ -z "$(git -C "$empty_conflict" config --local --get core.hooksPath)" ] || fail "installer changed the explicitly empty core.hooksPath"

	global_conflict="$tmp_root/install-global-conflict"
	global_config="$tmp_root/conflicting-global.gitconfig"
	init_installer_fixture "$global_conflict"
	GIT_CONFIG_GLOBAL="$global_config" git config --global core.hooksPath global-hooks
	set +e
	(cd "$global_conflict" && GIT_CONFIG_GLOBAL="$global_config" scripts/install-hooks.sh) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "installer replaced a global core.hooksPath"
	[ -z "$(git -C "$global_conflict" config --local --get core.hooksPath 2>/dev/null)" ] || fail "installer added local hooksPath over a global conflict"
	[ "$(GIT_CONFIG_GLOBAL="$global_config" git config --global --get core.hooksPath)" = global-hooks ] || fail "installer changed the global core.hooksPath"

	read_error="$tmp_root/install-read-error"
	git_shim="$tmp_root/git-shim"
	real_git=$(command -v git) || fail "test host lacks git"
	init_installer_fixture "$read_error"
	mkdir -p "$git_shim"
	cat >"$git_shim/git" <<'EOF'
#!/bin/sh
case "$*" in
	*' config --get core.hooksPath')
		printf '%s\n' 'simulated core.hooksPath read failure' >&2
		exit 2
		;;
esac
exec "$REAL_GIT" "$@"
EOF
	chmod +x "$git_shim/git"
	set +e
	(cd "$read_error" && REAL_GIT="$real_git" PATH="$git_shim:$PATH" scripts/install-hooks.sh) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "installer treated a core.hooksPath read failure as an absent setting"
	grep -q 'simulated core.hooksPath read failure' "$log" || fail "installer discarded the Git config diagnostic"
	[ -z "$(git -C "$read_error" config --local --get core.hooksPath 2>/dev/null)" ] || fail "installer mutated hooksPath after a config read failure"

	linked="$tmp_root/install-linked"
	linked_other="$tmp_root/install-linked-other"
	init_installer_fixture "$linked"
	git -C "$linked" config user.name "Utraque Hook Test"
	git -C "$linked" config user.email "hook-test@example.invalid"
	git -C "$linked" add .githooks scripts
	git -C "$linked" commit -m "linked installer fixture" >/dev/null
	git -C "$linked" worktree add -b linked-installer-test "$linked_other" HEAD >/dev/null
	set +e
	(cd "$linked" && scripts/install-hooks.sh) >"$log" 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] || fail "installer configured repository-local hooksPath with a linked worktree"
	grep -q 'linked worktrees' "$log" || fail "linked-worktree refusal did not explain the repository-wide effect"
	[ -z "$(git -C "$linked" config --local --get core.hooksPath 2>/dev/null)" ] || fail "installer mutated hooksPath with linked worktrees present"

	pass "hook installer is idempotent and preserves existing hook choices"
}

printf '1..4\n'
test_real_pre_push_enforcement
test_validator_contract
test_validator_errors_are_early
test_installer_conflicts
