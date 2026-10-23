package fleet_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The downloads site (#355): a final is published by .github/workflows/publish-downloads.yml
// and installed with .github/downloads/install.sh. The workflow cannot run here, so its shape
// is pinned the way release_workflow_test.go pins the release workflows; the installer is run
// for real against a site served from a temporary directory.

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A final reaches npm by two jobs, and the downloads job must follow BOTH, finals only, only
// when the npm publish succeeded, and only once the site variable is set (so a repo without a
// site is never red).
func TestDownloadsJobsFollowBothFinalPathsAndStayDormant(t *testing.T) {
	auto := readRepoFile(t, ".github/workflows/release-auto.yml")
	for _, want := range []string{
		"downloads-final:",
		"downloads-existing:",
		"needs: [release, publish-final]",
		"needs: [existing-tag, publish-existing]",
		"needs.publish-final.result == 'success'",
		"needs.publish-existing.result == 'success'",
		"uses: ./.github/workflows/publish-downloads.yml",
	} {
		if !strings.Contains(auto, want) {
			t.Errorf("release-auto.yml must carry %q", want)
		}
	}
	if n := strings.Count(auto, "vars.DOWNLOADS_URL != ''"); n != 2 {
		t.Errorf("both downloads jobs are dormant without vars.DOWNLOADS_URL; found the guard %d times, want 2", n)
	}
	if n := strings.Count(auto, "!contains("); n < 2 {
		t.Errorf("both downloads jobs publish finals only; found %d tag checks, want 2", n)
	}
	// A candidate's path must not reach the site.
	if i := strings.Index(auto, "publish-candidate:"); i >= 0 {
		j := strings.Index(auto[i:], "publish-final:")
		if strings.Contains(auto[i:i+j], "publish-downloads") {
			t.Error("the candidate job must not publish to the downloads site")
		}
	}
}

// The binaries reach the downloads job as the artifact of the run that built them, so the
// artifact's name is a contract between the two workflows.
func TestDownloadsWorkflowReadsTheArtifactReleaseNpmUploads(t *testing.T) {
	npm := readRepoFile(t, ".github/workflows/release-npm.yml")
	dl := readRepoFile(t, ".github/workflows/publish-downloads.yml")
	if n := strings.Count(npm, "name: muster-binaries-${{ inputs.tag }}"); n != 2 {
		t.Errorf("release-npm.yml must upload and download muster-binaries-<tag> (found %d uses, want 2): a run can publish two tags", n)
	}
	if !strings.Contains(dl, `--name "muster-binaries-$TAG"`) {
		t.Error("publish-downloads.yml must download the artifact release-npm.yml names")
	}
	if strings.Contains(dl, "gh release download") || strings.Contains(dl, "gh release view") {
		t.Error("publish-downloads.yml reads a Release; this repository makes none")
	}
	if !strings.Contains(dl, "actions: read") {
		t.Error("publish-downloads.yml needs actions: read to download another job's artifact")
	}
	if !strings.Contains(dl, "A final tag only") {
		t.Error("publish-downloads.yml must refuse a candidate tag")
	}
}

// The publisher's own validation of the downloads: block, the manifest and the installer.
func TestDownloadsConfigurationPassesThePublishersCheck(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", ".github/downloads/publish-downloads.mjs", "check")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("publish-downloads.mjs check: %v\n%s", err, out)
	}
	// The files it publishes are the names build-release.sh writes.
	names := exec.Command("node", ".github/downloads/publish-downloads.mjs", "names", "v1.2.3")
	b, err := names.CombinedOutput()
	if err != nil {
		t.Fatalf("publish-downloads.mjs names: %v\n%s", err, b)
	}
	build := readRepoFile(t, "scripts/build-release.sh")
	if !strings.Contains(build, `muster-${VERSION}-${os}-${arch}`) {
		t.Fatal("build-release.sh no longer names binaries muster-<version>-<os>-<arch>; update downloads.files in project.yml")
	}
	want := map[string]bool{}
	for _, l := range strings.Fields(string(b)) {
		want[l] = true
	}
	for _, n := range []string{"muster-v1.2.3-darwin-arm64", "muster-v1.2.3-darwin-amd64", "muster-v1.2.3-linux-amd64", "muster-v1.2.3-linux-arm64"} {
		if !want[n] {
			t.Errorf("downloads.files does not publish %s", n)
		}
	}
}

// ---- install.sh -------------------------------------------------------------------------------

type fakeSite struct {
	root    string
	version string
	srv     *httptest.Server
	mu      sync.Mutex
	hits    []string
}

// newFakeSite serves <root>/muster/{latest.json,<v>/SHA256SUMS,<v>/<binary>} for this machine's
// platform. The "binary" is a shell script that reports the version it was published under.
func newFakeSite(t *testing.T, version string, corrupt bool) *fakeSite {
	t.Helper()
	osName := map[string]string{"darwin": "darwin", "linux": "linux"}[runtime.GOOS]
	arch := map[string]string{"arm64": "arm64", "amd64": "amd64"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		t.Skip("installer supports macOS and Linux on arm64/amd64 only")
	}
	root := t.TempDir()
	name := fmt.Sprintf("muster-v%s-%s-%s", version, osName, arch)
	bin := fmt.Sprintf("#!/bin/sh\necho 'muster v%s (test build)'\n", version)
	sum := sha256.Sum256([]byte(bin))
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	if corrupt {
		bin += "# tampered\n"
	}
	dir := filepath.Join(root, "muster", version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, "muster", "latest.json"): fmt.Sprintf("{\n  \"version\": \"%s\",\n  \"files\": {\n    \"%s\": {\"sha256\": \"x\", \"size\": 1}\n  }\n}\n", version, name),
		filepath.Join(dir, "SHA256SUMS"):             sums,
		filepath.Join(dir, name):                     bin,
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &fakeSite{root: root, version: version}
	fs := http.FileServer(http.Dir(root))
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits = append(s.hits, r.URL.Path)
		s.mu.Unlock()
		fs.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeSite) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

func runInstaller(t *testing.T, site *fakeSite, installDir string, env ...string) (string, int) {
	t.Helper()
	for _, tool := range []string{"sh", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	cmd := exec.Command("sh", ".github/downloads/install.sh")
	cmd.Env = append(os.Environ(), "MUSTER_DOWNLOADS_URL="+site.srv.URL, "MUSTER_INSTALL_DIR="+installDir, "MUSTER_VERSION=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestInstallerInstallsTheNewestVersionAndReportsIt(t *testing.T) {
	site := newFakeSite(t, "1.2.3", false)
	dir := filepath.Join(t.TempDir(), "bin")
	out, code := runInstaller(t, site, dir)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	got, err := exec.Command(filepath.Join(dir, "muster"), "--version").Output()
	if err != nil {
		t.Fatalf("installed binary does not run: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(got), "muster v1.2.3 ") {
		t.Errorf("installed binary reports %q", got)
	}
	if !strings.Contains(out, "muster v1.2.3") {
		t.Errorf("installer should report what it installed:\n%s", out)
	}
}

func TestInstallerTakesAnExactVersion(t *testing.T) {
	site := newFakeSite(t, "1.2.3", false)
	dir := filepath.Join(t.TempDir(), "bin")
	if out, code := runInstaller(t, site, dir, "MUSTER_VERSION=1.2.3"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, h := range site.seen() {
		if strings.HasSuffix(h, "latest.json") {
			t.Error("an exact version needs no latest.json")
		}
	}
	out, code := runInstaller(t, site, filepath.Join(t.TempDir(), "other"), "MUSTER_VERSION=9.9.9")
	if code == 0 {
		t.Fatalf("a version the site does not have must fail:\n%s", out)
	}
}

func TestInstallerInstallsNothingWhenTheChecksumFails(t *testing.T) {
	site := newFakeSite(t, "1.2.3", true)
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := filepath.Join(dir, "muster")
	if err := os.WriteFile(prev, []byte("#!/bin/sh\necho previous\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := runInstaller(t, site, dir)
	if code == 0 || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("a tampered download must fail on its checksum (exit %d):\n%s", code, out)
	}
	if b, _ := os.ReadFile(prev); !strings.Contains(string(b), "previous") {
		t.Error("a failed install replaced the installed muster")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("a failed install left files behind: %v", ents)
	}
}

func TestInstallerRefusesAMalformedVersionBeforeFetchingAnything(t *testing.T) {
	site := newFakeSite(t, "1.2.3", false)
	for _, v := range []string{"1.2", "1.2.3.4", "../1.2.3", "1.2.x", "1..3"} {
		out, code := runInstaller(t, site, filepath.Join(t.TempDir(), "bin"), "MUSTER_VERSION="+v)
		if code == 0 || !strings.Contains(out, "is not a version") {
			t.Errorf("MUSTER_VERSION=%q should be refused (exit %d):\n%s", v, code, out)
		}
	}
	if h := site.seen(); len(h) != 0 {
		t.Errorf("a malformed version reached the site: %v", h)
	}
}

func TestInstallerNeedsASiteAddress(t *testing.T) {
	cmd := exec.Command("sh", ".github/downloads/install.sh")
	cmd.Env = append(os.Environ(), "MUSTER_DOWNLOADS_URL=", "MUSTER_INSTALL_DIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "never published") {
		t.Fatalf("an unstamped installer with no override must say so (err %v):\n%s", err, out)
	}
}
