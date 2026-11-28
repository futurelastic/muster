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
#   4. runs `muster service install` with the NEW binary. That command is the install: it creates the state directory
#      and a principal table (never overwriting one), writes the user service's unit, runs `muster doctor` under the
#      unit's environment, then copies the binary to ~/.local/bin/muster, restarts the service when the binary, the
#      unit or the build the running service reports is not the installed one, and waits for /v1/health. A machine that never had muster gets a new service; a machine
#      that has one keeps its address (a network address too, when the machine serves federation peers), machine id,
#      tmux path and operator variables and gets the new binary under it. A machine that runs muster under a unit of
#      its own (a label an operator made) is taken over once, by naming the unit in MUSTER_ADOPT; see below;
#   5. runs `muster service status`, which exits 1 unless the unit is present, loaded, running and healthy, and the
#      healthy service reports the build of the installed binary.
# If step 4 or 5 fails and there was an installed muster, it is put back: the saved copy runs its own
# `service install`, which is the same command in the other direction. The exit status is 1 either way, so a
# caller (an updater, a person) sees that the version it asked for is not the one running.
# Each failure says why in a `muster install:` line, for a caller that keeps only those: step 4 failing reads "v<x> was not
# started: service install exited N: <its last error line> [failed doctor check: <rows>]" (the new version never ran),
# step 5 failing reads "did not come up healthy" (it ran), and a put-back that fails carries its own last error line. When
# both binaries stop at the doctor gate the running service was never touched, and the installer says nothing needs putting
# back (muster #521).
# One failure of step 4 is not rolled back: `muster service install` exits 3 when it refused before it changed
# anything (a unit of the operator's beside where the standard unit goes, a refused --adopt, no tmux). There is
# nothing to undo, and the previous binary's own install could do harm (one older than that refusal writes the
# second muster the refusal prevented) or report a failed rollback of nothing. The installer says the machine
# was not changed and exits 1 with the previous version as it was.
#
#   DL_URL     the host's address, instead of the stamped one (another host, or a test)
#   DL_TOKEN   a read token for a host that wants one; sent as a bearer header, never on a command line
#   MUSTER_VERSION   X.Y.Z, when the version is not given as an argument
#   INSTALL_MIN      X.Y.Z. Set by an app that requires muster: "nothing to do when muster is already at least this
#                    version". When ~/.local/bin/muster reports that version or a later one, the installer says so,
#                    exits 0 and changes nothing: it does not look up the newest release, download or restart anything,
#                    and a version given as an argument or in MUSTER_VERSION is not installed either. When muster is
#                    older, absent or does not report a version, it installs as usual. A value that is not X.Y.Z is
#                    an error (exit 1) before anything happens, not a reason to upgrade.
#   MUSTER_ADOPT     the launchd label, systemd unit name, or file of a unit that already runs muster on this machine
#                    under a name of its own. Used once: the first run passes it to `muster service install --adopt`,
#                    which moves that unit's address and FLEET_*, LANG and LC_* variables into the standard unit and
#                    stops and puts away the old one, so there is one muster, not two. Every later run is an ordinary
#                    update and finds the standard unit; leaving the variable set is harmless (the command says the
#                    unit was taken over earlier). A rollback never passes it: the previous binary does not know it.
#
# Needs curl (or wget, without a token), sha256sum or shasum, and tmux for the service (see docs/install.md).
set -eu

HOST=${DL_URL:-@RELEASE_ORIGIN@}
HOST=${HOST%/}
APP=muster

fail() { echo "muster install: $*" >&2; exit 1; }

# What `muster service install` exits with when it refused before changing anything: svcExitNothingChanged in
# cmd/muster/service.go. A test holds the two numbers equal.
REFUSED_UNCHANGED=3

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

# ver_ge <a> <b>: success when version a is at least version b. Both are X.Y.Z, digits and dots only (checked by the caller).
ver_ge() {
  _a=$1 _b=$2
  while [ -n "$_a" ] || [ -n "$_b" ]; do
    _x=${_a%%.*} _y=${_b%%.*}
    [ "${_x:-0}" -gt "${_y:-0}" ] && return 0
    [ "${_x:-0}" -lt "${_y:-0}" ] && return 1
    case $_a in *.*) _a=${_a#*.} ;; *) _a= ;; esac
    case $_b in *.*) _b=${_b#*.} ;; *) _b= ;; esac
  done
  return 0
}

installed=$HOME/.local/bin/$APP

# INSTALL_MIN: an app that requires muster only needs this much, so a muster that has it is left exactly as it is.
min=${INSTALL_MIN:-}
if [ -n "$min" ]; then
  min=${min#v}
  case $min in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) fail "INSTALL_MIN \"$min\" is not a version (X.Y.Z)" ;;
  esac
  case $min in *[!0-9.]* | *..* | *.) fail "INSTALL_MIN \"$min\" is not a version (X.Y.Z)" ;; esac
  if [ -x "$installed" ] && have=$("$installed" --version 2>/dev/null | head -n 1); then
    have=$(printf '%s\n' "$have" | awk '{ print $2; exit }')
    have=${have#v}
    case $have in
      [0-9]*.[0-9]*.[0-9]*)
        case $have in
          *[!0-9.]* | *..* | *.) ;;
          *)
            if ver_ge "$have" "$min"; then
              echo "muster v$have is installed, which is at least INSTALL_MIN $min: nothing to do"
              exit 0
            fi ;;
        esac ;;
    esac
  fi
fi

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

had_previous=0
if [ -x "$installed" ]; then
  cp -p "$installed" "$tmp/previous"
  had_previous=1
  previous=$("$tmp/previous" --version 2>&1 | head -n 1) || previous="the previous muster"
fi

# The new binary's own install; the one place MUSTER_ADOPT is passed (see the top).
new_service_install() {
  if [ -n "${MUSTER_ADOPT:-}" ]; then
    "$tmp/$file" service install --adopt "$MUSTER_ADOPT"
  else
    "$tmp/$file" service install
  fi
}

# why <stdout file> <stderr file>: one line that says why a `service install` failed. The last line it printed on
# stderr (the error itself, without the "muster service install:" prefix), and the doctor rows that failed, when it ran
# the doctor (muster #521). A caller that keeps only the `muster install:` lines still gets the cause.
why() {
  _err=$(sed -e '/^[[:space:]]*$/d' -e 's/^muster service [a-z]*: //' "$2" 2>/dev/null | tail -n 1)
  [ -n "$_err" ] || _err=$(sed -e '/^[[:space:]]*$/d' "$1" 2>/dev/null | tail -n 1)
  [ -n "$_err" ] || _err="it printed nothing"
  _rows=$(awk '$1 == "FAIL" { printf "%s%s", sep, $2; sep = ", " }' "$1" 2>/dev/null)
  if [ -n "$_rows" ]; then
    echo "$_err [failed doctor check: $_rows]"
  else
    echo "$_err"
  fi
}

# doctor_refused <stderr file>: `service install` stopped at its doctor gate (cmd/muster/service.go says so in this
# phrase, and a test holds the two together). That is before the service was restarted, so the running one was not touched.
doctor_refused() { grep -q 'doctor reported a failure' "$1" 2>/dev/null; }

echo "installing muster v$version ($target)"
new_status=0
# The output is kept so a failure can be reported with its reason, then shown as it was: stdout on stdout, stderr on stderr.
new_service_install >"$tmp/new.out" 2>"$tmp/new.err" || new_status=$?
cat "$tmp/new.out"
cat "$tmp/new.err" >&2
if [ "$new_status" = 0 ] && "$installed" service status; then
  echo "installed muster v$version ($target), service healthy"
  case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) echo "note: $HOME/.local/bin is not on your PATH" ;; esac
  exit 0
fi

# A refusal that changed nothing has nothing to put back (see the top).
if [ "$new_status" = "$REFUSED_UNCHANGED" ]; then
  if [ "$had_previous" = 1 ]; then
    echo "muster install: v$version was not installed: the install refused before changing anything, so this machine is as it was and $previous was not replaced" >&2
  else
    echo "muster install: v$version was not installed: the install refused before changing anything, so this machine is as it was" >&2
  fi
  exit 1
fi

new_doctor=0
if [ "$new_status" != 0 ]; then
  # `service install` itself failed: the new version never started. Say that, and why.
  doctor_refused "$tmp/new.err" && new_doctor=1
  echo "muster install: v$version was not started: service install exited $new_status: $(why "$tmp/new.out" "$tmp/new.err")" >&2
else
  # The install finished and `service status` then failed: the new version started and is not healthy.
  echo "muster install: v$version did not come up healthy." >&2
fi
if [ "$had_previous" = 1 ]; then
  echo "muster install: putting back $previous" >&2
  if "$tmp/previous" service install >"$tmp/prev.out" 2>"$tmp/prev.err"; then
    echo "muster install: $previous is installed and running again; v$version is not" >&2
  elif [ "$new_doctor" = 1 ] && doctor_refused "$tmp/prev.err" && "$tmp/previous" service status >/dev/null 2>&1; then
    # Both stopped at the same gate, before the restart: the service that was running was never stopped.
    echo "muster install: $previous stopped at the same doctor gate: $(why "$tmp/prev.out" "$tmp/prev.err")" >&2
    echo "muster install: the service that was running is still healthy and was never stopped, so nothing needs putting back" >&2
  else
    echo "muster install: could not put the previous version back: $(why "$tmp/prev.out" "$tmp/prev.err"); run 'muster service status' and see docs/install.md" >&2
  fi
fi
exit 1
