package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/createpolicy"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #438: the operator's sandbox policy at a create — what an omitted sandbox
// is given, what a required one holds a create's own to, and what a runtime that
// cannot enforce one does.

// observedPlainDriver is a runtime that has been reached and cannot confine a
// session: the shape of the multiplexer runtime on a host with no sandbox.
type observedPlainDriver struct{ carryDriverImpl }

func (d *observedPlainDriver) Capabilities() fleet.DriverCapabilities {
	c := d.carryDriverImpl.Capabilities()
	c.Source = fleet.CapabilitiesObserved
	c.SandboxUnavailable = "no mechanism on this host"
	return c
}

// openOnlyDriver confines but cannot close the network.
type openOnlyDriver struct{ confinedDriver }

func (d *openOnlyDriver) Capabilities() fleet.DriverCapabilities {
	c := d.confinedDriver.Capabilities()
	c.Sandbox = &fleet.SandboxSupport{Mechanism: "test", Network: []fleet.SandboxNetwork{fleet.SandboxNetworkOpen}}
	return c
}

type policyRig struct {
	svc       *Service
	confined  *confinedDriver
	plain     *observedPlainDriver
	openOnly  *openOnlyDriver
	spec      *specDriver
	srv       *httptest.Server
	idemCount int
}

func newPolicyRig(t *testing.T, policy createpolicy.Policy) *policyRig {
	t.Helper()
	svc := New("testbox")
	r := &policyRig{svc: svc,
		confined: &confinedDriver{carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}},
		plain:    &observedPlainDriver{carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}},
		openOnly: &openOnlyDriver{confinedDriver{carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}}},
		spec:     &specDriver{confinedDriver{carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}}},
	}
	for id, d := range map[fleet.RuntimeId]driver.Driver{"confined": r.confined, "plain": r.plain, "openonly": r.openOnly, "spec": r.spec} {
		if err := svc.RegisterLocalDriver(id, d); err != nil {
			t.Fatal(err)
		}
	}
	if policy != nil {
		if err := svc.SetCreatePolicy(policy); err != nil {
			t.Fatal(err)
		}
	}
	r.srv = httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(r.srv.Close)
	return r
}

// create sends a create on runtime with extra body fields, and returns the status
// and the decoded error (zero on success).
func (r *policyRig) create(t *testing.T, runtime, extra string) (int, fleet.Error, string) {
	t.Helper()
	r.idemCount++
	body := `{"runtime":"` + runtime + `","cwd":"/w"`
	if extra != "" {
		body += "," + extra
	}
	body += "}"
	code, _, raw := carryCreate(t, r.srv, testToken, "sp-"+runtime+"-"+string(rune('a'+r.idemCount)), body)
	var e fleet.Error
	if code != http.StatusCreated {
		var env struct {
			Error fleet.Error `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &env); err == nil {
			e = env.Error
		}
	}
	return code, e, raw
}

func sandboxRule(mode createpolicy.SandboxMode, scope sessionenv.Scope, spec fleet.SandboxSpec) createpolicy.Rule {
	return createpolicy.Rule{AppliesTo: scope, Sandbox: &createpolicy.SandboxRule{Mode: mode, Spec: spec}}
}

var workProfile = fleet.SandboxSpec{
	ReadPaths:  []fleet.AbsolutePath{"/srv/data"},
	WritePaths: []fleet.AbsolutePath{"/srv/work"},
	Network:    fleet.SandboxNetworkClosed,
}

func TestSandboxPolicy_ADefaultIsGivenToACreateThatNamesNone(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxDefault, sessionenv.Scope{}, workProfile)})
	if code, _, raw := r.create(t, "confined", ""); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	sb := r.confined.last().Sandbox
	if sb == nil || sb.Network != fleet.SandboxNetworkClosed || len(sb.ReadPaths) != 1 || sb.ReadPaths[0] != "/srv/data" ||
		len(sb.WritePaths) != 1 || sb.WritePaths[0] != "/srv/work" {
		t.Fatalf("the driver was handed %+v, want the policy's profile", sb)
	}
	// What the driver was handed is a copy: a create's own path lists are appended
	// to by the service, and must never reach back into the policy.
	if code, _, raw := r.create(t, "confined", ""); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	sb.ReadPaths[0] = "/mutated"
	if got, _ := r.svc.createPolicySnapshot().SandboxFor("confined", fleet.SessionSpec{}); got.Spec.ReadPaths[0] != "/srv/data" {
		t.Errorf("the policy's own profile was reached through a create's copy: %v", got.Spec.ReadPaths)
	}
}

func TestSandboxPolicy_ADefaultLeavesACreatesOwnSandboxAlone(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxDefault, sessionenv.Scope{}, workProfile)})
	// Wider than the default in every way: a default is what a caller who did not
	// ask is given, not a limit on one who did.
	code, _, raw := r.create(t, "confined", `"sandbox":{"readPaths":["/elsewhere"],"network":"open"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	sb := r.confined.last().Sandbox
	if sb == nil || sb.Network != fleet.SandboxNetworkOpen || len(sb.ReadPaths) != 1 || sb.ReadPaths[0] != "/elsewhere" || len(sb.WritePaths) != 0 {
		t.Fatalf("the create's own sandbox was replaced: %+v", sb)
	}
}

func TestSandboxPolicy_ADefaultIsNotAppliedWhereTheRuntimeCannotEnforceIt(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxDefault, sessionenv.Scope{}, workProfile)})
	code, _, raw := r.create(t, "plain", "")
	if code != http.StatusCreated {
		t.Fatalf("a default must not refuse a create that asked for nothing: %d %s", code, raw)
	}
	if sb := r.plain.last().Sandbox; sb != nil {
		t.Errorf("a runtime that cannot confine was handed %+v", sb)
	}
	// Nor where it can confine but not close the network: the profile is not
	// something this runtime can enforce, so it is not applied half way.
	if code, _, raw := r.create(t, "openonly", ""); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if sb := r.openOnly.last().Sandbox; sb != nil {
		t.Errorf("a runtime that cannot close the network was handed %+v", sb)
	}
}

func TestSandboxPolicy_ARequiredProfileIsGivenToACreateThatNamesNone(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxRequired, sessionenv.Scope{}, workProfile)})
	if code, _, raw := r.create(t, "confined", ""); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if sb := r.confined.last().Sandbox; sb == nil || sb.Network != fleet.SandboxNetworkClosed {
		t.Fatalf("the driver was handed %+v, want the required profile", sb)
	}
}

func TestSandboxPolicy_ARequiredProfileOnARuntimeWithoutAMechanismRefusesTheCreate(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxRequired, sessionenv.Scope{}, workProfile)})
	for name, extra := range map[string]string{
		"naming none":        "",
		"naming its own":     `"sandbox":{"network":"closed"}`,
		"naming a posture":   `"posture":{"attendance":"unattended"}`,
		"asking for nothing": `"isolateEnvironment":true`,
	} {
		code, e, raw := r.create(t, "plain", extra)
		if code != http.StatusNotImplemented || e.Kind != fleet.ErrorUnsupported {
			t.Fatalf("%s: %d %s; want 501 unsupported", name, code, raw)
		}
		// A posture the runtime cannot enforce is refused by its own check first,
		// with its own reason; every other spelling carries the policy's.
		if name != "naming a posture" && e.Reason != fleet.ReasonSandboxRequired {
			t.Errorf("%s: reason = %q, want %q (%s)", name, e.Reason, fleet.ReasonSandboxRequired, raw)
		}
	}
	// And a runtime that cannot close the network cannot enforce a closed one.
	code, e, raw := r.create(t, "openonly", "")
	if code != http.StatusNotImplemented || e.Reason != fleet.ReasonSandboxRequired || !strings.Contains(e.Message, "close the network") {
		t.Fatalf("openonly: %d %s; want 501 sandbox-required naming the network", code, raw)
	}
	r.plain.mu.Lock()
	n := len(r.plain.specs)
	r.plain.mu.Unlock()
	if n != 0 {
		t.Errorf("a refused create reached the driver %d time(s)", n)
	}
}

func TestSandboxPolicy_ARequiredProfileMayBeNarrowedAndNeverWidened(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxRequired, sessionenv.Scope{}, workProfile)})
	accepted := map[string]string{
		"the same":                      `"sandbox":{"readPaths":["/srv/data"],"writePaths":["/srv/work"],"network":"closed"}`,
		"fewer paths":                   `"sandbox":{"network":"closed"}`,
		"a directory under a granted":   `"sandbox":{"writePaths":["/srv/work/job-1"],"readPaths":["/srv/data/sub"],"network":"closed"}`,
		"read-only under a writable":    `"sandbox":{"readPaths":["/srv/work/docs"],"network":"closed"}`,
		"a private cache of a readable": `"sandbox":{"network":"closed","packageCache":{"path":"/srv/data/cache","mode":"private"}}`,
	}
	for name, extra := range accepted {
		if code, _, raw := r.create(t, "confined", extra); code != http.StatusCreated {
			t.Errorf("%s: status = %d, want it accepted: %s", name, code, raw)
		}
	}
	refused := map[string]string{
		"an open network":               `"sandbox":{"network":"open"}`,
		"a path outside":                `"sandbox":{"readPaths":["/etc"],"network":"closed"}`,
		"a sibling of a granted path":   `"sandbox":{"readPaths":["/srv/data-2"],"network":"closed"}`,
		"a write of a read-only path":   `"sandbox":{"writePaths":["/srv/data"],"network":"closed"}`,
		"a parent of a granted path":    `"sandbox":{"readPaths":["/srv"],"network":"closed"}`,
		"a shared cache of a read path": `"sandbox":{"network":"closed","packageCache":{"path":"/srv/data/cache","mode":"shared"}}`,
		"a cache outside":               `"sandbox":{"network":"closed","packageCache":{"path":"/srv/cache"}}`,
	}
	for name, extra := range refused {
		code, e, raw := r.create(t, "confined", extra)
		if code != http.StatusBadRequest || e.Kind != fleet.ErrorInvalid || e.Reason != fleet.ReasonSandboxRequired {
			t.Errorf("%s: %d %s; want 400 invalid, reason %q", name, code, raw, fleet.ReasonSandboxRequired)
		}
	}
	// A create that names no network has not asked for an open one: it runs on the
	// network the machine requires (an empty `sandbox` is "the default profile").
	for name, extra := range map[string]string{
		"an empty sandbox":                 `"sandbox":{}`,
		"paths and no network":             `"sandbox":{"readPaths":["/srv/data"]}`,
		"a posture that names no network":  `"posture":{"attendance":"unattended"}`,
		"an agent definition's sandboxing": `"agentSpec":{"name":"a","prompt":"p","access":"sandboxed"}`,
	} {
		runtime := "confined"
		if strings.Contains(extra, "agentSpec") {
			runtime = "spec"
		}
		if code, _, raw := r.create(t, runtime, extra); code != http.StatusCreated {
			t.Errorf("%s: status = %d, want it accepted on the required network: %s", name, code, raw)
			continue
		}
		last := r.confined.last
		if runtime == "spec" {
			last = r.spec.last
		}
		if sb := last().Sandbox; sb == nil || sb.Network != fleet.SandboxNetworkClosed {
			t.Errorf("%s: the driver was handed %+v, want the required closed network", name, sb)
		}
	}
	// What the refusal says is what would have been accepted.
	_, e, _ := r.create(t, "confined", `"sandbox":{"readPaths":["/etc"],"network":"closed"}`)
	if !strings.Contains(e.Message, `"/etc"`) || !strings.Contains(e.Message, "/srv/data") || !strings.Contains(e.Message, "narrow") {
		t.Errorf("the refusal does not name the path and what is granted: %q", e.Message)
	}
}

func TestSandboxPolicy_APostureIsTheCreatesOwnSandbox(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxRequired, sessionenv.Scope{}, workProfile)})
	// Wider than required, spelled as a posture.
	code, e, raw := r.create(t, "confined", `"posture":{"attendance":"unattended","files":{"readPaths":["/etc"]},"network":"closed"}`)
	if code != http.StatusBadRequest || e.Reason != fleet.ReasonSandboxRequired {
		t.Fatalf("%d %s; want a posture's wider path refused", code, raw)
	}
	// A posture that confines nothing leaves the create with no sandbox, so it gets
	// the profile like any create that names none.
	if code, _, raw := r.create(t, "confined", `"posture":{"isolatedEnvironment":true}`); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if sb := r.confined.last().Sandbox; sb == nil || sb.Network != fleet.SandboxNetworkClosed {
		t.Fatalf("a posture that did not confine was left unconfined: %+v", sb)
	}
	// A posture inside the profile is accepted.
	if code, _, raw := r.create(t, "confined", `"posture":{"attendance":"unattended","files":{"readPaths":["/srv/data"]},"network":"closed"}`); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
}

// The selector is the sessionEnv one: a rule scoped to an agent matches only that
// agent, by `agent` or by an agentSpec's name, and a marker only that marker.
func TestSandboxPolicy_ASelectorMatchesOnlyWhatItNames(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{
		sandboxRule(createpolicy.SandboxRequired, sessionenv.Scope{Agents: []string{"untrusted"}}, workProfile),
		sandboxRule(createpolicy.SandboxDefault, sessionenv.Scope{Markers: []string{"batch"}}, fleet.SandboxSpec{Network: fleet.SandboxNetworkOpen}),
	})
	type want struct {
		extra   string
		sandbox bool
		network fleet.SandboxNetwork
	}
	for name, w := range map[string]want{
		"the named agent":      {`"agent":"untrusted"`, true, fleet.SandboxNetworkClosed},
		"an agentSpec by name": {`"agentSpec":{"name":"untrusted","prompt":"p"}`, true, fleet.SandboxNetworkClosed},
		"another agent":        {`"agent":"trusted"`, false, ""},
		"no agent":             {``, false, ""},
		"the marker":           {`"marker":"batch"`, true, fleet.SandboxNetworkOpen},
		"another marker":       {`"marker":"interactive"`, false, ""},
	} {
		if code, _, raw := r.create(t, "spec", w.extra); code != http.StatusCreated {
			t.Fatalf("%s: status = %d: %s", name, code, raw)
		}
		sb := r.spec.last().Sandbox
		switch {
		case w.sandbox && (sb == nil || sb.Network != w.network):
			t.Errorf("%s: sandbox = %+v, want one with network %q", name, sb, w.network)
		case !w.sandbox && sb != nil:
			t.Errorf("%s: sandbox = %+v, want none: the rule names someone else", name, sb)
		}
	}
	// The scoped required rule holds only its own agent to the profile.
	if code, _, raw := r.create(t, "spec", `"agent":"trusted","sandbox":{"network":"open"}`); code != http.StatusCreated {
		t.Errorf("another agent's own sandbox was held to a rule that names someone else: %d %s", code, raw)
	}
	if code, _, raw := r.create(t, "spec", `"agent":"untrusted","sandbox":{"network":"open"}`); code != http.StatusBadRequest {
		t.Errorf("the named agent's wider sandbox was accepted: %d %s", code, raw)
	}
}

// The keys of a rule decide on their own: an access rule that matches does not
// stop a later rule's sandbox from reaching the same create.
func TestSandboxPolicy_AnAccessRuleDoesNotHideALaterSandboxRule(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{
		{AppliesTo: sessionenv.Scope{Agents: []string{"researcher"}}, Access: fleet.AgentAccessAsk},
		sandboxRule(createpolicy.SandboxDefault, sessionenv.Scope{}, workProfile),
	})
	if code, _, raw := r.create(t, "spec", `"agentSpec":{"name":"researcher","prompt":"p"}`); code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if sb := r.spec.last().Sandbox; sb == nil {
		t.Fatal("the access rule that matched hid the sandbox rule behind it")
	}
}

// A definition's knowledge home is a path the session is granted. Under a default
// it is added to the profile the policy gave; under a required one it is a grant
// like any other and must lie inside the profile.
func TestSandboxPolicy_AKnowledgeHomeIsAGrantLikeAnyOther(t *testing.T) {
	const spec = `"agentSpec":{"name":"researcher","prompt":"p","knowledge":{"index":"/srv/mem/MEMORY.md","home":"/srv/mem"}}`

	r := newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxDefault, sessionenv.Scope{}, workProfile)})
	if code, _, raw := r.create(t, "spec", spec); code != http.StatusCreated {
		t.Fatalf("default: status = %d: %s", code, raw)
	}
	sb := r.spec.last().Sandbox
	if sb == nil || !containsPath(sb.ReadPaths, "/srv/mem") || !containsPath(sb.ReadPaths, "/srv/data") {
		t.Fatalf("default: the session was not granted its memory home beside the profile: %+v", sb)
	}

	r = newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxRequired, sessionenv.Scope{}, workProfile)})
	code, e, raw := r.create(t, "spec", spec)
	if code != http.StatusBadRequest || e.Reason != fleet.ReasonSandboxRequired || !strings.Contains(e.Message, "agentSpec.knowledge.home") {
		t.Fatalf("required, home outside the profile: %d %s; want 400 naming the knowledge home", code, raw)
	}

	inside := workProfile
	inside.ReadPaths = []fleet.AbsolutePath{"/srv/data", "/srv/mem"}
	r = newPolicyRig(t, createpolicy.Policy{sandboxRule(createpolicy.SandboxRequired, sessionenv.Scope{}, inside)})
	if code, _, raw := r.create(t, "spec", spec); code != http.StatusCreated {
		t.Fatalf("required, home inside the profile: status = %d: %s", code, raw)
	}
}

func containsPath(ps []fleet.AbsolutePath, want fleet.AbsolutePath) bool {
	for _, p := range ps {
		if p == want {
			return true
		}
	}
	return false
}

func TestRuntimes_ReportTheSandboxPolicyThatReachesEachRuntime(t *testing.T) {
	r := newPolicyRig(t, createpolicy.Policy{
		sandboxRule(createpolicy.SandboxRequired, sessionenv.Scope{Runtimes: []string{"confined"}, Agents: []string{"untrusted"}}, workProfile),
		sandboxRule(createpolicy.SandboxDefault, sessionenv.Scope{}, fleet.SandboxSpec{}),
	})
	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, r.srv.URL+"/v1/runtimes", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows fleet.Collection[fleet.RuntimeInfo]
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	byRuntime := map[fleet.RuntimeId]fleet.RuntimeInfo{}
	for _, row := range rows.Items() {
		byRuntime[row.Runtime] = row
	}

	conf := byRuntime["confined"].SandboxPolicy
	if len(conf) != 2 {
		t.Fatalf("confined: %+v, want both rules in the order they are tried", conf)
	}
	if conf[0].Mode != "required" || conf[0].AppliesTo == nil || len(conf[0].AppliesTo.Agents) != 1 || conf[0].AppliesTo.Agents[0] != "untrusted" ||
		conf[0].Network != fleet.SandboxNetworkClosed || len(conf[0].ReadPaths) != 1 || len(conf[0].WritePaths) != 1 || !conf[0].Enforceable {
		t.Errorf("confined rule 0 = %+v", conf[0])
	}
	if conf[1].Mode != "default" || conf[1].AppliesTo != nil || conf[1].Network != fleet.SandboxNetworkOpen {
		t.Errorf("confined rule 1 = %+v: an unscoped rule has no appliesTo and an unnamed network is open", conf[1])
	}

	// A rule scoped to another runtime does not reach this one, and a runtime that
	// cannot enforce a profile says so instead of leaving a reader to compare.
	plain := byRuntime["plain"].SandboxPolicy
	if len(plain) != 1 || plain[0].Mode != "default" || plain[0].Enforceable {
		t.Errorf("plain: %+v, want only the unscoped default and not enforceable", plain)
	}
	if got := byRuntime["openonly"].SandboxPolicy; len(got) != 1 || !got[0].Enforceable {
		t.Errorf("openonly: %+v: a default with an open network is enforceable there", got)
	}
}

func TestRuntimes_ANoSandboxPolicyIsAnAbsentField(t *testing.T) {
	r := newPolicyRig(t, nil)
	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, r.srv.URL+"/v1/runtimes", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw["items"]), "sandboxPolicy") {
		t.Errorf("a machine with no sandbox rule reports one: %s", raw["items"])
	}
}
