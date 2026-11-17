package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// `muster service install --adopt UNIT` (muster #492).
//
// A machine that serves federation peers often runs muster under a unit somebody
// made by hand: its own label, a listen address on a network interface, the
// operator's FLEET_* variables. The Library's update path runs `service install`,
// which only knew the standard unit, so on such a machine it wrote that unit
// beside the real one (a second muster on 127.0.0.1:9470), failed its health check
// and rolled back, and every release was marked blocked.
//
// Adopting is a one-time move of that unit into the standard one. What install
// reads back from a unit (the address, the machine id, tmux, PATH and every
// FLEET_*, LANG and LC_* variable) is read from the named unit instead of the
// standard one; the standard unit is written with it; and the named unit is then
// stopped and its file put away, so that the machine ends with one unit. From then
// on the machine is an ordinary one: the next update is `service install` with no
// flags, it finds the standard unit, and it keeps the address and the variables
// the way it keeps them anywhere.
//
// It replaces the unit under the standard label rather than updating the old one
// under its own. That is deliberate. Everything else here (status, restore, the
// login item of #480, uninstall, an older binary the installer rolls back to)
// knows exactly one label and one file, and a second label threaded through all of
// them would be one more place for the two kinds of machine to differ. The price
// is that the operator's label is gone, which the run says, and that whatever the
// old unit held beyond what install reads back is not written again; the old file
// is kept, so nothing is lost.

// adoptedDir is where a unit that was taken over is kept, under the state
// directory: out of the service manager's search path, so it cannot be loaded
// again at the next login, and next to what it ran.
const adoptedDir = "adopted"

// errAlreadyAdopted says a --adopt names a unit that an earlier run already took
// over, so an updater that passes the same unit on every run does not fail on the
// second one.
var errAlreadyAdopted = errors.New("already adopted")

type svcAdopted struct {
	File   string // the unit's file
	Name   string // its launchd label, or its systemd unit name
	Text   string // what the file said
	Domain string // macOS: the domain launchd has it loaded in; empty when it is not loaded
	Active bool   // it is running now
}

var rePlistLabel = regexp.MustCompile(`<key>Label</key>\s*<string>([^<]*)</string>`)

func plistLabel(text string) string {
	if m := rePlistLabel.FindStringSubmatch(text); m != nil {
		return html.UnescapeString(m[1])
	}
	return ""
}

// findPlistByLabel looks through the directory for the property list whose Label
// is the one asked for: a label need not be the name of its file.
func findPlistByLabel(dir, label string) (file string, raw []byte, err error) {
	ents, derr := os.ReadDir(dir)
	if derr != nil {
		return "", nil, derr
	}
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".plist") {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		b, rerr := os.ReadFile(path)
		if rerr == nil && plistLabel(string(b)) == label {
			return path, b, nil
		}
	}
	return "", nil, os.ErrNotExist
}

// resolveAdopt finds the unit --adopt names and checks, before anything is
// changed, that taking it over is safe: it is a muster unit, it does not point the
// service at another state directory or table, and stopping it will not end the
// sessions it started.
func (e svcEnv) resolveAdopt(ctx context.Context, p svcPaths, arg string) (*svcAdopted, error) {
	suffix := ".service"
	if e.GOOS == "darwin" {
		suffix = ".plist"
	}
	byPath := strings.ContainsRune(arg, filepath.Separator)
	file := arg
	if !byPath {
		file = filepath.Join(filepath.Dir(p.Unit), arg)
		if !strings.HasSuffix(arg, suffix) {
			file += suffix
		}
	}
	file = filepath.Clean(file)
	guess := file
	raw, err := os.ReadFile(file)
	if err != nil && e.GOOS == "darwin" && !byPath {
		file, raw, err = findPlistByLabel(filepath.Dir(p.Unit), arg)
	}
	if err != nil {
		// An earlier run took it over and put it away: the standard unit is what
		// is there to be updated, and this run is an ordinary install.
		if kept := e.keptAdopted(p, arg, guess); kept != "" {
			if _, serr := os.Stat(p.Unit); serr == nil {
				return nil, fmt.Errorf("%w: %s was taken over earlier (its file is kept at %s)", errAlreadyAdopted, arg, kept)
			}
		}
		return nil, fmt.Errorf("--adopt %q: no such unit (looked for %s): pass the launchd label, the systemd unit name, or the path of its file", arg, guess)
	}

	a := &svcAdopted{File: file, Text: string(raw)}
	if e.GOOS == "darwin" {
		a.Name = plistLabel(a.Text)
		if a.Name == "" {
			a.Name = strings.TrimSuffix(filepath.Base(file), ".plist")
		}
	} else {
		a.Name = filepath.Base(file)
	}
	if file == p.Unit || file == p.LoginUnit || a.Name == svcLabel || a.Name == svcLoginLabel || a.Name == svcUnitName || sameFile(file, p.Unit) {
		return nil, fmt.Errorf("--adopt %q is the unit this command writes itself: run it without --adopt", arg)
	}
	vars, _ := parseUnitVars(e.GOOS, a.Text)
	if _, ok := vars["FLEET_ADDR"]; !ok {
		return nil, fmt.Errorf("--adopt %q sets no FLEET_ADDR, so it does not look like a muster unit: nothing was changed", arg)
	}
	// Install owns these two: it makes the key, the table and the token where the
	// layout says and reads them there. Taking over a unit that keeps its table or
	// its state somewhere else would start the service on a different table and
	// state than it has now, so it is refused rather than reset.
	for _, kv := range [][2]string{{"FLEET_CONFIG", p.Config}, {"FLEET_STATE_DIR", p.StateDir}} {
		if got, ok := vars[kv[0]]; ok && got != kv[1] {
			return nil, fmt.Errorf("--adopt %q sets %s=%s, and install keeps the service on its own layout (%s). Taking it over would start the "+
				"service on a different principal table and state than it has now, so nothing was changed: put that data at the layout's path "+
				"(or point the unit there) and run this again", arg, kv[0], got, kv[1])
		}
	}

	switch e.GOOS {
	case "darwin":
		for _, d := range []string{e.domain(), e.guiDomain()} {
			out, lerr := e.run(ctx, "launchctl", "print", d+"/"+a.Name)
			if lerr != nil {
				continue
			}
			a.Domain, a.Active = d, strings.Contains(string(out), "state = running")
			if a.Active && !abandonsProcessGroup(out) {
				return nil, fmt.Errorf("--adopt %q is running in %s as a job that does not leave its processes alone when it stops, so stopping it "+
					"would end every session on this machine. Nothing was changed. When that is acceptable, run `launchctl bootout %s/%s`, then run this again",
					arg, d, d, a.Name)
			}
			break
		}
	default:
		if _, aerr := e.run(ctx, "systemctl", "--user", "is-active", "--quiet", a.Name); aerr == nil {
			a.Active = true
			out, _ := e.run(ctx, "systemctl", "--user", "show", a.Name, "-p", "KillMode", "--value")
			if km := strings.TrimSpace(string(out)); km != "process" && km != "none" {
				return nil, fmt.Errorf("--adopt %q is running with KillMode=%s, so stopping it would end every session its service started. Nothing was changed. "+
					"When that is acceptable, run `systemctl --user stop %s`, then run this again", arg, km, a.Name)
			}
		}
	}
	return a, nil
}

// svcOtherUnit is a muster unit in the manager's directory that this command does
// not write: one an operator made.
type svcOtherUnit struct {
	File string // the unit's file
	Name string // its launchd label, or its systemd unit name: what --adopt takes
	Addr string // the FLEET_ADDR it sets
}

// otherMusterUnits lists the units in the service manager's directory that run
// muster and are not the ones this command writes (muster #493). The test is the one
// --adopt applies to the unit it names: the unit sets FLEET_ADDR. A unit already taken
// over is not here, because taking it over moved its file out of the directory.
func (e svcEnv) otherMusterUnits(p svcPaths) []svcOtherUnit {
	suffix := ".service"
	if e.GOOS == "darwin" {
		suffix = ".plist"
	}
	dir := filepath.Dir(p.Unit)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []svcOtherUnit
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), suffix) {
			continue
		}
		file := filepath.Join(dir, ent.Name())
		if file == p.Unit || file == p.LoginUnit || sameFile(file, p.Unit) {
			continue
		}
		raw, rerr := os.ReadFile(file)
		if rerr != nil {
			continue
		}
		u := svcOtherUnit{File: file, Name: ent.Name()}
		if e.GOOS == "darwin" {
			u.Name = firstNonEmpty(plistLabel(string(raw)), strings.TrimSuffix(ent.Name(), suffix))
		}
		if u.Name == svcLabel || u.Name == svcLoginLabel || u.Name == svcUnitName {
			continue
		}
		vars, _ := parseUnitVars(e.GOOS, string(raw))
		addr, ok := vars["FLEET_ADDR"]
		if !ok {
			continue
		}
		u.Addr = addr
		found = append(found, u)
	}
	return found
}

// errOtherMusterUnit is the refusal of a plain install beside a unit an operator
// made: the standard unit written next to it would be a second muster on the same
// state, which is the failure --adopt exists to prevent. It names each unit and the
// command that takes it over.
func errOtherMusterUnit(others []svcOtherUnit) error {
	var b strings.Builder
	if len(others) == 1 {
		b.WriteString("this machine already runs muster under a unit of its own, and writing the standard unit beside it would start a second muster on the same state:\n")
	} else {
		b.WriteString("this machine already runs muster under units of its own, and writing the standard unit beside them would start a second muster on the same state:\n")
	}
	for _, u := range others {
		fmt.Fprintf(&b, "  %s (%s, FLEET_ADDR=%s)\n", u.Name, u.File, u.Addr)
	}
	b.WriteString("Nothing was changed. To take it over and leave one unit, run:\n")
	for _, u := range others {
		fmt.Fprintf(&b, "  muster service install --adopt %s\n", adoptArg(u.Name))
	}
	if len(others) > 1 {
		b.WriteString("one unit at a time: run it again for the next once the first is taken over")
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

// adoptArg is the unit's name as a shell word: bare when it is made of characters
// a shell leaves alone, quoted otherwise.
func adoptArg(name string) string {
	if reShellSafe.MatchString(name) {
		return name
	}
	return shQuote(name)
}

var reShellSafe = regexp.MustCompile(`^[A-Za-z0-9._@:+=/-]+$`)

// keptAdopted finds the copy an earlier run kept of the unit arg names: by the
// file name it would have had, or, on macOS, by the label inside.
func (e svcEnv) keptAdopted(p svcPaths, arg, file string) string {
	kept, _ := filepath.Glob(filepath.Join(p.StateDir, adoptedDir, "*"))
	for _, k := range kept {
		if strings.HasPrefix(filepath.Base(k), filepath.Base(file)) {
			return k
		}
		if e.GOOS == "darwin" {
			if b, err := os.ReadFile(k); err == nil && plistLabel(string(b)) == arg {
				return k
			}
		}
	}
	return ""
}

// retireAdopted stops the unit that was taken over and puts its file away, so the
// manager has one muster unit left. It runs after the standard unit is written and
// doctor has passed, and before that unit is loaded: the address is free by then.
func (e svcEnv) retireAdopted(ctx context.Context, p svcPaths, a *svcAdopted, rep *svcReport) error {
	switch e.GOOS {
	case "darwin":
		if a.Domain != "" {
			e.bootout(ctx, a.Domain, a.Name)
			if e.loadedIn(ctx, a.Domain, a.Name) {
				return fmt.Errorf("launchctl bootout did not remove %s from %s: run `launchctl bootout %s/%s` and try again", a.Name, a.Domain, a.Domain, a.Name)
			}
		}
	default:
		_, _ = e.run(ctx, "systemctl", "--user", "disable", "--now", a.Name)
	}
	keep := filepath.Join(p.StateDir, adoptedDir, filepath.Base(a.File))
	if _, err := os.Stat(keep); err == nil {
		keep += "." + strconv.FormatInt(time.Now().Unix(), 10)
	}
	if err := writeFileAtomic(keep, []byte(a.Text), 0o600, 0o700); err != nil {
		return fmt.Errorf("keeping a copy of %s: %w", a.File, err)
	}
	if err := os.Remove(a.File); err != nil {
		return fmt.Errorf("removing %s: %w", a.File, err)
	}
	rep.line("adopted", true, a.Name+" stopped and replaced by "+filepath.Base(p.Unit)+"; its file is kept at "+keep)
	return nil
}
