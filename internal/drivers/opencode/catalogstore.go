package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
)

// The catalog that outlives a restart (muster #496).
//
// GET /v1/runtimes lists the models a create on this machine would accept, from
// the last catalog read (catalog.go, muster #400). That read lived only in memory,
// so every restart — every update — put the row back at `none` until somebody
// created a session through the API, and a client that offers no picker for a
// runtime with no list had no way to make that create happen.
//
// Two things close it, and neither makes the list less honest:
//
//   - Every usable read is kept in the state directory, and read again at start-up.
//     It is served as the same source it was (`runtime`, with its own readAt) plus a
//     marker that says it was read by an earlier run (`stored`), until a fresh read
//     replaces it. A source of its own would read as `none` to every client that
//     predates it, which is the bug.
//   - The service reads the catalog itself once it is up: from a relaunched session's
//     server when one is running, else from one short-lived server of its own that
//     answers nothing and is gone as soon as it has been asked (refreshCatalog).
//
// The file holds only ids, the runtime's own default per provider, and a time: no
// credential, no path, no session. The provider policy is applied when the list is
// built (Models), never when the file is written, so a policy narrowed across the
// restart narrows what the stored list says.

// catalogRecordVersion is the format of the stored catalog; a file with another is
// not understood and reads as no catalog, never as an error.
const catalogRecordVersion = 1

// catalogFileMax bounds what a stored catalog may be before it is not read at all.
// A real one is some tens of kilobytes.
const catalogFileMax = 4 << 20

// catalogRecord is the stored catalog. It is catalogAnswer plus when it was read.
type catalogRecord struct {
	Version  int               `json:"version"`
	ReadAtMs int64             `json:"readAtMs"`
	IDs      []string          `json:"ids"`
	Defaults map[string]string `json:"defaults,omitempty"`
}

// stateBase is the directory a state directory keeps everything of this driver's in.
func stateBase(stateDir string) string { return filepath.Join(stateDir, "opencode") }

// catalogFileIn is where the stored catalog lives. Beside the session records and
// the session directories, in neither of them: the sweeps of those two read every
// entry as a session's, and this is nobody's.
func catalogFileIn(stateDir string) string {
	return filepath.Join(stateBase(stateDir), "catalog.json")
}

// readCatalogFile reads the stored catalog. A file that is not there is nil with no
// error (nothing has been stored yet); one that is there and cannot be used — not
// readable, not JSON, another version, ids that are not "provider/model" — is nil
// with the reason, which the caller logs and then proceeds as if nothing were
// stored. The reads of the runtime that follow replace it.
func readCatalogFile(path string) (*catalogSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, catalogFileMax+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > catalogFileMax {
		return nil, fmt.Errorf("larger than %d bytes", catalogFileMax)
	}
	var rec catalogRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("not a stored catalog: %w", err)
	}
	if rec.Version != catalogRecordVersion {
		return nil, fmt.Errorf("version %d is not one this build reads", rec.Version)
	}
	if rec.ReadAtMs <= 0 {
		return nil, errors.New("it names no time it was read")
	}
	seen := make(map[string]bool, len(rec.IDs))
	ids := make(modelCatalog, 0, len(rec.IDs))
	for _, id := range rec.IDs {
		provider, model, ok := strings.Cut(id, "/")
		if !ok || provider == "" || model == "" {
			return nil, fmt.Errorf("%q is not a provider/model id", id)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	defaults := make(map[string]string, len(rec.Defaults))
	for p, m := range rec.Defaults {
		defaults[p] = m
	}
	return &catalogSnapshot{
		answer: catalogAnswer{ids: ids, defaults: defaults},
		readAt: time.UnixMilli(rec.ReadAtMs).UTC(),
		stored: true,
	}, nil
}

// writeCatalogFile stores snap atomically, readable by the service user alone.
func writeCatalogFile(path string, snap *catalogSnapshot) error {
	rec := catalogRecord{
		Version:  catalogRecordVersion,
		ReadAtMs: snap.readAt.UnixMilli(),
		IDs:      append([]string{}, snap.answer.ids...),
		Defaults: snap.answer.defaults,
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := privfs.MkdirAll(dir); err != nil {
		return err
	}
	tmp, err := privfs.CreateTemp(dir, ".catalog-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o600)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return werr
	}
	return nil
}

// loadStoredCatalog makes the catalog an earlier run stored this driver's last
// read, marked stored, before anything is resumed. Without a state directory there
// is nothing to read. A file that cannot be used is said once and then ignored.
func (d *Driver) loadStoredCatalog() {
	if d.catalogFile == "" {
		return
	}
	snap, err := readCatalogFile(d.catalogFile)
	if err != nil {
		d.say("opencode: the stored model catalog could not be used (%v); the list is read again from the runtime", err)
		return
	}
	if snap == nil {
		return
	}
	d.catMu.Lock()
	defer d.catMu.Unlock()
	if d.lastCatalog == nil {
		d.lastCatalog = snap
	}
}

// persistCatalog stores snap, with d.catMu held so the file always says what the
// memory says. A write that fails costs the next restart its list, never the read
// that was made: it is said when it starts failing and when it stops.
func (d *Driver) persistCatalog(snap *catalogSnapshot) {
	if d.catalogFile == "" {
		return
	}
	err := writeCatalogFile(d.catalogFile, snap)
	switch {
	case err != nil && !d.catalogWriteFailed:
		d.say("opencode: could not store the model catalog (%v); a restart will not have it until it is stored", err)
	case err == nil && d.catalogWriteFailed:
		d.say("opencode: the model catalog is being stored again")
	}
	d.catalogWriteFailed = err != nil
}

// holdsCatalogModels reports whether the last read, stored or fresh, lists a model.
func (d *Driver) holdsCatalogModels() bool {
	d.catMu.Lock()
	defer d.catMu.Unlock()
	return d.lastCatalog != nil && len(d.lastCatalog.answer.ids) > 0
}

// sameAnswer reports whether two answers list the same ids and defaults.
func sameAnswer(a, b catalogAnswer) bool {
	if len(a.ids) != len(b.ids) || len(a.defaults) != len(b.defaults) {
		return false
	}
	for i := range a.ids {
		if a.ids[i] != b.ids[i] {
			return false
		}
	}
	for p, m := range a.defaults {
		if b.defaults[p] != m {
			return false
		}
	}
	return true
}

// startCatalogRefresh starts the read of the catalog that a restart owes the list
// (muster #496). It runs only with a state directory — the same condition as the
// stored file, and as everything else the driver does at a start — and stops with
// Shutdown. In the background: a runtime that is slow to start must not hold the
// service's start, and the stored list is already being served meanwhile.
func (d *Driver) startCatalogRefresh() {
	if d.noCatalogRefresh || d.store == nil || d.shared != nil || d.refreshDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.refreshCancel = cancel
	d.refreshDone = make(chan struct{})
	done := d.refreshDone
	go func() {
		defer close(done)
		d.refreshCatalog(ctx)
	}()
}

// stopCatalogRefresh ends the refresh and waits for it, so the server it may have
// started is gone before Shutdown stops the others. Safe to call any number of times.
func (d *Driver) stopCatalogRefresh() {
	if d.refreshDone == nil {
		return
	}
	d.refreshCancel()
	<-d.refreshDone
}

// refreshCatalog reads /config/providers once the service is up, so the list a
// restart left stored (or lacked) is replaced by what the runtime says now. It asks
// a session's own server when one is running — those were relaunched a moment ago
// with this machine's credentials — and otherwise starts one server of its own, with
// no session on it: no prompt is sent and nothing is spent, and it is stopped as soon
// as it has answered. Sessions that are parked have no server to ask and are not
// started for this.
func (d *Driver) refreshCatalog(ctx context.Context) {
	d.mu.RLock()
	var running []*server
	for _, k := range d.seen {
		if k.srv != nil && d.phaseOf(k.srv) == phaseRunning {
			running = append(running, k.srv)
		}
	}
	d.mu.RUnlock()
	for _, srv := range running {
		if ctx.Err() != nil {
			return
		}
		if d.readForRefresh(ctx, srv) {
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	rctx, cancel := d.bounded(ctx)
	defer cancel()
	// The probe's working directory is the session root: a directory this driver
	// owns, holding nothing a project would configure. It is started as a create
	// with no agent and no model would be, so it has the credentials such a create
	// has and answers what such a create would be told.
	probe, err := d.startSessionServer(rctx, fleet.SessionSpec{Cwd: fleet.AbsolutePath(d.sessionRoot)})
	if err != nil {
		if ctx.Err() == nil {
			d.say("opencode: the model catalog could not be read at start-up, because the runtime did not start: %v", err)
		}
		return
	}
	defer d.discardServer(probe)
	d.readForRefresh(ctx, probe)
}

// readForRefresh reads srv's catalog until two answers in a row agree or the settle
// window ends, and records what it learned. The first answer of a server that has
// only just started can be the list built into the binary, before it has fetched a
// newer one (muster #384), so one answer is not trusted; a create, which needs no
// such care, has its own settle. It reports whether the server gave an answer at
// all, which is what decides whether another server is worth asking.
//
// What a start-up read records is narrower than what a create's does: an answer that
// lists models is the latest read and replaces whatever is held, but an answer that
// lists NONE replaces nothing that lists some. A restart never empties a runtime that
// had a list; the create-time check, which reads for itself, is what refuses a model
// the runtime no longer has.
func (d *Driver) readForRefresh(ctx context.Context, srv *server) bool {
	window := time.NewTimer(d.catalogSettle)
	defer window.Stop()
	tick := time.NewTicker(d.catalogPoll)
	defer tick.Stop()
	var prev *catalogAnswer
	answered := false
	for {
		ans, read, err := d.fetchCatalog(ctx, srv)
		if err != nil {
			return answered
		}
		switch read {
		case catalogListed:
			d.noteCatalog(ans)
		case catalogEmpty:
			if !d.holdsCatalogModels() {
				d.noteCatalog(ans)
			}
		}
		if read != catalogUnknown {
			answered = true
			if prev != nil && sameAnswer(*prev, ans) {
				return true
			}
			a := ans
			prev = &a
		}
		select {
		case <-ctx.Done():
			return answered
		case <-window.C:
			return answered
		case <-tick.C:
		}
	}
}
