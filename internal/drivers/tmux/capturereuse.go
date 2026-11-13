package tmux

import (
	"hash/fnv"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reusing the capture of a pane that has produced no output (muster #449).
//
// A listing used to read every pane's screen on every pass. On a machine holding
// about seventy sessions, most of them idle for hours and some for days, that was
// the largest share of what the service cost: one invocation carrying a capture
// per pane, and a classification of each, so a pass paid for the whole fleet
// whatever the fleet was doing. The ask was a back-off ladder — read an idle
// session less often the longer it stays quiet. This is what replaces the
// ladder, and why.
//
// # The change signal
//
// A ladder has to guess when a quiet pane woke up, so it trades cost against the
// time an attended session takes to be noticed. The multiplexer already says when
// a pane produced output, and says it in the listing every pass makes anyway: the
// window's last-activity second, plus the pane's size, cursor and scrollback
// depth. When none of those moved since the capture was taken, the screen the
// capture holds is the screen now, and reading it again would return the same
// bytes. So a pane is read when its fingerprint changed, and not otherwise: an
// idle session costs nothing between two passes, and one that wakes is read on
// the very next pass — as fast as before, which no ladder step can promise.
//
// The activity second has one-second granularity, so a capture taken in the same
// second as the last output cannot prove that nothing came after it. A cached
// capture is therefore trusted only when the pane's last output was strictly
// earlier than the second the listing was taken in (reuseSafe). Any output after
// that listing has a later second, so it changes the fingerprint.
//
// # What stays as it was
//
// Only Driver.List reads through this. Every verb that acts on a screen — a send,
// a response, a discard, a key — still captures fresh, because it is about to
// change something on the strength of what it saw. And the listing stays one pass
// over the whole fleet: the same rows, the same classification, the same
// observation memory, with the screen text coming from a cache for the panes that
// have not moved. That is why this does not reopen the "re-read only some panes"
// path the stream's pacing comment rules out: there is no second path through the
// classifier to drift, only a second source for one of its inputs.
//
// # What bounds the harm if the signal is wrong
//
//   - a ceiling on how old a reused capture may be (maxAge), spread per pane so a
//     fleet cached in one pass is not read again in one pass;
//   - a self-check: when a read forced by that ceiling finds the screen different
//     although the fingerprint said nothing happened, the signal may have missed
//     output. The read is taken AFTER the listing that fingerprinted the pane, so
//     output between the two also makes the screen differ; only the NEXT listing
//     can tell the two apart, and the verdict waits for it (muster #472, below);
//   - no reuse for a pane that is dead, still starting, or listed without the
//     fields (a multiplexer that does not report them), and none after any input
//     this driver sent to it.
//
// # A suspected miss is judged by the next listing, and costs one pane
//
// The first version of the self-check judged on the spot and switched reuse off
// for the life of the process. A listing and the read that follows it are two
// moments, and on a loaded host they are seconds apart: output arriving between
// them changes the screen under a fingerprint that has not moved yet, and that
// was taken for a missed signal. It fired three times in two and a half days on
// one host, each time latching every pane back to being read on every pass, and
// the next burst of load then ran the capture budget out and made listings
// incomplete (muster #472).
//
// So a differing read is only a SUSPECT. The next listing settles it: if the
// pane's fingerprint has moved, the output is explained and nothing was missed
// (counted as a race, capture.reuse_raced); if it still equals the cached one,
// nothing explains the change and the signal missed it. Then the pane that was
// shown to miss is read on every pass from now on, and the rest keep their
// saving. Only reuseDistrustLimit panes shown to miss switch it off for the
// host, because several distinct panes disagreeing is what a multiplexer that
// does not report output looks like.
//
// Off unless the composition root asks (WithCaptureReuse): a driver built for a
// test reads every pane every time, as it always did.

// captureReuseFormat is the listing fields the fingerprint is made of, appended to
// the pane listing after the fields parseRows has always read. Six, in this order:
// the window's last activity (epoch seconds), then the pane's width, height, cursor
// column, cursor row and scrollback depth.
var captureReuseFormat = []string{
	"#{window_activity}", "#{pane_width}", "#{pane_height}",
	"#{cursor_x}", "#{cursor_y}", "#{history_size}",
}

// captureReuseFields is how many of those fields a listing row carries.
const captureReuseFields = 6

// reuseFingerprint reads the fingerprint fields of one listing row. activity is 0
// and sig empty when the multiplexer did not report them — the signal is then
// unavailable for the pane, never assumed.
func reuseFingerprint(f []string) (activity int64, sig string) {
	if len(f) != captureReuseFields {
		return 0, ""
	}
	for _, v := range f {
		if strings.TrimSpace(v) == "" || strings.HasPrefix(v, "#{") {
			return 0, ""
		}
	}
	a, err := strconv.ParseInt(strings.TrimSpace(f[0]), 10, 64)
	if err != nil || a <= 0 {
		return 0, ""
	}
	return a, strings.Join(f, "|")
}

// cachedCapture is the last screen read for a pane, with what proves it still is.
type cachedCapture struct {
	pane    paneCapture
	sig     string
	created time.Time
	pid     int
	// listedAt is when the listing that fingerprinted this capture was taken, on the
	// driver's clock. The capture itself came after it.
	listedAt time.Time
	// safe is whether the fingerprint can vouch for the capture at all: the pane's
	// last output was strictly before the second of listedAt (see the file comment).
	safe bool
}

// captureReuse is the cache and its two bounds. The zero value is off.
type captureReuse struct {
	idleAfter time.Duration
	maxAge    time.Duration

	mu      sync.Mutex
	entries map[string]cachedCapture
	off     bool // latched when reuseDistrustLimit panes were shown to miss output

	// suspects are the panes whose last forced read showed a screen different from
	// the cached one under an unchanged fingerprint, keyed to that fingerprint.
	// The next listing resolves each one (split).
	suspects map[string]string
	// distrusted are the panes whose fingerprint was shown to miss output: they
	// are read on every pass for as long as the pane lives.
	distrusted map[string]bool
}

// reuseDistrustLimit is how many distinct panes must be shown to change under an
// unmoved fingerprint before the signal is judged unreliable on the whole host
// and reuse is switched off until restart.
const reuseDistrustLimit = 3

func (c *captureReuse) enabled() bool {
	if c == nil || c.idleAfter <= 0 || c.maxAge <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.off
}

// ceilingFor is how old a reused capture of this pane may get: maxAge, less a share
// of up to half of it chosen by the pane id. The same pane always gets the same
// bound; different panes get different ones, so the captures taken together do not
// all expire together. It never exceeds maxAge, which is what the operator set.
func (c *captureReuse) ceilingFor(paneID string) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(paneID))
	frac := time.Duration(h.Sum32()%1000) * c.maxAge / 2000
	return c.maxAge - frac
}

// reuseDecision is one pass's split of the listed rows.
type reuseDecision struct {
	// capture are the rows to read, in listing order.
	capture []paneRow
	// reused are the screens served from the cache, by pane id.
	reused map[string]paneCapture
	// verify are the entries whose age alone forced a read although the fingerprint
	// held: the read is compared against them (the self-check).
	verify map[string]cachedCapture
	// confirmed are the panes this listing showed to have changed under a
	// fingerprint that did not move, now distrusted; raced is how many suspects the
	// listing cleared because the fingerprint had moved. switchedOff is set when
	// the confirmations reached reuseDistrustLimit and reuse is now off.
	confirmed   []string
	raced       int
	switchedOff bool
}

// split decides, for every listed row, whether its screen is read or reused. now is
// the time the listing was taken, not the time of this call.
func (c *captureReuse) split(rows []paneRow, now time.Time) reuseDecision {
	dec := reuseDecision{reused: map[string]paneCapture{}, verify: map[string]cachedCapture{}}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolveSuspects(rows, &dec)
	for _, r := range rows {
		e, have := c.entries[r.paneID]
		switch {
		case c.distrusted[r.paneID],
			!have, r.sig == "", !e.safe, e.sig != r.sig,
			!e.created.Equal(r.created), e.pid != r.pid,
			r.dead, now.Sub(r.created) < startingWindow,
			now.Sub(time.Unix(r.activity, 0)) < c.idleAfter:
			dec.capture = append(dec.capture, r)
		case now.Sub(e.listedAt) >= c.ceilingFor(r.paneID):
			dec.capture = append(dec.capture, r)
			dec.verify[r.paneID] = e
		default:
			dec.reused[r.paneID] = e.pane
		}
	}
	return dec
}

// remember stores what a pass read, drops what it could not, and forgets panes the
// listing no longer has. captured is the whole answer for the pass (read and reused
// together); read is the rows this pass actually read.
func (c *captureReuse) remember(rows, read []paneRow, captured map[string]paneCapture, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cachedCapture{}
	}
	live := make(map[string]bool, len(rows))
	for _, r := range rows {
		live[r.paneID] = true
	}
	for id := range c.entries {
		if !live[id] {
			delete(c.entries, id)
		}
	}
	for id := range c.distrusted {
		if !live[id] {
			delete(c.distrusted, id)
		}
	}
	for id := range c.suspects {
		if !live[id] {
			delete(c.suspects, id)
		}
	}
	for _, r := range read {
		cap, ok := captured[r.paneID]
		if !ok || r.sig == "" {
			// Nothing was read, or nothing can vouch for it: do not keep a screen
			// that a later pass could mistake for current.
			delete(c.entries, r.paneID)
			continue
		}
		c.entries[r.paneID] = cachedCapture{
			pane: cap, sig: r.sig, created: r.created, pid: r.pid,
			listedAt: now,
			safe:     r.activity > 0 && r.activity < now.Unix(),
		}
	}
}

// forget drops the cached screen of a pane after input was sent to it. A target
// that is not a pane id (a session name) drops everything: it cannot be matched,
// and reading once more is cheap against reusing a screen the input changed.
func (c *captureReuse) forget(target string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.HasPrefix(target, "%") {
		delete(c.entries, target)
		return
	}
	for id := range c.entries {
		delete(c.entries, id)
	}
}

// noteInput forgets the pane an input command is aimed at. It reads the argv of one
// multiplexer invocation, which may chain commands with ";" (a load-buffer then a
// paste-buffer), and acts on the -t of every send-keys and paste-buffer in it.
func (c *captureReuse) noteInput(args []string) {
	if c == nil {
		return
	}
	cmd := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case i == 0 || args[i-1] == ";":
			cmd = a
		case a == "-t" && i+1 < len(args) && (cmd == "send-keys" || cmd == "paste-buffer"):
			c.forget(args[i+1])
		}
	}
}

// resolveSuspects settles the suspects the previous pass recorded, against the
// fingerprints of this listing. Called with c.mu held. A pane whose fingerprint
// moved had output the previous pass could not have seen (it raced the listing);
// one whose fingerprint is still the cached one changed with nothing to explain
// it, so its signal is not trusted any more. A suspect with no signal this time,
// or whose pane is gone, is dropped unjudged.
func (c *captureReuse) resolveSuspects(rows []paneRow, dec *reuseDecision) {
	if len(c.suspects) == 0 {
		return
	}
	for _, r := range rows {
		sig, ok := c.suspects[r.paneID]
		if !ok || r.sig == "" {
			continue
		}
		if r.sig != sig {
			dec.raced++
			continue
		}
		if c.distrusted == nil {
			c.distrusted = map[string]bool{}
		}
		if !c.distrusted[r.paneID] {
			c.distrusted[r.paneID] = true
			dec.confirmed = append(dec.confirmed, r.paneID)
		}
	}
	c.suspects = nil
	if len(c.distrusted) >= reuseDistrustLimit && !c.off {
		c.off = true
		c.entries = nil
		dec.switchedOff = true
	}
}

// suspectReuseMisses is the self-check's first half. It compares the screens a
// pass read against the entries whose read was forced by age alone, and records a
// suspect for every one that differs; it judges nothing, because the read came
// after the listing that fingerprinted the pane (see the file comment).
func (d *Driver) suspectReuseMisses(verify map[string]cachedCapture, captured map[string]paneCapture) {
	c := d.reuse
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range verify {
		if got, ok := captured[id]; ok && got.text != e.pane.text {
			if c.suspects == nil {
				c.suspects = map[string]string{}
			}
			c.suspects[id] = e.sig
		}
	}
}

// noteReuseVerdicts counts and reports what a listing's resolution of last
// pass's suspects decided: a race is only counted, a pane shown to miss is logged
// by name (once, since it is distrusted from then on), and the host-wide
// switch-off is logged once.
func (d *Driver) noteReuseVerdicts(dec reuseDecision) {
	if dec.raced > 0 {
		d.counters.add(counterCaptureReuseRaced, int64(dec.raced))
	}
	if len(dec.confirmed) == 0 {
		return
	}
	d.counters.add(counterCaptureReuseMissed, int64(len(dec.confirmed)))
	for _, id := range dec.confirmed {
		log.Printf("tmux: pane %s changed although its activity fingerprint did not move across two "+
			"listings; this pane is read on every pass from now on (#472)", id)
	}
	if dec.switchedOff {
		log.Printf("tmux: %d panes changed under an activity fingerprint that did not move; this "+
			"multiplexer's activity signal cannot be trusted here, so every pane is read on every "+
			"pass again until the service restarts (#449)", reuseDistrustLimit)
	}
}

// WithCaptureReuse lets List serve a pane's screen from its last capture while the
// pane's activity fingerprint has not moved. idleAfter is how long a pane must have
// been quiet before that starts, and maxAge is the longest a captured screen is
// reused before it is read again; either at zero or below leaves the driver reading
// every pane every pass (the default). See the file comment for why this is safe and
// what bounds it when it is not.
func WithCaptureReuse(idleAfter, maxAge time.Duration) Option {
	return func(d *Driver) {
		if idleAfter > 0 && maxAge > 0 {
			d.reuse = &captureReuse{idleAfter: idleAfter, maxAge: maxAge}
		}
	}
}
