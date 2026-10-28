#!/usr/bin/env bash
# Which final tags release-auto.yml hands to publish-downloads.yml, and the hand-over itself (#383). Kept as a script
# rather than inline workflow shell so a test can drive it with a fake `gh` against a real git origin
# (release_downloads_test.go), the way release-npm-guard.sh is driven.
#
#   release-downloads-guard.sh tags
#       Prints the FINAL tags to hand over, one per line, each once. From the environment:
#         JUST_FINALIZED  the final this run tagged, if any
#         PUBLISH_TAG     the tag a dispatch named (a candidate is not a final and is left out)
#         CATCH_UP=1      also the newest final on origin that main reaches
#
#   release-downloads-guard.sh dispatch <tag>
#       Starts publish-downloads.yml AT THE TAG, unless there is a reason not to (each is a line in the log):
#         - the tag is not a final on origin reachable from main      -> exit 1 (release-npm-guard.sh check-tag)
#         - the tag's tree has no .github/workflows/publish-downloads.yml
#                                                                     -> skipped: a final cut before the workflow existed
#         - an earlier run of the workflow at this tag succeeded      -> skipped: the host already said "published"
#       A dry run does not count as that earlier run (its title says so).
#
# WHY AT THE TAG. The host accepts a publish only from a run whose ref is `refs/tags/vX.Y.Z` and whose workflow is
# that tag's own copy of the bound file. release-auto.yml runs on main, so it cannot publish; it dispatches a run at
# the tag instead (a workflow_dispatch started with GITHUB_TOKEN is the one event that is allowed to start another
# run). The same reason means a final tagged before this workflow merged can never be published: the file is not in
# its tree.
#
# WHAT "ALREADY PUBLISHED" MEANS HERE. The catch-up looks at what the HOST answered, not at npm: a publish run only
# ends green when the host reported the version published (or already published). A tag with no green run is
# dispatched again, and the host's own idempotent "already published" answer closes any gap between the two.
#
# REPO names the repository (owner/name). RETRY_MAX (default 3) attempts per `gh` call, RETRY_UNIT (default 3)
# seconds times the attempt number between them.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WORKFLOW="publish-downloads.yml"
RETRY_MAX="${RETRY_MAX:-3}"
RETRY_UNIT="${RETRY_UNIT:-3}"

is_final() { [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; }

# retry <command...>: run it up to RETRY_MAX times; its output is the last attempt's.
retry() {
	local attempt out rc
	for attempt in $(seq 1 "$RETRY_MAX"); do
		out="$("$@" 2>&1)"
		rc=$?
		if [ "$rc" -eq 0 ]; then
			printf '%s\n' "$out"
			return 0
		fi
		echo "transient: '$1 $2 $3' failed (attempt $attempt of $RETRY_MAX): $out" >&2
		[ "$attempt" -lt "$RETRY_MAX" ] && sleep $((attempt * RETRY_UNIT))
	done
	return 1
}

tags() {
	local seen=" " t
	emit() {
		case "$seen" in *" $1 "*) return ;; esac
		seen="$seen$1 "
		echo "$1"
	}
	if [ -n "${JUST_FINALIZED:-}" ] && is_final "$JUST_FINALIZED"; then emit "$JUST_FINALIZED"; fi
	if [ -n "${PUBLISH_TAG:-}" ] && is_final "$PUBLISH_TAG"; then emit "$PUBLISH_TAG"; fi
	if [ "${CATCH_UP:-0}" = 1 ]; then
		# Newest first; the first that is a real final on main is the only one considered, so a catch-up can never
		# publish an older version over a newer one.
		for t in $(git ls-remote --tags origin 'refs/tags/v*' 2>/dev/null | awk '{print $2}' | sed 's#^refs/tags/##' |
			grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V -r); do
			if "$HERE/release-npm-guard.sh" check-tag "$t" >/dev/null 2>&1; then
				emit "$t"
				break
			fi
		done
	fi
}

dispatch() {
	local tag="$1" dist green
	: "${REPO:?REPO must name the repository}"
	if ! dist="$("$HERE/release-npm-guard.sh" check-tag "$tag")"; then
		echo "::error::$tag is not a final tag on origin that main reaches — nothing to hand to the release host."
		return 1
	fi
	if [ "$dist" != latest ]; then
		echo "::error::$tag is not a final (dist-tag '$dist'): only finals go to the release host."
		return 1
	fi
	if ! git cat-file -e "refs/tags/$tag:.github/workflows/$WORKFLOW" 2>/dev/null; then
		echo "skipped: $tag was cut before .github/workflows/$WORKFLOW existed, so a run at that tag cannot publish it."
		return 0
	fi
	if ! green="$(retry gh run list --repo "$REPO" --workflow "$WORKFLOW" --branch "$tag" --status success --limit 50 \
		--json displayTitle --jq '[.[] | select(.displayTitle | contains("dry run") | not)] | length')"; then
		echo "::error::persistent error: could not read the earlier runs of $WORKFLOW at $tag."
		return 1
	fi
	if [ "${green:-0}" -gt 0 ] 2>/dev/null; then
		echo "skipped: $tag already has a successful run of $WORKFLOW — the release host reported it published."
		return 0
	fi
	if ! retry gh workflow run "$WORKFLOW" --repo "$REPO" --ref "$tag" -f "tag=$tag" >/dev/null; then
		echo "::error::persistent error: could not start $WORKFLOW at $tag."
		return 1
	fi
	echo "dispatched: $WORKFLOW at $tag."
}

case "${1:-}" in
tags) tags ;;
dispatch)
	[ $# -eq 2 ] || { echo "usage: $0 dispatch <tag>" >&2; exit 2; }
	dispatch "$2"
	;;
*)
	echo "usage: $0 tags | dispatch <tag>" >&2
	exit 2
	;;
esac
