package procgroup

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests re-execute the test binary as their child, so nothing is built and
// nothing outside the standard library is needed on any platform.
const helperEnv = "PROCGROUP_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "sleeper":
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	case "exit":
		os.Exit(0)
	case "spawner":
		// Starts a grandchild immediately, prints its id, then sleeps. Under
		// Windows this runs only after Attach resumed it, so the grandchild is
		// created inside the job.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), helperEnv+"=sleeper")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	return cmd
}

// waitExit waits for cmd in the background and returns a channel closed when
// the wait returned.
func waitExit(cmd *exec.Cmd) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return done
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", within, what)
}

func TestKillReachesAGrandchildSpawnedBeforeAttachReturned(t *testing.T) {
	cmd := helper("spawner")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g, err := Attach(cmd)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer g.Close()

	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		lines <- line
	}()
	var grandchild int
	select {
	case line := <-lines:
		grandchild, err = strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("the spawner printed %q, not a pid", line)
		}
	case <-time.After(15 * time.Second):
		_ = g.Kill()
		t.Fatal("the spawner never reported its grandchild")
	}
	if !Alive(grandchild) {
		t.Fatalf("the grandchild %d is not running before Kill", grandchild)
	}

	if err := g.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	done := waitExit(cmd)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the group leader is still running after Kill")
	}
	eventually(t, 10*time.Second, "the grandchild to be dead after Kill", func() bool { return !Alive(grandchild) })
}

func TestAliveIsTrueForSelfAndFalseForAnExitedChild(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("this process is not reported alive")
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("a non-pid is reported alive")
	}
	cmd := helper("exit")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	eventually(t, 5*time.Second, "an exited, waited-for child to read as not alive", func() bool { return !Alive(pid) })
}

func TestInterruptThenKillWhenGraceful(t *testing.T) {
	cmd := helper("sleeper")
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g, err := Attach(cmd)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer g.Close()
	done := waitExit(cmd)
	if g.Graceful() {
		if err := g.Interrupt(); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		select {
		case <-done:
			return
		case <-time.After(10 * time.Second):
			t.Fatal("the child ignored Interrupt")
		}
	}
	if err := g.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the child is still running after Kill")
	}
}

func TestKillAndCloseAreIdempotent(t *testing.T) {
	cmd := helper("sleeper")
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g, err := Attach(cmd)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	done := waitExit(cmd)
	for i := 0; i < 2; i++ {
		if err := g.Kill(); err != nil {
			t.Fatalf("Kill #%d: %v", i, err)
		}
	}
	<-done
	for i := 0; i < 2; i++ {
		if err := g.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
	if err := g.Kill(); err != nil {
		t.Fatalf("Kill after Close: %v", err)
	}
}
