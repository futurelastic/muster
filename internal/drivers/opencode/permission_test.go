package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	if got := strings.Join(p.Options, "|"); got != "Allow once|Allow always|Reject" {
		t.Errorf("Options = %v, want the three answers Respond takes, in the order choice indexes them", p.Options)
	}
	if p.Selected != 0 {
		t.Errorf("Selected = %d, want none: nothing is highlighted, so a bare answer has no default to accept", p.Selected)
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
	// It has a config (muster #293: every session carries the provider policy),
	// but no tool posture in it.
	var cfg map[string]any
	raw, err := os.ReadFile(filepath.Join(known.srv.dir, permissionConfigRel))
	if err != nil {
		t.Fatalf("the plain session has no config at all, so no provider policy: %v", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if _, has := cfg["permission"]; has {
		t.Errorf("a session that did not ask for bypass has a permission posture: %s", raw)
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

// ---- answering a parked ask (muster #296) ---------------------------------

// parkOnAsk creates a session, parks its turn on one ask, and returns it with
// the ask's id (the nonce a caller reads from state).
func parkOnAsk(t *testing.T, f *fakeServer, d *Driver, askID string) fleet.SessionRef {
	t.Helper()
	ref := createOne(t, d, "/work/x", "key-"+askID)
	f.setBusy(ref.ID)
	f.mu.Lock()
	f.permissions = append(f.permissions, wirePermissionAsk{
		ID: askID, SessionID: ref.ID, Permission: "bash", Patterns: []string{"make test"},
	})
	f.mu.Unlock()
	return ref
}

func respond(t *testing.T, d *Driver, id string, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	t.Helper()
	return d.Respond(context.Background(), testReq, fleet.SessionRef{ID: id}, resp)
}

func mustRespond(t *testing.T, d *Driver, id string, resp fleet.Response) fleet.DeliveryReceipt {
	t.Helper()
	rc, err := respond(t, d, id, resp)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	return rc
}

// A caller answers from state alone: read the prompt, pick an option, send its
// nonce. Allow once and Allow always send the verdicts the runtime names, and the
// parked turn then ends.
func TestRespond_ApprovalAnswersTheAskAndTheTurnCompletes(t *testing.T) {
	for _, tc := range []struct {
		choice int
		reply  string
		label  string
	}{
		{1, "once", "Allow once"},
		{2, "always", "Allow always"},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			f := newFakeServer(t)
			d := newTestDriver(t, f)
			ref := parkOnAsk(t, f, d, "per_1")

			st := stateOf(t, d, ref.ID)
			if st.Status != fleet.StatusWaitingInput || st.Prompt == nil {
				t.Fatalf("before: %+v, want waiting_input with a prompt", st)
			}
			rc := mustRespond(t, d, ref.ID, fleet.Response{Choice: tc.choice, Nonce: st.Prompt.Nonce})

			if rc.Outcome != fleet.OutcomeSubmitted {
				t.Fatalf("Outcome = %q (%s), want submitted", rc.Outcome, rc.Reason)
			}
			if !strings.Contains(rc.Reason, tc.label) {
				t.Errorf("Reason = %q, want it to say which option was chosen", rc.Reason)
			}
			if got := f.repliesSnapshot(); len(got) != 1 || got[0] != (recordedReply{"per_1", tc.reply}) {
				t.Errorf("replies = %+v, want exactly one %q for per_1", got, tc.reply)
			}
			after := stateOf(t, d, ref.ID)
			if after.Status != fleet.StatusIdle || after.Prompt != nil {
				t.Errorf("after: status %q prompt %+v, want the turn completed (idle, no prompt)", after.Status, after.Prompt)
			}
		})
	}
}

func TestRespond_RejectAnswersTheAsk(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := parkOnAsk(t, f, d, "per_1")

	rc := mustRespond(t, d, ref.ID, fleet.Response{Choice: 3, Nonce: "per_1"})

	if rc.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("Outcome = %q (%s), want submitted", rc.Outcome, rc.Reason)
	}
	if got := f.repliesSnapshot(); len(got) != 1 || got[0] != (recordedReply{"per_1", "reject"}) {
		t.Errorf("replies = %+v, want exactly one reject for per_1", got)
	}
	if st := stateOf(t, d, ref.ID); st.Prompt != nil {
		t.Errorf("Prompt = %+v after a reject, want none", st.Prompt)
	}
}

// §5.6: an answer to an ask that has already been resolved is refused, never
// applied to the NEXT ask — the harm a stale nonce exists to prevent, and the one
// the tool-permission case makes worst: the next ask may be a different tool.
func TestRespond_StaleNonceIsRefusedAndNeverAppliedToTheNextAsk(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := parkOnAsk(t, f, d, "per_1")
	read := stateOf(t, d, ref.ID).Prompt.Nonce

	// The first ask is resolved elsewhere and the turn raises another.
	f.mu.Lock()
	f.permissions = []wirePermissionAsk{{ID: "per_2", SessionID: ref.ID, Permission: "edit", Patterns: []string{"/etc/hosts"}}}
	f.mu.Unlock()

	rc := mustRespond(t, d, ref.ID, fleet.Response{Choice: 2, Nonce: read})

	if rc.Outcome != fleet.OutcomeRefused {
		t.Fatalf("Outcome = %q (%s), want refused", rc.Outcome, rc.Reason)
	}
	if got := f.repliesSnapshot(); len(got) != 0 {
		t.Errorf("replies = %+v, want none: nothing may reach the runtime on a stale nonce", got)
	}
	if st := stateOf(t, d, ref.ID); st.Prompt == nil || st.Prompt.Nonce != "per_2" {
		t.Errorf("the next ask was disturbed: %+v", st.Prompt)
	}
}

// The ask this answer was meant for is gone and nothing else is pending: refused,
// not queued for whatever is asked next.
func TestRespond_NonceOfAnAskThatIsGoneIsRefused(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	rc := mustRespond(t, d, ref.ID, fleet.Response{Choice: 1, Nonce: "per_gone"})

	if rc.Outcome != fleet.OutcomeRefused {
		t.Fatalf("Outcome = %q, want refused", rc.Outcome)
	}
	if got := f.repliesSnapshot(); len(got) != 0 {
		t.Errorf("replies = %+v, want none", got)
	}
}

// The ask can also be resolved between this driver's read and its reply. The
// runtime's 404 for that is the same refusal — not "no such session", which is a
// different fact and would send the caller looking for a session that exists.
func TestRespond_AskResolvedBetweenReadAndReplyIsRefusedNotMissingSession(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := parkOnAsk(t, f, d, "per_1")
	f.onReply = func(f *fakeServer, id string) {
		f.mu.Lock()
		f.permissions = nil
		f.mu.Unlock()
	}

	rc, err := respond(t, d, ref.ID, fleet.Response{Choice: 1, Nonce: "per_1"})

	if err != nil {
		t.Fatalf("Respond: %v, want a refusal receipt", err)
	}
	if rc.Outcome != fleet.OutcomeRefused {
		t.Errorf("Outcome = %q (%s), want refused", rc.Outcome, rc.Reason)
	}
}

// No choice is the answer a menu reads as "accept what is highlighted". Nothing
// is highlighted here, and the one default available is to run a tool nobody
// read, so every answer that does not name an option is refused with nothing sent.
func TestRespond_AnAnswerThatNamesNoOptionIsRefusedAndSendsNothing(t *testing.T) {
	blank := " "
	empty := []int{}
	for name, resp := range map[string]fleet.Response{
		"no choice":      {Nonce: "per_1"},
		"cancel":         {Cancel: true, Nonce: "per_1"},
		"choices":        {Choices: []int{1}, Nonce: "per_1"},
		"text":           {Text: &blank, Nonce: "per_1"},
		"empty choices":  {Choices: empty, Nonce: "per_1"},
		"past the end":   {Choice: 4, Nonce: "per_1"},
		"choice + text":  {Choice: 1, Text: ptr("yes"), Nonce: "per_1"},
		"choice + clear": {Choice: 1, Cancel: true, Nonce: "per_1"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeServer(t)
			d := newTestDriver(t, f)
			ref := parkOnAsk(t, f, d, "per_1")

			rc := mustRespond(t, d, ref.ID, resp)

			if rc.Outcome != fleet.OutcomeRefused {
				t.Fatalf("Outcome = %q (%s), want refused", rc.Outcome, rc.Reason)
			}
			if got := f.repliesSnapshot(); len(got) != 0 {
				t.Errorf("replies = %+v, want none", got)
			}
			if st := stateOf(t, d, ref.ID); st.Prompt == nil {
				t.Error("the ask was disturbed by a refused answer")
			}
		})
	}
}

func ptr(s string) *string { return &s }

// A nonce is optional, and its absence is not free (§3): the answer still lands
// on the ask that was read, and the receipt says nothing verified it.
func TestRespond_WithoutANonceAnswersAndSaysItWasUnverified(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := parkOnAsk(t, f, d, "per_1")

	rc := mustRespond(t, d, ref.ID, fleet.Response{Choice: 1})

	if rc.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("Outcome = %q (%s), want submitted", rc.Outcome, rc.Reason)
	}
	if !strings.Contains(rc.Reason, "without a nonce") {
		t.Errorf("Reason = %q, want it to say the ask was not verified", rc.Reason)
	}
	if got := f.repliesSnapshot(); len(got) != 1 || got[0].id != "per_1" {
		t.Errorf("replies = %+v, want one for per_1", got)
	}
}

// The reply endpoint names an ask by id alone, so the session check is this
// driver's: another session's ask is never answered through this one.
func TestRespond_NeverAnswersAnotherSessionsAsk(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	mine := createOne(t, d, "/work/x", "key-mine")
	f.setBusy(mine.ID)
	f.mu.Lock()
	f.permissions = []wirePermissionAsk{{ID: "per_other", SessionID: "ses_somebody_else", Permission: "bash"}}
	f.mu.Unlock()

	for _, resp := range []fleet.Response{{Choice: 1}, {Choice: 1, Nonce: "per_other"}} {
		rc := mustRespond(t, d, mine.ID, resp)
		if rc.Outcome != fleet.OutcomeRefused {
			t.Errorf("%+v: Outcome = %q, want refused", resp, rc.Outcome)
		}
	}
	if got := f.repliesSnapshot(); len(got) != 0 {
		t.Errorf("replies = %+v, want none: that ask is not this session's", got)
	}
}

func TestRespond_SessionThatIsNotParkedIsRefused(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1") // idle

	rc := mustRespond(t, d, ref.ID, fleet.Response{Choice: 1})

	if rc.Outcome != fleet.OutcomeRefused {
		t.Fatalf("Outcome = %q, want refused", rc.Outcome)
	}
	if got := f.repliesSnapshot(); len(got) != 0 {
		t.Errorf("replies = %+v, want none", got)
	}
}

// A runtime question is a different surface. It is never answered with a guessed
// choice: the caller is told it is unsupported, and why.
func TestRespond_AQuestionAskStaysUnsupportedWithAReason(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setBusy(ref.ID)
	f.mu.Lock()
	f.questions = []wireQuestionAsk{{ID: "que_1", SessionID: ref.ID}}
	f.mu.Unlock()

	_, err := respond(t, d, ref.ID, fleet.Response{Choice: 1})

	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want an unsupported fleet.Error", err)
	}
	if !strings.Contains(fe.Message, "question") {
		t.Errorf("Message = %q, want the reason to name the question", fe.Message)
	}
	if got := f.repliesSnapshot(); len(got) != 0 {
		t.Errorf("replies = %+v, want none", got)
	}
}

// A runtime without the question route (404) leaves the plain refusal: not
// knowing whether a question is pending must never turn into a different answer.
func TestRespond_UnreadableQuestionListFallsBackToThePlainRefusal(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setBusy(ref.ID)
	f.questionStatus = http.StatusNotFound

	rc := mustRespond(t, d, ref.ID, fleet.Response{Choice: 1})

	if rc.Outcome != fleet.OutcomeRefused {
		t.Errorf("Outcome = %q, want refused", rc.Outcome)
	}
}

// §5.7: a list that could not be read is not "nothing pending", so it is an
// error — not a refusal that tells the caller the ask is gone.
func TestRespond_UnreadablePermissionListIsAnErrorNotARefusal(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := parkOnAsk(t, f, d, "per_1")
	f.mu.Lock()
	f.permissionStatus = http.StatusInternalServerError
	f.mu.Unlock()

	if _, err := respond(t, d, ref.ID, fleet.Response{Choice: 1, Nonce: "per_1"}); err == nil {
		t.Fatal("err = nil, want the read failure")
	}
	if got := f.repliesSnapshot(); len(got) != 0 {
		t.Errorf("replies = %+v, want none", got)
	}
}

// The runtime's own `false` is not an approval, and an empty 2xx is not a
// refusal: each is reported as what was observed.
func TestRespond_ReportsWhatTheRuntimeSaidAboutTheReply(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	f.replyBody = "false"
	ref := parkOnAsk(t, f, d, "per_1")
	if rc := mustRespond(t, d, ref.ID, fleet.Response{Choice: 1, Nonce: "per_1"}); rc.Outcome != fleet.OutcomeUnknown {
		t.Errorf("a `false` reply: Outcome = %q (%s), want unknown", rc.Outcome, rc.Reason)
	}

	f2 := newFakeServer(t)
	d2 := newTestDriver(t, f2)
	f2.replyBody = " "
	ref2 := parkOnAsk(t, f2, d2, "per_1")
	if rc := mustRespond(t, d2, ref2.ID, fleet.Response{Choice: 1, Nonce: "per_1"}); rc.Outcome != fleet.OutcomeSubmitted {
		t.Errorf("an empty 2xx: Outcome = %q (%s), want submitted", rc.Outcome, rc.Reason)
	}
}

func TestRespond_UnknownSessionIsNoSuchSession(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	_, err := respond(t, d, "ses_never", fleet.Response{Choice: 1})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("err = %v, want ErrNoSuchSession", err)
	}
}
