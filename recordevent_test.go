package fleet

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// The session.record event exists so a client learns that a record grew without
// the record travelling on the feed (muster #444). What it may carry is a
// reference and a cursor, and that is held by keys, not by prose: a field added
// to the payload is a field of content risk and must fail here first.
func TestSessionRecordPayloadCarriesNoKeysButReferenceAndCursor(t *testing.T) {
	started := time.Date(2026, 11, 11, 6, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		in   SessionRecordPayload
		want []string
	}{
		"with a start": {
			SessionRecordPayload{Ref: SessionRef{Machine: "m", ID: "s1", Name: "n"}, StartedAt: &started, Cursor: "c1"},
			[]string{"cursor", "ref", "startedAt"},
		},
		"without a start": {
			SessionRecordPayload{Ref: SessionRef{Machine: "m", ID: "s1"}, Cursor: "c1"},
			[]string{"cursor", "ref"},
		},
	} {
		b, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(b, &keys); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := make([]string, 0, len(keys))
		for k := range keys {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: payload keys = %v, want exactly %v (%s)", name, got, tc.want, b)
		}
	}

	// And the Go type itself has no field beyond those three, so a driver cannot
	// put content into it without changing this test.
	typ := reflect.TypeOf(SessionRecordPayload{})
	if typ.NumField() != 3 {
		t.Fatalf("SessionRecordPayload has %d fields; the event is a reference and a cursor (ref, startedAt, cursor)", typ.NumField())
	}
}

func TestEventSessionRecordNameIsTheSpecifiedOne(t *testing.T) {
	if EventSessionRecord != "session.record" {
		t.Fatalf("EventSessionRecord = %q, want %q (api-http.md §4)", EventSessionRecord, "session.record")
	}
}

// recordFormat is declared by a runtime that reads a record, and by nobody else:
// an undeclared one is not on the wire, and neither is the floor an unreached
// peer reports. Absent means "not declared", which a client must never read as a
// format of its own.
func TestDriverCapabilities_RecordFormatIsAbsentUnlessDeclared(t *testing.T) {
	undeclared := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved}
	for name, c := range map[string]DriverCapabilities{"observed": undeclared, "assumed floor": undeclared.Assumed()} {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "recordFormat") {
			t.Errorf("%s: an undeclared recordFormat must not be serialised: %s", name, b)
		}
	}

	declared := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved,
		ReadsRecord: Declared(true), RecordFormat: DeclaredInt(RecordFormat)}
	b, err := json.Marshal(declared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"readsRecord":true`) || !strings.Contains(string(b), `"recordFormat":1`) {
		t.Fatalf("wire shape lacks readsRecord/recordFormat: %s", b)
	}
	var out DriverCapabilities
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.RecordFormat == nil || *out.RecordFormat != RecordFormat {
		t.Fatalf("a declared recordFormat did not survive: %+v", out.RecordFormat)
	}
}

// A peer built before the field sends neither readsRecord nor recordFormat. Both
// must read as not declared, under the peer's own `observed` — never as the false
// or the 0 a plain value would have decoded to.
func TestDriverCapabilities_PeerBeforeRecordFormatReadsNotDeclared(t *testing.T) {
	var c DriverCapabilities
	older := `{"observesState":true,"deliversRawKeys":false,"supportsPin":{"model":true,"effort":false,"agent":true},` +
		`"readsTurns":true,"deadlineMs":3000,"source":"observed"}`
	if err := json.Unmarshal([]byte(older), &c); err != nil {
		t.Fatal(err)
	}
	if c.Source != CapabilitiesObserved {
		t.Fatalf("source = %q", c.Source)
	}
	if c.ReadsRecord != nil || c.RecordFormat != nil {
		t.Fatalf("a peer before the field must read as not declared, got readsRecord=%v recordFormat=%v", c.ReadsRecord, c.RecordFormat)
	}
}
