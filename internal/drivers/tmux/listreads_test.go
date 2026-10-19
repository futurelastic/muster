package tmux

import (
	"os"
	"testing"
	"time"

	"github.com/futurelastic/muster/internal/driver"
)

// countOpens counts opens of one record path for the rest of the test.
func countOpens(t *testing.T, path string) *int {
	t.Helper()
	n := 0
	prev := openRecord
	openRecord = func(name string) (*os.File, error) {
		if name == path {
			n++
		}
		return prev(name)
	}
	t.Cleanup(func() { openRecord = prev })
	return &n
}

// #339: a caller that wants one session must not make the driver read every
// other session's record to answer it.
func TestList_IDFilterReadsOnlyThatSessionsRecord(t *testing.T) {
	d, _, ref, _, conv := titleSyncSession(t)
	appendLine(t, conv, usageEntry_(t, 1, "m1", 10, 20, 0, 0))
	opens := countOpens(t, conv)

	// The record belongs to alpha. Asking for beta must leave it untouched,
	// even though it just grew and a full listing would open it.
	col, err := d.List(t.Context(), testCaller, driver.ListFilter{ID: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if items := col.Items(); len(items) != 1 || items[0].ID != "beta" {
		t.Fatalf("ID filter returned %d sessions, want exactly beta", len(items))
	}
	if *opens != 0 {
		t.Fatalf("listing beta alone opened alpha's record %d times, want 0", *opens)
	}

	col, err = d.List(t.Context(), testCaller, driver.ListFilter{ID: ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	items := col.Items()
	if len(items) != 1 || items[0].ID != ref.ID || items[0].State.Usage == nil || items[0].State.Usage.Input != 10 {
		t.Fatalf("listing alpha = %+v, want only alpha, with its usage", items)
	}
	if *opens == 0 {
		t.Fatal("listing alpha never read alpha's record")
	}
}

// #339: with a freshness bound, a session whose screen has not changed is
// served from its last real read; any change of screen, an expired bound, or
// no bound at all reads the record again.
func TestList_RecordMaxAgeSkipsRecordsOfAnUnchangedScreen(t *testing.T) {
	d, f, ref, _, conv := titleSyncSession(t)
	now := time.Unix(1785760000, 0)
	d.now = func() time.Time { return now }
	const bound = 5 * time.Second

	usageIn := func(filter driver.ListFilter) int64 {
		t.Helper()
		col, err := d.List(t.Context(), testCaller, filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range col.Items() {
			if s.ID == ref.ID {
				if s.State.Usage == nil {
					return 0
				}
				return s.State.Usage.Input
			}
		}
		t.Fatal("alpha not listed")
		return 0
	}

	appendLine(t, conv, usageEntry_(t, 1, "m1", 10, 0, 0, 0))
	if got := usageIn(driver.ListFilter{RecordMaxAge: bound}); got != 10 {
		t.Fatalf("first listing = %d, want 10 (nothing to reuse yet)", got)
	}
	opens := countOpens(t, conv)

	appendLine(t, conv, usageEntry_(t, 2, "m2", 5, 0, 0, 0))
	now = now.Add(time.Second)
	if got := usageIn(driver.ListFilter{RecordMaxAge: bound}); got != 10 || *opens != 0 {
		t.Fatalf("same screen, inside the bound: usage=%d opens=%d, want the reused 10 and no open", got, *opens)
	}
	if got := usageIn(driver.ListFilter{}); got != 15 {
		t.Fatalf("no bound: usage=%d, want 15 — a caller that named no bound always reads", got)
	}

	appendLine(t, conv, usageEntry_(t, 3, "m3", 1, 0, 0, 0))
	f.captures["%1"] = idleFixtureFor("alpha, but the screen moved")
	now = now.Add(time.Second)
	if got := usageIn(driver.ListFilter{RecordMaxAge: bound}); got != 16 {
		t.Fatalf("screen changed: usage=%d, want a fresh 16", got)
	}

	appendLine(t, conv, usageEntry_(t, 4, "m4", 1, 0, 0, 0))
	now = now.Add(bound + time.Second)
	if got := usageIn(driver.ListFilter{RecordMaxAge: bound}); got != 17 {
		t.Fatalf("bound expired: usage=%d, want a fresh 17", got)
	}
}
