package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/inboxclient"
)

// #184: route auto | terminal | inbox on the driver's Send. What is pinned
// here is the driver's half of the contract — what it decides about the SESSION
// once the service has settled who is sending. The service's half is in
// internal/service.

func routeOpts(route fleet.Route) driver.SendOptions {
	return driver.SendOptions{Submit: true, Route: route}
}

// route:"inbox" is an insistence, not a preference: when the session cannot take
// it the answer is a refusal with nothing written, never a quiet trip through
// the terminal. Every reason the inbox can decline for is a row.
func TestSend_RouteInboxIneligible_RefusedNothingWritten(t *testing.T) {
	declined := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) { return InboxAddress{}, false, nil }
	resolverErr := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{}, false, errors.New("index unreadable")
	}
	bypass := attestableResolver(inboxclient.ModeBypass)

	type rig struct {
		resolver     InboxResolver
		dial         func(*inboxReceiver) inboxDialFunc
		noTranscript bool
		noProcess    bool
		text         string
	}
	cases := []struct {
		name string
		rig  rig
		exit string // "" means the inbox was never tried
		// checked/lookalike are the two #150 measurement counters.
		checked, lookalike int64
	}{
		{"no inbox configured", rig{text: "hello"}, "", 0, 0},
		{"the index has no entry", rig{resolver: declined, text: "hello"}, counterInboxFallbackResolverDeclined, 0, 0},
		{"the index cannot be read", rig{resolver: resolverErr, text: "hello"}, counterInboxFallbackResolverError, 0, 0},
		{"the process cannot be identified", rig{resolver: bypass, noProcess: true, text: "hello"}, counterInboxFallbackIdentityUnresolved, 0, 0},
		{"the index names no class", rig{resolver: attestableResolver(""), text: "hello"}, counterInboxFallbackNoModeClass, 1, 0},
		{"the text cannot be attested", rig{resolver: bypass, text: "a ＜ lookalike"}, counterInboxFallbackBodyUnattestable, 1, 1},
		{"no transcript to confirm against", rig{resolver: bypass, noTranscript: true, text: "hello"}, counterInboxFallbackNoTranscript, 1, 0},
		{"the socket cannot be dialled", rig{resolver: bypass, text: "hello", dial: func(*inboxReceiver) inboxDialFunc {
			return func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") }
		}}, counterInboxFallbackDialFailed, 1, 0},
		{"the write puts not one byte on the socket", rig{resolver: bypass, text: "hello", dial: func(*inboxReceiver) inboxDialFunc {
			return pipeDialer(t, closeBeforeReading)
		}}, counterInboxFallbackWriteFailed, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newInboxReceiver(t)
			f := twoSessions()
			ps := &fakePS{}
			if !tc.rig.noProcess {
				ps.set(100, time.Now())
			}
			ps.set(200, time.Now())
			dial := rcv.dialer(receiverRecordsEnvelope)
			if tc.rig.dial != nil {
				dial = tc.rig.dial(rcv)
			}
			var extra []Option
			if !tc.rig.noTranscript {
				extra = rcv.options()
			}
			d := newInboxTestDriverWith(f, ps, tc.rig.resolver, dial, extra...)

			got, err := d.Send(context.Background(), testCaller, alphaRef, tc.rig.text, routeOpts(fleet.RouteInbox))
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeRefused {
				t.Fatalf("outcome = %q (%s), want refused", got.Outcome, got.Reason)
			}
			if got.RouteOf() != fleet.RouteInbox {
				t.Errorf("route = %q, want inbox: the refusal is the inbox's answer", got.RouteOf())
			}
			if !strings.Contains(got.Reason, "Nothing was written") {
				t.Errorf("reason = %q, want it to say nothing was written", got.Reason)
			}
			if paneTouched(f) {
				t.Errorf("an explicit inbox request reached the pane: %v", f.callsSnapshot())
			}
			if n := rcv.bytesReceived(); n != 0 {
				t.Errorf("%d bytes reached the inbox for a send that was refused", n)
			}
			if tc.exit == "" {
				assertNoInboxCounters(t, d)
			} else {
				assertInboxExit(t, d, tc.exit, tc.checked, tc.lookalike)
			}
			if c := d.Counters(); c["route.auto_fallback"] != 0 {
				t.Errorf("route.auto_fallback = %d: that counter was retired in #257", c["route.auto_fallback"])
			}
		})
	}
}

// The same shape rule the service applies, restated by the driver because a
// driver can be called without the service in front of it.
func TestSend_RouteInboxShapeRefusedByTheDriverToo(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts driver.SendOptions
	}{
		{"submit false", driver.SendOptions{Route: fleet.RouteInbox}},
		{"resume", driver.SendOptions{Submit: true, ResumeIfStranded: true, Route: fleet.RouteInbox}},
		{"replace", driver.SendOptions{Submit: true, ReplaceIfStranded: true, Route: fleet.RouteInbox}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newInboxReceiver(t)
			resolverCalled := false
			resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
				resolverCalled = true
				return InboxAddress{}, false, nil
			}
			d, f := newRoutedDriver(t, rcv, resolver, rcv.dialer(receiverRecordsEnvelope))
			got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeRefused || got.RouteOf() != fleet.RouteInbox {
				t.Fatalf("got %+v, want a refusal naming the inbox", got)
			}
			if resolverCalled || paneTouched(f) || rcv.dialCount() != 0 {
				t.Error("a request that cannot be an inbox delivery must touch nothing")
			}
		})
	}
}

func TestSend_InvalidRouteRefusedNamingNoPath(t *testing.T) {
	rcv := newInboxReceiver(t)
	d, f := newRoutedDriver(t, rcv, attestableResolver(inboxclient.ModeBypass), rcv.dialer(receiverRecordsEnvelope))
	got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts("carrier-pigeon"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q, want refused", got.Outcome)
	}
	if got.Delivery != nil {
		t.Errorf("delivery = %+v; a refusal made before any path was chosen names none", got.Delivery)
	}
	if paneTouched(f) || rcv.dialCount() != 0 {
		t.Error("an unrecognised route touched something")
	}
}

// A forced terminal route never so much as consults the inbox — the resolver is
// not asked, nothing is dialled.
func TestSend_RouteTerminalNeverConsultsTheInbox(t *testing.T) {
	rcv := newInboxReceiver(t)
	resolverCalled := false
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		resolverCalled = true
		return InboxAddress{Network: "unix", Socket: "/x", Token: "t", ModeClass: inboxclient.ModeBypass}, true, nil
	}
	d, f := newRoutedDriver(t, rcv, resolver, rcv.dialer(receiverRecordsEnvelope))

	got, err := d.Send(context.Background(), testCaller, alphaRef, "a person's message",
		driver.SendOptions{Submit: true, Route: fleet.RouteTerminal, HumanRelay: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued || got.RouteOf() != fleet.RouteTerminal {
		t.Fatalf("got %+v, want a queued terminal delivery", got)
	}
	if resolverCalled || rcv.dialCount() != 0 {
		t.Error("route terminal consulted the inbox")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pasteLog) != 1 || strings.HasPrefix(f.pasteLog[0], panePrefix) {
		t.Errorf("pasteLog = %q, want one unlabelled paste — a human relay arrives as the user", f.pasteLog)
	}
}

// #257: auto never tries the inbox. Even where the inbox would take the
// message, auto goes to the terminal (no live lane here), and the retired
// route.auto_fallback counter never appears.
func TestSend_AutoNeverTriesTheInbox(t *testing.T) {
	rcv := newInboxReceiver(t)
	resolverCalls := 0
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		resolverCalls++
		return InboxAddress{Network: "unix", Socket: "/x", Token: "t", ModeClass: inboxclient.ModeBypass}, true, nil
	}
	d, f := newRoutedDriver(t, rcv, resolver, rcv.dialer(receiverRecordsEnvelope))

	for _, opts := range []driver.SendOptions{routeOpts(fleet.RouteAuto), func() driver.SendOptions {
		o := routeOpts(fleet.RouteAuto)
		o.HumanRelay = true
		return o
	}()} {
		got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", opts)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != fleet.OutcomeQueued || got.RouteOf() != fleet.RouteTerminal {
			t.Fatalf("got %+v, want queued via the terminal", got)
		}
	}
	if !paneTouched(f) {
		t.Error("auto did not reach the pane")
	}
	if resolverCalls != 0 {
		t.Errorf("the inbox resolver was called %d times for auto sends", resolverCalls)
	}
	c := d.Counters()
	if _, ok := c["route.auto_fallback"]; ok {
		t.Errorf("route.auto_fallback is present: %v", c)
	}
	if c["route.decided.auto.terminal"] != 2 {
		t.Errorf("route.decided.auto.terminal = %d, want 2", c["route.decided.auto.terminal"])
	}
}

// Every Send is counted under route.* exactly once, whichever path it took —
// including the ones refused before any path was chosen.
func TestSend_RouteCountersExactlyOncePerSend(t *testing.T) {
	rcv := newInboxReceiver(t)
	var mu sync.Mutex
	decline := false
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if decline {
			return InboxAddress{}, false, nil
		}
		return InboxAddress{Network: "unix", Socket: "/x", Token: "t", ModeClass: inboxclient.ModeBypass}, true, nil
	}
	setDecline := func(v bool) { mu.Lock(); decline = v; mu.Unlock() }
	d, _ := newRoutedDriver(t, rcv, resolver, rcv.dialer(receiverRecordsEnvelope))

	send := func(text string, opts driver.SendOptions) fleet.DeliveryReceipt {
		t.Helper()
		got, err := d.Send(context.Background(), testCaller, alphaRef, text, opts)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	send("one", routeOpts(fleet.RouteInbox))     // inbox, delivered
	setDecline(true)                             //
	send("two", routeOpts(fleet.RouteAuto))      // auto → terminal (the inbox is never tried)
	send("three", routeOpts(fleet.RouteInbox))   // inbox → refused
	setDecline(false)                            //
	send("four", routeOpts(fleet.RouteInbox))    // inbox → delivered
	send("five", routeOpts(fleet.RouteTerminal)) // terminal
	send("!ls", routeOpts(fleet.RouteAuto))      // refused by the syntax guard: no path

	c := d.Counters()
	want := map[string]int64{
		"route.decided.auto.terminal":     1,
		"route.decided.inbox.inbox":       3,
		"route.decided.terminal.terminal": 1,
		"route.decided.auto.none":         1,
		"route.outcome.inbox.delivered":   2,
		"route.outcome.inbox.refused":     1,
		"route.outcome.terminal.queued":   2,
	}
	var decided int64
	for name, n := range c {
		if strings.HasPrefix(name, "route.decided.") {
			decided += n
		}
	}
	if decided != 6 {
		t.Errorf("route.decided.* sums to %d for 6 sends: %v", decided, c)
	}
	for name, n := range want {
		if c[name] != n {
			t.Errorf("%s = %d, want %d", name, c[name], n)
		}
	}
	if c["delivery.tmux.unclassified"] != 0 {
		t.Errorf("delivery.tmux.unclassified = %d, want 0: every terminal result says what it amounted to", c["delivery.tmux.unclassified"])
	}
	// written = confirmed + unconfirmed, so a read checks itself.
	if c[counterInboxWritten] != c[counterInboxConfirmed]+c[counterInboxUnconfirmed] {
		t.Errorf("inbox.written=%d confirmed=%d unconfirmed=%d", c[counterInboxWritten], c[counterInboxConfirmed], c[counterInboxUnconfirmed])
	}
}

// flipPS answers every `ps` with one start time until call number flipAt, and
// with another from then on — a pid recycled between two verifications.
type flipPS struct {
	mu     sync.Mutex
	calls  int
	flipAt int
	before time.Time
	after  time.Time
}

func (p *flipPS) exec(_ context.Context, _ string, _ ...string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	at := p.before
	if p.calls >= p.flipAt {
		at = p.after
	}
	return []byte(at.Format(psStartTimeLayout) + "\n"), nil
}

// Identity is verified twice: where ADR 148 needs it (before attestation, so a
// fallback can never mask it) and immediately before the dial (#116), after the
// work that takes time. Either failing is the same FINAL refusal on the named
// inbox, with nothing dialled and no fallback. (Route auto no longer reaches
// the inbox since #257, so there is no auto variant of this failure to test.)
func TestSend_InboxIdentityVerifiedTwice_EitherFailureIsFinal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flipAt  int
		checked int64 // attestation runs between the two verifications
	}{
		{"the first verification fails", 2, 0},
		{"the second verification fails", 3, 1},
	} {
		for _, route := range []fleet.Route{fleet.RouteInbox} {
			t.Run(tc.name+"/"+string(route), func(t *testing.T) {
				rcv := newInboxReceiver(t)
				ps := &flipPS{flipAt: tc.flipAt, before: time.Unix(1785700000, 0), after: time.Unix(1785700999, 0)}
				f := twoSessions()
				d := newInboxTestDriverWith(f, &fakePS{}, attestableResolver(inboxclient.ModeBypass), rcv.dialer(receiverRecordsEnvelope), rcv.options()...)
				d.psRun = ps.exec

				got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(route))
				if err != nil {
					t.Fatal(err)
				}
				if got.Outcome != fleet.OutcomeRefused || got.RouteOf() != fleet.RouteInbox {
					t.Fatalf("got %+v, want a final refusal naming the inbox", got)
				}
				if !strings.Contains(got.Reason, "nothing was sent") {
					t.Errorf("reason = %q", got.Reason)
				}
				if rcv.dialCount() != 0 || paneTouched(f) {
					t.Error("a failed identity check must reach neither the inbox nor the pane")
				}
				assertInboxExit(t, d, counterInboxRefusedIdentityUnverified, tc.checked, 0)
			})
		}
	}
}

// A write that breaks after SOME bytes went out is not a fallback: the receiver
// may have been given the message, so the outcome is unknown, on the inbox's
// account, and the terminal is not tried — for a failure anywhere in either line.
func TestSend_InboxPartialWrite_UnknownNoPane(t *testing.T) {
	auth, _ := json.Marshal(map[string]string{"type": "auth", "token": "tok"})
	authLen := len(auth) + 1
	for _, tc := range []struct {
		name   string
		budget int
	}{
		{"inside the auth line", 3},
		{"the whole auth line and none of the message", authLen},
		{"part-way through the message", authLen + 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newInboxReceiver(t)
			d, f := newRoutedDriver(t, rcv, attestableResolver(inboxclient.ModeBypass), rcv.budgetDialer(tc.budget))

			got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(fleet.RouteInbox))
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeUnknown || got.RouteOf() != fleet.RouteInbox {
				t.Fatalf("got %+v, want unknown on the inbox's account", got)
			}
			if !strings.Contains(got.Reason, "NOT sent again") {
				t.Errorf("reason = %q, want it to say the message was not sent again", got.Reason)
			}
			if paneTouched(f) {
				t.Fatal("a write that put bytes on the socket fell back to the pane: the message could be delivered twice")
			}
			assertInboxExit(t, d, counterInboxUnknownPartialWrite, 1, 0)
			key := deliveryKey("hello", nil)
			if e, ok := d.unconfirmedFor("alpha💬", key); !ok || !e.Partial {
				t.Errorf("no partial ledger entry was kept (%+v, %v)", e, ok)
			}
		})
	}
}

// The create-time prompt is the creator's launch instruction, not a peer's
// message: it never consults the inbox, whatever the inbox could do. Since #368
// its first attempt is `auto` (a live delivery lane carries it; with none, the
// terminal does) and since #257 `auto` does not try the inbox. The retry stays
// pinned to the terminal, so it can never re-send text an inbox already put in
// front of the receiver.
func TestDeliverInitialPrompt_NeverConsultsTheInbox(t *testing.T) {
	rcv := newInboxReceiver(t)
	resolverCalled := false
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		resolverCalled = true
		return InboxAddress{Network: "unix", Socket: "/x", Token: "t", ModeClass: inboxclient.ModeBypass}, true, nil
	}
	d, f := newRoutedDriver(t, rcv, resolver, rcv.dialer(receiverRecordsEnvelope))

	d.deliverInitialPrompt(context.Background(), testCaller, alphaRef, "start working")

	if resolverCalled || rcv.dialCount() != 0 {
		t.Fatal("the create-time prompt consulted the inbox")
	}
	if !paneTouched(f) {
		t.Fatal("the create-time prompt never reached the terminal")
	}
	if c := d.Counters(); c["route.decided.auto.terminal"] < 1 {
		t.Errorf("the prompt was not sent as auto and carried by the terminal (no lane here): %v", c)
	}
}
