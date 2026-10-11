package tmux

import (
	"context"
	"strconv"
	"strings"
)

// Reading a session's agent from its own process (muster #301).
//
// # The gap this closes, measured
//
// Session.agent is documented, and so is the `agent` list filter, but the
// only source this driver had for it was the create request: createRecord
// keeps it for createRecordRetention, and only for a session this service
// created. A session started by a launcher or another client, or older than
// the retention, had no agent — on a host with 78 live sessions none
// reported one, so `?agent=x` matched nothing.
//
// The fact is still on the host. The runtime is started with
// `--agent <name>` on its command line, and a process's argv does not change
// after it starts. So the agent is read from the process table, the way this
// driver already reads other runtime facts from the OS (foreground.go,
// processidentity.go), and it needs no memory of the create at all: it holds
// for any creator, any age, across a service restart.
//
// # Which process, and what is refused
//
// The pane's root process is not always the runtime: a session started by
// hand has an interactive shell there, with the runtime a child. So the walk
// starts at the pane's pid and descends, and it considers only processes in
// the terminal's FOREGROUND process group (pgid == tpgid) that are not an
// interactive shell — the same definition of "the runtime is what is in
// front" the pre-paste check uses (foregroundIsRuntime). A shell left in the
// foreground because the runtime exited reports no agent: nothing is running
// that was started with one.
//
// The shallowest level that carries a value decides. Two different values at
// that level are a disagreement, and a disagreement is reported as absence,
// never as a pick: an agent field that might name the wrong persona is worse
// than none, because a caller grouping by it would mis-attribute work.
//
// # Absent is not empty, and not always "none"
//
//   - A runtime started without `--agent` reports no agent (the field is
//     omitted, not empty).
//   - When the process table could not be read at all the field is omitted
//     too, but the listing says so: List marks its source degraded, so a
//     caller filtering by agent sees an incomplete answer rather than an
//     empty one it would read as "nobody is running that agent".
//
// # What it is labelled
//
// The value is what the runtime is actually running, observed — it is the
// APPLIED value (Session.Agent), and when the create record still holds a
// request the pin is resolved against it with source `observed`
// (pinOutcomeObserving). A value remembered from a create request is never
// put in Session.Agent: that is the echo #84 closed.

// processRow is one line of `ps -A -ww -o pid=,ppid=,pgid=,tpgid=,args=`.
type processRow struct {
	pid, ppid, pgid, tpgid int
	args                   string
}

// processTableArgs is the one query agent discovery makes of the OS: every
// process, with its parent, its process group, its terminal's foreground
// process group, and its full argument vector (`-ww` so a long command line is
// not cut at the terminal width). One call per listing, whatever the number of
// sessions — List's constant-spawn contract holds.
var processTableArgs = []string{"-A", "-ww", "-o", "pid=,ppid=,pgid=,tpgid=,args="}

// parseProcessTable parses processTableArgs' output. args is everything after
// the fourth column, so a command line with spaces survives; a line that does
// not parse is skipped, not fatal — one malformed row must not hide the rest.
func parseProcessTable(out string) []processRow {
	var rows []processRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		tpgid, e4 := strconv.Atoi(f[3])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		rows = append(rows, processRow{pid: pid, ppid: ppid, pgid: pgid, tpgid: tpgid, args: strings.Join(f[4:], " ")})
	}
	return rows
}

// agentFromArgs returns the value of the first `--agent` flag on a command
// line, in either `--agent x` or `--agent=x` form. The first token is the
// executable and is never read as a flag; a lone `--` ends the options. A
// value that would itself read as a flag, or is empty, is not a value — the
// same rule safeArgvValue applies where the driver builds the argv.
func agentFromArgs(args string) (string, bool) {
	toks := strings.Fields(args)
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t == "--":
			return "", false
		case t == "--agent":
			if i+1 < len(toks) && safeArgvValue(toks[i+1]) {
				return toks[i+1], true
			}
			return "", false
		case strings.HasPrefix(t, "--agent="):
			if v := strings.TrimPrefix(t, "--agent="); safeArgvValue(v) {
				return v, true
			}
			return "", false
		}
	}
	return "", false
}

// agentsFromTable resolves each root pid to the agent its foreground runtime
// was started with. A root absent from the result has none to report, or its
// processes disagree (see the file comment). Pure: the table in, the answer
// out, so the rule is testable without a process table.
func agentsFromTable(table []processRow, roots map[string]int) map[string]string {
	byPID := make(map[int]processRow, len(table))
	children := make(map[int][]int, len(table))
	for _, p := range table {
		byPID[p.pid] = p
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	out := make(map[string]string, len(roots))
	for session, root := range roots {
		if _, ok := byPID[root]; !ok {
			continue
		}
		seen := map[int]bool{root: true}
		level := []int{root}
		for len(level) > 0 {
			var next []int
			found := map[string]bool{}
			for _, pid := range level {
				p := byPID[pid]
				if p.tpgid > 0 && p.pgid == p.tpgid {
					if fields := strings.Fields(p.args); len(fields) > 0 && !isShellName(fields[0]) {
						if v, ok := agentFromArgs(p.args); ok {
							found[v] = true
						}
					}
				}
				for _, c := range children[pid] {
					if !seen[c] {
						seen[c] = true
						next = append(next, c)
					}
				}
			}
			if len(found) == 1 {
				for v := range found {
					out[session] = v
				}
				break
			}
			if len(found) > 1 {
				break // a disagreement is absence, never a pick
			}
			level = next
		}
	}
	return out
}

// processAgents reads the process table once and answers, per listed session,
// the agent its runtime was started with. ok is false when the table could not
// be read — the caller must then not treat an absent agent as "none".
//
// Off unless WithProcessAgent was set: a driver built for a test must not
// query the real process table merely because it was constructed.
func (d *Driver) processAgents(ctx context.Context, rows []paneRow) (agents map[string]string, ok bool) {
	if !d.agentFromProcess {
		return nil, true
	}
	roots := make(map[string]int, len(rows))
	for _, r := range rows {
		if !r.dead && r.pid > 0 {
			roots[r.session] = r.pid
		}
	}
	if len(roots) == 0 {
		return nil, true
	}
	out, err := d.psRun(ctx, d.psBin, processTableArgs...)
	if err != nil {
		d.counters.incr(counterAgentUnread)
		return nil, false
	}
	return agentsFromTable(parseProcessTable(string(out)), roots), true
}
