package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// withLauncher names a launcher for the test and puts the previous one back. The
// launcher is process-wide, so a test that sets it must not run in parallel.
func withLauncher(t *testing.T, path string) {
	t.Helper()
	prev := Launcher()
	SetLauncher(path)
	t.Cleanup(func() { SetLauncher(prev) })
}

func TestWrapOnMacosIsExactlyTheLauncherTheDriversAlwaysRan(t *testing.T) {
	p := &Plan{Read: []string{"/usr", "/w"}, Write: []string{"/w"}}
	got, err := p.Wrap("/d/profile.sb", []string{"/bin/agent", "--flag"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ExecPath, "-f", "/d/profile.sb", "/bin/agent", "--flag"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrap = %q, want %q", got, want)
	}

	inline, err := p.WrapInline([]string{"/bin/agent", "serve"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := p.Profile()
	if err != nil {
		t.Fatal(err)
	}
	want = []string{ExecPath, "-p", profile, "/bin/agent", "serve"}
	if !reflect.DeepEqual(inline, want) {
		t.Fatalf("inline wrap = %q, want %q", inline, want)
	}
	if p.ProfileFileName() != "profile.sb" {
		t.Errorf("file name = %q", p.ProfileFileName())
	}
	// Policy on macOS is the profile text, unchanged.
	text, err := p.Policy()
	if err != nil || string(text) != profile {
		t.Errorf("policy is not the profile: %v", err)
	}
}

func TestWrapOnLinuxRunsTheServiceBinaryAsTheLauncher(t *testing.T) {
	withLauncher(t, "/opt/m/bin/muster")
	p := &Plan{Mechanism: MechanismLinux, Network: fleet.SandboxNetworkClosed, Read: []string{"/usr"}, Write: []string{"/w"}, BindTCP: []int{4100}}

	got, err := p.Wrap("/d/policy.json", []string{"/bin/agent", "a b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/m/bin/muster", LauncherArg, "--policy-file", "/d/policy.json", "--", "/bin/agent", "a b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrap = %q, want %q", got, want)
	}

	inline, err := p.WrapInline([]string{"/bin/agent", "serve"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inline) != 7 || inline[0] != "/opt/m/bin/muster" || inline[1] != LauncherArg || inline[2] != "--policy" || inline[4] != "--" || inline[5] != "/bin/agent" {
		t.Fatalf("inline wrap = %q", inline)
	}
	if p.ProfileFileName() != "policy.json" {
		t.Errorf("file name = %q", p.ProfileFileName())
	}
}

func TestWrapOnLinuxWithoutALauncherRefusesRatherThanRunUnconfined(t *testing.T) {
	withLauncher(t, "")
	p := &Plan{Mechanism: MechanismLinux}
	if _, err := p.Wrap("/d/policy.json", []string{"/bin/agent"}); err == nil {
		t.Error("Wrap returned an argv with no launcher in it")
	}
	if _, err := p.WrapInline([]string{"/bin/agent"}); err == nil {
		t.Error("WrapInline returned an argv with no launcher in it")
	}
}

func TestLinuxPolicyCarriesPathsAndAPostureAndNothingElse(t *testing.T) {
	p := &Plan{Mechanism: MechanismLinux, Network: fleet.SandboxNetworkClosed, Read: []string{"/usr", "/w"}, Write: []string{"/w"}, BindTCP: []int{4100}}
	text, err := p.Policy()
	if err != nil {
		t.Fatal(err)
	}
	var doc LinuxPolicy
	dec := json.NewDecoder(strings.NewReader(string(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("the policy does not decode into its own schema: %v\n%s", err, text)
	}
	if doc.Version != PolicyVersion || doc.Network != "closed" || !doc.OwnTTY {
		t.Errorf("policy = %+v", doc)
	}
	if !reflect.DeepEqual(doc.Read, []string{"/usr", "/w"}) || !reflect.DeepEqual(doc.Write, []string{"/w"}) || !reflect.DeepEqual(doc.BindTCP, []int{4100}) {
		t.Errorf("policy = %+v", doc)
	}
	if !reflect.DeepEqual(doc.Devices, LinuxDevices) {
		t.Errorf("devices = %v, want the driver-carried list", doc.Devices)
	}
	// The shared memory directory is the one device-looking path a session must
	// never be handed.
	for _, d := range doc.Devices {
		if strings.Contains(d, "shm") {
			t.Errorf("devices carry %s", d)
		}
	}
}

func TestLinuxPolicyDefaultsToAnOpenNetworkAndNeverNull(t *testing.T) {
	text, err := (&Plan{Mechanism: MechanismLinux}).Policy()
	if err != nil {
		t.Fatal(err)
	}
	s := string(text)
	for _, want := range []string{`"network":"open"`, `"read":[]`, `"write":[]`, `"bindTCP":[]`} {
		if !strings.Contains(s, want) {
			t.Errorf("policy lacks %s: %s", want, s)
		}
	}
}

func TestLinuxPolicyRefusesWhatTheLauncherWouldRefuse(t *testing.T) {
	cases := map[string]LinuxPolicy{
		"version":       {Version: 2, Network: "open"},
		"network":       {Version: 1, Network: "loopback"},
		"relative path": {Version: 1, Network: "open", Read: []string{"usr"}},
		"unclean path":  {Version: 1, Network: "open", Write: []string{"/a/../b"}},
		"root":          {Version: 1, Network: "open", Read: []string{"/"}},
		"bad port":      {Version: 1, Network: "closed", BindTCP: []int{0}},
		"bind on open":  {Version: 1, Network: "open", BindTCP: []int{4100}},
	}
	for name, d := range cases {
		if err := d.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
	if err := (LinuxPolicy{Version: 1, Network: "closed", Read: []string{"/usr"}, BindTCP: []int{4100}}).Validate(); err != nil {
		t.Errorf("a good policy was refused: %v", err)
	}
	if _, err := (&Plan{Mechanism: MechanismLinux, Read: []string{"relative"}}).Policy(); err == nil {
		t.Error("a plan with a relative path rendered a policy")
	}
}

func TestStateNamesThePlansMechanism(t *testing.T) {
	if got := (&Plan{}).State().Mechanism; got != Mechanism {
		t.Errorf("empty plan reports %q, want the macOS mechanism %q", got, Mechanism)
	}
	st := (&Plan{Mechanism: MechanismLinux, Read: []string{"/usr"}}).State()
	if st.Mechanism != MechanismLinux || len(st.Denies) != 3 {
		t.Errorf("state = %+v", st)
	}
}

func TestSystemReadRootsForLinuxGrantsTheSystemAndNothingOfTheUser(t *testing.T) {
	macos := SystemReadRootsFor("")
	if !reflect.DeepEqual(macos, SystemReadRoots) {
		t.Errorf("the macOS roots changed: %v", macos)
	}
	// A copy, so a caller cannot widen the package's list.
	macos[0] = "/changed"
	if SystemReadRoots[0] == "/changed" {
		t.Error("SystemReadRootsFor returned the package's own slice")
	}

	for _, r := range SystemReadRootsFor(MechanismLinux) {
		if _, err := os.Stat(r); err != nil {
			t.Errorf("%s is granted but does not exist", r)
		}
		for _, bad := range []string{"/opt", "/var", "/run", "/home", "/root", "/tmp", "/dev"} {
			if CoversOrIs(r, bad) {
				t.Errorf("%s would grant %s", r, bad)
			}
		}
	}
}

func TestPruneMissingEnsureDirsAndRequireExist(t *testing.T) {
	dir := t.TempDir()
	have := filepath.Join(dir, "have")
	if err := os.Mkdir(have, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")

	if got := PruneMissing([]string{have, missing}); !reflect.DeepEqual(got, []string{have}) {
		t.Errorf("PruneMissing = %v", got)
	}

	made := filepath.Join(dir, "a", "b")
	if err := EnsureDirs([]string{made}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(made)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory not made: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", info.Mode().Perm())
	}

	if err := RequireExist("sandbox.readPaths", []string{have}); err != nil {
		t.Errorf("an existing path was refused: %v", err)
	}
	err = RequireExist("sandbox.writePaths", []string{have, missing})
	if err == nil || !strings.Contains(err.Error(), "sandbox.writePaths") || !strings.Contains(err.Error(), missing) {
		t.Errorf("a missing path was not refused by field and name: %v", err)
	}
}
