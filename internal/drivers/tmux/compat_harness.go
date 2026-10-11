package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/compat"
	"github.com/futurelastic/muster/internal/trustseed"
)

// This file is the isolation of `muster compat`: a private multiplexer
// server, one door to it, throwaway working directories, and a teardown that
// always runs. Nothing here may ever address a server it did not start.
//
// # The one door
//
// Every multiplexer call the driver makes goes through the binary it was built
// with (d.bin — verified: the only other process the driver starts is a
// read-only process-table lookup). The harness builds the driver with a
// wrapper script as that binary, and the script is the ONLY thing that names a
// socket:
//
//	exec env -i <minimal environment> <tmux> -L <label> -f /dev/null "$@"
//
// `env -i` matters as much as `-L`: it drops $TMUX, so there is no implicit
// "current server" a stray command could fall back to, and it makes the
// server's own environment — which every session inherits — exactly the
// minimal set, never whatever the caller happened to export. The harness itself
// never runs the multiplexer except through that script.
//
// # What a run leaves behind, and why
//
// Teardown removes the server, its socket, the scratch directory, and the
// throwaway transcript directories and per-process records. Two things
// survive, by design, and docs/compat.md says so: the trust entries a run
// writes into the runtime's state file (there is no API to remove one, and
// editing that file while live sessions rewrite it is the race the trust
// seeder exists to survive), and the model turns spent.

const (
	compatKeeper = "cfc-keep"
	// compatTeardownWait bounds the whole teardown, including the settling
	// wait below.
	compatTeardownWait = 45 * time.Second
	compatPaneCols     = 120
	compatPaneRows     = 40
)

// The waits below are variables only so that a test driving a synthetic runtime
// — which boots instantly and has no bookkeeping to protect — can shorten them.
// Nothing outside a test assigns them.
var (
	// compatSettleAge is how long a launched candidate must have been alive
	// before it is killed.
	//
	// # Why a run must not kill a young session
	//
	// The runtime records each launch in a machine-wide state file and clears the
	// record once the launch has been alive for about ten seconds, which also
	// clears the count of failed launches. A launch that dies sooner is counted as
	// a failed start by the NEXT launch, and enough of those switch the runtime's
	// fullscreen renderer off for that version on the whole machine — measured,
	// not assumed: the record appeared one second after a session started and was
	// cleared, together with the strike count, at eleven. A compat run that killed
	// short-lived sessions could therefore change how every real session on the
	// machine renders, silently and durably.
	//
	// So teardown lets every session it started reach this age first. It is judged
	// by age, not by reading the runtime's private state, so it does not depend on
	// a key name that may change; the margin covers a slow boot. A full run lasts
	// minutes and never waits; only a run that ends within seconds of a boot does.
	compatSettleAge = 15 * time.Second
)

// compatStore is where the runtime keeps what the checks read back. It is the
// same store the service reads; the service's own environment variables move it
// (which is how a test points it somewhere else).
type compatStore struct {
	home       string // the runtime's home directory
	projects   string // transcript directories
	sessions   string // per-process session records
	trustState string // the runtime's own state file, where folder trust lives
}

func compatStoreFrom(getenv func(string) string) (compatStore, error) {
	home := getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return compatStore{}, fmt.Errorf("no home directory: %w", err)
		}
		home = h
	}
	s := compatStore{
		home:       home,
		projects:   filepath.Join(home, ".claude", "projects"),
		sessions:   filepath.Join(home, ".claude", "sessions"),
		trustState: filepath.Join(home, ".claude.json"),
	}
	// Same overrides the service honours: pointing them somewhere else is how
	// a test keeps a run off the real store.
	if v := getenv("FLEET_RECORD_ROOT"); v != "" {
		s.projects = v
	}
	if v := getenv("FLEET_PROCESS_SESSIONS_ROOT"); v != "" {
		s.sessions = v
	}
	if v := getenv("FLEET_TRUST_STATE_PATH"); v != "" {
		s.trustState = v
	}
	return s, nil
}

// compatWorld is the isolated environment one run drives.
type compatWorld struct {
	nonce    string
	scratch  string // symlink-resolved, so every path the runtime reports agrees with ours
	label    string // the multiplexer socket name, printed so an operator can inspect it
	socket   string
	mux      string // the wrapper: the only door to the multiplexer
	tmuxBin  string
	store    compatStore
	resolved string
	d        *Driver

	// dirs are the throwaway working directories, keyed by role.
	dirs map[string]string

	// sharedImport is the file, outside every working directory, that the
	// instruction files of directories "a" and "i" import (#212).
	sharedImport string

	// trustOnly answers the folder-trust question for directory "i" and leaves
	// the external-imports one standing. It is the only way the harness can
	// observe that dialog on a real candidate: the driver's own seeder writes
	// both answers, so a directory it seeds can never ask (#212).
	trustOnly *trustseed.Seeder

	// ctxFile is a system-prompt file for --append-system-prompt-file.
	ctxFile string

	// created records every session this run made, so teardown can find its
	// processes and files without listing anything that is not ours.
	created []compatCreated

	// extraArgs adds arguments to one session's command line, keyed by
	// session name. Used for a single session that needs a different setting.
	extraArgs map[string][]string
}

type compatCreated struct {
	label string
	ref   fleet.SessionRef
	cwd   string
	at    time.Time // when the launch was requested
}

var compatRequest = fleet.Request{Caller: fleet.Caller{Principal: "compat"}}

// compatDirs names the working directories under the scratch directory.
//
//	a  trusted, with a directory name built to break a naive encoder: a dot, an
//	   underscore, a space and a non-ASCII letter (E-SLUG). Its instruction file
//	   imports a file from outside it, so the seeder's external-imports answer is
//	   exercised too (C1, #212)
//	b  trusted, plain
//	u  UNtrusted, outside the trust root, so the folder-trust dialog appears
//	i  trusted (the harness writes ONLY the trust answer itself) but never
//	   approved to import from outside, and its instruction file does, so the
//	   external-imports dialog appears (F-IMPORTS, #212). Outside the trust root,
//	   so the driver's own seeder — which would write both answers — refuses it
func compatDirs(scratch string) map[string]string {
	return map[string]string{
		"a": filepath.Join(scratch, "t", "c.d_e f", "ñ"),
		"b": filepath.Join(scratch, "t", "b"),
		"u": filepath.Join(scratch, "u"),
		"i": filepath.Join(scratch, "i"),
	}
}

// compatImportsInstructions is the instruction file directories "a" and "i"
// carry: one import of a file that lives outside the directory, which is what
// makes the runtime ask whether it may follow it. It is deliberately inert text
// — it reaches a model on every turn a check spends.
func compatImportsInstructions(shared string) string {
	return "This directory belongs to a compatibility check and holds no project.\n\n" +
		"The line below imports a file from outside this directory, so that the runtime has to have been told whether it may.\n\n" +
		"@" + shared + "\n"
}

// compatSharedImportText is what the imported file says: nothing that is an
// instruction.
const compatSharedImportText = "Nothing in this file is an instruction. It exists only to be imported.\n"

// findTmux locates the multiplexer the same way the service is told to: an
// explicit FLEET_TMUX_BIN, then PATH, then the usual package-manager prefixes
// (a non-interactive caller often has a bare PATH).
func findTmux(getenv func(string) string) (string, error) {
	if b := getenv("FLEET_TMUX_BIN"); b != "" {
		if _, err := os.Stat(b); err != nil {
			return "", fmt.Errorf("FLEET_TMUX_BIN %q: %w", b, err)
		}
		return b, nil
	}
	if p, err := exec.LookPath("tmux"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no multiplexer found (set FLEET_TMUX_BIN)")
}

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// muxScript is the wrapper's text. Every value is quoted; nothing is expanded.
func muxScript(env []string, tmuxBin, tmuxTmp, label string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nexec /usr/bin/env -i")
	for _, kv := range env {
		b.WriteString(" " + shQuote(kv))
	}
	b.WriteString(" " + shQuote("TMUX_TMPDIR="+tmuxTmp))
	b.WriteString(" " + shQuote(tmuxBin) + " -L " + shQuote(label) + " -f /dev/null \"$@\"\n")
	return b.String()
}

// watchdogScript covers the one case teardown cannot: the harness being killed
// outright (SIGKILL), which runs no cleanup of any kind. It waits for the
// harness to disappear, then removes what it left. It only ever removes paths
// it is handed, and refuses any that are not under a directory named cfc-….
const watchdogScript = `p=$1; shift
while kill -0 "$p" 2>/dev/null; do sleep 2; done
for d in "$@"; do
  case "$d" in */cfc-*|*-cfc-*) rm -rf "$d" ;; esac
done
s=${TMUX#*,}; kill "${s%%,*}" 2>/dev/null
exit 0
`

// startWorld builds the isolated environment: scratch directory, wrapper,
// private server, driver. On any failure it tears down what it built and
// returns the error, which the caller reports as "could not run" for every
// check that needs a session.
func (h *compatHarness) startWorld(ctx context.Context) (err error) {
	if h.world != nil {
		return nil
	}
	tmuxBin, err := findTmux(h.getenv)
	if err != nil {
		return err
	}
	store, err := compatStoreFrom(h.getenv)
	if err != nil {
		return err
	}
	base := "/tmp"
	if fi, e := os.Stat(base); e != nil || !fi.IsDir() {
		base = os.TempDir()
	}
	nonce := randomNonce()[:10]
	scratch := filepath.Join(base, "cfc-"+nonce)
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return fmt.Errorf("scratch directory: %w", err)
	}
	if real, e := filepath.EvalSymlinks(scratch); e == nil {
		scratch = real
	}
	w := &compatWorld{
		nonce: nonce, scratch: scratch, label: "cfc-" + nonce, tmuxBin: tmuxBin,
		store: store, resolved: h.cand.Resolved, dirs: compatDirs(scratch),
		extraArgs: map[string][]string{},
	}
	// The world is registered before anything else can fail, so that teardown
	// — deferred by RunCompat — always sees what needs removing.
	h.world = w
	defer func() {
		if err != nil {
			h.teardown()
		}
	}()

	tmuxTmp := filepath.Join(scratch, "m")
	if err := os.Mkdir(tmuxTmp, 0o700); err != nil {
		return err
	}
	w.socket = filepath.Join(tmuxTmp, fmt.Sprintf("tmux-%d", os.Getuid()), w.label)
	if _, e := os.Stat(w.socket); e == nil {
		return fmt.Errorf("refusing to start: %s already exists", w.socket)
	}
	if len(w.socket) > 100 {
		return fmt.Errorf("the socket path is too long for a unix socket (%d bytes): %s", len(w.socket), w.socket)
	}

	for role, dir := range w.dirs {
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
			return err
		}
		// A project-local setting: it is honoured even when the user's own
		// setting says otherwise, and without it every throwaway session would
		// register a bridge with an outside service.
		settings := filepath.Join(dir, ".claude", "settings.local.json")
		if err := os.WriteFile(settings, []byte("{\"remoteControlAtStartup\":false}\n"), 0o600); err != nil {
			return fmt.Errorf("settings for %s: %w", role, err)
		}
	}

	// The import the instruction files of "a" and "i" carry (#212). It lives in the
	// scratch directory itself, outside every working directory, and is not named
	// like an instruction file, so nothing loads it except by that import.
	w.sharedImport = filepath.Join(scratch, "shared.md")
	if err := os.WriteFile(w.sharedImport, []byte(compatSharedImportText), 0o600); err != nil {
		return err
	}
	for _, role := range []string{"a", "i"} {
		if err := os.WriteFile(filepath.Join(w.dirs[role], "CLAUDE.md"), []byte(compatImportsInstructions(w.sharedImport)), 0o600); err != nil {
			return fmt.Errorf("instruction file for %s: %w", role, err)
		}
	}

	w.ctxFile = filepath.Join(scratch, "ctx.md")
	if err := os.WriteFile(w.ctxFile, []byte("IMPORTANT: every reply you write, whatever else you are asked, must end with this exact token on its own final line: CFC-CTX-"+nonce+"\n"), 0o600); err != nil {
		return err
	}

	w.mux = filepath.Join(scratch, "mux")
	script := muxScript(compatBaseEnv(h.getenv), tmuxBin, tmuxTmp, w.label)
	if err := os.WriteFile(w.mux, []byte(script), 0o700); err != nil {
		return err
	}

	// The keeper session holds the server open and runs the watchdog.
	var cleanup []string
	cleanup = append(cleanup, scratch)
	for _, dir := range w.dirs {
		cleanup = append(cleanup, filepath.Join(store.projects, recordDirFor(dir)))
	}
	keeperArgs := append([]string{"new-session", "-d", "-s", compatKeeper,
		"-x", fmt.Sprint(compatPaneCols), "-y", fmt.Sprint(compatPaneRows), "--",
		"sh", "-c", watchdogScript, "sh", fmt.Sprint(os.Getpid())}, cleanup...)
	if out, e := w.tmux(ctx, keeperArgs...); e != nil {
		return fmt.Errorf("starting the private multiplexer server: %v: %s", e, strings.TrimSpace(out))
	}
	// remain-on-exit BEFORE any candidate exists, so a candidate that crashes
	// leaves its last screen behind as evidence instead of vanishing.
	for _, args := range [][]string{
		{"set-option", "-g", "remain-on-exit", "on"},
		{"set-option", "-g", "default-size", fmt.Sprintf("%dx%d", compatPaneCols, compatPaneRows)},
	} {
		if out, e := w.tmux(ctx, args...); e != nil {
			return fmt.Errorf("configuring the private server: %v: %s", e, strings.TrimSpace(out))
		}
	}

	// Prove the door leads where it should before trusting it with anything.
	got, e := w.tmux(ctx, "display-message", "-p", "#{socket_path}")
	if e != nil || strings.TrimSpace(got) != w.socket {
		return fmt.Errorf("isolation check failed: the server reports its socket as %q, expected %q (%v)",
			strings.TrimSpace(got), w.socket, e)
	}
	list, e := w.tmux(ctx, "list-sessions", "-F", "#{session_name}")
	if e != nil || strings.TrimSpace(list) != compatKeeper {
		return fmt.Errorf("isolation check failed: the private server lists %q, expected only %q (%v)",
			strings.TrimSpace(list), compatKeeper, e)
	}

	w.d = New("compat",
		WithBinary(w.mux),
		WithBareExec(),
		WithCommandBuilder(w.commandBuilder()),
		WithRecordRoot(store.projects),
		WithProcessSessionsRoot(store.sessions),
		WithTrustSeed(store.trustState, store.home, []string{filepath.Join(scratch, "t")}),
	)
	w.trustOnly = trustseed.NewTrustOnly(store.trustState, store.home, []string{w.dirs["i"]})
	h.logf("isolated multiplexer server %s (socket %s)", w.label, w.socket)
	return nil
}

// tmux runs one multiplexer command through the wrapper.
func (w *compatWorld) tmux(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, w.mux, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commandBuilder returns the driver's own command line for a session, with the
// candidate as argv[0]. It is the driver's real builder — the point is to check
// what it would run — changed in exactly two ways: the binary is the resolved
// candidate rather than the word `claude` (which a login shell would look up on
// PATH), and the session's name is passed with -n even though remote control is
// off, so naming can be checked without registering a bridge.
func (w *compatWorld) commandBuilder() CommandBuilder {
	return func(spec fleet.SessionSpec, contextFile string) []string {
		argv := claudeCodeCommand(spec, contextFile)
		argv[0] = w.resolved
		if spec.Name != "" && !containsArg(argv, "-n") {
			rest := append([]string{"-n", spec.Name}, argv[1:]...)
			argv = append([]string{argv[0]}, rest...)
		}
		return append(argv, w.extraArgs[spec.Name]...)
	}
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// create starts a session in one of the throwaway directories through the
// driver's real Create. The session is named after the run's nonce, so nothing
// here can be mistaken for (or collide with) a session that is not ours.
func (w *compatWorld) create(ctx context.Context, label, dirRole string, mutate func(*fleet.SessionSpec)) (fleet.SessionRef, error) {
	cwd, ok := w.dirs[dirRole]
	if !ok {
		return fleet.SessionRef{}, fmt.Errorf("no working directory %q", dirRole)
	}
	off := false
	spec := fleet.SessionSpec{
		Name:          w.label + "-" + label,
		Cwd:           fleet.AbsolutePath(cwd),
		Model:         "haiku",
		Effort:        "low",
		RemoteControl: &off,
	}
	if mutate != nil {
		mutate(&spec)
	}
	sess, err := w.d.Create(ctx, compatRequest, w.label+"-key-"+label, spec)
	if err != nil {
		return fleet.SessionRef{}, err
	}
	w.created = append(w.created, compatCreated{label: label, ref: sess.SessionRef, cwd: cwd, at: time.Now()})
	return sess.SessionRef, nil
}

// paneProcesses lists the process ids of the panes on OUR server. It asks the
// private server, never the process table, so it can only ever name our own
// sessions' processes.
func (w *compatWorld) paneProcesses(ctx context.Context) []int {
	out, err := w.tmux(ctx, "list-panes", "-a", "-F", "#{pane_pid}")
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(out) {
		var n int
		if _, err := fmt.Sscanf(f, "%d", &n); err == nil && n > 1 {
			pids = append(pids, n)
		}
	}
	return pids
}

// teardown removes everything the run made. It is safe to call more than once
// and safe when the world never started, runs on its own deadline so a
// cancelled run still cleans up, and never stops at the first error: each step
// is attempted and each failure is reported, because a teardown that gives up
// halfway is how a private server is left running.
func (h *compatHarness) teardown() {
	h.tearOnce.Do(func() {
		w := h.world
		if w == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), compatTeardownWait)
		defer cancel()
		fail := func(step string, err error) {
			h.tearErrs = append(h.tearErrs, fmt.Errorf("teardown %s: %w", step, err))
			h.logf("teardown %s: %v", step, err)
		}

		// Remember the processes and identities BEFORE the server goes, then
		// give them a chance to end on their own.
		var stragglers []ProcessIdentity
		if w.d != nil {
			for _, pid := range w.paneProcesses(ctx) {
				if id, err := w.d.processIdentityOfPID(ctx, pid); err == nil {
					stragglers = append(stragglers, id)
				}
			}
		}
		pids := make([]int, 0, len(stragglers))
		for _, s := range stragglers {
			pids = append(pids, s.PID)
		}

		h.settleLaunches(ctx, w)

		if _, err := os.Stat(w.mux); err == nil {
			if out, err := w.tmux(ctx, "kill-server"); err != nil && !strings.Contains(out, "no server running") &&
				!strings.Contains(out, "error connecting") && !strings.Contains(out, "No such file") {
				fail("kill-server", fmt.Errorf("%v: %s", err, strings.TrimSpace(out)))
			}
		}
		h.reapStragglers(ctx, w, stragglers, fail)

		// The socket lives under the scratch directory, but say so if it was
		// somehow left over rather than trusting the directory removal below.
		if _, err := os.Stat(w.socket); err == nil {
			if err := os.Remove(w.socket); err != nil {
				fail("socket", err)
			}
		}
		for _, c := range w.created {
			w.removeTranscripts(c.cwd, fail)
		}
		for role, dir := range w.dirs {
			if !containsNonce(dir, w.nonce) {
				fail("transcripts", fmt.Errorf("refusing to remove transcripts for %s: no nonce in its path", role))
				continue
			}
			w.removeTranscripts(dir, fail)
		}
		for _, pid := range pids {
			w.removeProcessRecord(pid, fail)
		}
		// Only the exact directory this run created.
		if w.scratch != "" && filepath.Base(w.scratch) == "cfc-"+w.nonce {
			if err := os.RemoveAll(w.scratch); err != nil {
				fail("scratch", err)
			}
		}
		h.logf("teardown finished (%d step problem(s))", len(h.tearErrs))
	})
}

// containsNonce guards every deletion: a path that does not carry this run's
// nonce is never removed.
func containsNonce(path, nonce string) bool { return strings.Contains(path, "cfc-"+nonce) }

// removeTranscripts deletes the throwaway working directory's transcript
// directory. The directory name is derived from a path that carries the nonce,
// so a real project's transcripts can never match.
func (w *compatWorld) removeTranscripts(cwd string, fail func(string, error)) {
	if !containsNonce(cwd, w.nonce) {
		return
	}
	dir := filepath.Join(w.store.projects, recordDirFor(cwd))
	if !containsNonce(dir, w.nonce) {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		fail("transcripts", err)
	}
}

// removeProcessRecord deletes a leftover per-process record, but only one whose
// own content says its working directory is under this run's scratch directory.
func (w *compatWorld) removeProcessRecord(pid int, fail func(string, error)) {
	path := filepath.Join(w.store.sessions, fmt.Sprintf("%d.json", pid))
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if !strings.Contains(string(b), w.scratch) {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fail("process record", err)
	}
}

// reapStragglers ends any of OUR panes' processes that outlived the server.
// A process is only signalled after its identity — pid AND start time — is
// verified unchanged, so a recycled pid is never touched.
func (h *compatHarness) reapStragglers(ctx context.Context, w *compatWorld, ids []ProcessIdentity, fail func(string, error)) {
	if w.d == nil || len(ids) == 0 {
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, id := range ids {
			if w.d.VerifyProcessIdentity(ctx, id) == nil {
				alive = true
			}
		}
		if !alive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, id := range ids {
		if w.d.VerifyProcessIdentity(ctx, id) != nil {
			continue // gone, or no longer the same process
		}
		if err := killPID(id.PID); err != nil && err != syscall.ESRCH {
			fail("straggler", fmt.Errorf("pid %d: %w", id.PID, err))
		}
	}
}

// addWorld registers the probe every live check needs: the isolated
// environment itself. It is stage 1, and any probe that touches a session
// declares it as a prerequisite.
func (h *compatHarness) addWorld(s *compat.Suite) {
	s.Probes = append(s.Probes, compat.Probe{
		ID: "world.up", Stage: 1, Budget: 60 * time.Second,
		Run: h.startWorld,
	})
}

// processIdentityOfPID reads a process's identity — pid and start time — the
// way ResolveProcessIdentity does, for a pid the harness already knows is one
// of its own panes' processes.
func (d *Driver) processIdentityOfPID(ctx context.Context, pid int) (ProcessIdentity, error) {
	started, err := d.processStartedAt(ctx, pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{PID: pid, StartedAt: started}, nil
}

// settleLaunches waits until the youngest session this run started is old
// enough to be killed without leaving a failed-start record behind (see
// compatSettleAge). It waits only when it has to, and never past the teardown's
// own deadline: a cleanup that hangs is worse than one that leaves a strike.
func (h *compatHarness) settleLaunches(ctx context.Context, w *compatWorld) {
	if h.settleAge <= 0 || len(w.created) == 0 {
		return
	}
	var youngest time.Time
	for _, c := range w.created {
		if c.at.After(youngest) {
			youngest = c.at
		}
	}
	wait := h.settleAge - time.Since(youngest)
	if wait <= 0 {
		return
	}
	h.logf("teardown: waiting %s for the youngest session to reach a safe age before it is stopped",
		wait.Round(100*time.Millisecond))
	select {
	case <-time.After(wait):
	case <-ctx.Done():
	}
}
