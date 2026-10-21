package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// confinedDriver is a runtime that declares it can confine a session.
type confinedDriver struct{ carryDriverImpl }

func (d *confinedDriver) Capabilities() fleet.DriverCapabilities {
	c := d.carryDriverImpl.Capabilities()
	c.Source = fleet.CapabilitiesObserved
	c.IsolatesEnvironment = true
	c.CreatePermissionModes = []string{string(fleet.PermissionModeDefault), fleet.PermissionModeBypass}
	c.Sandbox = &fleet.SandboxSupport{
		Mechanism: "test",
		Denies:    []fleet.SandboxDeny{fleet.SandboxDenyFiles, fleet.SandboxDenyUnixSockets, fleet.SandboxDenySystemServices},
		Network:   []fleet.SandboxNetwork{fleet.SandboxNetworkOpen, fleet.SandboxNetworkClosed},
	}
	return c
}

// postureServer registers a runtime that confines ("confined") beside one that
// cannot ("plain", the shape of the default multiplexer runtime).
func postureServer(t *testing.T, ps []Principal) (*confinedDriver, *carryDriverImpl, *httptest.Server) {
	t.Helper()
	svc := New("testbox")
	conf := &confinedDriver{carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}}
	plain := &carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}
	if err := svc.RegisterLocalDriver("confined", conf); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("plain", plain); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Principals: ps}
	if len(ps) == 0 {
		cfg = Config{Token: testToken, AllowLocalMutations: true}
	}
	srv := httptest.NewServer(NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return conf, plain, srv
}

func postureErr(t *testing.T, raw string) fleet.Error {
	t.Helper()
	var env struct {
		Error fleet.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return env.Error
}

func TestCreatePosture_LowersOntoTheRuntimesOwnFields(t *testing.T) {
	conf, _, srv := postureServer(t, nil)
	code, _, raw := carryCreate(t, srv, testToken, "p-1", `{"runtime":"confined","cwd":"/w","posture":{
		"attendance":"unattended","files":{"writePaths":["/srv/git"]},"network":"closed","isolatedEnvironment":true}}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	spec := conf.last()
	if spec.PermissionMode != fleet.PermissionModeBypass || !spec.IsolateEnvironment {
		t.Errorf("spec = %+v", spec)
	}
	if spec.Sandbox == nil || spec.Sandbox.Network != fleet.SandboxNetworkClosed ||
		len(spec.Sandbox.WritePaths) != 1 || spec.Sandbox.WritePaths[0] != "/srv/git" {
		t.Errorf("sandbox = %+v", spec.Sandbox)
	}
}

// The default runtime cannot confine, so an unattended posture is refused with
// the reason — and the driver is never asked.
func TestCreatePosture_RefusedWhereTheRuntimeCannotEnforceIt(t *testing.T) {
	_, plain, srv := postureServer(t, nil)
	code, _, raw := carryCreate(t, srv, testToken, "p-2",
		`{"runtime":"plain","cwd":"/w","posture":{"attendance":"unattended"}}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", code, raw)
	}
	e := postureErr(t, raw)
	if e.Kind != fleet.ErrorUnsupported || e.Reason != fleet.ReasonPostureUnenforceable || e.Machine != "testbox" {
		t.Errorf("error = %+v", e)
	}
	if len(plain.specs) != 0 {
		t.Errorf("the driver was asked to create after the refusal: %+v", plain.specs)
	}
}

// An attended posture that asks for nothing is fine on the default runtime.
func TestCreatePosture_AnAttendedPostureStartsOnTheDefaultRuntime(t *testing.T) {
	_, plain, srv := postureServer(t, nil)
	code, _, raw := carryCreate(t, srv, testToken, "p-3", `{"runtime":"plain","cwd":"/w","posture":{"attendance":"attended"}}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if s := plain.last(); s.Sandbox != nil || s.PermissionMode != "" || s.IsolateEnvironment {
		t.Errorf("spec = %+v: an attended posture asked for nothing", s)
	}
}

func TestCreatePosture_ExcludesTheFieldsItReplaces(t *testing.T) {
	conf, _, srv := postureServer(t, nil)
	for name, extra := range map[string]string{
		"sandbox":            `"sandbox":{}`,
		"permissionMode":     `"permissionMode":"bypass"`,
		"isolateEnvironment": `"isolateEnvironment":true`,
	} {
		code, _, raw := carryCreate(t, srv, testToken, "px-"+name,
			`{"runtime":"confined","cwd":"/w","posture":{},`+extra+`}`)
		if code != http.StatusBadRequest || !strings.Contains(raw, "posture cannot be combined") {
			t.Errorf("%s: status = %d: %s", name, code, raw)
		}
	}
	if len(conf.specs) != 0 {
		t.Errorf("a refused create reached the driver: %+v", conf.specs)
	}
	code, _, raw := carryCreate(t, srv, testToken, "px-bad", `{"runtime":"confined","cwd":"/w","posture":{"files":{"writePaths":["/tmp"]}}}`)
	if code != http.StatusBadRequest {
		t.Errorf("a malformed posture: status = %d: %s", code, raw)
	}
}

// Unattended is bypass; it needs the send grant like bypass does.
func TestCreatePosture_UnattendedNeedsTheSendGrant(t *testing.T) {
	spawner := Principal{Name: "spawner", Token: "tok-spawn", Grants: []Grant{GrantRead, GrantCreate}}
	conf, _, srv := postureServer(t, []Principal{spawner})
	code, _, raw := carryCreate(t, srv, "tok-spawn", "pg-1", `{"runtime":"confined","cwd":"/w","posture":{"attendance":"unattended"}}`)
	if code != http.StatusUnauthorized || !strings.Contains(raw, "posture (unattended)") {
		t.Fatalf("status = %d: %s", code, raw)
	}
	code, _, raw = carryCreate(t, srv, "tok-spawn", "pg-2", `{"runtime":"confined","cwd":"/w","posture":{"files":{},"network":"closed"}}`)
	if code != http.StatusCreated {
		t.Fatalf("a confining-only posture narrows and needs only create: status = %d: %s", code, raw)
	}
	if len(conf.specs) != 1 {
		t.Errorf("specs = %d", len(conf.specs))
	}
}

func postureGet(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestRuntimes_ReportWhichPosturesEachCanEnforce(t *testing.T) {
	_, _, srv := postureServer(t, nil)
	code, raw := postureGet(t, srv, http.MethodGet, "/v1/runtimes", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	var col struct {
		Items []fleet.RuntimeInfo `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &col); err != nil {
		t.Fatal(err)
	}
	got := map[fleet.RuntimeId]*fleet.PostureSupport{}
	for _, it := range col.Items {
		got[it.Runtime] = it.Posture
	}
	if p := got["confined"]; p == nil || !p.Unattended || !p.Files || !p.IsolatedEnvironment {
		t.Errorf("confined posture = %+v", p)
	}
	if p := got["plain"]; p == nil || p.Unattended || p.Files || p.IsolatedEnvironment || len(p.Network) != 0 {
		t.Errorf("plain posture = %+v", p)
	}
}

func TestPostureDryRun_ShowsTheMappingPerRuntime(t *testing.T) {
	conf, plain, srv := postureServer(t, nil)
	code, raw := postureGet(t, srv, http.MethodPost, "/v1/posture/dry-run",
		`{"posture":{"attendance":"unattended","files":{"writePaths":["/srv/git"]}}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	var out struct {
		Runtimes []PostureDryRunRow `json:"runtimes"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	rows := map[fleet.RuntimeId]PostureDryRunRow{}
	for _, r := range out.Runtimes {
		rows[r.Runtime] = r
	}
	c := rows["confined"]
	if !c.Enforceable || c.Mapping == nil || c.Mapping.PermissionMode != fleet.PermissionModeBypass ||
		c.Mapping.Sandbox == nil || c.Mapping.Sandbox.WritePaths[0] != "/srv/git" {
		t.Errorf("confined row = %+v", c)
	}
	p := rows["plain"]
	if p.Enforceable || p.Mapping != nil || !strings.Contains(p.Reason, "cannot confine") {
		t.Errorf("plain row = %+v", p)
	}
	if len(conf.specs)+len(plain.specs) != 0 {
		t.Error("a dry run started something")
	}
}

func TestPostureDryRun_RefusesAMalformedRequest(t *testing.T) {
	_, _, srv := postureServer(t, nil)
	for name, body := range map[string]string{
		"no posture":      `{}`,
		"unknown field":   `{"posture":{},"runtime":"x"}`,
		"malformed shape": `{"posture":{"network":"filtered"}}`,
	} {
		if code, raw := postureGet(t, srv, http.MethodPost, "/v1/posture/dry-run", body); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d: %s", name, code, raw)
		}
	}
}
