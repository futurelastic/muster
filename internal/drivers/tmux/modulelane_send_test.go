package tmux

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/driver"
)

// #185, send: which sends may use a module, what each answer of the module
// becomes, and the one rule under all of it — a message goes down ONE path.

// liveRig is a rig with one session whose lane is live.
func liveRig(t *testing.T, b modtest.Behaviour) (*modRig, string) {
	t.Helper()
	r := newModRig(t, rigOptions{behaviour: b})
	sess := r.create("lane1", nil)
	r.waitLive(sess.ID)
	return r, sess.ID
}

// short is a caller's context whose deadline leaves the module's confirmation
// only a few hundred milliseconds to settle.
func short(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 1300*time.Millisecond)
}

func (r *modRig) sendCtx(ctx context.Context, id, text string, o driver.SendOptions) fleet.DeliveryReceipt {
	r.t.Helper()
	o.Submit = true
	if o.From == nil && !o.HumanRelay {
		o.From = agentFrom
	}
	got, err := r.d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: id}, text, o)
	if err != nil {
		r.t.Fatalf("Send: %v", err)
	}
	return got
}

// An agent's auto reaches the module when its lane is live: the receipt names
// the module, the text arrives LABELLED, and the built-in path is not touched.
func TestModuleSend_AutoUsesLiveLane(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.send(id, "do the thing", driver.SendOptions{})
	if got.RouteOf() != fleet.RouteModule || got.ModuleOf() != modName {
		t.Fatalf("receipt = %+v, want the module named", got)
	}
	if r.sends() != 1 || r.pastes() != 0 {
		t.Errorf("module sends = %d, built-in pastes = %d; want 1 and 0", r.sends(), r.pastes())
	}
	if !strings.HasPrefix(r.lastSendText(), panePrefix+"agent-x") || !strings.HasSuffix(r.lastSendText(), "do the thing") {
		t.Errorf("the module was given %q, want the labelled text", r.lastSendText())
	}
	if r.modCounter("send_written") != 1 || r.modCounter("send_confirmed") != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
	if r.counter("delivery."+modName+".delivered") != 1 || r.counter("delivery."+modName+".unclassified") != 0 {
		t.Errorf("delivery counters: %v", r.d.Counters())
	}
	if r.counter("route.decided.auto.module") != 1 {
		t.Errorf("route counters: %v", r.d.Counters())
	}
}

// Shapes a module has no composer for stay on the built-in path under auto.
func TestModuleSend_AutoIneligibleUsesBuiltin(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	for name, o := range map[string]driver.SendOptions{
		"resume stranded":  {ResumeIfStranded: true},
		"replace stranded": {ReplaceIfStranded: true},
	} {
		before := r.pastes()
		got := r.send(id, "x "+name, o)
		if got.RouteOf() == fleet.RouteModule {
			t.Errorf("%s: a composer-shaped send went to the module: %+v", name, got)
		}
		_ = before
	}
	// submit:false is composer-shaped too (send() sets Submit, so call directly).
	got, err := r.d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id},
		"staged only", driver.SendOptions{Submit: false, From: agentFrom})
	if err != nil {
		t.Fatal(err)
	}
	if got.RouteOf() == fleet.RouteModule {
		t.Errorf("submit:false went to the module: %+v", got)
	}
	if r.sends() != 0 {
		t.Errorf("the module saw %d sends", r.sends())
	}
}

// An explicit terminal is the terminal — never a module, from anyone.
func TestModuleSend_TerminalNeverTouchesModule(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.send(id, "typed", driver.SendOptions{Route: fleet.RouteTerminal})
	if got.RouteOf() != fleet.RouteTerminal {
		t.Fatalf("receipt = %+v, want terminal", got)
	}
	// Nor a human relay's explicit terminal: only `auto` (#257) is eligible for the lane.
	got = r.send(id, "a person typed this too", driver.SendOptions{Route: fleet.RouteTerminal, HumanRelay: true})
	if got.RouteOf() != fleet.RouteTerminal {
		t.Fatalf("receipt = %+v, want terminal", got)
	}
	if r.sends() != 0 || r.pastes() != 2 {
		t.Errorf("module sends = %d, pastes = %d; want 0 and 2", r.sends(), r.pastes())
	}
}

// A human relay's auto (marked by the service) may use a live lane, and its
// message stays UNLABELLED — it is the user's own words.
func TestModuleSend_HumanRelayAutoUsesModule(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.send(id, "approve it", driver.SendOptions{HumanRelay: true})
	if got.RouteOf() != fleet.RouteModule {
		t.Fatalf("receipt = %+v", got)
	}
	if r.lastSendText() != "approve it" {
		t.Errorf("the module was given %q, want the person's text unlabelled", r.lastSendText())
	}
}

// A forced module the session cannot use is refused — nothing written on either
// path, and never quietly served by the terminal.
func TestModuleSend_ForcedNotLiveRefused(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.d.mods.degrade(id, "test", false)
	got := r.send(id, "hello", driver.SendOptions{Route: modName})
	if got.Outcome != fleet.OutcomeRefused || got.ModuleOf() != modName {
		t.Fatalf("receipt = %+v, want a refusal naming the module", got)
	}
	if !strings.Contains(got.Reason, "Nothing was written") {
		t.Errorf("reason = %q", got.Reason)
	}
	if r.sends() != 0 || r.pastes() != 0 {
		t.Errorf("sends = %d, pastes = %d: a refused send was written somewhere", r.sends(), r.pastes())
	}
	if r.modCounter("send_refused_no_live_lane") != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}

	// A session with no lane at all is the same refusal.
	got = r.send("beta", "hello", driver.SendOptions{Route: modName})
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "no delivery lane") {
		t.Errorf("no lane: receipt = %+v", got)
	}
	// An enabled module this session's lane does not belong to.
	if r.d.deliveryModuleEnabled("relay-z") {
		t.Fatal("setup")
	}
}

// Forced module + a stranded-composer flag is refused before anything else.
func TestModuleSend_ForcedStrandedFlagRefused(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	for _, o := range []driver.SendOptions{
		{Route: modName, ResumeIfStranded: true},
		{Route: modName, ReplaceIfStranded: true},
	} {
		got := r.send(id, "hello", o)
		if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "composer") {
			t.Errorf("%+v: receipt = %+v", o, got)
		}
	}
	if r.sends() != 0 || r.pastes() != 0 {
		t.Errorf("sends = %d, pastes = %d", r.sends(), r.pastes())
	}
}

// confirmed is `queued`, never `submitted`.
func TestModuleSend_ConfirmedIsQueuedNeverSubmitted(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.send(id, "hello", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %q, want queued", got.Outcome)
	}
	if got.Outcome == fleet.OutcomeSubmitted {
		t.Fatal("a user-origin turn the runtime accepted was reported as the agent's acknowledgement")
	}
}

// queued (enqueued, the turn has not started) keeps polling until the window
// ends, then answers unknown — without degrading the lane, which did nothing
// wrong.
func TestModuleSend_QueuedPollsThenUnknown(t *testing.T) {
	queued := modtest.JSON(map[string]any{"verdict": "queued", "enqueued": true, "elapsedMs": 5, "final": false})
	r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"confirm": {Results: []json.RawMessage{queued}, DelayMs: 60},
	}})
	ctx, cancel := short(t)
	defer cancel()
	got := r.sendCtx(ctx, id, "hello", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeUnknown || got.ModuleOf() != modName {
		t.Fatalf("receipt = %+v, want unknown naming the module", got)
	}
	if n := r.fake.CountOp("confirm"); n < 2 {
		t.Errorf("confirm ran %d times; queued must be polled", n)
	}
	if v := r.view(id); !v.ClientConnected {
		t.Errorf("a merely-queued send degraded the lane: %+v", v)
	}
	if r.pastes() != 0 || r.sends() != 1 {
		t.Errorf("sends = %d, pastes = %d", r.sends(), r.pastes())
	}
}

// silent is unknown, and the message is NEVER written a second time — not by
// the retry a caller is invited to make, not on the other path. The lane is
// degraded, so a DIFFERENT message takes the built-in path until it is
// re-attached.
func TestModuleSend_SilentUnknownNoSecondWrite(t *testing.T) {
	silent := modtest.JSON(map[string]any{"verdict": "silent", "enqueued": false, "elapsedMs": 5, "final": false})
	r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"confirm": {Results: []json.RawMessage{silent}, DelayMs: 60},
	}})
	ctx, cancel := short(t)
	defer cancel()
	got := r.sendCtx(ctx, id, "the one that must not double", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("receipt = %+v", got)
	}
	if !strings.Contains(got.Reason, "NOT been sent again") {
		t.Errorf("reason = %q", got.Reason)
	}
	if v := r.view(id); v.ClientConnected {
		t.Errorf("a silent lane still reads live: %+v", v)
	}
	if r.sends() != 1 || r.pastes() != 0 {
		t.Fatalf("after the first send: module sends = %d, pastes = %d", r.sends(), r.pastes())
	}

	// The retry of the SAME text — auto, a forced terminal, a forced module, a
	// resume — is answered from the ledger and writes nothing anywhere.
	for name, o := range map[string]driver.SendOptions{
		"auto":            {},
		"terminal":        {Route: fleet.RouteTerminal},
		"module":          {Route: modName},
		"resume stranded": {ResumeIfStranded: true},
	} {
		again := r.send(id, "the one that must not double", o)
		if again.Outcome != fleet.OutcomeUnknown || again.ModuleOf() != modName {
			t.Errorf("%s: receipt = %+v, want the held unknown naming the module", name, again)
		}
	}
	if r.sends() != 1 || r.pastes() != 0 {
		t.Errorf("a retry wrote again: module sends = %d, pastes = %d", r.sends(), r.pastes())
	}

	// Different text is not held: the degraded lane sends it through the
	// built-in path.
	other := r.send(id, "a different message", driver.SendOptions{})
	if other.RouteOf() != fleet.RouteTerminal || r.pastes() != 1 {
		t.Errorf("a different message: receipt = %+v, pastes = %d", other, r.pastes())
	}
}

// The ledger asks the module once, with no wait: when it now says the message
// arrived, the answer is queued (not sent again) and the entry is dropped.
func TestModuleSend_LedgerLateConfirmAnswersQueued(t *testing.T) {
	silent := modtest.JSON(map[string]any{"verdict": "silent", "enqueued": false, "elapsedMs": 5, "final": false})
	r, id := liveRig(t, modtest.Behaviour{})
	var mu sync.Mutex
	arrived := false
	r.fake.SetHandler(func(req modtest.Request) (any, *modtest.WireError, time.Duration) {
		if req.Op != "confirm" {
			return nil, nil, 0
		}
		mu.Lock()
		defer mu.Unlock()
		if arrived {
			return map[string]any{"verdict": "confirmed", "enqueued": true, "elapsedMs": 9, "final": true}, nil, 0
		}
		return json.RawMessage(silent), nil, 50 * time.Millisecond
	})
	ctx, cancel := short(t)
	defer cancel()
	first := r.sendCtx(ctx, id, "eventually arrives", driver.SendOptions{})
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: receipt = %+v", first)
	}
	mu.Lock()
	arrived = true
	mu.Unlock()

	again := r.send(id, "eventually arrives", driver.SendOptions{})
	if again.Outcome != fleet.OutcomeQueued || again.ModuleOf() != modName || !strings.Contains(again.Reason, "not sent again") {
		t.Fatalf("receipt = %+v, want queued and not sent again", again)
	}
	if r.sends() != 1 || r.pastes() != 0 {
		t.Errorf("module sends = %d, pastes = %d", r.sends(), r.pastes())
	}
	if _, held := r.d.unconfirmedFor(id, deliveryKey("eventually arrives", agentFrom)); held {
		t.Error("the ledger entry outlived the message's arrival")
	}
}

// ...and when the module says it was rejected, the entry is dropped and the
// retry proceeds — the message never arrived.
func TestModuleSend_LedgerRejectedLetsTheRetryProceed(t *testing.T) {
	silent := modtest.JSON(map[string]any{"verdict": "silent", "enqueued": false, "elapsedMs": 5, "final": false})
	r, id := liveRig(t, modtest.Behaviour{})
	var mu sync.Mutex
	rejected := false
	r.fake.SetHandler(func(req modtest.Request) (any, *modtest.WireError, time.Duration) {
		if req.Op != "confirm" {
			return nil, nil, 0
		}
		mu.Lock()
		defer mu.Unlock()
		if rejected {
			return map[string]any{"verdict": "rejected", "enqueued": false, "elapsedMs": 9, "final": true}, nil, 0
		}
		return json.RawMessage(silent), nil, 50 * time.Millisecond
	})
	ctx, cancel := short(t)
	defer cancel()
	if first := r.sendCtx(ctx, id, "never arrived", driver.SendOptions{}); first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: %+v", first)
	}
	mu.Lock()
	rejected = true
	mu.Unlock()
	again := r.send(id, "never arrived", driver.SendOptions{})
	if again.Outcome == fleet.OutcomeUnknown && again.ModuleOf() == modName {
		t.Fatalf("the retry was held although the module says the message never arrived: %+v", again)
	}
}

// rejected by the runtime: refused, and the lane is degraded until re-attached.
func TestModuleSend_RejectedRefusedAndDegraded(t *testing.T) {
	rej := modtest.JSON(map[string]any{"verdict": "rejected", "enqueued": false, "elapsedMs": 5, "final": true})
	r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{"confirm": {Results: []json.RawMessage{rej}}}})
	got := r.send(id, "hello", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeRefused || got.ModuleOf() != modName {
		t.Fatalf("receipt = %+v", got)
	}
	if r.modCounter("send_rejected") != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
	if v := r.view(id); v.ClientConnected || v.Lane != fleet.DeliveryLaneTerminal {
		t.Errorf("view = %+v, want the lane degraded", v)
	}
	// A rejection is not an unknown: nothing is held, the next message is free.
	next := r.send(id, "another", driver.SendOptions{})
	if next.RouteOf() != fleet.RouteTerminal {
		t.Errorf("the next message: %+v, want the built-in path while degraded", next)
	}
}

// not-live on an auto send: nothing was written, so the built-in path carries
// THIS send — and only this one: once the lane re-attaches, the module is back.
func TestModuleSend_NotLiveAutoFallsBackThisSendOnly(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	var mu sync.Mutex
	refuse := true
	r.fake.SetHandler(func(req modtest.Request) (any, *modtest.WireError, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		if req.Op == "send" && refuse {
			return nil, &modtest.WireError{Code: "not-live", Message: "no client", Retryable: true}, 0
		}
		return nil, nil, 0
	})
	got := r.send(id, "first", driver.SendOptions{})
	if got.RouteOf() != fleet.RouteTerminal || r.pastes() != 1 {
		t.Fatalf("receipt = %+v, pastes = %d; want the built-in path for this send", got, r.pastes())
	}
	if r.modCounter("fallback_to_builtin") != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
	if v := r.view(id); v.ClientConnected {
		t.Errorf("a not-live answer left the lane reading live: %+v", v)
	}
	// Nothing was written to the module, so nothing is held: the same text can
	// go again, and once the lane is re-attached it goes to the module.
	mu.Lock()
	refuse = false
	mu.Unlock()
	r.d.mods.lanePass()
	r.waitLive(id)
	again := r.send(id, "first", driver.SendOptions{})
	if again.RouteOf() != fleet.RouteModule {
		t.Errorf("after the re-attach: receipt = %+v, want the module", again)
	}
}

// write-failed may have written part of the frame: unknown, never resent.
func TestModuleSend_WriteFailedUnknownNoResend(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"send": {Error: &modtest.WireError{Code: "write-failed", Message: "pipe", Retryable: false}},
	}})
	got := r.send(id, "hello", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeUnknown || got.ModuleOf() != modName {
		t.Fatalf("receipt = %+v", got)
	}
	if r.sends() != 1 || r.pastes() != 0 {
		t.Errorf("sends = %d, pastes = %d", r.sends(), r.pastes())
	}
	again := r.send(id, "hello", driver.SendOptions{})
	if again.Outcome != fleet.OutcomeUnknown || r.sends() != 1 || r.pastes() != 0 {
		t.Errorf("the retry was not held: %+v (sends %d, pastes %d)", again, r.sends(), r.pastes())
	}
}

// refused / too-large / bad-request are the same refusals the built-in path
// gives: nothing was written, and the lane is fine.
func TestModuleSend_RefusalCodesSurfaceAsRefused(t *testing.T) {
	for _, code := range []string{"refused", "too-large", "bad-request"} {
		t.Run(code, func(t *testing.T) {
			r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
				"send": {Error: &modtest.WireError{Code: code, Message: "no\nthanks\x00"}},
			}})
			got := r.send(id, "hello", driver.SendOptions{})
			if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, code) || !strings.Contains(got.Reason, "Nothing was written") {
				t.Fatalf("receipt = %+v", got)
			}
			if strings.ContainsAny(got.Reason, "\n\x00") {
				t.Errorf("a module's control bytes reached the receipt: %q", got.Reason)
			}
			if v := r.view(id); !v.ClientConnected {
				t.Errorf("a refusal degraded the lane: %+v", v)
			}
			if r.pastes() != 0 {
				t.Error("a refused send fell through to the terminal")
			}
		})
	}
}

// The authority guard runs BEFORE routing, on the sanitised text: a runtime-
// syntax message is refused whatever the route or the lane.
func TestModuleSend_AuthorityGuardBeforeRouting(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.d.mods.degrade(id, "test", false) // a forced route would otherwise refuse for the lane
	for _, text := range []string{"!id", "  !ls", "/clear", "\v!id"} {
		got := r.send(id, text, driver.SendOptions{Route: modName})
		if got.Outcome != fleet.OutcomeRefused || got.RouteOf() != "" {
			t.Errorf("%q: receipt = %+v, want the guard's own refusal, before any path", text, got)
		}
		if strings.Contains(got.Reason, "lane") {
			t.Errorf("%q: the lane was consulted before the guard: %q", text, got.Reason)
		}
	}
	if r.sends() != 0 || r.pastes() != 0 {
		t.Errorf("sends = %d, pastes = %d", r.sends(), r.pastes())
	}
}

// The label is applied before the text goes to the module, so an agent's send
// can never begin with `!` or `/`, and allowLeadingSlash is never set for it.
func TestModuleSend_LabelAppliedBeforeSend(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.send(id, "/rename tidy", driver.SendOptions{}) // a slash command any sender may deliver
	if got.RouteOf() != fleet.RouteModule {
		t.Fatalf("receipt = %+v", got)
	}
	args := r.lastSendArgs()
	if !strings.HasPrefix(args.Text, panePrefix) || strings.HasPrefix(args.Text, "/") || strings.HasPrefix(args.Text, "!") {
		t.Errorf("the module was given %q; the label must come first", args.Text)
	}
	if args.AllowLeadingSlash {
		t.Error("allowLeadingSlash was set for a labelled agent send")
	}
}

// allowLeadingSlash is the slash policy's and only a human relay's.
func TestModuleSend_AllowLeadingSlashOnlyForHumanRelay(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.send(id, "/status", driver.SendOptions{HumanRelay: true})
	if got.RouteOf() != fleet.RouteModule {
		t.Fatalf("receipt = %+v", got)
	}
	if a := r.lastSendArgs(); a.Text != "/status" || !a.AllowLeadingSlash {
		t.Errorf("human relay: args = %+v, want the slash allowed", a)
	}
	got = r.send(id, "plain words", driver.SendOptions{HumanRelay: true})
	if a := r.lastSendArgs(); a.AllowLeadingSlash {
		t.Errorf("allowLeadingSlash set for text with no slash: %+v", a)
	}
}

// The composer already holds this very text as a stranded terminal delivery: a
// module would deliver it once more. Auto goes to the terminal, whose rules
// decide; a forced module is refused.
func TestModuleSend_StrandedTerminalTextSkipsModule(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.mux.noEcho = true // the terminal delivery never confirms, so it is left stranded
	first := r.send(id, "already in the composer", driver.SendOptions{Route: fleet.RouteTerminal})
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: receipt = %+v, want a stranded terminal delivery", first)
	}
	pastes := r.pastes()

	auto := r.send(id, "already in the composer", driver.SendOptions{})
	if auto.RouteOf() == fleet.RouteModule {
		t.Errorf("auto was sent to the module although the composer holds the text: %+v", auto)
	}
	forced := r.send(id, "already in the composer", driver.SendOptions{Route: modName})
	if forced.Outcome != fleet.OutcomeRefused || forced.ModuleOf() != modName || !strings.Contains(forced.Reason, "composer") {
		t.Errorf("forced: receipt = %+v", forced)
	}
	if r.sends() != 0 {
		t.Errorf("the module was sent text that is already in the composer (%d)", r.sends())
	}
	if r.counter(counterRouteGuardTerminalUnconfirm) < 2 {
		t.Errorf("the guard did not count: %v", r.d.Counters())
	}
	_ = pastes
}

// Since #257 the inbox only ever sees a named inbox: the unset route, auto,
// terminal and a module route never try it.
func TestModuleSend_InboxEligibilityIgnoresModuleRoutes(t *testing.T) {
	for _, tc := range []struct {
		route fleet.Route
		want  bool
	}{
		{"", false}, {fleet.RouteAuto, false}, {fleet.RouteInbox, true},
		{fleet.RouteTerminal, false}, {modName, false},
	} {
		if got := inboxEligible(driver.SendOptions{Submit: true, Route: tc.route}); got != tc.want {
			t.Errorf("route %q: inboxEligible = %v, want %v", tc.route, got, tc.want)
		}
		if got := inboxEligible(driver.SendOptions{Submit: false, Route: tc.route}); got {
			t.Errorf("route %q without submit: inboxEligible = true, want false", tc.route)
		}
	}
}

// Many sessions sending at once: answers are matched by id, every send reaches
// its own lane, and none waits on another.
func TestModuleSend_ManySessionsConcurrent(t *testing.T) {
	r := newModRig(t, rigOptions{behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"send": {DelayMs: 80},
	}}})
	const n = 6
	ids := make([]string, n)
	for i := range ids {
		ids[i] = r.create(fmt.Sprintf("many%d", i), nil).ID
	}
	r.waitAllLive(ids)
	var wg sync.WaitGroup
	got := make([]fleet.DeliveryReceipt, n)
	start := time.Now()
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = r.send(ids[i], fmt.Sprintf("message %d", i), driver.SendOptions{})
		}(i)
	}
	wg.Wait()
	if took := time.Since(start); took > time.Duration(n)*80*time.Millisecond {
		t.Errorf("%d concurrent sends took %s; they queued behind each other", n, took)
	}
	for i, rc := range got {
		if rc.RouteOf() != fleet.RouteModule || rc.Outcome != fleet.OutcomeQueued {
			t.Errorf("send %d: %+v", i, rc)
		}
	}
	seen := map[string]bool{}
	lanes := map[string]bool{}
	for _, rec := range r.fake.Requests() {
		if rec.Op != "send" {
			continue
		}
		var a struct{ LaneKey, Text string }
		if err := json.Unmarshal(rec.Args, &a); err != nil {
			t.Fatal(err)
		}
		seen[a.Text] = true
		lanes[a.LaneKey] = true
	}
	if len(seen) != n || len(lanes) != n {
		t.Errorf("the module saw %d distinct texts on %d lanes, want %d of each", len(seen), len(lanes), n)
	}
}

// #264: a lane that enqueues on an idle session and never starts a turn is
// degraded after laneStallLimit sends in a row, so the next send takes the
// built-in path instead of the session staying unreachable behind a lane that
// looks live.
func TestModuleSend_StalledLaneIsDegraded(t *testing.T) {
	queued := modtest.JSON(map[string]any{"verdict": "queued", "enqueued": true, "elapsedMs": 5, "final": false})
	r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"confirm": {Results: []json.RawMessage{queued}, DelayMs: 60},
	}})
	// The session is idle, and the driver has seen it so.
	if st, err := r.d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id}); err != nil || !idleStatus(st.Status) {
		t.Fatalf("setup: state = %+v, err = %v; the rig's pane must read idle", st, err)
	}

	ctx, cancel := short(t)
	first := r.sendCtx(ctx, id, "first", driver.SendOptions{})
	cancel()
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("first = %+v, want unknown", first)
	}
	if v := r.view(id); !v.ClientConnected {
		t.Fatalf("one stalled send degraded the lane: %+v", v)
	}

	ctx, cancel = short(t)
	second := r.sendCtx(ctx, id, "second, different text", driver.SendOptions{})
	cancel()
	if second.Outcome != fleet.OutcomeUnknown || !strings.Contains(second.Reason, "now marked not live") {
		t.Fatalf("second = %+v, want unknown naming the degrade", second)
	}
	v := r.view(id)
	if v.ClientConnected || v.Lane != fleet.DeliveryLaneTerminal {
		t.Fatalf("lane after two stalls = %+v, want terminal and not connected", v)
	}
	if r.counter(counterLaneStallDegraded) != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}

	// The next send reaches the session by the built-in path.
	before := r.sends()
	third := r.send(id, "third", driver.SendOptions{})
	if third.RouteOf() == fleet.RouteModule || r.pastes() == 0 {
		t.Fatalf("third = %+v, pastes = %d; want the built-in path", third, r.pastes())
	}
	if r.sends() != before {
		t.Errorf("the degraded lane took another send")
	}
}

// A queued send on a session that was NOT idle when it went in is an ordinary
// enqueue and never counts toward degrading the lane.
func TestModuleSend_QueuedOnBusySessionNeverDegrades(t *testing.T) {
	queued := modtest.JSON(map[string]any{"verdict": "queued", "enqueued": true, "elapsedMs": 5, "final": false})
	r, id := liveRig(t, modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"confirm": {Results: []json.RawMessage{queued}, DelayMs: 60},
	}})
	for i := 0; i < laneStallLimit+1; i++ {
		r.d.mu.Lock()
		r.d.observed[id] = observation{status: fleet.StatusWorking, statusSince: r.d.now()}
		r.d.mu.Unlock()
		ctx, cancel := short(t)
		got := r.sendCtx(ctx, id, fmt.Sprintf("while busy %d", i), driver.SendOptions{})
		cancel()
		if got.Outcome != fleet.OutcomeUnknown {
			t.Fatalf("send %d = %+v, want unknown", i, got)
		}
	}
	if v := r.view(id); !v.ClientConnected {
		t.Errorf("enqueues on a busy session degraded the lane: %+v", v)
	}
	if r.counter(counterLaneStall) != 0 {
		t.Errorf("counters: %v", r.d.Counters())
	}
}

// A confirmed send ends a run of stalls.
func TestModuleSend_ConfirmedResetsStallRun(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	if n, deg := r.d.mods.noteStall(id); n != 1 || deg {
		t.Fatalf("n=%d degraded=%v", n, deg)
	}
	got := r.send(id, "lands", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("got = %+v", got)
	}
	if n, deg := r.d.mods.noteStall(id); n != 1 || deg {
		t.Fatalf("after a confirmed send the run restarted at n=%d degraded=%v, want 1/false", n, deg)
	}
}
