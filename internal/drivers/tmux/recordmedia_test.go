package tmux

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Synthetic fixtures only: the "images" here are a signature and padding.

// mediaFixture is a conversation with an image in each place one can be: typed by
// the person, returned by a tool, and written inside a sub-task.
func mediaFixture(t *testing.T) (path string, refs map[string]string, want map[string][]byte) {
	t.Helper()
	person := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 40)...)
	tool := append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{2}, 40)...)
	sub := append([]byte("GIF89a"), bytes.Repeat([]byte{3}, 40)...)
	enc := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

	path = writeTurnsRecord(t,
		user(t, 1, "first", nil),
		userBlocks(t, 2, nil, textBlock("see this"), imageBlockOf("image/png", enc(person))),
		toolUse(t, 3, "t1", "Shot", map[string]any{}),
		toolResult(t, 4, "t1", []map[string]any{{"type": "text", "text": "ok"}, imageBlockOf("image/jpeg", enc(tool))}, false),
		toolUse(t, 5, "task-1", "Task", map[string]any{}))
	writeSub(t, path, "a1", "task-1", true, time.Time{},
		subUser(t, "a1", 6, []map[string]any{{"type": "text", "text": "look"}, imageBlockOf("image/gif", enc(sub))}))

	page, err := readRecord(path, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	refs = map[string]string{}
	for _, m := range page.Messages {
		if m.Kind != fleet.RecordKindImage {
			continue
		}
		switch {
		case m.Parent != "":
			refs["sub"] = m.Media
		case m.Role == fleet.RecordRoleUser:
			refs["person"] = m.Media
		default:
			refs["tool"] = m.Media
		}
	}
	if len(refs) != 3 {
		t.Fatalf("the fixture's page holds %d image references, want 3: %v", len(refs), recs(page))
	}
	return path, refs, map[string][]byte{"person": person, "tool": tool, "sub": sub}
}

func TestReadRecordMedia_ServesTheImageAReferenceNames(t *testing.T) {
	path, refs, want := mediaFixture(t)
	types := map[string]string{"person": "image/png", "tool": "image/jpeg", "sub": "image/gif"}
	for k, ref := range refs {
		got, err := readRecordMedia(path, ref)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if got.ContentType != types[k] || !bytes.Equal(got.Data, want[k]) {
			t.Errorf("%s: %s, %d bytes; want %s and the image's own bytes", k, got.ContentType, len(got.Data), types[k])
		}
	}
	// The reference holds no content and no path.
	for k, ref := range refs {
		raw := string(mustDecode(t, ref))
		if strings.Contains(raw, "PNG") || strings.Contains(raw, os.TempDir()) || strings.Contains(raw, ".jsonl") {
			t.Errorf("%s: the reference carries content or a path: %q", k, raw)
		}
	}
}

func TestReadRecordMedia_AnythingElseIsNoSuchMedia(t *testing.T) {
	path, refs, _ := mediaFixture(t)
	good := string(mustDecode(t, refs["person"]))
	parts := strings.Split(good, "|") // m1 | conv | main | off | c1
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	with := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return enc(strings.Join(p, "|"))
	}

	// the same bytes in a conversation of another name: the session now holds
	// another conversation, so every reference of the old one has expired
	other := filepath.Join(t.TempDir(), "conv-2.jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, body, 0o600); err != nil {
		t.Fatal(err)
	}

	dir := subTaskDir(path)
	linkTarget := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(linkTarget, []byte(subUser(t, "ln", 9, []map[string]any{{"type": "text", "text": "x"}, imageBlockOf("image/png", pngData())})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTarget, filepath.Join(dir, "agent-ln.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-ln.meta.json"), []byte(`{"toolUseId":"task-1","finished":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct{ path, ref string }{
		"not base64":                       {path, "!!!"},
		"empty":                            {path, ""},
		"too few parts":                    {path, enc("m1|conv-1|main|0")},
		"unknown version":                  {path, with(0, "m2")},
		"another conversation":             {path, with(1, "conv-9")},
		"the conversation was replaced":    {other, refs["person"]},
		"a source that is not an id":       {path, with(2, "../x")},
		"a source with no such file":       {path, with(2, "ghost")},
		"a source that is a link":          {path, with(2, "ln")},
		"an offset that is not a number":   {path, with(3, "x")},
		"a negative offset":                {path, with(3, "-5")},
		"an offset past the end":           {path, with(3, "99999999")},
		"an offset inside a line":          {path, with(3, "7")},
		"a block that is text":             {path, with(4, "c0")},
		"a block that does not exist":      {path, with(4, "c9")},
		"a path with no block":             {path, with(4, "c")},
		"a path that is not one":           {path, with(4, "x1")},
		"an inner block of a plain line":   {path, with(4, "c1.r0")},
		"an inner index that is not there": {path, enc(strings.Replace(string(mustDecode(t, refs["tool"])), ".r1", ".r7", 1))},
	}
	for name, c := range cases {
		if _, err := readRecordMedia(c.path, c.ref); !errors.Is(err, fleet.ErrNoSuchMedia) {
			t.Errorf("%s: err = %v, want ErrNoSuchMedia", name, err)
		}
	}
}

// A reference is read through the allow-list again: an entry the runtime rewrote
// into something the list does not admit does not serve, and neither does an image
// whose bytes are not what its line says.
func TestReadRecordMedia_TheAllowListHoldsAtReadTime(t *testing.T) {
	p := writeTurnsRecord(t, userBlocks(t, 1, nil, textBlock("one"), imageBlockOf("image/png", pngData())))
	page, err := readRecord(p, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	ref := page.Messages[1].Media
	if _, err := readRecordMedia(p, ref); err != nil {
		t.Fatal(err)
	}
	// the same line, now marked as the runtime's meta entry
	if err := os.WriteFile(p, []byte(userBlocksMeta(t, 1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecordMedia(p, ref); !errors.Is(err, fleet.ErrNoSuchMedia) {
		t.Errorf("an entry the list no longer admits: err = %v, want ErrNoSuchMedia", err)
	}

	// a signature that passes the scan, then data that is not base64 all the way
	broken := pngData() + "!!!!"
	p2 := writeTurnsRecord(t, userBlocks(t, 1, nil, imageBlockOf("image/png", broken)))
	page, err = readRecord(p2, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("the scan checks only the type and size: %s", recs(page))
	}
	if _, err := readRecordMedia(p2, page.Messages[0].Media); !errors.Is(err, fleet.ErrNoSuchMedia) {
		t.Errorf("undecodable data: err = %v, want ErrNoSuchMedia", err)
	}
}

func userBlocksMeta(t *testing.T, i int) string {
	return userBlocks(t, i, map[string]any{"isMeta": true}, textBlock("one"), imageBlockOf("image/png", pngData()))
}

func TestAdmitImage_SizeAndSignatureDecideWhatIsReferenced(t *testing.T) {
	head := base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 20)...))[:b64HeadChars]
	ok := recImage{present: true, typ: "base64", media: "image/png", data: b64Head{ok: true, n: 400, head: head}}
	if _, good := admitImage(ok); !good {
		t.Fatal("a png of 400 characters was refused")
	}
	for name, mutate := range map[string]func(*recImage){
		"too big":         func(im *recImage) { im.data.n = (fleet.MaxRecordMediaBytes/3 + 1) * 4 },
		"empty":           func(im *recImage) { im.data.n = 0 },
		"not base64 type": func(im *recImage) { im.typ = "url" },
		"unknown type":    func(im *recImage) { im.media = "application/pdf" },
		"svg":             func(im *recImage) { im.media = "image/svg+xml" },
		"wrong signature": func(im *recImage) { im.media = "image/jpeg" },
		"no data":         func(im *recImage) { im.data.ok = false },
		"not present":     func(im *recImage) { im.present = false },
		"head not base64": func(im *recImage) { im.data.head = strings.Repeat("!", b64HeadChars) },
		"head is not data": func(im *recImage) {
			im.data.head = base64.StdEncoding.EncodeToString([]byte("plain text, not an image"))[:b64HeadChars]
		},
	} {
		im := ok
		mutate(&im)
		if _, good := admitImage(im); good {
			t.Errorf("%s: admitted", name)
		}
	}
}

func TestDriverRecordMedia_CorroboratesAndRefuses(t *testing.T) {
	d, _, ref, _, conv := titleSyncSession(t)
	ctx := t.Context()
	appendLine(t, conv, userBlocks(t, 1, nil, textBlock("a picture"), imageBlockOf("image/png", pngData())))
	page, err := d.Record(ctx, fleet.Request{}, ref, fleet.RecordQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[1].Media == "" {
		t.Fatalf("record = %s", recs(page))
	}
	media := page.Messages[1].Media

	got, err := d.RecordMedia(ctx, fleet.Request{}, ref, media)
	if err != nil || got.ContentType != "image/png" || len(got.Data) == 0 {
		t.Fatalf("media = %+v, %v", got, err)
	}
	if _, err := d.RecordMedia(ctx, withStartedAt(fleet.Request{}, startedAtFixture()), ref, media); err != nil {
		t.Errorf("matching startedAt: %v", err)
	}
	_, err = d.RecordMedia(ctx, withStartedAt(fleet.Request{}, startedAtFixture().Add(time.Hour)), ref, media)
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("recycled id: err = %v, want ErrAmbiguousTarget", err)
	}
	_, err = d.RecordMedia(ctx, fleet.Request{}, fleet.SessionRef{Machine: "testbox", ID: "nobody"}, media)
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("unknown session: err = %v, want ErrNoSuchSession", err)
	}
	_, err = d.RecordMedia(ctx, fleet.Request{}, fleet.SessionRef{Machine: "testbox", ID: "beta"}, media)
	if !errors.Is(err, fleet.ErrNoTurnRecord) {
		t.Errorf("session with no record: err = %v, want ErrNoTurnRecord", err)
	}
	// a reference of another conversation, handed to this session
	foreign := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(mustDecode(t, media)), "|conv-1|", "|conv-7|", 1)))
	if _, err := d.RecordMedia(ctx, fleet.Request{}, ref, foreign); !errors.Is(err, fleet.ErrNoSuchMedia) {
		t.Errorf("another conversation's reference: err = %v, want ErrNoSuchMedia", err)
	}
	if _, ok := any(d).(driver.RecordMediaReader); !ok {
		t.Fatal("the terminal driver does not implement driver.RecordMediaReader")
	}
}
