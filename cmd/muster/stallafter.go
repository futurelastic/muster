package main

import (
	"fmt"
	"strings"
	"time"
)

// parseStallAfter reads FLEET_STALL_AFTER (muster #391): how long a session
// that reads `working` may show the identical screen before it reads stalled.
//
// Returns the duration to configure and whether to call the option at all.
// Absent or empty configures nothing, so the driver's built-in default stands.
// A Go duration (`45m`, `2h`) sets the bound; `0` and `off` turn the judgement
// off. Anything else, and a negative duration, is not guessed at: it is
// reported in note and ignored, so a typo leaves the default in force rather
// than silently disabling the check or setting a bound nobody wrote.
func parseStallAfter(raw string) (d time.Duration, set bool, note string) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "":
		return 0, false, ""
	case "off", "0":
		return 0, true, "stall judgement off (FLEET_STALL_AFTER)"
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v < 0 {
		return 0, false, fmt.Sprintf("ignoring FLEET_STALL_AFTER=%q (want a duration like 30m, or 0/off)", raw)
	}
	if v == 0 {
		return 0, true, "stall judgement off (FLEET_STALL_AFTER)"
	}
	return v, true, fmt.Sprintf("stall bound set to %s (#391)", v)
}
