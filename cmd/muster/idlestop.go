package main

import (
	"fmt"
	"strings"
	"time"
)

// minIdleStop is the shortest bound FLEET_OPENCODE_IDLE_STOP accepts. A server
// stopped after less would be started again by the next poll or the next message of
// an ordinary conversation, each time paying a process start (seconds) for a few
// minutes' memory; the knob exists for sessions that stay idle for long.
const minIdleStop = 5 * time.Minute

// parseIdleStop reads FLEET_OPENCODE_IDLE_STOP (muster #421): how long an opencode
// session's server may have nothing to do before it is stopped to free its memory,
// and started again by the next input.
//
// It returns the bound to configure, zero for none, and a note to log when there
// is something to say. Absent, empty, `0` and `off` configure nothing: this is off
// unless an operator asks for it. A Go duration of at least five minutes sets the
// bound. Anything else — a shorter bound, a negative one, a bare number, a word —
// is not guessed at: it is reported in note and ignored, so a typo leaves the
// sessions running rather than stopping them on a bound nobody wrote.
func parseIdleStop(raw string) (d time.Duration, note string) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "":
		return 0, ""
	case "off", "0":
		return 0, "opencode idle stop off (FLEET_OPENCODE_IDLE_STOP)"
	}
	v, err := time.ParseDuration(raw)
	switch {
	case err != nil || v < 0:
		return 0, fmt.Sprintf("ignoring FLEET_OPENCODE_IDLE_STOP=%q (want a duration like 30m, or 0/off)", raw)
	case v == 0:
		return 0, "opencode idle stop off (FLEET_OPENCODE_IDLE_STOP)"
	case v < minIdleStop:
		return 0, fmt.Sprintf("ignoring FLEET_OPENCODE_IDLE_STOP=%q (the bound is at least %s: a shorter one would stop "+
			"sessions that are about to be used again)", raw, minIdleStop)
	}
	return v, fmt.Sprintf("opencode idle stop after %s (#421)", v)
}
