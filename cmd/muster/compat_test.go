package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/compat"
	"github.com/futurelastic/muster/internal/drivers/tmux"
)

// withChecker swaps the checker for the test and records what it was given.
type checkerCall struct {
	opts     tmux.CompatOptions
	deadline time.Time
	hadDL    bool
}

func withChecker(t *testing.T, rep compat.Report, err error) *checkerCall {
	t.Helper()
	call := &checkerCall{}
	old := runCompatFn
	runCompatFn = func(ctx context.Context, o tmux.CompatOptions) (compat.Report, error) {
		call.opts = o
		call.deadline, call.hadDL = ctx.Deadline()
		return rep, err
	}
	t.Cleanup(func() { runCompatFn = old })
	return call
}

func reportWith(checks ...compat.Result) compat.Report {
	r := compat.Report{
		Claude:     compat.Candidate{Path: "/opt/c/claude", Version: "1.2.3"},
		ColabFleet: compat.Build{Version: "v0", Commit: "abc"},
		Checks:     checks,
	}
	r.Finalize()
	return r
}

func run(t *testing.T, args ...string) (handled bool, code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	handled, code = runCompat(args, func(string) string { return "" }, &out, &errb)
	return handled, code, out.String(), errb.String()
}

func TestRunCompatIsOnlyForCompat(t *testing.T) {
	for _, args := range [][]string{nil, {"doctor"}, {"--claude", "/x"}, {"principal"}} {
		if handled, _, _, _ := run(t, args...); handled {
			t.Errorf("runCompat claimed %q", args)
		}
	}
}

// Flags are taken both as `--flag=value` (as doctor takes them) and as
// `--flag value` (as the usage line spells them).
func TestRunCompatParsesFlags(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	for name, args := range map[string][]string{
		"spaced": {"compat", "--claude", "/opt/c/claude", "--only", "F-LIMIT,H-RC", "--pack", "/tmp/p", "--timeout", "90s", "--json"},
		"equals": {"compat", "--claude=/opt/c/claude", "--only=F-LIMIT,H-RC", "--pack=/tmp/p", "--timeout=90s", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			call := withChecker(t, reportWith(compat.Result{ID: "F-LIMIT", Pass: true, Detail: "ok"}), nil)
			handled, code, _, stderr := run(t, args...)
			if !handled || code != 0 {
				t.Fatalf("handled=%v code=%d stderr=%q", handled, code, stderr)
			}
			if call.opts.Claude != "/opt/c/claude" || call.opts.PackDir != "/tmp/p" || !reflect.DeepEqual(call.opts.Only, []string{"F-LIMIT", "H-RC"}) {
				t.Errorf("options = %+v", call.opts)
			}
			if !call.hadDL {
				t.Fatal("the run has no deadline: --timeout was not applied")
			}
			if d := time.Until(call.deadline); d > 90*time.Second || d < 60*time.Second {
				t.Errorf("deadline in %s, want about 90s", d)
			}
		})
	}
}

func TestRunCompatDefaultTimeout(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	call := withChecker(t, reportWith(compat.Result{ID: "F-LIMIT", Pass: true}), nil)
	run(t, "compat", "--claude", "/x")
	if d := time.Until(call.deadline); !call.hadDL || d > compatDefaultTimeout || d < compatDefaultTimeout-time.Minute {
		t.Errorf("default deadline in %s, want about %s", d, compatDefaultTimeout)
	}
}

// A bad invocation exits 2, prints the usage, and never reaches the checker.
func TestRunCompatBadInvocations(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	cases := map[string][]string{
		"no --claude":               {"compat"},
		"--claude without a value":  {"compat", "--claude"},
		"empty --claude":            {"compat", "--claude="},
		"unknown argument":          {"compat", "--claude", "/x", "--frobnicate"},
		"a stray word":              {"compat", "/x"},
		"--only naming an unknown":  {"compat", "--claude", "/x", "--only", "NOT-A-CHECK"},
		"--only with a typo":        {"compat", "--claude", "/x", "--only", "F-LIMT"},
		"--only empty":              {"compat", "--claude", "/x", "--only="},
		"--only blank":              {"compat", "--claude", "/x", "--only", " "},
		"--only without a value":    {"compat", "--claude", "/x", "--only"},
		"bad --timeout":             {"compat", "--claude", "/x", "--timeout", "soon"},
		"zero --timeout":            {"compat", "--claude", "/x", "--timeout", "0s"},
		"negative --timeout":        {"compat", "--claude", "/x", "--timeout=-5m"},
		"--timeout without a value": {"compat", "--claude", "/x", "--timeout"},
		"--pack without a value":    {"compat", "--claude", "/x", "--pack"},
		"--json given a value":      {"compat", "--claude", "/x", "--json=yes"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			call := withChecker(t, reportWith(compat.Result{ID: "F-LIMIT", Pass: true}), nil)
			handled, code, stdout, stderr := run(t, args...)
			if !handled || code != 2 {
				t.Fatalf("handled=%v code=%d, want handled with exit 2", handled, code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing: a bad invocation produces no report", stdout)
			}
			if !strings.Contains(stderr, "usage: muster compat") {
				t.Errorf("stderr = %q, want the usage", stderr)
			}
			if call.opts.Claude != "" {
				t.Error("the checker ran on a bad invocation")
			}
		})
	}
}

func TestRunCompatHelp(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	withChecker(t, compat.Report{}, nil)
	handled, code, stdout, stderr := run(t, "compat", "--help")
	if !handled || code != 0 || !strings.Contains(stdout, "usage: muster compat") || stderr != "" {
		t.Errorf("handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout, stderr)
	}
}

// The exit code follows the report: 0 pass, 1 a must failed, 2 could not
// certify. The pinned mapping lives in internal/compat; this pins that the
// command uses it.
func TestRunCompatExitCodesFollowTheReport(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	must := func(pass, errored bool) compat.Result {
		return compat.Result{ID: "X-M", Gate: compat.GateMust, Pass: pass, Error: errored, Detail: "d"}
	}
	for name, tc := range map[string]struct {
		rep  compat.Report
		want int
	}{
		"pass":  {reportWith(must(true, false)), 0},
		"fail":  {reportWith(must(false, false)), 1},
		"error": {reportWith(must(false, true)), 2},
	} {
		t.Run(name, func(t *testing.T) {
			withChecker(t, tc.rep, nil)
			_, code, _, _ := run(t, "compat", "--claude", "/x")
			if code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
		})
	}
}

// --json prints ONLY the report on stdout, so it can be piped; progress and
// diagnostics go to stderr.
func TestRunCompatJSONIsPureAndValid(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	withChecker(t, reportWith(compat.Result{ID: "F-LIMIT", Pass: true, Detail: "ok"}), nil)
	_, code, stdout, _ := run(t, "compat", "--claude", "/x", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		Schema int  `json:"schema"`
		Pass   bool `json:"pass"`
		Claude struct {
			Path, Version string
		} `json:"claude"`
		Checks []struct {
			ID     string `json:"id"`
			Pass   bool   `json:"pass"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if doc.Schema != compat.Schema || !doc.Pass || doc.Claude.Version != "1.2.3" || len(doc.Checks) != 1 || doc.Checks[0].ID != "F-LIMIT" {
		t.Errorf("document = %+v", doc)
	}
}

func TestRunCompatTextReport(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	withChecker(t, reportWith(compat.Result{ID: "F-LIMIT", Pass: true, Detail: "ok"}), nil)
	_, code, stdout, _ := run(t, "compat", "--claude", "/x")
	if code != 0 || !strings.Contains(stdout, "F-LIMIT") || !strings.Contains(stdout, "PASS") {
		t.Errorf("code=%d stdout=%q", code, stdout)
	}
}

// A run that cannot be performed at all prints no report and exits 2. A
// report that arrives WITH an error (a pack that could not be written) is still
// shown — the verdict is never hidden — but the exit code says the run was not
// clean.
func TestRunCompatErrors(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	withChecker(t, compat.Report{}, errors.New("--claude \"/x\": no such file"))
	handled, code, stdout, stderr := run(t, "compat", "--claude", "/x")
	if !handled || code != 2 || stdout != "" || !strings.Contains(stderr, "no such file") {
		t.Errorf("handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout, stderr)
	}

	withChecker(t, reportWith(compat.Result{ID: "F-LIMIT", Pass: true, Detail: "ok"}), errors.New("--pack: disk full"))
	_, code, stdout, stderr = run(t, "compat", "--claude", "/x", "--json")
	if code != 2 || !strings.Contains(stdout, `"schema": 1`) || !strings.Contains(stderr, "disk full") {
		t.Errorf("report+error: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestRunCompatPassesTheBuild(t *testing.T) {
	skipOnWindows(t, "muster compat is refused on Windows (ADR 303), so its flag and report paths are unreachable there; the refusal is TestWindowsCompatRefuses")
	call := withChecker(t, reportWith(compat.Result{ID: "F-LIMIT", Pass: true}), nil)
	run(t, "compat", "--claude", "/x")
	// Under `go test` the VCS stamp is absent, so only its presence as a
	// structured value is asserted, not its contents.
	if call.opts.Getenv == nil || call.opts.Log == nil {
		t.Errorf("options = %+v, want the environment reader and the progress log wired", call.opts)
	}
}
