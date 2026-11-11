package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Record reads a peer session's record (muster #441).
//
// Forwarded exactly as Turns is, for the same reason: the session, its record
// and the cursor's meaning all live on the peer, so corroboration, the boundary
// on what may be returned and the `record` grant are all applied there, by the
// machine that owns the data, against the caller's asserted identity (§13). This
// driver never reads, filters or rewrites a message; it carries the peer's page
// back as it came — Format included, which is the peer's to state.
func (d *Driver) Record(ctx context.Context, req fleet.Request, ref fleet.SessionRef, q fleet.RecordQuery) (fleet.RecordPage, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/record",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	v := url.Values{}
	if q.Since != "" {
		v.Set("since", q.Since)
	}
	if q.Limit != 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if want := req.Expect.StartedAt; want != nil {
		v.Set("startedAt", want.UTC().Format(time.RFC3339Nano))
	}
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var out fleet.RecordPage
	if err := d.do(ctx, req, http.MethodGet, path, nil, &out); err != nil {
		if routeMissing(err) {
			return fleet.RecordPage{}, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: fmt.Sprintf("peer %s has no record route (older build)", d.machine),
				Machine: d.machine,
			}
		}
		return fleet.RecordPage{}, err
	}
	if out.Messages == nil {
		out.Messages = []fleet.RecordMessage{}
	}
	return out, nil
}

var _ driver.RecordReader = (*Driver)(nil)
