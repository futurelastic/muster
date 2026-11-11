package tmux

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
)

// muster #439: the Linux mechanism through this driver, against the same fakes
// the macOS tests use, so it runs on every platform. What is asserted is what the
// driver would run and what it would write; that the kernel then refuses what the
// policy says is the launcher's tests (internal/sandbox) and the real-host test.

const testLauncher = "/opt/m/bin/muster"

func linuxSupport() *fleet.SandboxSupport {
	return &fleet.SandboxSupport{
		Mechanism: sandbox.MechanismLinux,
		Denies:    sandbox.Denies(),
		Network:   []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
	}
}

// linuxEnv is a sandbox rig whose host measures as the Linux mechanism, with a
// launcher named. The launcher is process-wide, so these tests do not run in
// parallel.
func linuxEnv(t *testing.T) *sandboxEnv {
	t.Helper()
	prev := sandbox.Launcher()
	sandbox.SetLauncher(testLauncher)
	t.Cleanup(func() { sandbox.SetLauncher(prev) })
	e := newSandboxEnv(t)
	e.hostSup = linuxSupport()
	return e
}

func readPolicy(t *testing.T, path string) sandbox.LinuxPolicy {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p sandbox.LinuxPolicy
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("%s does not decode into the policy schema: %v\n%s", path, err, b)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("the policy the driver wrote is one the launcher would refuse: %v", err)
	}
	return p
}

func TestLinuxSandboxedCreateRunsTheAgentThroughTheServicesLauncher(t *testing.T) {
	e := linuxEnv(t)
	if caps := e.d.Capabilities(); caps.Sandbox == nil || caps.Sandbox.Mechanism != sandbox.MechanismLinux {
		t.Fatalf("a host measured as the Linux mechanism does not declare it: %+v / %q", caps.Sandbox, caps.SandboxUnavailable)
	}
	if _, err := e.create("k1", "gamma", &fleet.SandboxSpec{}); err != nil {
		t.Fatal(err)
	}
	argv := launchArgv(t, e.f)
	envAt := -1
	for i, a := range argv {
		if a == "/usr/bin/env" {
			envAt = i
		}
	}
	if envAt < 0 {
		t.Fatalf("argv = %v: no confining wrapper", argv)
	}
	layout := newSandboxLayout(e.root, "gamma")
	want := []string{
		"/usr/bin/env", "TMPDIR=" + layout.tmp, "CLAUDE_CODE_TMPDIR=" + layout.tmp,
		testLauncher, sandbox.LauncherArg, "--policy-file", filepath.Join(layout.dir, "policy.json"), "--", testAgentPath,
	}
	if got := argv[envAt:]; !reflect.DeepEqual(got[:len(want)], want) {
		t.Errorf("wrapper = %v, want it to begin %v", got[:len(want)], want)
	}
	if !containsSeq(argv[envAt:], "--remote-control", "gamma") {
		t.Errorf("the agent's flags were lost: %v", argv)
	}
	// Nothing of macOS's facility is in a Linux argv.
	for _, a := range argv {
		if a == sandbox.ExecPath {
			t.Errorf("a Linux launch names the macOS launcher: %v", argv)
		}
	}
}

func TestLinuxSandboxedCreateWritesAPolicyTheSessionCannotReachAndTheLauncherAccepts(t *testing.T) {
	e := linuxEnv(t)
	if _, err := e.create("k1", "gamma", &fleet.SandboxSpec{Network: fleet.SandboxNetworkClosed}); err != nil {
		t.Fatal(err)
	}
	layout := newSandboxLayout(e.root, "gamma")
	file := filepath.Join(layout.dir, "policy.json")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("policy mode = %v, want 0600", info.Mode().Perm())
	}
	// Beside the session's private directory, never inside the part it may write.
	if sandbox.CoversOrIs(layout.work, file) {
		t.Errorf("the policy %s is inside the directory the session may write (%s)", file, layout.work)
	}
	if _, err := os.Stat(layout.profile); err == nil {
		t.Error("a Linux create also wrote a macOS profile")
	}

	p := readPolicy(t, file)
	if p.Network != "closed" || len(p.BindTCP) != 0 || !p.OwnTTY {
		t.Errorf("policy = %+v", p)
	}
	if !reflect.DeepEqual(p.Devices, sandbox.LinuxDevices) {
		t.Errorf("devices = %v, want the driver-carried list", p.Devices)
	}
	transcripts := filepath.Join(e.cfg, agentProjectsDir, recordDirFor(e.cwd))
	for _, w := range []string{e.cwd, layout.work, transcripts} {
		if !contains(p.Write, w) {
			t.Errorf("the policy does not grant %s for writing: %v", w, p.Write)
		}
	}
	// The directory the agent writes its transcripts to exists before it starts,
	// because a Linux rule is attached to a path that is there.
	if info, err := os.Stat(transcripts); err != nil || !info.IsDir() {
		t.Errorf("the transcript directory was not made before the launch: %v", err)
	}
	// And the policy names nothing that is not there: a grant that did not take
	// would be a weaker profile than the report says.
	for _, list := range [][]string{p.Read, p.Write} {
		for _, path := range list {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the policy names %s, which does not exist: %v", path, err)
			}
		}
	}
	// The report is the plan, and says which mechanism it is.
	st := e.d.sandboxStateFor("gamma")
	if st == nil || st.Mechanism != sandbox.MechanismLinux || st.Network != fleet.SandboxNetworkClosed {
		t.Fatalf("state = %+v", st)
	}
	var rec sandboxRecord
	b, err := os.ReadFile(layout.report)
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.State == nil || rec.State.Mechanism != sandbox.MechanismLinux {
		t.Errorf("the report on disk does not say the mechanism: %s %v", b, err)
	}
}

func TestLinuxSandboxMeasurementUsesTheLauncherAndMakesNoTranscriptDirectory(t *testing.T) {
	e := linuxEnv(t)
	if e.d.Capabilities().Sandbox == nil {
		t.Fatal("setup: not declared")
	}
	args := e.enterArg
	// muster <cwd> env TMPDIR=… CLAUDE_CODE_TMPDIR=… <launcher> __sandbox-exec --policy-file <file> -- <agent> auth status
	if len(args) < 12 || args[0] != "muster" || args[2] != "/usr/bin/env" || args[5] != testLauncher || args[6] != sandbox.LauncherArg || args[7] != "--policy-file" || args[9] != "--" {
		t.Fatalf("probe argv = %v", args)
	}
	if got := args[len(args)-3:]; !reflect.DeepEqual(got, []string{testAgentPath, "auth", "status"}) {
		t.Errorf("probe tail = %v", got)
	}
	// A probe is not a session: it must not leave a directory in the user's
	// transcript tree for a path that was only ever a probe's.
	if entries, err := os.ReadDir(filepath.Join(e.cfg, agentProjectsDir)); err == nil && len(entries) != 0 {
		t.Errorf("a measurement left %d entries in the transcript tree", len(entries))
	}
}

func TestLinuxSandboxedCreateRefusesRatherThanRunUnconfinedWhenThereIsNoLauncher(t *testing.T) {
	e := linuxEnv(t)
	if e.d.Capabilities().Sandbox == nil {
		t.Fatal("setup: not declared")
	}
	sandbox.SetLauncher("") // restored by linuxEnv's cleanup
	_, err := e.create("k1", "gamma", &fleet.SandboxSpec{})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || !strings.Contains(fe.Message, "launcher") {
		t.Fatalf("Create = %v, want an unsupported refusal naming the launcher", err)
	}
	if argv := agentArgv(newSessionArgv(e.f)); argv != nil {
		t.Errorf("a session was started anyway: %v", argv)
	}
	if st := e.d.sandboxStateFor("gamma"); st != nil {
		t.Errorf("a refused create left a sandbox state: %+v", st)
	}
	if _, err := os.Stat(newSandboxLayout(e.root, "gamma").dir); err == nil {
		t.Error("a refused create left its private directory")
	}
}

func TestLinuxSandboxedCreateRefusesAGrantThatDoesNotExist(t *testing.T) {
	e := linuxEnv(t)
	missing := filepath.Join(e.home, "git-common-that-is-not-there")
	for _, sb := range []fleet.SandboxSpec{
		{ReadPaths: []fleet.AbsolutePath{fleet.AbsolutePath(missing)}},
		{WritePaths: []fleet.AbsolutePath{fleet.AbsolutePath(missing)}},
	} {
		_, err := e.create("k-"+filepath.Base(missing)+itoa(len(sb.ReadPaths)), "gamma", &sb)
		var fe *fleet.Error
		if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid || !strings.Contains(fe.Message, "does not exist") || !strings.Contains(fe.Message, missing) {
			t.Fatalf("Create = %v, want an invalid refusal naming the missing grant", err)
		}
	}
	if argv := agentArgv(newSessionArgv(e.f)); argv != nil {
		t.Errorf("a session was started anyway: %v", argv)
	}
	// The same request on macOS is not refused: a profile may name a path that is
	// not there yet.
	m := newSandboxEnv(t)
	if _, err := m.create("k-mac", "delta", &fleet.SandboxSpec{WritePaths: []fleet.AbsolutePath{fleet.AbsolutePath(filepath.Join(m.home, "later"))}}); err != nil {
		t.Errorf("the macOS plan lost its tolerance for a path that does not exist yet: %v", err)
	}
}
