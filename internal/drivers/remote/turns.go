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

// Turns reads a peer session's assistant turns (muster #258).
//
// The query and the caller's startedAt are forwarded rather than interpreted
// here, for the reason Close and Keys forward theirs: the session, its record
// and the cursor's meaning all live on the peer, so corroboration and the
// boundary on what may be returned are applied there, by the machine that owns
// the data, against the caller's asserted identity (§13). This driver never
// reads, filters or rewrites a turn; it carries the peer's page back as it came.
func (d *Driver) Turns(ctx context.Context, req fleet.Request, ref fleet.SessionRef, q fleet.TurnsQuery) (fleet.TurnsPage, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/turns",
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
	var out fleet.TurnsPage
	path = withRuntimeHint(req, path)
	if err := d.do(ctx, req, http.MethodGet, path, nil, &out); err != nil {
		if routeMissing(err) {
			return fleet.TurnsPage{}, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: fmt.Sprintf("peer %s has no turns route (older build)", d.machine),
				Machine: d.machine,
			}
		}
		return fleet.TurnsPage{}, err
	}
	if out.Turns == nil {
		out.Turns = []fleet.Turn{}
	}
	return out, nil
}

var _ driver.TurnReader = (*Driver)(nil)
