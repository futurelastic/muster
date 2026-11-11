package service

import (
	"net/http/httptest"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #461: the caller's ?runtime= travels in the Request, because a
// session-addressed call to a peer reaches the remote driver with nothing else.

func TestRequestFromCarriesTheRuntimeHint(t *testing.T) {
	for _, c := range []struct {
		target string
		want   fleet.RuntimeId
	}{
		{"/v1/machines/m/sessions/s1", ""},
		{"/v1/machines/m/sessions/s1?runtime=beta", "beta"},
		{"/v1/machines/m/sessions/s1/turns?since=c1&runtime=alpha&limit=2", "alpha"},
		{"/v1/machines/m/sessions/s1?runtime=", ""},
	} {
		r := httptest.NewRequest("GET", c.target, nil)
		if got := requestFrom(r).RuntimeHint; got != c.want {
			t.Errorf("%s: RuntimeHint = %q, want %q", c.target, got, c.want)
		}
	}
}

func TestListedSession(t *testing.T) {
	row := func(id string, rt fleet.RuntimeId, cwd string) fleet.Session {
		return fleet.Session{SessionRef: fleet.SessionRef{ID: id}, Runtime: rt, Cwd: fleet.AbsolutePath(cwd)}
	}
	both := []fleet.Session{row("s0", "alpha", "/other"), row("s1", "alpha", "/a"), row("s1", "beta", "/b")}

	for _, c := range []struct {
		name  string
		items []fleet.Session
		id    string
		hint  fleet.RuntimeId
		want  string // cwd of the row picked, "" when none
	}{
		// No hint is what it always was: the first row with the id.
		{"no hint takes the first row with the id", both, "s1", "", "/a"},
		{"no hint, id absent", both, "nope", "", ""},
		// A hint picks the row under that runtime, whatever its position.
		{"hint picks the second row", both, "s1", "beta", "/b"},
		{"hint picks the first row", both, "s1", "alpha", "/a"},
		// A runtime that does not hold the id matches nothing: the caller goes
		// on to the peer's own answer rather than being handed another runtime's.
		{"hint naming a runtime without the id", both, "s1", "gamma", ""},
		{"hint, id absent", both, "nope", "beta", ""},
		// A row that does not say its runtime stands only when it is alone.
		{"a lone unlabelled row stands for any hint", []fleet.Session{row("s1", "", "/old")}, "s1", "beta", "/old"},
		{"an unlabelled row among others does not", []fleet.Session{row("s1", "", "/old"), row("s1", "alpha", "/a")}, "s1", "beta", ""},
		{"a lone row of another runtime does not", []fleet.Session{row("s1", "alpha", "/a")}, "s1", "beta", ""},
	} {
		got, ok := listedSession(c.items, c.id, c.hint)
		switch {
		case c.want == "" && ok:
			t.Errorf("%s: picked %+v, want none", c.name, got)
		case c.want != "" && (!ok || string(got.Cwd) != c.want):
			t.Errorf("%s: picked %+v (ok=%v), want cwd %q", c.name, got, ok, c.want)
		}
	}
}
