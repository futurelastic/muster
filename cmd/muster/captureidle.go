package main

import (
	"fmt"
	"strings"
	"time"
)

// What an idle pane's screen is reused for, and how long (muster #449). The driver
// reads a pane's screen again only when the multiplexer reports output, a resize or
// a cursor move on it; these two bounds say when it may stop reading one at all and
// the longest it goes without reading one anyway. See internal/drivers/tmux
// capturereuse.go for why the first is not a back-off ladder.
const (
	// defaultCaptureIdleAfter is how long a pane must have produced no output
	// before a listing may serve its screen from the last capture. Short enough
	// that a fleet where most sessions are idle for hours stops paying for them,
	// long enough that a session being worked with is read every pass as before.
	defaultCaptureIdleAfter = 2 * time.Minute
	// defaultCaptureIdleMax is the ceiling on how old a reused screen may be before
	// it is read again whatever the multiplexer reports. It bounds the cost of a
	// multiplexer that fails to report output: such a pane is at most this stale.
	defaultCaptureIdleMax = 30 * time.Second
)

// parseCaptureReuse reads FLEET_CAPTURE_IDLE_AFTER and FLEET_CAPTURE_IDLE_MAX.
//
// Returns the two durations and whether reuse is on. Both absent or empty is the
// default (on, 2m and 30s). A Go duration sets that bound; `0` or `off` on EITHER
// turns reuse off, since a pane that must be quiet for no time, or reused for no
// time, is one that is always read. Anything else — a typo, a negative, a bare
// number with no unit — is not guessed at: it is reported in notes and the default
// for that bound stands, so a typo never silently turns the saving off or sets a
// bound nobody wrote.
func parseCaptureReuse(afterRaw, maxRaw string) (after, ceiling time.Duration, on bool, notes []string) {
	after, afterOff, note := parseCaptureBound("FLEET_CAPTURE_IDLE_AFTER", afterRaw, defaultCaptureIdleAfter)
	if note != "" {
		notes = append(notes, note)
	}
	ceiling, maxOff, note := parseCaptureBound("FLEET_CAPTURE_IDLE_MAX", maxRaw, defaultCaptureIdleMax)
	if note != "" {
		notes = append(notes, note)
	}
	if afterOff || maxOff {
		return 0, 0, false, append(notes, "idle pane captures are read every pass (reuse off)")
	}
	return after, ceiling, true, notes
}

// parseCaptureBound is one bound: its value, whether it asked for reuse off, and
// what to log. def is returned for an absent or unusable value.
func parseCaptureBound(name, raw string, def time.Duration) (d time.Duration, off bool, note string) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "":
		return def, false, ""
	case "off", "0":
		return 0, true, fmt.Sprintf("%s=%s turns idle capture reuse off", name, raw)
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v < 0 {
		return def, false, fmt.Sprintf("ignoring %s=%q (want a duration like 5m, or 0/off); using %s", name, raw, def)
	}
	if v == 0 {
		return 0, true, fmt.Sprintf("%s=%s turns idle capture reuse off", name, raw)
	}
	return v, false, ""
}
