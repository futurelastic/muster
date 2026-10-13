#!/bin/sh
# Cross-compile muster for every supported platform and write a SHA256SUMS
# file beside the binaries. This is the build half of the npm release (#241,
# #243); .github/workflows/release-npm.yml calls it and hands the output to the
# job that assembles and publishes the npm packages (scripts/assemble-npm.mjs).
#
# USAGE
#
#   scripts/build-release.sh OUTDIR [VERSION]
#
# Run it from inside the checkout to build — the CURRENT directory is the
# source tree, not the directory this script lives in. That split is what lets
# the workflow build a tag that predates this script: it checks the tag out
# next to a checkout of the current tooling and runs the tooling's copy here.
#
#   OUTDIR    where the binaries and SHA256SUMS land (created if absent).
#   VERSION   the release tag being built. Defaults to `git describe` of the
#             tree, exactly as scripts/deploy.sh stamps it. A version that is
#             not a "v<digit>..." tag is refused: build.go reports anything
#             else as unstamped, so a binary built under it would answer
#             `--version` with something other than the tag it is published
#             under — the one check a downloader can make.
#
# Output files are named muster-<version>-<os>-<arch>, with ".exe" appended for
# windows. The name is part of the contract: SHA256SUMS lists these exact names,
# so `shasum -a 256 -c` run next to a downloaded file verifies it without
# anyone renaming anything.
#
# The windows binaries are built on every run but are not published to npm
# (scripts/assemble-npm.mjs leaves them out unless told otherwise): they are
# there so the Windows end-to-end job (scripts/e2e-windows.ps1) installs what the
# release would build, and so their promotion later is a one-line change.
#
# WHICH TARGETS A TAG CAN BUILD (#316). This script is the TOOLING's copy, run
# against an older tag's source, so the target list cannot be assumed to match
# what that source supports. A tag cut before the Windows work does not compile
# for windows (it calls syscall.Kill), and trunk's script used to fail the whole
# release on it, on every scheduled retry. The four unix targets are required:
# any of them failing to build fails the release. The windows targets are
# optional: a source that does not build for one is skipped with a warning, and
# the release carries only what was built. Nothing downstream needs the skipped
# files: assemble-npm.mjs does not assemble an unpublished platform, and the
# Windows end-to-end job builds trunk and asks for the windows packages with
# --all, so a windows build that broke on trunk still fails there, loudly.
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	echo "usage: $0 OUTDIR [VERSION]" >&2
	exit 2
fi
OUTDIR=$1
VERSION=${2:-$(git describe --tags --match 'v[0-9]*' 2>/dev/null || true)}

case "$VERSION" in
v[0-9]*) ;;
*)
	echo "build-release: '${VERSION}' is not a release tag (want v<digit>...); refusing to build an unstamped release" >&2
	exit 1
	;;
esac

# A release is built from committed source. -buildvcs records vcs.modified, and
# a modified build has no identity to report.
if [ -n "$(git status --porcelain)" ]; then
	echo "build-release: working tree is dirty; a release is built from a clean checkout" >&2
	exit 1
fi

mkdir -p "$OUTDIR"
OUTDIR=$(cd "$OUTDIR" && pwd)
rm -f "$OUTDIR"/muster-"$VERSION"-* "$OUTDIR"/SHA256SUMS

REQUIRED="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64"
OPTIONAL="windows/amd64 windows/arm64"
skipped=""

for target in $REQUIRED $OPTIONAL; do
	os=${target%/*}
	arch=${target#*/}
	ext=""
	[ "$os" = windows ] && ext=".exe"
	out="$OUTDIR/muster-${VERSION}-${os}-${arch}${ext}"
	echo "build-release: ${os}/${arch} -> $(basename "$out")"
	# CGO_ENABLED=0: the service is standard-library only, and a static binary
	# runs on a Linux host whatever libc it has. -trimpath keeps the builder's
	# filesystem layout out of the artefact.
	if CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=true \
		-ldflags "-s -w -X github.com/futurelastic/muster.version=${VERSION}" \
		-o "$out" ./cmd/muster; then
		continue
	fi
	rm -f "$out"
	case " $OPTIONAL " in
	*" $target "*)
		echo "build-release: warning: this source does not build for ${target}; skipping it (a release cut before that platform was supported carries no binary for it)" >&2
		skipped="$skipped $target"
		;;
	*)
		echo "build-release: ${target} is a required target and did not build" >&2
		exit 1
		;;
	esac
done

# sha256sum on Linux, shasum on macOS; either writes the "<hash>  <name>"
# format both `sha256sum -c` and `shasum -a 256 -c` read.
cd "$OUTDIR"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum muster-"$VERSION"-* >SHA256SUMS
else
	shasum -a 256 muster-"$VERSION"-* >SHA256SUMS
fi
echo "build-release: wrote $(wc -l <SHA256SUMS | tr -d ' ') binaries and SHA256SUMS to ${OUTDIR}"
if [ -n "$skipped" ]; then
	echo "build-release: not built:${skipped}"
fi
