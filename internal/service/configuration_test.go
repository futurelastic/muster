package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// muster #392: a session whose configuration is only what the create names.

func TestHealth_ReportsSupportsIsolateConfiguration(t *testing.T) {
	_, srv := newTestServer(t)
	resp, err := http.DefaultClient.Do(authedRequest(t, http.MethodGet, srv.URL+"/v1/health", nil))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := body["supportsIsolateConfiguration"].(bool); !ok || !got {
		t.Errorf("supportsIsolateConfiguration = %v (present=%v), want true", body["supportsIsolateConfiguration"], ok)
	}
}

// The posture spelling lowers onto the per-runtime field, and asking for it
// carries nothing else: no sandbox, no mode.
func TestCreatePosture_GivenConfigurationLowersOntoTheRuntimesOwnField(t *testing.T) {
	conf, _, srv := postureServer(t, nil)
	code, _, raw := carryCreate(t, srv, testToken, "cfg-1",
		`{"runtime":"confined","cwd":"/w","posture":{"configuration":"given"}}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	spec := conf.last()
	if !spec.IsolateConfiguration {
		t.Errorf("spec = %+v, want IsolateConfiguration", spec)
	}
	if spec.Sandbox != nil || spec.PermissionMode != "" || spec.IsolateEnvironment {
		t.Errorf("spec = %+v: only the configuration was asked for", spec)
	}
	// "inherit" and absence are today's behaviour.
	for i, body := range []string{
		`{"runtime":"confined","cwd":"/w","posture":{"configuration":"inherit"}}`,
		`{"runtime":"confined","cwd":"/w","posture":{}}`,
		`{"runtime":"confined","cwd":"/w"}`,
	} {
		if code, _, raw := carryCreate(t, srv, testToken, fmt.Sprintf("cfg-inh-%d", i), body); code != http.StatusCreated {
			t.Fatalf("%s: status = %d: %s", body, code, raw)
		}
		if conf.last().IsolateConfiguration {
			t.Errorf("%s arrived as IsolateConfiguration", body)
		}
	}
}

// A runtime that cannot give the guarantee refuses with the reason, before the
// driver is asked — never a session that loads the account's own configuration.
func TestCreatePosture_GivenConfigurationRefusedWhereTheRuntimeCannot(t *testing.T) {
	_, plain, srv := postureServer(t, nil)
	code, _, raw := carryCreate(t, srv, testToken, "cfg-2",
		`{"runtime":"plain","cwd":"/w","posture":{"configuration":"given"}}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", code, raw)
	}
	e := postureErr(t, raw)
	if e.Kind != fleet.ErrorUnsupported || e.Reason != fleet.ReasonPostureUnenforceable {
		t.Errorf("error = %+v", e)
	}
	if !strings.Contains(e.Message, "configuration the create names") {
		t.Errorf("message %q does not say what is missing", e.Message)
	}
	if len(plain.specs) != 0 {
		t.Errorf("the driver was asked to create after the refusal: %+v", plain.specs)
	}
}

// The per-runtime field is the spelling a relay forwards: it reaches the driver,
// needs only create, and is exclusive with posture like the three before it.
func TestCreate_PassesIsolateConfigurationToTheDriverAndExcludesPosture(t *testing.T) {
	conf, _, srv := postureServer(t, nil)
	code, _, raw := carryCreate(t, srv, testToken, "cfg-3", `{"runtime":"confined","cwd":"/w","isolateConfiguration":true}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if !conf.last().IsolateConfiguration {
		t.Error("the driver's spec lost isolateConfiguration")
	}
	n := len(conf.specs)
	code, _, raw = carryCreate(t, srv, testToken, "cfg-4",
		`{"runtime":"confined","cwd":"/w","isolateConfiguration":true,"posture":{}}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, "posture cannot be combined") {
		t.Errorf("status = %d: %s", code, raw)
	}
	if len(conf.specs) != n {
		t.Error("a refused create reached the driver")
	}
	// It narrows, so a principal holding only create may send it.
	if got := createNeedsSend(createSessionBody{IsolateConfiguration: true}); got != "" {
		t.Errorf("isolateConfiguration needs %q; it narrows and grants nothing", got)
	}
}

func TestPostureDryRunAndRuntimes_ReportGivenConfiguration(t *testing.T) {
	_, _, srv := postureServer(t, nil)
	code, raw := postureGet(t, srv, http.MethodPost, "/v1/posture/dry-run", `{"posture":{"configuration":"given"}}`)
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
	if c := rows["confined"]; !c.Enforceable || c.Mapping == nil || !c.Mapping.IsolateConfiguration {
		t.Errorf("confined row = %+v", c)
	}
	if p := rows["plain"]; p.Enforceable || !strings.Contains(p.Reason, "configuration the create names") {
		t.Errorf("plain row = %+v", p)
	}

	code, raw = postureGet(t, srv, http.MethodGet, "/v1/runtimes", "")
	if code != http.StatusOK {
		t.Fatalf("runtimes status = %d: %s", code, raw)
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
	if p := got["confined"]; p == nil || !p.GivenConfiguration {
		t.Errorf("confined posture = %+v", p)
	}
	if p := got["plain"]; p == nil || p.GivenConfiguration {
		t.Errorf("plain posture = %+v", p)
	}
}

func configCarryServer(t *testing.T) (*Service, *confinedDriver, *carryDriverImpl, *httptest.Server) {
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
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	return svc, conf, plain, srv
}

func configResume(conv, posture string) string {
	if posture == "" {
		return fmt.Sprintf(`{"runtime":"confined","cwd":"/w","resume":%q}`, conv)
	}
	return fmt.Sprintf(`{"runtime":"confined","cwd":"/w","resume":%q,"posture":%s}`, conv, posture)
}

// The headline case: a resume must not come back loading the owner's tool
// servers. Silence about the configuration is not a request to widen it.
func TestResumeCarriesGivenConfiguration(t *testing.T) {
	_, conf, _, srv := configCarryServer(t)
	if code, _, raw := carryCreate(t, srv, testToken, "r1", fmt.Sprintf(
		`{"runtime":"confined","cwd":"/w","conversationId":%q,"posture":{"configuration":"given"}}`, convBypass)); code != http.StatusCreated {
		t.Fatalf("first create = %d %s", code, raw)
	}

	code, sess, raw := carryCreate(t, srv, testToken, "r2", configResume(convBypass, ""))
	if code != http.StatusCreated {
		t.Fatalf("resume = %d %s", code, raw)
	}
	if !conf.last().IsolateConfiguration {
		t.Error("the resumed session was launched without isolateConfiguration")
	}
	if sess.Carried == nil || !sess.Carried.IsolateConfiguration {
		t.Errorf("carried = %+v, want isolateConfiguration reported", sess.Carried)
	}

	// A posture that says nothing about configuration is silence too.
	if code, sess, raw := carryCreate(t, srv, testToken, "r3", configResume(convBypass, `{"attendance":"attended"}`)); code != http.StatusCreated ||
		!conf.last().IsolateConfiguration || sess.Carried == nil || !sess.Carried.IsolateConfiguration {
		t.Errorf("resume with an unrelated posture = %d %s, want it carried", code, raw)
	}

	// The chain continues: the carry itself is recorded for the next resume.
	if code, sess, raw := carryCreate(t, srv, testToken, "r4", configResume(convBypass, "")); code != http.StatusCreated ||
		sess.Carried == nil || !sess.Carried.IsolateConfiguration {
		t.Errorf("third resume = %d %s", code, raw)
	}
}

// The way back is in words, and it is recorded: after an explicit "inherit" the
// conversation no longer carries.
func TestResumeOptsOutWithExplicitInherit(t *testing.T) {
	_, conf, _, srv := configCarryServer(t)
	if code, _, raw := carryCreate(t, srv, testToken, "o1", fmt.Sprintf(
		`{"runtime":"confined","cwd":"/w","conversationId":%q,"posture":{"configuration":"given"}}`, convBypass)); code != http.StatusCreated {
		t.Fatalf("first create = %d %s", code, raw)
	}
	code, sess, raw := carryCreate(t, srv, testToken, "o2", configResume(convBypass, `{"configuration":"inherit"}`))
	if code != http.StatusCreated {
		t.Fatalf("resume = %d %s", code, raw)
	}
	if conf.last().IsolateConfiguration || sess.Carried != nil {
		t.Errorf("an explicit inherit was overridden: spec=%+v carried=%+v", conf.last(), sess.Carried)
	}
	if code, sess, raw := carryCreate(t, srv, testToken, "o3", configResume(convBypass, "")); code != http.StatusCreated ||
		conf.last().IsolateConfiguration || sess.Carried != nil {
		t.Errorf("after an opt-out the next resume carried again: %d %s", code, raw)
	}
}

// A conversation that was never launched this way carries nothing, and says so
// by omission.
func TestResumeOfAnOrdinaryConversationCarriesNoConfiguration(t *testing.T) {
	_, conf, _, srv := configCarryServer(t)
	if code, _, raw := carryCreate(t, srv, testToken, "n1", fmt.Sprintf(
		`{"runtime":"confined","cwd":"/w","conversationId":%q}`, convBypass)); code != http.StatusCreated {
		t.Fatalf("first create = %d %s", code, raw)
	}
	code, sess, raw := carryCreate(t, srv, testToken, "n2", configResume(convBypass, ""))
	if code != http.StatusCreated {
		t.Fatalf("resume = %d %s", code, raw)
	}
	if conf.last().IsolateConfiguration || sess.Carried != nil {
		t.Errorf("spec=%+v carried=%+v, want nothing", conf.last(), sess.Carried)
	}
}

// A runtime that can no longer give the guarantee refuses the resume — a
// silent resume loading the account's configuration is the leak itself — and the
// refusal names the way out.
func TestResumeRefusedWhereTheRuntimeCannotKeepTheConfiguration(t *testing.T) {
	svc, _, plain, srv := configCarryServer(t)
	svc.history.created("plain", fleet.Session{SessionRef: fleet.SessionRef{ID: "s1"}}, &launchFacts{
		IsolateConfiguration: true, Conversation: convBypass})

	body := fmt.Sprintf(`{"runtime":"plain","cwd":"/w","resume":%q}`, convBypass)
	code, _, raw := carryCreate(t, srv, testToken, "x1", body)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", code, raw)
	}
	e := postureErr(t, raw)
	if e.Kind != fleet.ErrorUnsupported || e.Reason != fleet.ReasonPostureUnenforceable ||
		!strings.Contains(e.Message, `"inherit"`) {
		t.Errorf("error = %+v, want unsupported / posture-unenforceable naming inherit", e)
	}
	if len(plain.specs) != 0 {
		t.Errorf("the driver was asked to create after the refusal: %+v", plain.specs)
	}
	// Saying inherit resumes it, with the account's configuration.
	code, _, raw = carryCreate(t, srv, testToken, "x2",
		fmt.Sprintf(`{"runtime":"plain","cwd":"/w","resume":%q,"posture":{"configuration":"inherit"}}`, convBypass))
	if code != http.StatusCreated {
		t.Fatalf("resume with inherit = %d %s", code, raw)
	}
}

// The launch record survives a restart with the new fact in it.
func TestLaunchRecordKeepsIsolateConfiguration(t *testing.T) {
	svc := New("testbox")
	svc.history.created("stub", fleet.Session{SessionRef: fleet.SessionRef{ID: "s1"}}, &launchFacts{
		IsolateConfiguration: true, Conversation: convBypass})
	rec, ok := svc.history.launchFor("stub", convBypass)
	if !ok || !rec.IsolateConfiguration {
		t.Fatalf("launchFor = %+v %v", rec, ok)
	}
	// A later launch of the same conversation that drops it is a change, and is
	// written — otherwise the opt-out would never reach the next resume.
	svc.history.created("stub", fleet.Session{SessionRef: fleet.SessionRef{ID: "s2"}}, &launchFacts{Conversation: convBypass})
	if rec, _ := svc.history.launchFor("stub", convBypass); rec.IsolateConfiguration {
		t.Errorf("record still carries isolateConfiguration after a launch without it: %+v", rec)
	}
}
