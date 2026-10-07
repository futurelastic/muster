package tmux

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// A live run of the whole remote-control toggle against the real runtime
// (muster #269), the way the daemon wires the driver: a record root and a
// process-sessions root, so the channel is read from the runtime's own record.
//
// Opt-in twice, because it is not free: FLEET_TMUX_INTEGRATION=1 for a real
// multiplexer, and FLEET_CLAUDE_LIVE=1 because it starts the real runtime on
// the operator's own account and registers (then disconnects) a remote-control
// bridge with the vendor's service. It leaves nothing behind: the session is
// closed on exit, and the channel is disconnected before it.
func TestLiveRemoteControlToggle(t *testing.T) {
	if os.Getenv("FLEET_CLAUDE_LIVE") != "1" {
		t.Skip("set FLEET_CLAUDE_LIVE=1 (with FLEET_TMUX_INTEGRATION=1) to run the real runtime")
	}
	m := newLiveMux(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("no runtime on PATH")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	d := New("livebox", WithBinary(m.wrapper),
		WithRecordRoot(home+"/.claude/projects"), WithProcessSessionsRoot(home+"/.claude/sessions"))

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	off := false
	sess, err := d.Create(ctx, testCaller, "live-rc-1", fleet.SessionSpec{
		Name: "rctoggle", Cwd: fleet.AbsolutePath(t.TempDir()), RemoteControl: &off, TrustCwd: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := fleet.SessionRef{Machine: "livebox", ID: sess.ID}
	t.Cleanup(func() {
		_, _ = d.SetRemoteControl(context.Background(), testCaller, ref, false)
		_, _ = d.Close(context.Background(), testCaller, ref)
	})

	waitFor := func(what string, want fleet.ControlChannelState, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		var last string
		for time.Now().Before(deadline) {
			st, err := d.State(ctx, testCaller, ref)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case st.ControlChannel == nil:
				last = "absent"
			case st.ControlChannel.State == want:
				return
			default:
				last = string(st.ControlChannel.State)
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("%s: controlChannel never reached %q (last %s)", what, want, last)
	}

	// A session created with remoteControl:false reads off — not absent.
	waitFor("created with remoteControl:false", fleet.ControlChannelOff, 60*time.Second)

	if ack, err := d.SetRemoteControl(ctx, testCaller, ref, true); err != nil || !ack.Accepted {
		t.Fatalf("enable: %+v %v", ack, err)
	}
	waitFor("after enable", fleet.ControlChannelActive, 30*time.Second)

	// Enabling again must be a no-op, not a dialog left open on the session.
	if ack, err := d.SetRemoteControl(ctx, testCaller, ref, true); err != nil || !ack.Accepted {
		t.Fatalf("second enable: %+v %v", ack, err)
	}
	if st, _ := d.State(ctx, testCaller, ref); st.Prompt != nil {
		t.Fatalf("a dialog is open after enabling twice: %+v", st.Prompt)
	}

	if ack, err := d.SetRemoteControl(ctx, testCaller, ref, false); err != nil || !ack.Accepted {
		t.Fatalf("disable: %+v %v", ack, err)
	}
	waitFor("after disable", fleet.ControlChannelOff, 30*time.Second)
}
