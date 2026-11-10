package main

import (
	"strings"
	"testing"
	"time"
)

// muster #449: the two bounds default on, can each turn reuse off, and a value that
// cannot be read never turns it off or sets a bound nobody wrote.
func TestParseCaptureReuse(t *testing.T) {
	for _, tc := range []struct {
		name           string
		after, ceiling string
		wantAfter      time.Duration
		wantMax        time.Duration
		wantOn         bool
		noteContains   []string
	}{
		{"both absent is the default, on", "", "", 2 * time.Minute, 30 * time.Second, true, nil},
		{"blank is absent", "  ", " ", 2 * time.Minute, 30 * time.Second, true, nil},
		{"both set", "10m", "1m", 10 * time.Minute, time.Minute, true, nil},
		{"only the idle bound set", "5m", "", 5 * time.Minute, 30 * time.Second, true, nil},
		{"only the ceiling set", "", "45s", 2 * time.Minute, 45 * time.Second, true, nil},
		{"zero idle bound is off", "0", "", 0, 0, false, []string{"FLEET_CAPTURE_IDLE_AFTER=0", "reuse off"}},
		{"off idle bound is off", "off", "", 0, 0, false, []string{"FLEET_CAPTURE_IDLE_AFTER=off"}},
		{"OFF is case-blind", "", "OFF", 0, 0, false, []string{"FLEET_CAPTURE_IDLE_MAX=OFF"}},
		{"zero ceiling is off", "", "0s", 0, 0, false, []string{"FLEET_CAPTURE_IDLE_MAX=0s"}},
		{"a typo keeps the default", "soon", "", 2 * time.Minute, 30 * time.Second, true, []string{"ignoring FLEET_CAPTURE_IDLE_AFTER"}},
		{"a negative keeps the default", "", "-5s", 2 * time.Minute, 30 * time.Second, true, []string{"ignoring FLEET_CAPTURE_IDLE_MAX"}},
		{"a bare number has no unit and is not guessed", "30", "", 2 * time.Minute, 30 * time.Second, true, []string{"ignoring FLEET_CAPTURE_IDLE_AFTER"}},
		{"a typo beside a valid value keeps both", "5m", "later", 5 * time.Minute, 30 * time.Second, true, []string{"ignoring FLEET_CAPTURE_IDLE_MAX"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after, ceiling, on, notes := parseCaptureReuse(tc.after, tc.ceiling)
			if after != tc.wantAfter || ceiling != tc.wantMax || on != tc.wantOn {
				t.Errorf("parseCaptureReuse(%q, %q) = (%v, %v, %v), want (%v, %v, %v)",
					tc.after, tc.ceiling, after, ceiling, on, tc.wantAfter, tc.wantMax, tc.wantOn)
			}
			joined := strings.Join(notes, "\n")
			for _, want := range tc.noteContains {
				if !strings.Contains(joined, want) {
					t.Errorf("notes %q lack %q", joined, want)
				}
			}
			if len(tc.noteContains) == 0 && len(notes) != 0 {
				t.Errorf("notes = %q, want none", joined)
			}
		})
	}
}
