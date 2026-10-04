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

// ---- publishing a tag that already exists (#263) ----------------------------
//
// release-auto.yml resolves which existing tag to publish with two reads in the
// guard script. They run against a real bare "origin" built here, so the tag
// shapes, the peeled commit of an annotated tag and the reachable-from-main test
// are git's own answers, not a fake's.

// gitIn runs git in dir and returns its trimmed stdout.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// tagFixture returns a clone whose origin has main with annotated tags
// v0.3.0-rc.8 and v0.3.0 on one commit, a later v0.3.1-rc.1 on main, and a
// branch commit that never reached main tagged v9.0.0-rc.1 and v9.0.0.
func tagFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	origin, work := filepath.Join(root, "origin.git"), filepath.Join(root, "work")
	gitIn(t, root, "init", "--bare", "-b", "main", origin)
	gitIn(t, root, "init", "-b", "main", work)
	gitIn(t, work, "remote", "add", "origin", origin)
	commit := func(msg string) {
		if err := os.WriteFile(filepath.Join(work, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, work, "add", "f")
		gitIn(t, work, "commit", "-m", msg)
	}
	commit("one")
	gitIn(t, work, "tag", "-a", "v0.3.0-rc.8", "-m", "rc")
	gitIn(t, work, "tag", "-a", "v0.3.0", "-m", "final", "v0.3.0-rc.8")
	commit("two")
	gitIn(t, work, "tag", "v0.3.1-rc.1") // lightweight
	gitIn(t, work, "push", "origin", "main", "--tags")
	gitIn(t, work, "checkout", "-b", "side")
	commit("side")
	gitIn(t, work, "tag", "-a", "v9.0.0-rc.1", "-m", "x")
	gitIn(t, work, "tag", "-a", "v9.0.0", "-m", "x")
	gitIn(t, work, "push", "origin", "side", "--tags")
	gitIn(t, work, "checkout", "main")
	gitIn(t, work, "fetch", "origin")
	return work
}

func runGuardIn(t *testing.T, work, pathDir string, args ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs("scripts/release-npm-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "PATH="+pathDir+":"+os.Getenv("PATH"), "RETRY_UNIT=0", "RETRY_MAX=2")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestCheckTagAcceptsATagOnMainAndNamesItsDistTag(t *testing.T) {
	requireBash(t)
	work := tagFixture(t)
	for tag, want := range map[string]string{
		"v0.3.0":      "latest", // annotated, peeled to a commit on main
		"v0.3.0-rc.8": "next",
		"v0.3.1-rc.1": "next", // lightweight
	} {
		out, code := runGuardIn(t, work, t.TempDir(), "check-tag", tag)
		if code != 0 || strings.TrimSpace(out) != want {
			t.Errorf("%s: exit %d, output %q, want dist-tag %q", tag, code, out, want)
		}
	}
}

func TestCheckTagRefusesAnythingNotAPublishableTag(t *testing.T) {
	requireBash(t)
	work := tagFixture(t)
	for tag, why := range map[string]string{
		"0.3.0":          "is not a release tag",
		"v0.3":           "is not a release tag",
		"v0.3.0-beta.1":  "is not a release tag",
		"v0.3.0; rm -rf": "is not a release tag",
		"":               "is not a release tag",
		"v0.4.0":         "does not exist on origin",
		"v9.0.0":         "is not on main",
		"v9.0.0-rc.1":    "is not on main",
	} {
		out, code := runGuardIn(t, work, t.TempDir(), "check-tag", tag)
		if code == 0 {
			t.Errorf("%q must be refused:\n%s", tag, out)
		}
		if !strings.Contains(out, why) {
			t.Errorf("%q: log must say %q:\n%s", tag, why, out)
		}
	}
}

func TestUnpublishedFinalNamesTheNewestFinalNpmLacks(t *testing.T) {
	requireBash(t)
	work := tagFixture(t)
	dir := t.TempDir()
	fakeTool(t, dir, "npm", npm404)
	out, code := runGuardIn(t, work, dir, "unpublished-final")
	if code != 0 || strings.TrimSpace(out) != "v0.3.0" {
		t.Fatalf("exit %d, output %q, want v0.3.0 (v9.0.0 is newer but off main — never reached)", code, out)
	}
}

func TestUnpublishedFinalIsEmptyWhenTheNewestFinalIsOnNpm(t *testing.T) {
	requireBash(t)
	work := tagFixture(t)
	dir := t.TempDir()
	fakeTool(t, dir, "npm", "0|0.3.0")
	out, code := runGuardIn(t, work, dir, "unpublished-final")
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("exit %d, output %q, want nothing", code, out)
	}
	if n := calls(t, dir, "npm"); n != 1 {
		t.Errorf("npm read %d times, want 1 (the launcher only)", n)
	}
}

// The run that just finalized a tag publishes it itself; the reconcile must not
// publish it a second time.
func TestUnpublishedFinalSkipsTheTagThisRunFinalized(t *testing.T) {
	requireBash(t)
	work := tagFixture(t)
	dir := t.TempDir()
	fakeTool(t, dir, "npm", npm404)
	out, code := runGuardIn(t, work, dir, "unpublished-final", "v0.3.0")
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("exit %d, output %q, want nothing", code, out)
	}
	if n := calls(t, dir, "npm"); n != 0 {
		t.Errorf("npm read %d times, want 0 — a skipped tag is not looked up", n)
	}
}

// Only the NEWEST final is ever reported: an older missing one would be published
// to `latest` after a newer one and move it backwards.
func TestUnpublishedFinalNeverReportsAnOlderFinal(t *testing.T) {
	requireBash(t)
	work := tagFixture(t)
	gitIn(t, work, "tag", "-a", "v0.3.1", "-m", "final", "v0.3.1-rc.1")
	gitIn(t, work, "push", "origin", "v0.3.1")
	dir := t.TempDir()
	fakeTool(t, dir, "npm", "0|0.3.1") // the newest is there; v0.3.0 is not asked about
	out, code := runGuardIn(t, work, dir, "unpublished-final")
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("exit %d, output %q, want nothing", code, out)
	}
}

func TestUnpublishedFinalFailsClosedWhenNpmCannotBeRead(t *testing.T) {
	requireBash(t)
	work := tagFixture(t)
	dir := t.TempDir()
	fakeTool(t, dir, "npm", "1|npm error code E503")
	out, code := runGuardIn(t, work, dir, "unpublished-final")
	if code == 0 || strings.TrimSpace(out) == "v0.3.0" {
		t.Fatalf("an unreadable registry must fail, not read as missing (exit %d):\n%s", code, out)
	}
	if !strings.Contains(out, "persistent error") {
		t.Errorf("log must say persistent error:\n%s", out)
	}
}

// The two workflows' contract: release-auto owns every publish, release-npm only
// dry-runs when started by hand (#263).
func TestReleaseWorkflowsKeepTheManualPathDryRunOnly(t *testing.T) {
	auto, err := os.ReadFile(".github/workflows/release-auto.yml")
	if err != nil {
		t.Fatal(err)
	}
	npm, err := os.ReadFile(".github/workflows/release-npm.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"publish-tag:", "check-tag", "unpublished-final", "release-npm.yml"} {
		if !strings.Contains(string(auto), want) {
			t.Errorf("release-auto.yml must carry %q", want)
		}
	}
	for _, want := range []string{"Refuse a manual publish", "release-npm.yml@", "publish-tag"} {
		if !strings.Contains(string(npm), want) {
			t.Errorf("release-npm.yml must carry %q", want)
		}
	}
	if strings.Contains(string(npm), "workflow file name = the file that STARTS") ||
		strings.Contains(string(npm), "For a manual dispatch that is `release-npm.yml`") {
		t.Error("release-npm.yml header still invites switching the trusted publisher")
	}
}
