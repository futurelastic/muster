package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #496: the list GET /v1/runtimes gives for the runtime survives a restart, and
// a restart reads it again without waiting for somebody to create a session.
//
// These tests use real per-session processes (the test binary standing in for the
// runtime, fakeopencode_test.go). A stand-in serves the catalog the marker file
// `.fake-providers.json` in its working directory holds, so one test can make a
// session's server say one thing and a server the service starts for itself say
// another, and change it between starts.

// catalogTiming keeps the start-up read's settle short: two answers that agree end it.
var catalogTiming = withCatalogTiming(300*time.Millisecond, 10*time.Millisecond)

// plantCatalog is what a stand-in started in dir will say its catalog is.
func plantCatalog(t *testing.T, dir string, providers map[string][]string, defaults map[string]string) {
	t.Helper()
	type provider struct {
		ID     string              `json:"id"`
		Models map[string]struct{} `json:"models"`
	}
	out := struct {
		Providers []provider        `json:"providers"`
		Default   map[string]string `json:"default"`
	}{Providers: []provider{}, Default: defaults}
	names := make([]string, 0, len(providers))
	for p := range providers {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		models := map[string]struct{}{}
		for _, m := range providers[p] {
			models[m] = struct{}{}
		}
		out.Providers = append(out.Providers, provider{ID: p, Models: models})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fake-providers.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func unplantCatalog(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, ".fake-providers.json")); err != nil {
		t.Fatal(err)
	}
}

// catalogDriver is the service starting over state with the start-up read ON — the
// production default, which the other tests turn off.
func catalogDriver(t *testing.T, state string, extra ...Option) *Driver {
	t.Helper()
	d := newStateDriver(t, state, append([]Option{withCatalogRefresh(), catalogTiming}, extra...)...)
	t.Cleanup(func() { _ = d.Shutdown() })
	return d
}

// storedDriver is the service starting over state with the start-up read OFF, so what
// Models says is only what the file said.
func storedDriver(t *testing.T, state string, extra ...Option) *Driver {
	t.Helper()
	d := newStateDriver(t, state, append([]Option{catalogTiming}, extra...)...)
	t.Cleanup(func() { _ = d.Shutdown() })
	return d
}

// refreshed waits for the start-up read to be over.
func refreshed(t *testing.T, d *Driver) {
	t.Helper()
	if d.refreshDone == nil {
		t.Fatal("the start-up read was not started")
	}
	select {
	case <-d.refreshDone:
	case <-time.After(wait):
		t.Fatal("timed out waiting for the start-up read to end")
	}
}

func modelsIDs(m fleet.RuntimeModels) []string {
	out := append([]string{}, m.IDs...)
	sort.Strings(out)
	return out
}

func wantIDs(t *testing.T, what string, m fleet.RuntimeModels, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := modelsIDs(m); !reflect.DeepEqual(got, want) {
		t.Errorf("%s: ids = %v, want %v (%+v)", what, got, want, m)
	}
}

// scopedDirs fails unless exactly want server directories are under root: the ones
// sessions own, and none left by a server started only to be asked.
func scopedDirs(t *testing.T, root string, want int) {
	t.Helper()
	entries, _ := os.ReadDir(root)
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "muster-opencode-") {
			n++
		}
	}
	if n != want {
		t.Errorf("%d server directories under the session root, want %d: a server started for the read was not cleaned up", n, want)
	}
}

func TestCatalogStore_ARestartWithNoSessionsListsTheModelsWithNoCreate(t *testing.T) {
	state := t.TempDir()
	_, root := stateLayout(state)
	plantCatalog(t, root, map[string][]string{"openai": {"gpt-a", "gpt-b"}}, map[string]string{"openai": "gpt-a"})

	d := catalogDriver(t, state)
	waitFor(t, "the start-up read to list the models", func() bool { return d.Models().Source == fleet.ModelsFromRuntime })
	refreshed(t, d)

	got := d.Models()
	wantIDs(t, "after the start-up read", got, "openai/gpt-a", "openai/gpt-b")
	if got.Source != fleet.ModelsFromRuntime || got.ReadAt == nil || got.Stored {
		t.Errorf("after the start-up read: %+v, want a runtime list with a readAt that is not marked stored: this run read it", got)
	}
	if got.Default == nil || *got.Default != "openai/gpt-a" {
		t.Errorf("default = %v, want openai/gpt-a: the stored answer carries the runtime's own default", got.Default)
	}
	if n := liveCount(d); n != 0 {
		t.Errorf("%d servers tracked after the read, want none: the server started to ask is stopped once it has answered", n)
	}
	scopedDirs(t, root, 0)

	file := catalogFileIn(state)
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("the read was not stored: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("stored catalog mode = %v, want 0600", info.Mode().Perm())
	}

	// The next start has no create, no marker and no server that could answer: all it
	// has is the file.
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	unplantCatalog(t, root)
	d2 := storedDriver(t, state)
	again := d2.Models()
	wantIDs(t, "after a restart", again, "openai/gpt-a", "openai/gpt-b")
	if again.Source != fleet.ModelsFromRuntime {
		t.Errorf("source after a restart = %q, want %q: a source of its own reads as none to every client that does not know it", again.Source, fleet.ModelsFromRuntime)
	}
	if !again.Stored {
		t.Errorf("after a restart: %+v, want stored set: this run has not read it", again)
	}
	if again.ReadAt == nil || !again.ReadAt.Equal(got.ReadAt.Truncate(time.Millisecond)) {
		t.Errorf("readAt after a restart = %v, want the original read's %v", again.ReadAt, got.ReadAt)
	}
	if again.Default == nil || *again.Default != "openai/gpt-a" {
		t.Errorf("default after a restart = %v, want openai/gpt-a", again.Default)
	}
}

func TestCatalogStore_ARelaunchedSessionsServerIsAskedBeforeAnyOtherIsStarted(t *testing.T) {
	state := t.TempDir()
	_, root := stateLayout(state)
	cwd := t.TempDir()
	plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-a"}}, nil)

	d1 := newStateDriver(t, state)
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Name: "survivor"})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// The runtime now says something else, to the session's server and to any other.
	plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-b"}}, nil)
	plantCatalog(t, root, map[string][]string{"openai": {"gpt-c"}}, nil)

	d2 := catalogDriver(t, state)
	waitFor(t, "the read to replace the stored list", func() bool { return !d2.Models().Stored })
	refreshed(t, d2)
	wantIDs(t, "after the relaunch", d2.Models(), "openai/gpt-b")
	if _, ok := listed(t, d2)[sess.ID]; !ok {
		t.Error("the session is not listed after the restart")
	}
	if n := liveCount(d2); n != 1 {
		t.Errorf("%d servers tracked, want only the relaunched session's: the read needed no server of its own", n)
	}
}

func TestCatalogStore_ParkedSessionsHaveNoServerToAskSoOneIsStartedAndStopped(t *testing.T) {
	cwd := t.TempDir()
	sess, state := parkedThenStopped(t, fleet.SessionSpec{Name: "sleeper", Cwd: fleet.AbsolutePath(cwd)})
	_, root := stateLayout(state)
	plantCatalog(t, root, map[string][]string{"openai": {"gpt-a"}}, nil)

	d2, _ := restartWith(t, state, parkBound, withCatalogRefresh(), catalogTiming)
	waitFor(t, "the start-up read to list the models", func() bool { return d2.Models().Source == fleet.ModelsFromRuntime })
	refreshed(t, d2)

	wantIDs(t, "after a restart with a session parked", d2.Models(), "openai/gpt-a")
	known, ok := d2.wasSeen(sess.ID)
	if !ok {
		t.Fatal("the session is not known after the restart")
	}
	if got := d2.phaseOf(known.srv); got != phaseParked {
		t.Errorf("phase = %d, want parked (%d): the read must not start the session it did not need", got, phaseParked)
	}
	if n := liveCount(d2); n != 0 {
		t.Errorf("%d servers tracked after the read, want none", n)
	}
	scopedDirs(t, root, 1) // the parked session's own, and none of the read's
}

func TestCatalogStore_AFreshReadReplacesTheStoredListAndIsStored(t *testing.T) {
	state := t.TempDir()
	cwd := t.TempDir()
	plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-a"}}, nil)

	d1 := storedDriver(t, state)
	mustCreate(t, d1, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	d2 := storedDriver(t, state)
	stored := d2.Models()
	wantIDs(t, "stored", stored, "openai/gpt-a")
	if !stored.Stored {
		t.Fatalf("stored list not marked stored: %+v", stored)
	}

	// Somebody creates a session: the runtime has changed what it lists.
	plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-b"}}, nil)
	mustCreate(t, d2, "k2", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	fresh := d2.Models()
	wantIDs(t, "after a create", fresh, "openai/gpt-b")
	if fresh.Stored {
		t.Errorf("after a create: %+v, want the stored marker gone: this run read it", fresh)
	}
	if fresh.ReadAt == nil || !fresh.ReadAt.After(*stored.ReadAt) {
		t.Errorf("readAt did not move forward: %v, was %v", fresh.ReadAt, stored.ReadAt)
	}
	if err := d2.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// And what the next restart serves is the new one.
	d3 := storedDriver(t, state)
	next := d3.Models()
	wantIDs(t, "next restart", next, "openai/gpt-b")
	if !next.Stored {
		t.Errorf("next restart: %+v, want stored", next)
	}
}

func TestCatalogStore_ThePolicyIsAppliedWhenTheListIsBuiltNotWhenItIsStored(t *testing.T) {
	state := t.TempDir()
	cwd := t.TempDir()
	plantCatalog(t, cwd, map[string][]string{"deepseek": {"m1"}, "other": {"m2"}}, nil)

	wide := []string{"deepseek", "other"}
	d1 := storedDriver(t, state, WithProviders(wide))
	mustCreate(t, d1, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Model: "deepseek/m1"})
	wantIDs(t, "before the restart", d1.Models(), "deepseek/m1", "other/m2")
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// The operator narrowed the policy while the service was down.
	d2 := storedDriver(t, state, WithProviders([]string{"deepseek"}))
	narrow := d2.Models()
	wantIDs(t, "narrowed across the restart", narrow, "deepseek/m1")
	if !narrow.Stored || narrow.Source != fleet.ModelsFromRuntime {
		t.Errorf("narrowed: %+v, want the stored runtime list", narrow)
	}
	if err := d2.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// Nothing was lost from the file by it: widened again, the list is whole.
	d3 := storedDriver(t, state, WithProviders(wide))
	wantIDs(t, "widened again", d3.Models(), "deepseek/m1", "other/m2")
}

func TestCatalogStore_AFileThatCannotBeUsedReadsAsNoneNeverAnError(t *testing.T) {
	good := catalogRecord{Version: catalogRecordVersion, ReadAtMs: time.Now().UnixMilli(), IDs: []string{"openai/gpt-a"}}
	with := func(f func(*catalogRecord)) []byte {
		r := good
		r.IDs = append([]string(nil), good.IDs...)
		f(&r)
		raw, _ := json.Marshal(r)
		return raw
	}
	cases := map[string][]byte{
		"not json":            []byte("{"),
		"empty":               {},
		"another version":     with(func(r *catalogRecord) { r.Version = 2 }),
		"an id with no /":     with(func(r *catalogRecord) { r.IDs = []string{"gpt-a"} }),
		"an id with no model": with(func(r *catalogRecord) { r.IDs = []string{"openai/"} }),
		"no time":             with(func(r *catalogRecord) { r.ReadAtMs = 0 }),
		"too large":           append([]byte(`{"version":1,"readAtMs":1,"ids":["a/b"],"pad":"`), make([]byte, catalogFileMax)...),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			file := catalogFileIn(state)
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			sink := &parkLog{}
			d := storedDriver(t, state, WithLogf(sink.logf))
			if got := d.Models(); got.Source != fleet.ModelsNone || len(got.IDs) != 0 || got.IDs == nil {
				t.Errorf("Models = %+v, want none with an empty non-null list", got)
			}
			if !sink.has("stored model catalog could not be used") {
				t.Error("no notice that the stored catalog was not used")
			}
			// Whatever it was, the next read replaces it.
			cwd := t.TempDir()
			plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-z"}}, nil)
			mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
			wantIDs(t, "after a create", d.Models(), "openai/gpt-z")
			if snap, err := readCatalogFile(file); err != nil || snap == nil {
				t.Errorf("the file after a read = %v, %v, want a usable one", snap, err)
			}
		})
	}

	t.Run("a directory where the file would be", func(t *testing.T) {
		state := t.TempDir()
		if err := os.MkdirAll(catalogFileIn(state), 0o700); err != nil {
			t.Fatal(err)
		}
		sink := &parkLog{}
		d := storedDriver(t, state, WithLogf(sink.logf))
		if got := d.Models(); got.Source != fleet.ModelsNone {
			t.Errorf("Models = %+v, want none", got)
		}
		// A write that fails costs the next restart its list, never the read that was
		// made, and is said once however many reads follow.
		cwd := t.TempDir()
		plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-z"}}, nil)
		mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
		mustCreate(t, d, "k2", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
		wantIDs(t, "after creates that could not store", d.Models(), "openai/gpt-z")
		n := 0
		sink.mu.Lock()
		for _, ln := range sink.lines {
			if strings.Contains(ln, "could not store the model catalog") {
				n++
			}
		}
		sink.mu.Unlock()
		if n != 1 {
			t.Errorf("%d notices that the catalog could not be stored, want 1", n)
		}
	})
}

func TestCatalogStore_AStoredReadFromAClockAheadOfThisOneDoesNotHoldTheListForEver(t *testing.T) {
	state := t.TempDir()
	cwd := t.TempDir()
	plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-b"}}, nil)
	ahead := &catalogSnapshot{
		answer: catalogAnswer{ids: modelCatalog{"openai/gpt-a"}},
		readAt: time.Now().Add(24 * time.Hour).UTC(),
	}
	if err := writeCatalogFile(catalogFileIn(state), ahead); err != nil {
		t.Fatal(err)
	}
	d := storedDriver(t, state)
	if got := d.Models(); !got.Stored || len(got.IDs) != 1 || got.IDs[0] != "openai/gpt-a" {
		t.Fatalf("stored = %+v, want the stored list", got)
	}
	mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	got := d.Models()
	wantIDs(t, "after a create", got, "openai/gpt-b")
	if got.Stored || got.ReadAt.After(time.Now().Add(time.Hour)) {
		t.Errorf("after a create: %+v, want this run's read", got)
	}
}

func TestCatalogStore_AStartUpReadThatListsNothingEmptiesNoListThatHadModels(t *testing.T) {
	state := t.TempDir()
	_, root := stateLayout(state)
	cwd := t.TempDir()
	plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-a"}}, nil)

	d1 := storedDriver(t, state)
	mustCreate(t, d1, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// The runtime answers, in its own shape, with no provider usable.
	plantCatalog(t, root, map[string][]string{}, nil)
	// Nor does the relaunched session's server list anything now.
	plantCatalog(t, cwd, map[string][]string{}, nil)

	d2 := catalogDriver(t, state)
	refreshed(t, d2)
	got := d2.Models()
	wantIDs(t, "after an empty start-up read", got, "openai/gpt-a")
	if !got.Stored {
		t.Errorf("after an empty start-up read: %+v, want the stored list still served as stored", got)
	}
}

func TestCatalogStore_AStartUpReadThatListsNothingIsKeptWhereNothingWasHeld(t *testing.T) {
	state := t.TempDir()
	_, root := stateLayout(state)
	plantCatalog(t, root, map[string][]string{}, nil)

	d := catalogDriver(t, state)
	waitFor(t, "the start-up read", func() bool { return d.Models().Source == fleet.ModelsFromRuntime })
	refreshed(t, d)
	got := d.Models()
	if got.Source != fleet.ModelsFromRuntime || len(got.IDs) != 0 || got.IDs == nil || got.Stored {
		t.Errorf("Models = %+v, want a runtime list that is empty: the runtime answered and listed nothing usable, which is not 'none'", got)
	}
}

func TestCatalogStore_ARuntimeThatCannotBeAskedLeavesTheStoredListAlone(t *testing.T) {
	state := t.TempDir()
	_, root := stateLayout(state)
	stored := &catalogSnapshot{
		answer: catalogAnswer{ids: modelCatalog{"openai/gpt-a"}},
		readAt: time.Now().Add(-time.Hour).UTC(),
	}
	if err := writeCatalogFile(catalogFileIn(state), stored); err != nil {
		t.Fatal(err)
	}
	// No marker anywhere: the stand-in answers 404 to the catalog, "could not tell".
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	d := catalogDriver(t, state)
	refreshed(t, d)
	got := d.Models()
	wantIDs(t, "after a read that could not tell", got, "openai/gpt-a")
	if !got.Stored {
		t.Errorf("Models = %+v, want the stored list, still stored", got)
	}
}

func TestCatalogStore_AParkedServerIsNotAskedForTheCatalog(t *testing.T) {
	// The read asks only a server that is running. A parked one has no process;
	// asking it would fail, or start the session it was parked to leave stopped.
	d := storedDriver(t, t.TempDir())
	parked := &server{}
	parked.park.Store(&parkNote{})
	d.markSeen("ses_parked", knownSession{srv: parked})
	_, root := stateLayout(d.stateDir)
	plantCatalog(t, root, map[string][]string{"openai": {"gpt-a"}}, nil)

	d.refreshCatalog(context.Background())
	wantIDs(t, "after a read with only a parked session", d.Models(), "openai/gpt-a")
	if n := liveCount(d); n != 0 {
		t.Errorf("%d servers tracked, want none", n)
	}
}

func TestCatalogStore_ShutdownDuringTheReadEndsItAndLeavesNothingBehind(t *testing.T) {
	state := t.TempDir()
	_, root := stateLayout(state)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// The server the read starts is alive but not answering for 20 seconds.
	if err := os.WriteFile(filepath.Join(root, ".fake-slow-ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := catalogDriver(t, state)
	time.Sleep(300 * time.Millisecond) // the read is in its start by now

	began := time.Now()
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("Shutdown took %s with the read in its start, want it to end the read", took)
	}
	select {
	case <-d.refreshDone:
	default:
		t.Error("Shutdown returned while the read was still running")
	}
	if got := d.Models(); got.Source != fleet.ModelsNone {
		t.Errorf("Models = %+v, want none: nothing was read", got)
	}
	scopedDirs(t, root, 0)
}

func TestCatalogStore_NoStateDirectoryStoresNothingAndReadsNothingAtStart(t *testing.T) {
	d, _ := newIsolatedDriver(t, withCatalogRefresh())
	if d.refreshDone != nil {
		t.Error("a start-up read was started with no state directory")
	}
	if d.catalogFile != "" {
		t.Errorf("catalogFile = %q with no state directory", d.catalogFile)
	}
	cwd := t.TempDir()
	plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-a"}}, nil)
	mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	wantIDs(t, "after a create", d.Models(), "openai/gpt-a")
}

func TestCatalogStore_TheFileHoldsNoCredentialPathOrSession(t *testing.T) {
	state := t.TempDir()
	cwd := t.TempDir()
	plantCatalog(t, cwd, map[string][]string{"openai": {"gpt-a"}}, map[string]string{"openai": "gpt-a"})
	d := storedDriver(t, state)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd)})
	raw, err := os.ReadFile(catalogFileIn(state))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	if want := []string{"defaults", "ids", "readAtMs", "version"}; !reflect.DeepEqual(names, want) {
		t.Errorf("stored fields = %v, want %v", names, want)
	}
	for _, leak := range []string{cwd, state, sess.ID} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the stored catalog names %q", leak)
		}
	}
}
