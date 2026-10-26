package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #378: an opencode session reports the runtime's own session id as its
// conversation, captured at create. These tests hold what a client keying a
// per-session link on `conversation.known` relies on.

// wantCaptured fails unless c says: known, this id, captured, with evidence — and
// unless it survives the encoder, which refuses an incoherent ref.
func wantCaptured(t *testing.T, where string, c *fleet.ConversationRef, id string) {
	t.Helper()
	if c == nil {
		t.Errorf("%s: conversation is absent; the driver created this session and knows its id", where)
		return
	}
	if !c.Known || c.ID != id || c.Source != fleet.ConversationCaptured || c.Evidence == "" {
		t.Errorf("%s: conversation = %+v; want known, id %q, source captured, evidence", where, *c, id)
	}
	if _, err := json.Marshal(c); err != nil {
		t.Errorf("%s: conversation does not encode: %v", where, err)
	}
}

func TestCreate_ReportsTheRuntimeSessionIdAsCapturedConversation(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	stream := subscribe(t, d, driver.SubscribeFilter{})

	sess, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: "/work/x", Name: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The id the fake returned, not one this test made up.
	f.mu.Lock()
	_, theRuntimesOwn := f.sessions[sess.ID]
	f.mu.Unlock()
	if !theRuntimesOwn {
		t.Fatalf("session id %q is not one the runtime returned", sess.ID)
	}

	wantCaptured(t, "the create's answer", sess.Conversation, sess.ID)

	replay, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Cwd: "/work/x", Name: "t"})
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if replay.ID != sess.ID {
		t.Fatalf("a replayed key made a second session: %q then %q", sess.ID, replay.ID)
	}
	wantCaptured(t, "the idempotent replay", replay.Conversation, sess.ID)

	col, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, s := range col.Items() {
		if s.ID == sess.ID {
			found = true
			wantCaptured(t, "the listing", s.Conversation, sess.ID)
		}
	}
	if !found {
		t.Fatalf("session %s is not listed", sess.ID)
	}

	ev := next(t, stream)
	announced, ok := ev.Payload.(fleet.Session)
	if ev.Kind != fleet.EventSessionCreated || !ok || announced.ID != sess.ID {
		t.Fatalf("event = %s %+v, want session.created for %s", ev.Kind, ev.Payload, sess.ID)
	}
	wantCaptured(t, "the session.created event", announced.Conversation, sess.ID)

	// Two sessions are two conversations: the id is the session's own, never shared.
	other, err := d.Create(context.Background(), testReq, "k2", fleet.SessionSpec{Cwd: "/work/y", Name: "u"})
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	wantCaptured(t, "a second session", other.Conversation, other.ID)
	if other.Conversation != nil && sess.Conversation != nil && other.Conversation.ID == sess.Conversation.ID {
		t.Errorf("two sessions report one conversation id %q", other.Conversation.ID)
	}
}

// The value the caller gets is a copy: altering it must not alter what the next
// read says.
func TestCreate_ConversationIsACopyNotTheDriversOwn(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "t"})
	if sess.Conversation == nil {
		t.Fatal("the create reported no conversation")
	}
	sess.Conversation.ID = "tampered"
	sess.Conversation.Known = false

	got := listed(t, d)[sess.ID]
	wantCaptured(t, "the listing after a caller altered its copy", got.Conversation, sess.ID)
}

func TestResume_ConversationSurvivesARestart(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "survivor"})
	wantCaptured(t, "before the restart", sess.Conversation, sess.ID)
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok {
		t.Fatal("the session is not listed after the restart")
	}
	// The relaunched runtime answered a read by this id, so the record is held.
	wantCaptured(t, "after the restart", got.Conversation, sess.ID)
	if got.Conversation != nil && !strings.Contains(got.Conversation.Evidence, "confirmed") {
		t.Errorf("evidence = %q; want it to say the id was confirmed after the restart", got.Conversation.Evidence)
	}
}

// A session whose id the runtime could not be asked about is not served the id
// the driver wrote down: "known: false" with the reason, for every way it can
// fail to come back.
func TestResume_AnUnconfirmedSessionReportsConversationUnknown(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	gone := mustCreate(t, d1, "k1", fleet.SessionSpec{})
	missingDir := mustCreate(t, d1, "k2", fleet.SessionSpec{})
	withEnv := mustCreate(t, d1, "k3", fleet.SessionSpec{Env: map[string]string{"MODEL_KEY": "v"}})
	kGone, _ := d1.wasSeen(gone.ID)
	kMissing, _ := d1.wasSeen(missingDir.ID)
	_ = d1.Shutdown()
	// The runtime's store lost one session; another's directory is deleted; a
	// third was created with environment values this driver never kept.
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
		withEnv.ID:    "MODEL_KEY",
		"ses_garbled": "could not be used",
	} {
		s, ok := all[id]
		if !ok {
			t.Errorf("%s is not listed", id)
			continue
		}
		c := s.Conversation
		switch {
		case c == nil:
			t.Errorf("%s: conversation is absent; the driver looked and could not tell, which is a real answer (known: false), not nobody-looked", id)
		case c.Known || c.ID != "" || c.Source != "":
			t.Errorf("%s: conversation = %+v; an id that could not be confirmed must not be served", id, *c)
		case !strings.Contains(c.Evidence, want):
			t.Errorf("%s: evidence = %q; want it to carry the reason (%q)", id, c.Evidence, want)
		}
		if c != nil {
			if _, err := json.Marshal(c); err != nil {
				t.Errorf("%s: conversation does not encode: %v", id, err)
			}
		}
	}
}

// The declarations a person-facing client reads (muster #378, #379), as this
// driver really behaves, in both modes.
func TestCapabilities_DeclaresWhatAPersonFacingClientNeeds(t *testing.T) {
	f := newFakeServer(t)
	shared := newTestDriver(t, f)
	isolated, _ := newIsolatedDriver(t)
	for name, d := range map[string]*Driver{"shared": shared, "isolated": isolated} {
		caps := d.Capabilities()
		if caps.ReadsTurns == nil || !*caps.ReadsTurns {
			t.Errorf("%s: ReadsTurns = %v; /turns reads this runtime's message record (#369)", name, caps.ReadsTurns)
		}
		if _, ok := driver.Driver(d).(driver.TurnReader); !ok {
			t.Errorf("%s: declares ReadsTurns but does not implement driver.TurnReader", name)
		}
		if caps.ReportsConversation == nil || !*caps.ReportsConversation {
			t.Errorf("%s: ReportsConversation = %v; every session carries the runtime's own id (#378)", name, caps.ReportsConversation)
		}
		if r := caps.Respond; r == nil || len(r.Kinds) != 1 || r.Kinds[0] != fleet.PromptToolPermission || r.Unclassified {
			t.Errorf("%s: Respond = %+v; want exactly tool-permission, and nothing unclassified", name, r)
		}
		if p := caps.PermissionModeChange; p == nil || p.Live || p.Via != "" {
			t.Errorf("%s: PermissionModeChange = %+v; want declared and not live: the mode is fixed at create", name, p)
		}
		// The other half of "cannot change it live": no raw keys, and the create
		// list is the only way to choose.
		if caps.DeliversRawKeys {
			t.Errorf("%s: DeliversRawKeys = true; a live mode change through keys is not available here", name)
		}
		if len(caps.CreatePermissionModes) == 0 {
			t.Errorf("%s: CreatePermissionModes is empty; it is the only way to choose a mode", name)
		}
	}
}
