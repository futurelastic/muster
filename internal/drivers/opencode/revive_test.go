package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/service"
)

// muster #427: a session a restart left unknown, "needs re-creation", is brought back
// by its caller handing the environment over again.

const reviveSecret = "sekrit-value-0a1b2c3d4e5f6071"

// reviveLog collects what the driver says, so a test can read every line.
type reviveLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *reviveLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *reviveLog) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// leftUnknown creates a session with caller-supplied environment, gives it three
// turns of history, stops the service and starts a new one on the same state
// directory: the session is listed unknown there. It returns the new driver.
func leftUnknown(t *testing.T, state string, spec fleet.SessionSpec, logs *reviveLog) (*Driver, fleet.Session) {
	t.Helper()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k-revive", spec)
	known, _ := d1.wasSeen(sess.ID)
	plantTurns(t, known.srv.dir, 3)
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	opts := []Option{}
	if logs != nil {
		opts = append(opts, WithLogf(logs.logf))
	}
	d2 := newStateDriver(t, state, opts...)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status != fleet.StatusUnknown {
		t.Fatalf("the session after the restart = %+v (listed: %v); want unknown", got.State, ok)
	}
	if k, _ := d2.wasSeen(sess.ID); k.recreate != recreateCallerEnv {
		t.Fatalf("recreate = %v; want the caller-environment cause", k.recreate)
	}
	if logs != nil && !strings.Contains(logs.all(), "1 session(s) were left needing re-creation by this start") {
		t.Errorf("the restart log does not say what the restart cost:\n%s", logs.all())
	}
	return d2, sess
}

func reviveWith(d *Driver, sess fleet.Session, env map[string]string) (fleet.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return d.Revive(ctx, testReq, sess.SessionRef, driver.ReviveSpec{Env: env, MayBypass: true})
}

func assertNotRevivable(t *testing.T, err error) *fleet.Error {
	t.Helper()
	fe, ok := err.(*fleet.Error)
	if !ok || fe.Kind != fleet.ErrorConflict || fe.Reason != fleet.ReasonNotRevivable {
		t.Fatalf("err = %v; want a conflict with reason %q", err, fleet.ReasonNotRevivable)
	}
	return fe
}

func assertNoSecretUnder(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if raw, _ := os.ReadFile(p); strings.Contains(string(raw), reviveSecret) {
				t.Errorf("%s holds the environment value", p)
			}
		}
		return nil
	})
}

// The whole of the issue's oracle: create with caller env, restart, observe unknown,
// revive with the env; the session reads idle with its conversation known, under the
// same id, with the turns it had, and nothing anywhere holds the value.
func TestRevive_ACallerEnvSessionComesBackWithItsConversation(t *testing.T) {
	state := t.TempDir()
	logs := &reviveLog{}
	d2, sess := leftUnknown(t, state, fleet.SessionSpec{Name: "with-env", Env: map[string]string{"MODEL_KEY": reviveSecret}}, logs)
	before := listed(t, d2)[sess.ID]
	recBefore, _ := os.ReadFile(filepath.Join(state, "opencode", "sessions", sess.ID+".json"))

	got, err := reviveWith(d2, sess, map[string]string{"MODEL_KEY": reviveSecret})
	if err != nil {
		t.Fatalf("Revive: %v", err)
	}
	if got.ID != sess.ID {
		t.Errorf("id = %q; want the same id %q", got.ID, sess.ID)
	}
	if got.State.Status != fleet.StatusIdle {
		t.Errorf("status = %q (%s); want idle", got.State.Status, got.State.Evidence)
	}
	if got.Conversation == nil || !got.Conversation.Known {
		t.Errorf("conversation = %+v; want known", got.Conversation)
	}
	if got.StartedAt == nil || before.StartedAt == nil || !got.StartedAt.Equal(*before.StartedAt) {
		t.Errorf("startedAt = %v; want it unchanged (%v)", got.StartedAt, before.StartedAt)
	}
	if got.Name != "with-env" {
		t.Errorf("name = %q; want the one it was created with", got.Name)
	}
	// The listing agrees with the answer.
	if l := listed(t, d2)[sess.ID]; l.State.Status != fleet.StatusIdle || l.Conversation == nil || !l.Conversation.Known {
		t.Errorf("listing = %+v; want idle with its conversation known", l)
	}
	// The process has the value, and it is the session's own process.
	if v := inside(t, d2, sess.ID).vars()["MODEL_KEY"]; v != reviveSecret {
		t.Errorf("MODEL_KEY inside the session = %q; want the value handed to the revive", v)
	}
	// Its prior turns are readable.
	if texts := texts(turnsOf(t, d2, sess.SessionRef, "")); !eqStrings(texts, threeAnswers) {
		t.Errorf("turns = %q; want the three it had", texts)
	}
	// It takes input again.
	if _, err := d2.Send(context.Background(), testReq, sess.SessionRef, "carry on", driver.SendOptions{Submit: true}); err != nil {
		t.Errorf("Send to the revived session: %v", err)
	}

	// Nothing persisted: the record is what it was but for the process it names, and
	// neither it, the directory, nor the log holds the value.
	recAfter, _ := os.ReadFile(filepath.Join(state, "opencode", "sessions", sess.ID+".json"))
	if strings.Contains(string(recAfter), reviveSecret) {
		t.Error("the record holds the environment value")
	}
	var a, b map[string]any
	_ = json.Unmarshal(recBefore, &a)
	_ = json.Unmarshal(recAfter, &b)
	for _, k := range []string{"pid", "port", "bin"} {
		delete(a, k)
		delete(b, k)
	}
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("the record changed beyond the process it names:\nbefore %v\nafter  %v", a, b)
	}
	assertNoSecretUnder(t, state)
	if strings.Contains(logs.all(), reviveSecret) {
		t.Errorf("the log holds the value:\n%s", logs.all())
	}
	if !strings.Contains(logs.all(), "revived session "+sess.ID) {
		t.Errorf("the log does not say the session was revived:\n%s", logs.all())
	}
}

// A session that was in the middle of a turn when the service stopped reads idle with
// an interrupted last turn, as a relaunched one does, never as an ordinary idle.
func TestRevive_AMidTurnSessionReadsIdleWithAnInterruptedLastTurn(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": reviveSecret}})
	known, _ := d1.wasSeen(sess.ID)
	raw, _ := json.Marshal([]wireMessage{{Info: wireMessageInfo{Role: "assistant"}}})
	if err := os.WriteFile(filepath.Join(known.srv.dir, "xdg", "data", "fake-messages.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = d1.Shutdown()
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })

	got, err := reviveWith(d2, sess, map[string]string{"MODEL_KEY": reviveSecret})
	if err != nil {
		t.Fatalf("Revive: %v", err)
	}
	if got.State.Status != fleet.StatusIdle {
		t.Errorf("status = %q; want idle", got.State.Status)
	}
	if got.State.LastTurn == nil || got.State.LastTurn.Outcome != "interrupted" || !got.State.LastTurn.Retryable {
		t.Errorf("lastTurn = %+v; want an interrupted, retryable turn", got.State.LastTurn)
	}
}

func TestRevive_WithoutEnvOrWithOtherNamesIsInvalidAndStartsNothing(t *testing.T) {
	state := t.TempDir()
	d2, sess := leftUnknown(t, state, fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": reviveSecret, "OTHER_KEY": "v"}}, nil)
	recPath := filepath.Join(state, "opencode", "sessions", sess.ID+".json")
	recBefore, _ := os.ReadFile(recPath)

	cases := []struct {
		name    string
		env     map[string]string
		mention string
	}{
		{"nil", nil, "env is required"},
		{"empty", map[string]string{}, "env is required"},
		{"one name short", map[string]string{"MODEL_KEY": reviveSecret}, "missing: OTHER_KEY"},
		{"one name too many", map[string]string{"MODEL_KEY": reviveSecret, "OTHER_KEY": "v", "EXTRA_KEY": "x"}, "not in the create: EXTRA_KEY"},
		{"a different name", map[string]string{"MODEL_KEY": reviveSecret, "ELSE_KEY": "v"}, "missing: OTHER_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reviveWith(d2, sess, tc.env)
			fe, ok := err.(*fleet.Error)
			if !ok || fe.Kind != fleet.ErrorInvalid {
				t.Fatalf("err = %v; want invalid", err)
			}
			if !strings.Contains(fe.Message, tc.mention) {
				t.Errorf("message = %q; want it to say %q", fe.Message, tc.mention)
			}
			if strings.Contains(fe.Message, reviveSecret) {
				t.Errorf("the refusal carries a value: %q", fe.Message)
			}
			if k, _ := d2.wasSeen(sess.ID); k.srv != nil || k.recreate != recreateCallerEnv {
				t.Error("a refused revive changed the session or started a server")
			}
		})
	}
	if recAfter, _ := os.ReadFile(recPath); string(recAfter) != string(recBefore) {
		t.Error("a refused revive rewrote the record")
	}
}

// A variable the service owns can never be among the names a create carried, so it is
// among the names a revive may not carry: the name check refuses it, naming the names.
func TestRevive_AVariableTheServiceOwnsIsInvalid(t *testing.T) {
	d2, sess := leftUnknown(t, t.TempDir(), fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": "v"}}, nil)
	_, err := reviveWith(d2, sess, map[string]string{"MODEL_KEY": "v", "HOME": "/elsewhere"})
	fe, ok := err.(*fleet.Error)
	if !ok || fe.Kind != fleet.ErrorInvalid || !strings.Contains(fe.Message, "not in the create: HOME") {
		t.Fatalf("err = %v; want invalid, naming HOME", err)
	}
	if k, _ := d2.wasSeen(sess.ID); k.srv != nil {
		t.Error("a server was started on a refused revive")
	}
}

func TestRevive_ARunningSessionIsNotRevivable(t *testing.T) {
	d, _ := newIsolatedDriver(t, WithStateDir(t.TempDir()))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": "v"}})
	pid := inside(t, d, sess.ID).Pid

	fe := assertNotRevivable(t, func() error { _, err := reviveWith(d, sess, map[string]string{"MODEL_KEY": "v"}); return err }())
	if !strings.Contains(fe.Message, "running") {
		t.Errorf("message = %q; want it to say the session is running", fe.Message)
	}
	if got := inside(t, d, sess.ID).Pid; got != pid {
		t.Error("a refused revive replaced the running server")
	}
}

func TestRevive_AParkedSessionIsNotRevivable(t *testing.T) {
	d, clock, _, _ := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	parkSession(t, d, clock, sess.ID)
	_, err := reviveWith(d, sess, map[string]string{"MODEL_KEY": "v"})
	assertNotRevivable(t, err)
}

// A session unknown for a cause the environment has nothing to do with is refused:
// supplying it changes nothing, and the next start already tries again.
func TestRevive_AnUnknownSessionOfAnotherCauseIsNotRevivable(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{}) // no caller env
	known, _ := d1.wasSeen(sess.ID)
	if err := os.Remove(filepath.Join(known.srv.dir, "xdg", "data", "fake-session.json")); err != nil {
		t.Fatalf("the stand-in keeps no store file to remove: %v", err)
	}
	_ = d1.Shutdown()
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got := listed(t, d2)[sess.ID]
	if got.State.Status != fleet.StatusUnknown {
		t.Fatalf("status = %q; want unknown (the runtime lost the session)", got.State.Status)
	}
	if k, _ := d2.wasSeen(sess.ID); k.recreate != recreateRelaunchFailed {
		t.Fatalf("recreate = %v; want the relaunch-failed cause", k.recreate)
	}
	fe := assertNotRevivable(t, func() error { _, err := reviveWith(d2, sess, map[string]string{"MODEL_KEY": "v"}); return err }())
	if !strings.Contains(fe.Message, "no longer has this session") {
		t.Errorf("message = %q; want it to carry the reason the listing gives", fe.Message)
	}
}

func TestRevive_AnUnknownIdIsNotFound(t *testing.T) {
	d, _ := newIsolatedDriver(t, WithStateDir(t.TempDir()))
	_, err := d.Revive(context.Background(), testReq, fleet.SessionRef{Machine: "test-machine", ID: "ses_nothing"},
		driver.ReviveSpec{Env: map[string]string{"A": "b"}})
	if err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("err = %v; want no such session", err)
	}
}

func TestRevive_AStartedAtThatIsNotTheSessionsIsAConflictAndStartsNothing(t *testing.T) {
	d2, sess := leftUnknown(t, t.TempDir(), fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": "v"}}, nil)
	wrong := time.Unix(1, 0)
	req := fleet.RequestFrom(fleet.Caller{Principal: "x"})
	req.Expect.StartedAt = &wrong
	_, err := d2.Revive(context.Background(), req, sess.SessionRef, driver.ReviveSpec{Env: map[string]string{"MODEL_KEY": "v"}})
	if kindOfErr(err) != "" || err == nil || !strings.Contains(err.Error(), "started at") {
		t.Fatalf("err = %v; want the §5.4 ambiguous-target error", err)
	}
	if k, _ := d2.wasSeen(sess.ID); k.srv != nil {
		t.Error("a server was started for a session the caller did not mean")
	}
}

// The context is a check: the file the create named is read again and the revived
// session carries it; a different file, or one for a session that had none, is refused.
func TestRevive_ContextRefMustMatchTheRecord(t *testing.T) {
	ctxFile := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(ctxFile, []byte("the brief"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other.md")
	if err := os.WriteFile(other, []byte("another"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MODEL_KEY": "v"}
	d2, sess := leftUnknown(t, t.TempDir(), fleet.SessionSpec{Env: env, ContextRef: fleet.AbsolutePath(ctxFile)}, nil)

	ctx := context.Background()
	if _, err := d2.Revive(ctx, testReq, sess.SessionRef, driver.ReviveSpec{Env: env, ContextRef: fleet.AbsolutePath(other)}); kindOfErr(err) != fleet.ErrorInvalid {
		t.Fatalf("a different contextRef: %v; want invalid", err)
	}
	if k, _ := d2.wasSeen(sess.ID); k.srv != nil {
		t.Fatal("a server was started on a refused revive")
	}
	if _, err := d2.Revive(ctx, testReq, sess.SessionRef, driver.ReviveSpec{Env: env, ContextRef: fleet.AbsolutePath(ctxFile)}); err != nil {
		t.Fatalf("the right contextRef: %v", err)
	}
	if k, _ := d2.wasSeen(sess.ID); k.context != "the brief" || k.contextRef != fleet.AbsolutePath(ctxFile) {
		t.Errorf("the revived session carries context %q from %q; want the recorded file's text", k.context, k.contextRef)
	}

	// A session created with no context refuses one.
	d3, sess3 := leftUnknown(t, t.TempDir(), fleet.SessionSpec{Env: env}, nil)
	if _, err := d3.Revive(ctx, testReq, sess3.SessionRef, driver.ReviveSpec{Env: env, ContextRef: fleet.AbsolutePath(ctxFile)}); kindOfErr(err) != fleet.ErrorInvalid {
		t.Fatalf("a contextRef for a session created without one: %v; want invalid", err)
	}
}

// With the caller's environment the relaunch still has to find the session in the
// runtime's store; a runtime that has lost it leaves the session as it was — unknown,
// no server — and the answer is "not revivable", not "try again".
func TestRevive_RuntimeThatLostTheSessionLeavesItUnknown(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": "v"}})
	known, _ := d1.wasSeen(sess.ID)
	storeFile := filepath.Join(known.srv.dir, "xdg", "data", "fake-session.json")
	_ = d1.Shutdown()
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	if err := os.Remove(storeFile); err != nil {
		t.Fatal(err)
	}
	recPath := filepath.Join(state, "opencode", "sessions", sess.ID+".json")
	recBefore, _ := os.ReadFile(recPath)

	_, err := reviveWith(d2, sess, map[string]string{"MODEL_KEY": "v"})
	assertNotRevivable(t, err)
	k, _ := d2.wasSeen(sess.ID)
	if k.srv != nil || k.recreate != recreateCallerEnv || k.unknown == "" {
		t.Errorf("the session after the failed revive = %+v; want it unchanged: unknown, no server, still revivable", k)
	}
	if recAfter, _ := os.ReadFile(recPath); string(recAfter) != string(recBefore) {
		t.Error("a failed revive rewrote the record")
	}
}

// Two revives of one session start one server: the second finds it running.
func TestRevive_TwoConcurrentRevivesStartOneServer(t *testing.T) {
	env := map[string]string{"MODEL_KEY": "v"}
	d2, sess := leftUnknown(t, t.TempDir(), fleet.SessionSpec{Env: env}, nil)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = reviveWith(d2, sess, env)
		}(i)
	}
	close(start)
	wg.Wait()
	ok, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case func() bool { fe, isFE := err.(*fleet.Error); return isFE && fe.Reason == fleet.ReasonNotRevivable }():
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("outcomes: %d revived, %d refused; want exactly one of each", ok, refused)
	}
	if st, err := d2.State(context.Background(), testReq, sess.SessionRef); err != nil || st.Status != fleet.StatusIdle {
		t.Errorf("State = %+v, %v; want idle", st, err)
	}
}

// A close waits for a revive in progress: it holds the session's lifecycle lock, with
// or without an idle bound, so the close cannot remove the directory a server is being
// started in. The test holds the lock as the revive does and shows the close queue
// behind it.
func TestRevive_ACloseWaitsForARevivesLock(t *testing.T) {
	env := map[string]string{"MODEL_KEY": "v"}
	d2, sess := leftUnknown(t, t.TempDir(), fleet.SessionSpec{Env: env}, nil)
	unlock := d2.lockLifecycle(sess.ID)
	closed := make(chan error, 1)
	go func() {
		_, err := d2.Close(context.Background(), testReq, sess.SessionRef)
		closed <- err
	}()
	select {
	case err := <-closed:
		t.Fatalf("Close finished (%v) while a revive held the session's lock", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Close did not run once the lock was released")
	}
}

// A close that wins the lock first removes the session; the revive that was waiting
// finds it gone and starts nothing.
func TestRevive_AfterACloseTheSessionIsNotFound(t *testing.T) {
	env := map[string]string{"MODEL_KEY": "v"}
	d2, sess := leftUnknown(t, t.TempDir(), fleet.SessionSpec{Env: env}, nil)
	unlock := d2.lockLifecycle(sess.ID)
	done := make(chan error, 1)
	go func() {
		_, err := reviveWith(d2, sess, env)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond) // the revive is queued behind the lock
	d2.forgetSeen(sess.ID)             // what a close that held the lock did
	unlock()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no session") {
			t.Fatalf("Revive after the session was removed = %v; want no such session", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Revive did not return")
	}
}

// A revive is not a way to keep a secret: the next restart lists the session unknown
// again, with the record naming the variables and holding no value.
func TestRevive_TheNextRestartListsTheSessionUnknownAgain(t *testing.T) {
	state := t.TempDir()
	env := map[string]string{"MODEL_KEY": reviveSecret}
	d2, sess := leftUnknown(t, state, fleet.SessionSpec{Env: env}, nil)
	if _, err := reviveWith(d2, sess, env); err != nil {
		t.Fatalf("Revive: %v", err)
	}
	if err := d2.Shutdown(); err != nil {
		t.Fatal(err)
	}
	d3 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d3.Shutdown() })
	got := listed(t, d3)[sess.ID]
	if got.State.Status != fleet.StatusUnknown || !strings.Contains(got.State.Evidence, "MODEL_KEY") {
		t.Errorf("after the second restart = %+v; want unknown, naming the variable", got.State)
	}
	assertNoSecretUnder(t, state)
	// And it can be revived again.
	if _, err := reviveWith(d3, sess, env); err != nil {
		t.Errorf("a second revive: %v", err)
	}
}

// A session created in the permission-bypass posture is started only for a caller who
// may send, as its create was.
func TestRevive_ABypassSessionNeedsMayBypass(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": "v"}})
	_ = d1.Shutdown()
	// A bypass session's record, as one a sandbox-enforcing host would have written.
	recPath := filepath.Join(state, "opencode", "sessions", sess.ID+".json")
	raw, _ := os.ReadFile(recPath)
	var rec map[string]any
	_ = json.Unmarshal(raw, &rec)
	rec["permissionMode"] = fleet.PermissionModeBypass
	raw, _ = json.Marshal(rec)
	if err := os.WriteFile(recPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })

	_, err := d2.Revive(context.Background(), testReq, sess.SessionRef,
		driver.ReviveSpec{Env: map[string]string{"MODEL_KEY": "v"}, MayBypass: false})
	if kindOfErr(err) != fleet.ErrorUnauthorized {
		t.Fatalf("a caller without send: %v; want unauthorized", err)
	}
	if k, _ := d2.wasSeen(sess.ID); k.srv != nil {
		t.Error("a server was started for a caller who may not start a bypass session")
	}
}

func TestRevive_CapabilityIsDeclaredWithAStateDirectoryAndIsolation(t *testing.T) {
	without, _ := newIsolatedDriver(t)
	if c := without.Capabilities().Revives; c == nil || *c {
		t.Errorf("revives = %v with no state directory; want declared false", c)
	}
	with := newStateDriver(t, t.TempDir())
	t.Cleanup(func() { _ = with.Shutdown() })
	if c := with.Capabilities().Revives; c == nil || !*c {
		t.Errorf("revives = %v with a state directory; want declared true", c)
	}
	f := newFakeServer(t)
	shared, err := newDriverWithOptions(t, f, WithStateDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if c := shared.Capabilities().Revives; c == nil || *c {
		t.Errorf("revives = %v in shared mode; want declared false", c)
	}
	// And the driver refuses a revive it cannot honour.
	_, err = without.Revive(context.Background(), testReq, fleet.SessionRef{ID: "ses_x"}, driver.ReviveSpec{Env: map[string]string{"A": "b"}})
	if kindOfErr(err) != fleet.ErrorUnsupported {
		t.Errorf("Revive with no state directory = %v; want unsupported", err)
	}
}

// The issue's oracle over the wire: a real service in front of the real driver. A
// session created with caller env, a restart, `unknown` on a read, then
// POST …/revive with the env — idle, `conversation.known`, the same id, its turns
// readable. Without env, or for a principal without the create grant, refused.
func TestRevive_OverHTTPBringsTheSessionBackAndRefusesWithoutEnvOrGrant(t *testing.T) {
	state := t.TempDir()
	env := map[string]string{"MODEL_KEY": reviveSecret}
	d2, sess := leftUnknown(t, state, fleet.SessionSpec{Env: env}, nil)

	svc := service.New("test-machine")
	if err := svc.RegisterLocalDriver(DefaultRuntime, d2); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(service.NewMux(svc, service.Config{Principals: []service.Principal{
		{Name: "creator", Token: "tok-create", Grants: []service.Grant{service.GrantRead, service.GrantCreate}},
		{Name: "sender", Token: "tok-send", Grants: []service.Grant{service.GrantRead, service.GrantSend}},
	}}))
	t.Cleanup(srv.Close)

	do := func(method, path, token, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	one := "/v1/machines/test-machine/sessions/" + sess.ID

	// Observed unknown, over the wire.
	code, raw := do("GET", one, "tok-send", "")
	if code != 200 || !strings.Contains(raw, `"status":"unknown"`) {
		t.Fatalf("before the revive: %d %s; want the session unknown", code, raw)
	}

	// Refused without env, and for a principal without the create grant; nothing started.
	if code, raw := do("POST", one+"/revive", "tok-create", `{}`); code != http.StatusBadRequest {
		t.Fatalf("revive without env: %d %s; want 400", code, raw)
	}
	if code, raw := do("POST", one+"/revive", "tok-send", `{"env":{"MODEL_KEY":"`+reviveSecret+`"}}`); code != http.StatusUnauthorized || !strings.Contains(raw, "create grant") {
		t.Fatalf("revive without the create grant: %d %s; want 401 naming it", code, raw)
	}
	if k, _ := d2.wasSeen(sess.ID); k.srv != nil {
		t.Fatal("a refused revive started a server")
	}

	// Revived.
	code, raw = do("POST", one+"/revive", "tok-create", `{"env":{"MODEL_KEY":"`+reviveSecret+`"}}`)
	if code != http.StatusOK {
		t.Fatalf("revive: %d %s; want 200", code, raw)
	}
	var got fleet.Session
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != sess.ID || got.State.Status != fleet.StatusIdle || got.Conversation == nil || !got.Conversation.Known {
		t.Errorf("answer = %+v; want the same id, idle, conversation known", got)
	}
	if strings.Contains(raw, reviveSecret) {
		t.Error("the answer carries the value")
	}
	// Its prior turns are readable over the wire, and a second revive is refused.
	if code, raw := do("GET", one+"/turns", "tok-send", ""); code != 200 || !strings.Contains(raw, "answer 1") {
		t.Errorf("turns after the revive: %d %s; want the three it had", code, raw)
	}
	code, raw = do("POST", one+"/revive", "tok-create", `{"env":{"MODEL_KEY":"`+reviveSecret+`"}}`)
	if code != http.StatusConflict || !strings.Contains(raw, `"reason":"not-revivable"`) {
		t.Errorf("a second revive: %d %s; want 409 not-revivable", code, raw)
	}
}
