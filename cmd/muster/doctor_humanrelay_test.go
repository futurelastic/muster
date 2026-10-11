package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// muster #189: principals.human-relay names the configuration gap #184
// leaves behind — an inbox index, a principal table, and nobody holding
// human-relay. Every case here is a shape the row must answer differently.

func humanRelayFixture(t *testing.T) (dir, bin, index string) {
	t.Helper()
	dir = t.TempDir()
	bin = executable(t, dir)
	index = filepath.Join(dir, "index")
	if err := os.Mkdir(index, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, bin, index
}

func TestDoctorHumanRelayRow(t *testing.T) {
	dir, bin, index := humanRelayFixture(t)
	notADir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	agents := []testPrincipal{
		{Name: "sup", Token: "a", Grants: grants(supervisorList + ",relay,label")},
		{Name: "viewer", Token: "b", Grants: grants("read")},
	}
	withHuman := append([]testPrincipal{
		{Name: "operator-ui", Token: "h", Grants: grants("read,send,human-relay")},
	}, agents...)

	cases := []struct {
		name       string
		principals []testPrincipal // nil means no principal table: single-token mode
		vars       map[string]string
		want       rowStatus
		summary    string // must appear in the summary
	}{
		{"index set, table, nobody holds it", agents,
			map[string]string{"FLEET_INBOX_INDEX": index}, statusWarn, "no principal holds human-relay"},
		{"a full supervisor is not a human relay", []testPrincipal{
			{Name: "sup", Token: "a", Grants: grants(supervisorList + ",relay,label")},
		}, map[string]string{"FLEET_INBOX_INDEX": index}, statusWarn, "no principal holds human-relay"},
		{"one principal holds it", withHuman,
			map[string]string{"FLEET_INBOX_INDEX": index}, statusPass, `"operator-ui"`},
		{"the index path being wrong is inbox.index's row, not this one", agents,
			map[string]string{"FLEET_INBOX_INDEX": notADir}, statusWarn, "no principal holds human-relay"},
		{"index unset — the inbox route is off", agents,
			map[string]string{}, statusSkip, "FLEET_INBOX_INDEX unset"},
		{"stub runtime has no inbox path", agents,
			map[string]string{"FLEET_INBOX_INDEX": index, "FLEET_RUNTIME": "stub"}, statusSkip, "no inbox delivery path"},
		// #196 (ruled on #195): this case used to skip, because there is no grant
		// to hold in single-token mode. It now fails — the inbox route needs a
		// principal table, so the service refuses to start with an index and none.
		{"single-token mode with an index is refused", nil,
			map[string]string{"FLEET_INBOX_INDEX": index, "FLEET_TOKEN": "t"}, statusFail, "no principal table is configured"},
		{"single-token mode without an index is untouched — no inbox route to divert into", nil,
			map[string]string{"FLEET_TOKEN": "t"}, statusSkip, "FLEET_INBOX_INDEX unset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{"FLEET_MACHINE": "m1", "FLEET_TMUX_BIN": bin}
			for k, v := range tc.vars {
				vars[k] = v
			}
			if tc.principals != nil {
				vars["FLEET_CONFIG"] = writeTestConfig(t, t.TempDir(), tc.principals, nil)
			}
			row := rowByID(t, runChecks(context.Background(), testDoctorEnv(vars)), "principals.human-relay")
			if row.Status != tc.want || !strings.Contains(row.Summary, tc.summary) {
				t.Fatalf("got %+v, want %s mentioning %q", row, tc.want, tc.summary)
			}
			if len(row.Refs) == 0 || row.Refs[0] != 184 {
				t.Errorf("row must cite #184, the change that made the grant matter: %+v", row.Refs)
			}
		})
	}
}

// A config that does not load decides nothing here; config.load already fails.
func TestDoctorHumanRelayUndecidableWhenConfigFails(t *testing.T) {
	_, bin, index := humanRelayFixture(t)
	missing := filepath.Join(t.TempDir(), "no-such-config.json")
	row := rowByID(t, runChecks(context.Background(), testDoctorEnv(map[string]string{
		"FLEET_CONFIG": missing, "FLEET_INBOX_INDEX": index, "FLEET_TMUX_BIN": bin,
	})), "principals.human-relay")
	if row.Status != statusSkip || !strings.Contains(row.Summary, "config.load") {
		t.Fatalf("got %+v, want a skip naming config.load", row)
	}
}

// --principal names the SUPERVISING client. human-relay is outside the
// supervisor set on purpose, so naming that client must not change what this
// row says: holder exists elsewhere → still pass; nobody holds it → still warn,
// even when the named principal is a full supervisor.
func TestDoctorHumanRelayIgnoresPrincipalFlag(t *testing.T) {
	_, bin, index := humanRelayFixture(t)
	for _, tc := range []struct {
		name string
		ps   []testPrincipal
		want rowStatus
	}{
		{"holder is not the named supervisor", []testPrincipal{
			{Name: "sup", Token: "a", Grants: grants(supervisorList)},
			{Name: "operator-ui", Token: "h", Grants: grants("read,send,human-relay")},
		}, statusPass},
		{"nobody holds it, named principal is a full supervisor", []testPrincipal{
			{Name: "sup", Token: "a", Grants: grants(supervisorList)},
		}, statusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{
				"FLEET_MACHINE": "m1", "FLEET_TMUX_BIN": bin, "FLEET_INBOX_INDEX": index,
				"FLEET_CONFIG": writeTestConfig(t, t.TempDir(), tc.ps, nil),
			}
			env := testDoctorEnv(vars)
			env.Principal = "sup"
			if row := rowByID(t, runChecks(context.Background(), env), "principals.human-relay"); row.Status != tc.want {
				t.Fatalf("got %+v, want %s", row, tc.want)
			}
		})
	}
}

// The warning names the remedy and its order, and the escape hatch, because a
// reader who sees only "warn" has to go and find out what to do about it.
func TestDoctorHumanRelayWarningNamesTheOrderAndTheEscape(t *testing.T) {
	_, bin, index := humanRelayFixture(t)
	cfg := writeTestConfig(t, t.TempDir(), []testPrincipal{{Name: "sup", Token: "a", Grants: grants(supervisorList)}}, nil)
	row := rowByID(t, runChecks(context.Background(), testDoctorEnv(map[string]string{
		"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_INBOX_INDEX": index, "FLEET_TMUX_BIN": bin,
	})), "principals.human-relay")
	for _, want := range []string{"BEFORE", "mode_class", "--skip=principals.human-relay", "Turning the inbox route on"} {
		if !strings.Contains(row.Detail, want) {
			t.Errorf("detail must mention %q: %s", want, row.Detail)
		}
	}
}

// A warning is never a failure (ADR 160), and --skip silences it where it is
// deliberate — a fleet whose callers are all agents.
func TestDoctorHumanRelayWarnsWithoutFailingAndSkips(t *testing.T) {
	skipOnWindows(t, "asserts the whole run's exit code, and the runtime row in it judges a POSIX execute bit (#312)")
	dir, bin, index := humanRelayFixture(t)
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := writeTestConfig(t, dir, []testPrincipal{{Name: "sup", Token: "a", Grants: grants(supervisorList)}}, nil)
	vars := map[string]string{
		"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_STATE_DIR": state,
		"FLEET_INBOX_INDEX": index, "FLEET_TMUX_BIN": bin, "FLEET_ADDR": "127.0.0.1:9000",
	}
	statusOf := func(out string) rowStatus {
		t.Helper()
		var doc struct {
			Rows []doctorRow `json:"rows"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("--json did not parse: %v\n%s", err, out)
		}
		return rowByID(t, doc.Rows, "principals.human-relay").Status
	}

	code, out, _ := runDoctorTest(vars, "--json", "--offline")
	if code != 0 {
		t.Fatalf("a human-relay warning must not fail the run: exit %d\n%s", code, out)
	}
	if got := statusOf(out); got != statusWarn {
		t.Fatalf("row = %s, want warn", got)
	}

	code, out, errOut := runDoctorTest(vars, "--json", "--offline", "--skip=principals.human-relay")
	if code != 0 || errOut != "" {
		t.Fatalf("--skip=principals.human-relay must be a known row: exit %d, stderr %q", code, errOut)
	}
	if got := statusOf(out); got != statusSkip {
		t.Fatalf("row = %s, want skip", got)
	}
}

// #196: a table-less machine that turns the inbox route on fails the run, points
// at the table AND at the other way out, and hiding the row does not un-refuse
// the service — the row says so.
func TestDoctorHumanRelayFailsWithoutATableAndPointsAtTheFix(t *testing.T) {
	skipOnWindows(t, "asserts the whole run's exit code, and the runtime row in it judges a POSIX execute bit (#312)")
	dir, bin, index := humanRelayFixture(t)
	vars := map[string]string{
		"FLEET_MACHINE": "m1", "FLEET_TOKEN": "t", "FLEET_INBOX_INDEX": index, "FLEET_TMUX_BIN": bin,
		"FLEET_STATE_DIR": dir, "FLEET_ADDR": "127.0.0.1:9000",
	}
	row := rowByID(t, runChecks(context.Background(), testDoctorEnv(vars)), "principals.human-relay")
	if row.Status != statusFail {
		t.Fatalf("row = %+v, want fail", row)
	}
	for _, want := range []string{"refuses to start", "docs/install.md step 5", "human-relay", "unset FLEET_INBOX_INDEX", "--skip=principals.human-relay"} {
		if !strings.Contains(row.Summary+" "+row.Detail, want) {
			t.Errorf("the row must mention %q: %+v", want, row)
		}
	}
	hasRef := func(n int) bool {
		for _, r := range row.Refs {
			if r == n {
				return true
			}
		}
		return false
	}
	if !hasRef(195) || !hasRef(196) {
		t.Errorf("row must cite the ruling and the change (#195, #196): %+v", row.Refs)
	}

	// A failing row stops the run — that is what fail means (ADR 160). The
	// control is the same environment with only this row skipped: it must pass,
	// or the exit code above was some other row's doing and proves nothing.
	if code, out, _ := runDoctorTest(vars, "--offline"); code != 1 {
		t.Fatalf("exit = %d, want 1: the service would refuse to start\n%s", code, out)
	}
	if code, out, _ := runDoctorTest(vars, "--offline", "--skip=principals.human-relay"); code != 0 {
		t.Fatalf("with only the human-relay row skipped exit = %d, want 0 — another row is failing this fixture\n%s", code, out)
	}
}
