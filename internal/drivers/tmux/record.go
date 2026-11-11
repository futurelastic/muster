package tmux

import (
	"context"
	"fmt"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Reading back a session's record (muster #441, #442).
//
// # The boundary, and where it is enforced
//
// docs/adr/441-a-sessions-full-record-is-read-under-its-own-grant.md widens what
// may be read from a session's record, on its own route and under its own grant
// (`record`), and docs/adr/442-… widens what this runtime's reader admits. The line
// is drawn in recordclassify.go and nowhere downstream: the service, the relay and
// the wire type carry whatever the reader returns without looking at it.
//
//   - recordclassify.go: what one line of the runtime's file may yield, an
//     allow-list that fails closed;
//   - recordsources.go: the files a record is read from (the conversation and its
//     sub-tasks), how a call and its result are paired, the page and the cursor;
//   - recordmedia.go: reading one image back by the reference its entry carries.
//
// # The cursor
//
// The first version of the cursor ("r1", #441) is an offset into the append-only
// conversation file, bound to the conversation, and is still accepted. The cursor
// this route issues now ("r2") also carries a position in each sub-task file that
// still needs one, and a watermark that says which files it need not name. Its
// version tag differs from turns' ("1"), so a turns cursor handed to this route —
// or the other way round — is refused as a cursor this route did not issue rather
// than silently read as a position in the wrong shape of page.

var _ driver.RecordReader = (*Driver)(nil)

// recordCursorVersion tells this route's cursor from turns': a cursor is a
// position in a route's own page sequence, and the two sequences differ (the
// same offset yields different entries), so neither route accepts the other's.
const recordCursorVersion = "r1"

// Record implements driver.RecordReader.
func (d *Driver) Record(ctx context.Context, req fleet.Request, ref fleet.SessionRef, q fleet.RecordQuery) (fleet.RecordPage, error) {
	limit := q.Limit
	if limit == 0 {
		limit = fleet.DefaultRecordLimit
	}
	if limit < 0 || limit > fleet.MaxRecordLimit {
		return fleet.RecordPage{}, fmt.Errorf("limit must be between 1 and %d", fleet.MaxRecordLimit)
	}

	ctx, cancel := d.bounded(ctx)
	defer cancel()

	src, _, err := d.sessionRecordSource(ctx, req, ref)
	if err != nil {
		return fleet.RecordPage{}, err
	}
	return readRecordAt(src.path, q.Since, limit, d.now())
}
