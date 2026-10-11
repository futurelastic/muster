package modclient

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/futurelastic/muster/internal/procgroup"
)

// Proc is a running module process as the client sees it. It is an interface so
// tests can serve the protocol over in-memory pipes; ExecLauncher provides the
// real one.
type Proc interface {
	// Stdin is where requests are written. Closing it is the polite way to tell
	// the module to shut down: it drops its connections but keeps its on-disk
	// state, so the next start can re-attach.
	Stdin() io.WriteCloser
	// Stdout is where responses are read from, one JSON object per line.
	Stdout() io.Reader
	// Stderr is the module's diagnostic output. It is logged as untrusted text
	// and never parsed.
	Stderr() io.Reader
	// Wait blocks until the process has exited. It is called once.
	Wait() error
	// Kill terminates the process (for the real launcher, its whole process
	// group). It must be safe to call at any time, more than once.
	Kill()
	// Pid is the process id, or 0 when there is none.
	Pid() int
}

// Launcher starts a module: path is the executable, args are its arguments
// (always ["serve"]) and env is its COMPLETE environment.
type Launcher func(ctx context.Context, path string, args []string, env []string) (Proc, error)

// readGrace is how long after a module exits its output pipes stay readable, so
// that a last response or stderr line already in the pipe is not lost, while a
// descendant that inherited the pipe and outlives the module cannot keep our
// reader goroutines alive forever.
const readGrace = time.Second

// ExecLauncher runs path with args as a real child process.
//
//   - The environment is env exactly; nothing is inherited. A nil env is an
//     EMPTY environment, not the parent's (os/exec would otherwise inherit).
//   - The working directory is os.TempDir(): a module never runs "inside" the
//     daemon's own directory or any session's.
//   - The child leads its own process group — a kill-on-close Job Object on
//     Windows (internal/procgroup) — so Kill takes down anything it spawned and
//     a terminal signal aimed at the daemon does not reach it.
//   - The pipes are created here, not by os/exec, so Wait does not close them
//     under a reader that has not finished (the documented os/exec trap).
//
// ctx is only consulted before starting: a running module is stopped through
// Proc, never by cancelling a context, so a graceful stdin-close shutdown is
// always available.
func ExecLauncher(ctx context.Context, path string, args []string, env []string) (Proc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW)
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, err
	}
	if env == nil {
		env = []string{}
	}
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Dir = os.TempDir()
	procgroup.Prepare(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		return nil, err
	}
	// Under control before anything else: where the child is created suspended
	// it runs only once it is in its group, and a failed Attach has already
	// ended and reaped it.
	group, err := procgroup.Attach(cmd)
	if err != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		return nil, err
	}
	// The child holds its own copies now.
	closeAll(inR, outW, errW)
	return &execProc{cmd: cmd, group: group, stdin: inW, stdout: outR, stderr: errR}, nil
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		_ = f.Close()
	}
}

type execProc struct {
	cmd            *exec.Cmd
	group          *procgroup.Group
	stdin          *os.File
	stdout, stderr *os.File

	mu     sync.Mutex
	waited bool
}

func (p *execProc) Stdin() io.WriteCloser { return p.stdin }
func (p *execProc) Stdout() io.Reader     { return p.stdout }
func (p *execProc) Stderr() io.Reader     { return p.stderr }
func (p *execProc) Pid() int              { return p.cmd.Process.Pid }

func (p *execProc) Wait() error {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waited = true
	p.mu.Unlock()
	// Where the group is a Job Object, closing it ends whatever the module left
	// running; on Unix there is nothing to release.
	_ = p.group.Close()
	// Give readers a moment to drain what is already in the pipes, then close
	// our read ends so a lingering descendant cannot hold a goroutine forever.
	time.AfterFunc(readGrace, func() {
		_ = p.stdout.Close()
		_ = p.stderr.Close()
	})
	return err
}

// Kill ends the process group. After Wait has returned the process is gone
// and its id may be reused, so it does nothing then: a stale id must never be
// signalled. (A window of a few instructions between the kernel reaping the
// child and Wait returning remains; it is accepted.)
func (p *execProc) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waited {
		return
	}
	// The group first, then the process itself in case it is not (yet) a group
	// leader.
	_ = p.group.Kill()
	_ = p.cmd.Process.Kill()
}
