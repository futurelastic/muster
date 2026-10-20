package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
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
