package tmux

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Reading one image of a session's record back (muster #442).
//
// A page of the record never carries an image's bytes: an `image` entry carries a
// REFERENCE, and this file is what turns one back into the bytes.
//
// # What a reference is
//
// Where in which file the image block sits, bound to the conversation it was read
// from: `m1|<conversation>|<source>|<line offset>|<block path>`, encoded. It holds
// no content, no path and nothing a caller could use to name a different file, and
// the service keeps nothing about it, so it is valid exactly as long as the
// runtime's file holds the entry: it stops working when the conversation is
// replaced (a /clear or a relaunch points the session at another file) or the file
// goes. There is no time limit of its own.
//
// # What a read does with one
//
// It resolves the session as a record read does, so a recycled id is refused
// before anything is opened; requires the reference's conversation to be the one
// the session holds NOW; finds the source by the rules sub-task files are found
// by (a name that is not an id, a link, or a file without a sidecar is not a
// source); requires the offset to be a line start; reads that one line; classifies
// it with the SAME allow-list that produced the reference and requires the block
// at the named path to be an image that list admits; decodes it; and checks the
// bytes are the type claimed. Every way that can fail is one answer,
// fleet.ErrNoSuchMedia: telling a caller which references exist, or why one does
// not, is information about a record it may not have read.

var _ driver.RecordMediaReader = (*Driver)(nil)

// RecordMedia implements driver.RecordMediaReader.
func (d *Driver) RecordMedia(ctx context.Context, req fleet.Request, ref fleet.SessionRef, media string) (fleet.RecordMedia, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	src, _, err := d.sessionRecordSource(ctx, req, ref)
	if err != nil {
		return fleet.RecordMedia{}, err
	}
	return readRecordMedia(src.path, media)
}

// mediaRefParts is a reference taken apart.
type mediaRefParts struct {
	agent string // empty for the conversation file
	off   int64
	block int
	inner int // -1 for a block of the message itself, else the block inside a tool result
}

func parseMediaRef(ref, conv string) (mediaRefParts, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(ref)
	if err != nil {
		return mediaRefParts{}, false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 5 || parts[0] != "m1" || parts[1] != conv {
		return mediaRefParts{}, false
	}
	p := mediaRefParts{inner: -1}
	if parts[2] != "main" {
		if !validRecordID(parts[2]) {
			return mediaRefParts{}, false
		}
		p.agent = parts[2]
	}
	if p.off, err = strconv.ParseInt(parts[3], 10, 64); err != nil || p.off < 0 {
		return mediaRefParts{}, false
	}
	blockS, ok := strings.CutPrefix(parts[4], "c")
	if !ok {
		return mediaRefParts{}, false
	}
	blockS, innerS, hasInner := strings.Cut(blockS, ".r")
	if p.block, err = strconv.Atoi(blockS); err != nil || p.block < 0 {
		return mediaRefParts{}, false
	}
	if hasInner {
		if p.inner, err = strconv.Atoi(innerS); err != nil || p.inner < 0 {
			return mediaRefParts{}, false
		}
	}
	return p, true
}

// readRecordMedia answers one media read against one conversation file. Split from
// RecordMedia so the reading — the part that holds the boundary — is testable
// without a multiplexer.
func readRecordMedia(convPath, ref string) (fleet.RecordMedia, error) {
	noSuch := fleet.RecordMedia{}
	conv := strings.TrimSuffix(filepath.Base(convPath), ".jsonl")
	p, ok := parseMediaRef(ref, conv)
	if !ok {
		return noSuch, fleet.ErrNoSuchMedia
	}

	path := convPath
	if p.agent != "" {
		s, ok := loadSubTask(subTaskDir(convPath), p.agent)
		if !ok {
			return noSuch, fleet.ErrNoSuchMedia
		}
		path = s.path
	}
	f, err := os.Open(path)
	if err != nil {
		return noSuch, fleet.ErrNoSuchMedia
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || p.off >= info.Size() {
		return noSuch, fleet.ErrNoSuchMedia
	}
	if start, err := alignToLine(f, p.off); err != nil || start != p.off {
		return noSuch, fleet.ErrNoSuchMedia
	}
	line, _, complete, _ := readBoundedLine(bufio.NewReaderSize(io.NewSectionReader(f, p.off, info.Size()-p.off), 64<<10), recordEntryLineLimit)
	if !complete || line == nil {
		return noSuch, fleet.ErrNoSuchMedia
	}

	// The same allow-list that issued the reference must still admit the entry and
	// the image in it.
	want := fmt.Sprintf("c%d", p.block)
	if p.inner >= 0 {
		want = fmt.Sprintf("c%d.r%d", p.block, p.inner)
	}
	entries, _ := classifyRecordLine(line, recScope{conv: conv, agent: p.agent})
	admitted := false
	for _, e := range entries {
		if e.kind == recMessage && e.msg.Kind == fleet.RecordKindImage && e.mediaPath == want {
			admitted = true
		}
	}
	if !admitted {
		return noSuch, fleet.ErrNoSuchMedia
	}
	return extractImage(line, p)
}

// extractImage decodes the one image block a reference names, from a line that has
// been classified and found to hold it.
func extractImage(line []byte, p mediaRefParts) (fleet.RecordMedia, error) {
	var env struct {
		Message struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &env) != nil || p.block >= len(env.Message.Content) {
		return fleet.RecordMedia{}, fleet.ErrNoSuchMedia
	}
	block := env.Message.Content[p.block]
	if p.inner >= 0 {
		var tr struct {
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(block, &tr) != nil || p.inner >= len(tr.Content) {
			return fleet.RecordMedia{}, fleet.ErrNoSuchMedia
		}
		block = tr.Content[p.inner]
	}
	var img struct {
		Type   string `json:"type"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if json.Unmarshal(block, &img) != nil || img.Type != "image" || img.Source.Type != "base64" {
		return fleet.RecordMedia{}, fleet.ErrNoSuchMedia
	}
	data, err := base64.StdEncoding.DecodeString(img.Source.Data)
	if err != nil || len(data) == 0 || len(data) > fleet.MaxRecordMediaBytes {
		return fleet.RecordMedia{}, fleet.ErrNoSuchMedia
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	if http.DetectContentType(sniff) != img.Source.MediaType {
		return fleet.RecordMedia{}, fleet.ErrNoSuchMedia
	}
	return fleet.RecordMedia{ContentType: img.Source.MediaType, Data: data}, nil
}
