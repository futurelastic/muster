package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery/modclient"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/driver"
)

// #185, a lane's life after create: the module dying under it, this service
// restarting around it, the session being renamed or closed, and the two facts
// — the peer check and the peer verification — that make a lane not live.

var modPaneCreated = time.Unix(1785600002, 0)

func closeReq() fleet.Request {
	req := testCaller
	req.Expect.StartedAt = &modPaneCreated
	return req
}

// The module dies mid-run: sends fall back to the built-in path with nothing
// lost, the client restarts it with backoff, and every laned session is
// re-attached before the module is used for it again.
func TestModuleLifecycle_KillMidRunFallsBackRestartsReattaches(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	if first := r.send(id, "before", driver.SendOptions{}); first.RouteOf() != fleet.RouteModule {
		t.Fatalf("setup: %+v", first)
	}

	// Keep it down for a while (each failed launch backs off), so the window
	// is wide enough to act in.
	r.fake.FailNextLaunches(12)
	r.fake.Kill()
	waitFor(t, "the module to be seen down", func() bool { return !r.client().Usable() })
	// Down: nothing is lost, the built-in path carries it.
	during := r.send(id, "while it is down", driver.SendOptions{})
	if during.RouteOf() != fleet.RouteTerminal || r.pastes() != 1 {
		t.Fatalf("receipt = %+v, pastes = %d; want the built-in path", during, r.pastes())
	}
	if r.modCounter("fallback_to_builtin") < 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}

	// Restarted with backoff, and the lane is re-attached under the new process.
	waitFor(t, "a restart", func() bool { return r.fake.Starts() >= 2 })
	r.waitLive(id)
	if r.modCounter("restarted") < 1 || r.modCounter("reattach_on_restart") < 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
	after := r.send(id, "after", driver.SendOptions{})
	if after.RouteOf() != fleet.RouteModule {
		t.Errorf("after the restart: %+v, want the module again", after)
	}
	// The re-attach carried the lane the create prepared: no new lane.
	rec, _ := r.record(id)
	var attachedKeys []string
	for _, q := range r.fake.Requests() {
		if q.Op == "attach" {
			var a modclient.AttachArgs
			mustUnmarshal(t, q.Args, &a)
			attachedKeys = append(attachedKeys, a.LaneKey)
		}
	}
	for _, k := range attachedKeys {
		if k != rec.LaneKey {
			t.Errorf("attach used lane key %q, want %q", k, rec.LaneKey)
		}
	}
	if n := r.fake.CountOp("prepare-launch"); n != 1 {
		t.Errorf("prepare-launch ran %d times; a restart must not make new lanes", n)
	}
}

// A service restart: the lane record survives, the module is fresh, and the
// order is New → ReconcileLanes → Start. An adopted session is re-attached; a
// vanished one is closed and dropped.
func TestModuleLifecycle_ReconcileAttachesAdoptedClosesVanished(t *testing.T) {
	first := newModRig(t, rigOptions{})
	kept := first.create("kept1", nil)
	gone := first.create("gone1", nil)
	first.waitLive(kept.ID)
	first.waitLive(gone.ID)
	keptRec, _ := first.record(kept.ID)
	goneRec, _ := first.record(gone.ID)
	first.d.StopDeliveryModules()

	// The same machine, a new process: same state directory, fresh module.
	second := newModRig(t, rigOptions{storeDir: first.dir, noStart: true})
	second.mux.addSession(fakeSession{name: kept.ID, paneID: "%77", cwd: "/work/kept1", pid: keptRec.PID, created: 1785600002, title: "2_1_220"}, idleFixtureFor("kept1"))
	if v := second.view(kept.ID); v == nil || v.ClientConnected {
		t.Fatalf("view before the module is ready = %+v, want the built-in lane", v)
	}
	second.d.ReconcileLanes(Reconciliation{Vanished: []fleet.Session{{SessionRef: fleet.SessionRef{ID: gone.ID}}}})
	second.d.StartDeliveryModules()
	second.waitUsable()

	second.waitLive(kept.ID)
	waitFor(t, "the vanished lane to be closed", func() bool { return second.fake.CountOp("close") == 1 })
	waitFor(t, "the vanished record to be dropped", func() bool { _, ok := second.record(gone.ID); return !ok })
	for _, q := range second.fake.Requests() {
		switch q.Op {
		case "close":
			var c modclient.CloseArgs
			mustUnmarshal(t, q.Args, &c)
			if c.LaneKey != goneRec.LaneKey {
				t.Errorf("closed lane %q, want the vanished one %q", c.LaneKey, goneRec.LaneKey)
			}
		case "attach":
			var a modclient.AttachArgs
			mustUnmarshal(t, q.Args, &a)
			if a.LaneKey != keptRec.LaneKey || a.PID != keptRec.PID {
				t.Errorf("attach = %+v, want the adopted session's lane and pid", a)
			}
		}
	}
	if n := second.fake.CountOp("prepare-launch"); n != 0 {
		t.Errorf("a restart prepared %d new lanes", n)
	}
	if second.modCounter("reattach_on_restart") != 1 {
		t.Errorf("counters: %v", second.d.Counters())
	}
	// The adopted session sends over its lane again.
	if got := second.send(kept.ID, "welcome back", driver.SendOptions{}); got.RouteOf() != fleet.RouteModule {
		t.Errorf("receipt = %+v", got)
	}
}

// muster #321: the whole path, with the real Reconcile in front of the lanes.
// A restart whose start-time read fails must not close a single lane for a
// session that is live. Only the session that is actually gone is closed.
func TestModuleLifecycle_FailedStartTimeReadClosesNoLiveLane(t *testing.T) {
	first := newModRig(t, rigOptions{})
	kept := first.create("kept1", nil)
	gone := first.create("gone1", nil)
	first.waitLive(kept.ID)
	first.waitLive(gone.ID)
	keptRec, _ := first.record(kept.ID)
	first.d.StopDeliveryModules()

	var second *modRig
	var reads atomic.Int32
	flaky := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list-panes" && reads.Add(1) >= 2 {
			return nil, errors.New("multiplexer read timed out")
		}
		return second.mux.exec(ctx, name, args...)
	}
	second = newModRig(t, rigOptions{storeDir: first.dir, noStart: true, driverOpts: []Option{withExec(flaky)}})
	second.mux.addSession(fakeSession{name: kept.ID, paneID: "%77", cwd: "/work/kept1", pid: keptRec.PID, created: 1785600002, title: "2_1_220"}, idleFixtureFor("kept1"))

	rec, err := second.d.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.ReadErr == "" {
		t.Fatalf("setup: the start-time read did not fail: %s", rec)
	}
	for _, v := range rec.Vanished {
		if v.ID == kept.ID {
			t.Fatalf("%s — a live, unchanged session was reported vanished", rec)
		}
	}
	second.d.ReconcileLanes(rec)
	second.d.StartDeliveryModules()
	second.waitUsable()

	second.waitLive(kept.ID)
	waitFor(t, "the gone session's lane to be closed", func() bool { return second.fake.CountOp("close") == 1 })
	for _, q := range second.fake.Requests() {
		if q.Op == "close" {
			var c modclient.CloseArgs
			mustUnmarshal(t, q.Args, &c)
			if c.LaneKey == keptRec.LaneKey {
				t.Errorf("the live session's lane %q was closed by a failed read", c.LaneKey)
			}
		}
	}
	if _, ok := second.record(kept.ID); !ok {
		t.Error("the live session's lane record was dropped")
	}
}

// An agent process that was replaced (a different pane pid) does not inherit the
// old lane: it is closed and dropped, never re-attached.
func TestModuleLifecycle_PidMismatchCloses(t *testing.T) {
	first := newModRig(t, rigOptions{})
	s := first.create("swap1", nil)
	first.waitLive(s.ID)
	rec, _ := first.record(s.ID)
	first.d.StopDeliveryModules()

	second := newModRig(t, rigOptions{storeDir: first.dir, noStart: true})
	second.mux.addSession(fakeSession{name: s.ID, paneID: "%78", cwd: "/work/swap1", pid: rec.PID + 1000, created: 1785600002, title: "2_1_220"}, idleFixtureFor("swap1"))
	second.d.StartDeliveryModules()
	second.waitUsable()
	waitFor(t, "the stale lane to be closed", func() bool { return second.fake.CountOp("close") == 1 })
	if n := second.fake.CountOp("attach"); n != 0 {
		t.Errorf("a lane belonging to a replaced process was re-attached %d times", n)
	}
	waitFor(t, "the record to be dropped", func() bool { _, ok := second.record(s.ID); return !ok })
}

// Close: the built-in kill is the authority; the module hears about it, and its
// failure never blocks it.
func TestModuleLifecycle_CloseCallsModuleCloseAndFailureNeverBlocks(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	rec, _ := r.record(id)
	ack, err := r.d.Close(context.Background(), closeReq(), fleet.SessionRef{Machine: "testbox", ID: id})
	if err != nil || !ack.Accepted {
		t.Fatalf("Close: %+v %v", ack, err)
	}
	waitFor(t, "the module to be told", func() bool { return r.fake.CountOp("close") == 1 })
	waitFor(t, "the record to be dropped", func() bool { _, ok := r.record(id); return !ok })
	var c modclient.CloseArgs
	for _, q := range r.fake.Requests() {
		if q.Op == "close" {
			mustUnmarshal(t, q.Args, &c)
		}
	}
	if c.LaneKey != rec.LaneKey {
		t.Errorf("closed %q, want %q", c.LaneKey, rec.LaneKey)
	}
	if r.modCounter("closed") != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}

	// A module that answers close with an error does not block or undo it.
	r2, id2 := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"close": {Error: &modtest.WireError{Code: "internal", Message: "no"}},
	}})
	ack, err = r2.d.Close(context.Background(), closeReq(), fleet.SessionRef{Machine: "testbox", ID: id2})
	if err != nil || !ack.Accepted {
		t.Fatalf("a failing module blocked the close: %+v %v", ack, err)
	}
	if countCalls(r2.mux, "kill-session") != 1 {
		t.Error("the session was not killed")
	}
}

// A module that is down when a session closes still hears about it: the close
// is remembered and flushed when it is next ready.
func TestModuleLifecycle_CloseFlushedWhenModuleReady(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.fake.FailNextLaunches(3)
	r.fake.Kill()
	waitFor(t, "the module to be seen down", func() bool { return !r.client().Usable() })
	if ack, err := r.d.Close(context.Background(), closeReq(), fleet.SessionRef{Machine: "testbox", ID: id}); err != nil || !ack.Accepted {
		t.Fatalf("Close with the module down: %+v %v", ack, err)
	}
	rec, ok := r.record(id)
	if !ok || !rec.Closing {
		t.Fatalf("record = %+v, %v; want it kept, marked closing", rec, ok)
	}
	waitFor(t, "the close to be flushed", func() bool { return r.fake.CountOp("close") == 1 })
	waitFor(t, "the record to be dropped", func() bool { _, ok := r.record(id); return !ok })
}

// A rename keeps the lane: it belongs to the process, not the name.
func TestModuleLifecycle_RenameKeepsLane(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	ctx := context.Background()
	if _, err := r.d.List(ctx, testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	rec, _ := r.record(id)
	if _, err := r.d.Rename(ctx, closeReq(), fleet.SessionRef{Machine: "testbox", ID: id}, "renamed1"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, ok := r.record(id); ok {
		t.Error("the old name still holds the record")
	}
	moved, ok := r.record("renamed1")
	if !ok || moved.LaneKey != rec.LaneKey {
		t.Fatalf("the renamed session's record = %+v, %v", moved, ok)
	}
	got := r.send("renamed1", "hello", driver.SendOptions{})
	if got.RouteOf() != fleet.RouteModule {
		t.Errorf("receipt = %+v", got)
	}
	if a := r.lastSendArgs(); a.LaneKey != rec.LaneKey {
		t.Errorf("sent on lane %q, want %q", a.LaneKey, rec.LaneKey)
	}
}

// peerCheck:false — the module cannot verify who is on the other end — is not
// live, whatever else it says. Nothing is delivered without the check.
func TestModuleLifecycle_PeerCheckFalseIsNotLive(t *testing.T) {
	noPeer := modtest.JSON(map[string]any{
		"ok": true, "module": "fake", "protocol": 1, "version": "0.0.1", "platform": "test",
		"peerCheck": false, "reservedEnvPrefixes": []string{}, "lanes": []any{}, "counters": map[string]any{}})
	r := newModRig(t, rigOptions{behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"health": {Results: []json.RawMessage{noPeer}},
	}}})
	sess := r.create("peer1", nil)
	waitFor(t, "the attach", func() bool { return r.fake.CountOp("attach") >= 1 })
	time.Sleep(50 * time.Millisecond)
	v := r.view(sess.ID)
	if v.ClientConnected || v.Lane != fleet.DeliveryLaneTerminal || !strings.Contains(v.Evidence, "peerCheck") {
		t.Fatalf("view = %+v, want not live, with the reason", v)
	}
	if st := r.d.Capabilities().DeliveryModules; len(st) != 1 || st[0].PeerCheck == nil || *st[0].PeerCheck {
		t.Errorf("capabilities = %+v, want peerCheck reported false", st)
	}
	// Auto uses the built-in path; a forced module is refused.
	if got := r.send(sess.ID, "hello", driver.SendOptions{}); got.RouteOf() != fleet.RouteTerminal {
		t.Errorf("auto: %+v", got)
	}
	if got := r.send(sess.ID, "hello", driver.SendOptions{Route: modName}); got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "peerCheck") {
		t.Errorf("forced: %+v", got)
	}
	if r.sends() != 0 {
		t.Errorf("the module was sent %d messages over a lane it cannot verify", r.sends())
	}
}

// An attach that reports peerVerified:false is not live either.
func TestModuleLifecycle_PeerVerifiedFalseIsNotLive(t *testing.T) {
	r := newModRig(t, rigOptions{behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"attach": {Results: []json.RawMessage{modtest.JSON(map[string]any{"live": true, "state": "live", "peerVerified": false, "sinceMs": 1})}},
	}}})
	sess := r.create("peer2", nil)
	waitFor(t, "the attach", func() bool { return r.modCounter("attach_live") == 1 })
	v := r.view(sess.ID)
	if v.ClientConnected || !strings.Contains(v.Evidence, "not verified") {
		t.Fatalf("view = %+v", v)
	}
	if got := r.send(sess.ID, "x", driver.SendOptions{Route: modName}); got.Outcome != fleet.OutcomeRefused || r.sends() != 0 {
		t.Errorf("forced: %+v (sends %d)", got, r.sends())
	}
}

// The background pass is the only way a degraded lane returns to service.
func TestModuleLifecycle_HealthTickReattachesDegraded(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.d.mods.degrade(id, "test", false)
	if r.view(id).ClientConnected {
		t.Fatal("setup: still live")
	}
	if got := r.send(id, "while degraded", driver.SendOptions{}); got.RouteOf() != fleet.RouteTerminal {
		t.Fatalf("degraded lane carried a send: %+v", got)
	}
	before := r.fake.CountOp("attach")
	r.d.mods.lanePass()
	r.waitLive(id)
	if r.fake.CountOp("attach") != before+1 {
		t.Errorf("attach ran %d times, want one more", r.fake.CountOp("attach")-before)
	}
	if got := r.send(id, "recovered", driver.SendOptions{}); got.RouteOf() != fleet.RouteModule {
		t.Errorf("after the re-attach: %+v", got)
	}
	// `gone` is not retried: the module has lost the lane and only a resume
	// relaunch gets a new one.
	r.d.mods.degrade(id, "lost", true)
	before = r.fake.CountOp("attach")
	r.d.mods.lanePass()
	if r.fake.CountOp("attach") != before {
		t.Error("a lane the module reports gone was re-attached")
	}
}

// The pass closes lanes whose session no longer exists.
func TestModuleLifecycle_VanishedSessionLaneClosedByPass(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.mux.dropSession(id)
	r.d.mods.lanePass()
	waitFor(t, "the lane to be closed", func() bool { return r.fake.CountOp("close") == 1 })
	waitFor(t, "the record to be dropped", func() bool { _, ok := r.record(id); return !ok })
}

// A slow attach on one lane does not delay a send on another.
func TestModuleLifecycle_SlowAttachDoesNotDelayAnotherLanesSend(t *testing.T) {
	r := newModRig(t, rigOptions{})
	fast := r.create("fast1", nil)
	r.waitLive(fast.ID)
	g := newGate()
	var mu sync.Mutex
	hold := true
	r.fake.SetHandler(func(req modtest.Request) (any, *modtest.WireError, time.Duration) {
		mu.Lock()
		h := hold
		mu.Unlock()
		if req.Op == "attach" && h {
			g.wait(req.Ctx)
		}
		return nil, nil, 0
	})
	slow := r.create("slow1", nil)
	start := time.Now()
	got := r.send(fast.ID, "not delayed", driver.SendOptions{})
	if got.RouteOf() != fleet.RouteModule || time.Since(start) > 2*time.Second {
		t.Errorf("receipt = %+v after %s: the slow attach held up another lane's send", got, time.Since(start))
	}
	mu.Lock()
	hold = false
	mu.Unlock()
	g.release()
	r.waitLive(slow.ID)
}

// #248: a lane bound to one agent process, whose session now runs a different
// one (replaced outside this service), is refused by the module on every
// attach. After the one refusal the lane is marked gone with a reason on the
// session's delivery field and the background pass stops retrying it.
func TestModuleLifecycle_ReplacedProcessStopsRetrying(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	rec, _ := r.record(id)
	if rec.PID == 0 {
		t.Fatal("setup: the lane was never bound to a process")
	}
	// The module now refuses any attach, as it does for a process it did not bind.
	r.fake.SetHandler(func(req modtest.Request) (any, *modtest.WireError, time.Duration) {
		if req.Op == "attach" {
			return nil, &modtest.WireError{Code: "refused", Message: "lane bound to another process"}, 0
		}
		return nil, nil, 0
	})
	// Same session name, new process.
	r.mux.dropSession(id)
	r.mux.addSession(fakeSession{name: id, paneID: "%900", cwd: "/work/lane1", pid: rec.PID + 1000, created: 1785600003, title: "2_1_220"}, idleFixtureFor(id))
	r.d.mods.degrade(id, "the process behind it changed", false)

	before := r.fake.CountOp("attach")
	r.d.mods.lanePass()
	if n := r.fake.CountOp("attach") - before; n != 1 {
		t.Fatalf("first pass made %d attach calls, want 1", n)
	}
	got, _ := r.record(id)
	if got.State != laneStateGone || got.Live || got.Reason != laneReasonReplaced {
		t.Fatalf("lane after the refusal = state %q live %v reason %q", got.State, got.Live, got.Reason)
	}
	if got.PID != rec.PID {
		t.Errorf("the lane's bound pid moved from %d to %d", rec.PID, got.PID)
	}
	if v := r.view(id); v == nil || v.ClientConnected || !strings.Contains(v.Evidence, laneReasonReplaced) {
		t.Errorf("state read does not carry the reason: %+v", v)
	}
	for i := 0; i < 3; i++ {
		r.d.mods.lanePass()
	}
	if n := r.fake.CountOp("attach") - before; n != 1 {
		t.Errorf("the lane was retried: %d attach calls in total, want 1", n)
	}
	if n := r.modCounter("attach_replaced"); n != 1 {
		t.Errorf("attach_replaced = %d, want 1", n)
	}
}

// A refusal with the same process the lane was bound to is NOT a replacement:
// it keeps the ordinary failure path and the lane stays retryable.
func TestModuleLifecycle_RefusedSameProcessIsNotReplaced(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.fake.SetHandler(func(req modtest.Request) (any, *modtest.WireError, time.Duration) {
		if req.Op == "attach" {
			return nil, &modtest.WireError{Code: "refused", Message: "no"}, 0
		}
		return nil, nil, 0
	})
	r.d.mods.degrade(id, "test", false)
	r.d.mods.lanePass()
	got, _ := r.record(id)
	if got.State == laneStateGone || got.Reason == laneReasonReplaced {
		t.Errorf("a refusal for the unchanged process marked the lane replaced: %+v", got)
	}
	if !strings.HasPrefix(got.Reason, "attach failed") {
		t.Errorf("reason = %q, want the ordinary attach failure", got.Reason)
	}
}
