package sandbox

import "time"

// How long a driver trusts a measurement of "can this runtime be confined here"
// (muster #345, #456). Both drivers that measure one — the multiplexer's, which
// asks the agent CLI for its account, and the one that owns its runtime's server,
// which asks the runtime for its version — read these, so a host the two disagree
// about is never one they merely cache differently.
//
// A measurement that found the runtime working is trusted for a long time. One
// that did not is trusted briefly, because the usual cause (a login just made, a
// runtime just installed or moved) is gone in minutes. In the background a
// failure that repeats waits twice as long each time, up to MeasuredFailMaxTTL: a
// host that cannot be confined used to be measured every half minute for ever,
// each time starting the runtime once more. A caller about to act on the answer (a
// create, which waits for it) is not made to sit out that backoff.
const (
	MeasuredFoundTTL   = 5 * time.Minute
	MeasuredFailTTL    = 30 * time.Second
	MeasuredFailMaxTTL = 5 * time.Minute
)

// MeasuredTTL is how long an answer is trusted. found says the runtime started
// under the policy; strict is for a caller that waits for the answer to act on it;
// failures counts the measurements in a row that found nothing.
func MeasuredTTL(found, strict bool, failures int) time.Duration {
	if found {
		return MeasuredFoundTTL
	}
	if strict {
		return MeasuredFailTTL
	}
	d := MeasuredFailTTL
	for i := 1; i < failures && d < MeasuredFailMaxTTL; i++ {
		d *= 2
	}
	if d > MeasuredFailMaxTTL {
		d = MeasuredFailMaxTTL
	}
	return d
}
