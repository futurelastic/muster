package modclient_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/delivery/modclient"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/procgroup"
)

// These tests run the REAL launcher against a real child process: the test
// binary itself, re-executed through a shell wrapper written by modtest.Install
// (see TestMain). Nothing is built, and it works offline under -race.

// pids records every process a launcher started.
type pids struct {
	mu sync.Mutex
	l  []int
}

func (p *pids) launch(ctxLauncher modclient.Launcher) modclient.Launcher {
	return func(ctx context.Context, path string, args []string, env []string) (modclient.Proc, error) {
		proc, err := ctxLauncher(ctx, path, args, env)
		if err == nil {
			p.mu.Lock()
			p.l = append(p.l, proc.Pid())
			p.mu.Unlock()
		}
		return proc, err
	}
}

func (p *pids) all() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.l...)
}

func (p *pids) count() int { return len(p.all()) }

// gone reports whether no process has this id any more.
func gone(pid int) bool { return !procgroup.Alive(pid) }

func installFake(t *testing.T, b modtest.Behaviour) string {
	t.Helper()
	return modtest.Install(t, t.TempDir(), "fake", b)
}

// A real child process gets budgets of its own, not the ones fastConfig gives
// an in-memory fake (#214, #207 mechanism 4). Nothing lets a test control how
// long the host takes to start a process, and a host running the whole suite in
// parallel takes seconds where an idle one takes milliseconds.
//
// The budget that decides is realChildHello, not the bound a test waits with:
// a missed hello is not retried — the client refuses the child and disables
// the module until the daemon restarts — so a hello budget that is only a guess
// turns one slow start into a dead wait, which then reports the wait's bound
// ("timed out after 15s waiting for the child to say hello") rather than the
// budget that was really spent. Both numbers are backstops: a test that passes
// never waits for either, and waitChild ends a wait on a refusal at once.
const (
	// realChildHello is how long the client waits for a real child's hello.
	realChildHello = 60 * time.Second
	// realChildWait bounds a test's wait on something a real child must do,
	// and is longer than realChildHello so that a refusal is reported by name.
	realChildWait = 90 * time.Second
)

// realChildDeadlines are the per-request limits for a real child: the module is
// a whole process that the scheduler may not run for a while, not a function.
var realChildDeadlines = modclient.Deadlines{Default: realChildWait, Prepare: realChildWait, AttachExtra: realChildWait}

// waitChild waits until cond holds — an event a real child causes — and fails
// at once, with the client's own reason, if the client gives up on the module
// for good (StateDisabled) since nothing can make cond true after that.
func waitChild(t testing.TB, c *modclient.Client, cond func() bool, what string) {
	t.Helper()
	waitFor(t, func() bool {
		t.Helper()
		if st := c.Status(); st.State == modclient.StateDisabled {
			t.Fatalf("waiting for %s: the client disabled the module for good: %s", what, st.Reason)
		}
		return cond()
	}, realChildWait, what)
}

// waitUsable is waitChild for the module to come up.
func waitUsable(t testing.TB, c *modclient.Client, what string) {
	t.Helper()
	waitChild(t, c, c.Usable, what)
}

// realClient starts a Client over the real ExecLauncher.
func realClient(t *testing.T, path string, mut ...func(*modclient.Config)) (*modclient.Client, *rec, *pids) {
	t.Helper()
	ps := &pids{}
	c, r := newClient(t, nil, append([]func(*modclient.Config){func(cfg *modclient.Config) {
		cfg.Path = path
		cfg.Launcher = ps.launch(modclient.ExecLauncher)
		env, _ := modclient.ChildEnv(os.Getenv, t.TempDir(), nil)
		cfg.Env = env
		cfg.HelloTimeout = realChildHello
		cfg.Deadlines = realChildDeadlines
	}}, mut...)...)
	return c, r, ps
}

func TestProcess_SpawnsServeWithMinimalEnv(t *testing.T) {
	t.Setenv("MODCLIENT_FORWARDED", "fwd")
	t.Setenv("MODCLIENT_SECRET", "must-not-leak")
	t.Setenv("FLEET_LISTEN", "must-not-leak")
	dump := filepath.Join(t.TempDir(), "dump.json")
	path := installFake(t, modtest.Behaviour{DumpEnvTo: dump})
	stateDir := "/state/dir"
	env, dropped := modclient.ChildEnv(os.Getenv, stateDir, []string{"MODCLIENT_FORWARDED", "FLEET_LISTEN"})
	if !reflect.DeepEqual(dropped, []string{"FLEET_LISTEN"}) {
		t.Fatalf("dropped = %v", dropped)
	}

	c, _, ps := realClient(t, path, func(cfg *modclient.Config) { cfg.Env = env })
	c.Start()
	waitUsable(t, c, "the real child to say hello")

	d, err := modtest.ReadEnvDump(dump)
	if err != nil {
		t.Fatal(err)
	}
	// The module was started as `<path> serve`.
	if len(d.Args) == 0 || d.Args[len(d.Args)-1] != "serve" {
		t.Errorf("argv = %q, want a final serve", d.Args)
	}
	// Its environment is exactly what ChildEnv said, plus what the /bin/sh
	// wrapper itself adds — and nothing inherited from this process.
	shellAdds := map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true, "GORACE": true, modtest.EnvBehaviour: true}
	if runtime.GOOS == "windows" {
		// os/exec adds SYSTEMROOT to a minimal environment: without it a
		// process cannot load most system libraries.
		shellAdds["SYSTEMROOT"] = true
	}
	got := map[string]string{}
	for _, kv := range d.Env {
		name, val, _ := strings.Cut(kv, "=")
		if !shellAdds[name] {
			got[name] = val
		}
	}
	want := map[string]string{}
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		want[name] = val
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("child environment = %v\nwant                = %v", sorted(got), sorted(want))
	}
	if got["FLEET_STATE_DIR"] != stateDir || got["MODCLIENT_FORWARDED"] != "fwd" {
		t.Errorf("state dir / forwarded name missing: %v", sorted(got))
	}
	for name := range got {
		if name == "MODCLIENT_SECRET" || name == "FLEET_LISTEN" {
			t.Errorf("%s leaked into the child", name)
		}
	}
	// It does not run in the daemon's directory.
	wantCwd, _ := filepath.EvalSymlinks(os.TempDir())
	if gotCwd, _ := filepath.EvalSymlinks(d.Cwd); gotCwd != wantCwd {
		t.Errorf("child cwd = %s, want %s", gotCwd, wantCwd)
	}
	// It leads its own process group, so a kill takes down whatever it spawned.
	assertLeadsItsOwnGroup(t, ps.all()[0])
	// And it answers the protocol.
	if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "over a real pipe"}); err != nil {
		t.Errorf("send to a real process: %v", err)
	}
}

func sorted(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func TestProcess_NilEnvIsEmptyNotInherited(t *testing.T) {
	t.Setenv("MODCLIENT_SECRET", "must-not-leak")
	dump := filepath.Join(t.TempDir(), "dump.json")
	path := installFake(t, modtest.Behaviour{DumpEnvTo: dump})

	proc, err := modclient.ExecLauncher(bg(), path, []string{"serve"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { proc.Kill(); _ = proc.Wait() }()
	if line, err := bufio.NewReader(proc.Stdout()).ReadString('\n'); err != nil || !strings.Contains(line, `"event":"hello"`) {
		t.Fatalf("first line = %q, %v", line, err)
	}
	d, err := modtest.ReadEnvDump(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range d.Env {
		if strings.HasPrefix(kv, "MODCLIENT_SECRET=") || strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "HOME=") {
			t.Errorf("a nil env inherited the parent's environment: %s", kv)
		}
	}
}

func TestProcess_MissingBinaryIsUnavailableNotFatal(t *testing.T) {
	c, r, _ := realClient(t, filepath.Join(t.TempDir(), "no-such-module"), func(cfg *modclient.Config) {
		cfg.BackoffMin, cfg.BackoffMax = 20*time.Millisecond, 40*time.Millisecond
	})
	c.Start()
	waitFor(t, func() bool {
		for _, l := range r.Logs() {
			if strings.Contains(l, "could not start") {
				return true
			}
		}
		return false
	}, 5*time.Second, "a spawn failure to be logged")
	st := c.Status()
	if st.State != modclient.StateUnavailable || !strings.Contains(st.Reason, "spawn failed") || r.Count("spawned") != 0 {
		t.Errorf("status = %+v spawned=%d: a module that cannot be started is unavailable, and not counted as spawned", st, r.Count("spawned"))
	}
	if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"}); !errors.Is(err, modclient.ErrNotSent) {
		t.Errorf("send: %v, want ErrNotSent", err)
	}
}

func TestProcess_StderrLoggedQuoted(t *testing.T) {
	stderr := "first line\n" +
		"second \x1b[31mred\x1b[0m\x07 bell\n" +
		strings.Repeat("a", 2000) + "\n" +
		"last, unterminated"
	path := installFake(t, modtest.Behaviour{Stderr: stderr})
	c, r, _ := realClient(t, path)
	c.Start()
	waitUsable(t, c, "the module")
	waitFor(t, func() bool { return len(stderrLogs(r)) >= 3 }, realChildWait, "the stderr lines")
	c.Stop() // stderr reaches EOF: the unterminated last line is flushed

	logs := stderrLogs(r)
	if len(logs) != 4 {
		t.Fatalf("logged %d stderr lines, want 4: %q", len(logs), logs)
	}
	for _, l := range logs {
		if strings.ContainsAny(l, "\x1b\x07\r") || strings.Count(l, "\n") != 0 {
			t.Errorf("a raw control character reached the log: %q", l)
		}
	}
	if !strings.Contains(logs[0], `"first line"`) {
		t.Errorf("line 1 = %q", logs[0])
	}
	if !strings.Contains(logs[1], `\x1b[31mred\x1b[0m\a bell`) {
		t.Errorf("line 2 = %q: control characters must appear escaped (%%q)", logs[1])
	}
	if n := strings.Count(logs[2], "a"); n < 512 || n > 530 || !strings.Contains(logs[2], "(truncated)") {
		t.Errorf("line 3 has %d a's / %q: want the first 512 bytes and a truncation note", n, logs[2][:40])
	}
	if !strings.Contains(logs[3], `"last, unterminated"`) {
		t.Errorf("line 4 = %q: the last words of a stopping module must not be lost", logs[3])
	}
}

func stderrLogs(r *rec) []string {
	var out []string
	for _, l := range r.Logs() {
		if strings.Contains(l, "stderr:") {
			out = append(out, l)
		}
	}
	return out
}

func TestProcess_KillMidRunRestartsAndReady(t *testing.T) {
	record := filepath.Join(t.TempDir(), "requests")
	path := installFake(t, modtest.Behaviour{RecordTo: record, Ops: map[string]modtest.OpScript{"attach": {HangForever: true}}})
	c, r, ps := realClient(t, path, func(cfg *modclient.Config) { cfg.BackoffMin, cfg.BackoffMax = 20*time.Millisecond, 40*time.Millisecond })
	c.Start()
	waitChild(t, c, func() bool { return len(r.Ready()) == 1 }, "the first generation")

	// A request is in flight inside the real process when it is killed.
	failed := make(chan error, 1)
	go func() {
		_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k"})
		failed <- err
	}()
	waitFor(t, func() bool {
		b, _ := os.ReadFile(record)
		return strings.Contains(string(b), "attach")
	}, 5*time.Second, "the attach to reach the real process")
	first := ps.all()[0]
	proc, err := os.FindProcess(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-failed:
		if !errors.Is(err, modclient.ErrLost) {
			t.Errorf("the in-flight request: %v, want ErrLost", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight request was never failed after the process was killed")
	}
	waitChild(t, c, func() bool { return c.Generation() == 2 && c.Usable() }, "the restarted module to be ready")
	waitFor(t, func() bool { return len(r.Ready()) == 2 }, 5*time.Second, "OnReady for the second generation")

	if got := r.Ready(); !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Errorf("OnReady = %v, want [1 2]", got)
	}
	if r.Count("exited") != 1 || r.Count("restarted") != 1 || r.Count("spawned") != 2 || c.Status().Restarts != 1 {
		t.Errorf("counters = %v", r.counts)
	}
	if all := ps.all(); len(all) != 2 || all[0] == all[1] {
		t.Errorf("pids = %v, want two distinct processes", all)
	}
	if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "after the restart"}); err != nil {
		t.Errorf("send after the restart: %v", err)
	}
}

func TestProcess_ExitByScriptRestartsOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "exited-once")
	path := installFake(t, modtest.Behaviour{
		ExitOnceMarker: marker,
		Ops:            map[string]modtest.OpScript{"health": {ExitAfter: 3}},
	})
	c, r, _ := realClient(t, path, func(cfg *modclient.Config) {
		cfg.HealthInterval = 25 * time.Millisecond
		cfg.BackoffMin, cfg.BackoffMax = 20*time.Millisecond, 40*time.Millisecond
	})
	c.Start()
	// The first process answers three health requests and exits by itself; the
	// marker keeps the second one alive.
	waitChild(t, c, func() bool { return len(r.Ready()) == 2 }, "the module to exit and come back")
	if !reflect.DeepEqual(r.Ready(), []uint64{1, 2}) || r.Count("exited") != 1 {
		t.Errorf("ready=%v exited=%d", r.Ready(), r.Count("exited"))
	}
	waitFor(t, c.Usable, 5*time.Second, "the second process to stay up")
}

func TestProcess_HelloSilenceKillsTheRealProcess(t *testing.T) {
	path := installFake(t, modtest.Behaviour{Hello: &modtest.HelloScript{NoLine: true}})
	c, r, ps := realClient(t, path, func(cfg *modclient.Config) { cfg.HelloTimeout = 300 * time.Millisecond })
	c.Start()
	waitFor(t, func() bool { return c.Status().State == modclient.StateDisabled }, 15*time.Second, "the silent module to be refused")
	if r.Count("hello_refused") != 1 {
		t.Errorf("hello_refused = %d", r.Count("hello_refused"))
	}
	pid := ps.all()[0]
	waitFor(t, func() bool { return gone(pid) }, 5*time.Second, "the refused process to be gone")
	time.Sleep(100 * time.Millisecond)
	if ps.count() != 1 {
		t.Errorf("a refused module was started %d times", ps.count())
	}
}

func TestProcess_ShutdownGrace(t *testing.T) {
	t.Run("CooperativeModuleIsNotKilledOrWaitedFor", func(t *testing.T) {
		path := installFake(t, modtest.Behaviour{})
		c, _, ps := realClient(t, path, func(cfg *modclient.Config) { cfg.ShutdownGrace = 10 * time.Second })
		c.Start()
		waitUsable(t, c, "the module")
		start := time.Now()
		c.Stop()
		if took := time.Since(start); took > 8*time.Second {
			t.Errorf("Stop took %v: a module that exits when its stdin closes must not be waited on for the grace", took)
		}
		if !gone(ps.all()[0]) {
			t.Error("the process outlived Stop")
		}
	})

	t.Run("StubbornModuleIsKilledAfterTheGrace", func(t *testing.T) {
		path := installFake(t, modtest.Behaviour{IgnoreStdinClose: true})
		c, _, ps := realClient(t, path, func(cfg *modclient.Config) { cfg.ShutdownGrace = 300 * time.Millisecond })
		c.Start()
		waitUsable(t, c, "the module")
		pid := ps.all()[0]
		start := time.Now()
		c.Stop()
		if took := time.Since(start); took < 290*time.Millisecond {
			t.Errorf("Stop returned after %v, before the grace ran out: a stubborn module must be given it", took)
		}
		if !gone(pid) {
			t.Error("a module that ignores its stdin closing must be killed, and the kill reaped, by the time Stop returns")
		}
		if st := c.Status(); st.State != modclient.StateDisabled {
			t.Errorf("state = %s", st.State)
		}
	})
}
