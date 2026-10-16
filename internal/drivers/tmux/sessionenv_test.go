package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// --- ValidateSessionEnv: a typo is a message an operator reads once -------

func TestValidateSessionEnvCatchesEachShapeAtStartup(t *testing.T) {
	cases := map[string]struct {
		entries []SessionEnvEntry
		wantErr bool
	}{
		"good entry passes": {
			entries: []SessionEnvEntry{{Name: "FLEET_IDENTITY", FromFile: "/etc/fleet/identity"}},
		},
		"empty name is refused": {
			entries: []SessionEnvEntry{{Name: "", FromFile: "/etc/fleet/identity"}},
			wantErr: true,
		},
		"name with a dash is refused": {
			entries: []SessionEnvEntry{{Name: "NOT-A-NAME", FromFile: "/etc/fleet/identity"}},
			wantErr: true,
		},
		"name starting with a digit is refused": {
			entries: []SessionEnvEntry{{Name: "1ST", FromFile: "/etc/fleet/identity"}},
			wantErr: true,
		},
		"missing fromFile is refused": {
			entries: []SessionEnvEntry{{Name: "FLEET_IDENTITY", FromFile: ""}},
			wantErr: true,
		},
		"relative fromFile is refused": {
			entries: []SessionEnvEntry{{Name: "FLEET_IDENTITY", FromFile: "relative/path"}},
			wantErr: true,
		},
		"the same name declared twice is refused": {
			entries: []SessionEnvEntry{
				{Name: "FLEET_IDENTITY", FromFile: "/etc/fleet/a"},
				{Name: "FLEET_IDENTITY", FromFile: "/etc/fleet/b"},
			},
			wantErr: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateSessionEnv(tc.entries)
			if tc.wantErr && err == nil {
				t.Fatal("ValidateSessionEnv accepted a shape it should have refused")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateSessionEnv refused a good entry: %v", err)
			}
		})
	}
}

// --- provisionSessionEnv: the precedence table from muster issue #94 -

// A required entry the caller never mentioned is filled in from
// configuration — the whole point of the feature.
func TestRequiredEntrySilentCallerGetsTheConfiguredValue(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "the-identity\n")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: credFile, Required: true},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if got["FLEET_IDENTITY"] != "the-identity" {
		t.Errorf("FLEET_IDENTITY = %q, want the value read from the configured file", got["FLEET_IDENTITY"])
	}
}

// The oracle, pinned shut: a caller supplying the CORRECT value for a
// required entry must still be refused, exactly like a caller supplying a
// wrong one. An earlier revision let an identical value proceed, on the
// reasoning that agreement is not a conflict — but that let a caller holding
// only the create grant use the create endpoint to confirm a candidate value
// for a secret it has no read access to (the create succeeds only when the
// guess is right). This test is the one that would catch a future
// well-meaning person re-adding that convenience: it must fail the moment
// "matching proceeds" comes back, however it is spelled.
func TestRequiredEntryEvenTheCorrectCallerValueIsRefused(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "the-identity")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: credFile, Required: true},
	}))
	_, err := d.provisionSessionEnv(fleet.SessionSpec{
		Cwd: "/work/x", Env: map[string]string{"FLEET_IDENTITY": "the-identity"},
	}, "")
	if err == nil {
		t.Fatal("a caller supplying the CORRECT value for a required entry was accepted; " +
			"this is the equality oracle the create endpoint must not offer — see the issue thread")
	}
	// Caller-correctable (omit the field), so this must NOT be the
	// operator-addressed kind the missing-file case uses below.
	var fe *fleet.Error
	if errors.As(err, &fe) {
		t.Errorf("a caller-supplied value for a required entry was reported as a typed %s "+
			"error; it should fall through to the ordinary invalid/400 path", fe.Kind)
	}
	// The refusal must not betray whether the value matched — no comparison
	// happened at all, so there is nothing to leak, but the message itself
	// must not claim otherwise.
	if strings.Contains(err.Error(), "the-identity") {
		t.Errorf("the refusal message quoted the caller's value: %v", err)
	}
}

// A caller asking for a DIFFERENT identity than the one this machine
// requires must be refused too — same refusal, same reason: a required
// entry does not compare a caller's value against the configured one at all,
// so "different" and "identical" answer identically. See
// TestRequiredEntryEvenTheCorrectCallerValueIsRefused for why "identical
// proceeds" was removed.
func TestRequiredEntryConflictingCallerValueIsRefused(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "the-identity")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: credFile, Required: true},
	}))
	_, err := d.provisionSessionEnv(fleet.SessionSpec{
		Cwd: "/work/x", Env: map[string]string{"FLEET_IDENTITY": "something-else"},
	}, "")
	if err == nil {
		t.Fatal("a caller-supplied value for a required entry was accepted")
	}
	// The caller can fix this by omitting the field (never by matching — see
	// above), so it must NOT be the operator-addressed kind the missing-file
	// case uses below.
	var fe *fleet.Error
	if errors.As(err, &fe) {
		t.Errorf("a caller-correctable conflict was reported as a typed %s error; "+
			"it should fall through to the ordinary invalid/400 path", fe.Kind)
	}
}

// A required entry must refuse a caller-supplied value WITHOUT reading the
// configured file at all — not merely without comparing after reading it.
// Pointing FromFile at a path that does not exist and getting the ordinary
// caller-correctable refusal (not the operator-addressed missing-file kind)
// proves the file was never opened: if it had been, entry.Required's
// missing-file branch would have returned the unsupported/501 error instead.
func TestRequiredEntryRefusesACallerValueBeforeReadingTheFile(t *testing.T) {
	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: "/no/such/credential/file", Required: true},
	}))
	_, err := d.provisionSessionEnv(fleet.SessionSpec{
		Cwd: "/work/x", Env: map[string]string{"FLEET_IDENTITY": "anything"},
	}, "")
	if err == nil {
		t.Fatal("a caller-supplied value for a required entry was accepted")
	}
	var fe *fleet.Error
	if errors.As(err, &fe) {
		t.Fatalf("got the missing-file kind (%s); the file must never be read when the "+
			"caller supplied a value for a required entry", fe.Kind)
	}
}

// The machine-side failure: a required entry whose file the operator has
// not put in place. No correction to the request body fixes this, so it
// must not answer the caller-fixable "invalid" kind the conflict case
// above uses — see sessionenv.go's provisionSessionEnv doc comment.
func TestRequiredEntryWithMissingFileRefusesAsUnsupportedNotInvalid(t *testing.T) {
	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: "/no/such/credential/file", Required: true},
	}))
	_, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err == nil {
		t.Fatal("a required entry with no backing file was accepted")
	}
	var fe *fleet.Error
	if !errors.As(err, &fe) {
		t.Fatalf("error is not a *fleet.Error: %v", err)
	}
	if fe.Kind != fleet.ErrorUnsupported {
		t.Errorf("Kind = %q, want %q — retrying this create cannot fix an absent file; "+
			"only an operator can", fe.Kind, fleet.ErrorUnsupported)
	}
}

// An empty file is exactly as unusable as a missing one, and must refuse
// the same way — an empty credential is not a value, it is the absence of
// one wearing a file that happens to exist.
func TestRequiredEntryWithEmptyFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: credFile, Required: true},
	}))
	_, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err == nil {
		t.Fatal("an empty required credential file was accepted as though it held a value")
	}
}

// A NON-required entry with no backing file is a legitimate, silent
// no-op — required is what makes the missing-file case above a refusal at
// all, and both answers are meant to coexist on the same machine.
func TestNonRequiredEntryWithMissingFileIsSilentlyOmitted(t *testing.T) {
	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_NICE_TO_HAVE", FromFile: "/no/such/file", Required: false},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err != nil {
		t.Fatalf("a non-required entry's missing file caused a refusal: %v", err)
	}
	if _, ok := got["FLEET_NICE_TO_HAVE"]; ok {
		t.Error("a variable this machine could not back was delivered anyway")
	}
}

// A non-required entry never overrides a caller who supplied their own
// value — "the caller wins" is the whole point of leaving it optional.
func TestNonRequiredEntryCallerValueWins(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "machine-default")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_NICE_TO_HAVE", FromFile: credFile, Required: false},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{
		Cwd: "/work/x", Env: map[string]string{"FLEET_NICE_TO_HAVE": "caller-value"},
	}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if got["FLEET_NICE_TO_HAVE"] != "caller-value" {
		t.Errorf("FLEET_NICE_TO_HAVE = %q, want the caller's own value to win", got["FLEET_NICE_TO_HAVE"])
	}
}

// No sessionEnv configured at all must be a complete no-op — the same
// off-by-default contract WithRecordRoot, WithCredentialPath and
// WithTrustSeed already keep, so a driver built for a test never merges
// anything merely because it was constructed.
func TestUnconfiguredSessionEnvLeavesCallerEnvUntouched(t *testing.T) {
	d := New("testbox")
	in := map[string]string{"A": "b"}
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x", Env: in}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if len(got) != 1 || got["A"] != "b" {
		t.Errorf("got = %v, want the caller's env unchanged", got)
	}
}

// --- appliesTo: the escape hatch that keeps an exception a config edit ----

func TestAppliesToExcludesASessionOutsideItsScope(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "machine-identity")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{
			Name: "FLEET_IDENTITY", FromFile: credFile, Required: true,
			AppliesTo: SessionEnvScope{Agents: []string{"sid"}},
		},
	}))
	// This session names a different agent, so it is out of scope: the
	// entry must not apply, and — the point of the escape hatch — a
	// required entry outside its own scope must not refuse the create
	// either.
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x", Agent: "alex"}, "")
	if err != nil {
		t.Fatalf("an out-of-scope session was refused for a requirement that does not apply to it: %v", err)
	}
	if _, ok := got["FLEET_IDENTITY"]; ok {
		t.Error("an entry scoped to a different agent was applied anyway")
	}
}

func TestAppliesToIncludesASessionMatchingItsScope(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "machine-identity")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{
			Name: "FLEET_IDENTITY", FromFile: credFile, Required: true,
			AppliesTo: SessionEnvScope{Agents: []string{"sid"}},
		},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x", Agent: "sid"}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if got["FLEET_IDENTITY"] != "machine-identity" {
		t.Errorf("FLEET_IDENTITY = %q, want it applied for the matching agent", got["FLEET_IDENTITY"])
	}
}

// A marker matches exactly like an agent — the issue states appliesTo
// "matches on agent name or marker", not on agent alone.
func TestAppliesToMatchesOnMarkerToo(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "machine-identity")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{
			Name: "FLEET_IDENTITY", FromFile: credFile, Required: true,
			AppliesTo: SessionEnvScope{Markers: []string{"filing"}},
		},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x", Marker: "filing"}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if got["FLEET_IDENTITY"] != "machine-identity" {
		t.Errorf("FLEET_IDENTITY = %q, want it applied for the matching marker", got["FLEET_IDENTITY"])
	}
}

// A scope naming neither axis is the common case — an operator declaring an
// identity for the whole machine, not carving out one agent.
func TestAnEmptyScopeMatchesEverySession(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "machine-identity")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: credFile, Required: true},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x", Agent: "anything-at-all"}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if got["FLEET_IDENTITY"] != "machine-identity" {
		t.Error("an entry with no appliesTo was not applied to an ordinary session")
	}
}

// --- appliesTo.runtimes: a credential that belongs to one runtime (#294) --

// An entry scoped to this driver's runtime is provisioned for its sessions —
// and it is the driver's own id that is matched, not the create's hint: the
// spec here names no runtime, which is what a caller taking the default sends.
func TestAppliesToRuntimesDeliversToTheScopedRuntimesSessions(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "machine-identity")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{
			Name: "FLEET_IDENTITY", FromFile: credFile, Required: true,
			AppliesTo: SessionEnvScope{Runtimes: []string{string(DefaultRuntime)}},
		},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if got["FLEET_IDENTITY"] != "machine-identity" {
		t.Errorf("FLEET_IDENTITY = %q, want it delivered to a session on the scoped runtime", got["FLEET_IDENTITY"])
	}
}

// An entry scoped to another runtime is absent from this driver's sessions,
// and — as with the other axes — a required entry outside its scope neither
// refuses the create nor refuses a caller's own value for the name.
func TestAppliesToRuntimesWithholdsFromAnotherRuntimesSessions(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "machine-identity")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{
			Name: "FLEET_IDENTITY", FromFile: credFile, Required: true,
			AppliesTo: SessionEnvScope{Runtimes: []string{"opencode"}},
		},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err != nil {
		t.Fatalf("a requirement scoped to another runtime refused this one's create: %v", err)
	}
	if _, ok := got["FLEET_IDENTITY"]; ok {
		t.Error("an entry scoped to another runtime reached this runtime's session")
	}
	// A caller hint naming the other runtime does not pull the credential in.
	got, err = d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x", Runtime: "opencode"}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if _, ok := got["FLEET_IDENTITY"]; ok {
		t.Error("a caller's runtime hint delivered a credential scoped to that runtime to another driver's session")
	}
}

// --- the staging-format bound, attributed to the right party -------------

// A configured value the staging format cannot carry is this machine's
// problem, caught before it ever reaches validateEnv's later pass over the
// merged map — see readSessionEnvFile's doc comment for why folding it into
// that later pass would misattribute an operator's bad file to the caller.
func TestConfiguredValueViolatingTheStagingBoundIsRefused(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "line-one\nFAKE=injected")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: credFile, Required: true},
	}))
	_, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err == nil {
		t.Fatal("a configured value containing a newline was accepted; the staging " +
			"format would have turned it into a second, fabricated variable")
	}
}

// A trailing newline is the ordinary shape of a file written by `echo` or an
// editor, so it is trimmed rather than treated as a bound violation — only
// an EMBEDDED newline is the fabrication hazard.
func TestATrailingNewlineInTheCredentialFileIsTrimmed(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "clean-value\n")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_IDENTITY", FromFile: credFile, Required: true},
	}))
	got, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	if got["FLEET_IDENTITY"] != "clean-value" {
		t.Errorf("FLEET_IDENTITY = %q, want the trailing newline trimmed", got["FLEET_IDENTITY"])
	}
}

// --- Create(): the bareExec refusal catches a configured value too -------

// bareExec has no login shell and therefore no out-of-band channel for env
// at all — the same refusal a caller's own Env already gets must also catch
// a value sessionEnv contributed, or a bareExec driver would silently drop
// exactly the identity this feature exists to guarantee.
func TestBareExecRefusesACreateWhoseEnvCameFromSessionEnv(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "machine-identity")

	f := twoSessions()
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		WithBareExec(),
		WithSessionEnv([]SessionEnvEntry{
			{Name: "FLEET_IDENTITY", FromFile: credFile, Required: true},
		}))
	_, err := d.Create(context.Background(), testCaller, "k-bare-sessionenv",
		fleet.SessionSpec{Name: "e", Cwd: "/work/x"})
	if err == nil {
		t.Fatal("a create was accepted on a bareExec driver even though sessionEnv had " +
			"values to deliver and no channel to deliver them through")
	}
}

// --- end to end, without a real tmux: the same pattern environment_test.go
// already uses to prove the staging/consume/unlink contract -------------

// This is the join nobody had proven: not "env delivery works" and not "the
// credential is valid" separately, but that a value THIS DRIVER READ FROM
// CONFIGURATION reaches a process through the exact real mechanism a caller's
// own Env already goes through — staged to a file, applied by a real shell,
// unlinked before the agent runs.
func TestASessionEnvProvisionedValueReachesTheProcessThroughTheRealWrapper(t *testing.T) {
	sh := shellForTest(t)
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred")
	writeFile(t, credFile, "example-identity\n")

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_TEST_IDENTITY", FromFile: credFile, Required: true},
	}))

	merged, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, "")
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	envPath, err := d.stageEnv(merged)
	if err != nil {
		t.Fatalf("stageEnv: %v", err)
	}

	record := filepath.Join(dir, "rec")
	out, err := exec.Command(sh, "-c", envRecordScript, "muster", record, envPath, "",
		"/bin/sh", "-c", `printf '%s' "$FLEET_TEST_IDENTITY"`).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v (%s)", err, out)
	}
	if string(out) != "example-identity" {
		t.Errorf("the process received %q, want the value this driver read from configuration", out)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Error("the staged file outlived its read; a configuration-sourced credential must " +
			"not survive on disk any more than a caller-supplied one does")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// #326: an entry may export the created session's own identity. Driven through
// the real wrapper like the file-sourced test above, so "byte for byte" covers
// the staging format too — the id carries a non-ASCII marker.
func TestASessionEnvSessionIdentityReachesTheProcessThroughTheRealWrapper(t *testing.T) {
	sh := shellForTest(t)
	dir := t.TempDir()
	const id = "review-work\U0001F4AC"

	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_TEST_REF", FromSession: "ref", Required: true},
		{Name: "FLEET_TEST_ID", FromSession: "id"},
	}))
	merged, err := d.provisionSessionEnv(fleet.SessionSpec{Cwd: "/work/x"}, id)
	if err != nil {
		t.Fatalf("provisionSessionEnv: %v", err)
	}
	envPath, err := d.stageEnv(merged)
	if err != nil {
		t.Fatalf("stageEnv: %v", err)
	}
	record := filepath.Join(dir, "rec")
	out, err := exec.Command(sh, "-c", envRecordScript, "muster", record, envPath, "",
		"/bin/sh", "-c", `printf '%s|%s' "$FLEET_TEST_REF" "$FLEET_TEST_ID"`).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v (%s)", err, out)
	}
	if want := "testbox/" + id + "|" + id; string(out) != want {
		t.Errorf("the process received %q, want %q", out, want)
	}
}

// A required entry exporting the session's identity refuses a caller-supplied
// value exactly like a file-sourced one: the caller cannot name the session.
func TestSessionIdentityEntryRefusesACallerValueWhenRequired(t *testing.T) {
	d := New("testbox", WithSessionEnv([]SessionEnvEntry{
		{Name: "FLEET_TEST_REF", FromSession: "ref", Required: true},
	}))
	_, err := d.provisionSessionEnv(fleet.SessionSpec{
		Cwd: "/work/x", Env: map[string]string{"FLEET_TEST_REF": "forged"},
	}, "s1")
	if err == nil {
		t.Fatal("a caller-supplied value for a required session-identity entry was accepted")
	}
}
