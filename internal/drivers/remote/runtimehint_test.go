package remote

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// muster #461: ?runtime= on a session-addressed call to a peer is forwarded.
//
// The peer resolves a session id among its OWN runtimes (§13.1). Before this,
// nothing a caller said about the runtime left this machine, so an id present
// under two of the peer's runtimes was answered "pass ?runtime= to
// disambiguate" to a caller who had no way to.

// sessionOps is every session-addressed operation of the driver, each as a call
// that reaches the peer. A new one belongs here AND in withRuntimeHint's
// callers; the source scan below fails the second half if it is forgotten.
var sessionOps = []struct {
	name string
	call func(d *Driver, req fleet.Request) error
}{
	{"State", func(d *Driver, req fleet.Request) error {
		_, err := d.State(context.Background(), req, fleet.SessionRef{ID: "s1"})
		return err
	}},
	{"Send", func(d *Driver, req fleet.Request) error {
		_, err := d.Send(context.Background(), req, fleet.SessionRef{ID: "s1"}, "hi", driver.SendOptions{})
		return err
	}},
	{"Interrupt", func(d *Driver, req fleet.Request) error {
		_, err := d.Interrupt(context.Background(), req, fleet.SessionRef{ID: "s1"})
		return err
	}},
	{"Close", func(d *Driver, req fleet.Request) error {
		_, err := d.Close(context.Background(), req, fleet.SessionRef{ID: "s1"})
		return err
	}},
	{"Discard", func(d *Driver, req fleet.Request) error {
		_, err := d.Discard(context.Background(), req, fleet.SessionRef{ID: "s1"}, "dg", driver.DiscardOptions{Force: true})
		return err
	}},
	{"Keys", func(d *Driver, req fleet.Request) error {
		_, err := d.Keys(context.Background(), req, fleet.SessionRef{ID: "s1"}, fleet.KeyUp, "dg")
		return err
	}},
	{"Rename", func(d *Driver, req fleet.Request) error {
		_, err := d.Rename(context.Background(), req, fleet.SessionRef{ID: "s1"}, "next")
		return err
	}},
	{"Labels", func(d *Driver, req fleet.Request) error {
		v := "x"
		_, err := d.Labels(context.Background(), req, fleet.SessionRef{ID: "s1"}, map[string]*string{"k": &v})
		return err
	}},
	{"SetRemoteControl", func(d *Driver, req fleet.Request) error {
		_, err := d.SetRemoteControl(context.Background(), req, fleet.SessionRef{ID: "s1"}, true)
		return err
	}},
	{"Composer", func(d *Driver, req fleet.Request) error {
		_, err := d.Composer(context.Background(), req, fleet.SessionRef{ID: "s1"})
		return err
	}},
	{"Respond", func(d *Driver, req fleet.Request) error {
		_, err := d.Respond(context.Background(), req, fleet.SessionRef{ID: "s1"}, fleet.Response{Choice: 1})
		return err
	}},
	{"Turns", func(d *Driver, req fleet.Request) error {
		_, err := d.Turns(context.Background(), req, fleet.SessionRef{ID: "s1"}, fleet.TurnsQuery{Since: "c1", Limit: 5})
		return err
	}},
	{"Record", func(d *Driver, req fleet.Request) error {
		_, err := d.Record(context.Background(), req, fleet.SessionRef{ID: "s1"}, fleet.RecordQuery{Since: "c1", Limit: 5})
		return err
	}},
	{"RecordMedia", func(d *Driver, req fleet.Request) error {
		_, err := d.RecordMedia(context.Background(), req, fleet.SessionRef{ID: "s1"}, "m1")
		return err
	}},
}

// Each operation, with a hint: the peer sees runtime=X, nothing else about the
// request moves, and whatever query the operation already had keeps its place
// in front of it. Without a hint: the request is what it was — no runtime key,
// and the same path and query as the same call made with the hint stripped.
func TestEverySessionAddressedCallForwardsTheRuntimeHint(t *testing.T) {
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	var withBase, withoutBase int
	for _, op := range sessionOps {
		t.Run(op.name, func(t *testing.T) {
			var rec capture
			srv := peerServing(t, 200, map[string]any{}, &rec)
			d := New("peerbox", srv.URL)

			plain := caller
			plain.Expect.StartedAt = &started
			_ = op.call(d, plain)
			if rec.path == "" {
				t.Fatalf("%s never reached the peer", op.name)
			}
			basePath, baseQuery := rec.path, rec.query
			if keys, _ := url.ParseQuery(baseQuery); keys.Has("runtime") {
				t.Fatalf("a call that named no runtime sent one: %q", baseQuery)
			}

			hinted := plain
			hinted.RuntimeHint = "beta"
			rec = capture{}
			_ = op.call(d, hinted)
			if rec.path != basePath {
				t.Errorf("the hint moved the path: %q → %q", basePath, rec.path)
			}
			want := baseQuery
			if want != "" {
				want += "&"
				withBase++
			} else {
				withoutBase++
			}
			want += "runtime=beta"
			if rec.query != want {
				t.Errorf("query = %q, want %q (the operation's own query, then the hint)", rec.query, want)
			}
		})
	}
	// The table has to exercise both shapes the hint is appended to, or the
	// "&" and "?" branches are not both covered by it.
	if withBase == 0 || withoutBase == 0 {
		t.Errorf("the table covers only one query shape: %d with a query already, %d without", withBase, withoutBase)
	}
}

// "A call without the parameter is byte-for-byte unchanged": the exact queries
// of three operations that used to have different ones.
func TestACallWithoutAHintIsByteForByteUnchanged(t *testing.T) {
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	req := caller
	req.Expect.StartedAt = &started

	cases := []struct {
		name  string
		call  func(d *Driver)
		query string
	}{
		{"State", func(d *Driver) { _, _ = d.State(context.Background(), req, fleet.SessionRef{ID: "s1"}) }, ""},
		{"Composer", func(d *Driver) { _, _ = d.Composer(context.Background(), req, fleet.SessionRef{ID: "s1"}) }, "startedAt=2026-03-04T05%3A06%3A07Z"},
		{"Turns", func(d *Driver) {
			_, _ = d.Turns(context.Background(), req, fleet.SessionRef{ID: "s1"}, fleet.TurnsQuery{Since: "c1", Limit: 5})
		}, "limit=5&since=c1&startedAt=2026-03-04T05%3A06%3A07Z"},
	}
	for _, c := range cases {
		var rec capture
		srv := peerServing(t, 200, map[string]any{}, &rec)
		c.call(New("peerbox", srv.URL))
		if rec.query != c.query {
			t.Errorf("%s: query = %q, want %q", c.name, rec.query, c.query)
		}
	}
}

// The value is the caller's text and goes into a query: it is encoded, not
// spliced, so it cannot add a parameter of its own.
func TestRuntimeHintIsEncodedNotSpliced(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, map[string]any{}, &rec)
	req := caller
	req.RuntimeHint = "a&force=true b"
	_, _ = New("peerbox", srv.URL).State(context.Background(), req, fleet.SessionRef{ID: "s1"})

	got, err := url.ParseQuery(rec.query)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got.Get("runtime") != "a&force=true b" {
		t.Errorf("query %q parsed to %v, want the one runtime key holding the text as given", rec.query, got)
	}
}

// Create carries its runtime in the body and a listing addresses no session, so
// neither takes the hint. A hint on the Request they are handed must not leak
// into their URL.
func TestCreateAndListDoNotCarryTheHint(t *testing.T) {
	req := caller
	req.RuntimeHint = "beta"

	var rec capture
	srv := peerServing(t, 200, map[string]any{"id": "n1"}, &rec)
	_, _ = New("peerbox", srv.URL).Create(context.Background(), req, "key-1", fleet.SessionSpec{Cwd: "/w", Runtime: "beta"})
	if rec.method == "" {
		t.Fatal("Create never reached the peer")
	}
	if strings.Contains(rec.path+"?"+rec.query, "runtime=") {
		t.Errorf("create carried the hint in its URL: %s?%s", rec.path, rec.query)
	}

	rec = capture{}
	srv2 := peerServing(t, 200, collectionJSON([]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}}, nil), &rec)
	_, _ = New("peerbox", srv2.URL).List(context.Background(), req, driver.ListFilter{})
	if rec.method == "" {
		t.Fatal("List never reached the peer")
	}
	if strings.Contains(rec.query, "runtime=") {
		t.Errorf("list carried the hint: %q", rec.query)
	}
}

// The structural half of the table above, for the day someone adds a fifteenth
// builder: a function in this package that builds a session-addressed path
// (".../sessions/%s...") and never calls withRuntimeHint silently drops the
// caller's runtime again, one route at a time. The table cannot see a builder
// it does not list; this can.
func TestNoSessionPathIsBuiltWithoutTheHint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Split on top-level func declarations; a body runs to the next one.
		for _, fn := range strings.Split(string(raw), "\nfunc ")[1:] {
			if !strings.Contains(fn, `/sessions/%s`) {
				continue
			}
			seen++
			name, _, _ := strings.Cut(fn, "{")
			if !strings.Contains(fn, "withRuntimeHint(") {
				t.Errorf("%s: %s builds a session-addressed path and never calls withRuntimeHint (#461)", f, strings.TrimSpace(name))
			}
		}
	}
	if seen != len(sessionOps) {
		t.Errorf("found %d session-path builders in the source, sessionOps lists %d: they must agree", seen, len(sessionOps))
	}
}
