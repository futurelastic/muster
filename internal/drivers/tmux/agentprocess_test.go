package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #301: Session.agent is read from the session's own runtime process,
// whoever created it and however long ago.

// psTable answers the one process-table query the driver makes with fixed
// output, counting calls.
func psTable(out string, calls *int32) execFunc {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		for _, a := range args {
			if a == "-t" || a == "-p" {
				return nil, errors.New("psTable: only the whole-table query is answered")
			}
		}
		return []byte(out), nil
	}
}

func TestParseProcessTable(t *testing.T) {
	got := parseProcessTable("  100     1   100   100 claude --agent my-agent --model opus\n" +
		"garbage line\n" +
		"  x  1  1  1 nope\n" +
		"  200   100   200   100 node /srv/tool.js\n")
	if len(got) != 2 {
		t.Fatalf("parsed %d rows, want 2 (malformed lines skipped): %+v", len(got), got)
	}
	if got[0].pid != 100 || got[0].ppid != 1 || got[0].pgid != 100 || got[0].tpgid != 100 ||
		got[0].args != "claude --agent my-agent --model opus" {
		t.Errorf("row 0 = %+v", got[0])
	}
}

func TestAgentFromArgs(t *testing.T) {
	for _, tc := range []struct {
		name, args, want string
		ok               bool
	}{
		{"separate value", "claude --agent alpha --model opus", "alpha", true},
		{"equals form", "claude --agent=alpha --model opus", "alpha", true},
		{"after other flags", "claude --model opus --effort high --agent alpha", "alpha", true},
		{"none", "claude --model opus", "", false},
		{"flag-shaped value", "claude --agent --model opus", "", false},
		{"flag-shaped equals value", "claude --agent=--model", "", false},
		{"trailing flag, no value", "claude --agent", "", false},
		{"first token is the executable, not a flag", "--agent alpha", "", false},
		{"options end at --", "claude -- --agent alpha", "", false},
		{"similar flag is not it", "claude --agents alpha --agent-id z", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := agentFromArgs(tc.args)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("agentFromArgs(%q) = (%q, %v), want (%q, %v)", tc.args, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestAgentsFromTable(t *testing.T) {
	roots := map[string]int{"s": 100}
	for _, tc := range []struct {
		name  string
		table string
		want  string // "" = absent
	}{
		{"runtime is the pane's own process",
			"100 1 100 100 claude --agent alpha\n", "alpha"},
		{"started by hand: a shell at the root, the runtime a child in the foreground group",
			"100 1 100 500 -zsh\n500 100 500 500 claude --agent alpha --model opus\n", "alpha"},
		{"started without an agent",
			"100 1 100 100 claude --model opus\n", ""},
		{"the runtime exited and a shell is left in the foreground",
			"100 1 100 100 -zsh\n", ""},
		{"a background child carrying the flag is not the runtime",
			"100 1 100 100 claude --model opus\n600 100 600 100 node helper --agent other\n", ""},
		{"a shell in the foreground carrying the flag in its own argv is not the runtime",
			"100 1 100 100 zsh -c claude --agent alpha\n", ""},
		{"two different values at the shallowest level are a disagreement, not a pick",
			"100 1 100 100 wrapper\n200 100 100 100 claude --agent a\n201 100 100 100 claude --agent b\n", ""},
		{"the shallowest level that carries a value decides",
			"100 1 100 100 claude --agent outer\n200 100 100 100 claude --agent inner\n", "outer"},
		{"a level that agrees with itself resolves",
			"100 1 100 100 wrapper\n200 100 100 100 claude --agent a\n201 100 100 100 helper --agent a\n", "a"},
		{"a pane whose root is not in the table has none to report",
			"999 1 999 999 claude --agent alpha\n", ""},
		{"no controlling terminal (tpgid -1) is not a foreground group",
			"100 1 100 -1 claude --agent alpha\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := agentsFromTable(parseProcessTable(tc.table), roots)["s"]
			if tc.want == "" {
				if ok {
					t.Fatalf("agent = %q, want absent", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("agent = (%q, %v), want %q", got, ok, tc.want)
			}
		})
	}
}

// the process table the fake host presents: alpha runs --agent x (started by a
// launcher, no create record here), beta runs without one.
const agentHostTable = "100 1 100 100 claude --agent x --model opus\n" +
	"200 1 200 200 claude --model opus\n"

func agentTestDriver(f *fakeMux, ps execFunc, now func() time.Time) *Driver {
	if now == nil {
		now = func() time.Time { return time.Unix(1785760000, 0) }
	}
	return New("testbox",
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(now),
		withPSExec(ps),
		WithProcessAgent(),
	)
}

func listWithAgent(t *testing.T, d *Driver, filter driver.ListFilter) fleet.Collection[fleet.Session] {
	t.Helper()
	col, err := d.List(context.Background(), testCaller, filter)
	if err != nil {
		t.Fatal(err)
	}
	return col
}

func sessionNamed(items []fleet.Session, name string) *fleet.Session {
	for i := range items {
		if items[i].ID == name {
			return &items[i]
		}
	}
	return nil
}

// The done-when of #301: a session created outside this service with
// `--agent x` lists agent "x" and matches ?agent=x; one without omits it —
// and the answer needs no create record, so a service restart (a fresh
// driver) changes nothing.
func TestListReportsTheAgentOfASessionThisServiceDidNotCreate(t *testing.T) {
	for _, restart := range []string{"first driver", "after a service restart"} {
		t.Run(restart, func(t *testing.T) {
			f := twoSessions()
			d := agentTestDriver(f, psTable(agentHostTable, nil), nil)
			if restart != "first driver" {
				d = agentTestDriver(f, psTable(agentHostTable, nil), nil) // a new process: nothing remembered
			}
			col := listWithAgent(t, d, driver.ListFilter{})
			a, b := sessionNamed(col.Items(), "alpha💬"), sessionNamed(col.Items(), "beta")
			if a == nil || b == nil {
				t.Fatalf("sessions missing from %+v", col.Items())
			}
			if a.Agent != "x" {
				t.Errorf("alpha agent = %q, want x", a.Agent)
			}
			if b.Agent != "" {
				t.Errorf("beta agent = %q, want absent", b.Agent)
			}
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"agent"`) {
				t.Errorf("a session without an agent must omit the field, not empty it: %s", raw)
			}

			filtered := listWithAgent(t, d, driver.ListFilter{Agent: "x"})
			if len(filtered.Items()) != 1 || filtered.Items()[0].ID != "alpha💬" {
				t.Fatalf("?agent=x matched %+v, want only alpha", filtered.Items())
			}
			if none := listWithAgent(t, d, driver.ListFilter{Agent: "nobody"}); len(none.Items()) != 0 {
				t.Fatalf("?agent=nobody matched %+v, want none", none.Items())
			}
			if !col.Complete() {
				t.Error("a clean read must be complete")
			}
		})
	}
}

// Old enough that the create record is gone: the agent is still reported, now
// from the process alone.
func TestAgentOutlivesTheCreateRecord(t *testing.T) {
	now := time.Unix(1785760000, 0)
	f := twoSessions()
	d := agentTestDriver(f, psTable(agentHostTable, nil), func() time.Time { return now })
	d.noteCreateRecord("alpha💬", "/work/alpha", fleet.SessionSpec{Name: "alpha💬", Cwd: "/work/alpha", Agent: "x"})

	young := sessionNamed(listWithAgent(t, d, driver.ListFilter{}).Items(), "alpha💬")
	if young.Agent != "x" {
		t.Fatalf("young agent = %q, want x", young.Agent)
	}
	if young.Pins == nil || young.Pins.Agent == nil {
		t.Fatalf("a live create record should still describe the pin: %+v", young.Pins)
	}

	now = now.Add(createRecordRetention + time.Minute)
	old := sessionNamed(listWithAgent(t, d, driver.ListFilter{}).Items(), "alpha💬")
	if old.Agent != "x" {
		t.Fatalf("agent after the create record expired = %q, want x", old.Agent)
	}
	if old.Pins != nil {
		t.Errorf("an expired record must not describe a request: %+v", old.Pins)
	}
}

// A value read from the process resolves the pin as `observed`; the request
// is never echoed into Session.Agent.
func TestObservedAgentResolvesTheRequestedPin(t *testing.T) {
	for _, tc := range []struct {
		name, requested string
		wantHonoured    bool
	}{
		{"honoured", "x", true},
		{"substituted", "y", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := twoSessions()
			d := agentTestDriver(f, psTable(agentHostTable, nil), nil)
			d.noteCreateRecord("alpha💬", "/work/alpha", fleet.SessionSpec{Name: "alpha💬", Cwd: "/work/alpha", Agent: fleet.AgentId(tc.requested)})
			s := sessionNamed(listWithAgent(t, d, driver.ListFilter{}).Items(), "alpha💬")
			if s.Agent != "x" {
				t.Fatalf("Session.Agent = %q, want the observed x (never the request %q)", s.Agent, tc.requested)
			}
			p := s.Pins.Agent
			if p == nil || p.Honoured == nil || *p.Honoured != tc.wantHonoured ||
				p.Requested != tc.requested || p.Applied != "x" || p.Source != fleet.PinObserved {
				t.Fatalf("pin = %+v, want requested %q applied x honoured %v observed", p, tc.requested, tc.wantHonoured)
			}
			if _, err := json.Marshal(s); err != nil {
				t.Fatalf("the session must stay encodable: %v", err)
			}
		})
	}
}

// Nothing observed: the request stays an unresolved request, and is never
// promoted to Session.Agent.
func TestAnUnobservedRequestIsNotReportedAsTheAgent(t *testing.T) {
	f := twoSessions()
	d := agentTestDriver(f, psTable("200 1 200 200 claude --model opus\n", nil), nil)
	d.noteCreateRecord("beta", "/work/beta", fleet.SessionSpec{Name: "beta", Cwd: "/work/beta", Agent: "z"})
	s := sessionNamed(listWithAgent(t, d, driver.ListFilter{}).Items(), "beta")
	if s.Agent != "" {
		t.Fatalf("Session.Agent = %q, want absent (a request is not an observation)", s.Agent)
	}
	if s.Pins == nil || s.Pins.Agent == nil || s.Pins.Agent.Honoured != nil || s.Pins.Agent.Requested != "z" {
		t.Fatalf("pin = %+v, want the request, unresolved", s.Pins)
	}
}

// An unreadable process table is not "no agent": the listing says it is
// incomplete, so an agent filter's empty answer is not mistaken for a real one.
func TestAnUnreadableProcessTableDegradesTheListing(t *testing.T) {
	f := twoSessions()
	failing := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("ps: not found") }
	d := agentTestDriver(f, failing, nil)
	col := listWithAgent(t, d, driver.ListFilter{Agent: "x"})
	if col.Complete() {
		t.Fatal("a listing that could not read the process table must not claim to be complete")
	}
	srcs := col.Sources()
	if len(srcs) != 1 || srcs[0].Status != fleet.SourceDegraded || !strings.Contains(srcs[0].Error, "process table") {
		t.Fatalf("sources = %+v, want degraded naming the process table", srcs)
	}
	if got := d.Counters()[counterAgentUnread]; got != 1 {
		t.Errorf("%s = %d, want 1", counterAgentUnread, got)
	}
	// The sessions themselves are still listed when no filter hides them.
	if all := listWithAgent(t, d, driver.ListFilter{}); len(all.Items()) != 2 {
		t.Errorf("sessions = %d, want 2", len(all.Items()))
	}
}

// Off by default: no process-table query at all, and the capability says so.
func TestAgentReadingIsOffUnlessWired(t *testing.T) {
	var calls int32
	f := twoSessions()
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		withPSExec(psTable(agentHostTable, &calls)))
	for _, s := range listWithAgent(t, d, driver.ListFilter{}).Items() {
		if s.Agent != "" {
			t.Errorf("%s agent = %q, want absent when the option is off", s.ID, s.Agent)
		}
	}
	if calls != 0 {
		t.Errorf("process table queried %d times with the option off", calls)
	}
	if d.Capabilities().ReportsAgent {
		t.Error("reportsAgent must be false when the driver cannot read it")
	}
	if !agentTestDriver(f, psTable("", nil), nil).Capabilities().ReportsAgent {
		t.Error("reportsAgent must be true when wired")
	}
}

// One process-table query per listing, however many sessions.
func TestAgentReadIsOneQueryPerListing(t *testing.T) {
	f := twoSessions()
	for i := 0; i < 40; i++ {
		id := "%" + intToStr(100+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "s" + intToStr(i), paneID: id, cwd: "/w", pid: 1000 + i, created: 1785600002,
		})
		f.captures[id] = idleFixtureFor("s" + intToStr(i))
	}
	var calls int32
	d := agentTestDriver(f, psTable(agentHostTable, &calls), nil)
	listWithAgent(t, d, driver.ListFilter{})
	if calls != 1 {
		t.Fatalf("process table queried %d times for %d sessions, want 1", calls, len(f.sessions))
	}
}
