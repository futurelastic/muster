#!/bin/sh
# The single declared quality gate for this repo. CI's `go` job runs this exact
# file — not a hand-copied list of the same steps — so a local pass and a CI
# pass answer the same question by construction.
#
# WHY THIS EXISTS (#233)
#
# Before this script, a wrap session's idea of "the gate" was whatever it
# personally chose to run, and CI's `go` job was a separately maintained list
# of steps. #230 shipped green against `go build`, `go vet` and `go test -race`
# — all real checks, all passing — and still turned trunk red, because CI's
# first step, `gofmt -l .`, was never in that list. The fix (#231) was a
# whitespace-only diff; the gap it closed was that "the gate" was remembered
# twice, in two places, and the two copies had already drifted.
#
# So: one file, and CI calls it. Widening the gate later (a linter, a new
# package to build) means editing this file once; nothing else needs to
# change to stay in sync, because nothing else re-declares these steps.
#
# Order matches CI's `go` job exactly: gofmt first (cheapest, and it is the
# step that actually reached trunk broken), then build, then vet, then the
# Windows cross-compile (#303), then the -race test suite (slowest, so it
# fails last, and the same order every session's own gate run trains muscle
# memory for).
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

# Windows must keep compiling (#303). The port is staged, and a Windows runner
# is not in this job: cross-compiling is. `go vet` type-checks the test files
# too, so a new `syscall.Kill` in a test fails here, in seconds, on any machine,
# rather than on the first Windows run. amd64 vets; arm64 builds, because the
# platform-specific code does not differ by architecture.
echo "== go vet (windows/amd64) =="
GOOS=windows GOARCH=amd64 go vet ./...

echo "== go build (windows/arm64) =="
GOOS=windows GOARCH=arm64 go build ./...

# -race because this codebase runs control-mode clients, pumps and an event
# engine concurrently; a data race there would surface as an event stream
# that is subtly wrong rather than one that crashes.
#
# The driver integration tests are opt-in via FLEET_TMUX_INTEGRATION and skip
# here: this gate (and CI, which calls it) has no multiplexer and no sessions
# to look at. That is deliberate — a green run means the unit suite passed,
# not that the substrate behaved.
echo "== go test -race =="
go test -race ./...

echo "PASS: gofmt clean, build/vet/windows-cross/test -race all green."
