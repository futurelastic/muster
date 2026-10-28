#!/bin/sh
# Install a released muster for the current user, and (re)start its user service — #383.
#
#   curl -fsSL <host>/muster/install.sh | sh                 # the newest release the host offers
#   curl -fsSL <host>/muster/install.sh | sh -s -- 0.4.0     # an exact version
#   sh install.sh 0.4.0
#
# The copy the host serves has its own address stamped in where the assignment below stands; the publish job does
# that. This file in the repository is the template, and carries no address.
#
# WHAT IT DOES, in order, and what it leaves alone when a step fails
#   1. picks this machine's build (macOS arm64 / x86_64, Linux x86_64 / arm64);
#   2. downloads it and checks it against the version's SHA256SUMS. It does not install anything until the checksum
#      matches, and then checks the binary names the release it was asked for (`muster --version`);
#   3. keeps a copy of the muster already installed, if there is one;
#   4. runs `muster service install` with the NEW binary. That command is the install: it copies the binary to
#      ~/.local/bin/muster, creates the state directory and a principal table (never overwriting one), writes the
#      user service's unit, runs `muster doctor` under the unit's environment, restarts the service when the binary
#      or the unit changed, and waits for /v1/health. A machine that never had muster gets a new service; a machine
#      that has one keeps its address, machine id and tmux path and gets the new binary under it;
#   5. runs `muster service status`, which exits 1 unless the unit is present, loaded, running and healthy.
# If step 4 or 5 fails and there was an installed muster, it is put back: the saved copy runs its own
# `service install`, which is the same command in the other direction. The exit status is 1 either way, so a
# caller (an updater, a person) sees that the version it asked for is not the one running.
#
#   DL_URL     the host's address, instead of the stamped one (another host, or a test)
#   DL_TOKEN   a read token for a host that wants one; sent as a bearer header, never on a command line
#   MUSTER_VERSION   X.Y.Z, when the version is not given as an argument
#
# Needs curl (or wget, without a token), sha256sum or shasum, and tmux for the service (see docs/install.md).
set -eu

HOST=${DL_URL:-@RELEASE_ORIGIN@}
HOST=${HOST%/}
APP=muster

fail() { echo "muster install: $*" >&2; exit 1; }

case $HOST in
  http://* | https://*) ;;
  *) fail "this copy of the installer has no host address: fetch it from the host, or set DL_URL" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { # fetch <url> <file>
    if [ -n "${DL_TOKEN:-}" ]; then
      # The header goes in through stdin: an argument would be readable by every account on the machine.
      printf 'header = "Authorization: Bearer %s"\n' "$DL_TOKEN" | curl -fsSL --retry 2 -K - -o "$2" "$1"
    else
      curl -fsSL --retry 2 -o "$2" "$1"
    fi
  }
elif command -v wget >/dev/null 2>&1; then
  [ -z "${DL_TOKEN:-}" ] || fail "DL_TOKEN needs curl: wget would put the token on its command line"
  fetch() { wget -q -O "$2" "$1"; }
else
  fail "needs curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  fail "needs sha256sum or shasum"
fi

os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
  Darwin/arm64 | Darwin/aarch64) target=darwin-arm64 ;;
  Darwin/x86_64 | Darwin/amd64) target=darwin-amd64 ;;
  Linux/x86_64 | Linux/amd64) target=linux-amd64 ;;
  Linux/aarch64 | Linux/arm64) target=linux-arm64 ;;
  *) fail "no build for $os/$arch (builds: macOS arm64 and x86_64, Linux x86_64 and arm64)" ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

version=${1:-${MUSTER_VERSION:-}}
if [ -z "$version" ]; then
  fetch "$HOST/$APP/latest.json" "$tmp/latest.json" || fail "cannot read $HOST/$APP/latest.json"
  version=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)
fi
version=${version#v}
case $version in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) fail "\"$version\" is not a version (X.Y.Z)" ;;
esac
case $version in *[!0-9.]*) fail "\"$version\" is not a version (X.Y.Z)" ;; esac

file="$APP-v$version-$target"
base="$HOST/$APP/$version"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || fail "cannot read $base/SHA256SUMS: is $version published?"
want=$(awk -v f="$file" '$2 == f { print $1; exit }' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "$file is not in $version's SHA256SUMS"
fetch "$base/$file" "$tmp/$file" || fail "cannot download $base/$file"
got=$(sha256_of "$tmp/$file")
[ "$got" = "$want" ] || fail "checksum mismatch for $file (expected $want, got $got): nothing was installed"
chmod 755 "$tmp/$file"

reported=$("$tmp/$file" --version 2>&1) || fail "the downloaded $file does not run on this machine: $reported"
case $reported in
  "muster v$version "*) ;;
  *) fail "the downloaded $file reports \"$reported\", not v$version: nothing was installed" ;;
esac

installed=$HOME/.local/bin/$APP
had_previous=0
if [ -x "$installed" ]; then
  cp -p "$installed" "$tmp/previous"
  had_previous=1
  previous=$("$tmp/previous" --version 2>&1 | head -n 1) || previous="the previous muster"
fi

echo "installing muster v$version ($target)"
if "$tmp/$file" service install && "$installed" service status; then
  echo "installed muster v$version ($target), service healthy"
  case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) echo "note: $HOME/.local/bin is not on your PATH" ;; esac
  exit 0
fi

echo "muster install: v$version did not come up healthy." >&2
if [ "$had_previous" = 1 ]; then
  echo "muster install: putting back $previous" >&2
  if "$tmp/previous" service install >/dev/null 2>&1; then
    echo "muster install: $previous is installed and running again; v$version is not" >&2
  else
    echo "muster install: could not put the previous version back; run 'muster service status' and see docs/install.md" >&2
  fi
fi
exit 1
