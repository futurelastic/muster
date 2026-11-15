package main

import (
	"fmt"
	"strings"

	"github.com/futurelastic/muster/internal/sessionenv"
)

// checkSessionEnvFiles answers the sessionenv.files row (muster #487): which
// optional sessionEnv entries read from a file hold a file that is there and
// unusable.
//
// An optional entry is allowed to have no value, and a create goes on without
// it. That is the right answer when the file does not exist and the wrong thing
// to leave unsaid when the file is there but empty, or holds a value with an
// interior newline or a NUL, or cannot be read: the operator sees only a
// downstream symptom, a session that starts without a variable it was meant to
// have. Each create logs one line for it; this row is the same fact read before
// any create, so a pretty-printed value is found when the file is written and
// not hours later.
//
// The question is asked of sessionenv.CheckFile, the function a create's own
// answer comes from, so the row cannot say "usable" about a file a create then
// leaves out. A file that does not exist is not a finding: the row passes and
// says how many such entries there are. Every entry is judged whatever its
// appliesTo, because a file is unusable for every session it could reach.
//
// A required entry is not judged here: a create naming one is refused outright
// (the operator is told on the first one), and this row is about the case where
// nobody is told. Like every row it names no path and no value, only the variable
// and a reason that holds neither. It reads each file but keeps none of what it
// reads. No row on a machine with no optional file entry, and none when the
// config file did not load (cfg is nil), so a report from one is unchanged.
func checkSessionEnvFiles(cfg *fileConfig) (doctorRow, bool) {
	if cfg == nil {
		return doctorRow{}, false
	}
	row := doctorRow{ID: "sessionenv.files", Refs: []int{487}}
	checked, absent := 0, 0
	var names, reasons []string
	for _, e := range cfg.sessionEnv() {
		if e.FromFile == "" || e.Required {
			continue
		}
		checked++
		switch state, reason := sessionenv.CheckFile(e.FromFile); state {
		case sessionenv.FileAbsent:
			absent++
		case sessionenv.FileUnusable:
			names = append(names, e.Name)
			reasons = append(reasons, e.Name+": "+reason)
		}
	}
	switch {
	case checked == 0:
		return doctorRow{}, false
	case len(names) > 0:
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%d optional sessionEnv file(s) are present but unusable — creates start without %s",
			len(names), strings.Join(names, ", "))
		row.Detail = strings.Join(reasons, "; ") + ". A fromFile value must be one line: not empty, with no " +
			"interior newline or NUL. A longer value does not belong in sessionEnv " +
			"(opencode.agents holds multi-line agent definitions). A file that does not exist is not " +
			"reported, because an optional entry may be absent"
	default:
		row.Status = statusPass
		row.Summary = fmt.Sprintf("%d optional sessionEnv file(s) checked: none is present but unusable", checked)
		if absent > 0 {
			row.Summary += fmt.Sprintf("; %d absent, which an optional entry allows", absent)
		}
	}
	return row, true
}
