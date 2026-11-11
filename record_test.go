package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The wire shape of the record route (muster #441). Synthetic values only.

func TestRecordPage_EmptyIsAListWithCursorAndFormat(t *testing.T) {
	b, err := json.Marshal(RecordPage{Messages: []RecordMessage{}, Next: "c", Format: RecordFormat})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"messages":[]`, `"next":"c"`, `"format":1`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %s", b, want)
		}
	}
}

// A text entry carries only the keys a text entry has; the keys later kinds fill
// are absent, not empty.
func TestRecordMessage_TextEntryCarriesOnlyItsKeys(t *testing.T) {
	at := time.Date(2026, 10, 4, 10, 0, 4, 0, time.UTC)
	keys := func(m RecordMessage) map[string]bool {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for k := range raw {
			out[k] = true
		}
		return out
	}
	same := func(got map[string]bool, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("keys = %v, want %v", got, want)
			return
		}
		for _, k := range want {
			if !got[k] {
				t.Errorf("keys = %v, want %v", got, want)
			}
		}
	}
	same(keys(RecordMessage{At: at, Kind: RecordKindText, Role: RecordRoleUser, Text: "x"}), "at", "kind", "role", "text")
	same(keys(RecordMessage{At: at, Kind: RecordKindText, Role: RecordRoleAssistant, Text: "x", Model: "m"}), "at", "kind", "role", "text", "model")
	same(keys(RecordMessage{At: at, Kind: RecordKindText, Role: RecordRoleUser, Text: "x", Truncated: true}), "at", "kind", "role", "text", "truncated")
	// The keys format 1 settles for the kinds the terminal runtime fills since #442.
	same(keys(RecordMessage{At: at, Kind: RecordKindTool, Role: RecordRoleAssistant, Parent: "p", Tool: &RecordTool{ID: "i", Name: "n"}}),
		"at", "kind", "role", "parent", "tool")
	same(keys(RecordMessage{At: at, Kind: RecordKindImage, Role: RecordRoleUser, Media: "ref"}), "at", "kind", "role", "media")
}

func TestRecordKindsAreTheOnesTheIssueNames(t *testing.T) {
	want := map[RecordKind]string{
		RecordKindText: "text", RecordKindReasoning: "reasoning", RecordKindTool: "tool",
		RecordKindImage: "image", RecordKindSummary: "summary", RecordKindSystem: "system",
	}
	for k, s := range want {
		if string(k) != s {
			t.Errorf("kind %q, want %q", k, s)
		}
	}
	if RecordRoleUser != "user" || RecordRoleAssistant != "assistant" {
		t.Errorf("roles = %q, %q", RecordRoleUser, RecordRoleAssistant)
	}
}

func TestDriverCapabilities_ReadsRecordIsDeclaredNotAssumed(t *testing.T) {
	caps := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved}
	b, _ := json.Marshal(caps)
	if strings.Contains(string(b), "readsRecord") {
		t.Fatalf("an undeclared capability must not be serialised: %s", b)
	}
	b, _ = json.Marshal(caps.Assumed())
	if strings.Contains(string(b), "readsRecord") {
		t.Fatalf("an assumed floor must not declare the capability: %s", b)
	}
	for _, want := range []bool{true, false} {
		caps.ReadsRecord = Declared(want)
		b, _ := json.Marshal(caps)
		var back DriverCapabilities
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.ReadsRecord == nil || *back.ReadsRecord != want {
			t.Errorf("round trip of %v: %v", want, back.ReadsRecord)
		}
	}
}

// The words a tool message's status takes, and the shape of one on the wire (#442):
// a running call has no result key, a finished one has both.
func TestRecordTool_StatusWordsAndWireShape(t *testing.T) {
	if RecordToolRunning != "running" || RecordToolCompleted != "completed" || RecordToolError != "error" {
		t.Errorf("status words = %q, %q, %q", RecordToolRunning, RecordToolCompleted, RecordToolError)
	}
	at := time.Date(2026, 10, 4, 10, 0, 4, 0, time.UTC)
	running, _ := json.Marshal(RecordMessage{At: at, Kind: RecordKindTool, Role: RecordRoleAssistant,
		Tool: &RecordTool{ID: "t1", Name: "Bash", Input: json.RawMessage(`{"c":1}`), Status: RecordToolRunning}})
	if !strings.Contains(string(running), `"input":{"c":1}`) || !strings.Contains(string(running), `"status":"running"`) || strings.Contains(string(running), `"result"`) {
		t.Errorf("a running call: %s", running)
	}
	done, _ := json.Marshal(RecordMessage{At: at, Kind: RecordKindTool, Role: RecordRoleAssistant, Parent: "p",
		Tool: &RecordTool{ID: "t1", Name: "Bash", Result: "out", Status: RecordToolCompleted}})
	for _, want := range []string{`"result":"out"`, `"status":"completed"`, `"parent":"p"`} {
		if !strings.Contains(string(done), want) {
			t.Errorf("a finished call lacks %s: %s", want, done)
		}
	}
}

// What the media route may serve is a short, closed list of image types, and an
// image of any size is bounded.
func TestRecordMedia_ServableTypesAreClosed(t *testing.T) {
	want := []string{"image/png", "image/jpeg", "image/gif", "image/webp"}
	if strings.Join(RecordMediaTypes, ",") != strings.Join(want, ",") {
		t.Errorf("RecordMediaTypes = %v, want %v", RecordMediaTypes, want)
	}
	if MaxRecordMediaBytes != 16<<20 {
		t.Errorf("MaxRecordMediaBytes = %d", MaxRecordMediaBytes)
	}
}
