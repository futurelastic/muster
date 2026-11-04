package fleet_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// buildReleaseFixture makes a one-commit git repo holding a tiny module at the
// real module path, and returns its directory. files maps a path to its
// contents, on top of a main package that compiles everywhere.
func buildReleaseFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	for _, tool := range []string{"git", "go", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	src := filepath.Join(t.TempDir(), "src")
	gitIn(t, filepath.Dir(src), "init", "-b", "main", src)
	all := map[string]string{
		"go.mod":             "module github.com/futurelastic/muster\n\ngo 1.21\n",
		"cmd/muster/main.go": "package main\n\nfunc main() {}\n",
	}
	for p, c := range files {
		all[p] = c
	}
	for p, c := range all {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, src, "add", ".")
	gitIn(t, src, "commit", "-m", "fixture")
	return src
}

func runBuildRelease(t *testing.T, src string) (out string, code int, dist string) {
	t.Helper()
	script, err := filepath.Abs("scripts/build-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	dist = filepath.Join(t.TempDir(), "dist")
	cmd := exec.Command("sh", script, dist, "v0.5.0")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(b), code, dist
}

func builtNames(t *testing.T, dist string) []string {
	t.Helper()
	ents, err := os.ReadDir(dist)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "muster-") {
			names = append(names, e.Name())
		}
	}
	return names
}

// A tag cut before a platform was supported must still release (#316): the
// tooling's script is run against that tag's source, and trunk's target list
// used to be assumed to match it.
func TestBuildReleaseSkipsAWindowsTargetTheSourceCannotBuild(t *testing.T) {
	src := buildReleaseFixture(t, map[string]string{
		"cmd/muster/kill.go": "package main\n\nimport \"syscall\"\n\nvar _ = syscall.Kill\n", // unix only, as the pre-windows tags were
	})
	out, code, dist := runBuildRelease(t, src)
	if code != 0 {
		t.Fatalf("a source that predates windows must still release; exit %d\n%s", code, out)
	}
	names := builtNames(t, dist)
	if len(names) != 4 {
		t.Fatalf("want the 4 unix binaries, got %v\n%s", names, out)
	}
	for _, n := range names {
		if strings.Contains(n, "windows") {
			t.Errorf("a windows binary was written for a source that cannot build one: %s", n)
		}
	}
	sums, err := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sums), "windows") || strings.Count(string(sums), "\n") != 4 {
		t.Errorf("SHA256SUMS must list exactly the 4 built binaries:\n%s", sums)
	}
	for _, want := range []string{"skipping it", "not built: windows/amd64 windows/arm64"} {
		if !strings.Contains(out, want) {
			t.Errorf("the skip must be said out loud; missing %q in:\n%s", want, out)
		}
	}
}

func TestBuildReleaseBuildsEveryTargetWhenTheSourceSupportsThem(t *testing.T) {
	out, code, dist := runBuildRelease(t, buildReleaseFixture(t, nil))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	names := builtNames(t, dist)
	if len(names) != 6 {
		t.Fatalf("want all 6 binaries, got %v\n%s", names, out)
	}
	if strings.Contains(out, "not built") {
		t.Errorf("nothing was skipped, and the output must not claim it:\n%s", out)
	}
}

// The skip is for windows only: a unix target that does not build is a broken
// release, not an older one.
func TestBuildReleaseStillFailsWhenARequiredTargetCannotBuild(t *testing.T) {
	src := buildReleaseFixture(t, map[string]string{
		"cmd/muster/broken.go": "//go:build linux && arm64\n\npackage main\n\nvar _ int = \"not an int\"\n",
	})
	out, code, _ := runBuildRelease(t, src)
	if code == 0 {
		t.Fatalf("a required target failing to build must fail the release\n%s", out)
	}
	if !strings.Contains(out, "linux/arm64 is a required target and did not build") {
		t.Errorf("the failure must name the target:\n%s", out)
	}
}

// ---- the Windows packages are published and proved on Windows (#309) --------
//
// The workflows are read as text: the module takes no YAML dependency, and what
// is pinned here is a handful of facts about how the jobs are wired, each one a
// way the Windows check could quietly stop being a check.

// jobBlock returns one job's text from a workflow: its `  name:` line up to the
// next job's. Job names sit at two spaces of indent, a job's own keys at four.
func jobBlock(t *testing.T, wf, name string) string {
	t.Helper()
	loc := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:[ \t]*$`).FindStringIndex(wf)
	if loc == nil {
		t.Fatalf("release-npm.yml has no job %q", name)
	}
	rest := wf[loc[1]:]
	if next := regexp.MustCompile(`(?m)^  [A-Za-z0-9_-]+:[ \t]*$`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	return rest
}

func releaseNpmWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(".github/workflows/release-npm.yml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The publish needs the Windows run, the run is on both architectures' own
// hardware, and it cannot be skipped: a job skipped by its own `if:` skips every
// job that needs it, and a publish that silently does not happen reads as green.
func TestReleaseNpmPublishRequiresTheWindowsRun(t *testing.T) {
	wf := releaseNpmWorkflow(t)
	verify := jobBlock(t, wf, "verify-windows")
	for _, want := range []string{"windows-latest", "windows-11-arm", "--version", "muster-$env:TAG-windows-$env:ARCH.exe", "steps.has.outputs.present"} {
		if !strings.Contains(verify, want) {
			t.Errorf("verify-windows must carry %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^    needs:.*\bbinaries\b`).MatchString(verify) {
		t.Error("verify-windows must need the build that produced the binaries")
	}
	if regexp.MustCompile(`(?m)^    if:`).MatchString(verify) {
		t.Error("verify-windows must not be skipped by a job-level if: a skipped dependency skips the publish; skip its steps instead")
	}
	npm := jobBlock(t, wf, "npm")
	if !regexp.MustCompile(`(?m)^    needs:.*\bverify-windows\b`).MatchString(npm) {
		t.Error("the publish job must need verify-windows")
	}
}

// The post-publish check installs what was published, from the registry, the way
// a user does, and never on a dry run (nothing is on the registry then).
func TestReleaseNpmInstallsThePublishedPackageOnWindows(t *testing.T) {
	wf := releaseNpmWorkflow(t)
	install := jobBlock(t, wf, "install-windows")
	for _, want := range []string{"windows-latest", "windows-11-arm", "npm install -g", "@futurelastic/muster@", "muster --version", "process.arch"} {
		if !strings.Contains(install, want) {
			t.Errorf("install-windows must carry %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^    needs:.*\bnpm\b`).MatchString(install) {
		t.Error("install-windows must run after the publish job")
	}
	if !regexp.MustCompile(`(?m)^    if:.*!inputs\.dry_run`).MatchString(install) {
		t.Error("install-windows must not run on a dry run: there is nothing on the registry to install")
	}
}

// The registry serves a just-published version late, and for longer than the old
// fixed five minutes (#407). The install step therefore waits by a deadline, on a
// cheap `npm view` of both the launcher and this runner's win32 package, and only
// then installs; it waits on "not listed yet" and on nothing else.
func TestReleaseNpmInstallWaitsForTheRegistryBeforeInstalling(t *testing.T) {
	wf := releaseNpmWorkflow(t)
	install := jobBlock(t, wf, "install-windows")

	for _, want := range []string{
		"WAIT_MINUTES",                // the deadline is a named setting, not a loop count
		"npm view",                    // the cheap read the wait polls
		"@futurelastic/muster-win32-", // the platform package is waited on too, or an optional dependency is skipped silently
		"ETARGET",                     // lag is told apart from every other npm answer...
		"E404",                        // ...for the view too
		"within the retry budget",     // the wording of a real failure is unchanged
	} {
		if !strings.Contains(install, want) {
			t.Errorf("install-windows must carry %q", want)
		}
	}

	// The old shape: a counted loop of fixed 20 s naps. A budget in attempts is
	// what ran out before the registry caught up.
	for _, gone := range []string{"-le 15", "Start-Sleep -Seconds 20", "$attempt"} {
		if strings.Contains(install, gone) {
			t.Errorf("install-windows must not keep the fixed retry loop (%q)", gone)
		}
	}

	// The wait comes first: both reads precede the one install.
	launcher := strings.Index(install, "Wait-Listed $spec\n")
	platform := strings.Index(install, "Wait-Listed $platformSpec\n")
	doInstall := strings.Index(install, "'install', '-g'")
	if launcher < 0 || platform < 0 || doInstall < 0 {
		t.Fatalf("install-windows must wait for the launcher and its platform package, then install (launcher %d, platform %d, install %d)", launcher, platform, doInstall)
	}
	if !(launcher < platform && platform < doInstall) {
		t.Error("install-windows must wait for both packages to be listed before it installs")
	}

	// A launcher that installs but does not name the tag is a real failure and ends
	// the step at once: nothing after the version check loops back to the install.
	check := strings.Index(install, `.StartsWith("muster $env:TAG ")`)
	if check < 0 {
		t.Fatal("install-windows must still check that `muster --version` names the tag")
	}
	if tail := install[check:]; strings.Contains(tail, "continue") || strings.Contains(tail, "while ($true)") {
		t.Error("a `muster --version` that does not name the tag must fail the step, not be waited on")
	}
}

// A wait longer than the job's own timeout would end as a cancelled job with no
// message instead of the step's own failure. The deadline must fit inside the
// timeout with room for the setup and one slow npm call.
func TestReleaseNpmInstallDeadlineFitsInsideTheJobTimeout(t *testing.T) {
	install := jobBlock(t, releaseNpmWorkflow(t), "install-windows")
	wait := regexp.MustCompile(`(?m)^\s*WAIT_MINUTES:\s*(\d+)\b`).FindStringSubmatch(install)
	timeout := regexp.MustCompile(`(?m)^    timeout-minutes:\s*(\d+)\b`).FindStringSubmatch(install)
	if wait == nil || timeout == nil {
		t.Fatalf("install-windows must set WAIT_MINUTES and timeout-minutes (wait %v, timeout %v)", wait, timeout)
	}
	w, _ := strconv.Atoi(wait[1])
	to, _ := strconv.Atoi(timeout[1])
	if w < 10 {
		t.Errorf("WAIT_MINUTES is %d: the slowest propagation measured (#407) was past 5 minutes, so the budget needs real headroom", w)
	}
	if to < w+5 {
		t.Errorf("timeout-minutes %d leaves under 5 minutes beyond the %d-minute wait for setup and a slow npm call", to, w)
	}
}

// A Windows check that may fail quietly is not a check, and release-auto.yml
// starts from a CI whose Windows job already gates every candidate.
func TestReleaseNpmNeverLetsAWindowsFailurePass(t *testing.T) {
	if regexp.MustCompile(`(?m)^\s*continue-on-error\s*:`).MatchString(releaseNpmWorkflow(t)) {
		t.Error("release-npm.yml must not set continue-on-error")
	}
}
