package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Whether the agent program is found (muster #342).

type probeExec struct {
	mu    sync.Mutex
	calls int
	out   string
	err   error
	args  []string
}

func (p *probeExec) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.args = append([]string{name}, args...)
	return []byte(p.out), p.err
}

func (p *probeExec) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the probe asks a login shell; this driver has none on Windows")
	}
}

func TestAgentProgramMissing_ReadsTheSentinelNotTheExitStatus(t *testing.T) {
	skipWindows(t)
	cases := []struct {
		name        string
		out         string
		err         error
		wantMissing bool
	}{
		{"found", agentFoundMark + "\n", nil, false},
		{"missing", agentMissingMark + "\n", nil, true},
		{"missing though the shell exited non-zero", "rc noise\n" + agentMissingMark + "\n", errors.New("exit status 1"), true},
		{"found though startup files printed first", "Last login: today\n" + agentFoundMark + "\n", errors.New("exit status 1"), false},
		{"no sentinel is not known, so not missing", "", errors.New("exit status 1"), false},
		{"unrecognised output is not known", "something else\n", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &probeExec{out: c.out, err: c.err}
			d := New("m", withExec(p.exec))
			prog, missing := d.AgentProgramMissing(context.Background())
			if prog != "claude" || missing != c.wantMissing {
				t.Errorf("got (%q, %v), want (claude, %v)", prog, missing, c.wantMissing)
			}
		})
	}
}

func TestAgentProgramMissing_AsksTheShellASessionWouldUse(t *testing.T) {
	skipWindows(t)
	p := &probeExec{out: agentFoundMark + "\n"}
	d := New("m", withExec(p.exec), WithLoginShell("/some/shell"))
	d.AgentProgramMissing(context.Background())
	if len(p.args) < 5 || p.args[0] != "/some/shell" || p.args[1] != "-lic" || p.args[len(p.args)-1] != "claude" {
		t.Errorf("probe argv = %q, want the configured shell with -lic and the program as a parameter", p.args)
	}
}

func TestAgentProgramMissing_IsCachedBriefly(t *testing.T) {
	skipWindows(t)
	p := &probeExec{out: agentMissingMark + "\n"}
	now := time.Unix(1_800_000_000, 0)
	d := New("m", withExec(p.exec), withClock(func() time.Time { return now }))
	for i := 0; i < 3; i++ {
		if _, missing := d.AgentProgramMissing(context.Background()); !missing {
			t.Fatal("want missing")
		}
	}
	if p.count() != 1 {
		t.Errorf("probes = %d, want 1 inside the window", p.count())
	}
	// Installed meanwhile: seen once the window has passed, with no restart.
	p.mu.Lock()
	p.out = agentFoundMark + "\n"
	p.mu.Unlock()
	now = now.Add(agentMissingTTL + time.Second)
	if _, missing := d.AgentProgramMissing(context.Background()); missing {
		t.Error("still missing after the window; want the new answer")
	}
	if p.count() != 2 {
		t.Errorf("probes = %d, want 2", p.count())
	}
}

func TestAgentProgramMissing_ACustomBuilderIsNotJudged(t *testing.T) {
	p := &probeExec{out: agentMissingMark + "\n"}
	d := New("m", withExec(p.exec), WithCommandBuilder(func(fleet.SessionSpec, string) []string { return []string{"whatever"} }))
	if prog, missing := d.AgentProgramMissing(context.Background()); missing || prog != "" || p.count() != 0 {
		t.Errorf("got (%q, %v) after %d probes; want nothing said about a program this driver cannot name", prog, missing, p.count())
	}
}

func TestAgentProgramMissing_ACancelledAskIsNotAnAnswer(t *testing.T) {
	skipWindows(t)
	p := &probeExec{err: context.Canceled}
	d := New("m", withExec(p.exec))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, missing := d.AgentProgramMissing(ctx); missing {
		t.Error("a cancelled ask must not read as missing")
	}
	d2 := New("m", withExec((&probeExec{out: agentMissingMark + "\n"}).exec))
	if _, missing := d2.AgentProgramMissing(context.Background()); !missing {
		t.Error("the cancelled ask must not have been remembered")
	}
}

// The script itself, run by a real /bin/sh behind a wrapper that stands in for
// the login shell (so a tester's startup files stay out of a unit test): found
// on the PATH it is given, missing without it, and a function or alias of the
// same name does not count — exec would not start one.
func TestAgentProgramScript_RealShell(t *testing.T) {
	skipWindows(t)
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "shell")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /bin/sh -c \"$2\" \"$3\" \"$4\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	d := New("m", WithLoginShell(wrapper))
	d.agentProgram = "fakeagent"

	if _, missing := d.AgentProgramMissing(context.Background()); !missing {
		t.Fatal("fakeagent is on no PATH, want missing")
	}

	if err := os.WriteFile(filepath.Join(bin, "fakeagent"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d2 := New("m", WithLoginShell(wrapper))
	d2.agentProgram = "fakeagent"
	if _, missing := d2.AgentProgramMissing(context.Background()); missing {
		t.Error("fakeagent is an executable on PATH, want found")
	}

	// Present but not executable: exec would refuse it.
	if err := os.WriteFile(filepath.Join(bin, "plainfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3 := New("m", WithLoginShell(wrapper))
	d3.agentProgram = "plainfile"
	if _, missing := d3.AgentProgramMissing(context.Background()); !missing {
		t.Error("a file without the execute bit, want missing")
	}
}

func TestProbeAgentProgram_PathNamesTheFile(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	f := filepath.Join(dir, "agent")
	if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if found, known := ProbeAgentProgram(context.Background(), f); !found || !known {
		t.Errorf("executable file: found=%v known=%v", found, known)
	}
	if found, known := ProbeAgentProgram(context.Background(), filepath.Join(dir, "absent")); found || !known {
		t.Errorf("absent file: found=%v known=%v, want missing and known", found, known)
	}
}
