package tmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/compat"
	"github.com/futurelastic/muster/internal/trustseed"
)

// fakeCandidate writes an executable stand-in for the runtime: a script that
// answers --version and whose body carries the given text, which is what the
// static checks scan. No real runtime, no network, no tokens.
func fakeCandidate(t *testing.T, version, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "candidate")
	script := "#!/bin/sh\n# " + body + "\ncase \"$1\" in --version) echo \"" + version + " (Synthetic Runtime)\" ;; esac\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The temp dir may sit behind a symlink; tests compare against the
	// resolved form.
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func allMarkersText() string {
	return strings.Join(staticMarkerList(), " ")
}

func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

// The static marker vocabulary is only worth having if it is exactly what the
// classifier matches. This runs every marker through the function that relies
// on it: change the classifier's wording and this fails until the vocabulary
// follows.
func TestStaticMarkersAreTiedToTheClassifier(t *testing.T) {
	tied := map[string]func() bool{
		// usageLimit
		"hit your":       func() bool { _, ok := usageLimit(newScreen("You've hit your session limit\n")); return ok },
		"/usage-credits": func() bool { _, ok := usageLimit(newScreen("run /usage-credits to finish\n")); return ok },
		// resetHintIn
		"resets ":          func() bool { h, ok := resetHintIn("limit · resets 9pm"); return ok && h == "9pm" },
		"available again ": func() bool { h, ok := resetHintIn("available again at 9pm"); return ok && h == "at 9pm" },
		// lastTurnFailed / retryableWords
		"api error": func() bool { _, ok := lastTurnFailed(newScreen("API Error: 529 overloaded\n")); return ok },
		"try again ": func() bool {
			te, ok := lastTurnFailed(newScreen("API Error: 529, please try again later\n"))
			return ok && te.Retryable
		},
		"temporary": func() bool {
			te, ok := lastTurnFailed(newScreen("API Error: temporary failure\n"))
			return ok && te.Retryable
		},
		// acceptanceScreen: the two option labels the classifier recognises.
		"yes, i accept": func() bool {
			return acceptanceScreen(compatBypassSpec(), &fleet.SessionPrompt{Options: []string{"No, exit", "Yes, I accept"}})
		},
		"no, exit": func() bool {
			return acceptanceScreen(compatBypassSpec(), &fleet.SessionPrompt{Options: []string{"No, exit", "Yes, I accept"}}) &&
				!acceptanceScreen(compatBypassSpec(), &fleet.SessionPrompt{Options: []string{"Yes, I accept", "Yes, I accept"}})
		},
		// classifyControlRecordLine (#270): each marker is the wording of an
		// entry the reader keys on, built as the runtime writes it.
		"/remote-control is active": func() bool {
			ev, ok := classifyControlRecordLine(`{"type":"system","subtype":"bridge_status","timestamp":"2026-10-07T20:00:00Z","content":"/remote-control is active · x"}`)
			return ok && ev.kind == controlRecordActive
		},
		"remote control disconnected": func() bool {
			ev, ok := classifyControlRecordLine(`{"type":"system","subtype":"informational","timestamp":"2026-10-07T20:00:00Z","content":"Remote Control disconnected — x"}`)
			return ok && ev.kind == controlRecordNotice
		},
		"bridge_status": func() bool {
			return controlBridgeSubtype == "bridge_status"
		},
		// permissionModeOf: the three indicator wordings that are literal strings in
		// the candidate. (The other two are composed at run time and are read off
		// live sessions by F-MODE instead — see compatStaticMarkers.)
		"accept edits on": func() bool {
			return permissionModeOf(newScreen(paneWithIndicator("  ⏵⏵ accept edits on"))) == fleet.PermissionModeAcceptEdits
		},
		"plan mode on": func() bool {
			return permissionModeOf(newScreen(paneWithIndicator("  ⏸ plan mode on"))) == fleet.PermissionModePlan
		},
		"auto mode on": func() bool {
			return permissionModeOf(newScreen(paneWithIndicator("  ⏵⏵ auto mode on"))) == fleet.PermissionModeAuto
		},
		// feedback card (muster#217): each marker is a substring of the
		// wording the recogniser matches whole, and that wording still reads.
		"send without reviewing": func() bool {
			return feedbackWordingReads(feedbackConfirmText, "send without reviewing", feedbackConfirm)
		},
		"(full draft + env, no transcript)": func() bool {
			return feedbackWordingReads(feedbackConfirmText, "(full draft + env, no transcript)", feedbackConfirm)
		},
		"review & retry": func() bool {
			return feedbackWordingReads(feedbackErrorHead+" (x). "+feedbackErrorTail, "review & retry", feedbackError)
		},
		"couldn't send feedback": func() bool {
			return feedbackWordingReads(feedbackErrorHead+" (x). "+feedbackErrorTail, "couldn't send feedback", feedbackError)
		},
		" more queued": func() bool {
			return feedbackWordingReads(feedbackFooter+" · +3 more queued", " more queued", feedbackKeys)
		},
		"turn off claude-drafted feedback?": func() bool {
			return strings.Contains(strings.ToLower(feedbackQuestionRow), "turn off claude-drafted feedback?") &&
				liveFeedbackQuestion(newScreen(feedbackQuestionScreen()))
		},
		"feedback drafts": func() bool {
			_, ok := liveFeedbackPanel(newScreen(feedbackPanelScreen(false)))
			return ok && strings.Contains(strings.ToLower(feedbackPanelTitle), "feedback drafts")
		},
		// controlStateIn
		"/rc active": func() bool {
			st, ok := controlStateIn("/rc active")
			return ok && st == fleet.ControlChannelActive
		},
		"/rc failed": func() bool {
			st, ok := controlStateIn("/rc failed")
			return ok && st == fleet.ControlChannelFailed
		},
		"/rc reconnecting": func() bool {
			st, ok := controlStateIn("/rc reconnecting")
			return ok && st == fleet.ControlChannelReconnecting
		},
		"/rc connecting": func() bool {
			st, ok := controlStateIn("/rc connecting")
			return ok && st == fleet.ControlChannelConnecting
		},
	}
	for _, m := range staticMarkerList() {
		fn, ok := tied[m]
		if !ok {
			t.Errorf("marker %q is required by a check but tied to no classifier function here", m)
			continue
		}
		if m != strings.ToLower(m) {
			t.Errorf("marker %q must be lower-case: the scan folds case", m)
		}
		if !fn() {
			t.Errorf("marker %q: the classifier function that relies on it no longer accepts this wording", m)
		}
	}
	for m := range tied {
		found := false
		for _, x := range staticMarkerList() {
			if x == m {
				found = true
			}
		}
		if !found {
			t.Errorf("tied marker %q is in no check's vocabulary", m)
		}
	}
}

func TestStaticMarkerListSortedUnique(t *testing.T) {
	l := staticMarkerList()
	if !sort.StringsAreSorted(l) {
		t.Errorf("not sorted: %v", l)
	}
	seen := map[string]bool{}
	for _, m := range l {
		if seen[m] {
			t.Errorf("duplicate %q", m)
		}
		seen[m] = true
	}
}

func TestScanMarkers(t *testing.T) {
	dir := t.TempDir()
	// 9 MiB of filler with a marker that straddles the first 4 MiB chunk
	// boundary, an upper-case one in the middle, and one at the very end.
	const chunk = 4 << 20
	data := bytes.Repeat([]byte{'x'}, 9<<20)
	copy(data[chunk-3:], "hit your")
	copy(data[6<<20:], "/RC Active")
	copy(data[len(data)-len("api error"):], "api error")
	p := filepath.Join(dir, "big")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := scanMarkers(context.Background(), p, []string{"hit your", "/rc active", "api error", "never there"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"hit your", "/rc active", "api error"} {
		if !got[m] {
			t.Errorf("%q not found (boundary, case-fold or end-of-file handling is wrong)", m)
		}
	}
	if got["never there"] {
		t.Error("a marker that is absent was reported present")
	}
}

func TestScanMarkersHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := fakeCandidate(t, "1.0.0", "x")
	if _, err := scanMarkers(ctx, p, []string{"x"}); err == nil {
		t.Error("a cancelled scan must return an error, not a verdict")
	}
}

func TestResolveCandidate(t *testing.T) {
	good := fakeCandidate(t, "1.0.0", "x")
	if got, err := resolveCandidate(good); err != nil || got != good {
		t.Errorf("resolveCandidate(%q) = %q, %v", good, got, err)
	}
	// A symlink resolves to its target, so a link swapped later cannot change
	// the binary under test.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveCandidate(link); err != nil || got != good {
		t.Errorf("symlink resolved to %q, %v; want %q", got, err, good)
	}
	notExec := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"empty":          "",
		"relative":       "claude",
		"dot-relative":   "./claude",
		"missing":        filepath.Join(t.TempDir(), "nope"),
		"a directory":    t.TempDir(),
		"not executable": notExec,
	} {
		if _, err := resolveCandidate(in); err == nil {
			t.Errorf("%s (%q): want a usage error", name, in)
		}
	}
}

func TestParseVersionLine(t *testing.T) {
	cases := map[string]string{
		"2.1.281 (Claude Code)\n":      "2.1.281",
		"\n  9.9.9 (Synthetic)  \nx\n": "9.9.9",
		"1.2.3":                        "1.2.3",
		"":                             "",
		"\n\n":                         "",
	}
	for in, want := range cases {
		if got := parseVersionLine(in); got != want {
			t.Errorf("parseVersionLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// The candidate's environment is the whole of it. A variable the caller
// happened to have set must never reach the runtime under test.
func TestCompatBaseEnvIsMinimal(t *testing.T) {
	got := compatBaseEnv(env("HOME", "/h", "USER", "u", "SHELL", "/bin/zsh", "SECRET_TOKEN", "leak", "CLAUDE_CODE_SESSION_KIND", "bg", "TMUX", "/x,1,0"))
	joined := strings.Join(got, "\n")
	for _, must := range []string{"HOME=/h", "USER=u", "LOGNAME=u", "SHELL=/bin/zsh", "TERM=xterm-256color", "DISABLE_UPDATES=1", "DISABLE_AUTOUPDATER=1", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"} {
		if !strings.Contains(joined, must) {
			t.Errorf("environment lacks %s\n%s", must, joined)
		}
	}
	for _, mustNot := range []string{"SECRET_TOKEN", "CLAUDE_CODE_SESSION_KIND", "TMUX="} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("environment leaks %s\n%s", mustNot, joined)
		}
	}
}

func TestCandidateArchOfARealExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	got := candidateArch(exe)
	want := map[string]string{"amd64": "x86_64", "arm64": "arm64", "386": "i386", "arm": "arm"}[runtime.GOARCH]
	if want != "" && got != want {
		t.Errorf("candidateArch(test binary) = %q, want %q", got, want)
	}
	if got := candidateArch(fakeCandidate(t, "1.0.0", "x")); got != "unknown" {
		t.Errorf("a script's arch = %q, want unknown", got)
	}
}

func TestDescribeCandidate(t *testing.T) {
	p := fakeCandidate(t, "9.9.9", "x")
	c, err := describeCandidate(context.Background(), p, p, env("HOME", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "9.9.9" || c.Path != p || c.Resolved != p || len(c.Sha256) != 64 || c.Arch == "" {
		t.Errorf("candidate = %+v", c)
	}
	// A candidate that cannot say what it is still reports what was learned.
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err = describeCandidate(context.Background(), broken, broken, env("HOME", t.TempDir()))
	if err == nil {
		t.Fatal("a candidate whose --version fails must not be identified")
	}
	if c.Sha256 == "" {
		t.Error("the digest should survive a failed --version")
	}
}

// End to end without a runtime: the static checks pass on a candidate that
// carries the wording, fail (as warnings) on one that does not, and every
// detail names the driver code that relies on it.
func TestRunCompatStaticChecks(t *testing.T) {
	ctx := context.Background()
	opts := func(claude string, only ...string) CompatOptions {
		return CompatOptions{Claude: claude, Only: only, Getenv: env("HOME", t.TempDir()), Build: compat.Build{Version: "v0", Commit: "abc"}}
	}

	rep, err := RunCompat(ctx, opts(fakeCandidate(t, "9.9.9", allMarkersText()), "F-LIMIT", "F-APIERR", "H-RC"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Schema != compat.Schema || rep.Claude.Version != "9.9.9" || rep.ColabFleet.Commit != "abc" || len(rep.Only) != 3 {
		t.Errorf("report header = %+v", rep)
	}
	if !rep.Pass || rep.ExitCode() != 0 || len(rep.Checks) != 3 {
		t.Fatalf("report = %+v, want 3 passing checks", rep)
	}
	for _, c := range rep.Checks {
		if !c.Pass || !strings.Contains(c.Detail, "relied on by") {
			t.Errorf("%s = %+v", c.ID, c)
		}
	}

	rep, err = RunCompat(ctx, opts(fakeCandidate(t, "9.9.9", "nothing relevant here"), "F-LIMIT"))
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Checks[0]
	if c.Pass || c.Error || !strings.Contains(c.Detail, `"hit your"`) {
		t.Errorf("F-LIMIT on a candidate without the wording = %+v, want a behavioural failure naming what is missing", c)
	}
	// A warn check failing never changes the verdict.
	if !rep.Pass || rep.ExitCode() != 0 {
		t.Errorf("a failing warn check changed the verdict: %+v", rep)
	}
}

// Nothing in the ungated tests may start a real multiplexer. The static checks
// need none; every other check does, so an ungated test that selected one would
// quietly launch a private server (and wait for it) on whatever machine runs the
// suite. Live checks belong behind FLEET_TMUX_INTEGRATION.
func TestStaticCheckSetNeedsNoMultiplexer(t *testing.T) {
	suite := newCompatSuite(&compatHarness{})
	static := map[string]bool{"F-LIMIT": true, "F-APIERR": true, "H-RC": true, "H-RCREC": true}
	probesOf := map[string][]string{}
	for _, c := range suite.Checks {
		probesOf[c.ID] = c.Probes
	}
	for id := range static {
		for _, p := range probesOf[id] {
			if p != "static.markers" {
				t.Errorf("%s reads probe %q; the static checks must read only the marker scan", id, p)
			}
		}
	}
}

func TestRunCompatRefusesBadInvocations(t *testing.T) {
	ctx := context.Background()
	good := fakeCandidate(t, "1.0.0", "x")
	base := func() CompatOptions { return CompatOptions{Claude: good, Getenv: env("HOME", t.TempDir())} }
	cases := map[string]func(o *CompatOptions){
		"relative path":         func(o *CompatOptions) { o.Claude = "claude" },
		"missing path":          func(o *CompatOptions) { o.Claude = filepath.Join(t.TempDir(), "nope") },
		"unknown --only":        func(o *CompatOptions) { o.Only = []string{"NOT-A-CHECK"} },
		"a typo in --only":      func(o *CompatOptions) { o.Only = []string{"F-LIMT"} },
		"--only naming nothing": func(o *CompatOptions) { o.Only = []string{" ", ""} },
	}
	for name, mutate := range cases {
		o := base()
		mutate(&o)
		if rep, err := RunCompat(ctx, o); err == nil {
			t.Errorf("%s: want an error, got report %+v", name, rep)
		} else if rep.Schema != 0 {
			t.Errorf("%s: a bad invocation must produce no report", name)
		}
	}
}

// A candidate that cannot say what it is is a report of errors — retryable —
// not a disappearing act and not a verdict.
func TestRunCompatUnidentifiableCandidateIsAllErrors(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := RunCompat(context.Background(), CompatOptions{Claude: broken, Getenv: env("HOME", t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("no checks in the report")
	}
	for _, c := range rep.Checks {
		if !c.Error || c.Pass || !strings.Contains(c.Detail, "could not identify the candidate") {
			t.Errorf("%s = %+v, want error", c.ID, c)
		}
	}
}

func TestRunCompatWritesThePack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pack")
	rep, err := RunCompat(context.Background(), CompatOptions{
		Claude: fakeCandidate(t, "9.9.9", allMarkersText()), PackDir: dir, Only: []string{"H-RC"}, Getenv: env("HOME", t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got compat.Report
	if err := json.Unmarshal(b, &got); err != nil || got.Claude.Version != rep.Claude.Version || len(got.Checks) != 1 {
		t.Errorf("pack report = %+v, %v", got, err)
	}
	if fi, _ := os.Stat(dir); fi == nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("pack dir mode = %v, want 0700", fi)
	}
	// Reusing a directory that already has contents is refused: two runs'
	// evidence in one pack describes no candidate.
	if _, err := RunCompat(context.Background(), CompatOptions{Claude: fakeCandidate(t, "9.9.9", "x"), PackDir: dir, Getenv: env("HOME", t.TempDir())}); err == nil {
		t.Error("a non-empty --pack directory must be refused")
	}
}

// Every catalogue entry names driver code that depends on the behaviour it
// asserts. If that code is renamed or removed, the entry points at nothing and
// a failing report would tell its reader to look somewhere that no longer
// exists — so each "<file>#<identifier>" is resolved against the source.
func TestReliedOnEntriesResolveToSource(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, spec := range compat.Catalogue() {
		for _, entry := range spec.ReliedOn {
			file, ident, ok := strings.Cut(entry, "#")
			if !ok || file == "" || ident == "" {
				t.Errorf("%s: %q is not <file>#<identifier>", spec.ID, entry)
				continue
			}
			if !topLevelIdentifiers(t, filepath.Join(root, file))[ident] {
				t.Errorf("%s: %s has no top-level %q — the code this check protects was renamed or removed; update the catalogue", spec.ID, file, ident)
			}
		}
	}
}

// topLevelIdentifiers lists the package-level names a Go file declares:
// functions (not methods), types, variables and constants.
func topLevelIdentifiers(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				out[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					out[sp.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						out[n.Name] = true
					}
				}
			}
		}
	}
	return out
}

// The suite implements exactly the catalogue: a catalogued check with no
// evaluator would be silently absent from every report, and an evaluator with no
// catalogue row could not be gated or documented.
func TestCompatSuiteCoversTheCatalogue(t *testing.T) {
	suite := newCompatSuite(&compatHarness{})
	if err := suite.Validate(); err != nil {
		t.Fatalf("the suite is inconsistent: %v", err)
	}
	have := map[string]bool{}
	for _, c := range suite.Checks {
		have[c.ID] = true
	}
	for _, s := range compat.Catalogue() {
		if !have[s.ID] {
			t.Errorf("%s is in the catalogue and has no check in the suite", s.ID)
		}
		delete(have, s.ID)
	}
	for id := range have {
		t.Errorf("the suite has a check %q that is not in the catalogue", id)
	}
}

// The wrapper is the one door to the multiplexer, so its text is checked as
// carefully as code: every value quoted, nothing expanded, the environment
// cleared, and the socket named exactly once.
func TestMuxScript(t *testing.T) {
	s := muxScript([]string{"HOME=/h o'me", "DISABLE_UPDATES=1"}, "/usr/bin/tmux", "/tmp/cfc-x/m", "cfc-x")
	for _, want := range []string{
		"#!/bin/sh\nexec /usr/bin/env -i ",
		`'HOME=/h o'\''me'`, // an embedded quote survives
		"'DISABLE_UPDATES=1'",
		"'TMUX_TMPDIR=/tmp/cfc-x/m'",
		"'/usr/bin/tmux' -L 'cfc-x' -f /dev/null \"$@\"",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wrapper lacks %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "-L") != 1 || strings.Contains(s, "$TMUX") || strings.Contains(s, "-S ") {
		t.Errorf("the wrapper must name exactly one socket, by label, and inherit nothing:\n%s", s)
	}
}

// A candidate is launched by the resolved path, never the bare word the driver
// would normally use; its name reaches it with -n even though remote control is
// off; and a session's extra arguments are added to that session alone.
func TestCompatCommandBuilder(t *testing.T) {
	w := &compatWorld{resolved: "/opt/cand/claude", extraArgs: map[string][]string{"cfc-x-c": {"--settings", `{"k":false}`}}}
	build := w.commandBuilder()
	off := false
	argv := build(fleet.SessionSpec{Name: "cfc-x-a", Model: "haiku", Effort: "low", RemoteControl: &off}, "")
	if argv[0] != "/opt/cand/claude" {
		t.Errorf("argv[0] = %q, want the resolved candidate", argv[0])
	}
	joined := " " + strings.Join(argv, " ") + " "
	for _, want := range []string{" -n cfc-x-a ", " --model haiku ", " --effort low "} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q: %v", want, argv)
		}
	}
	if strings.Contains(joined, "--remote-control") {
		t.Errorf("remote control must stay off: %v", argv)
	}
	if strings.Contains(joined, "--settings") {
		t.Errorf("another session's extra arguments leaked: %v", argv)
	}
	argv = build(fleet.SessionSpec{Name: "cfc-x-c", RemoteControl: &off}, "")
	if !strings.Contains(strings.Join(argv, " "), `--settings {"k":false}`) {
		t.Errorf("the session's own extra arguments are missing: %v", argv)
	}
	// Bypass mode reaches the candidate only when asked for.
	argv = build(fleet.SessionSpec{Name: "cfc-x-b", RemoteControl: &off, PermissionMode: fleet.PermissionModeBypass}, "")
	if !containsArg(argv, "--dangerously-skip-permissions") {
		t.Errorf("bypass was requested: %v", argv)
	}
}

func TestCompatDirsAndStore(t *testing.T) {
	d := compatDirs("/tmp/cfc-abc")
	if !strings.HasPrefix(d["a"], "/tmp/cfc-abc/t/") || !strings.HasPrefix(d["b"], "/tmp/cfc-abc/t/") {
		t.Errorf("trusted directories must sit under the trust root: %v", d)
	}
	if strings.HasPrefix(d["u"], "/tmp/cfc-abc/t/") {
		t.Errorf("the untrusted directory must sit OUTSIDE the trust root: %v", d)
	}
	// #212: the imports directory is trusted by the harness's own hand, never by
	// the driver's seeder — which would write the imports answer too and so could
	// never leave the question standing. So it must sit where that seeder refuses.
	if strings.HasPrefix(d["i"], "/tmp/cfc-abc/t/") || d["i"] == d["u"] {
		t.Errorf("the imports directory must sit OUTSIDE the trust root, and apart from u: %v", d)
	}
	// The awkward directory exists to break a naive encoder.
	for _, ch := range []string{".", "_", " ", "ñ"} {
		if !strings.Contains(d["a"], ch) {
			t.Errorf("directory a lacks %q: %s", ch, d["a"])
		}
	}
	// Every directory maps to a transcript directory that carries the nonce, so
	// a deletion guarded by it can never reach a real project's.
	for role, dir := range d {
		if !containsNonce(recordDirFor(dir), "abc") {
			t.Errorf("%s: %q lost the nonce in its encoded form", role, recordDirFor(dir))
		}
	}

	s, err := compatStoreFrom(env("HOME", "/home/x"))
	if err != nil || s.projects != "/home/x/.claude/projects" || s.sessions != "/home/x/.claude/sessions" || s.trustState != "/home/x/.claude.json" {
		t.Errorf("default store = %+v, %v", s, err)
	}
	s, _ = compatStoreFrom(env("HOME", "/home/x", "FLEET_RECORD_ROOT", "/r", "FLEET_PROCESS_SESSIONS_ROOT", "/p", "FLEET_TRUST_STATE_PATH", "/t.json"))
	if s.projects != "/r" || s.sessions != "/p" || s.trustState != "/t.json" {
		t.Errorf("overridden store = %+v", s)
	}
}

// A deletion must never reach a path that does not carry this run's nonce.
func TestContainsNonce(t *testing.T) {
	if !containsNonce("/tmp/cfc-0123456789/t/b", "0123456789") {
		t.Error("a path with the nonce must match")
	}
	for _, p := range []string{"/tmp/cfc-other/t/b", "/home/x/project", "/tmp/cfc-", ""} {
		if containsNonce(p, "0123456789") {
			t.Errorf("%q must not match", p)
		}
	}
}

// Teardown on a harness that never started a world is a no-op, and calling it
// again does nothing more.
func TestTeardownWithoutAWorldIsANoOp(t *testing.T) {
	h := &compatHarness{}
	h.teardown()
	h.teardown()
	if len(h.tearErrs) != 0 {
		t.Errorf("errors from a no-op teardown: %v", h.tearErrs)
	}
}

// The watchdog covers a harness that was killed outright. It removes only
// paths whose name marks them as this command's, and it waits for the pid it
// was given to disappear first.
func TestWatchdogRemovesOnlyMarkedPaths(t *testing.T) {
	root := t.TempDir()
	marked := filepath.Join(root, "cfc-marked")
	alsoMarked := filepath.Join(root, "-private-x-cfc-encoded")
	unmarked := filepath.Join(root, "real-project")
	for _, d := range []string{marked, alsoMarked, unmarked} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	child := exec.Command("sleep", "1")
	if err := child.Start(); err != nil {
		t.Skip(err)
	}
	go child.Wait()
	cmd := exec.Command("sh", "-c", watchdogScript, "sh", fmt.Sprint(child.Process.Pid), marked, alsoMarked, unmarked)
	cmd.Env = []string{"PATH=/usr/bin:/bin"} // no $TMUX: the final kill has nothing to aim at
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Error("the watchdog did not wait for the harness pid to disappear")
	}
	for _, d := range []string{marked, alsoMarked} {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s survived the watchdog", d)
		}
	}
	if _, err := os.Stat(unmarked); err != nil {
		t.Errorf("the watchdog removed a path that is not marked as ours: %v", err)
	}
}

// A session killed while young leaves a failed-start record in the runtime's
// machine-wide state, and enough of those change how every real session on the
// machine renders. Teardown therefore waits for the youngest session to reach a
// safe age — and only for that long, and only when it has to.
func TestSettleLaunchesWaitsForYoungSessionsOnly(t *testing.T) {
	ctx := context.Background()
	elapsed := func(h *compatHarness, w *compatWorld, ctx context.Context) time.Duration {
		start := time.Now()
		h.settleLaunches(ctx, w)
		return time.Since(start)
	}
	young := &compatWorld{created: []compatCreated{{label: "a", at: time.Now()}}}
	old := &compatWorld{created: []compatCreated{{label: "a", at: time.Now().Add(-time.Hour)}}}

	if d := elapsed(&compatHarness{settleAge: 400 * time.Millisecond}, young, ctx); d < 300*time.Millisecond {
		t.Errorf("a session started just now was not waited for (%s)", d)
	}
	if d := elapsed(&compatHarness{settleAge: 400 * time.Millisecond}, old, ctx); d > 100*time.Millisecond {
		t.Errorf("an old session made teardown wait %s", d)
	}
	if d := elapsed(&compatHarness{settleAge: 0}, young, ctx); d > 100*time.Millisecond {
		t.Errorf("with the rule off, teardown waited %s", d)
	}
	if d := elapsed(&compatHarness{settleAge: 400 * time.Millisecond}, &compatWorld{}, ctx); d > 100*time.Millisecond {
		t.Errorf("no sessions, and teardown waited %s", d)
	}
	// The wait is bounded by teardown's own deadline: a cleanup that hangs is
	// worse than one that leaves a strike behind.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if d := elapsed(&compatHarness{settleAge: time.Hour}, young, short); d > time.Second {
		t.Errorf("the wait outlived the deadline (%s)", d)
	}
}

// The youngest session decides: one old session does not excuse a young one.
func TestSettleLaunchesUsesTheYoungestSession(t *testing.T) {
	w := &compatWorld{created: []compatCreated{
		{label: "old", at: time.Now().Add(-time.Hour)},
		{label: "young", at: time.Now()},
	}}
	start := time.Now()
	(&compatHarness{settleAge: 300 * time.Millisecond}).settleLaunches(context.Background(), w)
	if time.Since(start) < 200*time.Millisecond {
		t.Error("the young session was not waited for")
	}
}

// The real run applies the rule; only a test with a synthetic runtime turns it off.
func TestRunCompatEnablesTheSettlingRule(t *testing.T) {
	if compatSettleAge < 12*time.Second {
		t.Errorf("compatSettleAge = %s: the runtime clears a launch's record after about ten seconds, so anything much shorter still leaves a failed start behind", compatSettleAge)
	}
}

// compatBypassSpec is a bypass-mode spec that consents to the acceptance screen,
// which is what acceptanceScreen needs before it will recognise one.
func compatBypassSpec() fleet.SessionSpec {
	return fleet.SessionSpec{PermissionMode: fleet.PermissionModeBypass, Consents: []fleet.PromptKind{fleet.PromptBypassAcceptance}}
}

// The folder-trust dialog as the runtime paints it, captured from a real
// launch (the directory name replaced). A check reads its prompt from the SAME
// capture it waits on: an earlier version asked the driver's State for the
// prompt and used a separate capture for the menu's shape, and the two could
// catch the dialog at different stages of painting.
const compatTrustDialog = `
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 Accessing workspace:

 /tmp/example

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source
 project, or work from your team). If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

func TestShotPromptIsReadFromItsOwnCapture(t *testing.T) {
	shot := compatShot{ok: true, sc: newScreen(compatTrustDialog)}
	p, unnumbered := shot.prompt()
	if p == nil {
		t.Fatal("the folder-trust dialog was not read as a prompt")
	}
	if p.Kind != fleet.PromptFolderTrust || !unnumbered || len(p.Options) != 2 {
		t.Errorf("prompt = %+v unnumbered=%v", p, unnumbered)
	}
	if idx, ok := affirmativeOption(p); !ok || idx != 2 {
		t.Errorf("affirmativeOption = %d, %v; want option 2", idx, ok)
	}
	// A half-painted dialog (no footer yet) is not a prompt at all — which is
	// why the evidence and the wait condition must come from one capture.
	partial := compatShot{ok: true, sc: newScreen(strings.Split(compatTrustDialog, " Enter to confirm")[0])}
	if p, _ := partial.prompt(); p != nil {
		t.Errorf("a dialog without its footer was read as a prompt: %+v", p)
	}
	// An ordinary idle screen carries no prompt.
	idle := compatShot{ok: true, sc: newScreen("\n────────────\n❯ \n────────────\n  ? for shortcuts\n")}
	if p, _ := idle.prompt(); p != nil {
		t.Errorf("an idle composer was read as a prompt: %+v", p)
	}
}

// Shell mode replaces the prompt glyph, so a leading "!" is recognised by the row
// that replaces it, never by the text a plain prompt happens to contain.
func TestShellModeDetection(t *testing.T) {
	shell := newScreen("\n────────────────────\n! echo hi\n────────────────────\n  ! for shell mode\n")
	if !shellMode(shell) {
		t.Error("a composer row starting with ! under a rule is shell mode")
	}
	normal := newScreen("\n────────────────────\n❯  !echo hi\n────────────────────\n  ⏸ manual mode on\n")
	if shellMode(normal) {
		t.Error("a plain prompt whose text begins with a space and ! is not shell mode")
	}
	if _, found := composerRow(normal); !found {
		t.Error("the prompt row was not found")
	}
}

// The pack holds what the checks were judged on, redacted, and says what it holds.
func TestPackWritesRedactedEvidenceAndAManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pack")
	p, err := newCompatPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &compatHarness{}
	h.ev.u = compatBoot{shot: compatShot{ok: true, sc: newScreen(compatTrustDialog), bracket: "1"}}
	h.ev.a = compatBoot{rec: []byte(`{"pid":1,"cwd":"/home/someone/x"}`)}
	h.ev.sends = map[string]*compatSend{"short": {entries: []map[string]any{
		{"type": "user", "message": map[string]any{"content": "hello /home/someone/x"}},
		{"type": "attachment", "attachment": "environment content that must not be kept"},
	}}}
	h.world = &compatWorld{scratch: "/tmp/cfc-abc", store: compatStore{home: "/home/someone"}}
	if err := p.writeEvidence(h); err != nil {
		t.Fatal(err)
	}

	pane, err := os.ReadFile(filepath.Join(dir, "states", "trust-dialog", "pane.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := RedactCapture(h.ev.u.shot.plain()); string(pane) != want {
		t.Errorf("pane.txt is not the redacted capture")
	}
	if strings.Contains(string(pane), "/tmp/example") {
		t.Errorf("the pane's path survived redaction:\n%s", pane)
	}
	rec, _ := os.ReadFile(filepath.Join(dir, "sessions", "a", "record.json"))
	if strings.Contains(string(rec), "/home/someone") || !strings.Contains(string(rec), "~/x") {
		t.Errorf("the record's home directory was not rewritten: %s", rec)
	}
	tr, _ := os.ReadFile(filepath.Join(dir, "sessions", "a", "transcript.jsonl"))
	if strings.Contains(string(tr), "attachment") || strings.Contains(string(tr), "/home/someone") || !strings.Contains(string(tr), "hello") {
		t.Errorf("the transcript slice should keep the user turn, scrubbed, and drop attachments: %s", tr)
	}
	var m struct {
		Files []string `json:"files"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err := json.Unmarshal(b, &m); err != nil || len(m.Files) != 3 {
		t.Errorf("manifest = %s (%v), want the three files written", b, err)
	}
}

// #212: what makes the runtime ask is a file imported from OUTSIDE the working
// directory, so the fixture is only worth having if the imported file really is
// outside every directory that imports it. Directory "a" and directory "i" carry
// the same instruction file, and that file is inert text.
func TestCompatImportsFixtureImportsFromOutsideTheDirectory(t *testing.T) {
	scratch := "/tmp/cfc-abc"
	shared := filepath.Join(scratch, "shared.md")
	text := compatImportsInstructions(shared)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	last := lines[len(lines)-1]
	if last != "@"+shared {
		t.Fatalf("the last line is %q, want a bare import of %q", last, shared)
	}
	for role, dir := range compatDirs(scratch) {
		if role != "a" && role != "i" {
			continue
		}
		if strings.HasPrefix(shared, dir+string(filepath.Separator)) {
			t.Errorf("directory %s (%s) contains the file it imports, so it is not an external import", role, dir)
		}
	}
	// Exactly one import: the runtime asks about the set, and the check reads one.
	if n := strings.Count(text, "@"); n != 1 {
		t.Errorf("the instruction file carries %d '@' characters, want exactly the one import", n)
	}
	if strings.Contains(compatSharedImportText, "@") {
		t.Errorf("the imported file must not import anything itself: %q", compatSharedImportText)
	}
}

// evalCheck runs one catalogued check's evaluator over evidence a test built by
// hand. The multiplexer-gated tests prove the same failures end to end against a
// synthetic runtime, but CI does not run those; these run everywhere.
func evalCheck(t *testing.T, h *compatHarness, id string) compat.Verdict {
	t.Helper()
	suite := newCompatSuite(h)
	for _, c := range suite.Checks {
		if c.ID == id {
			return c.Eval()
		}
	}
	t.Fatalf("no check %q in the suite", id)
	return compat.Verdict{}
}

func shotOf(screen string) compatShot { return compatShot{ok: true, sc: newScreen(screen)} }

const compatIdleScreen = "\n────────────\n❯ \n────────────\n  ? for shortcuts\n"

// #212: F-IMPORTS reads one screen — the one a trusted, never-approved directory
// that imports from outside itself comes up on — and says what is different when
// it is not the measured one.
func TestFImportsJudgesTheDialogItIsShown(t *testing.T) {
	swapped := strings.NewReplacer(
		"  ❯ No, disable external imports\n    Yes, allow external imports",
		"    No, disable external imports\n  ❯ Yes, allow external imports").Replace(fixtureExternalImportsMenu)
	numbered := strings.NewReplacer(
		"  ❯ No, disable external imports\n    Yes, allow external imports",
		"  ❯ 1. No, disable external imports\n    2. Yes, allow external imports").Replace(fixtureExternalImportsMenu)
	reworded := strings.NewReplacer("No, disable external imports", "Refuse to follow them", "Yes, allow external imports", "Follow them").Replace(fixtureExternalImportsMenu)

	for _, tc := range []struct {
		name   string
		screen string
		pass   bool
		want   []string
	}{
		{"the measured dialog", fixtureExternalImportsMenu, true, []string{"unnumbered menu", "decline highlighted (#1 of 2)", "affirmative option (#2)", "never answered"}},
		{"no dialog at all — a build that stopped asking", compatIdleScreen, false, []string{"no dialog appeared", "no longer asks"}},
		{"the options reworded", reworded, false, []string{`classified as ""`}},
		{"the allow row highlighted", swapped, false, []string{"not the decline", "bare Enter would allow the import"}},
		{"a numbered menu, where a digit would choose", numbered, false, []string{"numbered"}},
		{"the trust question standing in front of it", compatTrustDialog, false, []string{"trust answer the harness wrote", "never reached"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &compatHarness{}
			h.ev.i = compatBoot{ran: true, shot: shotOf(tc.screen)}
			v := evalCheck(t, h, "F-IMPORTS")
			if v.Pass != tc.pass || v.Err {
				t.Fatalf("verdict = %+v, want pass=%v and not an error", v, tc.pass)
			}
			for _, w := range tc.want {
				if !strings.Contains(v.Detail, w) {
					t.Errorf("detail %q does not say %q", v.Detail, w)
				}
			}
		})
	}
}

// #212: C1's half. The directory it boots imports a file from outside itself, so
// a dialog naming that import means the seeder's answer was not honoured, and the
// report has to say which keys the seeder writes — a rename is otherwise a
// session parked on a question with nothing to point at.
func TestC1NamesTheImportsKeysWhenTheSeededDirectoryStillAsks(t *testing.T) {
	ready := func(screen string) *compatHarness {
		h := &compatHarness{}
		h.ev.a = compatBoot{ran: true, ready: compatReady(shotOf(screen)), shot: shotOf(screen)}
		return h
	}

	v := evalCheck(t, ready(fixtureExternalImportsMenu), "C1")
	if v.Pass || v.Err {
		t.Fatalf("verdict = %+v, want a failure", v)
	}
	for _, w := range []string{"still asks about that import", trustseed.ImportsApprovedKey, trustseed.ImportsShownKey} {
		if !strings.Contains(v.Detail, w) {
			t.Errorf("detail %q does not say %q", v.Detail, w)
		}
	}

	// A trust question in that seat is the old failure, worded as before — the
	// imports hint belongs to the imports dialog only.
	v = evalCheck(t, ready(compatTrustDialog), "C1")
	if v.Pass || strings.Contains(v.Detail, "still asks about that import") || !strings.Contains(v.Detail, "still shows a prompt") {
		t.Errorf("a trust dialog in C1's seat: %+v", v)
	}

	v = evalCheck(t, ready(compatIdleScreen), "C1")
	if !v.Pass || !strings.Contains(v.Detail, "imports a file from outside it") {
		t.Errorf("a composer with no dialog: %+v", v)
	}
}

// feedbackWordingReads reports whether marker is inside text and text is read
// by parseFeedbackStatus as the given state.
func feedbackWordingReads(text, marker string, want feedbackState) bool {
	state, _, ok := parseFeedbackStatus(text)
	return ok && state == want && strings.Contains(strings.ToLower(text), marker)
}
