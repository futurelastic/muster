package tmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/driver"
)

// #491: an unconfirmed lane write whose arrival the session's own transcript
// rules out stops holding its text; anything short of that proof keeps the hold.

const (
	releaseQuotaScreen = "transcript\nYou've hit your weekly limit · resets Thu 9:00\n" +
		"────────────────────\n❯ \n────────────────────\n"
	releaseWorkingScreen = "  transcript\n✻ Brewing… (12s · esc to interrupt)\n" +
		"────────────────────\n❯\n────────────────────\n"
)

// releaseRig is a module rig whose one session has a real transcript the driver
// can locate, a clock the test can move, and a pane that reads quota_blocked.
type releaseRig struct {
	*modRig
	id       string
	pane     string
	conv     string // the transcript's path
	skew     atomic.Int64
	sessions string
	convID   string
	cwd      string
}

func (r *releaseRig) advance(d time.Duration) { r.skew.Add(int64(d)) }

// append writes lines to the transcript.
func (r *releaseRig) append(lines ...any) {
	r.t.Helper()
	f, err := os.OpenFile(r.conv, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			r.t.Fatal(err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			r.t.Fatal(err)
		}
	}
}

func newReleaseRig(t *testing.T, earlier ...any) *releaseRig {
	t.Helper()
	recordRoot, sessionsRoot := t.TempDir(), t.TempDir()
	r := &releaseRig{
		sessions: sessionsRoot,
		cwd:      "/work/lane1",
		convID:   "0a0b0c0d-0000-4000-8000-000000000491",
	}
	convDir := filepath.Join(recordRoot, recordDirFor(r.cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.conv = filepath.Join(convDir, r.convID+".jsonl")
	var seed strings.Builder
	for _, e := range append([]any{map[string]any{
		"type": "user", "sessionId": r.convID, "timestamp": time.Now().Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": "the first turn"},
	}}, earlier...) {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		seed.Write(raw)
		seed.WriteString("\n")
	}
	if err := os.WriteFile(r.conv, []byte(seed.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	// The rig's first pane gets pid 5001. A process record names its conversation.
	const pid = 5001
	fps := &fakePS{}
	instant := time.Date(2026, time.January, 2, 15, 4, 5, 0, time.Local)
	fps.set(pid, instant)
	if err := os.WriteFile(filepath.Join(sessionsRoot, "5001.json"), []byte(mustJSONLine(t, map[string]any{
		"pid": pid, "sessionId": r.convID, "cwd": r.cwd, "procStart": instant.UTC().Format(psStartTimeLayout),
	})), 0o600); err != nil {
		t.Fatal(err)
	}

	queued := modtest.JSON(map[string]any{"verdict": "queued", "enqueued": true, "elapsedMs": 5, "final": false})
	r.modRig = newModRig(t, rigOptions{
		behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{
			"confirm": {Results: []json.RawMessage{queued}, DelayMs: 60},
		}},
		driverOpts: []Option{
			withPSExec(fps.exec),
			WithRecordRoot(recordRoot),
			WithProcessSessionsRoot(sessionsRoot),
			withClock(func() time.Time { return time.Now().Add(time.Duration(r.skew.Load())) }),
		},
	})
	sess := r.create("lane1", nil)
	r.waitLive(sess.ID)
	r.id, r.pane = sess.ID, "%5001"
	r.mux.setCapture(r.pane, releaseQuotaScreen)
	return r
}

// first sends text once, and fails the test unless the lane wrote it and could
// not confirm it: the unknown a caller is told not to resend.
func (r *releaseRig) first(text string) {
	r.t.Helper()
	ctx, cancel := short(r.t)
	defer cancel()
	got := r.sendCtx(ctx, r.id, text, driver.SendOptions{})
	if got.Outcome != fleet.OutcomeUnknown || got.ModuleOf() != modName {
		r.t.Fatalf("setup: the first send = %+v, want unknown naming the module", got)
	}
	if r.sends() != 1 {
		r.t.Fatalf("setup: module sends = %d, want 1", r.sends())
	}
}

// held reports whether a send of text now is answered from the ledger.
func (r *releaseRig) held(text string) bool {
	r.t.Helper()
	before := r.sends()
	ctx, cancel := short(r.t)
	defer cancel()
	got := r.sendCtx(ctx, r.id, text, driver.SendOptions{})
	if r.sends() != before {
		return false
	}
	return got.Outcome == fleet.OutcomeUnknown && strings.Contains(got.Reason, "NOT been sent again")
}

// The measured case: a session at an account wall, an idle live pane, a lane
// write the module can only call queued, and nothing in the transcript. The
// session reads quota_blocked, so the stall detector never armed.
func TestLaneRelease_QuotaBlockedWriteThatNeverArrivedLetsTheResendGo(t *testing.T) {
	r := newReleaseRig(t)
	if st, err := r.d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: r.id}); err != nil || st.Status != fleet.StatusQuotaBlocked {
		t.Fatalf("setup: state = %+v, err = %v; the pane must read quota_blocked", st, err)
	}
	r.first("continue where you stopped")
	if r.counter(counterLaneStall) != 0 {
		t.Fatalf("a quota_blocked session counted as an idle stall: %v", r.d.Counters())
	}

	// Straight away the module's wait has only just ended: the hold stands.
	if !r.held("continue where you stopped") {
		t.Fatal("a resend inside the settle margin was not held")
	}

	// Later, with the transcript still silent, it goes out through the lane.
	r.advance(laneReleaseSettle + time.Second)
	before := r.sends()
	ctx, cancel := short(t)
	defer cancel()
	again := r.sendCtx(ctx, r.id, "continue where you stopped", driver.SendOptions{})
	if r.sends() != before+1 {
		t.Fatalf("module sends = %d, want %d: the resend was not written (receipt %+v)", r.sends(), before+1, again)
	}
	if got := r.counter(counterRouteGuardModuleReleased); got != 1 {
		t.Errorf("%s = %d, want 1", counterRouteGuardModuleReleased, got)
	}
}

// What the transcript shows after the offset is evidence the text arrived, in
// whatever shape the runtime wrote it: the hold stays.
func TestLaneRelease_ARecordOfTheTextKeepsTheHold(t *testing.T) {
	for name, entry := range map[string]any{
		"a user turn": map[string]any{"type": "user",
			"message": map[string]any{"role": "user", "content": "continue where you stopped"}},
		"a wrapped copy": map[string]any{"type": "user", "origin": map[string]any{"kind": "channel"},
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text",
				"text": "<channel>[from: agent-x] continue where you stopped</channel>"}}}},
		"an enqueue": map[string]any{"type": "queue-operation", "operation": "enqueue", "content": "continue where you stopped"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReleaseRig(t)
			r.first("continue where you stopped")
			r.append(entry)
			r.advance(laneReleaseSettle + time.Second)
			if !r.held("continue where you stopped") {
				t.Fatalf("the hold was let go although the transcript carries the text (%s)", name)
			}
			if r.counter(counterRouteGuardModuleReleased) != 0 {
				t.Errorf("counters: %v", r.d.Counters())
			}
		})
	}
}

// An identical message from before the write sits before the offset and proves
// nothing about this one; the assistant's own prose is not the record of an input.
func TestLaneRelease_EarlierTextAndAssistantProseDoNotKeepTheHold(t *testing.T) {
	r := newReleaseRig(t, map[string]any{"type": "user",
		"message": map[string]any{"role": "user", "content": "continue where you stopped"}})
	r.first("continue where you stopped")
	r.append(map[string]any{"type": "assistant",
		"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text",
			"text": "I will continue where you stopped as soon as the limit lifts."}}}})
	r.advance(laneReleaseSettle + time.Second)
	if r.held("continue where you stopped") {
		t.Fatal("the hold stayed although nothing after the write carries the text as an input")
	}
}

// A turn that may still be draining a queue keeps the hold: only a session that
// reads quiet can say nothing is coming.
func TestLaneRelease_ABusySessionKeepsTheHold(t *testing.T) {
	r := newReleaseRig(t)
	r.first("continue where you stopped")
	r.advance(laneReleaseSettle + time.Second)
	r.mux.setCapture(r.pane, releaseWorkingScreen)
	if st, err := r.d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: r.id}); err != nil || st.Status != fleet.StatusWorking {
		t.Fatalf("setup: state = %+v, err = %v; the pane must read working", st, err)
	}
	if !r.held("continue where you stopped") {
		t.Fatal("the hold was let go while a turn is running")
	}
}

// A transcript that is shorter than it was, or that is no longer the session's
// conversation, says nothing about the old write.
func TestLaneRelease_AChangedTranscriptKeepsTheHold(t *testing.T) {
	t.Run("shrunk", func(t *testing.T) {
		r := newReleaseRig(t)
		r.first("continue where you stopped")
		if err := os.WriteFile(r.conv, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		r.advance(laneReleaseSettle + time.Second)
		if !r.held("continue where you stopped") {
			t.Fatal("the hold was let go over a transcript shorter than before the write")
		}
	})
	t.Run("gone", func(t *testing.T) {
		r := newReleaseRig(t)
		r.first("continue where you stopped")
		if err := os.Remove(r.conv); err != nil {
			t.Fatal(err)
		}
		r.advance(laneReleaseSettle + time.Second)
		if !r.held("continue where you stopped") {
			t.Fatal("the hold was let go with no transcript to read")
		}
	})
}

// A line the runtime is still writing may be the one that carries the text.
func TestLaneRelease_AnUnfinishedLastLineKeepsTheHold(t *testing.T) {
	r := newReleaseRig(t)
	r.first("continue where you stopped")
	f, err := os.OpenFile(r.conv, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"user","message":{"role":"user","content":"continue wh`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r.advance(laneReleaseSettle + time.Second)
	if !r.held("continue where you stopped") {
		t.Fatal("the hold was let go over a half-written last line")
	}
}

// The doubts that need no session: each answers false, and the entry that has
// every proof answers true.
func TestLaneRelease_EveryDoubtKeepsTheHold(t *testing.T) {
	r := newReleaseRig(t)
	r.first("continue where you stopped")
	r.advance(laneReleaseSettle + time.Second)
	ref := fleet.SessionRef{Machine: "testbox", ID: r.id}
	key := deliveryKey("continue where you stopped", agentFrom)
	e, ok := r.d.unconfirmedFor(r.id, key)
	if !ok {
		t.Fatal("setup: no ledger entry")
	}
	if e.TranscriptPath != r.conv || e.needle == "" {
		t.Fatalf("setup: entry = %+v; the transcript and the fingerprint must be kept", e)
	}
	if !r.d.laneWriteNeverArrived(context.Background(), testCaller, ref, e) {
		t.Fatal("setup: the entry with every proof was not released")
	}
	// A real file that is not the session's conversation: a /clear, or a replaced one.
	other := r.conv + ".other"
	if raw, err := os.ReadFile(r.conv); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(other, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*unconfirmedEntry){
		"a partly written frame":     func(e *unconfirmedEntry) { e.Partial = true },
		"no transcript kept":         func(e *unconfirmedEntry) { e.TranscriptPath = "" },
		"no fingerprint (a restart)": func(e *unconfirmedEntry) { e.needle = "" },
		"not yet settled":            func(e *unconfirmedEntry) { e.At = r.d.now() },
		"another transcript":         func(e *unconfirmedEntry) { e.TranscriptPath = other },
	} {
		c := e
		mutate(&c)
		if r.d.laneWriteNeverArrived(context.Background(), testCaller, ref, c) {
			t.Errorf("%s: the hold was let go", name)
		}
	}
}

// The persisted ledger holds digests, never a message: the fingerprint is not
// written out.
func TestLaneRelease_TheFingerprintIsNeverPersisted(t *testing.T) {
	raw, err := json.Marshal(unconfirmedEntry{Key: "k", Module: modName, needle: "continuewhereyoustopped"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "continuewhereyoustopped") || strings.Contains(string(raw), "needle") {
		t.Fatalf("the ledger's persisted form carries the text: %s", raw)
	}
}

func TestLaneTailRecords(t *testing.T) {
	needle := laneNeedle("Resume the migration")
	entries := func(lines ...string) (string, int64) {
		p := filepath.Join(t.TempDir(), "c.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p, 0
	}

	t.Run("silent", func(t *testing.T) {
		p, off := entries(`{"type":"user","message":{"content":"something else"}}`)
		if got, err := laneTailRecords(p, off, needle); err != nil || got {
			t.Errorf("= %v, %v; want false, nil", got, err)
		}
	})
	t.Run("the whitespace of the layout does not matter", func(t *testing.T) {
		p, off := entries(`{"type":"user","message":{"content":"Resume   the\nmigration"}}`)
		if got, err := laneTailRecords(p, off, needle); err != nil || !got {
			t.Errorf("= %v, %v; want true, nil", got, err)
		}
	})
	t.Run("any non-assistant entry counts", func(t *testing.T) {
		p, off := entries(`{"type":"system","subtype":"x","detail":{"deep":["Resume the migration"]}}`)
		if got, err := laneTailRecords(p, off, needle); err != nil || !got {
			t.Errorf("= %v, %v; want true, nil", got, err)
		}
	})
	t.Run("the assistant's output does not", func(t *testing.T) {
		p, off := entries(`{"type":"assistant","message":{"content":[{"type":"text","text":"Resume the migration"}]}}`)
		if got, err := laneTailRecords(p, off, needle); err != nil || got {
			t.Errorf("= %v, %v; want false, nil", got, err)
		}
	})
	t.Run("a file shorter than the offset says nothing", func(t *testing.T) {
		p, _ := entries(`{"type":"user","message":{"content":"x"}}`)
		if got, err := laneTailRecords(p, 1<<20, needle); err == nil || got {
			t.Errorf("= %v, %v; want an error", got, err)
		}
	})
	t.Run("a file that is gone says nothing", func(t *testing.T) {
		if got, err := laneTailRecords(filepath.Join(t.TempDir(), "none.jsonl"), 0, needle); err == nil || got {
			t.Errorf("= %v, %v; want an error", got, err)
		}
	})
	t.Run("an empty needle proves nothing", func(t *testing.T) {
		p, off := entries(`{"type":"user","message":{"content":"x"}}`)
		if got, _ := laneTailRecords(p, off, ""); !got {
			t.Error("an empty needle was read as not found")
		}
	})
	t.Run("a garbled middle line is skipped, a garbled last line is not", func(t *testing.T) {
		p, off := entries(`not json`, `{"type":"user","message":{"content":"x"}}`)
		if got, err := laneTailRecords(p, off, needle); err != nil || got {
			t.Errorf("middle: = %v, %v; want false, nil", got, err)
		}
		p, off = entries(`{"type":"user","message":{"content":"x"}}`, `{"type":"user","mess`)
		if _, err := laneTailRecords(p, off, needle); err == nil {
			t.Error("last: an unfinished line was read as a clean silence")
		}
	})
	t.Run("a long text is fingerprinted by its prefix", func(t *testing.T) {
		long := strings.Repeat("abcdefgh ", 400)
		n := laneNeedle(long)
		if len([]rune(n)) != laneNeedleRunes {
			t.Fatalf("needle = %d runes, want %d", len([]rune(n)), laneNeedleRunes)
		}
		p, off := entries(`{"type":"user","message":{"content":` + mustJSONString(t, long) + `}}`)
		if got, err := laneTailRecords(p, off, n); err != nil || !got {
			t.Errorf("= %v, %v; want true, nil", got, err)
		}
	})
}

func TestQuietStatus(t *testing.T) {
	for s, want := range map[fleet.Status]bool{
		fleet.StatusIdle: true, fleet.StatusWaitingInput: true, fleet.StatusQuotaBlocked: true,
		fleet.StatusWorking: false, fleet.StatusDead: false, fleet.StatusUnknown: false,
	} {
		if got := quietStatus(s); got != want {
			t.Errorf("quietStatus(%s) = %v, want %v", s, got, want)
		}
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
