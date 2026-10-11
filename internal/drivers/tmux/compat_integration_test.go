package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// These tests run the compat harness against a real multiplexer with a
// synthetic runtime: no real agent, no network, no tokens. They are gated the
// same way the driver's other multiplexer tests are.

func compatIntegration(t *testing.T) (tmuxBin, python string) {
	t.Helper()
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	var err error
	if tmuxBin, err = exec.LookPath("tmux"); err != nil {
		t.Skip("no multiplexer on PATH")
	}
	if python, err = exec.LookPath("python3"); err != nil {
		t.Skip("no python3 for the synthetic runtime")
	}
	return tmuxBin, python
}

// fakeRuntime writes an executable stand-in for the agent: it answers
// --version and otherwise becomes the synthetic composer. Knobs are set INSIDE
// the launcher because the harness clears the environment — which is exactly
// what a knob passed the ordinary way would not survive.
func fakeRuntime(t *testing.T, python string, knobs map[string]string) string {
	t.Helper()
	tui, err := filepath.Abs(filepath.Join("testdata", "faketui.py"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncase \"$1\" in --version) echo \"0.0.0-synthetic (Synthetic Runtime)\"; exit 0 ;; esac\n")
	for k, v := range knobs {
		b.WriteString("export " + k + "=" + shQuote(v) + "\n")
	}
	// FAKE_MARKERS=1 makes the launcher itself carry the wording the static checks
	// scan a candidate for — the harness scans this file, not the synthetic
	// composer it execs — so a check that reads it can be shown to pass. Off by
	// default: a candidate that lacks the wording is what the failing cases need.
	if knobs["FAKE_MARKERS"] == "1" {
		b.WriteString("# " + allMarkersText() + "\n")
	}
	// "$@" so the synthetic runtime sees how it was launched: a bypass session is
	// started with a flag, and the real runtime paints a different indicator for it.
	b.WriteString("exec " + shQuote(python) + " " + shQuote(tui) + " \"$@\"\n")
	p := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(p, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// harnessFor builds a harness whose whole store lives in a temp directory, so
// a test can never touch the real runtime's files.
func harnessFor(t *testing.T, candidate string) *compatHarness {
	t.Helper()
	home := t.TempDir()
	getenv := env(
		"HOME", home,
		"FLEET_RECORD_ROOT", filepath.Join(home, "projects"),
		"FLEET_PROCESS_SESSIONS_ROOT", filepath.Join(home, "sessions"),
		"FLEET_TRUST_STATE_PATH", filepath.Join(home, "state.json"),
	)
	for _, d := range []string{"projects", "sessions"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h := &compatHarness{getenv: getenv}
	h.cand.Resolved = candidate
	t.Cleanup(h.teardown) // never leave a server behind, whatever the test did
	return h
}

func cfcDirs(t *testing.T) map[string]bool {
	t.Helper()
	m, _ := filepath.Glob("/tmp/cfc-*")
	out := map[string]bool{}
	for _, p := range m {
		out[p] = true
	}
	return out
}

// waitFor polls until fn is true.
func cfcWaitFor(t *testing.T, what string, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// assertGone proves a torn-down world left nothing behind.
func assertGone(t *testing.T, tmuxBin string, w *compatWorld, pids []int) {
	t.Helper()
	if _, err := os.Stat(w.scratch); err == nil {
		t.Errorf("the scratch directory %s was left behind", w.scratch)
	}
	if out, err := exec.Command(tmuxBin, "-S", w.socket, "list-sessions").CombinedOutput(); err == nil {
		t.Errorf("the private server is still answering on its socket:\n%s", out)
	}
	if _, err := os.Stat(w.socket); err == nil {
		t.Errorf("the socket %s was left behind", w.socket)
	}
	for _, pid := range pids {
		cfcWaitFor(t, "pane process "+strconv.Itoa(pid)+" to end", 5*time.Second, func() bool {
			return pidGone(pid)
		})
	}
	for _, d := range []string{"projects", "sessions"} {
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(w.store.projects), d, "*cfc-"+w.nonce+"*"))
		if len(left) > 0 {
			t.Errorf("leftovers in the store's %s: %v", d, left)
		}
	}
}

func TestCompatWorldIsolationAndTeardown(t *testing.T) {
	tmuxBin, python := compatIntegration(t)
	// A canary in the caller's own environment: it must never reach the
	// candidate. And $TMUX set to a socket that does not exist: a stray command
	// with an implicit "current server" would fail loudly here.
	t.Setenv("CFC_CANARY", "leaked-from-the-caller")
	t.Setenv("TMUX", "/nonexistent/socket,1,0")
	before := cfcDirs(t)

	cand := fakeRuntime(t, python, nil)
	h := harnessFor(t, cand)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	w := h.world

	// The private server holds only the keeper, and its environment is the
	// minimal set: the two update locks, and nothing from the caller.
	if out, _ := w.tmux(ctx, "list-sessions", "-F", "#{session_name}"); strings.TrimSpace(out) != compatKeeper {
		t.Fatalf("sessions = %q, want only the keeper", out)
	}
	envOut, err := w.tmux(ctx, "show-environment", "-g")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DISABLE_UPDATES=1", "DISABLE_AUTOUPDATER=1", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"} {
		if !strings.Contains(envOut, want) {
			t.Errorf("the server's environment lacks %s:\n%s", want, envOut)
		}
	}
	for _, leak := range []string{"CFC_CANARY", "leaked-from-the-caller", "CLAUDE_", "TMUX="} {
		if strings.Contains(envOut, leak) {
			t.Errorf("the server's environment leaks %q:\n%s", leak, envOut)
		}
	}

	// A session is created through the driver's real Create, launched by the
	// resolved candidate, on the private server only.
	ref, err := w.create(ctx, "a", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfcWaitFor(t, "the synthetic composer", 15*time.Second, func() bool {
		out, _ := w.tmux(ctx, "capture-pane", "-p", "-t", ref.ID)
		return strings.Contains(out, "fake tui ready")
	})
	cmdline, _ := w.tmux(ctx, "list-panes", "-a", "-F", "#{session_name} #{pane_start_command}")
	var row string
	for _, l := range strings.Split(cmdline, "\n") {
		if strings.HasPrefix(l, ref.ID+" ") {
			row = l
		}
	}
	if !strings.Contains(row, cand) || !strings.Contains(row, "-n "+ref.ID) || !strings.Contains(row, "--model") {
		t.Errorf("the pane was not started as the candidate with its name and pins: %q", row)
	}
	if strings.Contains(row, "--remote-control") {
		t.Errorf("remote control must stay off: %q", row)
	}
	// The default server must not have heard of it.
	if out, _ := exec.Command(tmuxBin, "list-sessions", "-F", "#{session_name}").CombinedOutput(); strings.Contains(string(out), w.label) {
		t.Errorf("the session appeared on a server that is not ours:\n%s", out)
	}
	pids := w.paneProcesses(ctx)
	if len(pids) < 2 {
		t.Fatalf("expected the keeper's and the candidate's pane processes, got %v", pids)
	}

	h.teardown()
	if len(h.tearErrs) != 0 {
		t.Errorf("teardown reported problems: %v", h.tearErrs)
	}
	assertGone(t, tmuxBin, w, pids)
	for p := range cfcDirs(t) {
		if !before[p] {
			t.Errorf("a new scratch directory survived: %s", p)
		}
	}
}

// A run cancelled by a signal must still clean up: teardown runs on its own
// deadline, not the cancelled context.
func TestCompatTeardownSurvivesACancelledContext(t *testing.T) {
	tmuxBin, python := compatIntegration(t)
	h := harnessFor(t, fakeRuntime(t, python, nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	w := h.world
	if _, err := w.create(ctx, "a", "a", nil); err != nil {
		t.Fatal(err)
	}
	pids := w.paneProcesses(context.Background())
	cancel() // the signal arrives
	h.teardown()
	if len(h.tearErrs) != 0 {
		t.Errorf("teardown reported problems: %v", h.tearErrs)
	}
	assertGone(t, tmuxBin, w, pids)
}

// The service's own sessions are untouched. A second private server stands in
// for it, with a session of its own and $TMUX pointing at it — the very trap
// an implicit "current server" would fall into — and its session ids and its
// pane must be identical after a whole world has been built and torn down.
func TestCompatNeverTouchesAnotherServer(t *testing.T) {
	tmuxBin, python := compatIntegration(t)
	// The socket lives under /tmp, not the per-test directory: a unix socket
	// path is capped near 104 bytes and the test's own directory overruns it.
	other := filepath.Join("/tmp", "svc-standin-"+randomNonce()[:8]+".sock")
	t.Cleanup(func() { _ = os.Remove(other) })
	otherMux := func(args ...string) (string, error) {
		out, err := exec.Command(tmuxBin, append([]string{"-S", other, "-f", "/dev/null"}, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := otherMux("new-session", "-d", "-s", "service-own", "--", "sleep", "300"); err != nil {
		// Not a skip: integration is explicitly enabled, and a skip here would
		// hide the one assertion this test exists for.
		t.Fatalf("could not start a stand-in server: %v %s", err, out)
	}
	defer otherMux("kill-server")
	snapshot := func() string {
		ids, _ := otherMux("list-sessions", "-F", "#{session_id} #{session_name}")
		panes, _ := otherMux("list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_dead}")
		return ids + "|" + panes
	}
	before := snapshot()
	// Everything a stray command could latch onto points at the stand-in.
	t.Setenv("TMUX", other+",1,0")
	t.Setenv("TMUX_TMPDIR", filepath.Dir(other))

	h := harnessFor(t, fakeRuntime(t, python, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.world.create(ctx, "a", "a", nil); err != nil {
		t.Fatal(err)
	}
	h.teardown()

	if after := snapshot(); after != before {
		t.Errorf("the stand-in server changed while the harness ran:\n before: %s\n after:  %s", before, after)
	}

	// Positive control: the comparison above is only worth anything if it can
	// see a change. Touch the stand-in on purpose and confirm it does.
	if out, err := otherMux("new-session", "-d", "-s", "control", "--", "sleep", "300"); err != nil {
		t.Fatalf("control session: %v %s", err, out)
	}
	if snapshot() == before {
		t.Error("the snapshot did not notice a new session: this test could not have caught a leak")
	}
}

// A static-only run never starts a server or creates a scratch directory.
func TestCompatStaticOnlyRunTouchesNoMultiplexer(t *testing.T) {
	_, python := compatIntegration(t)
	before := cfcDirs(t)
	cand := fakeRuntime(t, python, nil)
	rep, err := RunCompat(context.Background(), CompatOptions{Claude: cand, Only: []string{"H-RC"}, Getenv: env("HOME", t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) != 1 {
		t.Fatalf("checks = %+v", rep.Checks)
	}
	for p := range cfcDirs(t) {
		if !before[p] {
			t.Errorf("a static-only run created %s", p)
		}
	}
}

// However a caller shapes the spec, a created session lands on the private
// server: the wrapper, not the spec, decides where a session goes.
func TestCompatCreatedSessionIsOnThePrivateServer(t *testing.T) {
	_, python := compatIntegration(t)
	h := harnessFor(t, fakeRuntime(t, python, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	ref, err := h.world.create(ctx, "a", "a", func(s *fleet.SessionSpec) { s.Name = "someone-elses-name" })
	if err != nil {
		t.Fatal(err)
	}
	// Whatever the caller asked for, the created session is on OUR server.
	out, _ := h.world.tmux(ctx, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(out, ref.ID) {
		t.Errorf("session %q not on the private server: %s", ref.ID, out)
	}
}

// shortWaits lets a synthetic runtime, which boots at once and has no
// bookkeeping to protect, be checked in seconds rather than minutes. Nothing
// outside a test assigns these.
func shortWaits(t *testing.T) {
	t.Helper()
	settle, boot, dialog, record, bracket, mode := compatSettleAge, compatBootWait, compatDialogWait, compatRecordWait, compatBracketWait, compatModeWait
	compatSettleAge, compatBootWait, compatDialogWait, compatRecordWait, compatBracketWait, compatModeWait =
		0, 6*time.Second, 3*time.Second, 2*time.Second, 2*time.Second, 2*time.Second
	t.Cleanup(func() {
		compatSettleAge, compatBootWait, compatDialogWait, compatRecordWait, compatBracketWait, compatModeWait = settle, boot, dialog, record, bracket, mode
	})
}

// runFake runs the real command path against a synthetic runtime with the given
// knobs, restricted to the given checks, on a store that lives in a temp dir.
func runFake(t *testing.T, python string, knobs map[string]string, only ...string) (rep map[string]string, exit int) {
	t.Helper()
	shortWaits(t)
	home := t.TempDir()
	for _, d := range []string{"projects", "sessions"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The runtime's own state file, where folder trust and the imports answer live.
	// It has to exist for the seeder to write into it, as it always does for a real
	// runtime; the synthetic one reads the same file (FAKE_STATE) to decide whether
	// to ask about a directory's imports (#212).
	state := filepath.Join(home, "state.json")
	if err := os.WriteFile(state, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withState := map[string]string{"FAKE_STATE": state}
	for k, v := range knobs {
		withState[k] = v
	}
	report, err := RunCompat(context.Background(), CompatOptions{
		Claude: fakeRuntime(t, python, withState), Only: only,
		Getenv: env("HOME", home, "FLEET_RECORD_ROOT", filepath.Join(home, "projects"),
			"FLEET_PROCESS_SESSIONS_ROOT", filepath.Join(home, "sessions"), "FLEET_TRUST_STATE_PATH", state),
	})
	if err != nil {
		t.Fatalf("RunCompat: %v", err)
	}
	rep = map[string]string{}
	for _, c := range report.Checks {
		switch {
		case c.Error:
			rep[c.ID] = "error: " + c.Detail
		case c.Pass:
			rep[c.ID] = "pass: " + c.Detail
		default:
			rep[c.ID] = "fail: " + c.Detail
		}
	}
	return rep, report.ExitCode()
}

// The acceptance for a check is that it can FAIL, and fail with the right id.
// A synthetic runtime that is healthy passes the composer checks; the same
// runtime with one thing broken fails exactly the checks that depend on it.
func TestCompatBrokenSyntheticCandidates(t *testing.T) {
	_, python := compatIntegration(t)

	t.Run("healthy", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_PLACEHOLDER": "1"}, "F-COMPOSER", "F-MLDRAFT")
		for _, id := range []string{"F-COMPOSER", "F-MLDRAFT"} {
			if !strings.HasPrefix(rep[id], "pass") {
				t.Errorf("%s = %s", id, rep[id])
			}
		}
		if exit != 0 {
			t.Errorf("exit %d, want 0", exit)
		}
	})

	// #194: F-MODE reads the indicator off two live sessions (default, bypass) and
	// scans the candidate for the other three wordings.
	t.Run("F-MODE passes when the indicator rows are the measured ones", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_MARKERS": "1"}, "F-MODE")
		if !strings.HasPrefix(rep["F-MODE"], "pass") {
			t.Errorf("F-MODE = %s", rep["F-MODE"])
		}
		if exit != 0 {
			t.Errorf("exit %d, want 0", exit)
		}
	})

	t.Run("F-MODE fails, and says which session, when the wording is missing from the candidate", func(t *testing.T) {
		rep, exit := runFake(t, python, nil, "F-MODE")
		if !strings.HasPrefix(rep["F-MODE"], "fail") || !strings.Contains(rep["F-MODE"], "no longer contains") {
			t.Errorf("F-MODE = %s, want a failure naming the missing wording", rep["F-MODE"])
		}
		// A warn check: reported, never changes the verdict.
		if exit != 0 {
			t.Errorf("exit %d, want 0 (F-MODE is warn-gated)", exit)
		}
	})

	// The failure the check exists for: a candidate that REWORDS the indicator
	// would silently start reporting `unknown` in production. Here the wording is
	// all still in the launcher (so the static half passes) and only the live read
	// can notice — which is why the live half exists.
	t.Run("F-MODE fails, naming the read, when a candidate rewords the indicator", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_MARKERS": "1", "FAKE_MODE_WORDING": "reworded"}, "F-MODE")
		if !strings.HasPrefix(rep["F-MODE"], "fail") ||
			!strings.Contains(rep["F-MODE"], "reads as unknown") ||
			!strings.Contains(rep["F-MODE"], "default mode") || !strings.Contains(rep["F-MODE"], "bypass-permissions mode") {
			t.Errorf("F-MODE = %s, want a failure saying both sessions read as unknown", rep["F-MODE"])
		}
		if exit != 0 {
			t.Errorf("exit %d, want 0 (F-MODE is warn-gated)", exit)
		}
	})

	t.Run("a different prompt glyph fails the composer check", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_PLACEHOLDER": "1", "FAKE_GLYPH": ">"}, "F-COMPOSER")
		if !strings.HasPrefix(rep["F-COMPOSER"], "fail") || !strings.Contains(rep["F-COMPOSER"], "no composer") {
			t.Errorf("F-COMPOSER = %s, want a failure saying no composer was found", rep["F-COMPOSER"])
		}
		if exit != 1 {
			t.Errorf("exit %d, want 1 (a must check failed)", exit)
		}
	})

	t.Run("no bracketed paste fails the checks that paste, and says why", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_PLACEHOLDER": "1", "FAKE_NO_BRACKET": "1"}, "F-COMPOSER", "F-MLDRAFT")
		for _, id := range []string{"F-COMPOSER", "F-MLDRAFT"} {
			if !strings.HasPrefix(rep[id], "fail") || !strings.Contains(rep[id], "could not be pasted") {
				t.Errorf("%s = %s, want a failure saying the draft could not be pasted", id, rep[id])
			}
		}
		if exit != 1 {
			t.Errorf("exit %d, want 1", exit)
		}
	})

	t.Run("a placeholder that is not dim is read as a draft", func(t *testing.T) {
		// The driver tells a placeholder from typed text by its dimness, so a
		// placeholder painted without it reads as a draft — and an empty composer
		// that "holds a draft" makes every delivery refuse.
		rep, exit := runFake(t, python, map[string]string{"FAKE_PLACEHOLDER": "plain"}, "F-COMPOSER")
		if !strings.HasPrefix(rep["F-COMPOSER"], "fail") || !strings.Contains(rep["F-COMPOSER"], "placeholder was taken for a draft") {
			t.Errorf("F-COMPOSER = %s, want a failure saying the placeholder was taken for a draft", rep["F-COMPOSER"])
		}
		if exit != 1 {
			t.Errorf("exit %d, want 1", exit)
		}
	})

	t.Run("an empty composer with no placeholder at all still passes", func(t *testing.T) {
		// The real runtime sometimes paints no placeholder yet. That is a valid
		// empty composer, and a check that demanded one would fail a healthy build.
		rep, _ := runFake(t, python, nil, "F-COMPOSER")
		if !strings.HasPrefix(rep["F-COMPOSER"], "pass") {
			t.Errorf("F-COMPOSER = %s, want a pass", rep["F-COMPOSER"])
		}
	})
}

// #212: the external-imports dialog and the two state keys the seeder writes for
// it. F-IMPORTS observes the question on a directory the harness trusted and did
// not approve; C1 shows the seeder's answer stops it on a directory that imports
// a file from outside itself. Each has to be able to FAIL, with its own ID, and
// say why.
func TestCompatImportsChecksCanFail(t *testing.T) {
	_, python := compatIntegration(t)

	t.Run("a candidate that asks passes F-IMPORTS, and the seeder's answer stops it asking in C1", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_IMPORTS": "1"}, "C1", "F-IMPORTS")
		if !strings.HasPrefix(rep["F-IMPORTS"], "pass") || !strings.Contains(rep["F-IMPORTS"], "decline highlighted") {
			t.Errorf("F-IMPORTS = %s", rep["F-IMPORTS"])
		}
		if !strings.HasPrefix(rep["C1"], "pass") || !strings.Contains(rep["C1"], "imports a file from outside it") {
			t.Errorf("C1 = %s", rep["C1"])
		}
		if exit != 0 {
			t.Errorf("exit %d, want 0", exit)
		}
	})

	t.Run("a candidate that no longer asks fails F-IMPORTS and says so", func(t *testing.T) {
		rep, exit := runFake(t, python, nil, "F-IMPORTS")
		if !strings.HasPrefix(rep["F-IMPORTS"], "fail") || !strings.Contains(rep["F-IMPORTS"], "no dialog appeared") {
			t.Errorf("F-IMPORTS = %s, want a failure saying no dialog appeared", rep["F-IMPORTS"])
		}
		if exit != 1 {
			t.Errorf("exit %d, want 1 (F-IMPORTS is a must check)", exit)
		}
	})

	t.Run("a candidate that rewords the options fails F-IMPORTS on classification", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_IMPORTS": "reworded"}, "F-IMPORTS")
		if !strings.HasPrefix(rep["F-IMPORTS"], "fail") || !strings.Contains(rep["F-IMPORTS"], `classified as ""`) {
			t.Errorf("F-IMPORTS = %s, want a failure saying the dialog was not classified", rep["F-IMPORTS"])
		}
		if exit != 1 {
			t.Errorf("exit %d, want 1", exit)
		}
	})

	t.Run("a candidate that highlights the allow row fails F-IMPORTS", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_IMPORTS": "allow-highlighted"}, "F-IMPORTS")
		if !strings.HasPrefix(rep["F-IMPORTS"], "fail") || !strings.Contains(rep["F-IMPORTS"], "bare Enter would allow the import") {
			t.Errorf("F-IMPORTS = %s, want a failure saying a bare Enter would allow the import", rep["F-IMPORTS"])
		}
		if exit != 1 {
			t.Errorf("exit %d, want 1", exit)
		}
	})

	// The failure C1 gained this half for: the runtime keeps the answer under a name
	// the seeder does not write, so seeding silently stops working and the only
	// symptom is a session parked on the question.
	t.Run("a candidate that records the answer under another key fails C1 and names the keys", func(t *testing.T) {
		rep, exit := runFake(t, python, map[string]string{"FAKE_IMPORTS": "1", "FAKE_IMPORTS_KEY": "someOtherKey"}, "C1")
		if !strings.HasPrefix(rep["C1"], "fail") ||
			!strings.Contains(rep["C1"], "still asks about that import") ||
			!strings.Contains(rep["C1"], "hasClaudeMdExternalIncludesApproved") ||
			!strings.Contains(rep["C1"], "hasClaudeMdExternalIncludesWarningShown") {
			t.Errorf("C1 = %s, want a failure naming the imports question and both keys", rep["C1"])
		}
		if exit != 1 {
			t.Errorf("exit %d, want 1", exit)
		}
	})
}
