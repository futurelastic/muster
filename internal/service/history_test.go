package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/state"
)

// Tests for muster #179: a bounded record of sessions that ended.
// They reuse labels_test.go's labelDriver, which already models everything
// needed here: create, list, close, rename, a recycled id, and a session
// vanishing out from under the service.

func listClosed(t *testing.T, srv *httptest.Server, query string) (fleet.Collection[fleet.ClosedSession], []byte) {
	t.Helper()
	code, raw := labelCall(t, http.MethodGet, srv.URL+"/v1/sessions/closed?scope=local"+query, testToken, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/sessions/closed: %d %s", code, raw)
	}
	var col fleet.Collection[fleet.ClosedSession]
	if err := json.Unmarshal(raw, &col); err != nil {
		t.Fatalf("decoding closed list %s: %v", raw, err)
	}
	return col, raw
}

func closeSession(t *testing.T, srv *httptest.Server, id string) {
	t.Helper()
	code, raw := labelCall(t, http.MethodDelete, srv.URL+"/v1/machines/test-machine/sessions/"+id, testToken, nil, nil)
	if code != http.StatusAccepted {
		t.Fatalf("close %s: %d %s", id, code, raw)
	}
}

// The issue's oracle: create, close, restart — the closed read still returns
// it with its start and end, and the live list is unchanged.
func TestClosed_SurvivesRestartWithStartAndEnd(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewWithState("test-machine", st)
	if err != nil {
		t.Fatal(err)
	}
	d := newLabelDriver()
	srv := labelServer(t, svc, d, tokenCfg())

	code, raw := create(t, srv, testToken, nil)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	sess := decodeSession(t, raw)
	before := time.Now()
	closeSession(t, srv, sess.ID)
	after := time.Now()

	// Restart: a fresh service over the same state directory, same driver.
	st2, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc2, err := NewWithState("test-machine", st2)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := labelServer(t, svc2, d, tokenCfg())

	col, body := listClosed(t, srv2, "")
	if len(col.Items()) != 1 || !col.Complete() {
		t.Fatalf("want one closed session, complete; got %s", body)
	}
	got := col.Items()[0]
	if got.ID != sess.ID || got.Machine != "test-machine" || got.Runtime != "fake" || got.Cwd != "/work" {
		t.Errorf("identity wrong: %+v", got)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(*sess.StartedAt) {
		t.Errorf("startedAt = %v, want %v", got.StartedAt, sess.StartedAt)
	}
	if got.ClosedAt.Before(before) || got.ClosedAt.After(after) {
		t.Errorf("closedAt %v outside the close call [%v, %v]", got.ClosedAt, before, after)
	}
	if got.ClosedBy != fleet.ClosedByClose {
		t.Errorf("closedBy = %q, want close", got.ClosedBy)
	}

	// The live list is unchanged: the closed session is not in it.
	_, live, _ := listLocal(t, srv2, "")
	if len(live) != 0 {
		t.Errorf("live list should be empty, got %+v", live)
	}
}

// muster #235: a driver that captured a session's own exit before
// removing it is tombstoned with that exit's status and screen path, exact
// like a close — never the generic "absent" a driver with no such visibility
// would be left to report — and the screen's own content never reaches the
// wire, only its path.
func TestClosed_ExitIsCapturedByTheDriver(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)
	listLocal(t, srv, "")

	screenPath := filepath.Join(t.TempDir(), "screen.txt")
	if err := os.WriteFile(screenPath, []byte("boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	d.exitVia(sess.ID, 7, screenPath)
	after := time.Now()

	listLocal(t, srv, "")
	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 {
		t.Fatalf("want one closed session, got %s", body)
	}
	got := col.Items()[0]
	if got.ClosedBy != fleet.ClosedByExit {
		t.Errorf("closedBy = %q, want exit", got.ClosedBy)
	}
	if got.ClosedAt.Before(before) || got.ClosedAt.After(after) {
		t.Errorf("closedAt %v outside the capture window [%v, %v]", got.ClosedAt, before, after)
	}
	if got.Exit == nil {
		t.Fatal("no exit captured on the record")
	}
	if got.Exit.Status != 7 {
		t.Errorf("exit status = %d, want 7", got.Exit.Status)
	}
	if got.Exit.ScreenPath != screenPath {
		t.Errorf("exit screenPath = %q, want %q", got.Exit.ScreenPath, screenPath)
	}
	// The pane's own text never appears on the wire — only its path does.
	if strings.Contains(string(body), "boom") {
		t.Fatalf("the pane's screen text leaked onto the API: %s", body)
	}

	// The live list is unchanged: the closed session is not in it.
	_, live, _ := listLocal(t, srv, "")
	if len(live) != 0 {
		t.Errorf("live list should be empty, got %+v", live)
	}
}

// A session that ends on its own is recorded by the next complete listing,
// with its closedAt reported as an upper bound next to lastSeenAt.
func TestClosed_VanishedIsRecordedByACompleteListing(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)
	listLocal(t, srv, "")

	d.vanish(sess.ID)

	// A filtered listing proves nothing about absence (§5.7).
	listLocal(t, srv, "&cwdPrefix=/work")
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 0 {
		t.Fatalf("a filtered listing ended a session: %s", body)
	}

	listLocal(t, srv, "")
	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 {
		t.Fatalf("want one closed session, got %s", body)
	}
	got := col.Items()[0]
	if got.ClosedBy != fleet.ClosedByAbsent {
		t.Errorf("closedBy = %q, want absent", got.ClosedBy)
	}
	if got.ClosedAt.Before(got.LastSeenAt) {
		t.Errorf("closedAt %v before lastSeenAt %v", got.ClosedAt, got.LastSeenAt)
	}
	if got.Evidence == "" {
		t.Error("an inferred end must say how it was learned")
	}

	// Recorded once, not on every later listing.
	listLocal(t, srv, "")
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 1 {
		t.Fatalf("re-listing duplicated the record: %s", body)
	}
}

// A rename — through the service or behind its back — is the same run under
// a new id, not an end.
func TestClosed_RenameIsNotAnEnd(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	a := decodeSession(t, raw)
	_, raw = create(t, srv, testToken, nil)
	b := decodeSession(t, raw)
	listLocal(t, srv, "")

	code, body := labelCall(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions/"+a.ID+"/rename",
		testToken, map[string]any{"name": "renamed-a"}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("rename: %d %s", code, body)
	}
	// Behind the service's back: the carry rule matches it by startedAt.
	if _, err := d.Rename(t.Context(), fleet.Request{}, fleet.SessionRef{ID: b.ID}, "renamed-b"); err != nil {
		t.Fatal(err)
	}
	listLocal(t, srv, "")
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 0 {
		t.Fatalf("a rename was recorded as an end: %s", body)
	}

	// And the carried record is the one that ends later, under its new id.
	closeSession(t, srv, "renamed-b")
	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 || col.Items()[0].ID != "renamed-b" ||
		col.Items()[0].StartedAt == nil || !col.Items()[0].StartedAt.Equal(*b.StartedAt) {
		t.Fatalf("want renamed-b with b's startedAt, got %s", body)
	}
}

// §5.4: an id now held by a session that started later means the old one
// ended.
func TestClosed_RecycledIdEndsTheOldOccupant(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)
	listLocal(t, srv, "")

	d.recycle(sess.ID)
	listLocal(t, srv, "")

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 {
		t.Fatalf("want the old occupant recorded, got %s", body)
	}
	if got := col.Items()[0]; got.StartedAt == nil || !got.StartedAt.Equal(*sess.StartedAt) {
		t.Errorf("recorded the wrong run: %+v", got)
	}
	// The new occupant is live, not closed.
	if _, live, _ := listLocal(t, srv, ""); len(live) != 1 {
		t.Errorf("live list: %+v", live)
	}
}

// Past the retention window a record is gone — from reads immediately, and
// from the persisted document on the next write.
func TestClosed_RetentionAndSince(t *testing.T) {
	svc := New("test-machine")
	d := newLabelDriver()
	srv := labelServer(t, svc, d, tokenCfg())
	svc.SetClosedRetention(48 * time.Hour)

	clock := time.Now()
	svc.history.now = func() time.Time { return clock }

	_, raw := create(t, srv, testToken, nil)
	old := decodeSession(t, raw)
	closeSession(t, srv, old.ID)

	clock = clock.Add(24 * time.Hour)
	mid := clock
	_, raw = create(t, srv, testToken, nil)
	recent := decodeSession(t, raw)
	closeSession(t, srv, recent.ID)

	if col, body := listClosed(t, srv, ""); len(col.Items()) != 2 || col.Items()[0].ID != recent.ID {
		t.Fatalf("want both, newest first: %s", body)
	}
	since := mid.Add(-time.Minute).UTC().Format(time.RFC3339)
	if col, body := listClosed(t, srv, "&since="+since); len(col.Items()) != 1 || col.Items()[0].ID != recent.ID {
		t.Fatalf("since should keep only the recent one: %s", body)
	}

	clock = clock.Add(25*time.Hour + time.Minute) // old is now 49h past, recent 25h
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 1 || col.Items()[0].ID != recent.ID {
		t.Fatalf("the old record should have aged out: %s", body)
	}

	code, raw := labelCall(t, http.MethodGet, srv.URL+"/v1/sessions/closed?since=yesterday", testToken, nil, nil)
	if code != http.StatusBadRequest {
		t.Errorf("a malformed since should be refused, got %d %s", code, raw)
	}
}

// A close the driver accepted but that did not take is withdrawn when the
// session is plainly still running.
func TestClosed_PrematureCloseIsWithdrawn(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)

	d.mu.Lock()
	keep := d.sessions[sess.ID]
	d.mu.Unlock()
	closeSession(t, srv, sess.ID)
	d.mu.Lock()
	d.sessions[sess.ID] = keep // the close "did not take"
	d.mu.Unlock()

	listLocal(t, srv, "")
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 0 {
		t.Fatalf("a still-running session kept its tombstone: %s", body)
	}
}

// muster #236: an exit's screen file follows the same retention as the
// tombstone that names it. Aging the tombstone out on a later write (list()
// itself only filters a read — the comment on pruneLocked/list — so this
// forces an actual prune by closing a second session once the clock has
// moved past the window) must remove the file too, not just hide the record.
func TestClosed_ExitScreenFileAgesOutWithItsTombstone(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewWithState("test-machine", st)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetClosedRetention(48 * time.Hour)
	clock := time.Now()
	svc.history.now = func() time.Time { return clock }

	d := newLabelDriver()
	srv := labelServer(t, svc, d, tokenCfg())

	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)

	screenDir := filepath.Join(st.Dir(), "exit-screens")
	if err := os.MkdirAll(screenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	screenPath := filepath.Join(screenDir, "old.txt")
	if err := os.WriteFile(screenPath, []byte("boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.exitVia(sess.ID, 7, screenPath)
	listLocal(t, srv, "") // folds the exit in, writes the tombstone

	if _, err := os.Stat(screenPath); err != nil {
		t.Fatalf("screen file should exist right after the exit: %v", err)
	}

	clock = clock.Add(49 * time.Hour) // past the 48h retention
	_, raw = create(t, srv, testToken, nil)
	other := decodeSession(t, raw)
	closeSession(t, srv, other.ID) // a write, so pruneLocked actually runs

	if matches := listMustNotContain(t, srv, sess.ID); len(matches) != 0 {
		t.Fatalf("the aged-out record is still returned: %+v", matches)
	}
	if _, err := os.Stat(screenPath); !os.IsNotExist(err) {
		t.Fatalf("screen file should have been removed with its aged-out tombstone, stat err = %v", err)
	}
}

// listMustNotContain is TestClosed_ExitScreenFileAgesOutWithItsTombstone's
// own small helper: it returns every closed item matching id, so the caller
// can fail with the match in hand rather than a bare boolean.
func listMustNotContain(t *testing.T, srv *httptest.Server, id string) []fleet.ClosedSession {
	t.Helper()
	col, _ := listClosed(t, srv, "")
	var matches []fleet.ClosedSession
	for _, c := range col.Items() {
		if c.ID == id {
			matches = append(matches, c)
		}
	}
	return matches
}

// muster #236: a file left in exit-screens/ with no closed-session
// record naming it — a save that happened just before a crash prevented the
// tombstone from ever being written, or a record pruned by an older build
// that predates the file-aware prune — is removed at the next start-up.
// A file a live record still names must survive the same sweep.
func TestClosed_LoadSweepsOrphanExitScreenFiles(t *testing.T) {
	dir := t.TempDir()
	screenDir := filepath.Join(dir, "exit-screens")
	if err := os.MkdirAll(screenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(screenDir, "orphan.txt")
	if err := os.WriteFile(orphanPath, []byte("orphan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(screenDir, "live.txt")
	if err := os.WriteFile(livePath, []byte("live\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	doc := historyDoc{Closed: []fleet.ClosedSession{{
		SessionRef: fleet.SessionRef{Machine: "test-machine", ID: "s1"},
		ClosedBy:   fleet.ClosedByExit,
		ClosedAt:   time.Now(),
		Exit:       &fleet.SessionExit{Status: 1, At: time.Now(), ScreenPath: livePath},
	}}}
	if err := st.Save(historyStateName, doc); err != nil {
		t.Fatal(err)
	}

	if _, err := NewWithState("test-machine", st); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan file should be swept at load, stat err = %v", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("a file a live record still names should survive the sweep: %v", err)
	}
}

// setAgent stands in for a driver that can read a live session's applied
// agent (muster #301): the next listing reports it.
func (d *labelDriver) setAgent(id string, a fleet.AgentId) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sessions[id]
	s.Agent = a
	d.sessions[id] = s
}

// muster #319: the closed record carries the agent hint the live session had —
// through every way a session ends — and omits it, on the wire too, when no
// sighting ever reported one.
func TestClosed_CarriesTheAgentTheLiveSessionHad(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	mk := func() string {
		_, raw := create(t, srv, testToken, nil)
		return decodeSession(t, raw).ID
	}
	closed, vanished, exited, unknown := mk(), mk(), mk(), mk()
	for _, id := range []string{closed, vanished, exited} {
		d.setAgent(id, "reviewer")
	}
	listLocal(t, srv, "") // the sighting that learns the agent

	closeSession(t, srv, closed)
	d.vanish(vanished)
	d.exitVia(exited, 0, "")
	d.vanish(unknown)
	listLocal(t, srv, "") // complete read: records the vanished and exited ones

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 4 {
		t.Fatalf("want four closed sessions, got %s", body)
	}
	byID := map[string]fleet.ClosedSession{}
	for _, c := range col.Items() {
		byID[c.ID] = c
	}
	for name, id := range map[string]string{"close": closed, "absent": vanished, "exit": exited} {
		if got := byID[id].Agent; got != "reviewer" {
			t.Errorf("%s: closed record agent = %q, want reviewer", name, got)
		}
	}
	if got := byID[unknown].Agent; got != "" {
		t.Errorf("never reported: agent = %q, want empty", got)
	}

	// On the wire: present for the first case, the key absent for the second.
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, it := range raw.Items {
		var id string
		_ = json.Unmarshal(it["id"], &id)
		_, has := it["agent"]
		if want := id != unknown; has != want {
			t.Errorf("session %s: agent key present = %v, want %v (%s)", id, has, want, body)
		}
	}
}

// Tests for muster #482: a bounded read of the closed list, `limit` newest
// first and `before` to page back through older records.

func pagedRec(machine fleet.MachineId, id string, at time.Time) fleet.ClosedSession {
	return fleet.ClosedSession{
		SessionRef: fleet.SessionRef{Machine: machine, ID: id},
		Runtime:    "fake",
		ClosedAt:   at,
		ClosedBy:   fleet.ClosedByClose,
	}
}

func recIDs(items []fleet.ClosedSession) string {
	ids := make([]string, len(items))
	for i, c := range items {
		ids[i] = c.ID
	}
	return strings.Join(ids, ",")
}

// Records that closed at one instant have one fixed order whichever machine
// answered first: id, then machine. Without it a page boundary inside such a
// run would depend on which peer's reply was appended first.
func TestSortClosed_TiesHaveOneOrder(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	older := at.Add(-time.Second)
	in := []fleet.ClosedSession{
		pagedRec("b", "x", at), pagedRec("a", "y", at), pagedRec("a", "x", at), pagedRec("a", "old", older),
	}
	for _, perm := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}} {
		items := make([]fleet.ClosedSession, len(in))
		for i, p := range perm {
			items[i] = in[p]
		}
		sortClosed(items)
		var got []string
		for _, c := range items {
			got = append(got, string(c.Machine)+"/"+c.ID)
		}
		if want := "a/x b/x a/y a/old"; strings.Join(got, " ") != want {
			t.Errorf("order %v = %q, want %q", perm, strings.Join(got, " "), want)
		}
	}
}

func TestPageClosed(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	// Newest first, as every caller hands it over: three records share 30 s.
	all := []fleet.ClosedSession{
		pagedRec("m", "r1", at(50)), pagedRec("m", "r2", at(40)),
		pagedRec("m", "r3", at(30)), pagedRec("m", "r4", at(30)), pagedRec("m", "r5", at(30)),
		pagedRec("m", "r6", at(20)), pagedRec("m", "r7", at(10)),
	}
	cases := []struct {
		name   string
		before time.Time
		limit  int
		want   string
	}{
		{"no bound is everything", time.Time{}, 0, "r1,r2,r3,r4,r5,r6,r7"},
		{"limit takes the newest", time.Time{}, 2, "r1,r2"},
		{"limit inside a tied run keeps the whole run", time.Time{}, 3, "r1,r2,r3,r4,r5"},
		{"limit on the first of a run keeps the run", time.Time{}, 4, "r1,r2,r3,r4,r5"},
		{"limit past the end is everything", time.Time{}, 100, "r1,r2,r3,r4,r5,r6,r7"},
		{"before is exclusive", at(30), 0, "r6,r7"},
		{"before between records", at(35), 0, "r3,r4,r5,r6,r7"},
		{"before newer than everything", at(99), 0, "r1,r2,r3,r4,r5,r6,r7"},
		{"before older than everything", at(1), 0, ""},
		{"before and limit", at(45), 2, "r2,r3,r4,r5"},
	}
	for _, c := range cases {
		got := pageClosed(append([]fleet.ClosedSession(nil), all...), c.before, c.limit)
		if recIDs(got) != c.want {
			t.Errorf("%s: got %q, want %q", c.name, recIDs(got), c.want)
		}
	}
}

// seedClosed puts records straight into the store, the way a machine that has
// been closing sessions for days holds them.
func seedClosed(svc *Service, recs ...fleet.ClosedSession) {
	h := svc.history
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = append(h.closed, recs...)
	h.closedChangedLocked()
}

func TestClosed_LimitAndBeforePageBackThroughTheList(t *testing.T) {
	svc := New("test-machine")
	srv := labelServer(t, svc, newLabelDriver(), tokenCfg())
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	seedClosed(svc,
		pagedRec("test-machine", "r7", at(10)), pagedRec("test-machine", "r5", at(30)),
		pagedRec("test-machine", "r1", at(50)), pagedRec("test-machine", "r3", at(30)),
		pagedRec("test-machine", "r6", at(20)), pagedRec("test-machine", "r2", at(40)),
		pagedRec("test-machine", "r4", at(30)),
	)
	ids := func(query string) string {
		col, _ := listClosed(t, srv, query)
		return recIDs(col.Items())
	}
	cursor := func(s int) string { return "&before=" + url.QueryEscape(at(s).UTC().Format(time.RFC3339Nano)) }

	// A call naming neither parameter is what it always was.
	if got := ids(""); got != "r1,r2,r3,r4,r5,r6,r7" {
		t.Errorf("no parameters: %q", got)
	}
	if got := ids("&limit=2"); got != "r1,r2" {
		t.Errorf("limit=2: %q", got)
	}
	if got := ids("&limit=2" + cursor(40)); got != "r3,r4,r5" {
		t.Errorf("limit=2 before the second page: %q (a page never splits a run that closed together)", got)
	}
	if got := ids("&limit=2" + cursor(30)); got != "r6,r7" {
		t.Errorf("last page: %q", got)
	}
	if got := ids("&limit=2" + cursor(10)); got != "" {
		t.Errorf("past the oldest: %q", got)
	}
	if got := ids(cursor(30)); got != "r6,r7" {
		t.Errorf("before alone: %q", got)
	}

	// Walking the whole list one at a time returns every record exactly once.
	var walked []string
	next := ""
	for page := 0; page < 20; page++ {
		col, _ := listClosed(t, srv, "&limit=1"+next)
		items := col.Items()
		if len(items) == 0 {
			break
		}
		for _, c := range items {
			walked = append(walked, c.ID)
		}
		last := items[len(items)-1].ClosedAt
		next = "&before=" + url.QueryEscape(last.UTC().Format(time.RFC3339Nano))
	}
	if got := strings.Join(walked, ","); got != "r1,r2,r3,r4,r5,r6,r7" {
		t.Errorf("a walk by cursor read %q", got)
	}
}

// The cursor is the closedAt of the last row as the API printed it, nanoseconds
// included: one nanosecond more reaches the record, the value itself does not.
func TestClosed_BeforeKeepsNanosecondPrecision(t *testing.T) {
	svc := New("test-machine")
	srv := labelServer(t, svc, newLabelDriver(), tokenCfg())
	exact := time.Now().Add(-time.Hour).Truncate(time.Second).Add(123456789 * time.Nanosecond)
	seedClosed(svc, pagedRec("test-machine", "only", exact))
	q := func(t time.Time) string {
		return "&before=" + url.QueryEscape(t.UTC().Format(time.RFC3339Nano))
	}
	if col, _ := listClosed(t, srv, q(exact)); len(col.Items()) != 0 {
		t.Errorf("before = the record's own closedAt must exclude it, got %s", recIDs(col.Items()))
	}
	if col, _ := listClosed(t, srv, q(exact.Add(time.Nanosecond))); len(col.Items()) != 1 {
		t.Errorf("one nanosecond later must reach it, got %d", len(col.Items()))
	}
}

func TestClosed_LimitAndBeforeAreValidated(t *testing.T) {
	svc := New("test-machine")
	srv := labelServer(t, svc, newLabelDriver(), tokenCfg())
	for _, q := range []string{"limit=0", "limit=-3", "limit=abc", "limit=1.5", "before=yesterday", "before=2026-01-01"} {
		code, raw := labelCall(t, http.MethodGet, srv.URL+"/v1/sessions/closed?"+q, testToken, nil, nil)
		if code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %s", q, code, raw)
		}
	}
}

// closedPeer is a peer driver that holds closed records and answers a read with
// exactly the page it was asked for, recording what it was asked.
type closedPeer struct {
	stub.Driver
	rows []fleet.ClosedSession
	got  driver.ClosedQuery
}

func (p *closedPeer) ListClosed(_ context.Context, _ fleet.Request, q driver.ClosedQuery) (fleet.Collection[fleet.ClosedSession], error) {
	p.got = q
	return fleet.NewCollection(pageClosed(p.rows, q.Before, q.Limit),
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}})
}

// A fleet read merges this machine's records with each peer's page and cuts the
// result once: the newest across both, with a run that closed at one instant
// kept whole even when its members came from different machines.
func TestClosed_FleetReadMergesPagesAndCutsOnce(t *testing.T) {
	svc := New("test-machine")
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	seedClosed(svc,
		pagedRec("test-machine", "l50", at(50)), pagedRec("test-machine", "l30", at(30)), pagedRec("test-machine", "l10", at(10)))
	peer := &closedPeer{Driver: stub.Driver{DeadlineMs: 500}, rows: []fleet.ClosedSession{
		pagedRec("peerbox", "p40", at(40)), pagedRec("peerbox", "p30", at(30)), pagedRec("peerbox", "p20", at(20)),
	}}
	if err := svc.RegisterPeerDriver("peerbox", peer); err != nil {
		t.Fatal(err)
	}

	col, err := svc.ListClosedSessions(context.Background(), fleet.SystemRequest(), ScopeFleet, driver.ClosedQuery{Limit: 3}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := recIDs(col.Items()); got != "l50,p40,l30,p30" {
		t.Errorf("page = %q, want l50,p40,l30,p30 (the 30 s run stays whole)", got)
	}
	if peer.got.Limit != 3 {
		t.Errorf("the peer was asked %+v, want its own page of 3", peer.got)
	}

	col, err = svc.ListClosedSessions(context.Background(), fleet.SystemRequest(), ScopeFleet,
		driver.ClosedQuery{Before: at(30), Limit: 3}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := recIDs(col.Items()); got != "p20,l10" {
		t.Errorf("next page = %q, want p20,l10", got)
	}
	if !peer.got.Before.Equal(at(30)) {
		t.Errorf("the peer was not sent the cursor: %+v", peer.got)
	}

	// Local scope never reaches the peer, bounded or not.
	peer.got = driver.ClosedQuery{}
	col, _ = svc.ListClosedSessions(context.Background(), fleet.SystemRequest(), ScopeLocal, driver.ClosedQuery{Limit: 1}, 0)
	if got := recIDs(col.Items()); got != "l50" || peer.got != (driver.ClosedQuery{}) {
		t.Errorf("local read = %q, peer asked %+v", got, peer.got)
	}
}
