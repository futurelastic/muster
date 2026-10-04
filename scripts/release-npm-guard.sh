#!/usr/bin/env bash
# The two reads and the one write in release-npm.yml that talk to a network
# service, with the retry policy they share (#261). Kept as a script rather than
# inline workflow shell so a test can drive it with a fake `gh` and `npm`
# (release_workflow_test.go) — a retry loop nobody can run is a retry loop
# nobody has checked.
#
#   release-npm-guard.sh visibility
#       REPO must name the repository. Exit 0 only on an answer of "public".
#   release-npm-guard.sh publish <package> <version> <dist-tag> <dir>
#       Exit 0 when <package>@<version> is on the registry afterwards, whether
#       this run put it there or an earlier one did.
#
# THREE OUTCOMES, NAMED IN THE LOG (every line starts with its class)
#
#   transient         a 5xx, a 429, a dropped connection or a timeout. Retried,
#                     bounded, with a growing pause. If the retries then succeed
#                     the log says "transient, retried N"; if they run out the
#                     outcome is "persistent error" and the job fails.
#   private           the API ANSWERED, and the answer was "private". Never
#                     retried — a real answer is not a blip — and always fatal.
#   persistent error  an answer that retrying cannot improve (401/403/404, a
#                     body that is neither true nor false) or a transient one
#                     that outlasted its budget. Fatal; fails closed.
#
# Fail-closed is unchanged from before the retries: nothing is ever treated as
# "public" except an explicit `false` read from the API.
#
# RETRY_MAX (default 5) attempts per operation; RETRY_UNIT (default 3) seconds,
# multiplied by the attempt number, between them. A test sets RETRY_UNIT=0.
set -uo pipefail

RETRY_MAX="${RETRY_MAX:-5}"
RETRY_UNIT="${RETRY_UNIT:-3}"

pause() { # pause <attempt> — nothing after the last attempt
	[ "$1" -lt "$RETRY_MAX" ] && sleep $(($1 * RETRY_UNIT))
	return 0
}

# The HTTP status `gh` prints on a failed call ("gh: ... (HTTP 503)"), or empty.
http_status() { printf '%s\n' "$1" | grep -oE 'HTTP [0-9]{3}' | head -1 | cut -d' ' -f2; }

visibility() {
	local attempt out rc status
	: "${REPO:?REPO must name the repository}"
	for attempt in $(seq 1 "$RETRY_MAX"); do
		out="$(gh api "repos/$REPO" --jq '.private' 2>&1)"
		rc=$?
		if [ "$rc" -eq 0 ]; then
			case "$out" in
			false)
				[ "$attempt" -gt 1 ] && echo "transient, retried $((attempt - 1)): the visibility read of $REPO answered after $((attempt - 1)) failed attempt(s)"
				echo "$REPO is public"
				return 0
				;;
			true)
				echo "::error::private: $REPO is private — a private repository never publishes to public npm."
				return 1
				;;
			*)
				echo "::error::persistent error: the visibility read of $REPO answered '$out', neither true nor false — refused like private."
				return 1
				;;
			esac
		fi
		status="$(http_status "$out")"
		case "$status" in
		429 | 5[0-9][0-9] | "") ;; # transient: retry
		*)
			echo "::error::persistent error: the visibility read of $REPO failed with HTTP $status (retrying cannot help) — refused like private."
			return 1
			;;
		esac
		echo "transient: the visibility read of $REPO failed (attempt $attempt of $RETRY_MAX, ${status:+HTTP }${status:-no HTTP status})"
		pause "$attempt"
	done
	echo "::error::persistent error: the visibility read of $REPO kept failing for $RETRY_MAX attempts — refused like private."
	return 1
}

# Network-shaped npm failures worth another try.
npm_transient() {
	printf '%s\n' "$1" | grep -qE 'E(5[0-9][0-9]|429|TIMEDOUT|CONNRESET|CONNREFUSED|AI_AGAIN|NOTFOUND|SOCKETTIMEDOUT)|socket hang up|network (error|request)'
}

# on_registry <spec>: 0 it is there · 1 it is not (E404) · 2 could not tell.
on_registry() {
	local attempt out rc
	for attempt in $(seq 1 "$RETRY_MAX"); do
		out="$(npm view "$1" version 2>&1)"
		rc=$?
		[ "$rc" -eq 0 ] && return 0
		printf '%s\n' "$out" | grep -q 'E404' && return 1
		echo "transient: npm view $1 failed (attempt $attempt of $RETRY_MAX)" >&2
		pause "$attempt"
	done
	return 2
}

publish() {
	local name="$1" version="$2" dist="$3" dir="$4" spec attempt out rc
	spec="$name@$version"
	for attempt in $(seq 1 "$RETRY_MAX"); do
		on_registry "$spec"
		case $? in
		0)
			[ "$attempt" -gt 1 ] && echo "transient, retried $((attempt - 1)): $spec reached the registry"
			echo "$spec is already on npm — nothing to do"
			return 0
			;;
		2)
			echo "::error::persistent error: could not read whether $spec is on npm after $RETRY_MAX attempts."
			return 1
			;;
		esac
		out="$(cd "$dir" && npm publish --tag "$dist" --provenance --access public --ignore-scripts 2>&1)"
		rc=$?
		printf '%s\n' "$out"
		if [ "$rc" -eq 0 ]; then
			[ "$attempt" -gt 1 ] && echo "transient, retried $((attempt - 1)): published $spec"
			echo "Published $spec to npm (dist-tag $dist)"
			return 0
		fi
		# A publish that lost a race with an earlier attempt or run: the version is
		# there, which is the goal. (npm reports EPUBLISHCONFLICT / E403 for it.)
		if printf '%s\n' "$out" | grep -qE 'EPUBLISHCONFLICT|previously published|cannot publish over'; then
			echo "$spec is already on npm — nothing to do"
			return 0
		fi
		if ! npm_transient "$out"; then
			echo "::error::persistent error: npm publish of $spec failed and retrying cannot help."
			return 1
		fi
		echo "transient: npm publish of $spec failed (attempt $attempt of $RETRY_MAX); checking the registry before trying again"
		pause "$attempt"
	done
	echo "::error::persistent error: npm publish of $spec kept failing for $RETRY_MAX attempts."
	return 1
}

case "${1:-}" in
visibility) visibility ;;
publish)
	[ "$#" -eq 5 ] || { echo "usage: $0 publish <package> <version> <dist-tag> <dir>" >&2; exit 2; }
	publish "$2" "$3" "$4" "$5"
	;;
*)
	echo "usage: $0 visibility | publish <package> <version> <dist-tag> <dir>" >&2
	exit 2
	;;
esac
