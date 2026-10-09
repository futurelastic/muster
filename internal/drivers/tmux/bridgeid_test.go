package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// ---- the bridge id (muster #276) ------------------------------------------

func rcEnabledWithLink(t *testing.T, at time.Duration, link string) string {
	return rcLine(t, map[string]any{"type": "system", "subtype": "bridge_status",
		"content": "/remote-control is active · Continue here, on your phone, or at " + link,
		"url":     link, "timestamp": rcStamp(at)})
}

func TestBridgeIDFromURL(t *testing.T) {
	cases := map[string]string{
		"https://viewer.example.test/code/session_01AbC":     "session_01AbC",
		"https://viewer.example.test/code/session_01AbC/":    "session_01AbC",
		"https://viewer.example.test/code/session_01AbC?x=1": "session_01AbC",
		"https://viewer.example.test/code/id-2#frag":         "id-2",
		"":                                    "",
		"https://viewer.example.test/":        "",
		"https://viewer.example.test/a/b c":   "", // not a plain token
		"https://viewer.example.test/a/b%2Fc": "", // escapes are not decoded into an id
		"session_only":                        "session_only",
		"https://viewer.example.test/" + strings.Repeat("x", 129): "",
	}
	for in, want := range cases {
		if got := bridgeIDFromURL(in); got != want {
			t.Errorf("bridgeIDFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveControlChannel_BridgeID(t *testing.T) {
	const link = "https://viewer.example.test/code/session_01AbC"
	bridge := func(ch *fleet.ControlChannel) string {
		if ch == nil || ch.BridgeID == nil {
			return "<null>"
		}
		return *ch.BridgeID
	}

	t.Run("an active channel from the record carries the id", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledWithLink(t, time.Second, link))
		if got := bridge(d.resolveControlChannel(nil, conv, row)); got != "session_01AbC" {
			t.Fatalf("bridgeId = %s", got)
		}
	})
	t.Run("an enable that carried no link reports null", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledEntry(t, time.Second))
		ch := d.resolveControlChannel(nil, conv, row)
		if rcStateName(ch) != "active" || bridge(ch) != "<null>" {
			t.Fatalf("%+v", ch)
		}
	})
	t.Run("a disconnect after the enable reads off with null", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledWithLink(t, time.Second, link), rcDisconnectEntry(t, 2*time.Second))
		ch := d.resolveControlChannel(nil, conv, row)
		if rcStateName(ch) != "off" || bridge(ch) != "<null>" {
			t.Fatalf("%+v", ch)
		}
	})
	t.Run("a later enable replaces the id", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledWithLink(t, time.Second, link),
			rcDisconnectEntry(t, 2*time.Second), rcEnabledWithLink(t, 3*time.Second, "https://viewer.example.test/code/session_02"))
		if got := bridge(d.resolveControlChannel(nil, conv, row)); got != "session_02" {
			t.Fatalf("bridgeId = %s", got)
		}
	})
	t.Run("a link on an entry of another kind is not read", func(t *testing.T) {
		line := rcLine(t, map[string]any{"type": "user", "subtype": "bridge_status",
			"content": "/remote-control is active · x", "url": link, "timestamp": rcStamp(time.Second)})
		d, row, conv := rcDriverWithRecord(t, line)
		if bridge(d.resolveControlChannel(nil, conv, row)) != "<null>" {
			t.Fatal("a user-role entry supplied a bridge id")
		}
	})
	t.Run("the footer decides the state and the record supplies the id, on a copy", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledWithLink(t, time.Second, link))
		footer := &fleet.ControlChannel{State: fleet.ControlChannelReconnecting}
		got := d.resolveControlChannel(footer, conv, row)
		if got.State != fleet.ControlChannelReconnecting || bridge(got) != "session_01AbC" {
			t.Fatalf("%+v", got)
		}
		if footer.BridgeID != nil {
			t.Fatal("the footer's own value was mutated")
		}
	})
	t.Run("a failed footer gets no id", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledWithLink(t, time.Second, link))
		footer := &fleet.ControlChannel{State: fleet.ControlChannelFailed}
		if got := d.resolveControlChannel(footer, conv, row); got != footer {
			t.Fatalf("%+v", got)
		}
	})
}

// ---- the composer read (muster #276) --------------------------------------

func TestComposer_ReturnsTheUnsentTextAndTheDigestDiscardAccepts(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	st, err := d.State(context.Background(), testCaller, ref)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Composer(context.Background(), testCaller, ref)
	if err != nil {
		t.Fatal(err)
	}
	pending, scan := composerText(newScreen(f.captures["%2"]))
	if scan != composerFound || pending == "" || got.Text != pending {
		t.Fatalf("text = %q, want %q", got.Text, pending)
	}
	if got.ComposerDigest == "" || got.ComposerDigest != st.ComposerDigest {
		t.Fatalf("digest = %q, state publishes %q", got.ComposerDigest, st.ComposerDigest)
	}
	// The digest is the one discard accepts.
	if _, err := d.Discard(context.Background(), testCaller, ref, got.ComposerDigest, driver.DiscardOptions{}); err != nil &&
		strings.Contains(err.Error(), "different text") {
		t.Fatalf("discard refused the digest the read returned: %v", err)
	}
}

func TestComposer_AnEmptyComposerIsAnAnswerAndAnUnreadableOneIsARefusal(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	got, err := d.Composer(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil || got.Text != "" || got.ComposerDigest != "" {
		t.Fatalf("empty composer: %+v, %v", got, err)
	}

	f.captures["%1"] = clippedComposerFixture()
	if _, err := d.Composer(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}); err == nil {
		t.Fatal("a composer that cannot be read as a whole was answered; \"empty\" would be a claim about text never seen")
	}

	f.captures["%1"] = cardScreen(cardOpts{row: "half a thought"})
	_, err = d.Composer(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err == nil || !strings.Contains(err.Error(), "feedback-draft card") {
		t.Fatalf("card over typed text: %v", err)
	}

	if _, err := d.Composer(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "nope"}); err == nil {
		t.Fatal("no such session was answered")
	}
}

func TestComposer_RefusesASessionStartedAtADifferentTime(t *testing.T) {
	d := newTestDriver(twoSessions())
	other := time.Unix(1, 0)
	req := testCaller
	req.Expect.StartedAt = &other
	if _, err := d.Composer(context.Background(), req, fleet.SessionRef{Machine: "testbox", ID: "beta"}); err == nil {
		t.Fatal("a stale startedAt was accepted")
	}
}
