package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// muster #492: a machine that serves federation peers runs muster under a unit an
// operator made (its own label, a listen address on a network interface, the
// operator's variables). `service install` has to be able to own that unit, and a
// desk that declares nothing still refuses a network address.

// fakeOtherUnit is the operator's unit as the service manager sees it: its own
// label, loaded in a domain, running or not. It answers only for its own name.
type fakeOtherUnit struct {
	name     string
	domain   string // launchd: user/501 or gui/501
	loaded   bool
	active   bool
	leaves   bool   // stopping it takes its sessions along
	killMode string // systemd
}

func (u *fakeOtherUnit) handle(line string) (out []byte, err error, ok bool) {
	if !strings.Contains(line, u.name) {
		return nil, nil, false
	}
	switch {
	case strings.HasPrefix(line, "launchctl print "):
		if !u.loaded || line != "launchctl print "+u.domain+"/"+u.name {
			return nil, errors.New("not found"), true
		}
		props := "properties = runatload | abandon process group | inferred program"
		if u.leaves {
			props = "properties = runatload | inferred program"
		}
		state := "state = waiting"
		if u.active {
			state = "state = running"
		}
		return []byte(u.name + " = {\n\t" + state + "\n\t" + props + "\n}\n"), nil, true
	case line == "launchctl bootout "+u.domain+"/"+u.name:
		u.loaded, u.active = false, false
		return nil, nil, true
	case strings.Contains(line, "is-active"):
		if !u.active {
			return nil, errors.New("inactive"), true
		}
		return nil, nil, true
	case strings.Contains(line, " show "):
		return []byte(u.killMode + "\n"), nil, true
	case strings.Contains(line, "disable --now"):
		u.loaded, u.active = false, false
		return nil, nil, true
	}
	return nil, nil, false
}

const hubLabel = "org.example.hub-muster"

// the operator's unit: a label of its own, a binary somewhere else, the address on a
// LAN interface and the variables that make this machine what it is.
func (f *svcFixture) writeHubUnit(t *testing.T, addr string, extra ...operatorVar) (file string) {
	t.Helper()
	vars := []operatorVar{
		{"FLEET_MACHINE", "hub"},
		{"FLEET_ADDR", addr},
		{"FLEET_STATE_DIR", f.p.StateDir},
		{"FLEET_CONFIG", f.p.Config},
		{"FLEET_TMUX_BIN", f.tmux},
		{"FLEET_RUNTIME_OPENCODE", "1"},
		{"FLEET_OPENCODE_BIN", "/opt/opencode/bin/opencode"},
		{"FLEET_INBOX_INDEX", "/var/lib/hub/inbox-index.json"},
		{"FLEET_DELIVERY_MODULES", "/opt/hub/modules"},
		{"LANG", "ja_JP.UTF-8"},
		{"ZZ_NOT_CARRIED", "left behind"},
	}
	vars = append(vars, extra...)
	var b strings.Builder
	switch f.env.GOOS {
	case "darwin":
		file = filepath.Join(filepath.Dir(f.p.Unit), "hub-muster.plist")
		b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\">\n<dict>\n")
		b.WriteString("\t<key>Label</key>\n\t<string>" + hubLabel + "</string>\n")
		b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>/opt/hub/bin/muster</string>\n\t\t<string>serve</string>\n\t</array>\n")
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, v := range vars {
			b.WriteString("\t\t<key>" + v.name + "</key>\n\t\t<string>" + xmlEscape(v.value) + "</string>\n")
		}
		b.WriteString("\t</dict>\n\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<true/>\n</dict>\n</plist>\n")
	default:
		file = filepath.Join(filepath.Dir(f.p.Unit), "muster-hub.service")
		b.WriteString("[Unit]\nDescription=hub\n\n[Service]\nExecStart=\"/opt/hub/bin/muster\" serve\n")
		for _, v := range vars {
			b.WriteString(`Environment="` + v.name + "=" + v.value + `"` + "\n")
		}
		b.WriteString("KillMode=process\n\n[Install]\nWantedBy=default.target\n")
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mgr.other = &fakeOtherUnit{name: hubLabel, domain: "user/501", loaded: true, active: true, killMode: "process"}
	if f.env.GOOS == "linux" {
		f.mgr.other.name = "muster-hub.service"
	}
	return file
}

func (f *svcFixture) hubName() string { return f.mgr.other.name }

// the address a federation hub would have: not on this machine, so only its
// loopback twin can answer, which is what the service itself relies on.
func (f *svcFixture) hubAddr() string {
	_, port, _ := strings.Cut(f.addr(), ":")
	return "10.8.0.12:" + port
}

func TestAdoptTakesOverAFederationHubUnit(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			old := f.writeHubUnit(t, f.hubAddr())
			if code := f.run(t, "install", "--tmux", f.tmux, "--adopt", f.hubName()); code != 0 {
				t.Fatalf("install --adopt: %d\n%s%s", code, f.out, f.errb)
			}
			// One unit: the standard one, with the operator's address and every variable
			// the operator set that install keeps.
			unit := readUnitText(t, f.p.Unit)
			for _, want := range []string{f.hubAddr(), "hub", "FLEET_RUNTIME_OPENCODE", "/opt/opencode/bin/opencode", "FLEET_INBOX_INDEX",
				"/var/lib/hub/inbox-index.json", "FLEET_DELIVERY_MODULES", "/opt/hub/modules", "ja_JP.UTF-8"} {
				if !strings.Contains(unit, want) {
					t.Errorf("the unit lost %q:\n%s", want, unit)
				}
			}
			if strings.Contains(unit, "ZZ_NOT_CARRIED") {
				t.Errorf("a variable install does not carry was copied:\n%s", unit)
			}
			if !strings.Contains(unit, f.p.Bin) {
				t.Errorf("the unit does not run the installed binary %s:\n%s", f.p.Bin, unit)
			}
			// The operator's unit is stopped, gone from where the manager looks, and kept.
			if _, err := os.Stat(old); err == nil {
				t.Errorf("%s is still in the manager's directory: the next login would load a second muster", old)
			}
			kept, _ := filepath.Glob(filepath.Join(f.p.StateDir, adoptedDir, filepath.Base(old)+"*"))
			if len(kept) != 1 {
				t.Errorf("the old unit is not kept in the state directory: %v", kept)
			}
			if f.mgr.other.loaded || f.mgr.other.active {
				t.Errorf("the operator's unit is still loaded: %+v", f.mgr.other)
			}
			if !f.mgr.loaded || !f.mgr.active {
				t.Errorf("the standard unit is not running: loaded=%v active=%v", f.mgr.loaded, f.mgr.active)
			}
			// It was stopped before the standard unit was loaded: the address is free.
			seq := f.seq()
			stop, start := "launchctl bootout user/501/"+hubLabel, "launchctl bootstrap user/501 "+f.p.Unit
			if goos == "linux" {
				stop, start = "systemctl --user disable --now muster-hub.service", "systemctl --user enable --now "+svcUnitName
			}
			if i, j := strings.Index(seq, stop), strings.Index(seq, start); i < 0 || j < 0 || i > j {
				t.Errorf("the old unit must stop before the standard one starts (%q before %q):\n%s", stop, start, seq)
			}
			// Health was asked where the unit listens: the loopback twin of its address.
			out := f.out.String()
			for _, want := range []string{"adopted", "listen    serves peers on " + f.hubAddr(), "health    ok         http://" + f.addr() + "/v1/health"} {
				if !strings.Contains(out, want) {
					t.Errorf("install did not say %q:\n%s", want, out)
				}
			}
			// Only one muster unit file is left beside the login item.
			ents, _ := os.ReadDir(filepath.Dir(f.p.Unit))
			for _, ent := range ents {
				if ent.Name() != filepath.Base(f.p.Unit) && ent.Name() != filepath.Base(f.p.LoginUnit) {
					t.Errorf("a second unit file is left: %s", ent.Name())
				}
			}

			// The next update is an ordinary install with no flags: same address, same
			// variables, nothing to change, and a --adopt left in an updater's command
			// is not an error.
			f.mgr.reset()
			if err := os.WriteFile(f.exe, []byte("binary v2"), 0o755); err != nil {
				t.Fatal(err)
			}
			if code := f.run(t, "install", "--tmux", f.tmux); code != 0 {
				t.Fatalf("update: %d\n%s%s", code, f.out, f.errb)
			}
			if after := readUnitText(t, f.p.Unit); after != unit {
				t.Errorf("an update changed the unit:\n%s\n--- was\n%s", after, unit)
			}
			if !strings.Contains(f.out.String(), "health    ok") {
				t.Errorf("the update's health check did not pass:\n%s", f.out)
			}
			if code := f.run(t, "install", "--tmux", f.tmux, "--adopt", f.hubName()); code != 0 {
				t.Fatalf("a repeated --adopt: %d\n%s%s", code, f.out, f.errb)
			}
			if !strings.Contains(f.out.String(), "taken over earlier") {
				t.Errorf("a repeated --adopt did not say it was done:\n%s", f.out)
			}
			if again := readUnitText(t, f.p.Unit); again != unit {
				t.Errorf("a repeated --adopt changed the unit")
			}
		})
	}
}

func TestAdoptFindsTheUnitByLabelOrPath(t *testing.T) {
	for _, by := range []string{"label", "path", "file name"} {
		t.Run(by, func(t *testing.T) {
			f := newSvcFixture(t, "darwin")
			old := f.writeHubUnit(t, f.hubAddr())
			arg := map[string]string{"label": hubLabel, "path": old, "file name": filepath.Base(old)}[by]
			if code := f.run(t, "install", "--tmux", f.tmux, "--adopt", arg); code != 0 {
				t.Fatalf("install --adopt %s: %d\n%s%s", arg, code, f.out, f.errb)
			}
			if !strings.Contains(readUnitText(t, f.p.Unit), f.hubAddr()) {
				t.Errorf("the address was not taken over")
			}
		})
	}
}

func TestAdoptTakesAUnitLoadedInTheGuiDomain(t *testing.T) {
	f := newSvcFixture(t, "darwin")
	f.writeHubUnit(t, f.hubAddr())
	f.mgr.other.domain = "gui/501"
	if code := f.run(t, "install", "--tmux", f.tmux, "--adopt", hubLabel); code != 0 {
		t.Fatalf("install --adopt: %d\n%s%s", code, f.out, f.errb)
	}
	if !strings.Contains(f.seq(), "launchctl bootout gui/501/"+hubLabel) || f.mgr.other.loaded {
		t.Errorf("the unit loaded in gui/501 was not booted out:\n%s", f.seq())
	}
}

func TestAdoptRefusesBeforeChangingAnything(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, tc := range []struct {
			name  string
			setup func(*testing.T, *svcFixture)
			args  []string
			want  string
		}{
			{"no such unit", func(t *testing.T, f *svcFixture) {}, []string{"--adopt", "nothing.here"}, "no such unit"},
			{"the standard unit itself", func(t *testing.T, f *svcFixture) {
				if code := f.install(t); code != 0 {
					t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
				}
			}, []string{"--adopt", svcLabel}, "writes itself"},
			{"not a muster unit", func(t *testing.T, f *svcFixture) {
				f.writeHubUnit(t, f.hubAddr())
				b, _ := os.ReadFile(hubFile(f))
				if err := os.WriteFile(hubFile(f), []byte(strings.ReplaceAll(string(b), "FLEET_ADDR", "FLEET_ADDX")), 0o644); err != nil {
					t.Fatal(err)
				}
			}, []string{"--adopt", "hub"}, "does not look like a muster unit"},
			{"another state directory", func(t *testing.T, f *svcFixture) {
				// the later of two settings wins, in a plist as in a unit
				f.writeHubUnit(t, f.hubAddr(), operatorVar{"FLEET_STATE_DIR", "/elsewhere/state"})
			}, []string{"--adopt", "hub"}, "FLEET_STATE_DIR=/elsewhere/state"},
			{"it would end the sessions", func(t *testing.T, f *svcFixture) {
				f.writeHubUnit(t, f.hubAddr())
				f.mgr.other.leaves, f.mgr.other.killMode = true, "control-group"
			}, []string{"--adopt", "hub"}, "would end every session"},
			{"--no-start", func(t *testing.T, f *svcFixture) { f.writeHubUnit(t, f.hubAddr()) }, []string{"--adopt", "hub", "--no-start"}, "--no-start"},
		} {
			t.Run(goos+"/"+tc.name, func(t *testing.T) {
				f := newSvcFixture(t, goos)
				tc.setup(t, f)
				arg := tc.args
				for i, a := range arg {
					if a == "hub" {
						arg[i] = map[string]string{"linux": "muster-hub.service", "darwin": hubLabel}[goos]
					}
					if a == svcLabel && goos == "linux" {
						arg[i] = svcUnitName
					}
				}
				f.mgr.reset()
				code := f.run(t, append([]string{"install", "--tmux", f.tmux}, arg...)...)
				if code != 1 || !strings.Contains(f.errb.String(), tc.want) {
					t.Fatalf("exit %d, stderr %q, want %q", code, f.errb, tc.want)
				}
				if f.mgr.other != nil && (!f.mgr.other.loaded || !f.mgr.other.active) {
					t.Errorf("a refused --adopt stopped the operator's unit")
				}
				if h := hubFile(f); h != "" {
					if _, err := os.Stat(h); err != nil && tc.name != "no such unit" {
						t.Errorf("a refused --adopt moved the operator's unit: %v", err)
					}
				}
				for _, c := range f.mgr.sequence() {
					if strings.Contains(c, "bootout") || strings.Contains(c, "disable") || strings.Contains(c, "bootstrap") || strings.Contains(c, "enable") {
						t.Errorf("a refused --adopt still ran %q", c)
					}
				}
			})
		}
	}
}

func hubFile(f *svcFixture) string {
	if f.mgr.other == nil {
		return ""
	}
	name := "hub-muster.plist"
	if f.env.GOOS == "linux" {
		name = "muster-hub.service"
	}
	return filepath.Join(filepath.Dir(f.p.Unit), name)
}

// muster #493: a plain install beside a unit an operator made does not write the
// standard unit next to it. It says which unit it found and the command that takes
// it over, and changes nothing.
func TestPlainInstallBesideAnOperatorUnitNamesAdopt(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, standardThere := range []bool{false, true} {
			name := goos + "/no standard unit"
			if standardThere {
				name = goos + "/standard unit already beside it"
			}
			t.Run(name, func(t *testing.T) {
				f := newSvcFixture(t, goos)
				if standardThere {
					// the second muster an earlier update made, then disabled
					if code := f.install(t); code != 0 {
						t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
					}
					f.mgr.loaded, f.mgr.active = false, false
				}
				old := f.writeHubUnit(t, f.hubAddr())
				before := snapshotTree(t, f.env.Home)
				f.mgr.reset()

				code := f.run(t, "install", "--tmux", f.tmux)
				if code != 1 {
					t.Fatalf("a plain install beside %s exited %d, want 1\n%s%s", f.hubName(), code, f.out, f.errb)
				}
				errText := f.errb.String()
				for _, want := range []string{f.hubName(), old, f.hubAddr(), "muster service install --adopt " + f.hubName(), "Nothing was changed"} {
					if !strings.Contains(errText, want) {
						t.Errorf("the refusal does not say %q:\n%s", want, errText)
					}
				}
				if f.out.Len() != 0 {
					t.Errorf("install reported work before refusing:\n%s", f.out)
				}
				// Nothing was written or run: the home is as it was, and the service
				// manager was not asked to change anything.
				after := snapshotTree(t, f.env.Home)
				for path, was := range before {
					if got, ok := after[path]; !ok || got != was {
						t.Errorf("%s changed or went away", path)
					}
				}
				for path := range after {
					if _, ok := before[path]; !ok {
						t.Errorf("%s was written", path)
					}
				}
				if !f.mgr.other.loaded || !f.mgr.other.active {
					t.Errorf("the operator's unit was stopped: %+v", f.mgr.other)
				}
				for _, c := range f.mgr.sequence() {
					for _, verb := range []string{"bootout", "bootstrap", "enable", "disable", "kickstart", "restart", "start", "stop", "load"} {
						if strings.Contains(c, " "+verb+" ") || strings.HasSuffix(c, " "+verb) {
							t.Errorf("a refused install still ran %q", c)
						}
					}
				}

				// Running the command it names is the way out.
				if code := f.run(t, "install", "--tmux", f.tmux, "--adopt", f.hubName()); code != 0 {
					t.Fatalf("install --adopt after the refusal: %d\n%s%s", code, f.out, f.errb)
				}
				if !strings.Contains(readUnitText(t, f.p.Unit), f.hubAddr()) {
					t.Errorf("the adopted unit's address was not taken over")
				}
			})
		}
	}
}

// snapshotTree is every file under root with what it holds, and every directory.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			got[path] = "<dir>"
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		got[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPlainInstallIgnoresUnitsThatAreNotMuster(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			dir := filepath.Dir(f.p.Unit)
			if err := os.MkdirAll(filepath.Join(dir, "muster.service.d"), 0o755); err != nil {
				t.Fatal(err)
			}
			// A unit that sets no FLEET_ADDR is not a muster; a file the manager
			// does not read as a unit is not one either.
			suffix, body := ".service", "[Service]\nExecStart=/usr/bin/true\nEnvironment=\"FLEET_ADDX=1\"\n"
			if goos == "darwin" {
				suffix = ".plist"
				body = "<plist><dict><key>Label</key><string>org.example.other</string></dict></plist>\n"
			}
			for name, text := range map[string]string{
				"other" + suffix:               body,
				"notes.txt":                    "FLEET_ADDR=10.0.0.1:9470\n",
				"muster-hub" + suffix + ".bak": "FLEET_ADDR=10.0.0.1:9470\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if code := f.install(t); code != 0 {
				t.Fatalf("install beside units that are not muster: %d\n%s%s", code, f.out, f.errb)
			}
			if _, err := os.Stat(f.p.Unit); err != nil {
				t.Errorf("the standard unit was not written: %v", err)
			}
		})
	}
}

func TestPlainInstallNamesEveryOperatorUnit(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			f.writeHubUnit(t, f.hubAddr())
			dir := filepath.Dir(f.p.Unit)
			second := "muster-edge.service"
			text := "[Service]\nExecStart=/opt/edge/muster serve\nEnvironment=\"FLEET_ADDR=10.8.0.13:9470\"\n"
			wantName := second
			if goos == "darwin" {
				second = "edge-muster.plist"
				wantName = "org.example.edge muster"
				text = "<plist><dict><key>Label</key><string>org.example.edge muster</string><key>EnvironmentVariables</key><dict>" +
					"<key>FLEET_ADDR</key><string>10.8.0.13:9470</string></dict></dict></plist>\n"
			}
			if err := os.WriteFile(filepath.Join(dir, second), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			if code := f.run(t, "install", "--tmux", f.tmux); code != 1 {
				t.Fatalf("exit %d, want 1\n%s%s", code, f.out, f.errb)
			}
			errText := f.errb.String()
			for _, want := range []string{
				"--adopt " + f.hubName(),
				// a name a shell would split is quoted in the command it suggests
				"--adopt " + adoptArg(wantName),
				"10.8.0.13:9470",
			} {
				if !strings.Contains(errText, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, errText)
				}
			}
			if goos == "darwin" && !strings.Contains(errText, "--adopt 'org.example.edge muster'") {
				t.Errorf("a label with a space is not quoted:\n%s", errText)
			}
		})
	}
}

func TestAdoptArg(t *testing.T) {
	for in, want := range map[string]string{
		"org.example.hub-muster": "org.example.hub-muster",
		"muster-hub.service":     "muster-hub.service",
		"a b":                    "'a b'",
		"it's":                   `'it'\''s'`,
		"$(x)":                   "'$(x)'",
	} {
		if got := adoptArg(in); got != want {
			t.Errorf("adoptArg(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAdoptKeepsTheStandardUnitsHealthAnswerable(t *testing.T) {
	// A standard unit already sits beside the operator's (the second muster an update
	// once made, then disabled): the adopted settings are written over it.
	f := newSvcFixture(t, "linux")
	if code := f.install(t); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	f.mgr.loaded, f.mgr.active = false, false
	f.writeHubUnit(t, f.hubAddr())
	if code := f.run(t, "install", "--tmux", f.tmux, "--adopt", f.hubName()); code != 0 {
		t.Fatalf("install --adopt: %d\n%s%s", code, f.out, f.errb)
	}
	if u := readUnitText(t, f.p.Unit); !strings.Contains(u, f.hubAddr()) || !strings.Contains(u, "FLEET_RUNTIME_OPENCODE") {
		t.Errorf("the adopted settings did not replace the standard unit's:\n%s", u)
	}
	if !strings.Contains(f.out.String(), "exists too") {
		t.Errorf("install did not say it replaced an existing standard unit:\n%s", f.out)
	}
}

func TestNonLoopbackAddressNeedsTheDeclarationOnADesk(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSvcFixture(t, goos)
			lan := f.hubAddr()
			// A desk with no declaration: refused, and nothing is written.
			if code := f.run(t, "install", "--tmux", f.tmux, "--addr", lan); code != 1 || !strings.Contains(f.errb.String(), "--serves-peers") {
				t.Fatalf("exit %d, stderr %q", code, f.errb)
			}
			if _, err := os.Stat(f.p.Unit); err == nil {
				t.Fatal("a refused install wrote a unit")
			}
			// Declared: written as given; the health check is asked on its loopback twin.
			if code := f.run(t, "install", "--tmux", f.tmux, "--addr", lan, "--serves-peers"); code != 0 {
				t.Fatalf("declared: %d\n%s%s", code, f.out, f.errb)
			}
			if !strings.Contains(readUnitText(t, f.p.Unit), lan) || !strings.Contains(f.out.String(), "health    ok         http://"+f.addr()+"/v1/health") {
				t.Errorf("the declared address was not kept or probed on loopback:\n%s", f.out)
			}
			// The declaration lives in the unit: an update with no flags keeps the address.
			if code := f.run(t, "install", "--tmux", f.tmux); code != 0 || !strings.Contains(f.out.String(), "Nothing to change") {
				t.Fatalf("rerun: %d\n%s%s", code, f.out, f.errb)
			}
			// ... and so does naming the same address again; a new one is a new exposure.
			if code := f.run(t, "install", "--tmux", f.tmux, "--addr", lan); code != 0 {
				t.Fatalf("the same address again: %d\n%s%s", code, f.out, f.errb)
			}
			other := strings.Replace(lan, "10.8.0.12", "10.9.0.12", 1)
			if code := f.run(t, "install", "--tmux", f.tmux, "--addr", lan+","+other); code != 1 || !strings.Contains(f.errb.String(), other) {
				t.Fatalf("a new routable entry without the declaration: %d, %q", code, f.errb)
			}
			if code := f.run(t, "install", "--tmux", f.tmux, "--addr", lan+","+other, "--serves-peers"); code != 0 {
				t.Fatalf("two addresses, declared: %d\n%s%s", code, f.out, f.errb)
			}
			// Back to loopback needs no declaration.
			if code := f.run(t, "install", "--tmux", f.tmux, "--addr", f.addr()); code != 0 {
				t.Fatalf("back to loopback: %d\n%s%s", code, f.out, f.errb)
			}
			if code := f.run(t, "status"); code != 0 || !strings.Contains(f.out.String(), "loopback only") {
				t.Errorf("status %d:\n%s", code, f.out)
			}
		})
	}
}

func TestServesPeersTakesASpecificAddressOnly(t *testing.T) {
	f := newSvcFixture(t, "linux")
	for _, tc := range []struct{ addr, want string }{
		{"0.0.0.0:9470", "one specific interface address"},
		{"[::]:9470", "one specific interface address"},
		{"hub.example:9470", "one specific interface address"},
		{"10.8.0.12:0", "fixed port"},
		{"10.8.0.12", "HOST:PORT"},
	} {
		if code := f.run(t, "install", "--tmux", f.tmux, "--addr", tc.addr, "--serves-peers"); code != 1 || !strings.Contains(f.errb.String(), tc.want) {
			t.Errorf("--addr %s --serves-peers: exit %d, stderr %q, want %q", tc.addr, code, f.errb, tc.want)
		}
	}
}

func TestStatusSaysWhereAServingMachineListens(t *testing.T) {
	f := newSvcFixture(t, "linux")
	if code := f.run(t, "install", "--tmux", f.tmux, "--addr", f.hubAddr(), "--serves-peers"); code != 0 {
		t.Fatalf("install: %d\n%s%s", code, f.out, f.errb)
	}
	if code := f.run(t, "status"); code != 0 {
		t.Fatalf("status %d:\n%s", code, f.out)
	}
	for _, want := range []string{"listen    ok       serves peers on " + f.hubAddr(), "health    ok       http://" + f.addr() + "/v1/health"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("status did not say %q:\n%s", want, f.out)
		}
	}
	// Restore asks health the same way.
	if code := f.run(t, "restore"); code != 0 || !strings.Contains(f.out.String(), "http://"+f.addr()+"/v1/health") {
		t.Errorf("restore %d:\n%s", code, f.out)
	}
}

func TestHealthAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:9470", "127.0.0.1:9470"},
		{"10.8.0.12:8775", "127.0.0.1:8775"},
		{"10.8.0.12:8775,10.9.0.12:8775", "127.0.0.1:8775"},
		{"10.8.0.12:8775,127.0.0.1:9000", "127.0.0.1:9000"},
		{"0.0.0.0:8775", "127.0.0.1:8775"},
		{"[::1]:9470", "[::1]:9470"},
		{"nonsense", "nonsense"},
		{"", ""},
	} {
		if got := healthAddr(tc.in); got != tc.want {
			t.Errorf("healthAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCheckListenAddr(t *testing.T) {
	known := map[string]bool{"10.8.0.12:8775": true}
	for _, tc := range []struct {
		addr   string
		peers  bool
		wantOK bool
	}{
		{"127.0.0.1:9470", false, true},
		{"127.0.0.1:9470,[::1]:9470", false, true},
		{"10.8.0.12:8775", false, true}, // the unit already has it
		{"10.8.0.12:8775,127.0.0.1:8775", false, true},
		{"10.9.0.12:8775", false, false},
		{"10.9.0.12:8775", true, true},
		{"10.8.0.12:8775,10.9.0.12:8775", false, false},
		{"0.0.0.0:8775", true, false},
		{"localhost:9470", false, false},
		{"", false, false},
	} {
		err := checkListenAddr(tc.addr, known, tc.peers)
		if (err == nil) != tc.wantOK {
			t.Errorf("checkListenAddr(%q, peers=%v) = %v, want ok=%v", tc.addr, tc.peers, err, tc.wantOK)
		}
	}
}
