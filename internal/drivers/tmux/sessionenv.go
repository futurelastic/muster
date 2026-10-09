package tmux

import (
	"fmt"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster issue #94: an operator declaring what THIS machine's sessions
// always carry, instead of every caller having to pass the same variable on
// every create forever — and the one caller that forgets not failing, but
// starting a healthy-looking session that falls back to whatever ambient
// identity the consuming tool finds on disk.
//
// # Why this lives in the driver, not the HTTP handler
//
// A create aimed at this machine can arrive two ways: served locally, or
// relayed here by a peer that merely forwards the request body — env
// included — verbatim (internal/drivers/remote). Provisioning in the HTTP
// handler would run on every request THIS SERVICE HANDLES, including one it
// is only relaying onward to a THIRD machine, which would read this
// machine's files and ship the value over the network to a session that
// will run somewhere else entirely. "Per-machine identity" has to mean the
// machine that runs the session, not the machine that took the call, so
// this only runs where a session is actually started — Driver.Create,
// after the relay decision has already been made by whoever resolved which
// driver instance to call.
//
// # Why the declaration is loaded once and the value is not
//
// The list of entries — which variables, from which files, required or not,
// scoped to what — is machine configuration, read at daemon start
// (cmd/muster/config.go's DisallowUnknownFields means it must exist in
// the code before it can exist in a config file at all, so enabling this
// needs a restart same as DefaultRuntime and TrustRoots).
//
// The VALUE behind each entry is not cached alongside it. It is read fresh
// from FromFile on every Create. Caching it at startup was the obvious
// implementation and the wrong one: it would silently defeat credential
// rotation — rotate the file, and every session created afterward would
// keep receiving the old value until somebody restarted the service, with
// nothing to say why. Reading per create means a rotation takes effect on
// the very next session, no restart, no coordination.

// SessionEnvScope and SessionEnvEntry are the machine-declared environment's
// types, which moved to internal/sessionenv when a second driver began to apply
// them. The aliases keep this package's public surface unchanged.
type (
	SessionEnvScope = sessionenv.Scope
	SessionEnvEntry = sessionenv.Entry
)

// ValidateSessionEnv checks every entry's shape once, at startup. See
// sessionenv.Validate.
func ValidateSessionEnv(entries []SessionEnvEntry) error {
	return sessionenv.Validate(entries)
}

// ValidateSessionEnvReserved refuses a configured sessionEnv entry naming a
// variable this driver's delivery module reserves (#180): the module is the
// sole setter of those, and a configured value would silently compete with
// it. Separate from ValidateSessionEnv because it needs the constructed
// driver — which module is installed is not a fact about the entries alone.
func (d *Driver) ValidateSessionEnvReserved() error {
	reserved := d.ReservedEnv()
	if len(reserved) == 0 || len(d.sessionEnv) == 0 {
		return nil
	}
	names := make(map[string]string, len(d.sessionEnv))
	for _, e := range d.sessionEnv {
		names[e.Name] = ""
	}
	if err := delivery.CheckReservedEnv(names, reserved); err != nil {
		return fmt.Errorf("sessionEnv: %w", err)
	}
	return nil
}

// provisionSessionEnv merges this machine's declared identity into spec.Env —
// the precedence table of muster issue #94, implemented once in
// sessionenv.Provision. The only part that is this driver's own is the
// delivery-module carve-out (#185): a name a delivery module reserves by prefix
// is the module's to set, so a configured entry naming one is dropped here —
// the prefixes are only known once a module has declared them, so this cannot
// be refused at startup the way an exact name is.
func (d *Driver) provisionSessionEnv(spec fleet.SessionSpec) (map[string]string, error) {
	if len(d.sessionEnv) == 0 {
		return spec.Env, nil
	}
	reservedPrefixes := d.ReservedEnvPrefixes()
	return sessionenv.Provision(d.sessionEnv, spec, d.machine, func(name string) bool {
		if delivery.HasReservedPrefix(name, reservedPrefixes) {
			d.counters.incr("module.session_env_dropped")
			return true
		}
		return false
	})
}
