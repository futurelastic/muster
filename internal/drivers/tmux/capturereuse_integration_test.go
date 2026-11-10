package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveActivityFingerprintMatchesOutput runs the premise of capture reuse against
// a REAL multiplexer (muster #449): a pane that is quiet keeps the same fingerprint,
// so a listing serves its screen from the cache and asks the multiplexer to read
// nothing; a pane that prints moves it, so the very next listing reads it and reports
// what it printed. The fake models that rule; only the real thing can show the
// multiplexer keeps it.
func TestLiveActivityFingerprintMatchesOutput(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no multiplexer on PATH")
	}
	dir := t.TempDir()
	socket := filepath.Join("/tmp", "fl-reuse-"+randomNonce())
	t.Cleanup(func() { _ = os.Remove(socket) })
	logFile := filepath.Join(dir, "invocations.log")
	wrapper := filepath.Join(dir, "mux")
	// Every invocation is logged, so what the driver asked the multiplexer to read
	// can be counted instead of inferred.
	script := "#!/bin/sh\necho \"$@\" >> '" + logFile + "'\nexec " + bin + " -u -S " + socket + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	wake := filepath.Join(dir, "wake")
	body := "echo ready; while [ ! -f '" + wake + "' ]; do sleep 0.2; done; echo woke; exec sleep 600"
	cmd := exec.Command(wrapper, "new-session", "-d", "-x", "80", "-y", "24", "-s", "reuse-live", "sh", "-c", body)
	cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not start a private multiplexer server: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })

	// A pane younger than startingWindow is always read, and this one was created a
	// moment ago. The driver's clock runs ten minutes ahead of the multiplexer's, as
	// if the machine had been up that long; every comparison between the two is a
	// difference that only grows, so none of the rules under test is bent by it.
	ahead := func() time.Time { return time.Now().Add(10 * time.Minute) }
	d := New("reuse-host", WithBinary(wrapper), WithCaptureReuse(time.Second, time.Hour), withClock(ahead))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Reads the driver has asked the multiplexer for so far, counted from the log.
	const readCommand = "capture" + "-pane"
	reads := func() int {
		raw, _ := os.ReadFile(logFile)
		return strings.Count(string(raw), readCommand)
	}
	text := func(caps map[string]paneCapture) string {
		var all []string
		for _, c := range caps {
			all = append(all, c.text)
		}
		return strings.Join(all, "\n")
	}
	list := func() ([]paneRow, map[string]paneCapture) {
		t.Helper()
		rows, caps, err := d.enumerateScreensReusing(ctx)
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want the one session", len(rows))
		}
		return rows, caps
	}

	// Wait for the pane to print, then let that second pass: output is trusted only
	// once a listing in a later second has seen the pane still.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, caps := list(); strings.Contains(text(caps), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pane never printed")
		}
	}
	time.Sleep(2200 * time.Millisecond)

	rows, caps := list()
	if rows[0].activity <= 0 || rows[0].sig == "" {
		t.Fatalf("the multiplexer reported no activity fingerprint for the pane: activity=%d sig=%q", rows[0].activity, rows[0].sig)
	}
	if !strings.Contains(text(caps), "ready") {
		t.Fatalf("the read lost the screen: %q", text(caps))
	}

	// Quiet: a few listings a little over a second apart read nothing.
	for i := 0; i < 3; i++ {
		time.Sleep(1100 * time.Millisecond)
		before := reads()
		_, caps := list()
		if got := reads() - before; got != 0 {
			t.Fatalf("listing %d of a pane that printed nothing read it %d time(s); the fingerprint moved with no output", i+1, got)
		}
		if !strings.Contains(text(caps), "ready") {
			t.Fatalf("the reused screen is wrong: %q", text(caps))
		}
	}

	// Output: the next listing after it landed reads the pane and reports it.
	if err := os.WriteFile(wake, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		out, err := exec.Command(wrapper, readCommand, "-p", "-t", rows[0].paneID).Output()
		if err == nil && strings.Contains(string(out), "woke") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pane never printed after the trigger")
		}
	}
	before := reads()
	_, caps = list()
	if got := reads() - before; got != 1 {
		t.Errorf("the first listing after the pane printed read it %d time(s), want 1", got)
	}
	if !strings.Contains(text(caps), "woke") {
		t.Errorf("the first listing after the pane printed still reports the old screen: %q", text(caps))
	}
}
