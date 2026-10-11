package main

import (
	"fmt"
	"runtime"

	fleet "github.com/futurelastic/muster"
)

// The local runtime choice (FLEET_RUNTIME), as a pure decision so a test can
// cover it without starting a service.
//
// Three values exist:
//
//	tmux   the interactive-terminal driver. The default everywhere it can run.
//	stub   an in-memory stand-in for tests and trials.
//	none   no primary local driver. Whatever optional runtime is enabled
//	       (FLEET_RUNTIME_OPENCODE) is the only local one, or there is none at
//	       all and the service is a front for its peers. The default on Windows.
//
// Windows ships without an interactive-terminal driver (ADR 303), so there
// `tmux` is not a choice that can be made at all: it is refused at startup,
// naming the decision, instead of being left to fail later on a missing binary.
const (
	runtimeTmux = "tmux"
	runtimeStub = "stub"
	runtimeNone = "none"
)

// adr303 is what every refusal on Windows points at.
const adr303 = "ADR 303 (docs/adr/303-windows-ships-without-an-interactive-terminal-driver.md)"

// defaultRuntimeFor is the FLEET_RUNTIME value when the variable is unset.
func defaultRuntimeFor(goos string) string {
	if goos == "windows" {
		return runtimeNone
	}
	return runtimeTmux
}

// hostOS is the operating system this binary was built for.
func hostOS() string { return runtime.GOOS }

// defaultRuntime is defaultRuntimeFor on this build's operating system.
func defaultRuntime() string { return defaultRuntimeFor(hostOS()) }

// runtimeLabel names the local runtime in a log line; an empty id is "none".
func runtimeLabel(id fleet.RuntimeId) string {
	if id == "" {
		return runtimeNone
	}
	return string(id)
}

// checkRuntimeName reports why name cannot be the local runtime on goos, or nil.
// The message is what the operator reads in the service log and from doctor.
func checkRuntimeName(goos, name string) error {
	switch name {
	case runtimeStub, runtimeNone:
		return nil
	case runtimeTmux:
		if goos == "windows" {
			return fmt.Errorf("FLEET_RUNTIME=tmux is not available on Windows: this build has no native "+
				"interactive-terminal driver (%s). Leave FLEET_RUNTIME unset (it defaults to none) and "+
				"serve terminal sessions from the Linux build inside WSL, peered with this service", adr303)
		}
		return nil
	default:
		return fmt.Errorf("unknown FLEET_RUNTIME %q (want tmux, stub or none)", name)
	}
}

// compatRefusal is why `muster compat` cannot run on goos, or "". The checks
// drive a private multiplexer server; where there is no terminal driver there is
// nothing for them to check.
func compatRefusal(goos string) string {
	if goos == "windows" {
		return "muster compat checks a build of the interactive-terminal runtime, and this build has no " +
			"terminal driver on Windows (" + adr303 + "). Run it from the Linux build inside WSL."
	}
	return ""
}
