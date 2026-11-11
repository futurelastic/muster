package opencode

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #282: sessions are found again after the service restarts.

// newStateDriver builds a driver over stateDir. It does NOT register a shutdown:
// each test decides whether the "restart" is a clean stop or a crash.
func newStateDriver(t *testing.T, stateDir string, extra ...Option) *Driver {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts := append([]Option{WithBinary(exe), WithStateDir(stateDir), withoutStraySweep(), withStatusTTL(0, 0)}, extra...)
	d, err := New(context.Background(), "test-machine", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func listed(t *testing.T, d *Driver) map[string]fleet.Session {
	t.Helper()
	col, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := map[string]fleet.Session{}
	for _, s := range col.Items() {
		out[s.ID] = s
	}
	return out
}

func TestResume_CapabilityFollowsTheStateDirectory(t *testing.T) {
	without, _ := newIsolatedDriver(t)
	if without.Capabilities().SupportsResume {
		t.Error("SupportsResume = true with no state directory; nothing is remembered")
	}
	with := newStateDriver(t, t.TempDir())
	t.Cleanup(func() { _ = with.Shutdown() })
	if !with.Capabilities().SupportsResume {
		t.Error("SupportsResume = false with a state directory")
	}
	f := newFakeServer(t)
	shared, err := newDriverWithOptions(t, f, WithStateDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if shared.Capabilities().SupportsResume {
		t.Error("SupportsResume = true in shared mode, where the server is not this driver's to restart")
	}
}

func TestResume_SessionSurvivesARestart(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	cwd := t.TempDir()
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Name: "survivor", Agent: "build"})
	before := inside(t, d1, sess.ID)
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok {
		t.Fatalf("the session is not listed after the restart")
	}
	if got.Name != "survivor" || string(got.Cwd) != cwd || string(got.Agent) != "build" {
		t.Errorf("listed as %+v; want the name, cwd and agent it was created with", got)
	}
	if got.State.Status != fleet.StatusIdle {
		t.Errorf("status = %q (%s); want idle: the session is readable again", got.State.Status, got.State.Evidence)
	}
	st, err := d2.State(context.Background(), testReq, got.SessionRef)
	if err != nil || st.Status != fleet.StatusIdle {
		t.Fatalf("State = %+v, %v; want idle", st, err)
	}
	after := inside(t, d2, sess.ID)
	if after.Pid == before.Pid {
		t.Error("the same pid after a restart: the session was not relaunched in a process of its own")
	}
	// The relaunched process works in the same directory and holds the same
	// session directory, with the same scoped environment.
	if after.Cwd != before.Cwd {
		t.Errorf("cwd = %q; want %q", after.Cwd, before.Cwd)
	}
	if b, a := before.vars()["XDG_DATA_HOME"], after.vars()["XDG_DATA_HOME"]; a == "" || a != b {
		t.Errorf("XDG_DATA_HOME = %q; want the one the session was created with (%q)", a, b)
	}
	if _, err := d2.Send(context.Background(), testReq, got.SessionRef, "hello", driver.SendOptions{Submit: true}); err != nil {
		t.Errorf("Send to a relaunched session: %v", err)
	}
	if _, err := d2.Close(context.Background(), testReq, got.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if left := recordFiles(t, state); len(left) != 0 {
		t.Errorf("records left after close: %v", left)
	}
	if _, err := os.Stat(after.vars()["XDG_DATA_HOME"]); err == nil {
		t.Error("the session's directory is still there after close")
	}
}

func recordFiles(t *testing.T, state string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(state, "opencode", "sessions"))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestResume_CallerEnvIsNeverStoredAndTheSessionIsUnknown(t *testing.T) {
	const secret = "sekrit-value-d41d8cd98f00b204"
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "withenv", Env: map[string]string{"MODEL_KEY": secret}})
	srvPass := func() string { k, _ := d1.wasSeen(sess.ID); return k.srv.password }()
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// Nothing under the state directory — record or session directory — holds the
	// value, or the server credential.
	var names []string
	_ = filepath.WalkDir(state, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			names = append(names, p)
			raw, _ := os.ReadFile(p)
			if strings.Contains(string(raw), secret) || strings.Contains(string(raw), srvPass) {
				t.Errorf("%s holds a secret", p)
			}
		}
		return nil
	})
	rec, err := os.ReadFile(filepath.Join(state, "opencode", "sessions", sess.ID+".json"))
	if err != nil {
		t.Fatalf("no record written (files: %v): %v", names, err)
	}
	if !strings.Contains(string(rec), "MODEL_KEY") {
		t.Error("the record does not carry the NAME of the variable the create was given")
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok {
		t.Fatal("the session is not listed after the restart")
	}
	if got.State.Status != fleet.StatusUnknown {
		t.Fatalf("status = %q; want unknown", got.State.Status)
	}
	if !strings.Contains(got.State.Evidence, "re-creation") || !strings.Contains(got.State.Evidence, "MODEL_KEY") {
		t.Errorf("reason = %q; want it to say the session needs re-creation and name the variable", got.State.Evidence)
	}
	if k, _ := d2.wasSeen(sess.ID); k.srv != nil {
		t.Error("a process was started for a session that cannot be relaunched faithfully")
	}
	_, err = d2.Send(context.Background(), testReq, got.SessionRef, "hi", driver.SendOptions{Submit: true})
	if kindOfErr(err) != fleet.ErrorConflict {
		t.Errorf("Send to an unknown session: %v; want a conflict naming the reason", err)
	}
	if _, err := d2.Close(context.Background(), testReq, got.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(recordFiles(t, state)) != 0 {
		t.Error("the record outlived Close")
	}
	if left, _ := os.ReadDir(filepath.Join(state, "opencode", "dirs")); len(left) != 0 {
		t.Errorf("the session directory outlived Close: %v", left)
	}
	if _, ok := listed(t, d2)[sess.ID]; ok {
		t.Error("the session is still listed after Close")
	}
}

func TestResume_AnInterruptedTurnIsNotReportedIdle(t *testing.T) {
	cases := []struct {
		name string
		msgs []wireMessage
		cut  bool
	}{
		{"unfinished assistant message", []wireMessage{{Info: wireMessageInfo{Role: "assistant"}}}, true},
		{"prompt with no answer", []wireMessage{{Info: wireMessageInfo{Role: "user"}}}, true},
		{"finished turn", []wireMessage{{Info: func() wireMessageInfo {
			i := wireMessageInfo{Role: "assistant"}
			i.Time.Completed = 5
			return i
		}()}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			d1 := newStateDriver(t, state)
			sess := mustCreate(t, d1, "k1", fleet.SessionSpec{})
			known, _ := d1.wasSeen(sess.ID)
			raw, _ := json.Marshal(tc.msgs)
			if err := os.WriteFile(filepath.Join(known.srv.dir, "xdg", "data", "fake-messages.json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			_ = d1.Shutdown()

			d2 := newStateDriver(t, state)
			t.Cleanup(func() { _ = d2.Shutdown() })
			ref := fleet.SessionRef{Machine: "test-machine", ID: sess.ID}
			st, err := d2.State(context.Background(), testReq, ref)
			if err != nil {
				t.Fatal(err)
			}
			inList := listed(t, d2)[sess.ID].State
			for where, s := range map[string]fleet.SessionState{"State": st, "List": inList} {
				if tc.cut {
					if s.LastTurn == nil || s.LastTurn.Outcome != "interrupted" || !s.LastTurn.Retryable {
						t.Errorf("%s: lastTurn = %+v; want an interrupted, retryable turn", where, s.LastTurn)
					}
				} else if s.LastTurn != nil {
					t.Errorf("%s: lastTurn = %+v; the turn had finished", where, s.LastTurn)
				}
			}
			if tc.cut {
				if _, err := d2.Send(context.Background(), testReq, ref, "continue", driver.SendOptions{Submit: true}); err != nil {
					t.Fatal(err)
				}
				if st, _ := d2.State(context.Background(), testReq, ref); st.LastTurn != nil {
					t.Errorf("lastTurn = %+v after a new turn was sent; it superseded the interrupted one", st.LastTurn)
				}
			}
		})
	}
}

func TestResume_ACrashedServicesServerIsReapedBeforeRelaunch(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d1.Shutdown() })
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{})
	oldPid := inside(t, d1, sess.ID).Pid
	// No Shutdown: the service "crashed", and its server is still running.
	if !alive(oldPid) {
		t.Fatal("the first server is not running")
	}
	// Where the service's death takes its servers with it (Windows: the job is
	// kill-on-close), make it happen; elsewhere this does nothing and the
	// reaping below is what stops the old server.
	simulateCrash(d1)
	// The start-up sweep is off so it is the reap of the RECORDED server that acts
	// (muster #398); the sweep has its own tests.
	d2 := newStateDriver(t, state, withoutStartSweep())
	t.Cleanup(func() { _ = d2.Shutdown() })
	waitGone(t, "the previous service's server", oldPid)
	if got := listed(t, d2)[sess.ID].State.Status; got != fleet.StatusIdle {
		t.Errorf("status = %q; want idle", got)
	}
	killGroupOf(oldPid)
}

func TestResume_ASessionThatCannotBeFoundIsStillListedWithTheReason(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	gone := mustCreate(t, d1, "k1", fleet.SessionSpec{})
	missingDir := mustCreate(t, d1, "k2", fleet.SessionSpec{})
	kGone, _ := d1.wasSeen(gone.ID)
	kMissing, _ := d1.wasSeen(missingDir.ID)
	_ = d1.Shutdown()
	// The runtime's store lost one session; the other's directory is deleted.
	_ = os.Remove(filepath.Join(kGone.srv.dir, "xdg", "data", "fake-session.json"))
	_ = os.RemoveAll(kMissing.srv.dir)
	if err := os.WriteFile(filepath.Join(state, "opencode", "sessions", "ses_garbled.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	all := listed(t, d2)
	for id, want := range map[string]string{
		gone.ID:       "no longer has this session",
		missingDir.ID: "is gone",
		"ses_garbled": "could not be used",
	} {
		s, ok := all[id]
		if !ok {
			t.Errorf("%s is not listed: a restart must never silently drop a session", id)
			continue
		}
		if s.State.Status != fleet.StatusUnknown || !strings.Contains(s.State.Evidence, want) {
			t.Errorf("%s: %q / %q; want unknown mentioning %q", id, s.State.Status, s.State.Evidence, want)
		}
	}
	// A session the runtime lost is kept on disk for the next start to retry.
	if _, err := os.Stat(filepath.Join(state, "opencode", "sessions", gone.ID+".json")); err != nil {
		t.Errorf("the record of a session that could not be relaunched was removed: %v", err)
	}
}

// muster #446: a start that lists a session unknown because its relaunch failed says so
// on the session's record, and the first start that relaunches it takes that off, so the
// doctor can count the session from disk and stop counting it once it works.
func TestResume_AFailedRelaunchIsMarkedOnTheRecordAndClearedByASuccessfulOne(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	broken := mustCreate(t, d1, "k1", fleet.SessionSpec{})
	healthy := mustCreate(t, d1, "k2", fleet.SessionSpec{})
	kBroken, _ := d1.wasSeen(broken.ID)
	store := filepath.Join(kBroken.srv.dir, "xdg", "data", "fake-session.json")
	saved, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	_ = d1.Shutdown()
	rec := func(id string) *sessionRecord {
		t.Helper()
		storeDir, _ := stateLayout(state)
		r := (&recordStore{dir: storeDir}).load(id)
		if r == nil {
			t.Fatalf("no record for %s", id)
		}
		return r
	}
	if rec(broken.ID).RelaunchFailedAtMs != 0 {
		t.Fatal("a record carries a failed relaunch before any restart")
	}

	// The runtime's store lost the session: the relaunch fails, the session is listed unknown.
	if err := os.Remove(store); err != nil {
		t.Fatal(err)
	}
	// The real clock, not an injected one: the driver builds its relaunch deadline from
	// d.now(), so a clock that stands still in the past would expire every relaunch at once.
	before := time.Now()
	d2 := newStateDriver(t, state)
	if got := listed(t, d2)[broken.ID].State.Status; got != fleet.StatusUnknown {
		t.Fatalf("the session whose store is gone is %q after the restart; want unknown", got)
	}
	firstFail := rec(broken.ID).RelaunchFailedAtMs
	if firstFail < before.UnixMilli() || firstFail > time.Now().UnixMilli() {
		t.Errorf("RelaunchFailedAtMs = %d; want the time of the start that failed it (between %d and now)", firstFail, before.UnixMilli())
	}
	if h := listed(t, d2)[healthy.ID]; h.State.Status == fleet.StatusUnknown || rec(healthy.ID).RelaunchFailedAtMs != 0 {
		t.Errorf("the session that relaunched is %q (%s), mark %d; want running and unmarked", h.State.Status, h.State.Evidence, rec(healthy.ID).RelaunchFailedAtMs)
	}
	_ = d2.Shutdown()

	// A second start that fails the same way keeps the time of the first: it is written once.
	time.Sleep(5 * time.Millisecond)
	d3 := newStateDriver(t, state)
	if got := rec(broken.ID).RelaunchFailedAtMs; got != firstFail {
		t.Errorf("RelaunchFailedAtMs = %d after a second failed start; want the first one's %d", got, firstFail)
	}
	_ = d3.Shutdown()

	// The cause is fixed; the next start relaunches the session and the mark comes off.
	if err := os.WriteFile(store, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	d4 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d4.Shutdown() })
	if got := listed(t, d4)[broken.ID].State.Status; got == fleet.StatusUnknown {
		t.Fatalf("the session is still unknown once its store is back")
	}
	if got := rec(broken.ID).RelaunchFailedAtMs; got != 0 {
		t.Errorf("RelaunchFailedAtMs = %d after a start that relaunched it; want 0", got)
	}
}

// muster #446: the mark holds a time and nothing else, and a record written before it
// existed reads as no failed start.
func TestResume_TheFailedRelaunchMarkHoldsOnlyATime(t *testing.T) {
	rec := sessionRecord{Version: recordVersion, ID: "ses_a", StartedAtMs: 1, RelaunchFailedAtMs: 1762851600000}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"relaunchFailedAtMs":1762851600000`) {
		t.Errorf("record = %s; want the mark as one number", raw)
	}
	var old sessionRecord
	if err := json.Unmarshal([]byte(`{"version":1,"id":"ses_old","startedAtMs":1}`), &old); err != nil || old.RelaunchFailedAtMs != 0 {
		t.Errorf("a record from before the mark: %+v, %v; want no failed start", old, err)
	}
	clear, _ := json.Marshal(sessionRecord{Version: recordVersion, ID: "ses_b"})
	if strings.Contains(string(clear), "relaunchFailed") {
		t.Errorf("a record with no failed start still writes the field: %s", clear)
	}
}

// muster #437: what the create asked for is kept with the session's record, so a
// session relaunched after a restart still reports its model pin — as requested,
// and resolved from the runtime's own record once a read asks for it.
func TestResume_ThePinnedModelSurvivesARestartAndResolvesOnARead(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "pinned", Model: "anthropic/claude"})
	if rec := d1.store.load(sess.ID); rec == nil || rec.SpecModel != "anthropic/claude" {
		t.Fatalf("the session's record = %+v, want SpecModel anthropic/claude", rec)
	}
	known, _ := d1.wasSeen(sess.ID)
	raw, _ := json.Marshal([]wireMessage{{Info: wireMessageInfo{Role: "assistant", ProviderID: "anthropic", ModelID: "claude"}}})
	if err := os.WriteFile(filepath.Join(known.srv.dir, "xdg", "data", "fake-messages.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got := readOne(t, d2, sess.ID)
	if got.Pins == nil || got.Pins.Model == nil {
		t.Fatalf("after the restart Pins = %+v, want the Model pin", got.Pins)
	}
	if m := got.Pins.Model; m.Requested != "anthropic/claude" || m.Honoured == nil || !*m.Honoured || m.Applied != "anthropic/claude" {
		t.Errorf("model pin = %+v, want requested and applied anthropic/claude, honoured", m)
	}
	if got.Model != "anthropic/claude" {
		t.Errorf("Model = %q, want anthropic/claude", got.Model)
	}
}

// A record written before the model was kept has no request to report: such a
// session reports no model pin after a restart, rather than one it cannot know.
func TestResume_ARecordWithoutAModelReportsNoModelPin(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "old"})
	if rec := d1.store.load(sess.ID); rec == nil || rec.SpecModel != "" {
		t.Fatalf("the session's record = %+v, want no SpecModel", rec)
	}
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	if got := readOne(t, d2, sess.ID); got.Pins != nil || got.Model != "" {
		t.Errorf("Pins = %+v, Model = %q, want neither for a session that pinned nothing", got.Pins, got.Model)
	}
}
