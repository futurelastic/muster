package opencode

import (
	"context"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// A session listed `unknown` — one a restart could not relaunch, so it needs
// re-creation — has no runtime to ask, and a listing must not try (muster #449).
//
// The issue that asked for this was written against the pane-capture cost of a
// listing and named that class too. This driver never captures a screen, and the
// listing's own "listed, not running: nothing to read" is what keeps such a session
// free: it is reported from what this driver already holds, with no request, however
// often it is listed. Pinned here so a change to the listing cannot start asking.
func TestList_ASessionThatNeedsRecreationCostsNoRequest(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	d.markSeen("ses_needs_recreation", knownSession{unknown: "needs re-creation"})
	req := fleet.RequestFrom(fleet.Caller{Principal: "test"})

	before := len(f.requestsSnapshot())
	for i := 0; i < 5; i++ {
		col, err := d.List(context.Background(), req, driver.ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var found bool
		for _, s := range col.Items() {
			if s.ID != "ses_needs_recreation" {
				continue
			}
			found = true
			if s.State.Status != fleet.StatusUnknown {
				t.Fatalf("status = %q, want unknown", s.State.Status)
			}
		}
		if !found {
			t.Fatal("the session needing re-creation is not listed")
		}
	}
	if got := len(f.requestsSnapshot()) - before; got != 0 {
		t.Errorf("five listings of a session that needs re-creation made %d request(s) to the runtime; want none", got)
	}
}
