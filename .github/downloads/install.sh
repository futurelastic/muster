#!/bin/sh
# Install muster from the downloads site this file was published to (#355).
#
#   curl -fsSL <site>/muster/install.sh | sh
#   curl -fsSL <site>/muster/install.sh | MUSTER_VERSION=0.5.0 sh     # an exact version
#
# This file is a TEMPLATE in the repository: the publish job stamps the site's address into it
# (publish-downloads.mjs replaces the placeholder token below), and the copy on the site is the one to run.
#
# What it does, in this order, and stops at the first thing that is wrong:
#   1. picks the build for this machine (macOS or Linux; arm64 or amd64)
#   2. reads the version: MUSTER_VERSION, else the site's latest.json
#   3. downloads that version's SHA256SUMS and the binary, and checks the binary against it
#   4. runs the binary's own `--version` and checks it names the version asked for
#   5. only then puts it at $MUSTER_INSTALL_DIR/muster (default ~/.local/bin), replacing it atomically
# A failure at any step installs nothing and leaves an installed muster as it was.
#
# Environment:
#   MUSTER_VERSION        X.Y.Z. Default: the site's newest.
#   MUSTER_INSTALL_DIR    where `muster` goes. Default: $HOME/.local/bin.
#   MUSTER_DOWNLOADS_URL  the site root, replacing the stamped address (to test against another site).
#
# After it, `muster --version` reports `muster vX.Y.Z (...)`: a dependent installer compares that with its minimum.
# It does not install or start the service: `muster service install` does that (docs/install.md).
set -eu

SITE='@DOWNLOADS_URL@'
APP=muster

die() {
	echo "muster install: $*" >&2
	exit 1
}

# The stamp is applied to every occurrence of the token above, so the unstamped check is spelled
# without writing the token again.
ROOT=${MUSTER_DOWNLOADS_URL:-$SITE}
case "$ROOT" in
'@'DOWNLOADS_URL'@') die "this copy was never published, so it has no site address; set MUSTER_DOWNLOADS_URL to the site root" ;;
esac
ROOT=${ROOT%/}
BASE=$ROOT/$APP

fetch() { # fetch URL DEST
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 2 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "neither curl nor wget is installed"
	fi
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "neither sha256sum nor shasum is installed, so the download cannot be checked"
	fi
}

case "$(uname -s)" in
Darwin) OS=darwin ;;
Linux) OS=linux ;;
*) die "$(uname -s) is not supported by this installer (macOS and Linux only)" ;;
esac
case "$(uname -m)" in
arm64 | aarch64) ARCH=arm64 ;;
x86_64 | amd64) ARCH=amd64 ;;
*) die "$(uname -m) is not a supported machine type (arm64 or amd64)" ;;
esac

TMP=$(mktemp -d "${TMPDIR:-/tmp}/muster-install.XXXXXX") || die "cannot make a temporary directory"
trap 'rm -rf "$TMP"' EXIT INT TERM

VERSION=${MUSTER_VERSION:-}
if [ -z "$VERSION" ]; then
	fetch "$BASE/latest.json" "$TMP/latest.json" || die "cannot read $BASE/latest.json (is the site reachable?)"
	VERSION=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([0-9][0-9.]*\)".*/\1/p' "$TMP/latest.json" | head -n 1)
	[ -n "$VERSION" ] || die "$BASE/latest.json names no version"
fi
VERSION=${VERSION#v}
# X.Y.Z, digits and exactly two dots, none leading, trailing or doubled.
case "$VERSION" in
'' | *[!0-9.]* | .* | *. | *..*) die "'$VERSION' is not a version (want X.Y.Z)" ;;
esac
[ "$(printf '%s' "$VERSION" | tr -cd . | wc -c | tr -d ' ')" = 2 ] || die "'$VERSION' is not a version (want X.Y.Z)"

NAME=$APP-v$VERSION-$OS-$ARCH
echo "muster install: $NAME from $BASE/$VERSION"
fetch "$BASE/$VERSION/SHA256SUMS" "$TMP/SHA256SUMS" || die "cannot read $BASE/$VERSION/SHA256SUMS (no such version, or the site is unreachable)"
WANT=$(awk -v n="$NAME" '$2 == n { print $1 }' "$TMP/SHA256SUMS")
[ -n "$WANT" ] || die "version $VERSION has no build for $OS/$ARCH"
fetch "$BASE/$VERSION/$NAME" "$TMP/$NAME" || die "cannot download $BASE/$VERSION/$NAME"
GOT=$(sha256_of "$TMP/$NAME")
[ "$GOT" = "$WANT" ] || die "checksum mismatch for $NAME (SHA256SUMS says $WANT, the download is $GOT); nothing was installed"

chmod +x "$TMP/$NAME"
REPORTED=$("$TMP/$NAME" --version 2>&1) || die "the downloaded binary does not run on this machine; nothing was installed"
case "$REPORTED" in
"muster v$VERSION "* | "muster v$VERSION") ;;
*) die "the binary reports '$REPORTED', not v$VERSION; nothing was installed" ;;
esac

DIR=${MUSTER_INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$DIR" || die "cannot create $DIR"
cp "$TMP/$NAME" "$DIR/.muster.new.$$" || die "cannot write to $DIR"
mv -f "$DIR/.muster.new.$$" "$DIR/muster" || die "cannot replace $DIR/muster"

echo "muster install: installed $REPORTED at $DIR/muster"
case ":$PATH:" in
*":$DIR:"*) ;;
*) echo "muster install: $DIR is not on PATH; add it, or run $DIR/muster" ;;
esac
