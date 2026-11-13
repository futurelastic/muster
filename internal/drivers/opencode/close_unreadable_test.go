package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #473: in the isolated mode a session IS its process, and a server that
// answers 500 to everything is the session its holder most needs to be able to
// remove. These tests run real per-session processes (fakeopencode_test.go); the
// one that breaks does so after its create, from a marker in its own directory.

// breakServer makes the session's own server answer 500 to everything but the
// test-only endpoints, as a runtime does whose sandbox lost its directory.
func breakServer(t *testing.T, cwd string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cwd, ".fake-fail-all"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClose_ASessionWhoseServerAnswers500IsStillClosed(t *testing.T) {
	d, root := newIsolatedDriver(t)
	type one struct {
		sess fleet.Session
		cwd  string
		pid  int
	}
	var all []one
	for _, key := range []string{"s1", "s2", "s3"} {
		cwd := t.TempDir()
		sess := mustCreate(t, d, key, fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
		all = append(all, one{sess: sess, cwd: cwd, pid: inside(t, d, sess.ID).Pid})
	}
	bad := all[1]
	known, _ := d.wasSeen(bad.sess.ID)
	breakServer(t, bad.cwd)

	// The failure is the session's own fact, and every session is still listed.
	col, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(col.Items()) != 3 {
		t.Fatalf("List = %d sessions, want all 3 listed", len(col.Items()))
	}
	for _, s := range col.Items() {
		if got, want := s.State.Status == fleet.StatusUnknown, s.ID == bad.sess.ID; got != want {
			t.Errorf("session %s: unknown = %v, want %v (%s)", s.ID, got, want, s.State.Evidence)
		}
	}

	// A close that was told which session it meant still goes ahead.
	startedAt := known.startedAt
	req := testReq
	req.Expect.StartedAt = &startedAt
	ack, err := d.Close(context.Background(), req, bad.sess.SessionRef)
	if err != nil {
		t.Fatalf("Close of the session whose server answers 500: %v", err)
	}
	if !ack.Accepted {
		t.Error("Accepted = false")
	}
	if !strings.Contains(ack.Note, "confirmation was not available") || !strings.Contains(ack.Note, "500") {
		t.Errorf("the ack must say the runtime's own confirmation was not available, and why: note = %q", ack.Note)
	}
	waitGone(t, "the broken session's server", bad.pid)
	if _, err := os.Stat(known.srv.dir); !os.IsNotExist(err) {
		t.Errorf("the broken session's directory survived Close: %v", err)
	}
	if _, err := d.State(context.Background(), testReq, bad.sess.SessionRef); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("State after Close = %v, want ErrNoSuchSession", err)
	}
	if d.Holds(bad.sess.ID) {
		t.Error("the driver still holds the closed session")
	}

	// A second close, and an id nobody knows, are still a plain "no such session".
	if _, err := d.Close(context.Background(), testReq, bad.sess.SessionRef); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("second Close = %v, want ErrNoSuchSession", err)
	}
	if _, err := d.Close(context.Background(), testReq, fleet.SessionRef{ID: "ses_nobody_knows"}); !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("Close of an unknown id = %v, want ErrNoSuchSession", err)
	}

	// The other two sessions were not touched, and close the ordinary way: no note.
	for _, o := range []one{all[0], all[2]} {
		if !alive(o.pid) {
			t.Errorf("session %s's server was stopped by a close of another", o.sess.ID)
		}
		ack, err := d.Close(context.Background(), testReq, o.sess.SessionRef)
		if err != nil {
			t.Fatalf("Close(%s): %v", o.sess.ID, err)
		}
		if ack.Note != "" {
			t.Errorf("an ordinary close carries no note: %q", ack.Note)
		}
		waitGone(t, "a healthy session's server", o.pid)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("session root is not empty after every close: %v", entries)
	}
}

// The caller's belief about which session it meant is still checked against the
// driver's record when the runtime cannot be asked: closing "the session started
// at T" is not licence to close one started at another time.
func TestClose_AServerThatAnswers500StillHonoursTheCallersExpectation(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	cwd := t.TempDir()
	sess := mustCreate(t, d, "s1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	breakServer(t, cwd)

	known, _ := d.wasSeen(sess.ID)
	wrong := known.startedAt.Add(-time.Second)
	req := testReq
	req.Expect.StartedAt = &wrong
	if _, err := d.Close(context.Background(), req, sess.SessionRef); !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Fatalf("Close with a start time that is not the session's = %v, want ErrAmbiguousTarget", err)
	}
	if !d.Holds(sess.ID) {
		t.Error("a refused close dropped the record")
	}
}

// Shared mode keeps its refusal: the one server is every session's, and a failed
// read of it is no licence to guess about one of them (muster #473).
func TestClose_InSharedModeAServerThatAnswers500StillRefuses(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	f.mu.Lock()
	f.failAll = true
	f.mu.Unlock()

	_, err := d.Close(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err == nil || errors.Is(err, fleet.ErrNoSuchSession) {
		t.Fatalf("Close = %v, want the server's own failure", err)
	}
	if !d.Holds(ref.ID) {
		t.Error("a refused close in shared mode dropped the record")
	}

	// And the source stays degraded for a listing whose one shared server fails.
	col, lerr := d.List(context.Background(), fleet.RequestFrom(fleet.Caller{}), driver.ListFilter{})
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if col.Complete() {
		t.Error("a listing whose shared server answers 500 is complete")
	}
}

func TestHolds_IsTheDriversOwnRecordAndNeverTheRuntimes(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	if d.Holds("ses_never") {
		t.Error("Holds is true for an id never seen")
	}
	ref := createOne(t, d, "/work/x", "key-1")
	f.mu.Lock()
	f.failAll = true
	f.mu.Unlock()
	if !d.Holds(ref.ID) {
		t.Error("Holds is false for a session whose runtime cannot be asked")
	}
	var _ driver.RecordHolder = d
}
