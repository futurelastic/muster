package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #371: contextRef is honoured as the session's system context. The file
// is read when the session is created and its text rides on every prompt as the
// runtime's own system field, never in the user part.

const briefText = "You are the reviewer for this repo.\nEnd every reply with the word PINEAPPLE.\n"

// writeBrief writes a context file and returns its absolute path.
func writeBrief(t *testing.T, content string) fleet.AbsolutePath {
	t.Helper()
	p := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return fleet.AbsolutePath(p)
}

func createWith(t *testing.T, d *Driver, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	t.Helper()
	if spec.Cwd == "" {
		spec.Cwd = "/work/x"
	}
	return d.Create(context.Background(), testReq, key, spec)
}

func send(t *testing.T, d *Driver, ref fleet.SessionRef, text string) {
	t.Helper()
	if _, err := d.Send(context.Background(), testReq, ref, text, driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("Send(%q): %v", text, err)
	}
}

func TestContextRef_RidesOnTheOpeningPromptAndOnEveryLaterOne(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	sess, err := createWith(t, d, "k1", fleet.SessionSpec{
		Name: "reviewer", Prompt: "review PR 12", ContextRef: writeBrief(t, briefText),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	send(t, d, sess.SessionRef, "now PR 13")

	got := f.promptsFor(sess.ID)
	if len(got) != 2 {
		t.Fatalf("the runtime received %d prompts, want 2 (the opening one and the Send)", len(got))
	}
	for i, want := range []string{"review PR 12", "now PR 13"} {
		p := got[i]
		if !p.hasSystem || p.system != briefText {
			t.Errorf("prompt %d: system = %q (present %v), want the file's text exactly", i, p.system, p.hasSystem)
		}
		if len(p.parts) != 1 || p.parts[0] != want {
			t.Errorf("prompt %d: parts = %q, want only the prompt %q: the context is not a turn the user typed", i, p.parts, want)
		}
		if strings.Contains(strings.Join(p.parts, "\n"), "PINEAPPLE") {
			t.Errorf("prompt %d: the context leaked into the user part", i)
		}
	}
}

// The agent pin (muster #362) and the context (muster #371) are both per-prompt
// fields on the same call; neither may displace the other.
func TestContextRef_AndAnAgentPinRideTogetherOnEveryPrompt(t *testing.T) {
	f := newFakeServer(t)
	f.agents = []string{"build", "coder"}
	d := newTestDriver(t, f)
	sess, err := createWith(t, d, "k1", fleet.SessionSpec{
		Name: "both", Agent: "coder", Prompt: "first", ContextRef: writeBrief(t, briefText),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	send(t, d, sess.SessionRef, "second")

	got := f.promptsFor(sess.ID)
	if len(got) != 2 {
		t.Fatalf("the runtime received %d prompts, want 2", len(got))
	}
	for i, p := range got {
		if !p.hasAgent || p.agent != "coder" {
			t.Errorf("prompt %d: agent = %q (present %v), want coder", i, p.agent, p.hasAgent)
		}
		if !p.hasSystem || p.system != briefText {
			t.Errorf("prompt %d: system = %q (present %v), want the file's text", i, p.system, p.hasSystem)
		}
	}
}

func TestContextRef_AbsentMeansNoSystemField(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	sess, err := createWith(t, d, "k1", fleet.SessionSpec{Name: "plain", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	send(t, d, sess.SessionRef, "again")
	for i, p := range f.promptsFor(sess.ID) {
		if p.hasSystem {
			t.Errorf("prompt %d carried a system field %q though the create named no context", i, p.system)
		}
	}
}

// The file is read at create and kept; later edits to it do not change a running
// session, which is what an appended system prompt does too.
func TestContextRef_IsReadOnceAtCreate(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := writeBrief(t, briefText)
	sess, err := createWith(t, d, "k1", fleet.SessionSpec{Name: "r", ContextRef: ref})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(string(ref), []byte("changed after create"), 0o600); err != nil {
		t.Fatal(err)
	}
	send(t, d, sess.SessionRef, "go")
	got := f.promptsFor(sess.ID)
	if len(got) != 1 || got[0].system != briefText {
		t.Fatalf("prompts = %+v, want the text the file held at create", got)
	}
}

func TestContextRef_AnEmptyFileIsNoContext(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	sess, err := createWith(t, d, "k1", fleet.SessionSpec{Name: "r", Prompt: "hi", ContextRef: writeBrief(t, "")})
	if err != nil {
		t.Fatalf("Create with an empty context file: %v", err)
	}
	if got := f.promptsFor(sess.ID); len(got) != 1 || got[0].hasSystem {
		t.Errorf("prompts = %+v, want one with no system field", got)
	}
}

// Every refusal is `invalid`, and it comes before any process is started: the
// isolated driver would leave a session directory behind otherwise.
func TestContextRef_UnusableFilesAreRefusedInvalidBeforeAnythingStarts(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.md")
	if err := os.WriteFile(big, bytes.Repeat([]byte("a"), maxContextBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "binary.bin")
	if err := os.WriteFile(binary, []byte{'o', 'k', 0xff, 0xfe, 'x'}, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, ref, mention string
	}{
		{"relative", "briefs/brief.md", "absolute"},
		{"missing", filepath.Join(dir, "nope.md"), "cannot be read"},
		{"a directory", dir, "regular file"},
		{"over the cap", big, "larger than"},
		{"not UTF-8", binary, "UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, root := newIsolatedDriver(t)
			_, err := d.Create(context.Background(), testReq, "k", fleet.SessionSpec{
				Cwd: fleet.AbsolutePath(t.TempDir()), Name: "r", Prompt: "hi", ContextRef: fleet.AbsolutePath(tc.ref),
			})
			if kindOfErr(err) != fleet.ErrorInvalid {
				t.Fatalf("Create: %v; want an invalid refusal", err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not say why (%q)", err, tc.mention)
			}
			if left, _ := os.ReadDir(root); len(left) != 0 {
				t.Errorf("a refused create left %d entries in the session root: a process was started for it", len(left))
			}
			if got := len(listed(t, d)); got != 0 {
				t.Errorf("%d sessions listed after a refused create", got)
			}
		})
	}
}

func TestContextRef_ARefusedCreateMakesNoCallToTheRuntime(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	before := len(f.requests)
	_, err := createWith(t, d, "k", fleet.SessionSpec{Name: "r", ContextRef: "relative.md"})
	if kindOfErr(err) != fleet.ErrorInvalid {
		t.Fatalf("Create: %v; want invalid", err)
	}
	if after := len(f.requests); after != before {
		t.Errorf("a refused create made %d calls to the runtime, want 0", after-before)
	}
}

func TestContextRef_TheRefusalNeverQuotesTheFilesContent(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	secret := "sekrit-brief-line-5e1b"
	p := filepath.Join(t.TempDir(), "b.bin")
	if err := os.WriteFile(p, append([]byte(secret), 0xff), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := d.Create(context.Background(), testReq, "k", fleet.SessionSpec{
		Cwd: fleet.AbsolutePath(t.TempDir()), Name: "r", ContextRef: fleet.AbsolutePath(p),
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("error = %v; want a refusal that does not repeat the file's content", err)
	}
}

// What a session record stores is the path. The brief itself is not copied into
// the state directory.
func TestContextRef_TheRecordHoldsThePathNotTheText(t *testing.T) {
	state := t.TempDir()
	d := newStateDriver(t, state)
	t.Cleanup(func() { _ = d.Shutdown() })
	text := "brief-text-d41d8cd98f00b204 do not store me"
	ref := writeBrief(t, text)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "r", Prompt: "hi", ContextRef: ref})

	raw, err := os.ReadFile(filepath.Join(state, "opencode", "sessions", sess.ID+".json"))
	if err != nil {
		t.Fatalf("no record: %v", err)
	}
	// Decoded rather than searched: a Windows path is stored with its backslashes
	// escaped, so the raw bytes never contain it verbatim.
	var rec sessionRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("the record is not JSON: %v", err)
	}
	if rec.ContextRef != string(ref) {
		t.Errorf("record ContextRef = %q, want the context file's path %q", rec.ContextRef, ref)
	}
	if strings.Contains(string(raw), "d41d8cd98f00b204") {
		t.Error("the record holds the brief's text")
	}
}

// promptsOnDisk reads the prompts the stand-in runtime received, from the file it
// keeps in the session's data directory (fakeopencode_test.go).
func promptsOnDisk(t *testing.T, d *Driver, id string) []recordedPrompt {
	t.Helper()
	data := inside(t, d, id).vars()["XDG_DATA_HOME"]
	raw, err := os.ReadFile(filepath.Join(data, "fake-prompts.jsonl"))
	if err != nil {
		t.Fatalf("no prompts recorded: %v", err)
	}
	var out []recordedPrompt
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		out = append(out, decodePrompt(id, []byte(line)))
	}
	return out
}

func TestContextRef_ARealProcessIsSentItOnTheWire(t *testing.T) {
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Name: "r", Prompt: "first", ContextRef: writeBrief(t, briefText)})
	send(t, d, sess.SessionRef, "second")
	got := promptsOnDisk(t, d, sess.ID)
	if len(got) != 2 {
		t.Fatalf("%d prompts reached the process, want 2", len(got))
	}
	for i, p := range got {
		if p.system != briefText || len(p.parts) != 1 {
			t.Errorf("prompt %d = %+v, want the brief as system and one user part", i, p)
		}
	}
}

// A restart reads the file again: the session is relaunched with the context the
// file holds NOW, and every later prompt carries it.
func TestContextRef_ARestartReadsTheFileAgain(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	ref := writeBrief(t, briefText)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "r", ContextRef: ref})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	const revised = "Revised brief: answer in French."
	if err := os.WriteFile(string(ref), []byte(revised), 0o600); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status != fleet.StatusIdle {
		t.Fatalf("listed = %+v, %v; want the session idle and readable after the restart", got.State, ok)
	}
	send(t, d2, got.SessionRef, "after the restart")
	prompts := promptsOnDisk(t, d2, sess.ID)
	last := prompts[len(prompts)-1]
	if last.system != revised {
		t.Errorf("system after the restart = %q, want the file's current text %q", last.system, revised)
	}
}

// A context file that has gone is not a reason to relaunch the session without
// its brief: it is listed unknown with the reason and no process is started.
func TestContextRef_AGoneFileMeansUnknownNotARelaunchWithoutIt(t *testing.T) {
	state := t.TempDir()
	d1 := newStateDriver(t, state)
	ref := writeBrief(t, briefText)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Name: "r", ContextRef: ref})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(string(ref)); err != nil {
		t.Fatal(err)
	}

	d2 := newStateDriver(t, state)
	t.Cleanup(func() { _ = d2.Shutdown() })
	got, ok := listed(t, d2)[sess.ID]
	if !ok {
		t.Fatal("the session is not listed after the restart: a restart adopts, it does not drop")
	}
	if got.State.Status != fleet.StatusUnknown {
		t.Fatalf("status = %q; want unknown", got.State.Status)
	}
	if !strings.Contains(got.State.Evidence, "context file") || !strings.Contains(got.State.Evidence, "re-creation") {
		t.Errorf("reason = %q; want it to name the context file and say the session needs re-creation", got.State.Evidence)
	}
	if k, _ := d2.wasSeen(sess.ID); k.srv != nil {
		t.Error("a process was started for a session that cannot keep its context")
	}
	if _, err := d2.Send(context.Background(), testReq, got.SessionRef, "hi", driver.SendOptions{Submit: true}); kindOfErr(err) != fleet.ErrorConflict {
		t.Errorf("Send to it: %v; want a conflict naming the reason", err)
	}
}
