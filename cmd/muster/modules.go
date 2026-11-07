package main

// Optional external delivery modules (#185): reading the switch.
//
// The mechanism is in internal/delivery/modclient and the tmux driver. This
// file is only the composition root's half — turning three environment
// variables into "which modules are enabled, which of those are installed, and
// what does the child see" — and the doctor row that names the difference.
//
//	FLEET_DELIVERY_MODULES      ordered, comma-separated module names; empty or
//	                            unset means none, and this daemon behaves exactly
//	                            as it did before modules existed
//	FLEET_MODULES_DIR           where module executables live; default
//	                            <prefix>/libexec/muster/modules, <prefix>
//	                            being the parent of this binary's directory,
//	                            or the npm install prefix when this binary was
//	                            installed by npm (#414)
//	FLEET_DELIVERY_MODULE_ENV   comma-separated environment names forwarded to
//	                            the module child; nothing else is, and a FLEET_
//	                            name never is
//
// "Fleet-wide" means the same value in every machine's service environment; a
// machine overrides it in its own unit. No config-file field is added.

import (
	"fmt"
	"os"
	"strings"

	"github.com/futurelastic/muster/internal/delivery/modclient"
	"github.com/futurelastic/muster/internal/drivers/tmux"
	"github.com/futurelastic/muster/internal/service"
)

// deliveryModuleSetup is what the switch resolves to.
type deliveryModuleSetup struct {
	// Enabled is every valid name the operator listed, in order, installed or
	// not: the set a caller may force with `route`, and the modules a session
	// may be offered.
	Enabled []string
	// Config is the tmux driver's configuration; its Clients are the enabled
	// modules whose executable was found.
	Config tmux.ModulesConfig
	// Dir is the directory looked in.
	Dir string
	// Problems are enabled names that were skipped, with the reason. Invalid
	// are entries that are not legal module names at all.
	Problems []modclient.Problem
	Invalid  []string
	// Dropped are names FLEET_DELIVERY_MODULE_ENV asked to forward and were
	// refused.
	Dropped []string
}

// loadDeliveryModules resolves the switch. exe is this binary's path (for the
// default directory) and stateDir is FLEET_STATE_DIR, which the child is told
// about. Nothing is started and nothing is written: a daemon with no module
// enabled gets the zero setup.
func loadDeliveryModules(getenv func(string) string, exe, stateDir string) deliveryModuleSetup {
	names, invalid := modclient.ParseModuleList(getenv("FLEET_DELIVERY_MODULES"))
	setup := deliveryModuleSetup{Invalid: invalid}
	if len(names) == 0 {
		return setup
	}
	setup.Enabled = names
	setup.Dir = getenv("FLEET_MODULES_DIR")
	if setup.Dir == "" && exe != "" {
		setup.Dir = modclient.DefaultModulesDir(exe)
	}
	found, problems := modclient.Discover(setup.Dir, names)
	setup.Problems = problems

	env, dropped := modclient.ChildEnv(getenv, stateDir, splitList(getenv("FLEET_DELIVERY_MODULE_ENV")))
	setup.Dropped = dropped

	setup.Config = tmux.ModulesConfig{Enabled: names}
	for _, f := range found {
		setup.Config.Clients = append(setup.Config.Clients, modclient.Config{Name: f.Name, Path: f.Path, Env: env})
	}
	return setup
}

// describe returns the log lines that say what was resolved — names and
// reasons only, never a path: this process's stdout is not the place a
// machine's filesystem layout belongs.
func (s deliveryModuleSetup) describe() []string {
	var out []string
	if len(s.Enabled) == 0 && len(s.Invalid) == 0 {
		return nil
	}
	for _, n := range s.Invalid {
		out = append(out, fmt.Sprintf("delivery module %q ignored: not a legal module name (#185)", n))
	}
	for _, p := range s.Problems {
		out = append(out, fmt.Sprintf("delivery module %q enabled but skipped: %s (#185)", p.Name, p.Reason))
	}
	for _, n := range s.Dropped {
		out = append(out, fmt.Sprintf("delivery module env %q not forwarded to the module (#185)", n))
	}
	if len(s.Config.Clients) > 0 {
		names := make([]string, len(s.Config.Clients))
		for i, c := range s.Config.Clients {
			names[i] = c.Name
		}
		out = append(out, fmt.Sprintf("delivery modules configured: %s (#185)", strings.Join(names, ", ")))
	}
	return out
}

// healthStatus is what GET /v1/health reports under `modules` (#414): the same
// facts describe() logs, in a form a poller can read. Entries the operator
// listed that are not legal module names come first in Skipped, then the enabled
// names that were not found, each with the reason discovery gave — the order
// describe() logs them in. Names and reasons only, never a path, for the same
// reason describe() carries none.
func (s deliveryModuleSetup) healthStatus() service.DeliveryModulesStatus {
	st := service.DeliveryModulesStatus{Enabled: s.Enabled}
	for _, n := range s.Invalid {
		st.Skipped = append(st.Skipped, service.SkippedDeliveryModule{Name: n, Reason: "not a legal module name"})
	}
	for _, p := range s.Problems {
		st.Skipped = append(st.Skipped, service.SkippedDeliveryModule{Name: p.Name, Reason: p.Reason})
	}
	for _, c := range s.Config.Clients {
		st.Loaded = append(st.Loaded, c.Name)
	}
	return st
}

// checkDeliveryModules is the doctor row `delivery.modules`. Per ADR 160 it
// warns and never fails: a machine that could not fetch a module — or has not
// yet — is a legitimate state, so the row names the difference and lets the
// install proceed.
func checkDeliveryModules(getenv func(string) string) doctorRow {
	row := doctorRow{ID: "delivery.modules", Refs: []int{185}}
	exe, _ := os.Executable()
	setup := loadDeliveryModules(getenv, exe, getenv("FLEET_STATE_DIR"))
	switch {
	case len(setup.Enabled) == 0 && len(setup.Invalid) == 0:
		row.Status = statusPass
		row.Summary = "no external delivery module is enabled — the built-in terminal path is the only lane"
		return row
	case len(setup.Problems) == 0 && len(setup.Invalid) == 0:
		row.Status = statusPass
		row.Summary = fmt.Sprintf("%d enabled delivery module(s), every one present", len(setup.Enabled))
		return row
	}
	row.Status = statusWarn
	var parts []string
	for _, n := range setup.Invalid {
		parts = append(parts, fmt.Sprintf("%q is not a legal module name", n))
	}
	for _, p := range setup.Problems {
		parts = append(parts, fmt.Sprintf("%q: %s", p.Name, p.Reason))
	}
	row.Summary = "an enabled delivery module is not usable on this machine — its sessions use the built-in path"
	row.Detail = strings.Join(parts, "; ") + ". A machine that could not fetch the module is a supported state; " +
		"skip this row (--skip=delivery.modules) where that is deliberate"
	return row
}
