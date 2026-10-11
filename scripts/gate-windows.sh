#!/usr/bin/env bash
# The Windows gate (#303, #306). CI's `windows` job runs this exact file on a
# hosted Windows runner, under the bash that ships with git.
#
# It is NOT gate.sh. gate.sh is the Unix full suite and stays the one place the
# whole-module verdict comes from. This script answers a narrower question:
# does the platform layer actually WORK on Windows — not merely compile, which
# the cross-vet in gate.sh already proves on every machine.
#
# Three properties it enforces that `go test` alone does not:
#
#   1. An allowlist, not `./...`. The module has a terminal driver and a large
#      test corpus that is Unix-only by design (ADR 303). Packages join the
#      list as their tests are made Windows-clean; widening it is a follow-up.
#   2. A printed SKIP summary. A skipped test is green to `go test`, which is
#      how "the process-tree tests pass" can be true of a run that skipped them.
#   3. A must-pass list. Each name below must appear as `--- PASS:` in the log;
#      a skip, or a rename that silently drops the test, fails the gate.
#
# -race needs a cgo toolchain. If the runner has none the gate runs without it,
# and says so on the first line rather than pretending.
set -euo pipefail

cd "$(dirname "$0")/.."

ALLOW=(
	./internal/procgroup
	./internal/delivery/modclient
)

MUST_PASS=(
	TestKillReachesAGrandchildSpawnedBeforeAttachReturned
	TestCloseWithoutKillEndsTheJob
	TestStructSizesMatchTheWin32Layout
	TestProcess_SpawnsServeWithMinimalEnv
	TestProcess_KillMidRunRestartsAndReady
	TestProcess_ShutdownGrace
)

echo "== go build =="
go build ./...

echo "== go vet =="
go vet ./...

RACE="-race"
if ! go test -race -count=1 -run '^$' ./internal/procgroup >/dev/null 2>&1; then
	echo "NOTE: -race is unavailable on this runner (no cgo toolchain); running without it"
	RACE=""
fi

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

echo "== go test ${RACE} (allowlist: ${ALLOW[*]}) =="
status=0
# shellcheck disable=SC2086
go test $RACE -count=1 -timeout 20m -v "${ALLOW[@]}" 2>&1 | tee "$log" || status=$?

echo
echo "== SKIP summary =="
grep -E '^\s*--- SKIP' "$log" || echo "(no skipped tests)"

echo
echo "== package results =="
grep -E '^(ok|FAIL|---)\s' "$log" | grep -E '^(ok|FAIL)\s' || true

if [ "$status" -ne 0 ]; then
	echo "FAIL: go test exited $status"
	exit "$status"
fi

echo
echo "== must-pass =="
missing=0
for name in "${MUST_PASS[@]}"; do
	if grep -qE "^\s*--- PASS: ${name} \(" "$log"; then
		echo "PASS  ${name}"
	else
		echo "MISSING  ${name}  (skipped, renamed or not run)"
		missing=1
	fi
done
[ "$missing" -eq 0 ] || { echo "FAIL: a must-pass test did not pass"; exit 1; }

echo "PASS: windows gate."
