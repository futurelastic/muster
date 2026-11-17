package tmux

import (
	"context"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// #489: the account block cleared only on a session SEEN working, or on the
// record of a session whose own screen showed a notice. A machine whose
// account had been serving again for hours kept every session reading
// quota_blocked, because no screen held a notice (nobody is consulted) and,
// where one did, a refusal nobody ever resumed answered for the whole fleet.
//
// What these tests drive is the measured shape: a block remembered since T0,
// one session whose record still ends in the refusal, another whose record
// shows a real assistant turn after T0, and nothing reading `working`.

const quotaServedRule = "────────────────────"

// refusalScreen is the screen of a session that was refused and never resumed.
const refusalScreen = "transcript\nYou've hit your session limit · resets 9:50pm (Asia/Saigon)\n" +
	quotaServedRule + "\n❯ \n" + quotaServedRule + "\n"

func syntheticEntry(id string, at time.Time) map[string]any {
	e := cleanTurnEntry(id, at)
	e["message"] = map[string]any{
		"model":   "<synthetic>",
		"content": []map[string]any{{"type": "text", "text": "No response requested."}},
	}
	return e
}

// servedFixture lays down the two records and a driver reading them. alpha is
// listed first and its record ends in the refusal at refusedAt; beta served a
// turn BEFORE the refusal only. The clock is a variable the caller may move.
type servedFixture struct {
	f         *fakeMux
	root      string
	d         *Driver
	refusedAt time.Time
}

func newServedFixture(t *testing.T) *servedFixture {
	t.Helper()
	f := twoSessions()
	f.captures["%1"] = refusalScreen
	f.captures["%2"] = idleFixtureFor("beta")
	root := t.TempDir()
	refusedAt := sessionStart.Add(10 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		rateLimitEntry("conv-alpha", refusedAt, "9:50pm (Asia/Saigon)"))
	writeConversationWithAPIError(t, root, "/work/beta", "conv-beta", "beta", sessionStart,
		cleanTurnEntry("conv-beta", sessionStart.Add(5*time.Minute)))
	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(2 * time.Hour) }),
		WithRecordRoot(root))
	return &servedFixture{f: f, root: root, d: d, refusedAt: refusedAt}
}

func (s *servedFixture) list(t *testing.T) fleet.Collection[fleet.Session] {
	t.Helper()
	col, err := s.d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return col
}

func (s *servedFixture) blocked(t *testing.T) bool {
	t.Helper()
	q, _ := s.d.quotaBlock()
	return q != nil
}

// The headline: a session listed first whose record still ends in the refusal
// must not decide for the fleet, and a block must not outlive a real turn that
// another session completed after it began.
func TestQuotaBlockClearsWhenAnySessionsRecordShowsATurnAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// The refused session's screen still shows its notice: nobody types
		// into a session that refused them, so it never changes.
		staleNoticeStaysOnScreen bool
	}{
		{"the stale refusal is flagged first and shadows the rest", true},
		{"no screen shows a notice any more", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServedFixture(t)

			col := s.list(t)
			if !s.blocked(t) {
				t.Fatal("fixture never reached a blocked account; the test proves nothing")
			}
			if q := sessionOf(t, col, "beta").State.Quota; q == nil || !q.Since.Equal(s.refusedAt) {
				t.Fatalf("beta's block = %+v, want one dated at the refusal %v", q, s.refusedAt)
			}
			if !tc.staleNoticeStaysOnScreen {
				s.f.setCapture("%1", idleFixtureFor("alpha"))
			}

			// Control: a turn BEFORE the refusal is no proof the account serves now.
			s.list(t)
			if !s.blocked(t) {
				t.Fatal("the block cleared with no turn recorded after it began")
			}

			// beta serves a turn after the refusal. Nothing reads `working`.
			appendRecordLine(t, s.root, "/work/beta", "conv-beta",
				cleanTurnEntry("conv-beta", s.refusedAt.Add(20*time.Minute)))
			col = s.list(t)
			if s.blocked(t) {
				t.Fatal("the block outlived a turn another session completed after it began")
			}
			for _, sess := range col.Items() {
				if sess.State.Status == fleet.StatusQuotaBlocked || sess.State.Quota != nil {
					t.Errorf("%s still reads %s with quota %+v", sess.ID, sess.State.Status, sess.State.Quota)
				}
			}
			if q := quotaOnlyOf(col); q != nil {
				t.Errorf("the source still reports a block: %+v", q)
			}

			// And it stays cleared: alpha's notice (and its record's last entry,
			// the old refusal) is history now, not a fresh refusal to promote.
			for i := 0; i < 3; i++ {
				col = s.list(t)
				if s.blocked(t) {
					t.Fatalf("read %d: the block re-asserted itself from a refusal older than a turn that was served", i+1)
				}
				for _, sess := range col.Items() {
					if sess.State.Status == fleet.StatusQuotaBlocked {
						t.Fatalf("read %d: %s flipped back to quota_blocked", i+1, sess.ID)
					}
				}
			}
		})
	}
}

// quotaOnlyOf is the collection's own account-level answer: the same fact the
// machine.quota event announces, read the way a caller reads the listing.
func quotaOnlyOf(col fleet.Collection[fleet.Session]) *fleet.QuotaBlock {
	for _, src := range col.Sources() {
		if src.Quota != nil {
			return src.Quota
		}
	}
	return nil
}

// What does NOT prove the account serves again. Each of these leaves the
// block in force; clearing on any of them would send a supervisor back into a
// refusing account.
func TestQuotaBlockStaysWhenNothingProvesTheAccountServes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines func(s *servedFixture) []map[string]any
	}{
		{"a turn recorded exactly when the refusal was", func(s *servedFixture) []map[string]any {
			return []map[string]any{cleanTurnEntry("conv-beta", s.refusedAt)}
		}},
		{"an entry the runtime composed itself rather than a model answer", func(s *servedFixture) []map[string]any {
			return []map[string]any{syntheticEntry("conv-beta", s.refusedAt.Add(20*time.Minute))}
		}},
		{"a turn that was followed by a refusal of its own", func(s *servedFixture) []map[string]any {
			return []map[string]any{
				cleanTurnEntry("conv-beta", s.refusedAt.Add(20*time.Minute)),
				rateLimitEntry("conv-beta", s.refusedAt.Add(25*time.Minute), "9:50pm (Asia/Saigon)"),
			}
		}},
		{"a turn whose time cannot be read", func(s *servedFixture) []map[string]any {
			e := cleanTurnEntry("conv-beta", s.refusedAt.Add(20*time.Minute))
			e["timestamp"] = "yesterday-ish"
			return []map[string]any{e}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServedFixture(t)
			s.list(t)
			if !s.blocked(t) {
				t.Fatal("fixture never reached a blocked account; the test proves nothing")
			}
			s.f.setCapture("%1", idleFixtureFor("alpha"))
			for _, line := range tc.lines(s) {
				appendRecordLine(t, s.root, "/work/beta", "conv-beta", line)
			}
			col := s.list(t)
			if !s.blocked(t) {
				t.Fatal("the block cleared without proof the account serves")
			}
			if st := sessionOf(t, col, "beta").State.Status; st != fleet.StatusQuotaBlocked {
				t.Errorf("beta reads %s on a refusing account", st)
			}
		})
	}
}

// Newest evidence wins across the whole machine: a turn one session served is
// outranked by a later refusal in another, even when that other session's
// screen shows no notice to flag it.
func TestAServedTurnDoesNotOutrankALaterRefusalInAnotherSession(t *testing.T) {
	s := newServedFixture(t)
	s.list(t)
	if !s.blocked(t) {
		t.Fatal("fixture never reached a blocked account; the test proves nothing")
	}
	s.f.setCapture("%1", idleFixtureFor("alpha"))
	appendRecordLine(t, s.root, "/work/beta", "conv-beta",
		cleanTurnEntry("conv-beta", s.refusedAt.Add(20*time.Minute)))
	appendRecordLine(t, s.root, "/work/alpha", "conv-alpha",
		rateLimitEntry("conv-alpha", s.refusedAt.Add(30*time.Minute), "9:50pm (Asia/Saigon)"))
	s.list(t)
	if !s.blocked(t) {
		t.Fatal("the block cleared on a turn that a later refusal elsewhere had already outlived")
	}

	// And a turn after THAT refusal does clear it.
	appendRecordLine(t, s.root, "/work/beta", "conv-beta",
		cleanTurnEntry("conv-beta", s.refusedAt.Add(40*time.Minute)))
	s.list(t)
	if s.blocked(t) {
		t.Fatal("the block outlived a turn served after the newest refusal on the machine")
	}
}

// A listing that shows only some sessions must not forget what an earlier one
// proved: otherwise the stale notice on the one session a filtered read shows
// re-promotes a block the whole fleet had already outlived.
func TestServedProofSurvivesAListingThatOmitsTheSessionThatServed(t *testing.T) {
	s := newServedFixture(t)
	s.list(t)
	appendRecordLine(t, s.root, "/work/beta", "conv-beta",
		cleanTurnEntry("conv-beta", s.refusedAt.Add(20*time.Minute)))
	s.list(t)
	if s.blocked(t) {
		t.Fatal("fixture never cleared the block; the test proves nothing")
	}

	col, err := s.d.List(context.Background(), testCaller, driver.ListFilter{CwdPrefix: "/work/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(col.Items()) != 1 {
		t.Fatalf("filtered listing = %d sessions, want only alpha", len(col.Items()))
	}
	if s.blocked(t) {
		t.Fatal("a listing without the session that served re-promoted a block that had been outlived")
	}
}

// A refusal NEWER than the proof is a fresh refusal: the account served, then
// stopped serving. The block must come back, dated at the new refusal.
func TestARefusalAfterTheProofBlocksAgain(t *testing.T) {
	s := newServedFixture(t)
	s.list(t)
	servedAt := s.refusedAt.Add(20 * time.Minute)
	appendRecordLine(t, s.root, "/work/beta", "conv-beta", cleanTurnEntry("conv-beta", servedAt))
	s.list(t)
	if s.blocked(t) {
		t.Fatal("fixture never cleared the block; the test proves nothing")
	}

	again := servedAt.Add(30 * time.Minute)
	appendRecordLine(t, s.root, "/work/alpha", "conv-alpha", rateLimitEntry("conv-alpha", again, "9:50pm (Asia/Saigon)"))
	s.list(t)
	q, _ := s.d.quotaBlock()
	if q == nil {
		t.Fatal("the account refused again after it had served and nothing said so")
	}
	if !q.Since.Equal(again) {
		t.Errorf("since = %v, want the new refusal %v", q.Since, again)
	}
}

// The event a supervisor acts on: told once that the account refuses, and told
// explicitly when it serves again — by a session's record, with nothing
// working.
func TestSubscribeRetractsABlockWhenARecordShowsTheAccountServing(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = refusalScreen
	f.captures["%2"] = idleFixtureFor("beta")
	r := &ctlRegistry{}
	root := t.TempDir()
	refusedAt := sessionStart.Add(10 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		rateLimitEntry("conv-alpha", refusedAt, "9:50pm (Asia/Saigon)"))
	writeConversationWithAPIError(t, root, "/work/beta", "conv-beta", "beta", sessionStart,
		cleanTurnEntry("conv-beta", sessionStart.Add(5*time.Minute)))
	d := New("testbox", withExec(f.exec), withCtlDialer(r.dial), WithRecordRoot(root),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(2 * time.Hour) }))

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fire(r.contentFor("alpha💬"), "output")
	p, ok := awaitQuota(t, s)
	if !ok || !p.Blocked {
		t.Fatalf("the account was refusing work and the stream never said so (got %+v, %v)", p, ok)
	}

	f.setCapture("%1", idleFixtureFor("alpha"))
	appendRecordLine(t, root, "/work/beta", "conv-beta",
		cleanTurnEntry("conv-beta", refusedAt.Add(20*time.Minute)))
	fire(r.contentFor("alpha💬"), "output")
	p, ok = awaitQuota(t, s)
	if !ok {
		t.Fatal("a session's record showed the account serving again and the stream never retracted the block")
	}
	if p.Blocked || p.Quota != nil {
		t.Errorf("payload = %+v, want an explicit blocked: false", p)
	}
}
