//go:build unix

package opencode

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// reapStale stops a server the PREVIOUS service left running, so a relaunch
// does not put two processes on one session database. A recorded pid is only
// acted on when the process answering to it is demonstrably the same server —
// the recorded binary serving the recorded port — because a pid alone may now
// belong to anything. Returns whether it stopped one.
func reapStale(r *sessionRecord) bool {
	if r.Pid <= 1 || r.Port == 0 {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(r.Pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	cmdline := string(out)
	if !strings.Contains(cmdline, " serve ") || !strings.Contains(cmdline, "--port "+strconv.Itoa(r.Port)) {
		return false
	}
	if r.Bin != "" && !strings.Contains(cmdline, r.Bin) {
		return false
	}
	stopGroups([]int{r.Pid})
	return true
}

// stopGroups stops the process group led by each pid: all of them are asked to
// end at once and given one shared grace period, then every group is killed —
// also one whose leader already exited, because a tool the session ran can
// outlive the server it ran under (muster #280). Killing a group that has no
// member left is not an error: a group id is not reused while it has members, so
// the signal reaches our group or nothing. Returns the pids still running.
func stopGroups(pids []int) (survivors []int) {
	for _, pid := range pids {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		running := false
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				running = true
				break
			}
		}
		if !running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range pids {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil {
			survivors = append(survivors, pid)
		}
	}
	return survivors
}

// scanServers finds the opencode servers running under root: the processes
// that are the leader of their own process group, run `serve --port N
// --hostname 127.0.0.1`, and carry a TMPDIR inside a scoped directory directly
// under root (orphans.go says why that is the identity). uninspected counts
// processes that run the same command but could not be read — another user's, or
// gone unreadable — which a caller must not mistake for "none".
//
// Where the platform has no way to read another process's environment the answer
// is ErrScanUnsupported, not an empty list.
func scanServers(root string) (found []serverProc, uninspected int, err error) {
	switch runtime.GOOS {
	case "darwin":
		return scanWithPS(root)
	case "linux":
		return scanProc(root)
	}
	return nil, 0, ErrScanUnsupported
}

// inspectServer reads one process again and reports it when it is a server of
// root, so the identity is checked a second time right before a signal.
func inspectServer(pid int, root string) (serverProc, bool) {
	switch runtime.GOOS {
	case "darwin":
		p, ok, _ := inspectWithPS(pid, 0, root)
		return p, ok
	case "linux":
		return inspectProc(pid, root)
	}
	return serverProc{}, false
}

// ---- macOS: ps -E ---------------------------------------------------------

// scanWithPS lists the machine's processes without their environment first —
// that listing is cheap and holds no secrets — and reads the environment of the
// few that run a server's command line, one `ps -E` each.
func scanWithPS(root string) ([]serverProc, int, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,pgid=,uid=,command=").Output()
	if err != nil {
		return nil, 0, err
	}
	var found []serverProc
	uninspected := 0
	self := os.Getuid()
	for _, line := range strings.Split(string(out), "\n") {
		pid, pgid, uid, cmd, ok := splitPSLine(line)
		if !ok {
			continue
		}
		if _, isServe := servePort(cmd); !isServe {
			continue
		}
		if uid != self {
			uninspected++
			continue
		}
		p, ok, hidden := inspectWithPS(pid, pgid, root)
		switch {
		case ok:
			found = append(found, p)
		case hidden:
			uninspected++
		}
	}
	return found, uninspected, nil
}

// inspectWithPS reads pid's command line and environment with `ps -E` and
// returns it when it is a server of root leading its own group. pgid 0 asks ps
// for the group too. hidden is set when ps printed no environment at all: macOS
// does not show the environment of a protected process, and a server whose
// environment cannot be read is not one the caller may call "none".
func inspectWithPS(pid, pgid int, root string) (p serverProc, ok, hidden bool) {
	if pgid == 0 {
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pgid=").Output()
		if err != nil {
			return serverProc{}, false, false
		}
		pgid, err = strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return serverProc{}, false, false
		}
	}
	if pgid != pid {
		return serverProc{}, false, false
	}
	out, err := exec.Command("ps", "-ww", "-E", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return serverProc{}, false, false
	}
	line := strings.TrimSpace(string(out))
	p, ok = serverFromLine(line, pid, pgid, root)
	return p, ok, !ok && envHidden(line)
}

// envHidden reports whether a `ps -E` line carries no environment: every server
// this driver starts has PATH and HOME, so a line with neither was printed
// without one.
func envHidden(line string) bool {
	return !strings.Contains(line, " PATH=") && !strings.Contains(line, " HOME=")
}

// splitPSLine splits one line of `ps -o pid=,pgid=,uid=,command=`.
func splitPSLine(line string) (pid, pgid, uid int, cmd string, ok bool) {
	rest := strings.TrimLeft(line, " \t")
	var nums [3]int
	for i := range nums {
		field, after, found := strings.Cut(rest, " ")
		if !found {
			return 0, 0, 0, "", false
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return 0, 0, 0, "", false
		}
		nums[i] = n
		rest = strings.TrimLeft(after, " ")
	}
	if rest == "" {
		return 0, 0, 0, "", false
	}
	return nums[0], nums[1], nums[2], rest, true
}

// ---- Linux: /proc -----------------------------------------------------------

func scanProc(root string) ([]serverProc, int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, 0, err
	}
	var found []serverProc
	uninspected := 0
	self := os.Getuid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if _, isServe := servePort(nulToSpace(cmd)); !isServe {
			continue
		}
		if info, err := os.Stat(filepath.Join("/proc", e.Name())); err != nil {
			continue
		} else if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != self {
			uninspected++
			continue
		}
		if p, ok := inspectProc(pid, root); ok {
			found = append(found, p)
		} else if _, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ")); err != nil && !os.IsNotExist(err) {
			uninspected++
		}
	}
	return found, uninspected, nil
}

func inspectProc(pid int, root string) (serverProc, bool) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return serverProc{}, false
	}
	// "pid (comm) state ppid pgrp ...": comm may hold spaces and parentheses, so the
	// fields are counted from the LAST closing parenthesis.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return serverProc{}, false
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 3 {
		return serverProc{}, false
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil || pgid != pid {
		return serverProc{}, false
	}
	cmd, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return serverProc{}, false
	}
	env, err := os.ReadFile(filepath.Join(dir, "environ"))
	if err != nil {
		return serverProc{}, false
	}
	return serverFromLine(nulToSpace(cmd)+" "+nulToSpace(env), pid, pgid, root)
}

// nulToSpace turns the NUL separators of /proc's files into the spaces `ps`
// prints, so one matcher reads both.
func nulToSpace(b []byte) string { return string(bytes.ReplaceAll(b, []byte{0}, []byte{' '})) }

// serverFromLine builds the result from a command line followed by the
// environment. Only the port and the scoped directory's name leave it.
func serverFromLine(line string, pid, pgid int, root string) (serverProc, bool) {
	port, ok := servePort(line)
	if !ok {
		return serverProc{}, false
	}
	dir, ok := scopedDirIn(line, root)
	if !ok {
		return serverProc{}, false
	}
	return serverProc{pid: pid, pgid: pgid, port: port, dir: dir}, true
}
