package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Reproductions for muster #506: a poller saw `turns=0` on 53 of 54 reads of a
// live session, one `turns=3` among them, and read the zeros as turns the route
// had lost.
//
// What these pin is the answer to "does a read of a live session ever return fewer
// turns than an earlier read of it?" — and, the other half, "what does a poller
// that holds a cursor see?". They are the cases a live session's record goes
// through; each one is a place the count could have gone back down.

// recordStep is one change a live runtime makes to a session's message record, and
// the turns a read WITHOUT a cursor answers once it has been made.
type recordStep struct {
	name   string
	apply  func(f *fakeServer, id string)
	expect []string
}

// A tail read (no `since`) returns every turn the record holds, up to the limit,
// and never fewer than the read before it; a poller that carries `next` is told
// each turn once and otherwise nothing. The two are the same record read two ways,
// which is why the audit line says which one it saw (service/turns.go).
func TestTurns_ATailReadNeverShrinksAndACursorPollerSeesEachTurnOnce(t *testing.T) {
	steps := []recordStep{
		{"a prompt, nothing answered yet", func(f *fakeServer, id string) {
			f.addMessage(id, tUser("msg_u1", 10, "q1"))
		}, nil},
		{"the answer is in flight", func(f *fakeServer, id string) {
			f.addMessage(id, tAsst("msg_a1", 11, 0, tText("half of an answ")))
		}, nil},
		{"the answer finishes", func(f *fakeServer, id string) {
			f.deleteMessage(id, "msg_a1")
			f.addMessage(id, tAsst("msg_a1", 11, 15, tText("one")))
		}, []string{"one"}},
		{"a round that only calls a tool", func(f *fakeServer, id string) {
			f.addMessage(id, tUser("msg_u2", 20, "q2"))
			f.addMessage(id, tAsst("msg_a2", 21, 22, tPart("tool", map[string]any{"state": map[string]any{"output": "x"}})))
		}, []string{"one"}},
		{"the round after it answers", func(f *fakeServer, id string) {
			f.addMessage(id, tAsst("msg_a3", 23, 24, tText("two")))
		}, []string{"one", "two"}},
		{"the runtime compacts the context", func(f *fakeServer, id string) {
			f.addMessage(id, withInfo(tAsst("msg_a4", 30, 31, tText("SUMMARY-of-the-session")), "summary", true))
		}, []string{"one", "two"}},
		{"a round the provider refused", func(f *fakeServer, id string) {
			f.addMessage(id, withInfo(tAsst("msg_a5", 32, 33, tText("ERRORED")), "error", map[string]any{"name": "APIError"}))
		}, []string{"one", "two"}},
		{"a long run of tool rounds pushes the answers past the first window", func(f *fakeServer, id string) {
			for n := 0; n < 40; n++ {
				f.addMessage(id, tAsst(fmt.Sprintf("msg_t%03d", n), int64(40+n*2), int64(41+n*2),
					tPart("tool", map[string]any{"state": map[string]any{"output": "x"}})))
			}
		}, []string{"one", "two"}},
		{"and then the final answer", func(f *fakeServer, id string) {
			f.addMessage(id, tUser("msg_u3", 200, "q3"))
			f.addMessage(id, tAsst("msg_a6", 201, 202, tText("three")))
		}, []string{"one", "two", "three"}},
		{"a prompt queued behind a running turn", func(f *fakeServer, id string) {
			f.addMessage(id, tUser("msg_u4", 210, "q4"))
			f.addMessage(id, tAsst("msg_a7", 211, 0, tText("being written")))
			f.addMessage(id, tUser("msg_u5", 212, "q5"))
		}, []string{"one", "two", "three"}},
	}

	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")

	start := getTurns(t, d, ref, "", 0)
	if len(start.Turns) != 0 {
		t.Fatalf("a record with nothing in it returned %q", texts(start))
	}
	cursor := start.Next
	var polled []string
	var before []string
	for _, s := range steps {
		s.apply(f, ref.ID)

		tail := getTurns(t, d, ref, "", 0)
		if got := texts(tail); !eqStrings(got, s.expect) {
			t.Fatalf("after %q a tail read = %q, want %q", s.name, got, s.expect)
		}
		if !hasPrefix(texts(tail), before) {
			t.Fatalf("after %q a tail read = %q, which dropped what the read before it returned (%q)", s.name, texts(tail), before)
		}
		before = texts(tail)

		poll := getTurns(t, d, ref, cursor, 0)
		polled = append(polled, texts(poll)...)
		cursor = poll.Next
	}
	if want := []string{"one", "two", "three"}; !eqStrings(polled, want) {
		t.Fatalf("a poller carrying next was told %q across the whole run, want each turn once: %q", polled, want)
	}
	// The part of the report that was the poller's own: after the last turn every
	// further cursor read answers nothing and stays where it was — turns=0, moved=0.
	for i := 0; i < 5; i++ {
		idle := getTurns(t, d, ref, cursor, 0)
		if len(idle.Turns) != 0 || idle.Next != cursor {
			t.Fatalf("an idle poll = %+v, want no turns and the same next", idle)
		}
	}
	// And a read that does NOT carry the cursor still returns all three.
	if got, want := texts(getTurns(t, d, ref, "", 0)), []string{"one", "two", "three"}; !eqStrings(got, want) {
		t.Fatalf("a tail read after the idle polls = %q, want %q", got, want)
	}
}

func hasPrefix(got, prefix []string) bool {
	if len(prefix) > len(got) {
		return false
	}
	for i := range prefix {
		if got[i] != prefix[i] {
			return false
		}
	}
	return true
}

// plantTurns replaces the stand-in's record of a session with n finished question
// and answer pairs, as the runtime's store holds them.
func plantTurns(t *testing.T, dir string, n int) {
	t.Helper()
	var msgs []any
	for i := 1; i <= n; i++ {
		msgs = append(msgs,
			tUser(fmt.Sprintf("msg_u%04d", i), int64(i*10), fmt.Sprintf("question %d", i)),
			tAsst(fmt.Sprintf("msg_a%04d", i), int64(i*10+1), int64(i*10+2), tText(fmt.Sprintf("answer %d", i))))
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "xdg", "data", "fake-messages.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func turnsOf(t *testing.T, d *Driver, ref fleet.SessionRef, since string) fleet.TurnsPage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	page, err := d.Turns(ctx, testReq, ref, fleet.TurnsQuery{Since: since})
	if err != nil {
		t.Fatalf("Turns(since=%q): %v", since, err)
	}
	return page
}

var threeAnswers = []string{"answer 1", "answer 2", "answer 3"}

// A session read before it is parked, after it is started again, and after the
// service restarts holds the same turns, and a cursor issued before any of that
// still works after: the store the reads come from is the one the session started
// on, whichever process serves it. A read of the parked session in between is a
// conflict that says so — never a 200 with no turns.
func TestTurns_TheSameTurnsAreReadBeforeAParkAfterARevivalAndAfterARestart(t *testing.T) {
	d, clock, _, state := newParkDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	plantTurns(t, known.srv.dir, 3)

	first := turnsOf(t, d, sess.SessionRef, "")
	if got := texts(first); !eqStrings(got, threeAnswers) {
		t.Fatalf("before the park = %q, want %q", got, threeAnswers)
	}

	parkSession(t, d, clock, sess.ID)
	_, err := d.Turns(context.Background(), testReq, sess.SessionRef, fleet.TurnsQuery{})
	if kindOfErr(err) != fleet.ErrorConflict {
		t.Fatalf("a read of the parked session = %v, want a conflict (a 200 with no turns is the answer this must never be)", err)
	}
	_, err = d.Turns(context.Background(), testReq, sess.SessionRef, fleet.TurnsQuery{Since: first.Next})
	if kindOfErr(err) != fleet.ErrorConflict {
		t.Fatalf("a cursor read of the parked session = %v, want a conflict", err)
	}

	if _, err := sendText(t, d, sess.SessionRef, "wake up"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := texts(turnsOf(t, d, sess.SessionRef, "")); !eqStrings(got, threeAnswers) {
		t.Fatalf("after the session was started again = %q, want %q", got, threeAnswers)
	}
	if idle := turnsOf(t, d, sess.SessionRef, first.Next); len(idle.Turns) != 0 || idle.Next != first.Next {
		t.Fatalf("the cursor from before the park, after the revival = %+v, want no turns and the same next", idle)
	}

	// One more turn is written while it runs; the cursor from before the park finds
	// exactly it.
	known, _ = d.wasSeen(sess.ID)
	plantTurns(t, known.srv.dir, 4)
	more := turnsOf(t, d, sess.SessionRef, first.Next)
	if got, want := texts(more), []string{"answer 4"}; !eqStrings(got, want) {
		t.Fatalf("since the cursor from before the park = %q, want %q", got, want)
	}

	// The service restarts: a new driver on the same state directory relaunches the
	// session on the same store.
	if err := d.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	if got, want := texts(turnsOf(t, d2, sess.SessionRef, "")), []string{"answer 1", "answer 2", "answer 3", "answer 4"}; !eqStrings(got, want) {
		t.Fatalf("after a restart = %q, want %q", got, want)
	}
	if idle := turnsOf(t, d2, sess.SessionRef, more.Next); len(idle.Turns) != 0 || idle.Next != more.Next {
		t.Fatalf("a cursor read after a restart = %+v, want no turns and the same next", idle)
	}
}

// A runtime whose process has exited answers a read with the fact, never with an
// empty page: a person-facing client cannot tell "nothing yet" from "gone" by a
// zero, so the zero must not be possible.
func TestTurns_AnExitedRuntimeIsNeverAnEmptyPage(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	known, _ := d.wasSeen(sess.ID)
	plantTurns(t, known.srv.dir, 3)
	first := turnsOf(t, d, sess.SessionRef, "")
	if got := texts(first); !eqStrings(got, threeAnswers) {
		t.Fatalf("while it runs = %q, want %q", got, threeAnswers)
	}

	_ = known.srv.proc.group.Kill()
	select {
	case <-known.srv.proc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not exit")
	}

	for _, since := range []string{"", first.Next} {
		page, err := d.Turns(context.Background(), testReq, sess.SessionRef, fleet.TurnsQuery{Since: since})
		if err == nil {
			t.Fatalf("a read (since=%q) of a session whose process has exited answered %+v, want the fact that it exited", since, page)
		}
		if kindOfErr(err) != fleet.ErrorConflict || !strings.Contains(err.Error(), "runtime process has exited") {
			t.Errorf("a read (since=%q) of an exited session = %v, want a conflict that says the process has exited", since, err)
		}
	}
	st, err := d.State(context.Background(), testReq, sess.SessionRef)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != fleet.StatusDead {
		t.Fatalf("status = %q, want dead", st.Status)
	}
}

// killAndWait ends a session's process the way something outside the driver would.
func killAndWait(t *testing.T, d *Driver, id string) {
	t.Helper()
	known, _ := d.wasSeen(id)
	_ = known.srv.proc.group.Kill()
	select {
	case <-known.srv.proc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not exit")
	}
}

// waitForLine waits for the driver to have said something containing sub.
func waitForLine(t *testing.T, l *parkLog, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l.has(sub) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the driver never said %q: %q", sub, l.lines)
}

// `dead` is a client's word to close a session and lose its directory with it, so
// it has to say what happened while the evidence still exists: how the process
// ended, how long it had been up, and how long after the session last did
// anything. State, the listing and the line the driver leaves all carry it.
func TestState_ADeadSessionSaysHowItsProcessEnded(t *testing.T) {
	sink := &parkLog{}
	d, _ := newIsolatedDriver(t, WithLogf(sink.logf))
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{})
	killAndWait(t, d, sess.ID)

	// How a killed process reads is the platform's own: a signal where there are
	// signals, an exit status where a job object ends it.
	how := "signal: killed"
	if runtime.GOOS == "windows" {
		how = "exit status "
	}

	st, err := d.State(context.Background(), testReq, sess.SessionRef)
	if err != nil {
		t.Fatal(err)
	}
	inList := listed(t, d)[sess.ID].State
	for where, s := range map[string]fleet.SessionState{"State": st, "List": inList} {
		if s.Status != fleet.StatusDead {
			t.Errorf("%s: status = %q, want dead", where, s.Status)
		}
		for _, want := range []string{"the session's runtime process has exited", how, ", up ", "after the session last did anything"} {
			if !strings.Contains(s.Evidence, want) {
				t.Errorf("%s: evidence %q lacks %q", where, s.Evidence, want)
			}
		}
	}
	waitForLine(t, sink, "exited and this driver had not stopped it")
	if !sink.has("session "+sess.ID) || !sink.has(how) {
		t.Errorf("the line does not name the session and how it ended: %q", sink.lines)
	}
}

// A process the driver stopped — a close, a park, the service stopping — is not a
// death, and leaves no line saying it was.
func TestState_AStopTheDriverMadeIsNotReportedAsAnExit(t *testing.T) {
	sink := &parkLog{}
	d, clock, _, _ := newParkDriver(t, WithLogf(sink.logf))
	// newParkDriver installs its own sink first; the later option wins.
	closed := mustCreate(t, d, "k-close", fleet.SessionSpec{})
	parked := mustCreate(t, d, "k-park", fleet.SessionSpec{})

	if _, err := d.Close(context.Background(), testReq, closed.SessionRef); err != nil {
		t.Fatalf("Close: %v", err)
	}
	parkSession(t, d, clock, parked.ID)
	if err := d.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // a watcher that was going to speak has by now
	if sink.has("had not stopped it") {
		t.Errorf("a stop the driver made was reported as an exit: %q", sink.lines)
	}
}

// replaceMessage swaps the record's message with this id for another, in place, the
// way a runtime finishes a message it was still writing.
func replaceMessage(t *testing.T, f *fakeServer, session, id string, msg any) {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.messages[session] {
		var m struct {
			Info struct {
				ID string `json:"id"`
			} `json:"info"`
		}
		if json.Unmarshal(r, &m) == nil && m.Info.ID == id {
			f.messages[session][i] = raw
			return
		}
	}
	t.Fatalf("no message %q to replace", id)
}

// The one place a tail read and a cursor poller can come to different answers
// (muster #506, the plan's R3): an assistant message that finishes AFTER a later
// assistant message already exists.
//
// A message the runtime is still writing is held, and passed over only when a later
// assistant message exists, because the runtime writes a session's messages one
// after another — so an unfinished one with a successor can only be what a cut-off
// turn left behind (muster #282). On a runtime that does that, the case below
// cannot happen. This pins what the code does if one ever did not: the cursor
// poller has consumed past the message and never sees it, and a tail read returns
// it. It is here so that loosening the rule, or the runtime changing underneath it,
// is a deliberate change and not a silent one.
//
// Not measured against a live runtime in the session that wrote this: the premise
// is the one the package comment already rests on.
func TestTurns_ATurnThatFinishesAfterALaterAssistantMessageExistsIsOnlyInATailRead(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "k1")
	cursor := getTurns(t, d, ref, "", 0).Next

	f.addMessage(ref.ID, tUser("msg_u1", 10, "q1"))
	f.addMessage(ref.ID, tAsst("msg_a1", 11, 0, tText("in flight"))) // never finished when the next one was written
	f.addMessage(ref.ID, tUser("msg_u2", 20, "q2"))
	f.addMessage(ref.ID, tAsst("msg_a2", 21, 22, tText("the next answer")))

	poll := getTurns(t, d, ref, cursor, 0)
	if got, want := texts(poll), []string{"the next answer"}; !eqStrings(got, want) {
		t.Fatalf("a poller past a remnant = %q, want %q", got, want)
	}

	replaceMessage(t, f, ref.ID, "msg_a1", tAsst("msg_a1", 11, 15, tText("finished late")))

	if got := texts(getTurns(t, d, ref, poll.Next, 0)); len(got) != 0 {
		t.Fatalf("the poller that had passed it was told %q; it never sees it", got)
	}
	if got, want := texts(getTurns(t, d, ref, "", 0)), []string{"finished late", "the next answer"}; !eqStrings(got, want) {
		t.Fatalf("a tail read = %q, want both turns, oldest first: %q", got, want)
	}
}

// A second service started on a state directory a first one is running on stops the
// first one's servers before it does anything else: the start-up sweep takes every
// server under the session root for one an earlier service left behind. That is true
// after a crash and false against a live service, and nothing tells them apart
// (muster #507, found while looking into #506). Written as the behaviour #507 asks
// for and skipped until it is true; it was run once without the skip to confirm that
// it fails today.
func TestStartupSweep_ASecondDriverOnTheSameStateDirectoryLeavesTheFirstsServersRunning(t *testing.T) {
	t.Skip("muster #507: not yet true — the second start sweeps the first one's servers")
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d1.Shutdown() })
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{})
	pid := inside(t, d1, sess.ID).Pid

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })

	if !alive(pid) {
		t.Fatalf("the first service's server (pid %d) was stopped by a second service starting on its state directory", pid)
	}
	if st, err := d1.State(context.Background(), testReq, sess.SessionRef); err != nil || st.Status == fleet.StatusDead {
		t.Fatalf("the first service's session reads %q (err %v) after a second service started", st.Status, err)
	}
}
