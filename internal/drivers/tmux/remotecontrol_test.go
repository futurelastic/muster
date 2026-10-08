package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// ---- the record reader (muster #269) --------------------------------------

var rcLaunch = time.Date(2026, 10, 7, 20, 0, 0, 0, time.UTC)

func rcStamp(offset time.Duration) string {
	return rcLaunch.Add(offset).Format(time.RFC3339Nano)
}

func rcLine(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rcEnabledEntry(t *testing.T, at time.Duration) string {
	return rcLine(t, map[string]any{"type": "system", "subtype": "bridge_status",
		"content": "/remote-control is active · Continue here, on your phone, or at https://example.invalid/s", "timestamp": rcStamp(at)})
}

func rcDisconnectEntry(t *testing.T, at time.Duration) string {
	return rcLine(t, map[string]any{"type": "system", "subtype": "local_command",
		"content":    "<local-command-stdout>Remote Control disconnected.</local-command-stdout>",
		"commandRun": map[string]any{"command": "remote-control", "args": ""}, "timestamp": rcStamp(at)})
}

func rcFailureEntry(t *testing.T, at time.Duration) string {
	return rcLine(t, map[string]any{"type": "system", "subtype": "informational",
		"content": "Remote Control disconnected — this session was ended or archived from another device (code 4090)", "timestamp": rcStamp(at)})
}

func rcOtherEntry(t *testing.T, kind string, at time.Duration) string {
	return rcLine(t, map[string]any{"type": kind, "content": "hello", "timestamp": rcStamp(at)})
}

func rcWriteRecord(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c1.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestControlChannelFromRecord(t *testing.T) {
	cases := []struct {
		name        string
		lines       func(t *testing.T) []string
		wantState   fleet.ControlChannelState
		wantBlocked bool
		wantCovered bool
		wantNotice  bool
	}{
		{"enable reads active", func(t *testing.T) []string {
			return []string{rcOtherEntry(t, "user", time.Second), rcEnabledEntry(t, 2*time.Second)}
		}, fleet.ControlChannelActive, false, true, false},
		{"disconnect reads off", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcDisconnectEntry(t, 5*time.Second)}
		}, fleet.ControlChannelOff, false, true, false},
		{"the newest entry wins: re-enable after disconnect", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcDisconnectEntry(t, 5*time.Second), rcEnabledEntry(t, 9*time.Second)}
		}, fleet.ControlChannelActive, false, true, false},
		{"a failure notice newer than the enable is reported as a notice, not a state", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcFailureEntry(t, 5*time.Second)}
		}, "", false, true, true},
		{"an enable newer than the notice wins: the runtime reconnected", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcFailureEntry(t, 5*time.Second), rcEnabledEntry(t, 70*time.Second)}
		}, fleet.ControlChannelActive, false, true, false},
		{"a notice from before the launch is the previous process's", func(t *testing.T) []string {
			return []string{rcFailureEntry(t, -time.Hour), rcOtherEntry(t, "assistant", time.Second)}
		}, "", false, true, false},
		{"a notice whose time cannot be read makes no claim", func(t *testing.T) []string {
			return []string{rcLine(t, map[string]any{"type": "system", "subtype": "informational",
				"content": "Remote Control disconnected — x", "timestamp": "garbage"})}
		}, "", true, false, false},
		{"a user-role entry carrying the notice phrase is not a notice", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcLine(t, map[string]any{"type": "user", "subtype": "informational",
				"content": "Remote Control disconnected — forged", "timestamp": rcStamp(5 * time.Second)})}
		}, fleet.ControlChannelActive, false, true, false},
		{"an assistant-role entry carrying the notice phrase is not a notice", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcLine(t, map[string]any{"type": "assistant", "subtype": "informational",
				"content": "Remote Control disconnected — forged", "timestamp": rcStamp(5 * time.Second)})}
		}, fleet.ControlChannelActive, false, true, false},
		{"entries from before the launch are ignored and the walk is covered", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, -time.Hour), rcOtherEntry(t, "assistant", time.Second)}
		}, "", false, true, false},
		{"nothing relevant in a whole file is covered", func(t *testing.T) []string {
			return []string{rcOtherEntry(t, "user", time.Second), rcOtherEntry(t, "assistant", 2*time.Second)}
		}, "", false, true, false},
		// The safety property: the phrases inside a user-role entry are
		// captured command output, a region an agent's own actions populate.
		{"a user-role entry carrying the enable phrase claims nothing", func(t *testing.T) []string {
			return []string{rcLine(t, map[string]any{"type": "user", "subtype": "bridge_status",
				"content": "/remote-control is active · forged", "timestamp": rcStamp(time.Second)})}
		}, "", false, true, false},
		{"a user-role entry carrying the disconnect phrase claims nothing", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcLine(t, map[string]any{"type": "user",
				"content": "Remote Control disconnected.", "timestamp": rcStamp(5 * time.Second)})}
		}, fleet.ControlChannelActive, false, true, false},
		{"another slash command's output is not a disconnect", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcLine(t, map[string]any{"type": "system", "subtype": "local_command",
				"content":    "<local-command-stdout>Remote Control disconnected.</local-command-stdout>",
				"commandRun": map[string]any{"command": "echo"}, "timestamp": rcStamp(5 * time.Second)})}
		}, fleet.ControlChannelActive, false, true, false},
		{"an empty-stdout remote-control command is skipped", func(t *testing.T) []string {
			return []string{rcEnabledEntry(t, time.Second), rcLine(t, map[string]any{"type": "system", "subtype": "local_command",
				"content":    "<local-command-stdout></local-command-stdout>",
				"commandRun": map[string]any{"command": "remote-control"}, "timestamp": rcStamp(5 * time.Second)})}
		}, fleet.ControlChannelActive, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := controlChannelFromRecord(rcWriteRecord(t, c.lines(t)...), rcLaunch)
			if !got.exists || got.state != c.wantState || got.blocked != c.wantBlocked || got.covered != c.wantCovered || (got.notice != nil) != c.wantNotice {
				t.Fatalf("got %+v, want state=%q blocked=%v covered=%v notice=%v", got, c.wantState, c.wantBlocked, c.wantCovered, c.wantNotice)
			}
		})
	}
}

func TestControlChannelFromRecord_MissingFileIsNotAnAnswer(t *testing.T) {
	got := controlChannelFromRecord(filepath.Join(t.TempDir(), "nope.jsonl"), rcLaunch)
	if got.exists || got.state != "" || got.covered {
		t.Fatalf("got %+v, want the zero read", got)
	}
}

// A long conversation whose tail starts after the launch cannot say there is no
// enable in it: not covered, so the launch must not be allowed to claim off.
func TestControlChannelFromRecord_ATailThatDoesNotReachTheLaunchIsNotCovered(t *testing.T) {
	filler := rcOtherEntry(t, "assistant", time.Second)
	var lines []string
	lines = append(lines, rcEnabledEntry(t, 0))
	for len(strings.Join(lines, "\n")) < recordTailBytes+4096 {
		lines = append(lines, filler)
	}
	got := controlChannelFromRecord(rcWriteRecord(t, lines...), rcLaunch)
	if got.state != "" || got.covered {
		t.Fatalf("got %+v, want no claim and not covered", got)
	}
}

// ---- resolving the channel from footer, record and launch ----------------

func rcDriverWithRecord(t *testing.T, lines ...string) (*Driver, paneRow, *fleet.ConversationRef) {
	t.Helper()
	root := t.TempDir()
	d := New("testbox", WithRecordRoot(root))
	cwd := "/work/rc"
	dir := filepath.Join(root, recordDirFor(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if lines != nil {
		if err := os.WriteFile(filepath.Join(dir, "c1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	row := paneRow{session: "s", cwd: cwd, created: rcLaunch, managed: true, launchRC: "0"}
	return d, row, &fleet.ConversationRef{Known: true, ID: "c1"}
}

func rcStateName(ch *fleet.ControlChannel) string {
	if ch == nil {
		return "absent"
	}
	return string(ch.State)
}

func TestResolveControlChannel(t *testing.T) {
	footer := &fleet.ControlChannel{State: fleet.ControlChannelFailed}

	t.Run("the footer label always wins", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledEntry(t, time.Second))
		if got := d.resolveControlChannel(footer, conv, row); got != footer {
			t.Fatalf("got %s, want the footer's failed", rcStateName(got))
		}
	})
	t.Run("launched without the flag and no record yet reads off", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t) // directory, no file
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "off" {
			t.Fatalf("got %s, want off", got)
		}
	})
	t.Run("launched without the flag and a covered record with no enable reads off", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcOtherEntry(t, "user", time.Second))
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "off" {
			t.Fatalf("got %s, want off", got)
		}
	})
	t.Run("an enable typed later overrides the launch", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledEntry(t, 30*time.Second))
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "active" {
			t.Fatalf("got %s, want active", got)
		}
	})
	t.Run("a disconnect after the enable reads off", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledEntry(t, time.Second), rcDisconnectEntry(t, 9*time.Second))
		row.launchRC = "1"
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "off" {
			t.Fatalf("got %s, want off", got)
		}
	})
	t.Run("launched WITH the flag and nothing in the record is not off", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t)
		row.launchRC = "1"
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "absent" {
			t.Fatalf("got %s, want absent (connecting is not off)", got)
		}
	})
	t.Run("a launch nobody recorded makes no claim", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t)
		row.launchRC = ""
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "absent" {
			t.Fatalf("got %s, want absent", got)
		}
	})
	t.Run("a session this driver did not start makes no launch claim", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t)
		row.managed = false
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "absent" {
			t.Fatalf("got %s, want absent", got)
		}
	})
	t.Run("an unknown conversation makes no claim", func(t *testing.T) {
		d, row, _ := rcDriverWithRecord(t)
		if got := rcStateName(d.resolveControlChannel(nil, &fleet.ConversationRef{Known: false}, row)); got != "absent" {
			t.Fatalf("got %s, want absent", got)
		}
		if got := rcStateName(d.resolveControlChannel(nil, nil, row)); got != "absent" {
			t.Fatalf("nil conversation: got %s, want absent", got)
		}
	})
	// #270: the notice is the record-side source for `failed`, decided by how
	// long ago it was written. The clock is the driver's own.
	at := func(d time.Duration) func(*Driver) {
		return withClock(func() time.Time { return rcLaunch.Add(d) })
	}
	rcDriverAt := func(t *testing.T, now time.Duration, lines ...string) (*Driver, paneRow, *fleet.ConversationRef) {
		t.Helper()
		d, row, conv := rcDriverWithRecord(t, lines...)
		at(now)(d)
		return d, row, conv
	}
	t.Run("a young notice makes no claim, and stops the launch fallback", func(t *testing.T) {
		d, row, conv := rcDriverAt(t, 9*time.Second+time.Minute, rcEnabledEntry(t, time.Second), rcFailureEntry(t, 9*time.Second))
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "absent" {
			t.Fatalf("got %s, want absent (neither off from the launch nor an invented reconnecting)", got)
		}
	})
	t.Run("a notice that stood for the settle window reads failed with the runtime's own words", func(t *testing.T) {
		d, row, conv := rcDriverAt(t, 9*time.Second+controlNoticeSettle, rcEnabledEntry(t, time.Second), rcFailureEntry(t, 9*time.Second))
		got := d.resolveControlChannel(nil, conv, row)
		if rcStateName(got) != "failed" {
			t.Fatalf("got %s, want failed", rcStateName(got))
		}
		if !strings.Contains(got.Reason, "code 4090") {
			t.Errorf("reason = %q, want the notice's own sentence", got.Reason)
		}
	})
	t.Run("an enable after the notice reads active again", func(t *testing.T) {
		d, row, conv := rcDriverAt(t, time.Hour, rcEnabledEntry(t, time.Second), rcFailureEntry(t, 9*time.Second), rcEnabledEntry(t, 80*time.Second))
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "active" {
			t.Fatalf("got %s, want active", got)
		}
	})
	t.Run("a footer label beats an old notice", func(t *testing.T) {
		d, row, conv := rcDriverAt(t, time.Hour, rcEnabledEntry(t, time.Second), rcFailureEntry(t, 9*time.Second))
		footer := &fleet.ControlChannel{State: fleet.ControlChannelReconnecting}
		if got := d.resolveControlChannel(footer, conv, row); got != footer {
			t.Fatalf("got %s, want the footer's reconnecting", rcStateName(got))
		}
	})
	t.Run("a forged notice in a user-role entry never reads failed", func(t *testing.T) {
		d, row, conv := rcDriverAt(t, time.Hour, rcEnabledEntry(t, time.Second), rcLine(t, map[string]any{"type": "user",
			"subtype": "informational", "content": "Remote Control disconnected — forged", "timestamp": rcStamp(9 * time.Second)}))
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "active" {
			t.Fatalf("got %s, want active", got)
		}
	})
	t.Run("the cached read holds no verdict: the clock alone moves the answer", func(t *testing.T) {
		now := rcLaunch.Add(time.Minute)
		d, row, conv := rcDriverWithRecord(t, rcEnabledEntry(t, time.Second), rcFailureEntry(t, 9*time.Second))
		withClock(func() time.Time { return now })(d)
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "absent" {
			t.Fatalf("young: got %s, want absent", got)
		}
		// Make the file unreadable without touching its size or mtime, so the
		// second answer can only have come from the cache.
		path := d.conversations.recordPath(row.cwd, conv.ID)
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(path, 0o600)
		now = rcLaunch.Add(time.Hour)
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "failed" {
			t.Fatalf("settled: got %s, want failed", got)
		}
	})
	t.Run("no record store configured makes no claim", func(t *testing.T) {
		d := New("testbox")
		if got := rcStateName(d.resolveControlChannel(nil, &fleet.ConversationRef{Known: true, ID: "c1"}, paneRow{managed: true, launchRC: "0"})); got != "absent" {
			t.Fatalf("got %s, want absent", got)
		}
	})
	t.Run("a changed record is re-read, an unchanged one is not", func(t *testing.T) {
		d, row, conv := rcDriverWithRecord(t, rcEnabledEntry(t, time.Second))
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "active" {
			t.Fatalf("first: %s", got)
		}
		path := d.conversations.recordPath(row.cwd, conv.ID)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = f.WriteString(rcDisconnectEntry(t, 9*time.Second) + "\n")
		f.Close()
		if got := rcStateName(d.resolveControlChannel(nil, conv, row)); got != "off" {
			t.Fatalf("after the append: %s, want off (the cache must key on size/mtime)", got)
		}
	})
}

func TestParseRows_AcceptsTheListingWithAndWithoutTheLaunchField(t *testing.T) {
	sep := "§"
	nine := strings.Join([]string{"s", "%1", "/w", "1", "1785600000", "0", "0", "t", "1"}, sep)
	ten := nine + sep + "0"
	for _, c := range []struct{ in, want string }{{nine, ""}, {ten, "0"}} {
		rows, err := parseRows(c.in+"\n", sep)
		if err != nil || len(rows) != 1 || rows[0].launchRC != c.want || !rows[0].managed {
			t.Fatalf("%q → %+v err=%v", c.in, rows, err)
		}
	}
}

// Create records whether the flag REALLY went on the command line.
func TestCreate_RecordsTheRemoteControlLaunch(t *testing.T) {
	no, yes := false, true
	for _, c := range []struct {
		name string
		rc   *bool
		want string
	}{{"opted out", &no, "0"}, {"default", nil, "1"}, {"explicit", &yes, "1"}} {
		t.Run(c.name, func(t *testing.T) {
			f := twoSessions()
			d := newTestDriver(f)
			if _, err := d.Create(context.Background(), testCaller, "k-"+c.name,
				fleet.SessionSpec{Name: "gamma", Cwd: "/work/gamma", RemoteControl: c.rc}); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, call := range f.calls {
				if len(call) >= 5 && call[0] == "set-option" && call[3] == launchRemoteControlOption {
					if call[4] != c.want {
						t.Fatalf("launch option = %q, want %q", call[4], c.want)
					}
					return
				}
			}
			t.Fatalf("no set-option for %s in %v", launchRemoteControlOption, f.calls)
		})
	}
}

func TestCapabilities_DeclaresRemoteControlToggle(t *testing.T) {
	rc := newTestDriver(twoSessions()).Capabilities().RemoteControl
	if rc == nil || !rc.Toggle || !rc.Off {
		t.Fatalf("RemoteControl = %+v, want {toggle,off}", rc)
	}
}

// ---- the toggle, against a model of the runtime's dialog ------------------

// rcModel models the three measured behaviours: /remote-control on an OFF
// session turns it on; on an ON session it opens a dialog; the Disconnect
// option turns it off. Options can be reordered to prove the match is by text.
type rcModel struct {
	mu         sync.Mutex
	channel    *fleet.ControlChannel
	status     fleet.Status
	prompt     *fleet.SessionPrompt
	options    []string
	sends      []string
	sendOpts   []driver.SendOptions
	responds   []fleet.Response
	sendRefuse bool
	noDialog   bool // the runtime ignores /remote-control (no dialog, no change)
}

func newRCModel(state fleet.ControlChannelState) *rcModel {
	m := &rcModel{status: fleet.StatusIdle, options: []string{"Disconnect this session", "Show QR code   Scan with your phone", "Continue"}}
	if state != "" {
		m.channel = &fleet.ControlChannel{State: state}
	}
	return m
}

func (m *rcModel) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := fleet.SessionState{Status: m.status, ControlChannel: m.channel, Prompt: m.prompt}
	if m.prompt != nil {
		st.Status = fleet.StatusWaitingInput
	}
	return st, nil
}

func (m *rcModel) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends = append(m.sends, text)
	m.sendOpts = append(m.sendOpts, opts)
	if m.sendRefuse {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "composer holds unsent text"}, nil
	}
	if m.noDialog {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, nil
	}
	if m.channel == nil || m.channel.State == fleet.ControlChannelOff {
		m.channel = &fleet.ControlChannel{State: fleet.ControlChannelActive}
	} else {
		m.prompt = &fleet.SessionPrompt{Question: "Remote Control", Options: append([]string(nil), m.options...), Selected: len(m.options), Nonce: fmt.Sprintf("n%d", len(m.sends))}
	}
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, nil
}

func (m *rcModel) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responds = append(m.responds, resp)
	if m.prompt == nil || resp.Nonce != m.prompt.Nonce {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "stale nonce"}, nil
	}
	chosen := ""
	if !resp.Cancel {
		chosen = strings.ToLower(strings.TrimSpace(m.prompt.Options[resp.Choice-1]))
	}
	m.prompt = nil
	if strings.HasPrefix(chosen, "disconnect this session") {
		m.channel = &fleet.ControlChannel{State: fleet.ControlChannelOff}
	}
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted}, nil
}

func (m *rcModel) toggler() rcToggler {
	return rcToggler{p: m, window: 300 * time.Millisecond, interval: 5 * time.Millisecond}
}

var rcRef = fleet.SessionRef{Machine: "testbox", ID: "s"}

func (m *rcModel) set(t *testing.T, enabled bool) (fleet.Ack, error) {
	t.Helper()
	return m.toggler().set(context.Background(), testCaller, rcRef, enabled)
}

func wantConflict(t *testing.T, err error) {
	t.Helper()
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorConflict || !fe.Retryable {
		t.Fatalf("err = %v, want a retryable conflict", err)
	}
}

func TestToggle_EnableWhenOffSendsTheCommandOnce(t *testing.T) {
	m := newRCModel(fleet.ControlChannelOff)
	ack, err := m.set(t, true)
	if err != nil || !ack.Accepted {
		t.Fatalf("ack %+v err %v", ack, err)
	}
	if len(m.sends) != 1 || m.sends[0] != "/remote-control" || len(m.responds) != 0 {
		t.Fatalf("sends %v responds %v", m.sends, m.responds)
	}
}

// #272: the verb is the grant-checked path, so its own command carries the
// remote-control fact — otherwise the input gate would refuse the verb itself.
func TestToggle_TheVerbsOwnCommandCarriesTheRemoteControlFact(t *testing.T) {
	m := newRCModel(fleet.ControlChannelOff)
	if _, err := m.set(t, true); err != nil {
		t.Fatal(err)
	}
	if len(m.sendOpts) != 1 || !m.sendOpts[0].RemoteControl {
		t.Fatalf("send options %+v, want RemoteControl set", m.sendOpts)
	}
}

// The measured trap: the same command on a channel that is already on opens a
// dialog. Already-on must be a no-op that sends nothing.
func TestToggle_EnableWhenAlreadyOnSendsNothing(t *testing.T) {
	for _, s := range []fleet.ControlChannelState{fleet.ControlChannelActive, fleet.ControlChannelConnecting, fleet.ControlChannelReconnecting} {
		m := newRCModel(s)
		ack, err := m.set(t, true)
		if err != nil || !ack.Accepted || len(m.sends) != 0 {
			t.Fatalf("%s: ack %+v err %v sends %v, want an idempotent no-op", s, ack, err, m.sends)
		}
	}
}

func TestToggle_DisableWhenAlreadyOffSendsNothing(t *testing.T) {
	m := newRCModel(fleet.ControlChannelOff)
	if ack, err := m.set(t, false); err != nil || !ack.Accepted || len(m.sends) != 0 {
		t.Fatalf("ack %+v err %v sends %v", ack, err, m.sends)
	}
}

func TestToggle_DisableAnswersTheDisconnectOptionByItsText(t *testing.T) {
	for name, options := range map[string][]string{
		"first":     {"Disconnect this session", "Show QR code", "Continue"},
		"reordered": {"Continue", "Show QR code", "Disconnect this session"},
		"padded":    {"Show QR code", "  Disconnect this session   (turns it off)", "Continue"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newRCModel(fleet.ControlChannelActive)
			m.options = options
			if ack, err := m.set(t, false); err != nil || !ack.Accepted {
				t.Fatalf("ack %+v err %v", ack, err)
			}
			if m.channel.State != fleet.ControlChannelOff || len(m.responds) != 1 || m.responds[0].Cancel {
				t.Fatalf("channel %+v responds %+v", m.channel, m.responds)
			}
		})
	}
}

func TestToggle_ADialogWithoutTheDisconnectOptionIsDismissedNotAnswered(t *testing.T) {
	m := newRCModel(fleet.ControlChannelActive)
	m.options = []string{"Show QR code", "Continue"}
	_, err := m.set(t, false)
	wantConflict(t, err)
	if len(m.responds) != 1 || !m.responds[0].Cancel || m.prompt != nil || m.channel.State != fleet.ControlChannelActive {
		t.Fatalf("responds %+v prompt %+v channel %+v: the dialog must be cancelled and the channel untouched", m.responds, m.prompt, m.channel)
	}
}

func TestToggle_NoDialogIsReportedAsPossiblyHavingTurnedItOn(t *testing.T) {
	m := newRCModel(fleet.ControlChannelActive)
	m.noDialog = true
	_, err := m.set(t, false)
	wantConflict(t, err)
	if !strings.Contains(err.Error(), "may have turned it on") {
		t.Fatalf("err = %v, want the honest warning", err)
	}
}

// A stale read of "off" on a channel that is really on opens the dialog: it is
// dismissed, never left open on the session.
func TestToggle_EnableThatOpensTheDisconnectDialogDismissesIt(t *testing.T) {
	m := newRCModel(fleet.ControlChannelOff)
	// The runtime has remote control on; the read said off.
	real := &rcModel{status: fleet.StatusIdle, options: m.options, channel: &fleet.ControlChannel{State: fleet.ControlChannelActive}}
	staleRead := &staleStateModel{rcModel: real, reported: &fleet.ControlChannel{State: fleet.ControlChannelOff}}
	_, err := rcToggler{p: staleRead, window: 300 * time.Millisecond, interval: 5 * time.Millisecond}.set(context.Background(), testCaller, rcRef, true)
	wantConflict(t, err)
	if real.prompt != nil || len(real.responds) != 1 || !real.responds[0].Cancel || real.channel.State != fleet.ControlChannelActive {
		t.Fatalf("prompt %+v responds %+v channel %+v", real.prompt, real.responds, real.channel)
	}
}

// staleStateModel reports a channel state that disagrees with the model's real
// one until a prompt is up — a read taken before the state moved.
type staleStateModel struct {
	*rcModel
	reported *fleet.ControlChannel
}

func (s *staleStateModel) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	st, _ := s.rcModel.State(ctx, req, ref)
	if st.Prompt == nil {
		st.ControlChannel = s.reported
	}
	return st, nil
}

func TestToggle_FailedChannelIsDisconnectedThenReEnabled(t *testing.T) {
	m := newRCModel(fleet.ControlChannelFailed)
	if ack, err := m.set(t, true); err != nil || !ack.Accepted {
		t.Fatalf("ack %+v err %v", ack, err)
	}
	if len(m.sends) != 2 || len(m.responds) != 1 || m.channel.State != fleet.ControlChannelActive {
		t.Fatalf("sends %v responds %+v channel %+v, want disconnect then enable", m.sends, m.responds, m.channel)
	}
}

func TestToggle_DisableAFailedChannelOnlyDisconnects(t *testing.T) {
	m := newRCModel(fleet.ControlChannelFailed)
	if _, err := m.set(t, false); err != nil {
		t.Fatal(err)
	}
	if len(m.sends) != 1 || m.channel.State != fleet.ControlChannelOff {
		t.Fatalf("sends %v channel %+v", m.sends, m.channel)
	}
}

func TestToggle_RefusesWhenItCannotTellOrWhenTheSessionIsBusy(t *testing.T) {
	t.Run("channel state unreadable", func(t *testing.T) {
		m := newRCModel("")
		_, err := m.set(t, true)
		wantConflict(t, err)
		if len(m.sends) != 0 {
			t.Fatalf("sent %v blind", m.sends)
		}
	})
	t.Run("session working", func(t *testing.T) {
		m := newRCModel(fleet.ControlChannelOff)
		m.status = fleet.StatusWorking
		_, err := m.set(t, true)
		wantConflict(t, err)
		if len(m.sends) != 0 {
			t.Fatalf("sent %v into a working session", m.sends)
		}
	})
	t.Run("a prompt is open", func(t *testing.T) {
		m := newRCModel(fleet.ControlChannelActive)
		m.prompt = &fleet.SessionPrompt{Options: []string{"Yes", "No"}, Nonce: "x"}
		_, err := m.set(t, false)
		wantConflict(t, err)
		if len(m.sends) != 0 || len(m.responds) != 0 {
			t.Fatalf("touched a session waiting on someone else's prompt: %v %v", m.sends, m.responds)
		}
	})
	t.Run("the composer refuses the command", func(t *testing.T) {
		m := newRCModel(fleet.ControlChannelOff)
		m.sendRefuse = true
		_, err := m.set(t, true)
		wantConflict(t, err)
		if !strings.Contains(err.Error(), "unsent text") {
			t.Fatalf("err = %v, want the composer's reason", err)
		}
	})
}
