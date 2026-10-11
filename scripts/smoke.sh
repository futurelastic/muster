#!/bin/sh
# The quick check a session runs while it works: `gate.smoke` in
# .github/project.yml points here (#299).
#
# This is NOT the gate. scripts/gate.sh is the full suite (gofmt, build, vet,
# `go test -race ./...`) and CI's `go` job calls it; it stays the one place the
# verdict comes from. A session's own machine runs only this script, and the
# full suite runs on a runner against the pushed branch (`authoritative: ci`).
#
# What it runs:
#   1. gofmt -l .          the whole tree: cheap, and the step that once let
#                          a dirty file reach trunk (#233)
#   2. go build ./...      the whole module: catches a broken caller in a
#                          package nobody touched
#   3. go vet ./...        same reason
#   4. go test <packages>  ONLY the packages with a changed .go file, plus
#                          everything if go.mod / go.sum changed. No -race:
#                          that is the full gate's job, on a runner.
#
# "Changed" means: committed on this branch since it left trunk, plus anything
# modified or untracked in the working tree. Trunk is origin/<default branch>,
# resolved from origin/HEAD, falling back to origin/main.
#
# A package that depends on a changed one is not re-tested here. That is the
# trade for a few seconds instead of the whole suite; the runner catches it.
set -eu

cd "$(dirname "$0")/.."

echo "== gofmt =="
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
	echo "These files are not gofmt-clean:"
	echo "$unformatted"
	exit 1
fi

echo "== go build =="
go build ./...

echo "== go vet =="
go vet ./...

trunk="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)"
[ -n "$trunk" ] || trunk="origin/main"

if base="$(git merge-base "$trunk" HEAD 2>/dev/null)"; then
	changed="$(
		{
			git diff --name-only "$base" HEAD
			git diff --name-only HEAD
			git ls-files --others --exclude-standard
		} | sort -u
	)"
else
	echo "note: cannot resolve $trunk; testing every package"
	changed="go.mod"
fi

# A dependency change can move any package.
if printf '%s\n' "$changed" | grep -qxE 'go\.(mod|sum)'; then
	echo "== go test (go.mod/go.sum changed: all packages) =="
	go test ./...
	echo "PASS: smoke (all packages)."
	exit 0
fi

# Directories of changed .go files that still exist, as ./relative packages.
pkgs="$(
	printf '%s\n' "$changed" |
		grep -E '\.go$' |
		while IFS= read -r f; do
			d="$(dirname "$f")"
			[ -d "$d" ] && printf './%s\n' "$d"
		done |
		sed 's|^\./\.$|.|' |
		sort -u
)"

if [ -z "$pkgs" ]; then
	echo "== go test: no changed Go package, nothing to run =="
	echo "PASS: smoke (gofmt, build, vet)."
	exit 0
fi

echo "== go test (changed packages) =="
printf '%s\n' "$pkgs"
# shellcheck disable=SC2086
go test $(printf '%s\n' "$pkgs" | tr '\n' ' ')

echo "PASS: smoke (gofmt, build, vet, tests for changed packages)."
