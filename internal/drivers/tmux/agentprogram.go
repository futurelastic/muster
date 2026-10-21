package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Whether the agent program a session starts is on this machine (muster #342).
//
// # The gap this closes, measured
//
// This driver needs tmux and nothing else to register, so a machine with tmux
// and without the agent CLI listed the runtime as available. A create then
// answered 201: tmux accepted `new-session`, the pane's program did not exist,
// and the session was gone before the next read said so (404 on every read
// after it). A client trusting the runtime row had no way to know.
//
// # Resolving it the way a session will start it
//
// A session is not started by the service's own PATH. Unless the driver was
// told to run bare, the agent argv is wrapped in `$SHELL -lic …` (environment.go)
// so that it inherits what a launcher-created session would; the program is
// then found by that shell's PATH, which a user's startup files extend. A
// check against the service's PATH would call a working install missing (a
// service manager starts with a bare PATH), and the opposite check would call
// a missing one present. So the question is put to the same shell, through the
// same flags, and nothing else.
//
// The probe prints a sentinel rather than being judged by its exit status: an
// interactive shell's startup files are free to exit non-zero or print
// anything, and neither is evidence about the program. No sentinel means the
// probe did not run to its end, and that is "not known" (see below).
//
// # Not known is not missing
//
// Missing is reported only for a program positively not found. A shell that
// would not start, a probe that timed out, output with no sentinel, a platform
// this driver has no shell for: all read as "not known", and the caller treats
// that as available. Taking a working runtime offline because a probe had a
// bad day is the worse error.
//
// # Cached, but not for long
//
// Listing the runtimes and every create ask, and each ask would start a login
// shell. The answer is kept for a few seconds — long enough that a burst of
// creates costs one shell, short enough that installing the program is
// noticed without a restart.

const defaultAgentProgram = "claude"

// agentProbeTimeout bounds one probe. A variable so a test can shorten it.
var agentProbeTimeout = 8 * time.Second

const (
	agentFoundTTL   = 30 * time.Second
	agentMissingTTL = 5 * time.Second
	agentUnknownTTL = 10 * time.Second

	agentFoundMark   = "MUSTER_AGENT_PROGRAM_FOUND"
	agentMissingMark = "MUSTER_AGENT_PROGRAM_MISSING"
)

// agentProbeScript takes the program as $1 — a positional parameter, never
// spliced into the script. It accepts what exec would run: a regular,
// executable file. `command -v` also names aliases, functions and builtins,
// none of which `exec "$@"` in the launch wrapper would start, so a bare name
// that is not a file does not count as found.
const agentProbeScript = `p=$(command -v "$1" 2>/dev/null)
if [ -n "$p" ] && [ -f "$p" ] && [ -x "$p" ]; then
  echo ` + agentFoundMark + `
else
  echo ` + agentMissingMark + `
fi`

type agentProbeResult int

const (
	agentUnknown agentProbeResult = iota
	agentFound
	agentMissing
)

type agentProbeCache struct {
	mu     sync.Mutex
	result agentProbeResult
	at     time.Time
	valid  bool
}

// AgentProgramMissing implements driver.AgentProgramReporter.
func (d *Driver) AgentProgramMissing(ctx context.Context) (string, bool) {
	prog := d.agentProgram
	if prog == "" {
		return "", false
	}
	c := &d.agentProbe
	c.mu.Lock()
	defer c.mu.Unlock()
	now := d.now()
	if c.valid {
		ttl := agentUnknownTTL
		switch c.result {
		case agentFound:
			ttl = agentFoundTTL
		case agentMissing:
			ttl = agentMissingTTL
		}
		if now.Sub(c.at) < ttl {
			return prog, c.result == agentMissing
		}
	}
	res := probeAgentProgram(ctx, d.run, d.loginShell(), d.bareExec, prog)
	if ctx.Err() != nil && res == agentUnknown {
		// Our caller gave up; that says nothing about the program and must
		// not be remembered as an answer about it.
		return prog, false
	}
	c.result, c.at, c.valid = res, now, true
	return prog, res == agentMissing
}

// ProbeAgentProgram answers the same question for a caller with no Driver (the
// doctor): is program found the way the default driver's sessions find it?
// known is false when it could not be decided.
func ProbeAgentProgram(ctx context.Context, program string) (found, known bool) {
	res := probeAgentProgram(ctx, runReal, (&Driver{}).loginShell(), false, program)
	return res == agentFound, res != agentUnknown
}

func probeAgentProgram(ctx context.Context, run execFunc, shell string, bare bool, program string) agentProbeResult {
	if runtime.GOOS == "windows" {
		return agentUnknown // no login-shell wrap and no tmux to start it
	}
	if strings.ContainsRune(program, filepath.Separator) {
		// A path names the file; no PATH is involved.
		if fi, err := os.Stat(program); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return agentFound
		}
		return agentMissing
	}
	if bare {
		// Run directly by the multiplexer: the service's own PATH, nothing else.
		if p, err := exec.LookPath(program); err == nil && p != "" {
			return agentFound
		}
		return agentMissing
	}
	pctx, cancel := context.WithTimeout(ctx, agentProbeTimeout)
	defer cancel()
	out, _ := run(pctx, shell, "-lic", agentProbeScript, "muster", program)
	for _, line := range strings.Split(string(out), "\n") {
		switch strings.TrimSpace(line) {
		case agentFoundMark:
			return agentFound
		case agentMissingMark:
			return agentMissing
		}
	}
	return agentUnknown
}
