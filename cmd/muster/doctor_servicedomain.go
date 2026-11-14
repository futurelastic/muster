package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// The service.domain row (macOS) says which launchd domain `muster service
// install`'s unit is loaded in, and warns when it is the wrong one (muster #480).
//
// The unit is a Background job for user/<uid>. A copy loaded into gui/<uid>, the
// domain launchd restores at a GUI login, makes the next update fail (a bootstrap
// into user/<uid> is refused while that copy holds the label, so the installer
// reports "did not come up healthy" and rolls back), and every session server it
// starts runs in the desktop context, where an agent runtime's login can move into
// the Keychain. Neither stops the service, so the row is a warn, not a fail:
// `muster service install` runs doctor before it moves the copy, and a fail would
// stop it.

// serviceDomains is where launchd has the unit loaded.
type serviceDomains struct {
	UID  int
	GUI  bool // loaded in gui/<uid>
	User bool // loaded in user/<uid>
}

// doctorServiceDomain reads where the unit is loaded. ok is false where there is
// nothing to say: not macOS. A variable for the same reason as doctorServiceTask:
// the generic doctor tests must not depend on what launchd on the machine they run
// on has loaded; main_run_test.go replaces it, and the tests that are about the
// row pass their own answer.
var doctorServiceDomain = readServiceDomains

func readServiceDomains() (serviceDomains, bool) {
	if hostOS() != "darwin" {
		return serviceDomains{}, false
	}
	d := serviceDomains{UID: os.Getuid()}
	loaded := func(domain string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// An absolute path: `muster service install` runs doctor under the unit's own
		// environment, which has no PATH, and a bare name would be "not found" there.
		return exec.CommandContext(ctx, "/bin/launchctl", "print", fmt.Sprintf("%s/%d/%s", domain, d.UID, svcLabel)).Run() == nil
	}
	d.GUI, d.User = loaded("gui"), loaded("user")
	return d, true
}

// judgeServiceDomain turns where the unit is loaded into the row. Loaded nowhere
// is a skip, not an absent row: a --skip that names a row this host does not have
// is refused (exit 2), and the row must exist wherever launchd does.
func judgeServiceDomain(d serviceDomains) doctorRow {
	row := doctorRow{ID: "service.domain"}
	switch {
	case d.GUI:
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("the service is loaded in gui/%d, not user/%d: the next update fails, and the session server it starts runs in the desktop context", d.UID, d.UID)
		row.Detail = "run `muster service restore` to move it into the Background domain. That is refused, naming the command, when the loaded copy would take its sessions with it as it stops"
	case d.User:
		row.Status = statusPass
		row.Summary = fmt.Sprintf("the service is loaded in user/%d, the Background domain", d.UID)
	default:
		row.Status = statusSkip
		row.Summary = "the service is not loaded in either launchd domain (`muster service status`)"
	}
	return row
}
