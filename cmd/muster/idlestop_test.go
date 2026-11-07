package main

import (
	"strings"
	"testing"
	"time"
)

// muster #421: FLEET_OPENCODE_IDLE_STOP is off unless set, takes a duration of at
// least five minutes, and is never guessed at.
func TestParseIdleStop(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		noteHas string
	}{
		{"", 0, ""},
		{"  ", 0, ""},
		{"30m", 30 * time.Minute, "30m0s"},
		{"2h", 2 * time.Hour, "2h0m0s"},
		{"5m", 5 * time.Minute, "5m0s"},
		{" 45m ", 45 * time.Minute, "45m0s"},
		{"0", 0, "off"},
		{"off", 0, "off"},
		{"OFF", 0, "off"},
		{"0s", 0, "off"},
		{"4m", 0, "at least 5m0s"},
		{"90s", 0, "at least 5m0s"},
		{"-5m", 0, "ignoring"},
		{"soon", 0, "ignoring"},
		{"30", 0, "ignoring"}, // a bare number has no unit; do not guess minutes or seconds
	} {
		d, note := parseIdleStop(tc.raw)
		if d != tc.want {
			t.Errorf("parseIdleStop(%q) = %v, want %v", tc.raw, d, tc.want)
		}
		if !strings.Contains(note, tc.noteHas) {
			t.Errorf("parseIdleStop(%q) note = %q, want it to contain %q", tc.raw, note, tc.noteHas)
		}
		if tc.noteHas == "" && note != "" {
			t.Errorf("parseIdleStop(%q) note = %q, want none", tc.raw, note)
		}
	}
}
