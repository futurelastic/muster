package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/createpolicy"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #509: a create carrying a portable agent definition — what is refused on
// any machine, how `access` is lowered, and what the operator's policy decides.

// specDriver is a runtime that translates an agentSpec (declares it), and
// confines when asked to.
type specDriver struct{ confinedDriver }

func (d *specDriver) Capabilities() fleet.DriverCapabilities {
	c := d.confinedDriver.Capabilities()
	c.SupportsAgentSpec = fleet.Declared(true)
	return c
}

// specPlainDriver translates an agentSpec but cannot confine a session.
type specPlainDriver struct{ carryDriverImpl }

func (d *specPlainDriver) Capabilities() fleet.DriverCapabilities {
	c := d.carryDriverImpl.Capabilities()
	c.Source = fleet.CapabilitiesObserved
	c.SupportsAgentSpec = fleet.Declared(true)
	return c
}

func agentSpecServer(t *testing.T, ps []Principal, policy createpolicy.Policy) (conf *specDriver, plain *specPlainDriver, none *carryDriverImpl, srv *httptest.Server) {
	t.Helper()
	svc := New("testbox")
	conf = &specDriver{confinedDriver{carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}}}
	plain = &specPlainDriver{carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}}
	none = &carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}
	if err := svc.RegisterLocalDriver("confined", conf); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("plain", plain); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("none", none); err != nil {
		t.Fatal(err)
	}
	if policy != nil {
		if err := svc.SetCreatePolicy(policy); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Principals: ps}
	if len(ps) == 0 {
		cfg = Config{Token: testToken, AllowLocalMutations: true}
	}
	srv = httptest.NewServer(NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return conf, plain, none, srv
}

const specBody = `"agentSpec":{"name":"researcher","prompt":"be brief","instructions":["a","b"],` +
	`"knowledge":{"index":"/srv/mem/MEMORY.md","home":"/srv/mem"}`

func specCreate(t *testing.T, srv *httptest.Server, key, runtime, extra string) (int, fleet.Error, string) {
	t.Helper()
	code, _, raw := carryCreate(t, srv, testToken, key, `{"runtime":"`+runtime+`","cwd":"/w",`+extra+`}`)
	var e fleet.Error
	if code != http.StatusCreated {
		e = postureErr(t, raw)
	}
	return code, e, raw
}

func TestAgentSpecCreate_RefusedOnAnyMachine(t *testing.T) {
	_, _, _, srv := agentSpecServer(t, nil, nil)
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"agent and agentSpec", `"agent":"x",` + specBody + `}`, "mutually exclusive"},
		{"two model hints", `"model":"p/m",` + specBody + `,"model":"p/n"}`, "model"},
		{"access with a posture", specBody + `,"access":"sandboxed"},"posture":{"attendance":"unattended","files":{}}`, "cannot be combined"},
		{"access with a permissionMode", specBody + `,"access":"ask"},"permissionMode":"plan"`, "cannot be combined"},
		{"access with a sandbox", specBody + `,"access":"allow"},"sandbox":{}`, "cannot be combined"},
		{"a malformed definition", `"agentSpec":{"name":"Not Valid"}`, "agentSpec.name"},
		{"an index outside its home", `"agentSpec":{"name":"a","knowledge":{"index":"/x/MEMORY.md","home":"/srv/mem"}}`, "inside agentSpec.knowledge.home"},
	}
	for i, c := range cases {
		code, e, raw := specCreate(t, srv, "k-bad-"+string(rune('a'+i)), "confined", c.extra)
		if code != http.StatusBadRequest || e.Kind != fleet.ErrorInvalid || !strings.Contains(e.Message, c.want) {
			t.Errorf("%s: %d %s; want 400 invalid naming %q", c.name, code, raw, c.want)
		}
	}
}

func TestAgentSpecCreate_ARuntimeThatCannotTranslateRefusesWithTheReason(t *testing.T) {
	_, _, none, srv := agentSpecServer(t, nil, nil)
	code, e, raw := specCreate(t, srv, "k-none", "none", specBody+`}`)
	if code != http.StatusNotImplemented || e.Kind != fleet.ErrorUnsupported || e.Reason != fleet.ReasonAgentSpecUnsupported || e.Retryable {
		t.Fatalf("%d %s; want 501 unsupported, reason agent-spec-unsupported, not retryable", code, raw)
	}
	if len(none.specs) != 0 {
		t.Fatal("the driver was asked to create after the refusal")
	}
}

func TestAgentSpecCreate_TheDefinitionReachesTheDriverWithItsModel(t *testing.T) {
	conf, _, _, srv := agentSpecServer(t, nil, nil)
	code, _, raw := specCreate(t, srv, "k-ok", "confined", specBody+`,"model":"provider/model-id"}`)
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, raw)
	}
	got := conf.last()
	if got.AgentSpec == nil || got.AgentSpec.Name != "researcher" || got.AgentSpec.Prompt != "be brief" || len(got.AgentSpec.Instructions) != 2 {
		t.Fatalf("spec = %+v", got.AgentSpec)
	}
	if got.Model != "provider/model-id" {
		t.Errorf("the definition's model must reach the field the driver checks, got %q", got.Model)
	}
	if got.Agent != "" {
		t.Errorf("the service must not invent an agent name, got %q", got.Agent)
	}
	if got.PermissionMode != "" || got.Sandbox != nil {
		t.Errorf("a definition with no access asks for no posture: %+v", got)
	}
}

func TestAgentSpecCreate_AccessIsLoweredOntoTheRuntimesOwnFields(t *testing.T) {
	conf, plain, _, srv := agentSpecServer(t, nil, nil)

	t.Run("ask pins the ordinary mode", func(t *testing.T) {
		code, _, raw := specCreate(t, srv, "k-ask", "confined", specBody+`,"access":"ask"}`)
		if code != http.StatusCreated {
			t.Fatalf("%d %s", code, raw)
		}
		if got := conf.last(); got.PermissionMode != string(fleet.PermissionModeDefault) || got.Sandbox != nil {
			t.Errorf("spec = %+v", got)
		}
	})
	t.Run("allow is bypass, unconfined, with the acceptance consent", func(t *testing.T) {
		code, _, raw := specCreate(t, srv, "k-allow", "plain", specBody+`,"access":"allow"}`)
		if code != http.StatusCreated {
			t.Fatalf("%d %s", code, raw)
		}
		got := plain.last()
		if got.PermissionMode != fleet.PermissionModeBypass || got.Sandbox != nil {
			t.Errorf("spec = %+v", got)
		}
		if len(got.Consents) != 1 || got.Consents[0] != fleet.PromptBypassAcceptance {
			t.Errorf("consents = %v", got.Consents)
		}
	})
	t.Run("sandboxed is bypass inside a sandbox that holds the knowledge home", func(t *testing.T) {
		code, _, raw := specCreate(t, srv, "k-sb", "confined", specBody+`,"access":"sandboxed"}`)
		if code != http.StatusCreated {
			t.Fatalf("%d %s", code, raw)
		}
		got := conf.last()
		if got.PermissionMode != fleet.PermissionModeBypass || got.Sandbox == nil {
			t.Fatalf("spec = %+v", got)
		}
		if len(got.Sandbox.ReadPaths) != 1 || got.Sandbox.ReadPaths[0] != "/srv/mem" || len(got.Sandbox.WritePaths) != 0 {
			t.Errorf("sandbox = %+v, want the home readable and nothing writable", got.Sandbox)
		}
	})
	t.Run("sandboxed with write grants the home writable", func(t *testing.T) {
		code, _, raw := specCreate(t, srv, "k-sbw", "confined",
			`"agentSpec":{"name":"w","knowledge":{"index":"/srv/mem/MEMORY.md","home":"/srv/mem","write":true},"access":"sandboxed"}`)
		if code != http.StatusCreated {
			t.Fatalf("%d %s", code, raw)
		}
		sb := conf.last().Sandbox
		if sb == nil || len(sb.WritePaths) != 1 || sb.WritePaths[0] != "/srv/mem" || len(sb.ReadPaths) != 0 {
			t.Errorf("sandbox = %+v", sb)
		}
	})
	t.Run("sandboxed on a runtime that cannot confine is refused, never weakened", func(t *testing.T) {
		code, e, raw := specCreate(t, srv, "k-sbp", "plain", specBody+`,"access":"sandboxed"}`)
		if code != http.StatusNotImplemented || e.Reason != fleet.ReasonPostureUnenforceable {
			t.Fatalf("%d %s; want 501 posture-unenforceable", code, raw)
		}
		if n := len(plain.specs); n != 1 {
			t.Errorf("the plain driver was asked %d times; only the allow case should have reached it", n)
		}
	})
}

func TestAgentSpecCreate_ACreateSandboxKeepsItsPathsAndGainsTheHome(t *testing.T) {
	conf, _, _, srv := agentSpecServer(t, nil, nil)
	code, _, raw := specCreate(t, srv, "k-own", "confined", specBody+`},"sandbox":{"readPaths":["/srv/other"],"writePaths":["/srv/git"]}`)
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, raw)
	}
	sb := conf.last().Sandbox
	if sb == nil || len(sb.ReadPaths) != 2 || sb.ReadPaths[0] != "/srv/other" || sb.ReadPaths[1] != "/srv/mem" ||
		len(sb.WritePaths) != 1 || sb.WritePaths[0] != "/srv/git" {
		t.Fatalf("sandbox = %+v", sb)
	}
}

func TestAgentSpecCreate_APostureThatConfinesGainsTheHome(t *testing.T) {
	conf, _, _, srv := agentSpecServer(t, nil, nil)
	code, _, raw := specCreate(t, srv, "k-post", "confined", specBody+`},"posture":{"attendance":"unattended","files":{"writePaths":["/srv/git"]}}`)
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, raw)
	}
	sb := conf.last().Sandbox
	if sb == nil || len(sb.ReadPaths) != 1 || sb.ReadPaths[0] != "/srv/mem" || len(sb.WritePaths) != 1 || sb.WritePaths[0] != "/srv/git" {
		t.Fatalf("sandbox = %+v", sb)
	}
}

func TestAgentSpecCreate_AllowAndSandboxedNeedTheSendGrant(t *testing.T) {
	spawner := Principal{Name: "spawner", Token: "tok-spawn", Grants: []Grant{GrantRead, GrantCreate}}
	conf, _, _, srv := agentSpecServer(t, []Principal{spawner}, nil)
	for _, access := range []string{"allow", "sandboxed"} {
		code, _, raw := carryCreate(t, srv, "tok-spawn", "k-grant-"+access, `{"runtime":"confined","cwd":"/w",`+specBody+`,"access":"`+access+`"}}`)
		if code != http.StatusUnauthorized || !strings.Contains(raw, "agentSpec.access") {
			t.Errorf("%s: %d %s; want 401 naming agentSpec.access", access, code, raw)
		}
	}
	if len(conf.specs) != 0 {
		t.Fatal("a refused create reached the driver")
	}
	// A definition that asks for the ordinary posture needs only create.
	code, _, raw := carryCreate(t, srv, "tok-spawn", "k-grant-ask", `{"runtime":"confined","cwd":"/w",`+specBody+`,"access":"ask"}}`)
	if code != http.StatusCreated {
		t.Fatalf("ask: %d %s", code, raw)
	}
}

func TestAgentSpecCreate_TheOperatorsPolicyWinsOverTheCreate(t *testing.T) {
	policy := createpolicy.Policy{
		{AppliesTo: sessionenv.Scope{Agents: []string{"researcher"}, Runtimes: []string{"plain"}}, Access: fleet.AgentAccessAllow},
		{AppliesTo: sessionenv.Scope{Agents: []string{"locked"}}, Access: fleet.AgentAccessSandboxed},
	}
	conf, plain, _, srv := agentSpecServer(t, nil, policy)

	t.Run("the policy raises what a host that cannot confine would refuse", func(t *testing.T) {
		// The client asked for a sandbox this runtime cannot give; the operator says
		// allow for this agent here, so the create goes ahead unconfined.
		code, _, raw := specCreate(t, srv, "k-op1", "plain", specBody+`,"access":"sandboxed"}`)
		if code != http.StatusCreated {
			t.Fatalf("%d %s", code, raw)
		}
		got := plain.last()
		if got.AgentSpec.Access != fleet.AgentAccessAllow || got.PermissionMode != fleet.PermissionModeBypass || got.Sandbox != nil {
			t.Errorf("spec = %+v access %q", got, got.AgentSpec.Access)
		}
	})
	t.Run("the policy confines an agent the client left open", func(t *testing.T) {
		code, _, raw := specCreate(t, srv, "k-op2", "confined", `"agentSpec":{"name":"locked","access":"allow"}`)
		if code != http.StatusCreated {
			t.Fatalf("%d %s", code, raw)
		}
		got := conf.last()
		if got.AgentSpec.Access != fleet.AgentAccessSandboxed || got.Sandbox == nil {
			t.Errorf("spec = %+v", got)
		}
	})
	t.Run("an operator value that cannot be honoured refuses the create", func(t *testing.T) {
		code, e, raw := specCreate(t, srv, "k-op3", "plain", `"agentSpec":{"name":"locked"}`)
		if code != http.StatusNotImplemented || e.Reason != fleet.ReasonPostureUnenforceable {
			t.Fatalf("%d %s; want 501 posture-unenforceable", code, raw)
		}
	})
	t.Run("a posture the create carries cannot stand beside the operator's value", func(t *testing.T) {
		code, e, raw := specCreate(t, srv, "k-op4", "confined", `"agentSpec":{"name":"locked"},"posture":{"attendance":"unattended","files":{}}`)
		if code != http.StatusBadRequest || !strings.Contains(e.Message, "createPolicy") {
			t.Fatalf("%d %s; want 400 naming createPolicy", code, raw)
		}
	})
	t.Run("an agent no rule names keeps the create's own value", func(t *testing.T) {
		code, _, raw := specCreate(t, srv, "k-op5", "confined", `"agentSpec":{"name":"other","access":"ask"}`)
		if code != http.StatusCreated {
			t.Fatalf("%d %s", code, raw)
		}
		if got := conf.last(); got.AgentSpec.Access != fleet.AgentAccessAsk {
			t.Errorf("access = %q", got.AgentSpec.Access)
		}
	})
}

func TestAgentSpecCreate_APolicyCannotLaunderTheSendGrant(t *testing.T) {
	spawner := Principal{Name: "spawner", Token: "tok-spawn", Grants: []Grant{GrantRead, GrantCreate}}
	policy := createpolicy.Policy{{AppliesTo: sessionenv.Scope{Agents: []string{"researcher"}}, Access: fleet.AgentAccessAllow}}
	conf, _, _, srv := agentSpecServer(t, []Principal{spawner}, policy)
	// The client sent no access at all, so the create itself needs no extra grant;
	// the operator's rule is keyed on a name the client picked, and must not hand a
	// principal that may only start sessions a session that needs no permission.
	code, _, raw := carryCreate(t, srv, "tok-spawn", "k-launder", `{"runtime":"confined","cwd":"/w",`+specBody+`}}`)
	if code != http.StatusUnauthorized || !strings.Contains(raw, "createPolicy") {
		t.Fatalf("%d %s; want 401 naming the policy", code, raw)
	}
	if len(conf.specs) != 0 {
		t.Fatal("a refused create reached the driver")
	}
}

func TestSetCreatePolicy_RefusesARuleThatCannotBeOne(t *testing.T) {
	svc := New("testbox")
	if err := svc.SetCreatePolicy(createpolicy.Policy{{Access: "yolo"}}); err == nil {
		t.Fatal("a rule with an unknown access was accepted")
	}
	if err := svc.SetCreatePolicy(nil); err != nil {
		t.Fatalf("no policy is the default: %v", err)
	}
}
