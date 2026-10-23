package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/drivers/stub"
)

func tableServer(t *testing.T, initial []Principal) (*PrincipalTable, *httptest.Server) {
	t.Helper()
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	table := NewPrincipalTable(initial)
	srv := httptest.NewServer(NewMux(svc, Config{Table: table}))
	t.Cleanup(srv.Close)
	return table, srv
}

func get(t *testing.T, srv *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// streamEnds reports whether the body reaches EOF within d.
func streamEnds(resp *http.Response, d time.Duration) bool {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// #347: a principal added to a running service works at once, and a stream that
// was already open is not disturbed.
func TestPrincipalTable_AddWithoutRestartLeavesStreamsOpen(t *testing.T) {
	read := []Grant{GrantRead}
	table, srv := tableServer(t, []Principal{{Name: "old", Token: "tok-old", Grants: read}})

	if resp := get(t, srv, "/v1/health", "tok-new"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unlisted token = %d, want 401", resp.StatusCode)
	}
	stream := get(t, srv, "/v1/events", "tok-old")
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d", stream.StatusCode)
	}

	closed := table.Replace([]Principal{
		{Name: "old", Token: "tok-old", Grants: read},
		{Name: "new", Token: "tok-new", Grants: read},
	})
	if len(closed) != 0 {
		t.Errorf("an addition closed streams: %v", closed)
	}
	if resp := get(t, srv, "/v1/health", "tok-new"); resp.StatusCode != http.StatusOK {
		t.Errorf("the new principal = %d, want 200 without a restart", resp.StatusCode)
	}
	if streamEnds(stream, 300*time.Millisecond) {
		t.Error("the existing stream was closed by an unrelated addition")
	}
}

// #347: a revoked principal is refused from its next request on, its open
// stream and its parked long poll are closed, and another principal's stream
// carries on.
func TestPrincipalTable_RevokeClosesOnlyThatPrincipalsStreams(t *testing.T) {
	read := []Grant{GrantRead}
	table, srv := tableServer(t, []Principal{
		{Name: "gone", Token: "tok-gone", Grants: read},
		{Name: "stays", Token: "tok-stays", Grants: read},
	})
	goneStream := get(t, srv, "/v1/events", "tok-gone")
	defer goneStream.Body.Close()
	staysStream := get(t, srv, "/v1/events", "tok-stays")
	defer staysStream.Body.Close()

	poll := make(chan int, 1)
	go func() {
		resp := get(t, srv, "/v1/sessions/watch?wait=20s", "tok-gone")
		defer resp.Body.Close()
		poll <- resp.StatusCode
	}()
	time.Sleep(150 * time.Millisecond) // let the poll park

	closed := table.Replace([]Principal{{Name: "stays", Token: "tok-stays", Grants: read}})
	if closed["gone"] != 2 {
		t.Errorf("closed = %v, want two of gone's (stream and poll)", closed)
	}

	if resp := get(t, srv, "/v1/health", "tok-gone"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked principal's next request = %d, want 401", resp.StatusCode)
	}
	if !streamEnds(goneStream, 2*time.Second) {
		t.Error("the revoked principal's stream is still open")
	}
	select {
	case code := <-poll:
		if code != http.StatusUnauthorized {
			t.Errorf("the revoked principal's parked poll = %d, want 401", code)
		}
	case <-time.After(2 * time.Second):
		t.Error("the revoked principal's parked poll is still waiting")
	}
	if streamEnds(staysStream, 300*time.Millisecond) {
		t.Error("another principal's stream was closed")
	}
}

// A changed credential ends the streams of the old one; a changed grant that
// keeps read does not.
func TestPrincipalTable_ChangedCredentialClosesGrantChangeDoesNot(t *testing.T) {
	table, srv := tableServer(t, []Principal{
		{Name: "rotated", Token: "tok-1", Grants: []Grant{GrantRead}},
		{Name: "regranted", Token: "tok-r", Grants: []Grant{GrantRead}},
	})
	rotated := get(t, srv, "/v1/events", "tok-1")
	defer rotated.Body.Close()
	regranted := get(t, srv, "/v1/events", "tok-r")
	defer regranted.Body.Close()

	table.Replace([]Principal{
		{Name: "rotated", Token: "tok-2", Grants: []Grant{GrantRead}},
		{Name: "regranted", Token: "tok-r", Grants: []Grant{GrantRead, GrantCreate}},
	})
	if !streamEnds(rotated, 2*time.Second) {
		t.Error("a stream authenticated by a replaced token is still open")
	}
	if streamEnds(regranted, 300*time.Millisecond) {
		t.Error("a grant change that keeps read closed the stream")
	}
	if resp := get(t, srv, "/v1/health", "tok-1"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the replaced token = %d, want 401", resp.StatusCode)
	}
}

// A request that authenticated just before a swap and binds just after it must
// not slip through the gap.
func TestPrincipalTable_BindAfterRevokeIsClosed(t *testing.T) {
	p := Principal{Name: "x", Token: "t", Grants: []Grant{GrantRead}}
	table := NewPrincipalTable([]Principal{p})
	table.Replace(nil)
	ctx, release := table.bind(t.Context(), p)
	defer release()
	select {
	case <-ctx.Done():
	default:
		t.Error("a stream bound after its principal was revoked was left open")
	}
}
