package tmux

import (
	"context"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// #272: with the gate on, the runtime's remote-control commands need the
// remote-control or human-relay grant on top of send. With it off, nothing
// changes from before #272.
func TestRemoteControlCommandGate(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		text                            string
		gate, humanRelay, remoteControl bool
		refused                         bool
	}{
		{"send only, gate on, /rc", "/rc", true, false, false, true},
		{"send only, gate on, /remote-control", "/remote-control", true, false, false, true},
		{"send only, gate on, with trailing words", "/rc now please", true, false, false, true},
		{"leading invisible character", "\u200b/rc", true, false, false, true},
		{"leading space", "  /remote-control", true, false, false, true},
		{"command ended by a newline", "/rc\nmore", true, false, false, true},
		{"remote-control grant", "/rc", true, false, true, false},
		{"human-relay grant", "/remote-control", true, true, false, false},
		{"gate off", "/rc", false, false, false, false},
		{"another session command is untouched", "/rename x", true, false, false, false},
		{"a message that merely mentions it", "please run /rc later", true, false, false, false},
		{"a longer command name is not this command", "/rcx", true, false, false, false},
	} {
		reason, refused := refuseRemoteControlCommand(sanitizeForBracketedPaste(tc.text), tc.gate, tc.humanRelay, tc.remoteControl)
		if refused != tc.refused {
			t.Errorf("%s: refused = %v, want %v (%s)", tc.name, refused, tc.refused, reason)
		}
		if refused && (!strings.Contains(reason, "remote-control grant") || !strings.Contains(reason, "human-relay grant")) {
			t.Errorf("%s: reason %q must name both grants", tc.name, reason)
		}
	}
}

// End to end through Send: a gated driver refuses a send-only caller and
// pastes nothing; the grants and the verb's own path get through; an ungated
// driver is unchanged. A control byte the sanitiser drops cannot hide the
// command.
func TestSendGatesRemoteControlCommand(t *testing.T) {
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	for _, tc := range []struct {
		name    string
		gate    bool
		text    string
		opts    driver.SendOptions
		refused bool
	}{
		{"gate on, send only", true, "/rc", driver.SendOptions{Submit: true}, true},
		{"gate on, hidden behind a control byte", true, "\x0b/rc", driver.SendOptions{Submit: true}, true},
		{"gate on, remote-control", true, "/rc", driver.SendOptions{Submit: true, RemoteControl: true}, false},
		{"gate on, human-relay", true, "/remote-control", driver.SendOptions{Submit: true, HumanRelay: true}, false},
		{"gate off, send only", false, "/rc", driver.SendOptions{Submit: true}, false},
	} {
		f := twoSessions()
		d := newTestDriver(f)
		d.gateRemoteControlInput = tc.gate
		got, err := d.Send(context.Background(), testCaller, ref, tc.text, tc.opts)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.refused {
			if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "remote-control grant") {
				t.Errorf("%s: outcome %s (%s), want refused naming the grant", tc.name, got.Outcome, got.Reason)
			}
			if pastesIn(f.callsSnapshot()) != 0 {
				t.Errorf("%s: a refused command was pasted", tc.name)
			}
		} else if got.Outcome == fleet.OutcomeRefused && strings.Contains(got.Reason, "remote-control grant") {
			t.Errorf("%s: refused by the gate: %s", tc.name, got.Reason)
		}
	}
}
