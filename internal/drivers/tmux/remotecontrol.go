package tmux

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Turning a RUNNING session's remote control on or off (driver.RemoteControlSetter,
// muster #269).
//
// # What the runtime does, measured rather than assumed
//
// On the runtime this was built against (2.1.293), measured live through this
// driver:
//
//   - `/remote-control` (alias `/rc`) on a session whose remote control is OFF
//     turns it on, and the runtime writes a `bridge_status` entry.
//   - The SAME command on a session whose remote control is already ON does not
//     toggle: it opens a dialog — "Disconnect this session", "Show QR code",
//     "Continue" — with Continue highlighted.
//   - Answering that dialog with the Disconnect option turns remote control off
//     ("Remote Control disconnected."), and the command turns it on again.
//
// So the off path exists, but it is a conversation with a dialog, and the on
// path is dangerous to send blind: where the channel is already on it leaves a
// dialog open on the session. That is why every branch below reads the channel
// FIRST and refuses to act on a state it cannot read.
//
// # Composed from the verbs that already carry the safety
//
// Nothing here types into a pane directly. The command goes through Send (which
// takes the composer lock, refuses a busy composer and never submits text that
// is not its own) and the dialog is answered through Respond (which checks the
// prompt's nonce, so a stale answer fails rather than landing on a different
// dialog). The disconnect option is found by its TEXT, never by position: a
// runtime that reorders its options must not turn Disconnect into Continue.

// disconnectOptionText is what the runtime's dialog calls the option that turns
// remote control off.
const disconnectOptionText = "disconnect this session"

// rcDialogWindow bounds how long the toggle waits for the dialog (or for the
// channel to settle) before reporting what it saw.
const rcDialogWindow = 6 * time.Second

// rcLocks serialises toggles on one session: two concurrent toggles would each
// read the same state and then each send the command, the second of which opens
// a dialog the first did not expect.
type rcLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (l *rcLocks) lock(id string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sync.Mutex{}
	}
	m := l.m[id]
	if m == nil {
		m = &sync.Mutex{}
		l.m[id] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// disconnectOption returns the 1-based index of the disconnect option in a
// prompt, or 0.
func disconnectOption(p *fleet.SessionPrompt) int {
	if p == nil {
		return 0
	}
	for i, o := range p.Options {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(o)), disconnectOptionText) {
			return i + 1
		}
	}
	return 0
}

func rcConflict(format string, args ...any) error {
	return &fleet.Error{Kind: fleet.ErrorConflict, Message: fmt.Sprintf(format, args...), Retryable: true}
}

// SetRemoteControl implements driver.RemoteControlSetter.
func (d *Driver) SetRemoteControl(ctx context.Context, req fleet.Request, ref fleet.SessionRef, enabled bool) (fleet.Ack, error) {
	unlock := d.rcLocks.lock(ref.ID)
	defer unlock()
	return rcToggler{p: d, window: rcDialogWindow, interval: submitConfirmInterval * 2}.set(ctx, req, ref, enabled)
}

// rcPort is the three operations the toggle is composed from, so the algorithm
// can be exercised against a model of the runtime's dialog without a
// multiplexer. *Driver is the only production implementation.
type rcPort interface {
	State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error)
	Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error)
	Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error)
}

type rcToggler struct {
	p        rcPort
	window   time.Duration
	interval time.Duration
}

func (t rcToggler) set(ctx context.Context, req fleet.Request, ref fleet.SessionRef, enabled bool) (fleet.Ack, error) {

	st, err := t.p.State(ctx, req, ref)
	if err != nil {
		return fleet.Ack{}, err
	}
	ch := st.ControlChannel
	if ch == nil {
		// The command is not safe to send blind (see above), and a state that
		// cannot be read is not evidence of anything.
		return fleet.Ack{}, rcConflict("cannot tell what this session's remote control is doing right now " +
			"(no control channel state could be read), so the toggle was not sent: it would open a " +
			"disconnect dialog if remote control is already on")
	}

	on := ch.State != fleet.ControlChannelOff
	failed := ch.State == fleet.ControlChannelFailed
	switch {
	case enabled && on && !failed:
		return fleet.Ack{Accepted: true}, nil // already on; idempotent
	case !enabled && !on:
		return fleet.Ack{Accepted: true}, nil // already off; idempotent
	}

	// Everything past here types into the session, which only an idle one with an
	// empty composer can take.
	if st.Prompt != nil {
		return fleet.Ack{}, rcConflict("the session is waiting on a prompt; answer it first")
	}
	if st.Status != fleet.StatusIdle {
		return fleet.Ack{}, rcConflict("the session is %s, not idle; the toggle is a command typed into it", st.Status)
	}

	switch {
	case enabled && !on:
		return t.rcEnable(ctx, req, ref)
	case enabled && failed:
		// Reconnect: disconnect, wait for the runtime to let go, enable again.
		// Not measured live — a failed channel could not be induced — so the
		// sequence is the two measured halves joined, and says so in the docs.
		if err := t.rcDisconnect(ctx, req, ref); err != nil {
			return fleet.Ack{}, err
		}
		if err := t.rcWaitNotFailed(ctx, req, ref); err != nil {
			return fleet.Ack{}, err
		}
		return t.rcEnable(ctx, req, ref)
	default: // disable a channel that is on
		if err := t.rcDisconnect(ctx, req, ref); err != nil {
			return fleet.Ack{}, err
		}
		return fleet.Ack{Accepted: true}, nil
	}
}

// rcSend delivers the runtime's command and turns a refusal into the retryable
// conflict it is.
//
// RemoteControl is set because this IS the grant-checked verb (#269): the
// service has already required the remote-control grant before the driver is
// reached, and without this the #272 input gate would refuse the verb's own
// command.
func (t rcToggler) rcSend(ctx context.Context, req fleet.Request, ref fleet.SessionRef) error {
	receipt, err := t.p.Send(ctx, req, ref, "/remote-control", driver.SendOptions{Submit: true, Route: fleet.RouteTerminal, RemoteControl: true})
	if err != nil {
		return err
	}
	if receipt.Outcome == fleet.OutcomeRefused {
		return rcConflict("the session did not take the command: %s", receipt.Reason)
	}
	return nil
}

// rcEnable turns remote control on for a session that reads off, and checks
// that it did not just open the disconnect dialog (a stale read of "off").
func (t rcToggler) rcEnable(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	if err := t.rcSend(ctx, req, ref); err != nil {
		return fleet.Ack{}, err
	}
	deadline := time.Now().Add(t.window / 2)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fleet.Ack{}, ctx.Err()
		case <-time.After(t.interval):
		}
		st, err := t.p.State(ctx, req, ref)
		if err != nil {
			return fleet.Ack{}, err
		}
		if disconnectOption(st.Prompt) > 0 {
			// It was already on. Do not leave the dialog open on the session.
			t.rcCancel(req, ref, st.Prompt.Nonce)
			return fleet.Ack{}, rcConflict("remote control was already on (the runtime offered to disconnect it); " +
				"the dialog was dismissed and nothing changed")
		}
		if st.ControlChannel != nil && st.ControlChannel.State != fleet.ControlChannelOff {
			break
		}
	}
	return fleet.Ack{Accepted: true}, nil
}

// rcDisconnect sends the command, waits for the dialog, and answers it with the
// Disconnect option, found by text.
func (t rcToggler) rcDisconnect(ctx context.Context, req fleet.Request, ref fleet.SessionRef) error {
	if err := t.rcSend(ctx, req, ref); err != nil {
		return err
	}
	deadline := time.Now().Add(t.window)
	var last *fleet.SessionPrompt
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(t.interval):
		}
		st, err := t.p.State(ctx, req, ref)
		if err != nil {
			return err
		}
		if st.Prompt == nil {
			continue
		}
		last = st.Prompt
		idx := disconnectOption(st.Prompt)
		if idx == 0 {
			// Some other dialog got there first. Leave nothing open that we
			// opened, and say what we saw.
			t.rcCancel(req, ref, st.Prompt.Nonce)
			return rcConflict("the runtime showed a dialog without a %q option (%q); it was dismissed and "+
				"nothing was changed", "Disconnect this session", st.Prompt.Options)
		}
		receipt, err := t.p.Respond(ctx, req, ref, fleet.Response{Choice: idx, Nonce: st.Prompt.Nonce})
		if err != nil {
			return err
		}
		if receipt.Outcome == fleet.OutcomeRefused {
			return rcConflict("the disconnect dialog refused the answer: %s", receipt.Reason)
		}
		return nil
	}
	_ = last
	// No dialog. The command may have done the opposite of what was wanted — on
	// a channel that was not really on, it turns it ON — so this says so rather
	// than reporting a failure that sounds harmless.
	return rcConflict("no disconnect dialog appeared within %s, so the runtime did not treat remote control as "+
		"on; the command may have turned it on instead — read the session's controlChannel before retrying",
		t.window)
}

// rcWaitNotFailed waits for a just-disconnected channel to stop reading failed.
func (t rcToggler) rcWaitNotFailed(ctx context.Context, req fleet.Request, ref fleet.SessionRef) error {
	deadline := time.Now().Add(t.window)
	for time.Now().Before(deadline) {
		st, err := t.p.State(ctx, req, ref)
		if err != nil {
			return err
		}
		if st.ControlChannel == nil || st.ControlChannel.State != fleet.ControlChannelFailed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(t.interval):
		}
	}
	return rcConflict("the channel still reads failed after the disconnect; it was not re-enabled")
}

// rcCancel dismisses a dialog this toggle opened. It uses a fresh, short
// context: it runs on the error paths, where the caller's may already be spent,
// and a dialog left open is the one outcome worse than the error.
func (t rcToggler) rcCancel(req fleet.Request, ref fleet.SessionRef, nonce string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = t.p.Respond(ctx, req, ref, fleet.Response{Cancel: true, Nonce: nonce})
}

var _ driver.RemoteControlSetter = (*Driver)(nil)
