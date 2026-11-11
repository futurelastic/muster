package opencode

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #445: the sessions a restart cannot relaunch, and what they hold.

// recordWith writes a record with the given fields over the fixture's store.
func (f *scopedFixture) recordWith(r sessionRecord) {
	f.t.Helper()
	if err := (&recordStore{dir: f.storeDir}).write(&r); err != nil {
		f.t.Fatal(err)
	}
}

// snapshot is every file under dir as name -> content hash plus mode and size, so a
// test can say a call left the tree exactly as it found it.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, ierr := e.Info()
		if ierr != nil {
			return ierr
		}
		rel, _ := filepath.Rel(dir, p)
		if e.IsDir() {
			out[rel] = "dir"
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[rel] = fmt.Sprintf("%x %v %d", sha256.Sum256(raw), info.Mode(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ids(r UnknownReport) string {
	var out []string
	for _, s := range r.Sessions {
		out = append(out, s.ID)
	}
	return strings.Join(out, ",")
}

func TestUnknownSessions_CountsWhatARestartCannotRelaunch(t *testing.T) {
	f := newScopedFixture(t)
	day := 24 * time.Hour
	started := func(ago time.Duration) int64 { return time.Now().Add(-ago).UnixMilli() }
	env := []string{"MODEL_KEY"}

	// Left unknown by a restart: created with caller environment, no server of its own.
	f.recordWith(sessionRecord{ID: "ses_old", StartedAtMs: started(5 * day), EnvNames: env,
		Dir: f.dir("muster-opencode-1", day, map[string]string{"xdg/data/opencode/opencode.db": "1234567", "tmp/a": "123"})})
	f.recordWith(sessionRecord{ID: "ses_new", StartedAtMs: started(2 * time.Hour), EnvNames: env,
		Dir: f.dir("muster-opencode-2", time.Hour, map[string]string{"tmp/a": "12"})})

	// Resumed: its directory is held by a running server.
	f.recordWith(sessionRecord{ID: "ses_live", StartedAtMs: started(4 * day), EnvNames: env,
		Dir: f.dir("muster-opencode-4", day, map[string]string{"tmp/a": "1234"})})
	// A restart relaunches it (no caller environment), so it is listed as running or tried again.
	f.recordWith(sessionRecord{ID: "ses_plain", StartedAtMs: started(6 * day),
		Dir: f.dir("muster-opencode-5", day, map[string]string{"tmp/a": "1234"})})

	// A record that names no directory this driver made is counted but never walked.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "elsewhere"), []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.recordWith(sessionRecord{ID: "ses_foreign", StartedAtMs: started(day), EnvNames: env, Dir: outside})
	// A directory that is gone holds nothing.
	f.recordWith(sessionRecord{ID: "ses_gone", StartedAtMs: started(day + time.Hour), EnvNames: env,
		Dir: filepath.Join(f.root, "muster-opencode-9")})
	// A record that cannot be read is listed unknown by the service; here it is only counted.
	if err := os.WriteFile(filepath.Join(f.storeDir, "ses_broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := unknownSessionReport(f.storeDir, f.root, serversHolding("muster-opencode-4"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(rep), "ses_old,ses_gone,ses_foreign,ses_new"; got != want {
		t.Fatalf("counted %q; want %q (oldest first; a session a server holds, and one a restart relaunches, are not counted)", got, want)
	}
	if rep.Unreadable != 1 || rep.Uninspected != 0 {
		t.Errorf("Unreadable, Uninspected = %d, %d; want 1, 0", rep.Unreadable, rep.Uninspected)
	}
	by := map[string]UnknownSession{}
	for _, s := range rep.Sessions {
		by[s.ID] = s
	}
	if s := by["ses_old"]; !s.Measured || s.Partial || s.Bytes != 10 {
		t.Errorf("ses_old = %+v; want 10 measured bytes (the store and the scratch)", s)
	}
	if age := time.Since(by["ses_old"].StartedAt); age < 5*day || age > 5*day+time.Minute {
		t.Errorf("ses_old started %s ago; want about 5 days", age)
	}
	if s := by["ses_gone"]; !s.Measured || s.Bytes != 0 {
		t.Errorf("ses_gone = %+v; a directory that does not exist holds nothing", s)
	}
	if s := by["ses_foreign"]; s.Measured || s.Bytes != 0 {
		t.Errorf("ses_foreign = %+v; a directory this driver did not make is not walked", s)
	}
	if total, exact := rep.Bytes(); total != 12 || exact {
		t.Errorf("Bytes() = %d, %v; want 12 and not exact (one session has no figure)", total, exact)
	}
}

// muster #446: a session whose relaunch failed at the last start is counted from its
// record, a session of a stopped service that no start has failed is not.
func TestUnknownSessions_CountsASessionWhoseRelaunchFailedAtTheLastStart(t *testing.T) {
	f := newScopedFixture(t)
	day := 24 * time.Hour
	started := func(ago time.Duration) int64 { return time.Now().Add(-ago).UnixMilli() }
	failedAt := time.Now().Add(-time.Hour).UnixMilli()

	// Failed at the last start: no caller environment, and a mark saying the relaunch failed.
	f.recordWith(sessionRecord{ID: "ses_failed", StartedAtMs: started(3 * day), RelaunchFailedAtMs: failedAt,
		Dir: f.dir("muster-opencode-1", day, map[string]string{"xdg/data/opencode/opencode.db": "123456", "tmp/a": "1234"})})
	// The stopped service's session: nothing in its record says a start failed it, so it is
	// the session a restart relaunches, and it is not counted however long it has been down.
	f.recordWith(sessionRecord{ID: "ses_stopped", StartedAtMs: started(9 * day),
		Dir: f.dir("muster-opencode-2", day, map[string]string{"tmp/a": "12"})})
	// Marked, but a running server holds its directory: the service is up and serves it.
	f.recordWith(sessionRecord{ID: "ses_serving", StartedAtMs: started(8 * day), RelaunchFailedAtMs: failedAt,
		Dir: f.dir("muster-opencode-3", day, map[string]string{"tmp/a": "12"})})
	// Both: it needs re-creation whatever else is wrong, so it is the first class.
	f.recordWith(sessionRecord{ID: "ses_both", StartedAtMs: started(2 * day), RelaunchFailedAtMs: failedAt, EnvNames: []string{"MODEL_KEY"},
		Dir: f.dir("muster-opencode-4", day, map[string]string{"tmp/a": "1"})})
	// A caller-environment session beside them, as before.
	f.recordWith(sessionRecord{ID: "ses_env", StartedAtMs: started(day), EnvNames: []string{"MODEL_KEY"},
		Dir: f.dir("muster-opencode-5", day, map[string]string{"tmp/a": "12"})})

	rep, err := unknownSessionReport(f.storeDir, f.root, serversHolding("muster-opencode-3"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(rep), "ses_failed,ses_both,ses_env"; got != want {
		t.Fatalf("counted %q; want %q (oldest first; a session no start failed, and one a server holds, are not counted)", got, want)
	}
	by := map[string]UnknownSession{}
	for _, s := range rep.Sessions {
		by[s.ID] = s
	}
	if s := by["ses_failed"]; !s.RelaunchFailed || !s.Measured || s.Bytes != 10 || time.Since(s.StartedAt) < 3*day {
		t.Errorf("ses_failed = %+v; want the failed class, with its age and its 10 measured bytes", s)
	}
	if by["ses_both"].RelaunchFailed || by["ses_env"].RelaunchFailed {
		t.Errorf("a caller-environment session is reported as a failed relaunch: %+v, %+v", by["ses_both"], by["ses_env"])
	}
	if n := rep.RelaunchFailedCount(); n != 1 {
		t.Errorf("RelaunchFailedCount = %d; want 1", n)
	}
}

func TestUnknownSessions_ReadsAndRemovesNothing(t *testing.T) {
	f := newScopedFixture(t)
	for i, id := range []string{"ses_a", "ses_b", "ses_c"} {
		f.recordWith(sessionRecord{ID: id, StartedAtMs: time.Now().Add(-time.Duration(i+1) * time.Hour).UnixMilli(), EnvNames: []string{"K"},
			Dir: f.dir(fmt.Sprintf("muster-opencode-%d", i+1), time.Hour, map[string]string{"xdg/data/opencode/opencode.db": "the conversation", "tmp/x": "scratch"})})
	}
	// Even a directory nobody names, and every file in a record's directory, stays.
	f.dir("muster-opencode-8", 5*time.Hour, remnantXDG)
	before := snapshot(t, filepath.Dir(f.root))

	rep, err := unknownSessionReport(f.storeDir, f.root, noServers, time.Minute)
	if err != nil || len(rep.Sessions) != 3 {
		t.Fatalf("report = %+v, %v; want the three sessions", rep, err)
	}
	if after := snapshot(t, filepath.Dir(f.root)); !equalMaps(before, after) {
		t.Fatalf("the state directory changed: the report must never remove or rewrite a record or a directory\nbefore: %v\nafter:  %v", before, after)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestUnknownSessions_AnUnreadableScanIsNotAnEmptyAnswer(t *testing.T) {
	f := newScopedFixture(t)
	f.recordWith(sessionRecord{ID: "ses_a", EnvNames: []string{"K"}, Dir: f.dir("muster-opencode-1", time.Hour, remnantTmp)})
	unsupported := func(string) ([]serverProc, int, error) { return nil, 0, ErrScanUnsupported }
	rep, err := unknownSessionReport(f.storeDir, f.root, unsupported, time.Minute)
	if !errors.Is(err, ErrScanUnsupported) || len(rep.Sessions) != 0 {
		t.Fatalf("report = %+v, %v; where processes cannot be listed nothing is counted, and the error says why", rep, err)
	}

	// A process that looks like a server but cannot be inspected may hold a counted
	// session's directory: the report carries the count so the row can say so.
	blind := func(string) ([]serverProc, int, error) { return nil, 2, nil }
	rep, err = unknownSessionReport(f.storeDir, f.root, blind, time.Minute)
	if err != nil || rep.Uninspected != 2 || len(rep.Sessions) != 1 {
		t.Fatalf("report = %+v, %v; want the session and Uninspected 2", rep, err)
	}
}

func TestUnknownSessions_MeasuringIsBounded(t *testing.T) {
	f := newScopedFixture(t)
	for i := 1; i <= 3; i++ {
		f.recordWith(sessionRecord{ID: fmt.Sprintf("ses_%d", i), StartedAtMs: int64(i), EnvNames: []string{"K"},
			Dir: f.dir(fmt.Sprintf("muster-opencode-%d", i), time.Hour, remnantTmp)})
	}
	// No time to measure: every session is still reported, none with a figure. The budget
	// is zero, not "very small": a clock that has not ticked between two reads (Windows's
	// is coarse) leaves a one-nanosecond budget a nanosecond to spend, and the walk runs.
	rep, err := unknownSessionReport(f.storeDir, f.root, noServers, 0)
	if err != nil || len(rep.Sessions) != 3 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	for _, s := range rep.Sessions {
		if s.Measured {
			t.Errorf("%s was measured with no time left", s.ID)
		}
	}
	if _, exact := rep.Bytes(); exact {
		t.Error("a total with unmeasured sessions in it was called exact")
	}
}

func TestOwnedScopedDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opencode", "dirs")
	sep := string(filepath.Separator)
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"a session directory of the root", root + sep + "muster-opencode-12", true},
		{"with a trailing separator", root + sep + "muster-opencode-12" + sep, true},
		{"the root itself", root, false},
		{"deeper than a session directory", root + sep + "muster-opencode-12" + sep + "tmp", false},
		{"a sibling of the root", root + "2" + sep + "muster-opencode-12", false},
		{"another name under the root", root + sep + "other-12", false},
		{"a path that climbs out", root + sep + ".." + sep + "muster-opencode-12", false},
		{"relative", "muster-opencode-12", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := ownedScopedDir(root, tc.dir); got != tc.want {
			t.Errorf("%s: ownedScopedDir(%q) = %v; want %v", tc.name, tc.dir, got, tc.want)
		}
	}
}

// A real restart, not a hand-built record: the session created with caller
// environment is listed `unknown` by the restarted service and is the one the
// report counts; the session that was relaunched is not.
func TestUnknownSessions_AfterARealRestart(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	withEnv := mustCreate(t, d1, "k-env", fleet.SessionSpec{Name: "needs-env", Env: map[string]string{"MODEL_KEY": "x-value"}})
	plain := mustCreate(t, d1, "k-plain", fleet.SessionSpec{Name: "plain"})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	all := listed(t, d2)
	if all[withEnv.ID].State.Status != fleet.StatusUnknown {
		t.Fatalf("the session created with caller environment is %q after the restart; want unknown", all[withEnv.ID].State.Status)
	}
	if all[plain.ID].State.Status == fleet.StatusUnknown {
		t.Fatalf("the plain session is unknown after the restart (%s); it should have been relaunched", all[plain.ID].State.Evidence)
	}

	storeDir, root := stateLayout(state)
	plainRec := (&recordStore{dir: storeDir}).load(plain.ID)
	if plainRec == nil {
		t.Fatal("no record for the relaunched session")
	}
	// The relaunched session's server holds its directory, as a real scan would show.
	running := serversHolding(filepath.Base(plainRec.Dir))
	recPath := filepath.Join(storeDir, withEnv.ID+".json")
	recBefore, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := unknownSessionReport(storeDir, root, running, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rep); got != withEnv.ID {
		t.Fatalf("counted %q; want only %q (the session that resumed is not counted)", got, withEnv.ID)
	}
	if s := rep.Sessions[0]; !s.Measured || s.Bytes <= 0 || s.StartedAt.IsZero() {
		t.Errorf("session = %+v; want an age and a size above zero (a created session's directory holds its runtime files)", s)
	}
	// Reporting changed nothing: the record and both directories are still there.
	if got := recordFiles(t, state); len(got) != 2 {
		t.Errorf("records after the report: %v; want both", got)
	}
	dirs, _ := os.ReadDir(root)
	var left []string
	for _, e := range dirs {
		left = append(left, e.Name())
	}
	sort.Strings(left)
	if len(left) != 2 {
		t.Errorf("directories after the report: %v; want both", left)
	}
	// A relaunched server is live and writes into its own directory, so the whole tree is
	// not compared here (TestUnknownSessions_ReadsAndRemovesNothing does that on a still
	// one); the counted session's record is the thing the report must leave alone.
	if recAfter, err := os.ReadFile(recPath); err != nil || string(recAfter) != string(recBefore) {
		t.Errorf("the record of the counted session changed or went missing (%v)", err)
	}
}
