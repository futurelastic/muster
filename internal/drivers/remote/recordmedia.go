package remote

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// RecordMedia reads an image of a peer session's record (muster #442).
//
// Forwarded as Record is, for the same reason: the session, its record, the
// reference's meaning and the `record` grant are all on the peer, which applies
// them to the caller's asserted identity (§13). This driver never reads or
// rewrites the bytes; it carries them back as they came, after checking that what
// the peer sent is an image of a type this API names within the size cap. A peer
// is another machine's service, and these bytes end up under a viewer: nothing
// else is passed on.
func (d *Driver) RecordMedia(ctx context.Context, req fleet.Request, ref fleet.SessionRef, media string) (fleet.RecordMedia, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/record/media/%s",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID), url.PathEscape(media))
	if want := req.Expect.StartedAt; want != nil {
		path += "?" + url.Values{"startedAt": {want.UTC().Format(time.RFC3339Nano)}}.Encode()
	}
	var out imageBody
	if err := d.do(ctx, req, http.MethodGet, path, nil, &out); err != nil {
		if routeMissing(err) {
			return fleet.RecordMedia{}, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: fmt.Sprintf("peer %s has no record media route (older build)", d.machine),
				Machine: d.machine,
			}
		}
		return fleet.RecordMedia{}, err
	}
	return out.media, nil
}

var _ driver.RecordMediaReader = (*Driver)(nil)

// imageBody reads an image response: the type the peer states, and the bytes, no
// more than the cap.
type imageBody struct{ media fleet.RecordMedia }

func (b *imageBody) readBody(resp *http.Response, machine fleet.MachineId) error {
	ct, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	ok := false
	for _, t := range fleet.RecordMediaTypes {
		if err == nil && ct == t {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("remote: %s answered a media read with %q, which is not an image type this API serves", machine, resp.Header.Get("Content-Type"))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, fleet.MaxRecordMediaBytes+1))
	if err != nil {
		return fmt.Errorf("remote: reading the image from %s: %w", machine, err)
	}
	if len(data) == 0 || len(data) > fleet.MaxRecordMediaBytes {
		return fmt.Errorf("remote: %s answered a media read with %d bytes, outside what this API serves", machine, len(data))
	}
	b.media = fleet.RecordMedia{ContentType: ct, Data: data}
	return nil
}
