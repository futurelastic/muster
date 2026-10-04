package fleet_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The publish job's network reads and its publish are one script so this file
// can run them (#261). A fake `gh` and `npm` on PATH replay a scripted list of
// answers, one per call, and count the calls — so a test states "the API fails
// twice and then answers" and then checks both the verdict and how many times
// the script asked.

// fakeTool writes an executable that, on its Nth call, prints replies[N-1]
// (last one repeating) and exits with the code in its first line. A reply is
// "<exit code>|<output>". Calls are counted in <dir>/<name>.count.
func fakeTool(t *testing.T, dir, name string, replies ...string) {
	t.Helper()
	var cases strings.Builder
	for i, r := range replies {
		code, out, _ := strings.Cut(r, "|")
		cases.WriteString("  " + strconv.Itoa(i+1) + ") printf '%s\\n' '" + out + "'; exit " + code + " ;;\n")
	}
	last := replies[len(replies)-1]
	code, out, _ := strings.Cut(last, "|")
	script := "#!/bin/sh\n" +
		"c=\"" + filepath.Join(dir, name+".count") + "\"\n" +
		"n=$(cat \"$c\" 2>/dev/null || echo 0); n=$((n+1)); echo $n > \"$c\"\n" +
		"echo \"$*\" >> \"" + filepath.Join(dir, name+".args") + "\"\n" +
		"case $n in\n" + cases.String() +
		"  *) printf '%s\\n' '" + out + "'; exit " + code + " ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func calls(t *testing.T, dir, name string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name+".count"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func runGuard(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"scripts/release-npm-guard.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "REPO=owner/repo", "RETRY_UNIT=0")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

const gh503 = "1|gh: No server is currently available to service your request. (HTTP 503)"

func TestVisibilityRetriesATransientErrorThenAcceptsPublic(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	fakeTool(t, dir, "gh", gh503, gh503, "0|false")
	out, code := runGuard(t, dir, "visibility")
	if code != 0 {
		t.Fatalf("a read that recovers must pass, exit %d:\n%s", code, out)
	}
	if n := calls(t, dir, "gh"); n != 3 {
		t.Errorf("gh called %d times, want 3", n)
	}
	if !strings.Contains(out, "HTTP 503)") || strings.Contains(out, "503503") {
		t.Errorf("each failed attempt must be logged with its status:\n%s", out)
	}
	if !strings.Contains(out, "transient, retried 2") {
		t.Errorf("log must say \"transient, retried 2\":\n%s", out)
	}
}

func TestVisibilityNeverRetriesARealPrivateAnswer(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	fakeTool(t, dir, "gh", "0|true", "0|false")
	out, code := runGuard(t, dir, "visibility")
	if code == 0 {
		t.Fatalf("private must fail:\n%s", out)
	}
	if n := calls(t, dir, "gh"); n != 1 {
		t.Errorf("a real answer must not be retried away: gh called %d times", n)
	}
	if !strings.Contains(out, "private:") || strings.Contains(out, "transient") {
		t.Errorf("log must say private, not transient:\n%s", out)
	}
}

func TestVisibilityFailsClosedOnAPersistentTransientError(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	fakeTool(t, dir, "gh", gh503)
	out, code := runGuard(t, dir, "visibility")
	if code == 0 {
		t.Fatalf("an unreadable visibility must be refused:\n%s", out)
	}
	if n := calls(t, dir, "gh"); n != 5 {
		t.Errorf("gh called %d times, want the bound of 5", n)
	}
	if !strings.Contains(out, "persistent error") || !strings.Contains(out, "kept failing") {
		t.Errorf("log must say persistent error:\n%s", out)
	}
}

func TestVisibilityDoesNotRetryAnAnswerRetryingCannotImprove(t *testing.T) {
	requireBash(t)
	for name, reply := range map[string]string{
		"404":         "1|gh: Not Found (HTTP 404)",
		"403":         "1|gh: Forbidden (HTTP 403)",
		"neither":     "0|maybe",
		"empty":       "0|",
		"public-ish":  "0|False",
		"private-str": "0|\"true\"",
	} {
		dir := t.TempDir()
		fakeTool(t, dir, "gh", reply)
		out, code := runGuard(t, dir, "visibility")
		if code == 0 {
			t.Errorf("%s: must fail closed:\n%s", name, out)
		}
		if n := calls(t, dir, "gh"); n != 1 {
			t.Errorf("%s: gh called %d times, want 1", name, n)
		}
		if !strings.Contains(out, "persistent error") && !strings.Contains(out, "private:") {
			t.Errorf("%s: log must name the class:\n%s", name, out)
		}
	}
}

func TestVisibilityRetriesARateLimit(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	fakeTool(t, dir, "gh", "1|gh: rate limited (HTTP 429)", "0|false")
	if out, code := runGuard(t, dir, "visibility"); code != 0 {
		t.Fatalf("429 is transient, exit %d:\n%s", code, out)
	}
}

const npm404 = "1|npm error code E404"

func publishArgs(dir string) []string {
	return []string{"publish", "@owner/pkg", "1.2.3", "next", dir}
}

func TestPublishOfAnAlreadyPublishedVersionIsDone(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	fakeTool(t, dir, "npm", "0|1.2.3")
	out, code := runGuard(t, dir, publishArgs(dir)...)
	if code != 0 {
		t.Fatalf("an already-published version must exit 0:\n%s", out)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "npm.args"))
	if strings.Contains(string(args), "publish") {
		t.Errorf("must not publish what is on the registry:\n%s", args)
	}
}

func TestPublishRetriesATransientRegistryErrorAndPublishes(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	// view: 404 · publish: 503 · view (re-check): 404 · publish: ok
	fakeTool(t, dir, "npm", npm404, "1|npm error code E503", npm404, "0|+ @owner/pkg@1.2.3")
	out, code := runGuard(t, dir, publishArgs(dir)...)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "transient, retried 1") {
		t.Errorf("log must say \"transient, retried 1\":\n%s", out)
	}
}

func TestPublishThatLandedDespiteAnErrorIsDoneOnTheRecheck(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	// view: 404 · publish: connection dropped after the upload · view: 200
	fakeTool(t, dir, "npm", npm404, "1|npm error code ECONNRESET", "0|1.2.3")
	out, code := runGuard(t, dir, publishArgs(dir)...)
	if code != 0 {
		t.Fatalf("a publish that did land is done, exit %d:\n%s", code, out)
	}
	if n := calls(t, dir, "npm"); n != 3 {
		t.Errorf("npm called %d times, want 3 (no second publish)", n)
	}
}

func TestPublishConflictCountsAsDone(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	fakeTool(t, dir, "npm", npm404, "1|npm error code E403\nnpm error You cannot publish over the previously published versions: 1.2.3")
	if out, code := runGuard(t, dir, publishArgs(dir)...); code != 0 {
		t.Fatalf("a version that already exists is done, exit %d:\n%s", code, out)
	}
}

func TestPublishFailsOnAnErrorRetryingCannotFix(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	fakeTool(t, dir, "npm", npm404, "1|npm error code E401")
	out, code := runGuard(t, dir, publishArgs(dir)...)
	if code == 0 {
		t.Fatalf("an auth failure must fail:\n%s", out)
	}
	if n := calls(t, dir, "npm"); n != 2 {
		t.Errorf("npm called %d times, want 2 (view, one publish)", n)
	}
	if !strings.Contains(out, "persistent error") {
		t.Errorf("log must say persistent error:\n%s", out)
	}
}

func TestPublishFailsAfterTheRetryBudgetOnAPersistentOutage(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	fakeTool(t, dir, "npm", npm404, "1|npm error code E503", npm404, "1|npm error code E503")
	// the last reply repeats; view alternates are not scripted, so every later
	// call is a 503 — which on_registry reads as "could not tell".
	out, code := runGuard(t, dir, publishArgs(dir)...)
	if code == 0 {
		t.Fatalf("a registry that never recovers must fail:\n%s", out)
	}
	if !strings.Contains(out, "persistent error") {
		t.Errorf("log must say persistent error:\n%s", out)
	}
}

// The workflow must call the script rather than carry a second, untested copy
// of the same logic inline.
func TestReleaseNpmWorkflowUsesTheGuardScript(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/release-npm.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	for _, want := range []string{"release-npm-guard.sh visibility", "release-npm-guard.sh publish"} {
		if !strings.Contains(wf, want) {
			t.Errorf("release-npm.yml must call %q", want)
		}
	}
	if strings.Contains(wf, "gh api \"repos/$REPO\"") || strings.Contains(wf, "npm view \"$NAME@$VERSION\"") {
		t.Error("release-npm.yml carries an inline read the script now owns")
	}
}
