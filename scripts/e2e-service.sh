#!/bin/sh
# End to end for `muster service` (#341): the installed unit, not the unit
# tests' fake service manager.
#
#   e2e-service.sh full <muster-binary>   Linux: install -> status -> second
#                                         install (no change) -> health ->
#                                         upgrade restarts -> uninstall, against
#                                         a real `systemd --user`.
#   e2e-service.sh lint <muster-binary>   macOS: install --no-start, then lint
#                                         the plist it wrote. Loading a Background
#                                         launchd job needs a login session a hosted
#                                         runner does not give; the Linux run is
#                                         where the lifecycle is exercised.
#
# It works in the HOME it is run in and --purge removes what it made, so run it
# on a throwaway account (a CI runner), not on a machine that has a muster
# installed. Exits non-zero at the first assertion that fails.
set -eu

mode="${1:-}"
bin="${2:-}"
if [ -z "$mode" ] || [ -z "$bin" ] || [ ! -x "$bin" ]; then
	echo "usage: $0 full|lint <muster-binary>" >&2
	exit 2
fi
bin="$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")"

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "== $* =="; }

# A multiplexer the doctor will accept. The service starts without one running;
# only the path has to name an executable file.
tmux_bin="$(command -v tmux || true)"
if [ -z "$tmux_bin" ]; then
	for c in /usr/bin/true /bin/true; do
		[ -x "$c" ] && tmux_bin="$c" && break
	done
fi

# A fixed port for the health check, away from the default.
addr="127.0.0.1:19470"

case "$mode" in
lint)
	step "install --no-start (writes the plist and runs doctor, loads nothing)"
	"$bin" service install --no-start --addr "$addr" --tmux "$tmux_bin"
	plist="$HOME/Library/LaunchAgents/com.futurelastic.muster.plist"
	[ -f "$plist" ] || fail "no plist at $plist"
	step "plutil -lint"
	plutil -lint "$plist"
	# The Background domain is the point of the launchd path (docs/install.md,
	# step 8): assert it is in what was written, not just that the XML parses.
	[ "$(plutil -extract LimitLoadToSessionType raw "$plist")" = "Background" ] || fail "LimitLoadToSessionType is not Background"
	[ "$(plutil -extract ProgramArguments.1 raw "$plist")" = "serve" ] || fail "the unit does not run 'serve'"
	[ "$(plutil -extract EnvironmentVariables.FLEET_ADDR raw "$plist")" = "$addr" ] || fail "FLEET_ADDR not written"
	# The login item (muster #480) is what brings the unit back after a reboot on a Mac
	# with a GUI login. Assert its shape, not just that the XML parses: it must be loaded
	# into gui/<uid> at login (so NO session-type limit), run once (so no KeepAlive), and
	# bootstrap the unit into user/<uid>.
	login="$HOME/Library/LaunchAgents/com.futurelastic.muster.login.plist"
	step "the login item"
	[ -f "$login" ] || fail "no login item at $login"
	plutil -lint "$login"
	[ "$(plutil -extract Label raw "$login")" = "com.futurelastic.muster.login" ] || fail "the login item has the wrong label"
	[ "$(plutil -extract RunAtLoad raw "$login")" = "true" ] || fail "the login item does not run at load"
	if plutil -extract LimitLoadToSessionType raw "$login" >/dev/null 2>&1; then fail "the login item has a session-type limit, so launchd would not load it into gui/<uid>"; fi
	if plutil -extract KeepAlive raw "$login" >/dev/null 2>&1; then fail "the login item is kept alive; it must run once per login"; fi
	cmdline="$(plutil -extract ProgramArguments.2 raw "$login")"
	case "$cmdline" in *'launchctl bootstrap "user/$uid"'*"$plist"*) ;; *) fail "the login item does not bootstrap the unit into user/<uid>: $cmdline" ;; esac
	step "second install changes nothing"
	"$bin" service install --no-start --addr "$addr" --tmux "$tmux_bin" | tee /tmp/e2e-service.out
	grep -q 'unit      unchanged' /tmp/e2e-service.out || fail "the second run rewrote the unit"
	grep -q 'login     unchanged' /tmp/e2e-service.out || fail "the second run rewrote the login item"
	step "uninstall --purge removes the login item too"
	"$bin" service uninstall --purge
	[ ! -e "$plist" ] || fail "the plist survived uninstall"
	[ ! -e "$login" ] || fail "the login item survived uninstall, and would put the service back at the next login"
	echo; echo "OK"
	;;
full)
	step "install"
	"$bin" service install --addr "$addr" --tmux "$tmux_bin" | tee /tmp/e2e-service.out
	grep -q 'health    ok' /tmp/e2e-service.out || fail "install did not report a healthy service"
	installed="$HOME/.local/bin/muster"
	[ -x "$installed" ] || fail "no binary at $installed"
	systemctl --user is-active --quiet muster.service || fail "muster.service is not active"

	step "status"
	"$bin" service status | tee /tmp/e2e-service.out
	grep -q "$installed" /tmp/e2e-service.out || fail "status does not name the installed binary"

	step "health, with the supervisor's token"
	token="$(cat "$HOME/.config/muster/supervisor.token")"
	curl -fsS -H "Authorization: Bearer $token" "http://$addr/v1/health" >/dev/null || fail "/v1/health did not answer 200"
	code="$(curl -s -o /dev/null -w '%{http_code}' "http://$addr/v1/health")"
	[ "$code" = "401" ] || fail "/v1/health without a token answered $code, want 401"

	step "second install changes nothing"
	pid_before="$(systemctl --user show -p MainPID --value muster.service)"
	"$bin" service install --addr "$addr" --tmux "$tmux_bin" | tee /tmp/e2e-service.out
	grep -q 'Nothing to change' /tmp/e2e-service.out || fail "the second run did not say it changed nothing"
	[ "$(systemctl --user show -p MainPID --value muster.service)" = "$pid_before" ] || fail "the second run restarted the service"

	step "an upgrade updates the binary and restarts"
	# A different binary with the same behaviour: the file differs, the service
	# does not need to.
	cp "$bin" "$HOME/muster-v2"
	printf '\n' >> "$HOME/muster-v2"
	chmod 0755 "$HOME/muster-v2"
	"$HOME/muster-v2" service install --addr "$addr" --tmux "$tmux_bin" | tee /tmp/e2e-service.out
	grep -q 'restarted' /tmp/e2e-service.out || fail "the upgrade did not restart the service"
	[ "$(systemctl --user show -p MainPID --value muster.service)" != "$pid_before" ] || fail "the service kept its old process"
	cmp -s "$HOME/muster-v2" "$installed" || fail "the installed binary is not the new one"

	step "uninstall"
	"$bin" service uninstall
	systemctl --user is-active --quiet muster.service && fail "the service is still running"
	[ ! -e "$HOME/.config/systemd/user/muster.service" ] || fail "the unit survived uninstall"
	[ -f "$HOME/.config/muster/config.json" ] || fail "uninstall removed the configuration"
	"$bin" service status && fail "status reported a healthy service after uninstall"
	"$bin" service uninstall --purge
	[ ! -e "$HOME/.config/muster" ] || fail "--purge left the configuration"
	rm -f "$HOME/muster-v2"
	echo; echo "OK"
	;;
*)
	echo "usage: $0 full|lint <muster-binary>" >&2
	exit 2
	;;
esac
