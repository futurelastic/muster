package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// whoami issues GET /v1/whoami and decodes the report. t.Fatal on transport
// or decode failure; the status code is returned for the caller to assert.
func whoami(t *testing.T, srv *httptest.Server, token, machine string) (int, fleet.GrantReport) {
	t.Helper()
	url := srv.URL + "/v1/whoami"
	if machine != "" {
		url += "?machine=" + machine
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fleet.GrantReport{}
	}
	var report fleet.GrantReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decoding GrantReport: %v", err)
	}
	return resp.StatusCode, report
}

func hasGrant(grants []string, g Grant) bool {
	for _, have := range grants {
		if have == string(g) {
			return true
		}
	}
	return false
}

// The whole point of #106: a principal holding NO grants at all must still
// be able to call this route and learn that it holds none. Every other read
// route refuses such a principal outright (TestReadRequiresItsOwnGrant) —
// this one is the deliberate exception.
func TestWhoAmIAnswersAPrincipalWithNoGrants(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "empty", Token: "tok-empty"},
	})
	code, report := whoami(t, srv, "tok-empty", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a no-grants principal must still be able to read its own report)", code)
	}
	if len(report.Grants) != 0 {
		t.Errorf("grants = %v, want empty", report.Grants)
	}
	if report.Principal != "empty" {
		t.Errorf("principal = %q, want %q", report.Principal, "empty")
	}
	if report.Machine != "testbox" {
		t.Errorf("machine = %q, want %q", report.Machine, "testbox")
	}
	if report.Source != fleet.CapabilitiesObserved {
		t.Errorf("source = %q, want %q (this machine just resolved its own table)", report.Source, fleet.CapabilitiesObserved)
	}
}

// A principal's own report names exactly its own grants — no more, no
// fewer — and never leaks a sibling principal's table.
func TestWhoAmIReportsExactlyTheCallersOwnGrants(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "watcher", Token: "tok-watch", Grants: []Grant{GrantRead}},
		{Name: "operator", Token: "tok-op", Grants: []Grant{GrantRead, GrantSend, GrantRelay}},
	})

	_, watcherReport := whoami(t, srv, "tok-watch", "")
	if len(watcherReport.Grants) != 1 || !hasGrant(watcherReport.Grants, GrantRead) {
		t.Errorf("watcher grants = %v, want [read]", watcherReport.Grants)
	}
	if watcherReport.Principal != "watcher" {
		t.Errorf("principal = %q, want watcher", watcherReport.Principal)
	}

	_, opReport := whoami(t, srv, "tok-op", "")
	if len(opReport.Grants) != 3 {
		t.Errorf("operator grants = %v, want 3 entries", opReport.Grants)
	}
	for _, g := range []Grant{GrantRead, GrantSend, GrantRelay} {
		if !hasGrant(opReport.Grants, g) {
			t.Errorf("operator grants = %v, missing %s", opReport.Grants, g)
		}
	}
	// Never the other principal's name or grants.
	if opReport.Principal == "watcher" {
		t.Errorf("operator's report named watcher")
	}
}

// An unrecognised credential is refused before this route ever runs — the
// exception carved out above is from reading()'s grant gate, never from
// withAuth's authentication gate (§5: no unauthenticated mode, no exception).
func TestWhoAmIStillRequiresAValidCredential(t *testing.T) {
	srv := principalSrv(t, []Principal{{Name: "a", Token: "good"}})
	code, _ := whoami(t, srv, "not-a-real-token", "")
	if code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Errorf("status = %d, want unauthorized", code)
	}
}

// A peer machine never gets a real answer — this service has no mechanism
// to learn what a peer has granted a given credential, so it reuses
// CapabilitiesAssumed's conservative-floor shape rather than guessing or
// inventing a second "I don't know" vocabulary.
func TestWhoAmIForAPeerIsAlwaysAssumed(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "operator", Token: "tok-op", Grants: []Grant{GrantRead, GrantSend, GrantRelay}},
	})
	code, report := whoami(t, srv, "tok-op", "otherbox")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if report.Source != fleet.CapabilitiesAssumed {
		t.Errorf("source = %q, want %q", report.Source, fleet.CapabilitiesAssumed)
	}
	if len(report.Grants) != 0 {
		t.Errorf("grants = %v, want empty (a peer's table is never observed)", report.Grants)
	}
	if report.Machine != "otherbox" {
		t.Errorf("machine = %q, want otherbox", report.Machine)
	}
	// Never fabricated from what the caller happens to hold locally.
	if hasGrant(report.Grants, GrantSend) {
		t.Errorf("reported a local grant (%v) as though it applied on the unreached peer", report.Grants)
	}
}

// Naming this instance's own id explicitly is the same as omitting `machine`.
func TestWhoAmISelfNamedExplicitlyIsTheSameAsOmitted(t *testing.T) {
	srv := principalSrv(t, []Principal{{Name: "op", Token: "tok", Grants: []Grant{GrantRead}}})
	_, implicit := whoami(t, srv, "tok", "")
	_, explicit := whoami(t, srv, "tok", "testbox")
	if implicit.Source != explicit.Source || len(implicit.Grants) != len(explicit.Grants) {
		t.Errorf("implicit self (%+v) and explicit self (%+v) disagree", implicit, explicit)
	}
	if explicit.Source != fleet.CapabilitiesObserved {
		t.Errorf("source = %q, want observed", explicit.Source)
	}
}

// Legacy single-token mode has no per-verb table, only the two coarse flags
// mutating() actually enforces — grantsForRequest must report exactly those,
// never a finer grain the instance is not actually configured with.
func TestWhoAmIUnderLegacyTokenMode(t *testing.T) {
	build := func(local, relay bool) *httptest.Server {
		svc := New("legacybox")
		if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
			t.Fatal(err)
		}
		mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: local, AllowPeerRelay: relay})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("neither flag", func(t *testing.T) {
		srv := build(false, false)
		_, report := whoami(t, srv, testToken, "")
		if len(report.Grants) != 1 || !hasGrant(report.Grants, GrantRead) {
			t.Errorf("grants = %v, want [read] only", report.Grants)
		}
	})

	t.Run("both flags", func(t *testing.T) {
		srv := build(true, true)
		_, report := whoami(t, srv, testToken, "")
		for _, g := range Grants() {
			// GrantHumanRelay is deliberately excluded from this "both flags
			// implies everything" check (#180 review fix): it is never a
			// coarser version of "can mutate" or "can relay" the way every
			// other grant here is under legacy mode — it names a caller's
			// own claim to BE a human-facing relay (route:"terminal" with no
			// `from` label), which a blanket local-mutations flag says
			// nothing about. It is granted only by explicit principal
			// configuration (Config.Principals), never implied by the two
			// legacy flags.
			// GrantActingPerson (#349) is excluded for the same reason: naming
			// the human behind a request is a client's claim, never implied by a
			// blanket flag.
			if g == GrantHumanRelay || g == GrantActingPerson {
				continue
			}
			if !hasGrant(report.Grants, g) {
				t.Errorf("grants = %v, missing %s (both legacy flags are on)", report.Grants, g)
			}
		}
		if hasGrant(report.Grants, GrantHumanRelay) || hasGrant(report.Grants, GrantActingPerson) {
			t.Errorf("grants = %v, legacy mode must never imply human-relay or acting-person", report.Grants)
		}
	})
}
