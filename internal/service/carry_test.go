package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/state"
)

// carryDriverImpl is a driver whose create records the spec it was handed and
// answers the way a runtime does: the session reads back `bypass` when it was
// launched with it and `auto` otherwise (the mode a bare launch lands in on
// the fleet this was measured on), and its conversation is the one it resumed
// or was told to start.
type carryDriverImpl struct {
	stub.Driver
	mu    sync.Mutex
	specs []fleet.SessionSpec
	n     int
}

func (d *carryDriverImpl) Create(_ context.Context, _ fleet.Request, _ string, spec fleet.SessionSpec) (fleet.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs = append(d.specs, spec)
	d.n++
	conv := spec.Resume
	if conv == "" {
		conv = spec.ConversationId
	}
	st := fleet.ObservedState(fleet.StatusIdle, "carryDriver", nil)
	st.PermissionMode = fleet.PermissionModeAuto
	if spec.PermissionMode == fleet.PermissionModeBypass {
		st.PermissionMode = fleet.PermissionModeBypass
	}
	sess := fleet.Session{
		SessionRef: fleet.SessionRef{Machine: spec.Machine, ID: fmt.Sprintf("created-%d", d.n)},
		State:      st,
	}
	if conv != "" {
		sess.Conversation = &fleet.ConversationRef{Known: true, ID: conv, Source: fleet.ConversationCaptured, Evidence: "test fixture"}
	}
	return sess, nil
}

func (d *carryDriverImpl) last() fleet.SessionSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.specs[len(d.specs)-1]
}

const (
	convBypass = "11111111-1111-4111-8111-111111111111"
	convAuto   = "22222222-2222-4222-8222-222222222222"
	convOuter  = "33333333-3333-4333-8333-333333333333"
)

func carryServer(t *testing.T, ps []Principal) (*Service, *carryDriverImpl, *httptest.Server) {
	t.Helper()
	svc := New("testbox")
	d := &carryDriverImpl{Driver: stub.Driver{DeadlineMs: 1000}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Principals: ps}
	if len(ps) == 0 {
		cfg = Config{Token: testToken, AllowLocalMutations: true}
	}
	srv := httptest.NewServer(NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return svc, d, srv
}

func carryCreate(t *testing.T, srv *httptest.Server, token, key, body string) (int, fleet.Session, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/testbox/sessions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var sess fleet.Session
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(raw, &sess); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return resp.StatusCode, sess, string(raw)
}

// observeBypass teaches the history store what a listing would: a session of
// `conv` that this service did not launch, reading back `mode`.
func observeMode(svc *Service, id, conv string, mode fleet.PermissionModeState) {
	svc.history.observe("stub", []fleet.Session{{
		SessionRef:   fleet.SessionRef{Machine: "testbox", ID: id},
		Conversation: &fleet.ConversationRef{Known: true, ID: conv, Source: fleet.ConversationCaptured, Evidence: "test fixture"},
		State:        fleet.SessionState{PermissionMode: mode},
	}}, false, time.Now(), nil)
}

func resumeBody(conv, extra string) string {
	return fmt.Sprintf(`{"runtime":"stub","cwd":"/w","resume":%q%s}`, conv, extra)
}

// The headline case (#256): a conversation whose last session ran in bypass,
// resumed with no permissionMode, comes back in bypass — and the response says
// what it carried, settings included.
func TestResumeCarriesBypassAndSettings(t *testing.T) {
	_, d, srv := carryServer(t, nil)
	if code, _, raw := carryCreate(t, srv, testToken, "k1", fmt.Sprintf(
		`{"runtime":"stub","cwd":"/w","conversationId":%q,"permissionMode":"bypass","settings":{"crossSessionInbound":"accept","other":1}}`, convBypass)); code != 201 {
		t.Fatalf("first create = %d %s", code, raw)
	}

	code, sess, raw := carryCreate(t, srv, testToken, "k2", resumeBody(convBypass, ""))
	if code != 201 {
		t.Fatalf("resume = %d %s", code, raw)
	}
	spec := d.last()
	if spec.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("driver got permissionMode %q, want bypass", spec.PermissionMode)
	}
	if string(spec.Settings) != `{"crossSessionInbound":"accept","other":1}` {
		t.Errorf("driver got settings %s, want the previous launch's", spec.Settings)
	}
	if sess.State.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("the resumed session reads %q, want bypass", sess.State.PermissionMode)
	}
	if sess.Carried == nil || sess.Carried.PermissionMode != fleet.PermissionModeBypass ||
		string(sess.Carried.Settings) != `{"crossSessionInbound":"accept","other":1}` {
		t.Errorf("carried = %+v, want bypass and the settings", sess.Carried)
	}

	// The chain continues: a second restart carries from the first's launch.
	if code, sess, raw := carryCreate(t, srv, testToken, "k3", resumeBody(convBypass, "")); code != 201 ||
		sess.Carried == nil || sess.Carried.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("second resume = %d %s, want bypass carried again", code, raw)
	}
}

// A conversation that ran in the runtime's ordinary mode stays there: nothing
// is carried and the driver is handed no mode.
func TestResumeOfAutoCarriesNothing(t *testing.T) {
	_, d, srv := carryServer(t, nil)
	if code, _, raw := carryCreate(t, srv, testToken, "k1", fmt.Sprintf(`{"runtime":"stub","cwd":"/w","conversationId":%q}`, convAuto)); code != 201 {
		t.Fatalf("first create = %d %s", code, raw)
	}
	code, sess, raw := carryCreate(t, srv, testToken, "k2", resumeBody(convAuto, ""))
	if code != 201 {
		t.Fatalf("resume = %d %s", code, raw)
	}
	if d.last().PermissionMode != "" {
		t.Errorf("driver got permissionMode %q for a conversation that ran in auto", d.last().PermissionMode)
	}
	if sess.Carried != nil {
		t.Errorf("carried = %+v, want nothing", sess.Carried)
	}
	if sess.State.PermissionMode != fleet.PermissionModeAuto {
		t.Errorf("the resumed session reads %q, want auto", sess.State.PermissionMode)
	}
	if strings.Contains(raw, `"carried"`) {
		t.Errorf("an empty carry must not be on the wire: %s", raw)
	}
}

// An explicit mode wins over both: over a carried bypass (the ordinary mode,
// spelled out) and over a carried auto (bypass).
func TestExplicitModeOverridesWhatWouldBeCarried(t *testing.T) {
	_, d, srv := carryServer(t, nil)
	carryCreate(t, srv, testToken, "k1", fmt.Sprintf(
		`{"runtime":"stub","cwd":"/w","conversationId":%q,"permissionMode":"bypass","settings":{"crossSessionInbound":"accept","other":1}}`, convBypass))
	carryCreate(t, srv, testToken, "k2", fmt.Sprintf(`{"runtime":"stub","cwd":"/w","conversationId":%q}`, convAuto))

	code, sess, raw := carryCreate(t, srv, testToken, "k3", resumeBody(convBypass, `,"permissionMode":"default"`))
	if code != 201 {
		t.Fatalf("resume with default = %d %s", code, raw)
	}
	spec := d.last()
	if spec.PermissionMode != "" {
		t.Errorf("driver got permissionMode %q; the explicit ordinary mode must reach it as none", spec.PermissionMode)
	}
	// The bypass-only key stops travelling with the mode; the allow-listed one stays.
	if string(spec.Settings) != `{"crossSessionInbound":"accept"}` {
		t.Errorf("driver got settings %s, want only the allow-listed key", spec.Settings)
	}
	if sess.Carried == nil || sess.Carried.PermissionMode != "" || string(sess.Carried.Settings) != `{"crossSessionInbound":"accept"}` {
		t.Errorf("carried = %+v, want settings only", sess.Carried)
	}
	if sess.State.PermissionMode != fleet.PermissionModeAuto {
		t.Errorf("the session reads %q, want auto", sess.State.PermissionMode)
	}

	code, sess, raw = carryCreate(t, srv, testToken, "k4", resumeBody(convAuto, `,"permissionMode":"bypass"`))
	if code != 201 {
		t.Fatalf("resume with bypass = %d %s", code, raw)
	}
	if d.last().PermissionMode != fleet.PermissionModeBypass || sess.Carried != nil {
		t.Errorf("explicit bypass over auto: spec %q carried %+v, want bypass and nothing carried", d.last().PermissionMode, sess.Carried)
	}

	// Explicit settings win over carried ones, and are not reported as carried.
	// (A fresh conversation: the resume above with the ordinary mode rewrote
	// convBypass's record, as it should.)
	carryCreate(t, srv, testToken, "k4b", fmt.Sprintf(
		`{"runtime":"stub","cwd":"/w","conversationId":%q,"permissionMode":"bypass","settings":{"crossSessionInbound":"accept"}}`, convOuter))
	code, sess, raw = carryCreate(t, srv, testToken, "k5", resumeBody(convOuter, `,"settings":{"crossSessionInbound":"reject"}`))
	if code != 201 {
		t.Fatalf("resume with settings = %d %s", code, raw)
	}
	if string(d.last().Settings) != `{"crossSessionInbound":"reject"}` ||
		sess.Carried == nil || sess.Carried.PermissionMode != fleet.PermissionModeBypass || len(sess.Carried.Settings) != 0 {
		t.Errorf("explicit settings: spec %s carried %+v", d.last().Settings, sess.Carried)
	}
}

// What a session LAST reported beats what it was launched with: a launch in
// bypass whose session was later read back as auto is not carried as bypass.
func TestResumeCarriesWhatTheSessionLastReported(t *testing.T) {
	svc, d, srv := carryServer(t, nil)
	carryCreate(t, srv, testToken, "k1", fmt.Sprintf(`{"runtime":"stub","cwd":"/w","conversationId":%q,"permissionMode":"bypass"}`, convBypass))
	observeMode(svc, "created-1", convBypass, fleet.PermissionModeAuto)

	code, sess, raw := carryCreate(t, srv, testToken, "k2", resumeBody(convBypass, ""))
	if code != 201 {
		t.Fatalf("resume = %d %s", code, raw)
	}
	if d.last().PermissionMode != "" || sess.Carried != nil {
		t.Errorf("spec %q carried %+v, want nothing: the last report was auto", d.last().PermissionMode, sess.Carried)
	}

	// An unreadable mode is silence, not a report: it must not erase the launch's.
	svc2, d2, srv2 := carryServer(t, nil)
	carryCreate(t, srv2, testToken, "k1", fmt.Sprintf(`{"runtime":"stub","cwd":"/w","conversationId":%q,"permissionMode":"bypass"}`, convBypass))
	observeMode(svc2, "created-1", convBypass, fleet.PermissionModeUnknown)
	if code, _, raw := carryCreate(t, srv2, testToken, "k2", resumeBody(convBypass, "")); code != 201 || d2.last().PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("resume = %d %s spec %q, want bypass carried through an unreadable report", code, raw, d2.last().PermissionMode)
	}
}

// Carrying bypass is the widening a create asks for explicitly, so it needs the
// same grant: from the caller, or from the original launch.
func TestResumeCarryNeedsSend(t *testing.T) {
	spawner := Principal{Name: "spawner", Token: "tok-spawn", Grants: []Grant{GrantRead, GrantCreate}}
	driver := Principal{Name: "driver", Token: "tok-drive", Grants: []Grant{GrantRead, GrantCreate, GrantSend}}
	svc, d, srv := carryServer(t, []Principal{spawner, driver})

	// A bypass session this service never launched, as a coordinator's
	// recovery sees it after a restart wave: nobody here vouched for it.
	observeMode(svc, "outside", convOuter, fleet.PermissionModeBypass)

	code, _, raw := carryCreate(t, srv, "tok-spawn", "k1", resumeBody(convOuter, ""))
	if code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("a principal without send carried bypass forward from a session nobody vouched for: %d %s", code, raw)
	}
	if !strings.Contains(raw, "send") || !strings.Contains(raw, `permissionMode \"default\"`) {
		t.Errorf("the refusal must name the grant and the way out: %s", raw)
	}
	if len(d.specs) != 0 {
		t.Errorf("a refused resume reached the driver: %+v", d.specs)
	}

	// A caller holding send may carry it.
	if code, sess, raw := carryCreate(t, srv, "tok-drive", "k3", resumeBody(convOuter, "")); code != 201 ||
		sess.Carried == nil || sess.Carried.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("a principal with send: %d %s, want bypass carried", code, raw)
	}

	// The ordinary mode, asked for out loud, needs nothing beyond create.
	if code, _, raw := carryCreate(t, srv, "tok-spawn", "k2", resumeBody(convOuter, `,"permissionMode":"default"`)); code != 201 {
		t.Errorf("explicit ordinary mode without send = %d %s, want 201", code, raw)
	}

	// And a launch made by a principal holding send vouches for its own resumes:
	// the restarting caller needs no grant of its own.
	if code, _, raw := carryCreate(t, srv, "tok-drive", "k4", fmt.Sprintf(
		`{"runtime":"stub","cwd":"/w","conversationId":%q,"permissionMode":"bypass"}`, convBypass)); code != 201 {
		t.Fatalf("launch = %d %s", code, raw)
	}
	code, sess, raw := carryCreate(t, srv, "tok-spawn", "k5", resumeBody(convBypass, ""))
	if code != 201 || sess.Carried == nil || sess.Carried.PermissionMode != fleet.PermissionModeBypass {
		t.Errorf("resume of a send-launched bypass by a principal without send: %d %s, want it carried", code, raw)
	}
}

// A create that does not resume never reads the record, and a conversation the
// service holds none for carries nothing.
func TestNoResumeNoRecordCarriesNothing(t *testing.T) {
	_, d, srv := carryServer(t, nil)
	carryCreate(t, srv, testToken, "k1", fmt.Sprintf(`{"runtime":"stub","cwd":"/w","conversationId":%q,"permissionMode":"bypass"}`, convBypass))
	if code, sess, raw := carryCreate(t, srv, testToken, "k2", `{"runtime":"stub","cwd":"/w"}`); code != 201 || sess.Carried != nil || d.last().PermissionMode != "" {
		t.Errorf("a plain create: %d %s", code, raw)
	}
	if code, sess, raw := carryCreate(t, srv, testToken, "k3", resumeBody("99999999-9999-4999-8999-999999999999", "")); code != 201 || sess.Carried != nil || d.last().PermissionMode != "" {
		t.Errorf("a resume of an unknown conversation: %d %s", code, raw)
	}
}

// The ordinary mode spelled out is not a widening and is not an unknown mode.
func TestExplicitDefaultNeedsNoSend(t *testing.T) {
	if got := createNeedsSend(createSessionBody{PermissionMode: "default"}); got != "" {
		t.Errorf("permissionMode default needs %q; it asks for nothing beyond a create", got)
	}
	if got := createNeedsSend(createSessionBody{PermissionMode: "bypass"}); got != "permissionMode" {
		t.Errorf("permissionMode bypass needs %q, want permissionMode", got)
	}
}

// A restart keeps the record: it is the whole reason the daemon, not each
// client, carries this. The old process is gone by the time anyone asks.
func TestLaunchRecordSurvivesRestartAndTheSession(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewWithState("testbox", st)
	if err != nil {
		t.Fatal(err)
	}
	svc.history.created("stub", fleet.Session{SessionRef: fleet.SessionRef{ID: "s1"}}, &launchFacts{
		Mode: fleet.PermissionModeBypass, Settings: json.RawMessage(`{"a":1}`), SendAuth: true, Conversation: convBypass})
	// Closing the session ends the seen record, never the launch record.
	svc.history.closedByRequest("stub", "s1")

	st2, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc2, err := NewWithState("testbox", st2)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := svc2.history.launchFor("stub", convBypass)
	if !ok || rec.Mode != fleet.PermissionModeBypass || !rec.SendAuth || string(rec.Settings) != `{"a":1}` {
		t.Fatalf("after restart launchFor = %+v %v", rec, ok)
	}

	// Retention applies to it like everything else in the store.
	svc2.history.setRetention(time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, ok := svc2.history.launchFor("stub", convBypass); ok {
		t.Error("a launch record outlived the retention period")
	}
}
