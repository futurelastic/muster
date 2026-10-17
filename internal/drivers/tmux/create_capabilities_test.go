package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// A supervisor's sessions carry three things this API could not express, and a
// session missing any of them starts, looks healthy, and fails later somewhere
// else. These tests are about the three, plus the flag-injection hole found
// while adding them.

// --- environment ------------------------------------------------------------

// The values must not be visible to every process on the machine.
//
// The obvious mechanism — the multiplexer's own `-e NAME=value` — puts them in
// an argv, which is §5.3's rule broken for the payload MOST likely to be a
// credential. This is the same assertion the prompt and context paths already
// carry, and it is the one that must never regress.
func TestEnvValuesNeverReachACommandLine(t *testing.T) {
	f := twoSessions()
	const secret = "s3cr3t-not-for-ps"
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "envy", Cwd: "/work/x",
		Env: map[string]string{"FLEET_TEST_TOKEN": secret},
	})
	_ = argv
	for _, c := range f.callsSnapshot() {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Fatalf("an env VALUE reached a command line: %v", c)
			}
		}
	}
}

// And the session must actually receive them. This runs the real wrapper
// through a real shell, the same way the record tests do — a staging file the
// session never applies would satisfy the test above and deliver nothing.
func TestStagedEnvIsAppliedToTheSessionAndThenUnlinked(t *testing.T) {
	sh := shellForTest(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "rec")
	envFile := filepath.Join(dir, "env")

	// A value with spaces, quotes and shell metacharacters: applied verbatim,
	// never re-parsed. If any of it were evaluated, the value would differ or
	// the wrapper would run something.
	const tricky = `a b "c" $(touch ` + "`" + `pwned` + "`" + `) ; echo no`
	if err := os.WriteFile(envFile, []byte("FLEET_TEST_APPLIED="+tricky+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(sh, "-c", envRecordScript, "muster", record, envFile, "",
		"/bin/sh", "-c", `printf '%s' "$FLEET_TEST_APPLIED"`).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v (%s)", err, out)
	}
	if string(out) != tricky {
		t.Errorf("the session received %q, want %q — the value was re-parsed on its way in",
			out, tricky)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("the value was EXECUTED: a command substitution in an env value ran")
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Error("the staging file outlived its read; it may hold a credential")
	}
	// The record is written after the variables are applied, so it is evidence
	// about the environment the agent actually got.
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no record written: %v", err)
	}
	if !strings.Contains(string(raw), "FLEET_TEST_APPLIED") {
		t.Error("the record does not list the injected variable, so it describes an " +
			"environment the agent did not have")
	}
	if strings.Contains(string(raw), tricky) {
		t.Error("the record leaked the VALUE")
	}
}

// The staging format is line-oriented, so a newline in a value would arrive as
// a second variable — inventing a name out of value content, which is the exact
// fabrication the record script was already hardened against. Refuse, loudly.
func TestEnvThatTheFormatCannotCarryIsRefused(t *testing.T) {
	cases := map[string]map[string]string{
		"newline in value":           {"OK": "fine", "BAD": "one\nFAKE=injected"},
		"NUL in value":               {"BAD": "one\x00two"},
		"empty name":                 {"": "x"},
		"name with a dash":           {"NOT-A-NAME": "x"},
		"name starting with a digit": {"1ST": "x"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			f := twoSessions()
			d := newTestDriver(f)
			_, err := d.Create(context.Background(), testCaller, "k-"+name,
				fleet.SessionSpec{Name: "e", Cwd: "/work/x", Env: env})
			if err == nil {
				t.Fatal("create accepted an env the staging format cannot carry faithfully")
			}
			if !strings.Contains(err.Error(), "env") {
				t.Errorf("the refusal does not say which input was wrong: %v", err)
			}
		})
	}
}

// A driver with no shell to apply the file in must refuse rather than start a
// session silently missing its identity.
func TestEnvIsRefusedWhenThereIsNoChannelForIt(t *testing.T) {
	f := twoSessions()
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }), WithBareExec())
	_, err := d.Create(context.Background(), testCaller, "k-bare",
		fleet.SessionSpec{Name: "e", Cwd: "/work/x", Env: map[string]string{"A": "b"}})
	if err == nil {
		t.Fatal("a create carrying env succeeded on a driver that cannot deliver it")
	}
}

// --- resume, permission mode, and flag injection ----------------------------

func TestResumeAndPermissionModeReachTheAgentArgv(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "resumed", Cwd: "/work/x",
		Resume:         "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70",
		PermissionMode: fleet.PermissionModeBypass,
	})
	agent := agentArgv(argv)
	got, ok := flagValue(agent, "--resume")
	if !ok || got != "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70" {
		t.Errorf("--resume = %q (present=%v); a session that cannot continue a "+
			"conversation is a different kind of session", got, ok)
	}
	found := false
	for _, a := range agent {
		if a == "--dangerously-skip-permissions" {
			found = true
		}
	}
	if !found {
		t.Error("permissionMode bypass did not reach the argv")
	}
}

// The hole found while adding the above: a pin is caller data and lands beside
// real flags, so a value beginning with "-" is read as a flag. A create grant
// would otherwise mean "run the agent with arguments of my choosing" — most
// pointedly, the very permission mode that is gated behind a second grant.
//
// muster #84: this guard used to silently drop the flag and let the
// create succeed, with the response echoing the pin back as if it had been
// applied — a caller had no way to notice the substitution. Now the whole
// create is refused before any argv is built, the same as an equally
// flag-shaped resume id already is (TestFlagShapedResumeIsRefusedRatherThanDropped).
func TestFlagShapedPinIsRefusedRatherThanDropped(t *testing.T) {
	for _, tc := range []struct {
		field string
		spec  fleet.SessionSpec
		value string
	}{
		{"agent", fleet.SessionSpec{Name: "sneaky-a", Cwd: "/work/x", Agent: "-n"}, "-n"},
		{"model", fleet.SessionSpec{Name: "sneaky-m", Cwd: "/work/x", Model: "--dangerously-skip-permissions"}, "--dangerously-skip-permissions"},
		{"effort", fleet.SessionSpec{Name: "sneaky-e", Cwd: "/work/x", Effort: "--resume"}, "--resume"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			f := twoSessions()
			d := newTestDriver(f)
			_, err := d.Create(context.Background(), testCaller, "key-"+tc.field, tc.spec)
			if err == nil {
				t.Fatalf("a %s pin that is really a flag was accepted", tc.field)
			}
			var ferr *fleet.Error
			if !errors.As(err, &ferr) {
				t.Fatalf("error is not a *fleet.Error: %v", err)
			}
			if ferr.Kind != fleet.ErrorInvalid {
				t.Errorf("Kind = %q, want %q", ferr.Kind, fleet.ErrorInvalid)
			}
			if !strings.Contains(ferr.Message, tc.field) || !strings.Contains(ferr.Message, tc.value) {
				t.Errorf("message %q does not name the field/value", ferr.Message)
			}
			if newSessionArgv(f) != nil {
				t.Error("the session was started anyway; the refusal must happen before the spawn")
			}
		})
	}
}

func TestFlagShapedResumeIsRefusedRatherThanDropped(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-r",
		fleet.SessionSpec{Name: "r", Cwd: "/work/x", Resume: "--dangerously-skip-permissions"})
	if err == nil {
		t.Fatal("a resume id that is really a flag was accepted")
	}
}

// muster #224: the mirror of TestResumeAndPermissionModeReachTheAgentArgv
// for a create that asks to START a conversation rather than continue one.
func TestConversationIdReachesTheAgentArgv(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "started", Cwd: "/work/x",
		ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70",
	})
	agent := agentArgv(argv)
	got, ok := flagValue(agent, "--session-id")
	if !ok || got != "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70" {
		t.Errorf("--session-id = %q (present=%v); a session that cannot be launched under a "+
			"caller-chosen id is a different kind of session", got, ok)
	}
}

// conversationId and resume answer the same question two incompatible ways
// (#224's own Ask): sending both is refused before any argv is built.
func TestConversationIdAndResumeAreMutuallyExclusive(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-cr",
		fleet.SessionSpec{Name: "cr", Cwd: "/work/x",
			ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70", Resume: "some-other-conv"})
	if err == nil {
		t.Fatal("a create carrying both conversationId and resume was accepted")
	}
	var ferr *fleet.Error
	if !errors.As(err, &ferr) {
		t.Fatalf("error is not a *fleet.Error: %v", err)
	}
	if ferr.Kind != fleet.ErrorInvalid {
		t.Errorf("Kind = %q, want %q", ferr.Kind, fleet.ErrorInvalid)
	}
	if newSessionArgv(f) != nil {
		t.Error("the session was started anyway; the refusal must happen before the spawn")
	}
}

// A conversationId that is not UUID-shaped is refused rather than passed to
// the runtime, which would read it back as a plain string with no format
// guarantee at all.
func TestMalformedConversationIdIsRefused(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-m",
		fleet.SessionSpec{Name: "m", Cwd: "/work/x", ConversationId: "not-a-uuid"})
	if err == nil {
		t.Fatal("a malformed conversationId was accepted")
	}
	var ferr *fleet.Error
	if !errors.As(err, &ferr) {
		t.Fatalf("error is not a *fleet.Error: %v", err)
	}
	if ferr.Kind != fleet.ErrorInvalid {
		t.Errorf("Kind = %q, want %q", ferr.Kind, fleet.ErrorInvalid)
	}
	if newSessionArgv(f) != nil {
		t.Error("the session was started anyway; the refusal must happen before the spawn")
	}
}

// THE Ask in #224: an id already recorded for this working directory must be
// refused, because starting a fresh conversation under it would make two
// conversations share one transcript file.
func TestConversationIdAlreadyRecordedForThisCwdIsRefused(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		WithRecordRoot(root))
	writeRecord(t, root, "/work/x", "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70", "whatever", sessionStart)

	_, err := d.Create(context.Background(), testCaller, "k-dup",
		fleet.SessionSpec{Name: "dup", Cwd: "/work/x", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"})
	if err == nil {
		t.Fatal("a conversationId already recorded for this cwd was accepted")
	}
	var ferr *fleet.Error
	if !errors.As(err, &ferr) {
		t.Fatalf("error is not a *fleet.Error: %v", err)
	}
	if ferr.Kind != fleet.ErrorInvalid {
		t.Errorf("Kind = %q, want %q", ferr.Kind, fleet.ErrorInvalid)
	}
	if newSessionArgv(f) != nil {
		t.Error("the session was started anyway; the refusal must happen before the spawn")
	}
}

// The same id is fine under a DIFFERENT working directory: the runtime's own
// store partitions records by cwd (recordDirFor), so this is not a collision
// it could ever produce.
func TestConversationIdRecordedElsewhereDoesNotBlockADifferentCwd(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		WithRecordRoot(root))
	writeRecord(t, root, "/work/other", "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70", "whatever", sessionStart)

	_, err := d.Create(context.Background(), testCaller, "k-ok",
		fleet.SessionSpec{Name: "ok", Cwd: "/work/x", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestUnknownPermissionModeIsRefused(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-p",
		fleet.SessionSpec{Name: "p", Cwd: "/work/x", PermissionMode: "yolo"})
	if err == nil {
		t.Fatal("an unrecognised permission mode was accepted and handed to the CLI")
	}
	var ferr *fleet.Error
	if !errors.As(err, &ferr) || ferr.Kind != fleet.ErrorInvalid {
		t.Fatalf("error = %v, want a *fleet.Error of kind %q", err, fleet.ErrorInvalid)
	}
	if !strings.Contains(ferr.Message, "acceptEdits") {
		t.Errorf("the refusal should name the accepted set: %s", ferr.Message)
	}
	if newSessionArgv(f) != nil {
		t.Error("the session was started anyway; the refusal must happen before the spawn")
	}
}

// muster #329: every mode the CLI's --permission-mode takes reaches the agent
// argv as that flag, and only bypass uses the skip flag. Capabilities
// advertises exactly the set the create accepts.
func TestEveryCreatePermissionModeReachesTheAgentArgv(t *testing.T) {
	for _, mode := range []string{"default", "acceptEdits", "plan", "auto"} {
		t.Run(mode, func(t *testing.T) {
			f := twoSessions()
			_, argv := createWith(t, f, fleet.SessionSpec{Name: "m", Cwd: "/work/x", PermissionMode: mode})
			agent := agentArgv(argv)
			if got, ok := flagValue(agent, "--permission-mode"); !ok || got != mode {
				t.Errorf("--permission-mode = %q (present=%v), want %q; argv %v", got, ok, mode, agent)
			}
			for _, a := range agent {
				if a == "--dangerously-skip-permissions" {
					t.Errorf("mode %q must not raise the bypass flag", mode)
				}
			}
		})
	}
	// bypass is unchanged: the skip flag, and no --permission-mode beside it.
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "b", Cwd: "/work/x", PermissionMode: fleet.PermissionModeBypass})
	agent := agentArgv(argv)
	if _, ok := flagValue(agent, "--permission-mode"); ok {
		t.Errorf("bypass must keep its own flag, not --permission-mode: %v", agent)
	}
	// And no mode at all passes neither.
	f = twoSessions()
	_, argv = createWith(t, f, fleet.SessionSpec{Name: "n", Cwd: "/work/x"})
	if _, ok := flagValue(agentArgv(argv), "--permission-mode"); ok {
		t.Error("a create naming no mode must pass no --permission-mode")
	}
}

func TestCapabilitiesAdvertisesTheCreatePermissionModes(t *testing.T) {
	d := newTestDriver(twoSessions())
	got := d.Capabilities().CreatePermissionModes
	if strings.Join(got, ",") != strings.Join(fleet.CreatePermissionModes(), ",") {
		t.Errorf("CreatePermissionModes = %v, want %v", got, fleet.CreatePermissionModes())
	}
}

// --- consents ---------------------------------------------------------------

// The permission-mode acceptance screen, with the option text the runtime
// actually ships — read out of its binary, not sampled from a capture.
//
// It classifies as NOTHING, and that is correct: its identifying words are in
// the question, and option matching must not read questions. This test is
// therefore about the other half of the answer — the driver identifying the
// screen from what it DID, having passed the flag that raises it.
const fixtureAcceptanceReal = `  By proceeding, you accept all responsibility for actions taken while running in Bypass Permissions mode.
❯ 1. No, exit
  2. Yes, I accept
Enter to confirm · Esc to cancel`

// The index matters more here than anywhere: the HIGHLIGHTED option is
// "No, exit", so a consent that accepted the default would kill the very
// session it was starting.
func TestAcceptanceScreenIsAnsweredWhenTheCallerAskedForThatMode(t *testing.T) {
	h, d := newSettleHarness(t, fixtureAcceptanceReal, true)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{
			Cwd: "/work/alpha", Prompt: "start working",
			PermissionMode: fleet.PermissionModeBypass,
			Consents:       []fleet.PromptKind{fleet.PromptBypassAcceptance},
		})

	waitFor(t, "the work to be delivered after the acceptance screen", func() bool {
		return h.sawDelivery("start working")
	})
	got := h.keysPressed()
	if len(got) == 0 || got[0] != "2" {
		t.Errorf("keys pressed = %v, want option 2 first — option 1 on this screen is "+
			"\"No, exit\", which would have killed the session", got)
	}
}

// Provenance is required, not decorative. The identical screen, with the
// identical consent, is answered by NOTHING when this driver did not start the
// session in that mode — because then it has no grounds beyond wording, and the
// wording is generic enough to belong to a dialog nobody has seen.
func TestTheSameScreenIsNotAnsweredWithoutTheModeThatRaisesIt(t *testing.T) {
	h, d := newSettleHarness(t, fixtureAcceptanceReal, false)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{
			Cwd: "/work/alpha",
			// Consent given, but no permission mode was requested.
			Consents: []fleet.PromptKind{fleet.PromptBypassAcceptance},
		})

	time.Sleep(3 * promptPollInterval)
	if got := h.keysPressed(); len(got) != 0 {
		t.Errorf("keys pressed = %v; a screen was accepted on wording alone, with no "+
			"evidence this driver caused it", got)
	}
}

// And the fixture the whole issue turned on: the two matchers must agree about
// this screen. The classifier must not name it, and the consent path must
// identify it only with provenance — a future edit that loosens either one
// breaks here rather than silently shipping an inert or over-eager consent.
func TestTheAcceptanceScreenIsUnclassifiableButStillReachable(t *testing.T) {
	p := parsePrompt(newScreen(fixtureAcceptanceReal))
	if p == nil {
		t.Fatal("the fixture does not parse as a prompt at all")
	}
	if k := classifyPromptKind(p); k != "" {
		t.Errorf("classifyPromptKind named this screen %q; its identifying words are in "+
			"the QUESTION, and matching those is what the options-only rule forbids", k)
	}
	consented := fleet.SessionSpec{
		PermissionMode: fleet.PermissionModeBypass,
		Consents:       []fleet.PromptKind{fleet.PromptBypassAcceptance},
	}
	if !acceptanceScreen(consented, p) {
		t.Error("with the mode requested and the consent given, the driver still could " +
			"not identify the screen it caused")
	}
	if acceptanceScreen(fleet.SessionSpec{PermissionMode: fleet.PermissionModeBypass}, p) {
		t.Error("identified without a consent")
	}
	if acceptanceScreen(consented, parsePrompt(newScreen(fixtureTrustPrompt))) {
		t.Error("the folder-trust screen was mistaken for the acceptance screen")
	}
}

// A consent is per kind: consenting to one question does not answer another.
func TestAConsentDoesNotSpendItselfOnADifferentQuestion(t *testing.T) {
	h, d := newSettleHarness(t, fixtureTrustPrompt, false)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{
			Cwd: "/work/alpha",
			// Consent to the OTHER boot question only.
			Consents: []fleet.PromptKind{fleet.PromptBypassAcceptance},
		})

	// Give the loop several polls at a folder-trust screen it was not told about.
	time.Sleep(3 * promptPollInterval)
	if got := h.keysPressed(); len(got) != 0 {
		t.Errorf("keys pressed = %v; a consent to one question answered another", got)
	}
}

// The resume chooser has no safe affirmative option — its choices are summaries
// of somebody's prior sessions, and nothing in the text identifies the one the
// caller named. Consenting to it must be refused at the boundary rather than
// answered with a guess.
func TestTheResumeChooserIsNotConsentable(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-rc",
		fleet.SessionSpec{
			Name: "rc", Cwd: "/work/x",
			Consents: []fleet.PromptKind{fleet.PromptResumeChooser},
		})
	if err == nil {
		t.Fatal("a consent was accepted for a question whose affirmative option " +
			"cannot be identified; answering it is a coin flip")
	}
}

// The older boolean still means what it meant. It shipped, so it keeps working.
func TestTrustCwdStillMeansTheFolderTrustConsent(t *testing.T) {
	spec := fleet.SessionSpec{TrustCwd: true}
	if !spec.ConsentsTo(fleet.PromptFolderTrust) {
		t.Error("trustCwd stopped meaning consent to the folder-trust question")
	}
	if spec.ConsentsTo(fleet.PromptBypassAcceptance) {
		t.Error("trustCwd leaked into consent for a different question")
	}
	both := fleet.SessionSpec{TrustCwd: true, Consents: []fleet.PromptKind{fleet.PromptFolderTrust}}
	if !both.ConsentsTo(fleet.PromptFolderTrust) {
		t.Error("saying the same thing twice was treated as a conflict")
	}
}

// --- tool-server configuration ----------------------------------------------

// The last create path a consumer could not route through this API: a session
// that needs tool servers. There was no field, so the only way to ask was to
// smuggle the flag through a pin — where the leading-dash guard catches it,
// correctly, and the caller falls back to driving the substrate directly.
func TestMcpConfigPathsReachTheAgentArgvOncePerEntry(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "base.json")
	two := filepath.Join(dir, "session.json")
	for _, p := range []string{one, two} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "tooled", Cwd: "/work/x",
		McpConfig: []fleet.AbsolutePath{fleet.AbsolutePath(one), fleet.AbsolutePath(two)},
	})

	agent := agentArgv(argv)
	var got []string
	for i, a := range agent {
		if a == "--mcp-config" && i+1 < len(agent) {
			got = append(got, agent[i+1])
		}
	}
	if len(got) != 2 || got[0] != one || got[1] != two {
		t.Errorf("--mcp-config values = %v; the flag repeats once per entry, and a "+
			"joined list would reach the runtime as one filename containing a separator", got)
	}
}

// Paths, never content — the same rule contextRef follows, and for a sharper
// reason: these files commonly hold the credentials their servers authenticate
// with, so inline content would put a secret in an argv every process table can
// read.
func TestMcpConfigContentNeverReachesACommandLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.json")
	const secret = "mcp-token-not-for-ps"
	if err := os.WriteFile(path, []byte(`{"token":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	f := twoSessions()
	createWith(t, f, fleet.SessionSpec{
		Name: "tooled", Cwd: "/work/x",
		McpConfig: []fleet.AbsolutePath{fleet.AbsolutePath(path)},
	})
	for _, c := range f.callsSnapshot() {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Fatalf("configuration CONTENT reached a command line: %v", c)
			}
		}
	}
}

// Refuse the create rather than start a session that comes up healthy-looking
// and missing the tools it was created for — the same refusal env already makes,
// for the same reason.
func TestUnreadableMcpConfigIsRefusedRatherThanStarted(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-m", fleet.SessionSpec{
		Name: "tooled", Cwd: "/work/x",
		McpConfig: []fleet.AbsolutePath{"/no/such/servers.json"},
	})
	if err == nil {
		t.Fatal("a configuration this machine cannot read was accepted")
	}
	if newSessionArgv(f) != nil {
		t.Error("the session was started anyway; the refusal must happen before the spawn")
	}
}

func TestRelativeAndFlagShapedMcpConfigAreRefused(t *testing.T) {
	for _, path := range []fleet.AbsolutePath{"relative/servers.json", "--dangerously-skip-permissions", ""} {
		f := twoSessions()
		d := newTestDriver(f)
		_, err := d.Create(context.Background(), testCaller, "k-m", fleet.SessionSpec{
			Name: "tooled", Cwd: "/work/x", McpConfig: []fleet.AbsolutePath{path},
		})
		if err == nil {
			t.Errorf("mcpConfig %q was accepted", path)
		}
	}
}
