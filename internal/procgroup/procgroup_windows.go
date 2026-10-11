//go:build windows

package procgroup

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Everything below goes through kernel32.dll, loaded lazily. Both it and
// advapi32.dll are KnownDLLs, so loading them by bare name cannot be satisfied
// by a file planted next to the executable.
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObject           = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procThread32First             = kernel32.NewProc("Thread32First")
	procThread32Next              = kernel32.NewProc("Thread32Next")
	procOpenThread                = kernel32.NewProc("OpenThread")
	procResumeThread              = kernel32.NewProc("ResumeThread")
)

const (
	// Process creation flags (winbase.h).
	createSuspended       = 0x00000004
	createNewProcessGroup = 0x00000200

	// JOBOBJECT_BASIC_LIMIT_INFORMATION.LimitFlags: every process in the job
	// is terminated when the last handle to the job closes. Neither
	// BREAKAWAY_OK nor SILENT_BREAKAWAY_OK is set, so no descendant can leave.
	jobObjectLimitKillOnJobClose = 0x00002000

	// JOBOBJECTINFOCLASS values.
	jobObjectBasicAccountingInformation = 1
	jobObjectExtendedLimitInformation   = 9

	th32csSnapThread               = 0x00000004
	threadSuspendResume            = 0x0002
	processQueryLimitedInformation = 0x1000
	synchronize                    = 0x00100000
	waitTimeout                    = 258

	// How long Kill waits for the job to drain. A file a dying process still
	// holds cannot be removed on Windows, so a caller that deletes a directory
	// right after Kill wants the job actually empty.
	drainBound = 5 * time.Second
)

// jobObjectBasicLimitInformation is JOBOBJECT_BASIC_LIMIT_INFORMATION.
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// ioCounters is IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobObjectExtendedLimitInformation is JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
// It is 144 bytes on a 64-bit build; a test pins that, because a wrong size is
// rejected by the kernel as an invalid parameter.
type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// jobObjectBasicAccountingInfo is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobObjectBasicAccountingInfo struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// threadEntry32 is THREADENTRY32.
type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
}

// Group is a Job Object holding a started child and everything it starts.
type Group struct {
	mu  sync.Mutex
	job syscall.Handle // zero once closed
}

// resumeThread resumes the suspended primary thread of the process with this
// id. It is a variable so a test can make that step fail.
var resumeThread = resumeMainThread

// Supported reports whether this platform can stop a child's whole tree.
func Supported() bool { return true }

// Prepare creates the child suspended, so it cannot run (and cannot start a
// grandchild) before Attach has put it in the job, and in its own process
// group, so a Ctrl+C aimed at the service's console does not reach it. Call it
// before Start.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended | createNewProcessGroup
}

// Attach puts the started, still-suspended child in a new kill-on-close job and
// resumes it. Call it right after Start and before Wait.
//
// Any failed step terminates what was started and fails the call: the child is
// dead and has been waited for, so a caller only returns the error. The
// alternative — carrying on with a child that is outside any job — is the
// failure this package exists to prevent.
func Attach(cmd *exec.Cmd) (*Group, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("procgroup: the command has not been started")
	}
	job, err := newJob()
	if err != nil {
		abandon(cmd, 0)
		return nil, fmt.Errorf("procgroup: creating the job object: %w", err)
	}

	var assignErr error
	if err := cmd.Process.WithHandle(func(h uintptr) {
		r, _, e := procAssignProcessToJobObject.Call(uintptr(job), h)
		if r == 0 {
			assignErr = e
		}
	}); err != nil {
		assignErr = err
	}
	if assignErr != nil {
		abandon(cmd, job)
		return nil, fmt.Errorf("procgroup: assigning the process to the job object: %w", assignErr)
	}

	if err := resumeThread(uint32(cmd.Process.Pid)); err != nil {
		// The process is in the job, so terminating the job is what ends it.
		_, _, _ = procTerminateJobObject.Call(uintptr(job), 1)
		abandon(cmd, job)
		return nil, fmt.Errorf("procgroup: resuming the process: %w", err)
	}
	return &Group{job: job}, nil
}

// abandon ends a child that could not be put under control, releases the job
// (zero means none was made) and reaps the process.
func abandon(cmd *exec.Cmd, job syscall.Handle) {
	_ = cmd.Process.Kill()
	if job != 0 {
		_ = syscall.CloseHandle(job)
	}
	_ = cmd.Wait()
}

// newJob creates an unnamed job with kill-on-close. The handle is not
// inheritable (no security attributes), which matters: a child holding it
// would keep the job alive past the service, and kill-on-close would never
// fire.
func newJob() (syscall.Handle, error) {
	r, _, e := procCreateJobObject.Call(0, 0)
	if r == 0 {
		return 0, e
	}
	job := syscall.Handle(r)
	var info jobObjectExtendedLimitInfo
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, e = procSetInformationJobObject.Call(
		uintptr(job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		_ = syscall.CloseHandle(job)
		return 0, fmt.Errorf("setting kill-on-close: %w", e)
	}
	return job, nil
}

// resumeMainThread resumes the suspended thread(s) of process pid.
//
// os/exec closes the primary-thread handle CreateProcess returns, so the
// thread is found again through a Toolhelp32 snapshot. The pid cannot have been
// reused: the caller holds the process handle through os.Process.
func resumeMainThread(pid uint32) error {
	snap, err := syscall.CreateToolhelp32Snapshot(th32csSnapThread, 0)
	if err != nil {
		return fmt.Errorf("snapshotting threads: %w", err)
	}
	defer syscall.CloseHandle(snap)

	var e threadEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	r, _, callErr := procThread32First.Call(uintptr(snap), uintptr(unsafe.Pointer(&e)))
	if r == 0 {
		return fmt.Errorf("listing threads: %w", callErr)
	}
	resumed := false
	for {
		if e.OwnerProcessID == pid {
			// A thread that cannot be opened or resumed is skipped: what
			// matters is that the one that was suspended ran.
			if prev, err := resumeOne(e.ThreadID); err == nil && prev >= 1 {
				resumed = true
			}
		}
		e.Size = uint32(unsafe.Sizeof(e))
		r, _, _ = procThread32Next.Call(uintptr(snap), uintptr(unsafe.Pointer(&e)))
		if r == 0 {
			break
		}
	}
	if !resumed {
		return errors.New("no suspended thread found for the process")
	}
	return nil
}

// resumeOne resumes one thread and returns its previous suspend count.
func resumeOne(tid uint32) (uint32, error) {
	h, _, e := procOpenThread.Call(threadSuspendResume, 0, uintptr(tid))
	if h == 0 {
		return 0, e
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	prev, _, e := procResumeThread.Call(h)
	if uint32(prev) == 0xFFFFFFFF {
		return 0, e
	}
	return uint32(prev), nil
}

// Graceful is false: there is no console shared with a child to deliver a
// gentle stop through, and a stop that depended on how the service happened to
// be launched would be worse than none.
func (g *Group) Graceful() bool { return false }

// Interrupt is not available on Windows.
func (g *Group) Interrupt() error { return errors.ErrUnsupported }

// Kill terminates every process in the job and waits, up to a bound, for the
// job to be empty. Safe to call at any time and more than once; it does nothing
// after Close.
func (g *Group) Kill() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}
	if r, _, e := procTerminateJobObject.Call(uintptr(g.job), 1); r == 0 {
		return fmt.Errorf("procgroup: terminating the job: %w", e)
	}
	deadline := time.Now().Add(drainBound)
	for {
		n, err := activeProcesses(g.job)
		if err != nil || n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("procgroup: %d process(es) still active %s after the job was terminated", n, drainBound)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// activeProcesses is the number of processes currently in the job.
func activeProcesses(job syscall.Handle) (uint32, error) {
	var acct jobObjectBasicAccountingInfo
	r, _, e := procQueryInformationJobObject.Call(
		uintptr(job),
		jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&acct)),
		unsafe.Sizeof(acct),
		0,
	)
	if r == 0 {
		return 0, e
	}
	return acct.ActiveProcesses, nil
}

// Close releases the job handle. Because the job was created with kill-on-close,
// anything still in it ends here: closing a Group without Kill is a complete
// stop. Idempotent.
func (g *Group) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}
	h := g.job
	g.job = 0
	return syscall.CloseHandle(h)
}

// Alive reports whether a process with this id exists and has not exited.
// Waiting on the process with a zero timeout is exact: a process that happens
// to exit with the code GetExitCodeProcess uses for "still running" is not
// mistaken for a live one.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation|synchronize, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	ev, err := syscall.WaitForSingleObject(h, 0)
	return err == nil && ev == waitTimeout
}
