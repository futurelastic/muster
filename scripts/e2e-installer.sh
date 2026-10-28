#!/bin/sh
# End to end for the release host's installer (.github/downloads/install.sh, #383): the real binary under a real
# `systemd --user`, not the unit tests' fake muster.
#
#   e2e-installer.sh
#
# It builds three stamped versions of this tree, serves them the way the host serves a release
# (/muster/latest.json, /muster/<version>/{SHA256SUMS,muster-v<version>-<os>-<arch>}) from a local file server, and runs
# the installer against it:
#
#   1. a clean machine: install 0.0.1 -> the binary and a healthy service
#   2. over an existing install: no version given -> the newest (0.0.2) replaces 0.0.1 and the service runs it
#   3. the same version again -> nothing to change, still healthy
#   4. a release whose checksum does not match -> refused, and 0.0.2 is still what runs
#
# What this cannot show is the host itself, or macOS: a Background launchd job needs a login session a hosted runner
# does not give (see scripts/e2e-service.sh, `lint`).
#
# It installs a real user service and --purge removes it, so it refuses to run anywhere but a CI runner. Exits
# non-zero at the first assertion that fails.
set -eu

[ "${GITHUB_ACTIONS:-}" = "true" ] || { echo "refusing: this installs and purges a real user service; it runs on a CI runner only" >&2; exit 2; }
[ "$(uname -s)" = "Linux" ] || { echo "refusing: Linux only (the macOS half stops at the plist, e2e-service.sh lint)" >&2; exit 2; }

cd "$(dirname "$0")/.."
root="$(pwd)"

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "== $* =="; }

work="$(mktemp -d)"
site="$work/site/muster"
port=18770
mkdir -p "$site"
server_pid=""
cleanup() {
	[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

os="$(go env GOOS)"
arch="$(go env GOARCH)"

release() { # release <version>: build it, stamped, into the site
	dir="$site/$1"
	mkdir -p "$dir"
	go build -trimpath -ldflags "-X github.com/futurelastic/muster.version=v$1" -o "$dir/muster-v$1-$os-$arch" ./cmd/muster
	(cd "$dir" && sha256sum "muster-v$1-$os-$arch" > SHA256SUMS)
}

step "build three releases"
release 0.0.1
release 0.0.2
release 0.0.3
# 0.0.3 is the broken one: its SHA256SUMS names the wrong hash.
printf '%s  %s\n' "$(printf '0%.0s' $(seq 1 64))" "muster-v0.0.3-$os-$arch" > "$site/0.0.3/SHA256SUMS"
printf '{"version":"0.0.2","files":{}}\n' > "$site/latest.json"

python3 -m http.server "$port" --bind 127.0.0.1 --directory "$work/site" >/dev/null 2>&1 &
server_pid=$!
for _ in $(seq 1 40); do
	curl -fsS "http://127.0.0.1:$port/muster/latest.json" >/dev/null 2>&1 && break
	sleep 0.25
done
curl -fsS "http://127.0.0.1:$port/muster/latest.json" >/dev/null || fail "the local file server did not come up"

export DL_URL="http://127.0.0.1:$port"
installer="$root/.github/downloads/install.sh"
installed="$HOME/.local/bin/muster"
addr="127.0.0.1:9470"

running_version() { # the version the RUNNING service reports, not the file on disk
	token="$(cat "$HOME/.config/muster/supervisor.token")"
	curl -fsS -H "Authorization: Bearer $token" "http://$addr/v1/health"
}

step "1. a clean machine"
[ ! -e "$installed" ] || fail "this runner already has a muster"
sh "$installer" 0.0.1 | tee "$work/out"
grep -q 'service healthy' "$work/out" || fail "the installer did not report a healthy service"
"$installed" --version | grep -q '^muster v0\.0\.1 ' || fail "the installed binary is not v0.0.1: $("$installed" --version)"
systemctl --user is-active --quiet muster.service || fail "muster.service is not active"
running_version | grep -q 'v0\.0\.1' || fail "the running service does not report v0.0.1"

step "2. over an existing install, no version given: the newest"
sh "$installer" | tee "$work/out"
grep -q 'installed muster v0\.0\.2' "$work/out" || fail "the newest version was not installed"
"$installed" --version | grep -q '^muster v0\.0\.2 ' || fail "the installed binary is not v0.0.2"
systemctl --user is-active --quiet muster.service || fail "muster.service is not active after the upgrade"
running_version | grep -q 'v0\.0\.2' || fail "the service is not running v0.0.2 after the upgrade"
"$installed" service status

step "3. the same version again"
sh "$installer" 0.0.2 | tee "$work/out"
grep -q 'Nothing to change' "$work/out" || fail "a second install of the same version changed something"
systemctl --user is-active --quiet muster.service || fail "muster.service is not active"

step "4. a release whose checksum is wrong"
if sh "$installer" 0.0.3 >"$work/out" 2>&1; then
	cat "$work/out"
	fail "a checksum mismatch was installed"
fi
cat "$work/out"
grep -q 'checksum mismatch' "$work/out" || fail "the refusal did not say why"
"$installed" --version | grep -q '^muster v0\.0\.2 ' || fail "a refused install changed the installed binary"
running_version | grep -q 'v0\.0\.2' || fail "a refused install changed what the service runs"

step "uninstall --purge"
"$installed" service uninstall --purge
echo; echo "OK"
