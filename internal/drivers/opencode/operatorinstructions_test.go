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

// muster #504: instruction files the operator names reach every isolated session.
// The reading and its refusals are unit-tested on their own; what a session is told
// is tested through the stand-in runtime (fakeopencode_test.go), which, like the
// real one, reads the files its own opencode.json names when a prompt arrives. The
// gated integration test pins the same against the real runtime.

// writeInstructionFile writes one operator instruction file and returns its path.
func writeInstructionFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// instructionsOf is what a session's stand-in runtime read from the files its own
// configuration names, once per prompt, oldest first.
func instructionsOf(t *testing.T, d *Driver, id string) [][]string {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok || known.srv == nil {
		t.Fatalf("session %s is not running here", id)
	}
	raw, err := os.ReadFile(filepath.Join(known.srv.dir, "xdg", "data", "fake-instructions.jsonl"))
	if err != nil {
		t.Fatalf("reading what the session was told: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var texts []string
		if err := json.Unmarshal([]byte(line), &texts); err != nil {
			t.Fatalf("a record is not a JSON array: %v", err)
		}
		out = append(out, texts)
	}
	return out
}

func instructionPathsIn(cfg map[string]any) []string {
	list, _ := cfg["instructions"].([]any)
	var out []string
	for _, v := range list {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func sendGo(t *testing.T, d *Driver, ref fleet.SessionRef, text string) {
	t.Helper()
	if _, err := d.Send(context.Background(), testReq, ref, text, driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestOperatorInstructions_AFileIsReadAsWritten(t *testing.T) {
	const body = "Always answer in English.\n\n- Never edit generated files.\n"
	got, err := readOperatorInstruction(writeInstructionFile(t, "rules.md", body))
	if err != nil {
		t.Fatalf("readOperatorInstruction: %v", err)
	}
	if string(got.text) != body {
		t.Errorf("text = %q, want the file exactly as written", got.text)
	}
}

func TestOperatorInstructions_AFileThatIsNotUsableIsRefusedWithTheReason(t *testing.T) {
	const secret = "SECRET-RULE-TEXT-do-not-quote-me"
	for _, tc := range []struct {
		name   string
		path   func(t *testing.T) string
		reason string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone.md") }, "cannot be read"},
		{"a directory", func(t *testing.T) string { return t.TempDir() }, "a directory"},
		{"empty", func(t *testing.T) string { return writeInstructionFile(t, "rules.md", "") }, "is empty"},
		{"only whitespace", func(t *testing.T) string { return writeInstructionFile(t, "rules.md", " \n\t\n") }, "is empty"},
		{"over the bound", func(t *testing.T) string {
			return writeInstructionFile(t, "rules.md", secret+strings.Repeat("x", operatorInstructionsMaxBytes))
		}, "larger than"},
		{"not text", func(t *testing.T) string {
			return writeInstructionFile(t, "rules.md", secret+"\xff\xfe\x00binary")
		}, "not UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readOperatorInstruction(tc.path(t))
			if err == nil {
				t.Fatal("readOperatorInstruction succeeded; want a refusal")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error = %q, want it to say %q", err, tc.reason)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error = %q quotes the file's content", err)
			}
		})
	}
}

func TestOperatorInstructions_ACopyIsNamedSafely(t *testing.T) {
	for _, tc := range []struct {
		index  int
		source string
		want   string
	}{
		{0, "/etc/rules.md", "01-rules.md"},
		{1, "/etc/rules.md", "02-rules.md"}, // the same name twice stays apart
		{2, "/etc/rules[1]*.md", "03-rules_1__.md"},
		{0, "/etc/.hidden", "01-hidden"},
		{0, "/etc/..", "01-instructions"},
		{0, "/etc/régles.md", "01-r_gles.md"},
		{0, "/etc/" + strings.Repeat("a", 200), "01-" + strings.Repeat("a", 64)},
	} {
		if got := copyName(tc.index, tc.source); got != tc.want {
			t.Errorf("copyName(%d, %q) = %q, want %q", tc.index, tc.source, got, tc.want)
		}
	}
}

func TestOperatorInstructions_TheCopiesFollowTheOperatorsOrderAndNothingStaysBehind(t *testing.T) {
	dir := t.TempDir()
	files := []operatorInstruction{
		{source: "/etc/b.md", text: []byte("second alphabetically, first in the setting")},
		{source: "/etc/a.md", text: []byte("first alphabetically, second in the setting")},
	}
	paths, err := writeOperatorInstructions(dir, files)
	if err != nil {
		t.Fatalf("writeOperatorInstructions: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want two", paths)
	}
	for i, p := range paths {
		if !strings.HasPrefix(p, filepath.Join(dir, operatorInstructionsDir)+string(filepath.Separator)) {
			t.Errorf("copy %d is at %q, want it inside the session's own directory", i, p)
		}
		got, err := os.ReadFile(p)
		if err != nil || string(got) != string(files[i].text) {
			t.Errorf("copy %d = %q (%v), want the text read", i, got, err)
		}
	}
	if !strings.HasSuffix(paths[0], "01-b.md") || !strings.HasSuffix(paths[1], "02-a.md") {
		t.Errorf("paths = %v, want the operator's order kept in the names", paths)
	}

	// A relaunch with fewer files, or none, leaves nothing the setting no longer names.
	if paths, err = writeOperatorInstructions(dir, files[:1]); err != nil || len(paths) != 1 {
		t.Fatalf("second write = %v, %v", paths, err)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, operatorInstructionsDir)); len(left) != 1 {
		t.Errorf("%d copies after a write of one file, want 1", len(left))
	}
	if paths, err = writeOperatorInstructions(dir, nil); err != nil || paths != nil {
		t.Fatalf("write of none = %v, %v", paths, err)
	}
	if _, err := os.Stat(filepath.Join(dir, operatorInstructionsDir)); !os.IsNotExist(err) {
		t.Errorf("the instruction directory is still there after a write of none: %v", err)
	}
}

func TestOperatorInstructions_TheConfigCarriesThemOnlyWhenThereAreSome(t *testing.T) {
	dir := t.TempDir()
	if err := writeSessionConfig(dir, false, nil, providerPolicy{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, permissionConfigRel))
	if strings.Contains(string(raw), `"instructions"`) {
		t.Errorf("a machine with no instruction files got an instructions entry:\n%s", raw)
	}

	agents, err := parseOperatorAgents([]byte(operatorAgentsFile))
	if err != nil {
		t.Fatal(err)
	}
	copies := []string{filepath.Join(dir, "instructions", "01-rules.md")}
	// Alongside a bypass session's posture, the provider policy and the operator's
	// agents, none of which moved.
	plan := &sandboxPlan{read: []string{"/work"}}
	if err := writeSessionConfig(dir, true, plan, providerPolicy{}, agents, copies); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	raw, _ = os.ReadFile(filepath.Join(dir, permissionConfigRel))
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := instructionPathsIn(cfg); len(got) != 1 || got[0] != copies[0] {
		t.Errorf("instructions in the config = %v, want %v", got, copies)
	}
	for _, key := range []string{"permission", "disabled_providers", "agent"} {
		if _, ok := cfg[key]; !ok {
			t.Errorf("the %q entry is gone once instruction files are written", key)
		}
	}
}

func TestOperatorInstructions_AnIsolatedSessionIsToldTheFilesText(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	first := writeInstructionFile(t, "rules.md", "RULES-ONE: answer in English.\n")
	second := writeInstructionFile(t, "more.md", "RULES-TWO: never edit generated files.\n")
	d, _ := newIsolatedDriver(t, WithOperatorInstructions([]string{first, second}))

	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Prompt: "go"})

	// The session's own configuration names copies inside its own directory, not the
	// operator's paths.
	known, _ := d.wasSeen(sess.ID)
	paths := instructionPathsIn(sessionConfigOf(t, d, sess.ID))
	if len(paths) != 2 {
		t.Fatalf("instructions in the session's config = %v, want two", paths)
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, known.srv.dir) {
			t.Errorf("instruction path %q is outside the session's directory %q", p, known.srv.dir)
		}
		if p == first || p == second {
			t.Errorf("the session was handed the operator's own path %q", p)
		}
	}
	// The runtime read both, in the operator's order.
	told := instructionsOf(t, d, sess.ID)
	if len(told) != 1 || len(told[0]) != 2 ||
		!strings.Contains(told[0][0], "RULES-ONE") || !strings.Contains(told[0][1], "RULES-TWO") {
		t.Errorf("the session was told %q, want both files, in order", told)
	}
}

func TestOperatorInstructions_AMachineWithNoneIsUnchanged(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Prompt: "go"})
	if _, ok := sessionConfigOf(t, d, sess.ID)["instructions"]; ok {
		t.Error("a session on a machine with no instruction files got an instructions entry")
	}
	known, _ := d.wasSeen(sess.ID)
	if _, err := os.Stat(filepath.Join(known.srv.dir, operatorInstructionsDir)); !os.IsNotExist(err) {
		t.Errorf("an instruction directory exists with nothing to put in it: %v", err)
	}
}

func TestOperatorInstructions_EditingAFileNeedsNoRestartAndARunningSessionKeepsWhatItStartedWith(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	file := writeInstructionFile(t, "rules.md", "RULES-V1\n")
	d, _ := newIsolatedDriver(t, WithOperatorInstructions([]string{file}))

	running := mustCreate(t, d, "k1", fleet.SessionSpec{Prompt: "go"})
	if err := os.WriteFile(file, []byte("RULES-V2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A session already running is told what it started with, on a later prompt too:
	// the runtime reads its own copy, not the operator's file.
	sendGo(t, d, running.SessionRef, "again")
	told := instructionsOf(t, d, running.ID)
	if len(told) != 2 || told[0][0] != "RULES-V1\n" || told[1][0] != "RULES-V1\n" {
		t.Errorf("the running session was told %q, want the first reading both times", told)
	}

	// A session created after the edit gets the file as it is then, with no restart.
	later := mustCreate(t, d, "k2", fleet.SessionSpec{Prompt: "go"})
	if told := instructionsOf(t, d, later.ID); len(told) != 1 || told[0][0] != "RULES-V2\n" {
		t.Errorf("the later session was told %q, want the edited file", told)
	}

	// Removing the file does not reach a session already running either.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	sendGo(t, d, running.SessionRef, "once more")
	if told := instructionsOf(t, d, running.ID); len(told) != 3 || told[2][0] != "RULES-V1\n" {
		t.Errorf("after the file was removed the running session was told %q, want its own copy still", told)
	}
}

func TestOperatorInstructions_AFileThatIsNotUsableRefusesTheCreateAndStartsNothing(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	good := writeInstructionFile(t, "good.md", "fine\n")
	for _, tc := range []struct {
		name  string
		files func(t *testing.T) []string
	}{
		{"missing", func(t *testing.T) []string { return []string{filepath.Join(t.TempDir(), "gone.md")} }},
		{"empty", func(t *testing.T) []string { return []string{writeInstructionFile(t, "e.md", "")} }},
		{"a directory", func(t *testing.T) []string { return []string{t.TempDir()} }},
		{"not text", func(t *testing.T) []string { return []string{writeInstructionFile(t, "b.md", "\xff\xfe")} }},
		// One bad file among good ones refuses the create: the session would run
		// without part of its rules.
		{"the second of two", func(t *testing.T) []string {
			return []string{good, filepath.Join(t.TempDir(), "gone.md")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, root := newIsolatedDriver(t, WithOperatorInstructions(tc.files(t)))
			_, err := d.Create(context.Background(), testReq, "k1",
				fleet.SessionSpec{Prompt: "go", Cwd: fleet.AbsolutePath(t.TempDir())})
			if kindOfErr(err) != fleet.ErrorUnsupported {
				t.Fatalf("Create = %v, want an unsupported refusal: the operator must repair the file", err)
			}
			if !strings.Contains(err.Error(), "operator") || !strings.Contains(err.Error(), "instruction file") {
				t.Errorf("refusal = %q, want it to say what is wrong and who repairs it", err)
			}
			if left, _ := os.ReadDir(root); len(left) != 0 {
				t.Errorf("a refused create left %d entries under the session root", len(left))
			}
		})
	}
}

func TestOperatorInstructions_AnUnreadableFileRefusesTheCreate(t *testing.T) {
	skipOnWindows(t, "a file mode does not make a file unreadable on windows")
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	file := writeInstructionFile(t, "rules.md", "RULES\n")
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	d, root := newIsolatedDriver(t, WithOperatorInstructions([]string{file}))
	_, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Prompt: "go", Cwd: fleet.AbsolutePath(t.TempDir())})
	if kindOfErr(err) != fleet.ErrorUnsupported || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("Create = %v, want an unsupported refusal saying the file cannot be read", err)
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Errorf("a refused create left %d entries under the session root", len(left))
	}
}

func TestOperatorInstructions_ARelaunchedSessionGetsTheFilesAsTheyAreThen(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	file := writeInstructionFile(t, "rules.md", "RULES-V1\n")
	state := t.TempDir()
	d1 := newStateDriver(t, state, WithOperatorInstructions([]string{file}))
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Prompt: "one"})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(file, []byte("RULES-V2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state, WithOperatorInstructions([]string{file}))
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status != fleet.StatusIdle {
		t.Fatalf("after the restart the session is %+v (listed %v), want it running again", got.State, ok)
	}
	sendGo(t, d2, got.SessionRef, "two")
	told := instructionsOf(t, d2, sess.ID)
	if len(told) == 0 || told[len(told)-1][0] != "RULES-V2\n" {
		t.Errorf("the relaunched session was told %q, want the file as it is now", told)
	}
}

func TestOperatorInstructions_ARelaunchWithAnUnusableFileLeavesTheSessionUnknownWithTheReason(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	file := writeInstructionFile(t, "rules.md", "RULES\n")
	state := t.TempDir()
	d1 := newStateDriver(t, state, WithOperatorInstructions([]string{file}))
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Prompt: "go"})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	d2 := newStateDriver(t, state, WithOperatorInstructions([]string{file}))
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok {
		t.Fatal("the session vanished instead of being listed with a reason")
	}
	if got.State.Status != fleet.StatusUnknown || !strings.Contains(got.State.Evidence, "instruction file") {
		t.Errorf("state = %q (%s), want unknown, with the file named as the reason", got.State.Status, got.State.Evidence)
	}
}
