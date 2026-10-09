package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #281: the sandbox. The profile TEXT is checked on every platform; what
// the host actually refuses is checked where the host can enforce it (macOS),
// by running real commands from inside a real session's process — a tool the
// agent starts, not a probe the driver makes about itself.

func requireSandboxHost(t *testing.T, d *Driver) {
	t.Helper()
	if d.Capabilities().Sandbox == nil {
		t.Skipf("this host cannot enforce a sandbox: %s", d.sandboxWhy)
	}
}

// ---- the capability, and refusing instead of degrading --------------------

func TestSandboxCapabilityIsDeclaredOnlyWhereItCanBeEnforced(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	sup := d.Capabilities().Sandbox
	if runtime.GOOS != "darwin" {
		if sup != nil {
			t.Fatalf("declared a sandbox on %s, where none is implemented: %+v", runtime.GOOS, sup)
		}
		if d.sandboxWhy == "" {
			t.Fatal("no reason recorded for the absent capability; a refused create would have nothing to say")
		}
		return
	}
	if sup == nil {
		t.Skipf("macOS host that cannot apply a profile (nested sandbox?): %s", d.sandboxWhy)
	}
	want := map[fleet.SandboxDeny]bool{fleet.SandboxDenyFiles: true, fleet.SandboxDenyUnixSockets: true, fleet.SandboxDenySystemServices: true}
	for _, c := range sup.Denies {
		delete(want, c)
	}
	if len(want) != 0 {
		t.Fatalf("a sandbox that does not deny every class is not offered under this name; missing %v", want)
	}
}

func TestSandboxRefusedUnsupportedWhenTheDriverCannotEnforceIt(t *testing.T) {
	// Shared mode (one server for every session) cannot confine one of them.
	d := newTestDriver(t, newFakeServer(t))
	if d.Capabilities().Sandbox != nil {
		t.Fatal("shared mode declared a sandbox")
	}
	_, err := d.Create(context.Background(), testReq, "k-shared", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{},
	})
	assertUnsupported(t, err, "shared mode")
}

func TestSandboxRefusedOnAHostWithoutTheMechanism(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this host has the mechanism; the shared-mode test covers the refusal path here")
	}
	d, _ := newIsolatedDriver(t)
	_, err := d.Create(context.Background(), testReq, "k-nohost", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{},
	})
	assertUnsupported(t, err, runtime.GOOS)
	if len(d.live) != 0 {
		t.Fatal("a refused create started a server")
	}
}

func assertUnsupported(t *testing.T, err error, mention string) {
	t.Helper()
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("want an unsupported refusal, got %v", err)
	}
	if !strings.Contains(fe.Message, "sandbox") || !strings.Contains(fe.Message, mention) {
		t.Fatalf("the refusal must name what is missing (%q): %s", mention, fe.Message)
	}
}

// ---- the profile text (every platform) ------------------------------------

func testPlan(net fleet.SandboxNetwork) *sandboxPlan {
	return &sandboxPlan{
		network: net,
		read:    []string{"/usr", "/work/a", "/work/b"},
		write:   []string{"/work/a"},
	}
}

func TestProfileIsDenyByDefaultForFilesSocketsAndServices(t *testing.T) {
	p, err := testPlan(fleet.SandboxNetworkOpen).profile()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(deny file-read* file-write*)",
		"(allow file-read-metadata)",
		`(subpath "/work/b")`,
		"(deny network-outbound (remote unix-socket))",
		`(allow network-outbound (literal "/private/var/run/mDNSResponder"))`,
		"(deny mach-lookup)",
		"(deny appleevent-send)",
		"(deny lsopen)",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile lacks %s:\n%s", want, p)
		}
	}
	// Deny comes before the allow-list it is subtracted from: later rules win.
	if strings.Index(p, "(deny file-read* file-write*)") > strings.Index(p, `(subpath "/work/b")`) {
		t.Error("the file deny must precede the file grants, or the grants are overridden")
	}
	if strings.Index(p, "(deny mach-lookup)") > strings.Index(p, "(allow mach-lookup") {
		t.Error("the service deny must precede its allow-list")
	}
	// Writable paths appear only in the write grant.
	if w := p[strings.Index(p, "(allow file-write*"):]; strings.Contains(w[:strings.Index(w, "\n\n")], "/work/b") {
		t.Error("a read-only path was granted write access")
	}
	for _, never := range []string{"pasteboard", "launchservicesd", "appleevents", "coreservices"} {
		if strings.Contains(p, never) {
			t.Errorf("the service allow-list names %q, which is a way out", never)
		}
	}
}

func TestProfileNetworkClosedKeepsLoopbackAndDropsTheResolver(t *testing.T) {
	p, err := testPlan(fleet.SandboxNetworkClosed).profile()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, resolverSocket) {
		t.Error("a closed network must not allow the resolver socket")
	}
	if !strings.Contains(p, `(allow network-outbound (remote ip "localhost:*"))`) {
		t.Errorf("loopback must stay reachable, the runtime's own server lives there:\n%s", p)
	}
}

func TestProfileQuotesPathsAndRefusesWhatCannotBeQuoted(t *testing.T) {
	plan := testPlan(fleet.SandboxNetworkOpen)
	plan.read = append(plan.read, `/odd/a"b\c`)
	p, err := plan.profile()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, `(subpath "/odd/a\"b\\c")`) {
		t.Errorf("a quote or backslash in a path must be escaped, not interpreted:\n%s", p)
	}
	plan.read = append(plan.read, "/odd/new\nline")
	if _, err := plan.profile(); err == nil {
		t.Error("a path with a newline can start a new rule; it must be refused")
	}
}

func TestInstallRootsNeverOpenTheHome(t *testing.T) {
	home := "/Users/someone"
	got := installRoots(home+"/bin/opencode", home)
	for _, r := range got {
		if r == home {
			t.Fatalf("a binary in ~/bin opened the whole home: %v", got)
		}
	}
	got = installRoots(home+"/.opencode/bin/opencode", home)
	if len(got) != 2 || got[1] != home+"/.opencode" {
		t.Fatalf("the conventional <prefix>/bin layout should grant the prefix: %v", got)
	}
}

// ---- planning --------------------------------------------------------------

func TestPlanRefusesGrantsThatWouldPutTheHomeBack(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for _, spec := range []fleet.SandboxSpec{
		{ReadPaths: []fleet.AbsolutePath{fleet.AbsolutePath(home)}},
		{WritePaths: []fleet.AbsolutePath{fleet.AbsolutePath(filepath.Dir(home))}},
		{PackageCache: &fleet.SandboxPackageCache{Path: fleet.AbsolutePath(home)}},
	} {
		spec := spec
		_, err := d.planSandbox(fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &spec}, t.TempDir())
		var fe *fleet.Error
		if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid || !strings.Contains(fe.Message, "home") {
			t.Errorf("%+v: want an invalid refusal naming the home, got %v", spec, err)
		}
	}
	// Narrower paths under the home are the point, not the problem.
	sub := t.TempDir()
	if _, err := d.planSandbox(fleet.SessionSpec{Cwd: fleet.AbsolutePath(sub), Sandbox: &fleet.SandboxSpec{}}, t.TempDir()); err != nil {
		t.Errorf("the default profile was refused: %v", err)
	}
}

func TestPlanDefaultsAreTheWorkdirAndThePrivateDirOnly(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	cwd, dir := t.TempDir(), t.TempDir()
	plan, err := d.planSandbox(fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Sandbox: &fleet.SandboxSpec{}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.write) != 2 {
		t.Fatalf("default writable set is the workdir and the session's own directory, got %v", plan.write)
	}
	if plan.network != fleet.SandboxNetworkOpen {
		t.Fatalf("default network is open, got %q", plan.network)
	}
	st := plan.state()
	if st.Mechanism == "" || len(st.Denies) != 3 {
		t.Fatalf("the report must name the mechanism and every denied class: %+v", st)
	}
}

// ---- enforcement (macOS) ----------------------------------------------------

type shResult struct {
	Out string `json:"out"`
	Rc  int    `json:"rc"`
}

// sh runs cmd as a tool of the session: a shell started by the runtime's
// process, inheriting its profile.
func sh(t *testing.T, d *Driver, id, cmd string) shResult {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok {
		t.Fatalf("session %s is not known", id)
	}
	body, _ := json.Marshal(map[string]string{"cmd": cmd})
	req, _ := http.NewRequest(http.MethodPost, known.srv.baseURL+"/__test/sh", bytes.NewReader(body))
	req.SetBasicAuth(known.srv.username, known.srv.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("running %q in the session: %v", cmd, err)
	}
	defer resp.Body.Close()
	var r shResult
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// plantUnderHome writes a file under the real home directory, where a session's
// own HOME is NOT, and removes it afterwards.
func plantUnderHome(t *testing.T, name, content string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to plant a probe file under")
	}
	dir, err := os.MkdirTemp(home, ".muster-sandbox-probe-")
	if err != nil {
		t.Skipf("cannot write under the home directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// shortDir makes a directory with a path short enough for a unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "msbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func sandboxedSession(t *testing.T, d *Driver, key string, sb fleet.SandboxSpec) (fleet.Session, string) {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess := mustCreate(t, d, key, fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Sandbox: &sb})
	return sess, cwd
}

func TestSandboxedSessionCannotReadTheHomeButWorksInItsWorkdir(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	secret := plantUnderHome(t, "id_probe", "PRIVATE-KEY-MATERIAL")

	open := mustCreate(t, d, "k-open", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if r := sh(t, d, open.ID, "cat "+secret); r.Rc != 0 || !strings.Contains(r.Out, "PRIVATE-KEY-MATERIAL") {
		t.Fatalf("control: an UNsandboxed session should read the planted file (otherwise the probe proves nothing): %+v", r)
	}

	sess, cwd := sandboxedSession(t, d, "k-sbx", fleet.SandboxSpec{})
	r := sh(t, d, sess.ID, "cat "+secret)
	if r.Rc == 0 || strings.Contains(r.Out, "PRIVATE-KEY-MATERIAL") {
		t.Fatalf("the sandboxed session read a file under the service user's home: %+v", r)
	}
	if r := sh(t, d, sess.ID, "ls "+filepath.Dir(secret)); r.Rc == 0 {
		t.Fatalf("the sandboxed session listed a directory under the home: %+v", r)
	}
	// Inside its working directory it is a normal session.
	if r := sh(t, d, sess.ID, "cd "+cwd+" && echo hello > f && cat f && mkdir sub && echo x > sub/g && ls sub"); r.Rc != 0 || !strings.Contains(r.Out, "hello") {
		t.Fatalf("a sandboxed session must read and write its working directory: %+v", r)
	}
	// Its own private temp directory works; the shared one does not.
	if r := sh(t, d, sess.ID, `echo t > "$TMPDIR/x" && cat "$TMPDIR/x"`); r.Rc != 0 {
		t.Fatalf("the session's private temp directory must be writable: %+v", r)
	}
	shared := filepath.Join("/tmp", "msbx-shared-"+filepath.Base(cwd))
	t.Cleanup(func() { _ = os.Remove(shared) })
	if r := sh(t, d, sess.ID, "echo leak > "+shared); r.Rc == 0 || fileExists(shared) {
		t.Fatalf("the session wrote to the shared /tmp, a side channel between sessions: %+v", r)
	}
	// A write outside every grant, under the home, is refused too.
	if r := sh(t, d, sess.ID, "echo x > "+filepath.Join(filepath.Dir(secret), "new")); r.Rc == 0 {
		t.Fatalf("the session wrote under the home: %+v", r)
	}
}

func TestSandboxedSessionCannotConnectToUnixSockets(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)

	// A stand-in for the multiplexer's server socket and the ssh-agent's: any
	// listener. A real connect from outside the profile proves the control.
	dir := shortDir(t)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	probe := "/usr/bin/nc -U " + sock + " </dev/null"

	open := mustCreate(t, d, "k-open", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if r := sh(t, d, open.ID, probe); r.Rc != 0 {
		t.Fatalf("control: an unsandboxed session should reach the socket: %+v", r)
	}
	for _, net := range []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed} {
		sess, _ := sandboxedSession(t, d, "k-sbx-"+string(net), fleet.SandboxSpec{
			// Even a socket in a directory the session was GRANTED is not
			// connectable: a file grant says nothing about connecting.
			ReadPaths: []fleet.AbsolutePath{fleet.AbsolutePath(resolvePath(dir))},
			Network:   net,
		})
		if r := sh(t, d, sess.ID, probe); r.Rc == 0 {
			t.Fatalf("network %s: the sandboxed session connected to a unix socket: %+v", net, r)
		}
	}
}

func TestSandboxedSessionCannotReachARealMultiplexerServer(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux on this host")
	}
	dir := shortDir(t)
	sock := filepath.Join(dir, "mx")
	if out, err := exec.Command(tmux, "-S", sock, "-f", "/dev/null", "new-session", "-d", "-s", "probe", "sleep 60").CombinedOutput(); err != nil {
		t.Skipf("cannot start a private multiplexer server: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(tmux, "-S", sock, "kill-server").Run() })

	open := mustCreate(t, d, "k-open", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})
	if r := sh(t, d, open.ID, tmux+" -S "+sock+" list-sessions"); r.Rc != 0 || !strings.Contains(r.Out, "probe") {
		t.Fatalf("control: an unsandboxed session should reach the server: %+v", r)
	}
	sess, _ := sandboxedSession(t, d, "k-sbx", fleet.SandboxSpec{})
	// `new-window "<cmd>"` through the server would run <cmd> OUTSIDE the
	// profile; the connect itself must fail.
	r := sh(t, d, sess.ID, tmux+" -S "+sock+" new-window 'touch "+filepath.Join(dir, "escaped")+"'")
	if r.Rc == 0 || fileExists(filepath.Join(dir, "escaped")) {
		t.Fatalf("the sandboxed session ran a command through the multiplexer, outside the profile: %+v", r)
	}
}

func TestSandboxedSessionCannotReadTheClipboard(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	if _, err := exec.LookPath("pbpaste"); err != nil {
		t.Skip("no pbpaste")
	}
	sess, _ := sandboxedSession(t, d, "k-sbx", fleet.SandboxSpec{})
	// The pasteboard is a system service; the allow-list does not include it,
	// so the tool cannot even ask. (Read-only: the probe never writes to the
	// developer's clipboard.)
	if r := sh(t, d, sess.ID, "/usr/bin/pbpaste"); r.Rc == 0 {
		t.Fatalf("the sandboxed session reached the pasteboard service: %+v", r)
	}
}

func TestSandboxNetworkPosture(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	loop := "/usr/bin/nc -z -w 3 127.0.0.1 " + itoa(port)
	// A routable address the probe never expects to complete a connection to.
	remote := "/usr/bin/nc -z -w 3 192.0.2.1 80"

	for _, n := range []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed} {
		sess, _ := sandboxedSession(t, d, "k-"+string(n), fleet.SandboxSpec{Network: n})
		if r := sh(t, d, sess.ID, loop); r.Rc != 0 {
			t.Fatalf("network %s: loopback must stay reachable (the runtime's own server is there): %+v", n, r)
		}
		st, err := d.State(context.Background(), testReq, sess.SessionRef)
		if err != nil {
			t.Fatal(err)
		}
		if st.Sandbox == nil || st.Sandbox.Network != n {
			t.Fatalf("the state must report the posture in force (%s): %+v", n, st.Sandbox)
		}
	}
	closed, _ := sandboxedSession(t, d, "k-closed2", fleet.SandboxSpec{Network: fleet.SandboxNetworkClosed})
	if r := sh(t, d, closed.ID, remote); r.Rc == 0 {
		t.Fatalf("a closed network reached a remote address: %+v", r)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestSandboxStateReportsTheProfileOnReadAndList(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	extra := t.TempDir()
	sess, cwd := sandboxedSession(t, d, "k-sbx", fleet.SandboxSpec{ReadPaths: []fleet.AbsolutePath{fleet.AbsolutePath(extra)}})
	plain := mustCreate(t, d, "k-plain", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})

	st, err := d.State(context.Background(), testReq, sess.SessionRef)
	if err != nil {
		t.Fatal(err)
	}
	sb := st.Sandbox
	if sb == nil {
		t.Fatal("a sandboxed session's state does not say so")
	}
	has := func(paths []fleet.AbsolutePath, want string) bool {
		for _, p := range paths {
			if string(p) == resolvePath(want) {
				return true
			}
		}
		return false
	}
	if !has(sb.WritePaths, cwd) || !has(sb.ReadPaths, extra) || !has(sb.ReadPaths, cwd) {
		t.Fatalf("the report must show the grants in force, defaults included: %+v", sb)
	}
	if has(sb.WritePaths, extra) {
		t.Fatalf("a read-only grant is reported writable: %+v", sb)
	}
	if sb.Network != fleet.SandboxNetworkOpen {
		t.Fatalf("default network is open: %+v", sb)
	}

	pst, _ := d.State(context.Background(), testReq, plain.SessionRef)
	if pst.Sandbox != nil {
		t.Fatalf("an unsandboxed session reports a sandbox: %+v", pst.Sandbox)
	}
	list, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range list.Items() {
		seen[s.ID] = s.State.Sandbox != nil
	}
	if !seen[sess.ID] || seen[plain.ID] {
		t.Fatalf("List must carry the sandbox report per session: %v", seen)
	}
}

func TestSandboxPackageCacheModes(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	cache := resolvePath(t.TempDir())
	if err := os.WriteFile(filepath.Join(cache, "warm"), []byte("prewarmed"), 0o600); err != nil {
		t.Fatal(err)
	}

	// private: the session works on its own copy; the original is untouched.
	priv, _ := sandboxedSession(t, d, "k-priv", fleet.SandboxSpec{PackageCache: &fleet.SandboxPackageCache{Path: fleet.AbsolutePath(cache)}})
	st, _ := d.State(context.Background(), testReq, priv.SessionRef)
	pc := st.Sandbox.PackageCache
	if pc == nil || pc.Mode != fleet.PackageCachePrivate || (pc.Copy != "clone" && pc.Copy != "copy") || string(pc.Path) == cache {
		t.Fatalf("the report must say a private copy was made, and how: %+v", pc)
	}
	if r := sh(t, d, priv.ID, `cat "$MUSTER_PACKAGE_CACHE"/warm && echo new > "$MUSTER_PACKAGE_CACHE"/added`); r.Rc != 0 || !strings.Contains(r.Out, "prewarmed") {
		t.Fatalf("a private cache must be readable and writable by its session: %+v", r)
	}
	if fileExists(filepath.Join(cache, "added")) {
		t.Fatal("a write to a private cache reached the original")
	}
	if r := sh(t, d, priv.ID, "echo x > "+filepath.Join(cache, "direct")); r.Rc == 0 {
		t.Fatalf("the original cache is writable from a private-mode session: %+v", r)
	}

	// shared: the same directory, writable; the report says so.
	shr, _ := sandboxedSession(t, d, "k-shr", fleet.SandboxSpec{PackageCache: &fleet.SandboxPackageCache{Path: fleet.AbsolutePath(cache), Mode: fleet.PackageCacheShared}})
	if r := sh(t, d, shr.ID, `echo s > "$MUSTER_PACKAGE_CACHE"/shared-write`); r.Rc != 0 || !fileExists(filepath.Join(cache, "shared-write")) {
		t.Fatalf("a shared cache is the caller's own directory, writable: %+v", r)
	}
	st, _ = d.State(context.Background(), testReq, shr.SessionRef)
	if pc := st.Sandbox.PackageCache; pc == nil || pc.Mode != fleet.PackageCacheShared || string(pc.Path) != cache {
		t.Fatalf("the report must say the cache is shared: %+v", pc)
	}
}

func TestSandboxedCreateRefusalsLeaveNothingRunning(t *testing.T) {
	d, root := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	for name, spec := range map[string]fleet.SessionSpec{
		"cwd that does not exist":   {Cwd: "/no/such/dir", Sandbox: &fleet.SandboxSpec{}},
		"write grant on shared tmp": {Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{WritePaths: []fleet.AbsolutePath{"/tmp"}}},
		"cache that does not exist": {Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{PackageCache: &fleet.SandboxPackageCache{Path: "/no/such/cache"}}},
		"caller naming the cache variable": {Cwd: fleet.AbsolutePath(t.TempDir()), Sandbox: &fleet.SandboxSpec{},
			Env: map[string]string{"MUSTER_PACKAGE_CACHE": "/elsewhere"}},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := d.Create(ctx, testReq, "k-"+strings.ReplaceAll(name, " ", "-"), spec)
		cancel()
		var fe *fleet.Error
		if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid {
			t.Errorf("%s: want an invalid refusal, got %v", name, err)
		}
	}
	if n := len(d.live); n != 0 {
		t.Fatalf("refused creates left %d servers running", n)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("refused creates left session directories behind: %v", entries)
	}
}
