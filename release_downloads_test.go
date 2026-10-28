package fleet_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Publishing a final to the company release host, next to npm (#383).
//
// Three pieces, each proven against what it talks to rather than against a copy of its own logic:
//
//   - .github/downloads/publish.mjs runs as a real `node` process against an httptest server that plays the host's
//     publish API: it checks the token, verifies the ed25519 signature with Go's crypto (an independent verifier of
//     the exact bytes the contract names), serves offsets, assembles the parts and compares them with the signed sums.
//   - .github/downloads/install.sh runs under `sh` with a fake `uname`, a fake muster that logs what it was asked to
//     do, and an httptest server serving the version folder.
//   - scripts/release-downloads-guard.sh runs against a real bare origin and a fake `gh`.
//
// What none of it can prove, and the issue says so: that the next final lands on the real host, and that a real
// service manager takes the install. Those are the first live run.

func requireTool(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
}

// ---- a fake host -------------------------------------------------------------------------------------------------------

const dlTag = "v1.2.3"
const dlVersion = "1.2.3"

type dlFile struct {
	name string
	data []byte
}

type beginBody struct {
	Version string `json:"version"`
	Files   []struct {
		Name string
		Size int
	} `json:"files"`
	Sums      string         `json:"sums"`
	Sig       string         `json:"sig"`
	Manifest  map[string]any `json:"manifest"`
	InstallSh string         `json:"install_sh"`
}

// dlHost is the publish API as the contract describes it, with just enough state to check a whole run.
type dlHost struct {
	t   *testing.T
	srv *httptest.Server
	pub ed25519.PublicKey

	partBytes int
	// held is what the host already has when begin is answered (a resumed upload): name -> bytes.
	held map[string][]byte
	// already makes begin answer "published" for a version the host has.
	already bool
	// failPartOnce names a file whose first part is answered 503 without being stored.
	failPartOnce string
	// refuseBegin makes begin answer this status and error code.
	refuseStatus int
	refuseCode   string

	mu        sync.Mutex
	issued    int
	used      map[string]bool
	audiences []string
	begins    []beginBody
	puts      []string // "name@offset"
	completes int
	failed    bool
	rejected  []string
}

func newDLHost(t *testing.T, pub ed25519.PublicKey) *dlHost {
	h := &dlHost{t: t, pub: pub, partBytes: 1000, held: map[string][]byte{}, used: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/oidc", h.locked(h.oidc))
	mux.HandleFunc("/api/v1/publish/items/", h.locked(h.begin))
	mux.HandleFunc("/api/v1/publish/uploads/", h.locked(h.upload))
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

// locked runs a handler under the host's mutex: one request at a time, and the state a test reads afterwards (see
// settle) is never raced with.
func (h *dlHost) locked(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		f(w, r)
	}
}

// set changes how the host behaves, under the same mutex its handlers hold.
func (h *dlHost) set(f func(h *dlHost)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

// settle orders everything a handler wrote before whatever the test reads next.
func (h *dlHost) settle() {
	h.mu.Lock()
	h.mu.Unlock() //nolint:staticcheck // an empty critical section is the point
}

func (h *dlHost) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *dlHost) refuse(w http.ResponseWriter, status int, code, msg string) {
	h.rejected = append(h.rejected, code)
	h.reply(w, status, map[string]string{"error": code, "message": msg})
}

// oidc plays GitHub's token endpoint: one fresh token per request, for the audience asked.
func (h *dlHost) oidc(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer request-secret" {
		h.reply(w, http.StatusUnauthorized, map[string]string{"message": "no"})
		return
	}
	h.issued++
	n := h.issued
	h.audiences = append(h.audiences, r.URL.Query().Get("audience"))
	h.reply(w, http.StatusOK, map[string]string{"value": "oidc-" + strconv.Itoa(n)})
}

func (h *dlHost) begin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/publish/items/muster/uploads" {
		h.refuse(w, http.StatusNotFound, "NOT_FOUND", r.Method+" "+r.URL.Path)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	replayed := h.used[tok]
	h.used[tok] = true
	if !strings.HasPrefix(tok, "oidc-") {
		h.refuse(w, http.StatusUnauthorized, "TOKEN_MISSING", "send the OIDC token")
		return
	}
	if replayed {
		h.refuse(w, http.StatusUnauthorized, "TOKEN_REPLAYED", "This token was already used.")
		return
	}
	if h.refuseStatus != 0 {
		h.refuse(w, h.refuseStatus, h.refuseCode, "refused by the test")
		return
	}
	var b beginBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		h.refuse(w, http.StatusBadRequest, "BODY_INVALID", err.Error())
		return
	}
	sig, err := hex.DecodeString(b.Sig)
	// The contract's signed bytes: this prefix, then the exact SHA256SUMS text.
	msg := []byte("library release v1\nmuster\n" + b.Version + "\n" + b.Sums)
	if err != nil || !ed25519.Verify(h.pub, msg, sig) {
		h.refuse(w, http.StatusForbidden, "SIGNATURE_INVALID", "The SHA256SUMS signature does not match.")
		return
	}
	table := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(b.Sums, "\n"), "\n") {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok {
			h.refuse(w, http.StatusUnprocessableEntity, "SUMS_INVALID", line)
			return
		}
		table[name] = sum
	}
	if len(table) != len(b.Files) {
		h.refuse(w, http.StatusUnprocessableEntity, "FILES_MISMATCH", "not the same set")
		return
	}
	for _, f := range b.Files {
		if _, ok := table[f.Name]; !ok {
			h.refuse(w, http.StatusUnprocessableEntity, "FILES_MISMATCH", f.Name)
			return
		}
	}
	h.begins = append(h.begins, b)
	if h.already {
		h.reply(w, http.StatusOK, map[string]any{"status": "published", "slug": "muster", "version": b.Version, "already": true})
		return
	}
	files := make([]map[string]any, 0, len(b.Files))
	for _, f := range b.Files {
		files = append(files, map[string]any{"name": f.Name, "size": f.Size, "received": len(h.held[f.Name])})
	}
	h.reply(w, http.StatusCreated, map[string]any{"data": map[string]any{
		"id": "up1", "token": "upload-secret", "resumed": len(h.held) > 0, "partBytes": h.partBytes, "files": files,
	}})
}

func (h *dlHost) upload(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer upload-secret" {
		h.refuse(w, http.StatusNotFound, "UPLOAD_NOT_FOUND", "No such upload, or it has expired. Begin again.")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/publish/uploads/up1")
	switch {
	case r.Method == http.MethodGet && rest == "":
		var files []map[string]any
		for _, f := range h.begins[len(h.begins)-1].Files {
			files = append(files, map[string]any{"name": f.Name, "size": f.Size, "received": len(h.held[f.Name])})
		}
		h.reply(w, http.StatusOK, map[string]any{"data": map[string]any{"files": files}})
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "/files/"):
		name := strings.TrimPrefix(rest, "/files/")
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if name == h.failPartOnce && !h.failed {
			h.failed = true
			h.reply(w, http.StatusServiceUnavailable, map[string]string{"error": "UNAVAILABLE", "message": "try again"})
			return
		}
		if off != len(h.held[name]) {
			h.reply(w, http.StatusConflict, map[string]string{"error": "OFFSET_MISMATCH", "message": fmt.Sprintf("%s holds %d, got offset %d", name, len(h.held[name]), off)})
			return
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) > h.partBytes {
			h.reply(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "PART_TOO_LARGE", "message": "part"})
			return
		}
		h.held[name] = append(h.held[name], body...)
		h.puts = append(h.puts, name+"@"+strconv.Itoa(off))
		h.reply(w, http.StatusOK, map[string]any{"name": name, "received": len(h.held[name])})
	case r.Method == http.MethodPost && rest == "/complete":
		h.completes++
		b := h.begins[len(h.begins)-1]
		for _, line := range strings.Split(strings.TrimSuffix(b.Sums, "\n"), "\n") {
			sum, name, _ := strings.Cut(line, "  ")
			got := sha256.Sum256(h.held[name])
			if hex.EncodeToString(got[:]) != sum {
				h.reply(w, http.StatusUnprocessableEntity, map[string]string{"error": "FILE_HASH_MISMATCH", "message": name})
				return
			}
		}
		h.reply(w, http.StatusOK, map[string]any{"status": "published", "slug": "muster", "version": b.Version, "already": false})
	default:
		h.reply(w, http.StatusNotFound, map[string]string{"error": "NOT_FOUND", "message": r.Method + " " + r.URL.Path})
	}
}

// ---- staged files and the node process --------------------------------------------------------------------------------

// dlStaged writes the four published files and the SHA256SUMS `stage` would write. Sizes are chosen against a part
// size of 1000: three parts, an exact multiple, one byte, and a ragged tail.
func dlStaged(t *testing.T) (dir string, files []dlFile) {
	t.Helper()
	dir = t.TempDir()
	sizes := map[string]int{"darwin-arm64": 2500, "darwin-amd64": 1000, "linux-amd64": 1, "linux-arm64": 3001}
	var sums strings.Builder
	for _, tgt := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"} {
		data := make([]byte, sizes[tgt])
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		name := "muster-" + dlTag + "-" + tgt
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		files = append(files, dlFile{name, data})
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, files
}

type dlRun struct {
	seed string
	pub  ed25519.PublicKey
	host *dlHost
}

func newDLRun(t *testing.T) *dlRun {
	t.Helper()
	requireTool(t, "node")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &dlRun{seed: hex.EncodeToString(priv.Seed()), pub: pub, host: newDLHost(t, pub)}
}

// runNode runs publish.mjs with exactly this environment (and none of the PUBLISH_/ACTIONS_ ones the test machine may
// have). A value of "-" leaves that variable unset.
func runNode(t *testing.T, env map[string]string, args ...string) (string, int) {
	t.Helper()
	var full []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "PUBLISH_") || strings.HasPrefix(k, "ACTIONS_") || k == "GITHUB_REF" {
			continue
		}
		full = append(full, kv)
	}
	for k, v := range env {
		if v != "-" {
			full = append(full, k+"="+v)
		}
	}
	cmd := exec.Command("node", append([]string{".github/downloads/publish.mjs"}, args...)...)
	cmd.Env = full
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// node runs it as the publish job does: the fake host's address, the run's token endpoint, a signing key, a run at
// the tag. override changes or ("-") removes a variable.
func (r *dlRun) node(t *testing.T, override map[string]string, args ...string) (string, int) {
	t.Helper()
	env := map[string]string{
		"PUBLISH_ORIGIN":                 r.host.srv.URL,
		"PUBLISH_SIGNING_KEY":            r.seed,
		"ACTIONS_ID_TOKEN_REQUEST_URL":   r.host.srv.URL + "/oidc?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-secret",
		"GITHUB_REF":                     "refs/tags/" + dlTag,
		"PUBLISH_RETRY_PAUSE_MS":         "1",
	}
	for k, v := range override {
		env[k] = v
	}
	out, code := runNode(t, env, args...)
	r.host.settle()
	return out, code
}

func TestPublishSendsASignedReleaseAndEveryByte(t *testing.T) {
	r := newDLRun(t)
	dir, files := dlStaged(t)
	out, code := r.node(t, nil, "publish", dir, dlTag)
	if code != 0 {
		t.Fatalf("publish failed (%d):\n%s", code, out)
	}
	h := r.host
	if len(h.begins) != 1 {
		t.Fatalf("want one begin, got %d", len(h.begins))
	}
	b := h.begins[0]
	if b.Version != dlVersion {
		t.Errorf("version %q: the host's folder is the tag without its v (%s)", b.Version, dlVersion)
	}
	if len(h.audiences) != 1 || h.audiences[0] != h.srv.URL {
		t.Errorf("the token's audience must be the host's exact origin %q, got %v", h.srv.URL, h.audiences)
	}
	for _, f := range files {
		if got := h.held[f.name]; string(got) != string(f.data) {
			t.Errorf("%s: the host holds %d bytes that differ from the %d staged", f.name, len(got), len(f.data))
		}
	}
	// 3 + 1 + 1 + 4 parts: no part larger than the host's limit, none sent twice, each at its own offset.
	if len(h.puts) != 9 {
		t.Errorf("want 9 parts, got %d: %v", len(h.puts), h.puts)
	}
	if h.completes != 1 {
		t.Errorf("complete called %d times", h.completes)
	}
	// The installer is the repository's, with the host's address stamped in, and the manifest is the repository's.
	if !strings.Contains(b.InstallSh, h.srv.URL) || strings.Contains(b.InstallSh, "@RELEASE_ORIGIN@") {
		t.Error("install_sh must carry the host's address and no placeholder")
	}
	if b.Manifest["name"] != "Muster" {
		t.Errorf("manifest name = %v", b.Manifest["name"])
	}
	// Only the four unix binaries are in the signed set; SHA256SUMS never lists itself.
	if strings.Count(b.Sums, "\n") != 4 || strings.Contains(b.Sums, "windows") || strings.Contains(b.Sums, "SHA256SUMS") {
		t.Errorf("signed sums:\n%s", b.Sums)
	}
	if strings.Contains(out, r.seed) || strings.Contains(out, "request-secret") || strings.Contains(out, "upload-secret") || strings.Contains(out, "oidc-1") {
		t.Errorf("the log leaks a secret:\n%s", out)
	}
}

func TestPublishResumesFromWhereTheHostIs(t *testing.T) {
	r := newDLRun(t)
	dir, files := dlStaged(t)
	// A run that died: the host already holds the first 1500 bytes of one file and all of another.
	r.host.set(func(h *dlHost) {
		h.held[files[3].name] = append([]byte(nil), files[3].data[:1500]...)
		h.held[files[1].name] = append([]byte(nil), files[1].data...)
	})
	if out, code := r.node(t, nil, "publish", dir, dlTag); code != 0 {
		t.Fatalf("resume failed (%d):\n%s", code, out)
	}
	for _, p := range r.host.puts {
		if p == files[1].name+"@0" || p == files[3].name+"@0" || p == files[3].name+"@1000" {
			t.Errorf("sent %s again: the begin answer said the host held it", p)
		}
	}
	for _, f := range files {
		if string(r.host.held[f.name]) != string(f.data) {
			t.Errorf("%s did not end up whole", f.name)
		}
	}
}

func TestPublishOfAVersionTheHostHasSendsNothing(t *testing.T) {
	r := newDLRun(t)
	r.host.set(func(h *dlHost) { h.already = true })
	dir, _ := dlStaged(t)
	out, code := r.node(t, nil, "publish", dir, dlTag)
	if code != 0 || !strings.Contains(out, "already on the host") {
		t.Fatalf("want success saying it is already there (%d):\n%s", code, out)
	}
	if len(r.host.puts) != 0 || r.host.completes != 0 {
		t.Errorf("nothing may be sent for a published version: %v, %d completes", r.host.puts, r.host.completes)
	}
}

func TestPublishAsksTheHostWhereItIsAfterAFailedPart(t *testing.T) {
	r := newDLRun(t)
	dir, files := dlStaged(t)
	r.host.set(func(h *dlHost) { h.failPartOnce = files[3].name })
	if out, code := r.node(t, nil, "publish", dir, dlTag); code != 0 {
		t.Fatalf("a single 503 must be absorbed (%d):\n%s", code, out)
	}
	if string(r.host.held[files[3].name]) != string(files[3].data) {
		t.Error("the file did not end up whole after the retry")
	}
}

func TestPublishDoesNotRetryARefusal(t *testing.T) {
	r := newDLRun(t)
	r.host.set(func(h *dlHost) { h.refuseStatus, h.refuseCode = http.StatusForbidden, "REPO_NOT_ALLOWED" })
	dir, _ := dlStaged(t)
	out, code := r.node(t, nil, "publish", dir, dlTag)
	if code == 0 || !strings.Contains(out, "REPO_NOT_ALLOWED") {
		t.Fatalf("a refusal must fail and say why (%d):\n%s", code, out)
	}
	// One token per attempt that reaches the host; a refusal is not retried.
	if r.host.issued != 1 {
		t.Errorf("the host refused once and a token was asked for %d times", r.host.issued)
	}
}

func TestPublishRefusesBeforeAnyNetworkCall(t *testing.T) {
	cases := []struct {
		name string
		tag  string
		env  map[string]string
		want string
	}{
		{"a candidate", "v1.2.3-rc.4", nil, "not a final tag"},
		{"a branch run", dlTag, map[string]string{"GITHUB_REF": "refs/heads/main"}, "not refs/tags/" + dlTag},
		{"no signing key", dlTag, map[string]string{"PUBLISH_SIGNING_KEY": "-"}, "PUBLISH_SIGNING_KEY is not set"},
		{"a malformed key", dlTag, map[string]string{"PUBLISH_SIGNING_KEY": "abc"}, "64 lowercase hex"},
		{"an origin with a trailing slash", dlTag, map[string]string{"PUBLISH_ORIGIN": "https://host.example/"}, "exact origin"},
		{"no origin", dlTag, map[string]string{"PUBLISH_ORIGIN": "-"}, "exact origin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newDLRun(t)
			dir, _ := dlStaged(t)
			out, code := r.node(t, c.env, "publish", dir, c.tag)
			if code == 0 || !strings.Contains(out, c.want) {
				t.Fatalf("want a refusal containing %q (%d):\n%s", c.want, code, out)
			}
			if r.host.issued != 0 || len(r.host.begins) != 0 {
				t.Error("the run reached the network before refusing")
			}
		})
	}
}

func TestPublishRefusesAnArtifactThatChangedAfterStaging(t *testing.T) {
	r := newDLRun(t)
	dir, files := dlStaged(t)
	if err := os.WriteFile(filepath.Join(dir, files[0].name), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := r.node(t, nil, "publish", dir, dlTag)
	if code == 0 || !strings.Contains(out, "does not match SHA256SUMS") || r.host.issued != 0 {
		t.Fatalf("a changed file must be refused before signing (%d):\n%s", code, out)
	}
}

func TestPublishDryRunSignsAndSendsNothing(t *testing.T) {
	r := newDLRun(t)
	dir, _ := dlStaged(t)
	// A dry run may start from any ref: it is how the environment is proven without a new final.
	out, code := r.node(t, map[string]string{"GITHUB_REF": "refs/heads/main"}, "publish", dir, dlTag, "--dry-run")
	if code != 0 || !strings.Contains(out, "nothing was sent") {
		t.Fatalf("dry run (%d):\n%s", code, out)
	}
	if r.host.issued != 0 || len(r.host.begins) != 0 {
		t.Error("a dry run must not ask GitHub for a token or call the host")
	}
	if !strings.Contains(out, hex.EncodeToString(r.pub)) {
		t.Error("a dry run must print the public key the host has to be bound to")
	}
}

func TestStageKeepsTheFourUnixBinariesAndRefusesAChangedOne(t *testing.T) {
	requireTool(t, "node")
	dist := t.TempDir()
	var sums strings.Builder
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		s := sha256.Sum256([]byte(content))
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(s[:]), name)
	}
	for _, tgt := range []string{"darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64", "windows-amd64.exe", "windows-arm64.exe"} {
		write("muster-"+dlTag+"-"+tgt, "binary "+tgt)
	}
	if err := os.WriteFile(filepath.Join(dist, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "stage")
	if o, code := runNode(t, nil, "stage", dist, out, dlTag); code != 0 {
		t.Fatalf("stage failed:\n%s", o)
	}
	ents, _ := os.ReadDir(out)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"SHA256SUMS", "muster-" + dlTag + "-darwin-amd64", "muster-" + dlTag + "-darwin-arm64", "muster-" + dlTag + "-linux-amd64", "muster-" + dlTag + "-linux-arm64"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("staged %v, want %v", names, want)
	}
	staged, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if strings.Count(string(staged), "\n") != 4 || strings.Contains(string(staged), "windows") {
		t.Errorf("staged sums:\n%s", staged)
	}
	// A binary changed between the build and the stage.
	if err := os.WriteFile(filepath.Join(dist, "muster-"+dlTag+"-linux-amd64"), []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if o, code := runNode(t, nil, "stage", dist, filepath.Join(t.TempDir(), "again"), dlTag); code == 0 || !strings.Contains(o, "does not match the build's SHA256SUMS") {
		t.Fatalf("a changed binary must be refused (%d):\n%s", code, o)
	}
	// A candidate is never staged.
	if o, code := runNode(t, nil, "stage", dist, filepath.Join(t.TempDir(), "rc"), "v1.2.3-rc.1"); code == 0 || !strings.Contains(o, "not a final tag") {
		t.Fatalf("a candidate must be refused (%d):\n%s", code, o)
	}
}

func TestKeygenMakesASeedThatSignsForItsPublicKey(t *testing.T) {
	requireTool(t, "node")
	cmd := exec.Command("node", ".github/downloads/publish.mjs", "keygen")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	seedOut, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	seed := strings.TrimSpace(string(seedOut))
	pubHex := strings.TrimSpace(strings.TrimPrefix(stderr.String(), "public key:"))
	if len(seed) != 64 || len(pubHex) != 64 {
		t.Fatalf("seed %q, public key %q", seed, pubHex)
	}
	raw, _ := hex.DecodeString(seed)
	if got := hex.EncodeToString(ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)); got != pubHex {
		t.Errorf("the printed public key %s is not the seed's (%s)", pubHex, got)
	}
}

// ---- the installer -----------------------------------------------------------------------------------------------------

// fakeMuster is a stand-in for the released binary. `service install` copies the running binary to
// ~/.local/bin/muster, as the real one does, and every call is logged.
func fakeMuster(version, mode string) string {
	return `#!/bin/sh
V=` + version + `
MODE=` + mode + `
case "$1" in
--version) echo "muster v$V (abc1234, go1.23.0)"; exit 0 ;;
service)
  case "$2" in
  install)
    echo "install v$V" >> "$HOME/calls.log"
    [ "$MODE" = install-fails ] && { echo "the service did not become healthy" >&2; exit 1; }
    mkdir -p "$HOME/.local/bin"
    cp "$0" "$HOME/.local/bin/muster.tmp" && mv "$HOME/.local/bin/muster.tmp" "$HOME/.local/bin/muster"
    exit 0 ;;
  status)
    echo "status v$V" >> "$HOME/calls.log"
    [ "$MODE" = status-fails ] && exit 1
    exit 0 ;;
  esac ;;
esac
exit 2
`
}

type dlSite struct {
	srv   *httptest.Server
	token string
	hits  []string
	mu    sync.Mutex
}

// serveRelease serves /muster/latest.json and /muster/<version>/{SHA256SUMS,muster-v<version>-linux-amd64}.
// A binary named for a different version, or a wrong hash, are the test's to ask for.
func serveRelease(t *testing.T, version, binary, token string, hashOverride string) *dlSite {
	t.Helper()
	name := "muster-v" + version + "-linux-amd64"
	sum := sha256.Sum256([]byte(binary))
	hash := hex.EncodeToString(sum[:])
	if hashOverride != "" {
		hash = hashOverride
	}
	s := &dlSite{token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits = append(s.hits, r.URL.Path)
		s.mu.Unlock()
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "locked", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/muster/latest.json":
			fmt.Fprintf(w, `{"version":%q,"files":{}}`, version)
		case "/muster/" + version + "/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", hash, name)
		case "/muster/" + version + "/" + name:
			io.WriteString(w, binary)
		default:
			http.NotFound(w, r)
		}
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

type dlInstall struct {
	home string
	bin  string
}

func newDLInstall(t *testing.T) *dlInstall {
	t.Helper()
	requireTool(t, "sh", "curl", "awk", "sed")
	d := &dlInstall{home: t.TempDir(), bin: t.TempDir()}
	// A fixed platform, so the test serves one file whatever machine it runs on.
	uname := "#!/bin/sh\ncase \"$1\" in -s) echo Linux ;; -m) echo \"${FAKE_ARCH:-x86_64}\" ;; *) echo Linux ;; esac\n"
	if err := os.WriteFile(filepath.Join(d.bin, "uname"), []byte(uname), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *dlInstall) previous(t *testing.T, version string) {
	t.Helper()
	p := filepath.Join(d.home, ".local", "bin")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "muster"), []byte(fakeMuster(version, "ok")), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (d *dlInstall) run(t *testing.T, env map[string]string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{".github/downloads/install.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+d.bin+":"+os.Getenv("PATH"), "HOME="+d.home, "DL_URL=-", "DL_TOKEN=", "MUSTER_VERSION=")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func (d *dlInstall) installedVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(d.home, ".local", "bin", "muster"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "V=") {
			return strings.TrimPrefix(line, "V=")
		}
	}
	return "?"
}

func (d *dlInstall) log(t *testing.T) string {
	b, _ := os.ReadFile(filepath.Join(d.home, "calls.log"))
	return strings.TrimSpace(string(b))
}

func TestInstallOnACleanMachineInstallsTheVersionAndChecksTheService(t *testing.T) {
	d := newDLInstall(t)
	site := serveRelease(t, "0.4.0", fakeMuster("0.4.0", "ok"), "", "")
	out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL}, "0.4.0")
	if code != 0 {
		t.Fatalf("install failed (%d):\n%s", code, out)
	}
	if v := d.installedVersion(t); v != "0.4.0" {
		t.Errorf("installed version = %q", v)
	}
	// The install, then the status check, in that order, by the new binary.
	if got := d.log(t); got != "install v0.4.0\nstatus v0.4.0" {
		t.Errorf("calls:\n%s", got)
	}
	if !strings.Contains(out, "service healthy") {
		t.Errorf("output:\n%s", out)
	}
}

func TestInstallTakesTheNewestVersionWhenNoneIsGiven(t *testing.T) {
	d := newDLInstall(t)
	site := serveRelease(t, "0.4.0", fakeMuster("0.4.0", "ok"), "", "")
	if out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL}); code != 0 {
		t.Fatalf("install failed (%d):\n%s", code, out)
	}
	if v := d.installedVersion(t); v != "0.4.0" {
		t.Errorf("installed version = %q", v)
	}
	// "v0.4.0" and MUSTER_VERSION are accepted too.
	d2 := newDLInstall(t)
	if out, code := d2.run(t, map[string]string{"DL_URL": site.srv.URL, "MUSTER_VERSION": "v0.4.0"}); code != 0 || d2.installedVersion(t) != "0.4.0" {
		t.Fatalf("MUSTER_VERSION=v0.4.0 (%d):\n%s", code, out)
	}
}

func TestInstallOverAnExistingInstallUpgradesIt(t *testing.T) {
	d := newDLInstall(t)
	d.previous(t, "0.3.0")
	site := serveRelease(t, "0.4.0", fakeMuster("0.4.0", "ok"), "", "")
	out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL}, "0.4.0")
	if code != 0 || d.installedVersion(t) != "0.4.0" {
		t.Fatalf("upgrade (%d), installed %q:\n%s", code, d.installedVersion(t), out)
	}
}

func TestInstallPutsThePreviousVersionBackWhenTheNewOneDoesNotComeUp(t *testing.T) {
	for _, mode := range []string{"install-fails", "status-fails"} {
		t.Run(mode, func(t *testing.T) {
			d := newDLInstall(t)
			d.previous(t, "0.3.0")
			site := serveRelease(t, "0.4.0", fakeMuster("0.4.0", mode), "", "")
			out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL}, "0.4.0")
			if code != 1 {
				t.Fatalf("a version that does not come up must exit 1, got %d:\n%s", code, out)
			}
			if v := d.installedVersion(t); v != "0.3.0" {
				t.Errorf("the previous version must be running again, installed %q:\n%s", v, out)
			}
			if !strings.Contains(d.log(t), "install v0.3.0") {
				t.Errorf("the previous binary must have run its own service install:\n%s", d.log(t))
			}
			if !strings.Contains(out, "is installed and running again") {
				t.Errorf("output must say what is running now:\n%s", out)
			}
		})
	}
}

func TestInstallWithNothingBeforeItLeavesNothingWhenItFails(t *testing.T) {
	d := newDLInstall(t)
	site := serveRelease(t, "0.4.0", fakeMuster("0.4.0", "install-fails"), "", "")
	out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL}, "0.4.0")
	if code != 1 || d.installedVersion(t) != "" {
		t.Fatalf("a failed first install must exit 1 and leave no binary (%d, %q):\n%s", code, d.installedVersion(t), out)
	}
}

func TestInstallRefusesAWrongChecksumBeforeTouchingAnything(t *testing.T) {
	d := newDLInstall(t)
	d.previous(t, "0.3.0")
	site := serveRelease(t, "0.4.0", fakeMuster("0.4.0", "ok"), "", strings.Repeat("0", 64))
	out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL}, "0.4.0")
	if code != 1 || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("want a checksum refusal (%d):\n%s", code, out)
	}
	if d.installedVersion(t) != "0.3.0" || d.log(t) != "" {
		t.Errorf("nothing may run or change: installed %q, calls %q", d.installedVersion(t), d.log(t))
	}
}

func TestInstallRefusesABinaryThatNamesAnotherRelease(t *testing.T) {
	d := newDLInstall(t)
	// Served under 0.4.0's name, but built as 0.3.9.
	site := serveRelease(t, "0.4.0", fakeMuster("0.3.9", "ok"), "", "")
	out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL}, "0.4.0")
	if code != 1 || !strings.Contains(out, "not v0.4.0") || d.installedVersion(t) != "" {
		t.Fatalf("a mislabelled binary must be refused (%d):\n%s", code, out)
	}
}

func TestInstallSendsTheTokenAsAHeaderAndNeverInAnArgument(t *testing.T) {
	d := newDLInstall(t)
	site := serveRelease(t, "0.4.0", fakeMuster("0.4.0", "ok"), "read-token", "")
	// curl is wrapped so its argument list is recorded: the token must not be in it.
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not available")
	}
	wrap := "#!/bin/sh\necho \"$*\" >> \"$HOME/curl.args\"\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(d.bin, "curl"), []byte(wrap), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL}, "0.4.0"); code == 0 {
		t.Fatalf("a locked host must refuse an install with no token:\n%s", out)
	}
	out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL, "DL_TOKEN": "read-token"}, "0.4.0")
	if code != 0 {
		t.Fatalf("install with a token failed (%d):\n%s", code, out)
	}
	args, _ := os.ReadFile(filepath.Join(d.home, "curl.args"))
	if strings.Contains(string(args), "read-token") {
		t.Errorf("the token is on curl's command line:\n%s", args)
	}
}

func TestInstallRefusesWhatItCannotBuildFor(t *testing.T) {
	d := newDLInstall(t)
	site := serveRelease(t, "0.4.0", fakeMuster("0.4.0", "ok"), "", "")
	out, code := d.run(t, map[string]string{"DL_URL": site.srv.URL, "FAKE_ARCH": "riscv64"}, "0.4.0")
	if code != 1 || !strings.Contains(out, "no build for Linux/riscv64") {
		t.Fatalf("an unsupported platform must say so (%d):\n%s", code, out)
	}
	// The template in the repository carries no address; an unstamped copy says how to get one.
	out, code = d.run(t, map[string]string{"DL_URL": ""}, "0.4.0")
	if code != 1 || !strings.Contains(out, "no host address") {
		t.Fatalf("an unstamped copy must refuse (%d):\n%s", code, out)
	}
	out, code = d.run(t, map[string]string{"DL_URL": site.srv.URL}, "not-a-version")
	if code != 1 || !strings.Contains(out, "is not a version") {
		t.Fatalf("a malformed version must be refused (%d):\n%s", code, out)
	}
}

// ---- the dispatch guard -------------------------------------------------------------------------------------------------

// dispatchFixture is a bare origin with main carrying the publish workflow at v1.2.0 (a final), v1.1.0 (a final cut
// before the workflow existed), and a final v9.0.0 on a side branch main never reaches.
func dispatchFixture(t *testing.T) string {
	t.Helper()
	requireTool(t, "git", "bash")
	root := t.TempDir()
	origin, work := filepath.Join(root, "origin.git"), filepath.Join(root, "work")
	gitIn(t, root, "init", "--bare", "-b", "main", origin)
	gitIn(t, root, "init", "-b", "main", work)
	gitIn(t, work, "remote", "add", "origin", origin)
	commit := func(msg string, files map[string]string) {
		for p, c := range files {
			full := filepath.Join(work, p)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		gitIn(t, work, "add", "-A")
		gitIn(t, work, "commit", "-m", msg)
	}
	commit("old", map[string]string{"f": "1"})
	gitIn(t, work, "tag", "-a", "v1.1.0", "-m", "final")
	commit("workflow", map[string]string{".github/workflows/publish-downloads.yml": "name: x\n"})
	gitIn(t, work, "tag", "-a", "v1.2.0-rc.1", "-m", "rc")
	gitIn(t, work, "tag", "-a", "v1.2.0", "-m", "final")
	gitIn(t, work, "push", "origin", "main", "--tags")
	gitIn(t, work, "checkout", "-b", "side")
	commit("side", map[string]string{"g": "2"})
	gitIn(t, work, "tag", "-a", "v9.0.0", "-m", "x")
	gitIn(t, work, "push", "origin", "side", "--tags")
	gitIn(t, work, "checkout", "main")
	gitIn(t, work, "fetch", "origin")
	return work
}

func runDispatchGuard(t *testing.T, work, ghDir string, env []string, args ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs("scripts/release-downloads-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "PATH="+ghDir+":"+os.Getenv("PATH"), "REPO=owner/repo", "RETRY_UNIT=0", "RETRY_MAX=2",
		"JUST_FINALIZED=", "PUBLISH_TAG=", "CATCH_UP=0")
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

func TestDispatchGuardHandsOverEachFinalOnce(t *testing.T) {
	work := dispatchFixture(t)
	dir := t.TempDir()
	run := func(env ...string) string {
		out, code := runDispatchGuard(t, work, dir, env, "tags")
		if code != 0 {
			t.Fatalf("tags failed (%d):\n%s", code, out)
		}
		return strings.TrimSpace(out)
	}
	if got := run("JUST_FINALIZED=v1.2.0", "PUBLISH_TAG=v1.2.0", "CATCH_UP=1"); got != "v1.2.0" {
		t.Errorf("the same final named three ways must come out once, got %q", got)
	}
	if got := run("PUBLISH_TAG=v1.2.0-rc.1"); got != "" {
		t.Errorf("a candidate is not handed over, got %q", got)
	}
	if got := run("JUST_FINALIZED=v1.1.0", "PUBLISH_TAG=v1.2.0"); got != "v1.1.0\nv1.2.0" {
		t.Errorf("two different finals, in the order named, got %q", got)
	}
	// The catch-up is the newest final main REACHES: not v9.0.0 on a side branch, and never an older one.
	if got := run("CATCH_UP=1"); got != "v1.2.0" {
		t.Errorf("catch-up = %q, want v1.2.0", got)
	}
	if got := run(); got != "" {
		t.Errorf("a run that names nothing and does not catch up hands over nothing, got %q", got)
	}
}

func TestDispatchGuardStartsThePublishWorkflowAtTheTag(t *testing.T) {
	work := dispatchFixture(t)
	dir := t.TempDir()
	fakeTool(t, dir, "gh", "0|0", "0|")
	out, code := runDispatchGuard(t, work, dir, nil, "dispatch", "v1.2.0")
	if code != 0 || !strings.Contains(out, "dispatched") {
		t.Fatalf("dispatch (%d):\n%s", code, out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "gh.args"))
	args := string(b)
	for _, want := range []string{
		"run list --repo owner/repo --workflow publish-downloads.yml --branch v1.2.0 --status success",
		"workflow run publish-downloads.yml --repo owner/repo --ref v1.2.0 -f tag=v1.2.0",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("gh was not asked %q:\n%s", want, args)
		}
	}
}

func TestDispatchGuardSkipsWhatIsAlreadyPublishedOrCannotBe(t *testing.T) {
	work := dispatchFixture(t)

	// An earlier run at this tag succeeded: the host already said published, so nothing is started.
	dir := t.TempDir()
	fakeTool(t, dir, "gh", "0|1", "0|")
	out, code := runDispatchGuard(t, work, dir, nil, "dispatch", "v1.2.0")
	if code != 0 || !strings.Contains(out, "already has a successful run") || calls(t, dir, "gh") != 1 {
		t.Errorf("a published tag must not be dispatched again (%d, %d gh calls):\n%s", code, calls(t, dir, "gh"), out)
	}

	// A dry run's success does not count: the run list is filtered by title.
	b, _ := os.ReadFile(filepath.Join(dir, "gh.args"))
	if !strings.Contains(string(b), `contains("dry run") | not`) {
		t.Errorf("the run list must leave dry runs out:\n%s", b)
	}

	// A final cut before the workflow existed is skipped, and gh is never asked.
	dir = t.TempDir()
	fakeTool(t, dir, "gh", "0|0", "0|")
	out, code = runDispatchGuard(t, work, dir, nil, "dispatch", "v1.1.0")
	if code != 0 || !strings.Contains(out, "before .github/workflows/publish-downloads.yml existed") || calls(t, dir, "gh") != 0 {
		t.Errorf("an old final must be skipped without a call (%d, %d gh calls):\n%s", code, calls(t, dir, "gh"), out)
	}
}

func TestDispatchGuardRefusesATagThatIsNotAFinalOnMain(t *testing.T) {
	work := dispatchFixture(t)
	for _, tag := range []string{"v1.2.0-rc.1", "v9.0.0", "v7.7.7", "main"} {
		dir := t.TempDir()
		fakeTool(t, dir, "gh", "0|0", "0|")
		out, code := runDispatchGuard(t, work, dir, nil, "dispatch", tag)
		if code == 0 || calls(t, dir, "gh") != 0 {
			t.Errorf("%s must be refused without a gh call (%d, %d calls):\n%s", tag, code, calls(t, dir, "gh"), out)
		}
	}
}

func TestDispatchGuardRetriesATransientErrorAndFailsClosedOtherwise(t *testing.T) {
	work := dispatchFixture(t)

	dir := t.TempDir()
	fakeTool(t, dir, "gh", "0|0", gh503, "0|") // list ok, dispatch 503 once, then ok
	if out, code := runDispatchGuard(t, work, dir, nil, "dispatch", "v1.2.0"); code != 0 || calls(t, dir, "gh") != 3 {
		t.Errorf("a single 503 on the dispatch must be absorbed (%d, %d calls):\n%s", code, calls(t, dir, "gh"), out)
	}

	// The earlier runs cannot be read: do not guess either way.
	dir = t.TempDir()
	fakeTool(t, dir, "gh", gh503)
	out, code := runDispatchGuard(t, work, dir, nil, "dispatch", "v1.2.0")
	if code == 0 || strings.Contains(string(mustRead(t, filepath.Join(dir, "gh.args"))), "workflow run") {
		t.Errorf("an unreadable run list must fail without dispatching (%d):\n%s", code, out)
	}

	// The dispatch itself never works.
	dir = t.TempDir()
	fakeTool(t, dir, "gh", "0|0", gh503)
	if out, code = runDispatchGuard(t, work, dir, nil, "dispatch", "v1.2.0"); code == 0 || !strings.Contains(out, "could not start") {
		t.Errorf("a dispatch that never lands must fail (%d):\n%s", code, out)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- the shape of the workflows -----------------------------------------------------------------------------------------

// These read the YAML as text, like the neighbouring workflow tests: they pin the properties the host's contract and
// this repository's privacy rule depend on, which nothing else would notice losing.
func TestPublishDownloadsWorkflowKeepsTheKeyAndTheRunnerWhereTheyBelong(t *testing.T) {
	wf := string(mustRead(t, ".github/workflows/publish-downloads.yml"))

	// Started by hand or by dispatch at a tag, and by nothing else.
	if !strings.Contains(wf, "\non:\n  workflow_dispatch:\n") || strings.Contains(wf, "workflow_call") || strings.Contains(wf, "\n  push:") || strings.Contains(wf, "schedule:") {
		t.Error("publish-downloads.yml must be workflow_dispatch only: a reusable call would run at the caller's ref, which the host refuses")
	}
	// The runner is a repository variable, never a label written here.
	if strings.Contains(wf, "self-hosted") || !strings.Contains(wf, "runs-on: ${{ fromJSON(needs.build.outputs.runner) }}") {
		t.Error("the publish runner must come from the PUBLISH_RUNNER variable through the build job's output")
	}
	// The OIDC permission, the signing key and the environment exist once each, in the publish job.
	jobs := strings.SplitN(wf, "\n  publish:\n", 2)
	if len(jobs) != 2 {
		t.Fatal("no publish job")
	}
	build, publish := jobs[0], jobs[1]
	if strings.Contains(build, "${{ secrets.") || strings.Contains(build, "id-token: write") {
		t.Error("the build job runs on a hosted runner and must hold no secret and no OIDC permission")
	}
	for _, want := range []string{"environment: release", "id-token: write", "secrets.PUBLISH_SIGNING_KEY"} {
		if strings.Count(wf, want) != 1 || !strings.Contains(publish, want) {
			t.Errorf("%q must appear exactly once, in the publish job", want)
		}
	}
	// Caller-controlled text goes through env, never into a script.
	if strings.Contains(wf, "run: node") && strings.Contains(wf, "${{ inputs.tag }}\"") {
		t.Error("an input is interpolated into a script")
	}
}

func TestReleaseAutoDispatchesThePublishWorkflowAtTheTagNeverCallsIt(t *testing.T) {
	auto := string(mustRead(t, ".github/workflows/release-auto.yml"))
	if strings.Contains(auto, "uses: ./.github/workflows/publish-downloads.yml") {
		t.Error("release-auto.yml must DISPATCH publish-downloads.yml at the tag; a reusable call would run at main's ref")
	}
	for _, want := range []string{"dispatch-downloads:", "scripts/release-downloads-guard.sh tags", "scripts/release-downloads-guard.sh dispatch", "actions: write", "vars.PUBLISH_ORIGIN != ''"} {
		if !strings.Contains(auto, want) {
			t.Errorf("release-auto.yml must carry %q", want)
		}
	}
	// The npm publish paths are as they were: the host never gates them.
	for _, want := range []string{"publish-candidate:", "publish-final:", "publish-existing:", "uses: ./.github/workflows/release-npm.yml"} {
		if !strings.Contains(auto, want) {
			t.Errorf("release-auto.yml lost %q", want)
		}
	}
}
