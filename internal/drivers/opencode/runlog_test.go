package opencode

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #420: the runtime logs every bash command it evaluates, in full, at its
// default level — so a credential passed on a command line sits in plaintext in
// the session's log. Two layers answer it: the level the runtime is started with,
// and the log's own mode. These pin both without the real binary; the
// real-runtime check that the secret really stays out is
// TestIntegration_ARuntimeLogHoldsNoCommandText (opt-in, runlog_integration_test.go).

// flagValue returns the argument that follows flag in args.
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestBuildServeCmd_CapsTheRuntimeLogBelowInfo(t *testing.T) {
	for name, wrap := range map[string][]string{"plain": nil, "sandboxed": {sandboxExecPath, "-p", "(version 1)"}} {
		t.Run(name, func(t *testing.T) {
			cmd := buildServeCmd("/bin/true", "", 1234, "muster", "cred", nil, wrap)
			level, ok := flagValue(cmd.Args, "--log-level")
			if !ok {
				t.Fatalf("--log-level missing from argv %v; the runtime would log every bash command at INFO (#420)", cmd.Args)
			}
			if level != "WARN" {
				t.Fatalf("--log-level = %q, want WARN: INFO and DEBUG both carry the evaluated command (#420)", level)
			}
			// The cap must not take the log away: the error-name capture (#288) reads it.
			printLogs := false
			for _, a := range cmd.Args {
				printLogs = printLogs || a == "--print-logs"
			}
			if !printLogs {
				t.Fatalf("--print-logs missing from argv %v; the error-name capture has no log to read (#288)", cmd.Args)
			}
		})
	}
}

func TestCreate_TheRuntimeStartsWithTheLogCapped(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k-cap", fleet.SessionSpec{})
	v := inside(t, d, sess.ID)
	if level, ok := flagValue(v.Args, "--log-level"); !ok || level != "WARN" {
		t.Fatalf("the runtime the driver started got argv %v, want --log-level WARN (#420)", v.Args)
	}
}

func TestPrivateRuntimeLog_CreatesAnOwnerOnlyLogBeforeTheRuntimeRuns(t *testing.T) {
	skipOnWindows(t, "POSIX modes")
	dir := t.TempDir()
	if err := privateRuntimeLog(dir); err != nil {
		t.Fatal(err)
	}
	wantMode(t, runtimeLogDir(dir), 0o700)
	wantMode(t, filepath.Join(runtimeLogDir(dir), runtimeLogName), 0o600)
}

func TestCreate_TheRuntimesLogIsOwnerOnly(t *testing.T) {
	skipOnWindows(t, "POSIX modes")
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k-priv", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	wantMode(t, runtimeLogDir(known.srv.dir), 0o700)
	wantMode(t, filepath.Join(runtimeLogDir(known.srv.dir), runtimeLogName), 0o600)
}

// A session directory an earlier start left holds a log the runtime wrote 0644.
// The next start tightens it in place and keeps what it holds.
func TestPrivateRuntimeLog_TightensALogAnEarlierStartLeft(t *testing.T) {
	skipOnWindows(t, "POSIX modes")
	dir := t.TempDir()
	logDir := runtimeLogDir(dir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const history = "timestamp=x level=ERROR message=earlier\n"
	named := filepath.Join(logDir, runtimeLogName)
	other := filepath.Join(logDir, "2026-11-07T000000.log")
	for _, p := range []string{named, other} {
		if err := os.WriteFile(p, []byte(history), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := privateRuntimeLog(dir); err != nil {
		t.Fatal(err)
	}
	wantMode(t, logDir, 0o700)
	for _, p := range []string{named, other} {
		wantMode(t, p, 0o600)
		if got, err := os.ReadFile(p); err != nil || string(got) != history {
			t.Errorf("%s = %q, %v; the log an earlier start wrote must survive the next one", filepath.Base(p), got, err)
		}
	}
}

func wantMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %#o, want %#o", path, got, want)
	}
}
