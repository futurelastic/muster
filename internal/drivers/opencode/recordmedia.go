package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// Reading one image of a session's record back (muster #443, on the route of #442).
//
// A page of the record never carries an image's bytes: an `image` entry carries a
// REFERENCE, and this file is what turns one back into the bytes.
//
// # What a reference is
//
// A place in one session's record — which message, which part, and which of a tool's
// attachments — bound to the conversation it was read from (the session's id and its
// start time). It holds no content and no path, and the service keeps nothing about it,
// so it is valid for exactly as long as the runtime's record holds the entry. It stops
// working when the id now names another session or the message is deleted or reverted.
// There is no time limit of its own.
//
// # What a read does with one
//
// It resolves the session as a record read does, so a recycled id is refused before
// anything is read; requires the reference's conversation to be the session's now;
// reads the one message the reference names; classifies it with the SAME allow-list
// that produced the reference and requires that list to emit this very reference;
// decodes the image and checks the bytes are the type claimed. Every way that can fail
// is one answer, fleet.ErrNoSuchMedia: telling a caller which references exist, or why
// one does not, is information about a record it may not have read. A failure to read
// at all (the runtime is unreachable) is not one of them: it is the error it is.

// RecordMedia implements driver.RecordMediaReader.
func (d *Driver) RecordMedia(ctx context.Context, req fleet.Request, ref fleet.SessionRef, media string) (fleet.RecordMedia, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()
	srv, created, err := d.openRead(ctx, req, ref, "record media")
	if err != nil {
		return fleet.RecordMedia{}, err
	}

	r, ok := parseMediaRef(media)
	if !ok || r.S != ref.ID || r.C != created.UnixMilli() {
		return fleet.RecordMedia{}, fleet.ErrNoSuchMedia
	}

	var raw json.RawMessage
	path := "/session/" + url.PathEscape(ref.ID) + "/message/" + url.PathEscape(r.M)
	if err := d.do(ctx, srv, "GET", path, nil, &raw); err != nil {
		if isNotFound(err) {
			return fleet.RecordMedia{}, fleet.ErrNoSuchMedia
		}
		return fleet.RecordMedia{}, err
	}

	sc := recordScope{session: ref.ID, created: created.UnixMilli()}
	rec := classifyRecordMessage(raw, sc)
	if rec.id != r.M || !rec.admitted || !referencesImage(&rec, media, r) {
		return fleet.RecordMedia{}, fleet.ErrNoSuchMedia
	}
	return extractImage(raw, r)
}

// referencesImage reports whether the allow-list emits this reference for the message:
// an image of a user message, or one a completed tool returned.
func referencesImage(rec *recordMessage, ref string, r mediaRef) bool {
	if r.P >= len(rec.parts) {
		return false
	}
	p := &rec.parts[r.P]
	switch p.kind {
	case partEntry:
		return r.A == -1 && p.entry.Kind == fleet.RecordKindImage && p.entry.Media == ref
	case partTool:
		if r.A < 0 || !p.terminal() {
			return false
		}
		for _, a := range p.attachment {
			if a.Media == ref {
				return true
			}
		}
	}
	return false
}

// extractImage decodes the one image a reference names, from a message that has been
// classified and found to hold it.
func extractImage(raw json.RawMessage, r mediaRef) (fleet.RecordMedia, error) {
	noSuch := fleet.RecordMedia{}
	var m struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if json.Unmarshal(raw, &m) != nil || r.P >= len(m.Parts) {
		return noSuch, fleet.ErrNoSuchMedia
	}
	part := m.Parts[r.P]
	if r.A >= 0 {
		var t struct {
			State struct {
				Attachments []json.RawMessage `json:"attachments"`
			} `json:"state"`
		}
		if json.Unmarshal(part, &t) != nil || r.A >= len(t.State.Attachments) {
			return noSuch, fleet.ErrNoSuchMedia
		}
		part = t.State.Attachments[r.A]
	}
	var f struct {
		Type string `json:"type"`
		Mime string `json:"mime"`
		URL  string `json:"url"`
	}
	if json.Unmarshal(part, &f) != nil || f.Type != "file" {
		return noSuch, fleet.ErrNoSuchMedia
	}
	payload, ok := strings.CutPrefix(f.URL, "data:"+f.Mime+";base64,")
	if !ok {
		return noSuch, fleet.ErrNoSuchMedia
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(data) == 0 || len(data) > fleet.MaxRecordMediaBytes {
		return noSuch, fleet.ErrNoSuchMedia
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	if http.DetectContentType(sniff) != f.Mime {
		return noSuch, fleet.ErrNoSuchMedia
	}
	return fleet.RecordMedia{ContentType: f.Mime, Data: data}, nil
}
