package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #283: pre-approved tool permissions only inside an enforced sandbox,
// and a parked permission ask reads as blocked.

func stateOf(t *testing.T, d *Driver, id string) fleet.SessionState {
	t.Helper()
	st, err := d.State(context.Background(), testReq, fleet.SessionRef{ID: id})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return st
}

// ---- a parked ask reads as blocked ----------------------------------------

func TestState_ParkedPermissionAskReadsAsWaitingOnAPrompt(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setBusy(ref.ID)
	f.mu.Lock()
	f.permissions = []wirePermissionAsk{
		{ID: "per_other", SessionID: "ses_somebody_else", Permission: "bash", Patterns: []string{"rm -rf /"}},
		{ID: "per_mine", SessionID: ref.ID, Permission: "bash", Patterns: []string{"make test"}},
	}
	f.mu.Unlock()

	st := stateOf(t, d, ref.ID)
	if st.Status != fleet.StatusWaitingInput {
		t.Fatalf("Status = %q, want waiting_input: a turn stopped on an ask is blocked, not busy", st.Status)
	}
	if st.WaitingOn != fleet.WaitingPrompt {
		t.Errorf("WaitingOn = %q, want prompt", st.WaitingOn)
	}
	if st.Confidence != fleet.ConfidenceObserved {
		t.Errorf("Confidence = %q, want observed", st.Confidence)
	}
	p := st.Prompt
	if p == nil {
		t.Fatal("Prompt is nil")
	}
	if p.Kind != fleet.PromptToolPermission {
		t.Errorf("Kind = %q, want tool-permission", p.Kind)
	}
	if p.Nonce != "per_mine" {
		t.Errorf("Nonce = %q, want the ask's own id, and never another session's ask", p.Nonce)
	}
	if !strings.Contains(p.Question, "bash") || !strings.Contains(p.Question, "make test") {
		t.Errorf("Question = %q, want the permission and what it was asked for", p.Question)
	}
	if strings.Contains(p.Question, "rm -rf") {
		t.Errorf("Question = %q carries another session's ask", p.Question)
	}
	if p.Options == nil || len(p.Options) != 0 {
		t.Errorf("Options = %v, want an empty list: Respond is unsupported, so nothing can be picked", p.Options)
	}
}

func TestState_QuestionIsBounded(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setBusy(ref.ID)
	f.mu.Lock()
	f.permissions = []wirePermissionAsk{{ID: "per_1", SessionID: ref.ID, Permission: "bash", Patterns: []string{strings.Repeat("x", 5000)}}}
	f.mu.Unlock()

	if q := stateOf(t, d, ref.ID).Prompt.Question; len([]rune(q)) > askQuestionMax+1 {
		t.Errorf("Question is %d runes, want at most %d", len([]rune(q)), askQuestionMax+1)
	}
}

func TestState_BusyWithNoAskStaysWorking(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setBusy(ref.ID)

	if st := stateOf(t, d, ref.ID); st.Status != fleet.StatusWorking || st.Prompt != nil {
		t.Errorf("got %+v, want plain working", st)
	}
}

// An idle session is not asked about: there is no turn for an ask to stop.
func TestState_IdleSessionIsNotQueriedForAsks(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	if st := stateOf(t, d, ref.ID); st.Status != fleet.StatusIdle {
		t.Fatalf("Status = %q, want idle", st.Status)
	}
	for _, r := range f.requestsSnapshot() {
		if r.path == "/permission" {
			t.Fatal("an idle session cost a permission read")
		}
	}
}

// §5.7: a question that could not be asked is not the answer "no".
func TestState_UnreadablePermissionListIsNotNoAsk(t *testing.T) {
	for _, status := range []int{404, 500} {
		f := newFakeServer(t)
		d := newTestDriver(t, f)
		ref := createOne(t, d, "/work/x", "key-1")
		f.setBusy(ref.ID)
		f.mu.Lock()
		f.permissionStatus = status
		f.mu.Unlock()

		st := stateOf(t, d, ref.ID)
		if st.Status != fleet.StatusWorking {
			t.Errorf("status %d: Status = %q, want working: the busy read itself succeeded", status, st.Status)
		}
		if !strings.Contains(st.Evidence, "could not be read") {
			t.Errorf("status %d: Evidence = %q, want it to say the permission list could not be read", status, st.Evidence)
		}
	}
}

func TestList_ParkedAskReadsAsBlockedAndIdleDoesNotCostARead(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	parked := createOne(t, d, "/work/a", "key-1")
	busy := createOne(t, d, "/work/b", "key-2")
	f.setBusy(parked.ID)
	f.setBusy(busy.ID)
	f.mu.Lock()
	f.permissions = []wirePermissionAsk{{ID: "per_1", SessionID: parked.ID, Permission: "edit"}}
	f.mu.Unlock()

	col, err := d.List(context.Background(), testReq, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fleet.Status{}
	for _, s := range col.Items() {
		got[s.ID] = s.State.Status
	}
	if got[parked.ID] != fleet.StatusWaitingInput {
		t.Errorf("parked session listed %q, want waiting_input", got[parked.ID])
	}
	if got[busy.ID] != fleet.StatusWorking {
		t.Errorf("other busy session listed %q, want working", got[busy.ID])
	}
}

// ---- bypass: refused without a sandbox -------------------------------------

func TestBypassWithoutASandboxIsRefusedUnsupported(t *testing.T) {
	d, root := newIsolatedDriver(t)
	_, err := d.Create(context.Background(), testReq, "k-nosbx", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), PermissionMode: fleet.PermissionModeBypass,
	})
	assertRefusedUnsupported(t, err, "sandbox")
	if len(d.live) != 0 {
		t.Fatal("a refused create started a server")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("a refused create left %v behind", entries)
	}
}

func TestBypassInSharedModeIsRefused(t *testing.T) {
	d := newTestDriver(t, newFakeServer(t))
	_, err := d.Create(context.Background(), testReq, "k-shared", fleet.SessionSpec{
		Cwd: "/work/x", PermissionMode: fleet.PermissionModeBypass, Sandbox: &fleet.SandboxSpec{},
	})
	// The sandbox refusal comes first and says why; either way it is unsupported
	// and nothing was created.
	assertRefusedUnsupported(t, err, "")
}

func TestAnyOtherPermissionModeIsRefused(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	_, err := d.Create(context.Background(), testReq, "k-other", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), PermissionMode: "acceptEdits", Sandbox: &fleet.SandboxSpec{},
	})
	assertRefusedUnsupported(t, err, "")
}

func assertRefusedUnsupported(t *testing.T, err error, mention string) {
	t.Helper()
	fe, ok := err.(*fleet.Error)
	if !ok || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("want an unsupported refusal, got %v", err)
	}
	if mention != "" && !strings.Contains(fe.Message, mention) {
		t.Fatalf("the refusal must say why (%q): %s", mention, fe.Message)
	}
}

// ---- bypass inside a sandbox ----------------------------------------------

func bypassSession(t *testing.T, d *Driver, key string) (fleet.Session, string) {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess := mustCreate(t, d, key, fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(cwd), Sandbox: &fleet.SandboxSpec{}, PermissionMode: fleet.PermissionModeBypass,
	})
	return sess, cwd
}

func TestBypassInASandboxConfiguresOnlyThatSessionsOwnRuntime(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	sess, _ := bypassSession(t, d, "k-bypass")
	known, _ := d.wasSeen(sess.ID)

	raw, err := os.ReadFile(filepath.Join(known.srv.dir, permissionConfigRel))
	if err != nil {
		t.Fatalf("the session's own config was not written: %v", err)
	}
	var cfg struct {
		Permission map[string]string `json:"permission"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	for _, tool := range []string{"edit", "bash", "webfetch"} {
		if cfg.Permission[tool] != "allow" {
			t.Errorf("permission.%s = %q, want allow", tool, cfg.Permission[tool])
		}
	}

	v := inside(t, d, sess.ID)
	if !contains(v.Args, "--pure") {
		t.Errorf("serve was started with %v, want --pure (no external plugins in a pre-approved session)", v.Args)
	}
	// It is the session's own config the runtime reads, not a shared one.
	if !strings.HasPrefix(v.vars()["XDG_CONFIG_HOME"], known.srv.dir) {
		t.Errorf("XDG_CONFIG_HOME = %q, want it inside the session's own directory %q", v.vars()["XDG_CONFIG_HOME"], known.srv.dir)
	}

	st := stateOf(t, d, sess.ID)
	if st.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("PermissionMode = %q, want bypass", st.PermissionMode)
	}
	if st.Sandbox == nil {
		t.Error("the state does not report the sandbox bypass depends on")
	}
	if !d.Capabilities().ObservesPermissionMode {
		t.Error("ObservesPermissionMode = false for a driver that reports it")
	}
	listed := listed(t, d)[sess.ID]
	if listed.State.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("List reports PermissionMode %q, want bypass", listed.State.PermissionMode)
	}
}

// A second session on the same driver, created without bypass, shares nothing
// with the first: no config, no flag, nothing reported.
func TestBypassDoesNotLeakIntoAnotherSession(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	requireSandboxHost(t, d)
	bypass, _ := bypassSession(t, d, "k-bypass")
	plain := mustCreate(t, d, "k-plain", fleet.SessionSpec{Cwd: fleet.AbsolutePath(t.TempDir())})

	known, _ := d.wasSeen(plain.ID)
	if _, err := os.Stat(filepath.Join(known.srv.dir, permissionConfigRel)); !os.IsNotExist(err) {
		t.Errorf("a session that did not ask for bypass has a permission config: %v", err)
	}
	if contains(inside(t, d, plain.ID).Args, "--pure") {
		t.Error("a session that did not ask for bypass was started with --pure")
	}
	if st := stateOf(t, d, plain.ID); st.PermissionMode != "" {
		t.Errorf("PermissionMode = %q, want absent: the driver set nothing for this session", st.PermissionMode)
	}
	if st := stateOf(t, d, bypass.ID); st.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("the bypass session reads %q", st.PermissionMode)
	}
}

func TestBypassSurvivesARestart(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	requireSandboxHost(t, d1)
	sess, _ := bypassSession(t, d1, "k-bypass")
	known, _ := d1.wasSeen(sess.ID)
	cfg := filepath.Join(known.srv.dir, permissionConfigRel)
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// The relaunch must not depend on the file being left intact.
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status == fleet.StatusUnknown {
		t.Fatalf("the session was not relaunched: %+v", got.State)
	}
	if got.State.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("PermissionMode after a restart = %q, want bypass", got.State.PermissionMode)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Errorf("the relaunch did not write the session's config again: %v", err)
	}
	if !contains(inside(t, d2, sess.ID).Args, "--pure") {
		t.Error("the relaunched server was not started with --pure")
	}
}

func TestRecordedBypassWithoutASandboxIsNotRelaunched(t *testing.T) {
	d, _ := newIsolatedDriver(t, WithStateDir(t.TempDir()))
	_, _, err := d.relaunch(context.Background(), &sessionRecord{PermissionMode: fleet.PermissionModeBypass})
	if err == nil || !strings.Contains(err.Error(), "without a sandbox") {
		t.Fatalf("relaunch = %v, want a refusal naming the missing sandbox", err)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ---- capability -------------------------------------------------------------

func TestCapabilities_ObservesPermissionModeOnlyWhereTheDriverSetsIt(t *testing.T) {
	if iso, _ := newIsolatedDriver(t); !iso.Capabilities().ObservesPermissionMode {
		t.Error("isolated mode writes each session's posture and should report it")
	}
	if shared := newTestDriver(t, newFakeServer(t)); shared.Capabilities().ObservesPermissionMode {
		t.Error("shared mode has no posture of its own to report")
	}
}

func TestBuildServeCmd_ExtraArgsFollowTheFixedOnes(t *testing.T) {
	cmd := buildServeCmd("/bin/true", "", 1234, "muster", "cred", nil, "", "--pure")
	if got := cmd.Args[len(cmd.Args)-1]; got != "--pure" {
		t.Errorf("argv = %v, want --pure last", cmd.Args)
	}
	if cmd := buildServeCmd("/bin/true", "", 1234, "muster", "cred", nil, ""); contains(cmd.Args, "--pure") {
		t.Errorf("argv = %v carries --pure without being asked", cmd.Args)
	}
}
