package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
	"github.com/futurelastic/muster/internal/state"
)

// muster #281, #345: this driver runs sessions in the user's own multiplexer,
// which is the very thing a sandbox keeps a session away from. The server stays
// outside; the agent inside the pane is wrapped, and only on a host where that
// was measured to work. Everything below runs without a real host: the host
// probe and the login shell are replaced, and what is checked is the decision,
// the profile that would be applied and the argv that would apply it. The real
// host is checked by TestLiveSandboxedSessionIsConfined.

const (
	testAgentPath = "/opt/agent/bin/claude"
	testShell     = "/bin/testsh"
)

// A driver nobody enabled the sandbox on keeps refusing it, as before.
func TestCreateRefusesSandboxAsUnsupported(t *testing.T) {
	d := New("testbox")
	_, err := d.Create(context.Background(), fleet.Request{}, "key-1", fleet.SessionSpec{
		Cwd: "/work/x", Sandbox: &fleet.SandboxSpec{},
	})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || !strings.Contains(fe.Message, "sandbox") {
		t.Fatalf("Create = %v, want an unsupported refusal naming the sandbox", err)
	}
	if c := d.Capabilities(); c.Sandbox != nil || c.SandboxUnavailable != "" {
		t.Errorf("a driver the sandbox was not enabled on reports %+v / %q", c.Sandbox, c.SandboxUnavailable)
	}
}

// sandboxEnv is a driver whose host, login shell and multiplexer are all fakes.
type sandboxEnv struct {
	t    *testing.T
	f    *fakeMux
	d    *Driver
	root string // the sandbox root
	cfg  string // the agent's configuration directory
	home string
	cwd  string

	mu       sync.Mutex
	now      time.Time
	hostSup  *fleet.SandboxSupport
	hostWhy  string
	authJSON string // what the agent prints for `auth status`
	pathOut  string // what the login shell prints for the path lookup
	shell    int    // shell invocations, of any kind
	probes   int    // measurements (the enter script)
	enterArg []string
	hold     chan struct{} // when set, a measurement waits for it to close
}

func testHostSupport() *fleet.SandboxSupport {
	return &fleet.SandboxSupport{
		Mechanism: sandbox.Mechanism,
		Denies:    sandbox.Denies(),
		Network:   []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
	}
}

func newSandboxEnv(t *testing.T, extra ...Option) *sandboxEnv {
	t.Helper()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	e := &sandboxEnv{
		t:        t,
		f:        twoSessions(),
		root:     filepath.Join(base, "state", "sandbox"),
		cfg:      filepath.Join(base, "home", ".claude"),
		home:     filepath.Join(base, "home"),
		cwd:      filepath.Join(base, "work"),
		now:      time.Unix(1785760000, 0),
		hostSup:  testHostSupport(),
		authJSON: `{"loggedIn": true, "authMethod": "claude.ai"}`,
		pathOut:  agentPathMark + testAgentPath + "\n",
	}
	for _, d := range []string{e.cfg, e.cwd, filepath.Join(base, "state")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := state.Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	opts := []Option{
		withExec(e.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }),
		WithLoginShell(testShell),
		WithState(st),
		WithRecordRoot(filepath.Join(e.cfg, agentProjectsDir)),
		WithSandbox(SandboxOptions{Home: e.home}),
	}
	e.d = New("testbox", append(opts, extra...)...)
	// A background refresh runs in line, so a test can say what the NEXT read sees.
	e.d.sandboxGo = func(f func()) { f() }
	e.d.sandboxHost = func() (*fleet.SandboxSupport, string) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.hostSup == nil {
			return nil, e.hostWhy
		}
		c := *e.hostSup
		return &c, ""
	}
	return e
}

func (e *sandboxEnv) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != testShell {
		return e.f.exec(ctx, name, args...)
	}
	e.mu.Lock()
	e.shell++
	if len(args) < 2 || args[0] != "-lic" {
		e.mu.Unlock()
		return nil, errors.New("unexpected login-shell call")
	}
	var out []byte
	hold := e.hold
	switch args[1] {
	case agentPathScript:
		out = []byte(e.pathOut)
	case agentProbeScript:
		out = []byte(agentFoundMark + "\n")
	case sandboxEnterScript:
		e.probes++
		e.enterArg = append([]string(nil), args[2:]...)
		out = []byte("noise from a startup file\n" + sandboxProbeBegin + "\n" + e.authJSON + "\n")
	default:
		e.mu.Unlock()
		return nil, errors.New("unexpected login-shell script")
	}
	e.mu.Unlock()
	if hold != nil && args[1] == sandboxEnterScript {
		<-hold // a measurement that takes its time
	}
	return out, nil
}

func (e *sandboxEnv) advance(by time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(by)
	e.mu.Unlock()
}

func (e *sandboxEnv) probeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.probes
}

func (e *sandboxEnv) create(key, name string, sb *fleet.SandboxSpec) (fleet.Session, error) {
	return e.d.Create(context.Background(), testCaller, key, fleet.SessionSpec{
		Machine: "testbox", Cwd: fleet.AbsolutePath(e.cwd), Name: name, Sandbox: sb,
	})
}

// launchArgv returns what the multiplexer was told to run, after `--`.
func launchArgv(t *testing.T, f *fakeMux) []string {
	t.Helper()
	argv := agentArgv(newSessionArgv(f))
	if argv == nil {
		t.Fatal("no new-session call")
	}
	return argv
}

// ---- the capability is the measurement ------------------------------------

func TestSandboxCapabilityIsDeclaredWhereTheAgentWasMeasuredToWork(t *testing.T) {
	e := newSandboxEnv(t)
	caps := e.d.Capabilities()
	if caps.Sandbox == nil {
		t.Fatalf("not declared on a host that passed the measurement: %q", caps.SandboxUnavailable)
	}
	if caps.SandboxUnavailable != "" {
		t.Errorf("a declared capability also carries a reason: %q", caps.SandboxUnavailable)
	}
	if len(caps.Sandbox.Denies) != 3 {
		t.Errorf("denies = %v, want all three classes", caps.Sandbox.Denies)
	}
	if len(caps.Sandbox.PackageCache) != 0 {
		t.Errorf("a driver with no package-cache mode lists %v", caps.Sandbox.PackageCache)
	}
	// And the posture follows by derivation, with no special case anywhere.
	sup := fleet.PostureSupportFrom(caps)
	if !sup.Files || !sup.Unattended || len(sup.Network) != 2 {
		t.Errorf("posture support = %+v, want files + unattended + both networks", sup)
	}
	if sup.IsolatedEnvironment {
		t.Error("this driver still cannot isolate the environment; the posture must not claim it")
	}
	if _, err := (fleet.Posture{Attendance: fleet.AttendanceUnattended, Files: &fleet.PostureFiles{}}).Lower(caps); err != nil {
		t.Errorf("an unattended, file-confined posture is refused: %v", err)
	}
	if _, err := (fleet.Posture{IsolatedEnvironment: true}).Lower(caps); err == nil {
		t.Error("an isolated environment is accepted by a driver that cannot isolate one")
	}
}

func TestSandboxMeasurementRunsTheAgentUnderTheProfileThroughTheLoginShell(t *testing.T) {
	e := newSandboxEnv(t)
	if e.d.Capabilities().Sandbox == nil {
		t.Fatal("setup: not declared")
	}
	args := e.enterArg
	if len(args) < 8 {
		t.Fatalf("probe args = %v", args)
	}
	// muster <cwd> env TMPDIR=… CLAUDE_CODE_TMPDIR=… sandbox-exec -f <profile> <agent> auth status
	if args[0] != "muster" || args[2] != "/usr/bin/env" || args[5] != sandbox.ExecPath || args[6] != "-f" {
		t.Errorf("probe argv = %v", args)
	}
	if got := args[len(args)-3:]; !reflect.DeepEqual(got, []string{testAgentPath, "auth", "status"}) {
		t.Errorf("probe command = %v, want the agent's own account report by its resolved path", got)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "TMPDIR=") && !strings.HasPrefix(strings.TrimPrefix(a, "TMPDIR="), e.root) {
			t.Errorf("probe temp dir %q is outside the sandbox root %q", a, e.root)
		}
	}
	// The probe cleans up after itself.
	if ents, _ := os.ReadDir(e.root); len(ents) != 0 {
		t.Errorf("the probe left %d entries behind", len(ents))
	}
}

func TestSandboxIsAbsentWhenTheHostCannotApplyAProfileAndSaysWhy(t *testing.T) {
	e := newSandboxEnv(t)
	e.hostSup, e.hostWhy = nil, "no sandbox mechanism is implemented for plan9"
	caps := e.d.Capabilities()
	if caps.Sandbox != nil || !strings.Contains(caps.SandboxUnavailable, "plan9") {
		t.Fatalf("caps = %+v / %q", caps.Sandbox, caps.SandboxUnavailable)
	}
	// The posture refusal carries the reason (the ruling: refuse, with the reason).
	_, err := (fleet.Posture{Attendance: fleet.AttendanceUnattended}).Lower(caps)
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Reason != fleet.ReasonPostureUnenforceable || !strings.Contains(fe.Message, "plan9") {
		t.Fatalf("Lower = %v, want posture-unenforceable naming the host's reason", err)
	}
	// And a create is refused before anything is started.
	_, err = e.create("k1", "gamma", &fleet.SandboxSpec{})
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || !strings.Contains(fe.Message, "plan9") {
		t.Fatalf("Create = %v, want an unsupported refusal naming the reason", err)
	}
	if n := countCalls(e.f, "new-session"); n != 0 {
		t.Errorf("a refused create started %d sessions", n)
	}
}

func TestSandboxIsAbsentWhereTheAgentIsLoggedOutUnderTheProfile(t *testing.T) {
	e := newSandboxEnv(t)
	e.authJSON = `{"loggedIn": false, "authMethod": "none"}`
	caps := e.d.Capabilities()
	if caps.Sandbox != nil {
		t.Fatal("declared on a host where the agent is logged out under the profile")
	}
	if !strings.Contains(caps.SandboxUnavailable, "keychain") {
		t.Errorf("reason = %q, want the credential store named", caps.SandboxUnavailable)
	}
}

func TestSandboxIsAbsentWhenTheMeasurementCannotBeRead(t *testing.T) {
	e := newSandboxEnv(t)
	e.authJSON = "this is not a status report"
	if caps := e.d.Capabilities(); caps.Sandbox != nil || caps.SandboxUnavailable == "" {
		t.Fatalf("caps = %+v / %q: an unreadable measurement must refuse, with a reason", caps.Sandbox, caps.SandboxUnavailable)
	}
}

func TestSandboxIsAbsentWhenTheAgentCommandWasReplacedOrIsNotTheMeasuredCLI(t *testing.T) {
	e := newSandboxEnv(t, WithCommandBuilder(func(fleet.SessionSpec, string) []string { return []string{"other"} }))
	if caps := e.d.Capabilities(); caps.Sandbox != nil || !strings.Contains(caps.SandboxUnavailable, "replaced") {
		t.Errorf("custom builder: %+v / %q", caps.Sandbox, caps.SandboxUnavailable)
	}
	e = newSandboxEnv(t)
	e.d.agentProgram = "otheragent"
	if caps := e.d.Capabilities(); caps.Sandbox != nil || !strings.Contains(caps.SandboxUnavailable, "otheragent") {
		t.Errorf("another CLI: %+v / %q", caps.Sandbox, caps.SandboxUnavailable)
	}
}

func TestSandboxIsAbsentWithoutARecordRootOrAStateDirectory(t *testing.T) {
	d := New("testbox", WithSandbox(SandboxOptions{}))
	d.sandboxHost = func() (*fleet.SandboxSupport, string) { return testHostSupport(), "" }
	if caps := d.Capabilities(); caps.Sandbox != nil || !strings.Contains(caps.SandboxUnavailable, "record root") {
		t.Errorf("no record root: %+v / %q", caps.Sandbox, caps.SandboxUnavailable)
	}
}

func TestSandboxMeasurementIsCachedAndRetakenWhenStale(t *testing.T) {
	e := newSandboxEnv(t)
	for i := 0; i < 3; i++ {
		e.d.Capabilities()
	}
	if n := e.probeCount(); n != 1 {
		t.Fatalf("three capability reads took %d measurements, want 1", n)
	}
	// A stale answer is still returned at once and retaken behind the read.
	e.advance(sandboxFoundTTL + time.Second)
	if e.d.Capabilities().Sandbox == nil {
		t.Fatal("a stale answer was withdrawn instead of served while it is retaken")
	}
	if n := e.probeCount(); n != 2 {
		t.Fatalf("a stale measurement was not retaken: %d", n)
	}
	// A failure is retaken sooner: the user may have just logged in.
	e.authJSON = `{"loggedIn": false}`
	e.advance(sandboxFoundTTL + time.Second)
	e.d.Capabilities() // serves the old answer, learns the new one
	if e.d.Capabilities().Sandbox != nil {
		t.Fatal("the failed measurement was not taken up")
	}
	e.authJSON = `{"loggedIn": true}`
	e.advance(sandboxFailTTL + time.Second)
	e.d.Capabilities()
	if e.d.Capabilities().Sandbox == nil {
		t.Error("a failed measurement was not retaken after its short lifetime")
	}
}

// Capabilities is read on the path of nearly every request, for its deadline: a
// measurement in progress must never make a reader wait.
func TestSandboxCapabilityReadNeverWaitsBehindAMeasurement(t *testing.T) {
	e := newSandboxEnv(t)
	e.d.sandboxGo = nil // a real goroutine
	if e.d.Capabilities().Sandbox == nil {
		t.Fatal("setup: not declared")
	}
	e.mu.Lock()
	e.hold = make(chan struct{})
	e.mu.Unlock()
	e.advance(sandboxFoundTTL + time.Second)

	done := make(chan *fleet.SandboxSupport, 1)
	go func() { done <- e.d.Capabilities().Sandbox }()
	select {
	case sup := <-done:
		if sup == nil {
			t.Error("the stale answer was withdrawn while the new one is being taken")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a capability read waited for a measurement")
	}
	// Reads during the measurement are served too, and none starts a second one.
	for i := 0; i < 5; i++ {
		e.d.Capabilities()
	}
	e.mu.Lock()
	close(e.hold)
	e.hold = nil
	e.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for e.probeCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.probeCount(); n != 2 {
		t.Errorf("%d measurements, want the first plus exactly one refresh", n)
	}
}

func TestParseAuthStatus(t *testing.T) {
	cases := []struct {
		name         string
		out          string
		loggedIn, ok bool
	}{
		{"in", sandboxProbeBegin + "\n{\n \"loggedIn\": true,\n \"email\": \"x\"\n}\n", true, true},
		{"out", sandboxProbeBegin + "\n{\"loggedIn\": false}", false, true},
		{"noise before the marker is ignored", "{\"loggedIn\": true}\n" + sandboxProbeBegin + "\n{\"loggedIn\": false}", false, true},
		{"no marker", "{\"loggedIn\": true}", false, false},
		{"no report", sandboxProbeBegin + "\nerror: nothing\n", false, false},
		{"not json", sandboxProbeBegin + "\n{oops}", false, false},
	}
	for _, c := range cases {
		li, ok := parseAuthStatus([]byte(c.out))
		if li != c.loggedIn || ok != c.ok {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, li, ok, c.loggedIn, c.ok)
		}
	}
}

// ---- a create under the profile -------------------------------------------

func TestSandboxedCreateWrapsTheAgentInsideTheLoginShell(t *testing.T) {
	e := newSandboxEnv(t)
	if _, err := e.create("k1", "gamma", &fleet.SandboxSpec{}); err != nil {
		t.Fatal(err)
	}
	argv := launchArgv(t, e.f)
	// <shell> -lic <script> muster <record> <env> <cwd> /usr/bin/env … sandbox-exec -f … <agent> …
	if argv[0] != testShell || argv[1] != "-lic" {
		t.Fatalf("argv = %v: the login shell must still be what the multiplexer runs", argv)
	}
	envAt := -1
	for i, a := range argv {
		if a == "/usr/bin/env" {
			envAt = i
		}
	}
	if envAt < 0 {
		t.Fatalf("argv = %v: no confining wrapper", argv)
	}
	wrapped := argv[envAt:]
	layout := newSandboxLayout(e.root, "gamma")
	want := []string{
		"/usr/bin/env", "TMPDIR=" + layout.tmp, "CLAUDE_CODE_TMPDIR=" + layout.tmp,
		sandbox.ExecPath, "-f", layout.profile, testAgentPath,
	}
	if !reflect.DeepEqual(wrapped[:len(want)], want) {
		t.Errorf("wrapper = %v, want it to begin %v", wrapped[:len(want)], want)
	}
	// The agent's own arguments follow the program, untouched.
	if !containsSeq(wrapped, "--remote-control", "gamma") {
		t.Errorf("the agent's flags were lost: %v", wrapped)
	}
	// The wrapper is the ONLY thing that moved: nothing else about the launch
	// (the login shell, the record, the working directory) changed.
	if argv[len(argv)-len(wrapped)-1] != e.cwd {
		t.Errorf("the working directory argument moved: %v", argv)
	}
}

func containsSeq(hay []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(hay); i++ {
		if reflect.DeepEqual(hay[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestSandboxedCreateWritesAProfileThatGrantsOnlyWhatTheAgentNeeds(t *testing.T) {
	e := newSandboxEnv(t)
	if _, err := e.create("k1", "gamma", &fleet.SandboxSpec{
		WritePaths: []fleet.AbsolutePath{fleet.AbsolutePath(filepath.Join(e.home, "git-common"))},
	}); err != nil {
		t.Fatal(err)
	}
	layout := newSandboxLayout(e.root, "gamma")
	b, err := os.ReadFile(layout.profile)
	if err != nil {
		t.Fatal(err)
	}
	prof := string(b)
	recordDir := recordDirFor(e.cwd)

	for _, want := range []string{
		"(deny file-read* file-write*)",
		"(deny network-outbound (remote unix-socket))",
		"(deny mach-lookup)",
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("profile lacks %s", want)
		}
	}
	for _, want := range []string{
		filepath.Join(e.cfg, ".credentials.json"),
		filepath.Join(e.cfg, "settings.json"),
		filepath.Join(e.home, ".claude.json"),
		filepath.Join(e.cfg, "projects", recordDir),
		layout.work,
		e.cwd,
		filepath.Join(e.home, "git-common"),
		"/opt/agent/bin",
	} {
		if !strings.Contains(prof, `(subpath "`+want+`")`) {
			t.Errorf("profile lacks a grant for %s", want)
		}
	}
	// Writes: the working directory, the session's own space, its transcript
	// directory and what the caller named — and nothing else of the agent's.
	writes := profileWrites(t, prof)
	wantWrites := []string{
		e.cwd, layout.work, filepath.Join(e.cfg, "projects", recordDir), filepath.Join(e.home, "git-common"),
	}
	for _, w := range writes {
		if !containsString(wantWrites, w) {
			t.Errorf("profile grants a write to %s", w)
		}
	}
	for _, w := range wantWrites {
		if !containsString(writes, w) {
			t.Errorf("profile does not grant a write to %s", w)
		}
	}
	// Not the configuration directory itself, not the config file, not the
	// other sessions' transcripts, not the home.
	for _, never := range []string{e.cfg, filepath.Join(e.cfg, "projects"), filepath.Join(e.home, ".claude.json"), e.home, layout.dir} {
		if containsString(writes, never) {
			t.Errorf("profile grants a write to %s", never)
		}
	}
	if strings.Contains(prof, `(subpath "`+filepath.Join(e.cfg, "projects")+`")`) {
		t.Error("other sessions' transcripts are readable")
	}
	for _, n := range []string{"SecurityServer", "securityd"} {
		if strings.Contains(prof, n) {
			t.Errorf("the keychain's service %s is allowed", n)
		}
	}
	// What is in force sits where the session cannot write it.
	if containsString(profileReads(t, prof), layout.dir) || containsString(writes, layout.dir) {
		t.Error("the directory holding the profile is granted to the session")
	}
	for _, f := range []string{layout.profile, layout.report} {
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
}

var subpathRE = regexp.MustCompile(`\(subpath "([^"]*)"`)

// profileBlock returns the paths of every `(allow <op> …)` form in a profile.
func profileBlock(prof, op string) []string {
	var out []string
	rest := prof
	for {
		i := strings.Index(rest, "(allow "+op)
		if i < 0 {
			return out
		}
		rest = rest[i+1:]
		end := strings.Index(rest, "\n(")
		if end < 0 {
			end = len(rest)
		}
		for _, m := range subpathRE.FindAllStringSubmatch(rest[:end], -1) {
			out = append(out, m[1])
		}
		rest = rest[end:]
	}
}

func profileWrites(t *testing.T, prof string) []string {
	t.Helper()
	var w []string
	for _, p := range profileBlock(prof, "file-write*") {
		if !strings.HasPrefix(p, "/dev/") {
			w = append(w, p)
		}
	}
	return w
}

func profileReads(t *testing.T, prof string) []string {
	t.Helper()
	return profileBlock(prof, "file-read*")
}

func TestSandboxedSessionReportsTheProfileInForceOnEveryRead(t *testing.T) {
	e := newSandboxEnv(t)
	if _, err := e.create("k1", "gamma", &fleet.SandboxSpec{Network: fleet.SandboxNetworkClosed}); err != nil {
		t.Fatal(err)
	}
	e.f.addSession(fakeSession{name: "gamma", paneID: "%gamma", cwd: e.cwd, pid: 900, created: 1785760000, title: "2_1_220"}, idleFixtureFor("gamma"))

	st, err := e.d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "gamma", Name: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Sandbox == nil {
		t.Fatal("state.sandbox is absent for a sandboxed session")
	}
	if st.Sandbox.Mechanism != sandbox.Mechanism || st.Sandbox.Network != fleet.SandboxNetworkClosed || len(st.Sandbox.Denies) != 3 {
		t.Errorf("state.sandbox = %+v", st.Sandbox)
	}
	if !containsAbs(st.Sandbox.WritePaths, e.cwd) {
		t.Errorf("writePaths = %v, want the working directory among them", st.Sandbox.WritePaths)
	}

	// And it survives a service restart: a fresh driver reads the report file.
	d2 := New("testbox", withExec(e.exec), WithState(e.d.store), WithSandbox(SandboxOptions{Home: e.home}),
		WithRecordRoot(filepath.Join(e.cfg, agentProjectsDir)))
	if got := d2.sandboxStateFor("gamma"); got == nil || !reflect.DeepEqual(got.WritePaths, st.Sandbox.WritePaths) {
		t.Errorf("after a restart: %+v", got)
	}
	// A session that was never sandboxed reports nothing, and an unsandboxed
	// neighbour is not given its profile.
	if got := e.d.sandboxStateFor("alpha💬"); got != nil {
		t.Errorf("an unsandboxed session reports %+v", got)
	}
}

func containsAbs(in []fleet.AbsolutePath, s string) bool {
	for _, x := range in {
		if string(x) == s {
			return true
		}
	}
	return false
}

func TestUnsandboxedCreateOnASandboxedDriverIsUntouched(t *testing.T) {
	e := newSandboxEnv(t)
	if _, err := e.create("k1", "gamma", nil); err != nil {
		t.Fatal(err)
	}
	argv := launchArgv(t, e.f)
	for _, a := range argv {
		if a == sandbox.ExecPath || a == "/usr/bin/env" {
			t.Fatalf("an unsandboxed create was wrapped: %v", argv)
		}
	}
	if e.probeCount() != 0 {
		t.Error("an unsandboxed create ran the measurement")
	}
	if ents, _ := os.ReadDir(e.root); len(ents) != 0 {
		t.Errorf("an unsandboxed create made %d private directories", len(ents))
	}
}

func TestSandboxedCreateRefusals(t *testing.T) {
	e := newSandboxEnv(t)
	cases := []struct {
		name string
		spec *fleet.SandboxSpec
		cwd  string
		kind fleet.ErrorKind
		want string
	}{
		{"package cache", &fleet.SandboxSpec{PackageCache: &fleet.SandboxPackageCache{Path: "/cache"}}, "", fleet.ErrorUnsupported, "package-cache"},
		{"a grant that contains the home", &fleet.SandboxSpec{ReadPaths: []fleet.AbsolutePath{fleet.AbsolutePath(e.home)}}, "", fleet.ErrorInvalid, "home directory"},
		{"a write grant under the shared temp", &fleet.SandboxSpec{WritePaths: []fleet.AbsolutePath{"/tmp"}}, "", fleet.ErrorInvalid, "shared"},
		{"a relative path", &fleet.SandboxSpec{ReadPaths: []fleet.AbsolutePath{"rel"}}, "", fleet.ErrorInvalid, "absolute"},
		{"a working directory that is the home", &fleet.SandboxSpec{}, e.home, fleet.ErrorInvalid, "home directory"},
	}
	for _, c := range cases {
		cwd := c.cwd
		if cwd == "" {
			cwd = e.cwd
		}
		before := countCalls(e.f, "new-session")
		_, err := e.d.Create(context.Background(), testCaller, "k-"+c.name, fleet.SessionSpec{
			Machine: "testbox", Cwd: fleet.AbsolutePath(cwd), Name: "delta", Sandbox: c.spec,
		})
		var fe *fleet.Error
		if !errors.As(err, &fe) || fe.Kind != c.kind || !strings.Contains(fe.Message, c.want) {
			t.Errorf("%s: Create = %v, want %s containing %q", c.name, err, c.kind, c.want)
		}
		if countCalls(e.f, "new-session") != before {
			t.Errorf("%s: a refused create started a session", c.name)
		}
	}
	if ents, _ := os.ReadDir(e.root); len(ents) != 0 {
		t.Errorf("refused creates left %d private directories", len(ents))
	}
	// A refusal after the reservation releases it: the same key can be retried.
	if _, err := e.d.Create(context.Background(), testCaller, "k-a grant that contains the home", fleet.SessionSpec{
		Machine: "testbox", Cwd: fleet.AbsolutePath(e.cwd), Name: "delta", Sandbox: &fleet.SandboxSpec{},
	}); err != nil {
		t.Errorf("a key whose create was refused is stuck: %v", err)
	}
}

func TestSandboxedCreateThatFailsToStartRemovesItsPrivateDirectory(t *testing.T) {
	e := newSandboxEnv(t)
	e.f.failCreate = true
	if _, err := e.create("k1", "gamma", &fleet.SandboxSpec{}); err == nil {
		t.Fatal("setup: the create was expected to fail")
	}
	if ents, _ := os.ReadDir(e.root); len(ents) != 0 {
		t.Errorf("a failed create left %d private directories", len(ents))
	}
	if e.d.sandboxStateFor("gamma") != nil {
		t.Error("a failed create left a sandbox report")
	}
}

func TestCloseRemovesThePrivateDirectoryAndASweepRemovesOrphans(t *testing.T) {
	e := newSandboxEnv(t)
	if _, err := e.create("k1", "gamma", &fleet.SandboxSpec{}); err != nil {
		t.Fatal(err)
	}
	layout := newSandboxLayout(e.root, "gamma")
	if _, err := os.Stat(layout.dir); err != nil {
		t.Fatal(err)
	}
	e.f.addSession(fakeSession{name: "gamma", paneID: "%gamma", cwd: e.cwd, pid: 900, created: 1785760000, title: "2_1_220"}, idleFixtureFor("gamma"))
	started := time.Unix(1785760000, 0)
	closeReq := testCaller
	closeReq.Expect.StartedAt = &started
	if _, err := e.d.Close(context.Background(), closeReq, fleet.SessionRef{Machine: "testbox", ID: "gamma", Name: "gamma"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.dir); !os.IsNotExist(err) {
		t.Errorf("Close left the private directory: %v", err)
	}

	// An orphan: a directory no live session owns. Kept for the grace period
	// (a session just created is not yet listed), swept after it.
	orphan := newSandboxLayout(e.root, "ghost")
	if err := os.MkdirAll(orphan.work, 0o700); err != nil {
		t.Fatal(err)
	}
	e.d.sweepSandbox(nil)
	if _, err := os.Stat(orphan.dir); err != nil {
		t.Fatalf("a directory inside its grace period was swept: %v", err)
	}
	old := e.now.Add(-2 * sandboxDirGrace)
	if err := os.Chtimes(orphan.dir, old, old); err != nil {
		t.Fatal(err)
	}
	e.advance(sandboxSweepEvery + time.Second)
	e.d.sweepSandbox(nil)
	if _, err := os.Stat(orphan.dir); !os.IsNotExist(err) {
		t.Errorf("an orphan past its grace period was kept: %v", err)
	}
	// A live session's directory is never swept, however old.
	live := newSandboxLayout(e.root, "alpha💬")
	if err := os.MkdirAll(live.work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(live.dir, old, old); err != nil {
		t.Fatal(err)
	}
	e.advance(sandboxSweepEvery + time.Second)
	e.d.sweepSandbox([]paneRow{{session: "alpha💬"}})
	if _, err := os.Stat(live.dir); err != nil {
		t.Errorf("a live session's directory was swept: %v", err)
	}
}

func TestSandboxLayoutNeverCollidesAcrossSessionNames(t *testing.T) {
	a := newSandboxLayout("/r", "alpha💬")
	b := newSandboxLayout("/r", "alpha")
	if a.dir == b.dir {
		t.Fatal("two names share a directory")
	}
	if filepath.Dir(a.dir) != "/r" || strings.ContainsAny(filepath.Base(a.dir), "💬/ ") {
		t.Errorf("a directory name carries the session name: %s", a.dir)
	}
}
