//go:build windows

package procgroup

import (
	"errors"
	"testing"
	"time"
	"unsafe"
)

func TestCloseWithoutKillEndsTheJob(t *testing.T) {
	cmd := helper("sleeper")
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g, err := Attach(cmd)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	pid := cmd.Process.Pid
	done := waitExit(cmd)
	if !Alive(pid) {
		t.Fatal("the child is not running before Close")
	}
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close without Kill left the child running: kill-on-close did not fire")
	}
}

func TestStructSizesMatchTheWin32Layout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the struct layouts are pinned for 64-bit builds only (windows, #303)")
	}
	if got := unsafe.Sizeof(jobObjectExtendedLimitInfo{}); got != 144 {
		t.Errorf("JOBOBJECT_EXTENDED_LIMIT_INFORMATION is %d bytes, want 144", got)
	}
	if got := unsafe.Sizeof(jobObjectBasicAccountingInfo{}); got != 48 {
		t.Errorf("JOBOBJECT_BASIC_ACCOUNTING_INFORMATION is %d bytes, want 48", got)
	}
	if got := unsafe.Offsetof(jobObjectBasicAccountingInfo{}.ActiveProcesses); got != 40 {
		t.Errorf("ActiveProcesses is at offset %d, want 40", got)
	}
	if got := unsafe.Sizeof(threadEntry32{}); got != 28 {
		t.Errorf("THREADENTRY32 is %d bytes, want 28", got)
	}
}

func TestNoGracefulStopOnWindows(t *testing.T) {
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
		t.Error("Graceful is true on Windows")
	}
	if err := g.Interrupt(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Interrupt = %v, want errors.ErrUnsupported", err)
	}
	_ = g.Kill()
	<-done
}

func TestAttachFailureLeavesNothingRunning(t *testing.T) {
	old := resumeThread
	resumeThread = func(uint32) error { return errors.New("injected") }
	defer func() { resumeThread = old }()

	cmd := helper("sleeper")
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	g, err := Attach(cmd)
	if err == nil {
		_ = g.Kill()
		t.Fatal("Attach succeeded although the resume step failed")
	}
	if g != nil {
		t.Error("Attach returned a Group alongside an error")
	}
	if Alive(pid) {
		t.Errorf("the child %d is still running after a failed Attach", pid)
	}
}
