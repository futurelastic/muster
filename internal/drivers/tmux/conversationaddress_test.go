package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

const (
	convAlpha = "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"
	convGhost = "00000000-1111-4222-8333-444444444444"
)

// A driver whose alpha session's conversation resolves to convAlpha.
func conversationAddressRig(t *testing.T) (*Driver, *fakeMux) {
	t.Helper()
	f := twoSessions()
	root := t.TempDir()
	writeRecord(t, root, "/work/alpha", convAlpha, "alpha💬", sessionStart.Add(4*time.Second))
	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(root))
	if c := conversationOf(t, d, "alpha💬"); c == nil || !c.Known || c.ID != convAlpha {
		t.Fatalf("rig: alpha's conversation = %+v, want %s", c, convAlpha)
	}
	return d, f
}

func mustNameSession(t *testing.T, reason string, ids []string, want string) {
	t.Helper()
	if !strings.Contains(reason, want) {
		t.Errorf("reason %q does not name the session id %q", reason, want)
	}
	if !strings.Contains(reason, "conversation id") {
		t.Errorf("reason %q does not say the value is a conversation id", reason)
	}
	if strings.Contains(reason, "no session with this id") || strings.Contains(strings.ToLower(reason), "gone") {
		t.Errorf("reason %q reads as though the session is gone", reason)
	}
	if len(ids) != 1 || ids[0] != want {
		t.Errorf("sessionIds = %v, want [%s]", ids, want)
	}
}

// #268: /input addressed with a live session's conversation id names the
// session id, in prose and in the structured field.
func TestSendByConversationIdNamesTheSessionId(t *testing.T) {
	d, f := conversationAddressRig(t)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: convAlpha}, "hi", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q, want refused: ids stay strict", got.Outcome)
	}
	mustNameSession(t, got.Reason, got.SessionIds, "alpha💬")
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" || c[0] == "paste-buffer" {
			t.Errorf("a refused address must write nothing; saw %v", c)
		}
	}
}

// #268: the same for /respond.
func TestRespondByConversationIdNamesTheSessionId(t *testing.T) {
	d, _ := conversationAddressRig(t)
	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: convAlpha}, fleet.Response{Choice: 1, Nonce: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q, want refused", got.Outcome)
	}
	mustNameSession(t, got.Reason, got.SessionIds, "alpha💬")
}

// #268: the single-session read keeps ErrNoSuchSession in its chain (a service
// probing drivers must still read "not mine") and carries the ids.
func TestStateByConversationIdNamesTheSessionId(t *testing.T) {
	d, _ := conversationAddressRig(t)
	_, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: convAlpha})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Fatalf("err = %v, want it to still satisfy ErrNoSuchSession", err)
	}
	var ce *fleet.ConversationIdError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a ConversationIdError", err)
	}
	mustNameSession(t, ce.Error(), ce.SessionIds, "alpha💬")
}

// An id that matches neither a session id nor a live conversation keeps
// today's reason and carries no ids — including a UUID that is nobody's.
func TestUnknownIdKeepsTodaysRefusal(t *testing.T) {
	d, _ := conversationAddressRig(t)
	for _, id := range []string{"ghost", convGhost} {
		got, err := d.Send(context.Background(), testCaller,
			fleet.SessionRef{Machine: "testbox", ID: id}, "hi", driver.SendOptions{Submit: true})
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != fleet.OutcomeRefused || got.Reason != "no session with this id" || got.SessionIds != nil {
			t.Errorf("id %q: receipt = %+v, want today's refusal with no session ids", id, got)
		}
		_, err = d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id})
		var ce *fleet.ConversationIdError
		if !errors.Is(err, fleet.ErrNoSuchSession) || errors.As(err, &ce) {
			t.Errorf("id %q: State err = %v, want a plain ErrNoSuchSession", id, err)
		}
	}
}

// Two live sessions holding one conversation after a resume: every candidate
// is listed, sorted. A dead row is not a candidate — a retry cannot reach it.
func TestSeveralSessionsHoldingOneConversationAreAllListed(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	writeRecord(t, root, "/work/alpha", convAlpha, "alpha💬", sessionStart.Add(4*time.Second))
	writeRecord(t, root, "/work/beta", convAlpha, "beta", sessionStart.Add(4*time.Second))
	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(root))

	rows, _, err := d.enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := d.sessionsHoldingConversation(context.Background(), rows, convAlpha)
	if len(got) != 2 || got[0] != "alpha💬" || got[1] != "beta" {
		t.Fatalf("holders = %v, want [alpha💬 beta]", got)
	}
	reason := fleet.ConversationIdReason(convAlpha, got)
	if !strings.Contains(reason, `"alpha💬"`) || !strings.Contains(reason, `"beta"`) {
		t.Errorf("reason %q must list every candidate id", reason)
	}

	f.sessions[1].dead = true
	rows, _, err = d.enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := d.sessionsHoldingConversation(context.Background(), rows, convAlpha); len(got) != 1 || got[0] != "alpha💬" {
		t.Errorf("with beta dead, holders = %v, want [alpha💬]", got)
	}
}

// A session id that is itself UUID-shaped still wins: the conversation lookup
// only runs when no session carries the id.
func TestASessionIdIsNeverReadAsAConversationId(t *testing.T) {
	f := twoSessions()
	f.sessions[1].name = convAlpha
	root := t.TempDir()
	writeRecord(t, root, "/work/alpha", convAlpha, "alpha💬", sessionStart.Add(4*time.Second))
	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(root))
	if _, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: convAlpha}); err != nil {
		t.Fatalf("a session whose id is %s must be found by it: %v", convAlpha, err)
	}
}
