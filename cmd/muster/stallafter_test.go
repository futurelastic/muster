package main

import (
	"strings"
	"testing"
	"time"
)

// muster #391: FLEET_STALL_AFTER overrides the driver's default bound, turns
// the judgement off, and is never guessed at.
func TestParseStallAfter(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		set     bool
		noteHas string
	}{
		{"", 0, false, ""},
		{"  ", 0, false, ""},
		{"45m", 45 * time.Minute, true, "45m0s"},
		{"2h", 2 * time.Hour, true, "2h0m0s"},
		{"0", 0, true, "off"},
		{"off", 0, true, "off"},
		{"OFF", 0, true, "off"},
		{"0s", 0, true, "off"},
		{"soon", 0, false, "ignoring"},
		{"-5m", 0, false, "ignoring"},
		{"30", 0, false, "ignoring"}, // a bare number has no unit; do not guess seconds
	} {
		d, set, note := parseStallAfter(tc.raw)
		if d != tc.want || set != tc.set {
			t.Errorf("parseStallAfter(%q) = (%v, %v), want (%v, %v)", tc.raw, d, set, tc.want, tc.set)
		}
		if !strings.Contains(note, tc.noteHas) {
			t.Errorf("parseStallAfter(%q) note = %q, want it to contain %q", tc.raw, note, tc.noteHas)
		}
		if tc.noteHas == "" && note != "" {
			t.Errorf("parseStallAfter(%q) note = %q, want none", tc.raw, note)
		}
	}
}
