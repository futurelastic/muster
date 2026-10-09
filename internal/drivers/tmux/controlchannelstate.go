package tmux

import (
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Resolving what a session's remote control IS, from the three kinds of
// evidence there are (muster #269). See controlchannel.go for the footer
// label, controlchannelrecord.go for the record, and ControlChannelOff for why
// `off` cannot be read off a label at all.
//
// # The order, and why it is this order
//
//  1. The footer label wins whenever it is there. It is the runtime's own
//     chrome and the only source that distinguishes connecting and
//     reconnecting.
//  2. Otherwise the newest remote-control entry in the runtime's own record
//     since this process launched decides between active and off, or, when
//     it is the runtime's own disconnection notice, reads `failed` once the
//     settle window has passed without a recovery (#270). Inside the window
//     it makes no claim.
//  3. Otherwise `off` is claimed from the LAUNCH only when every condition that
//     would make the absence of an entry mean something holds: this driver
//     started the session without the remote-control flag, its conversation is
//     known from the live process, and the record either does not exist yet or
//     was read all the way back to the launch with no enable in it.
//  4. Anything else leaves the field absent, and absent still means "not read".
//
// The launch evidence in (3) is the weakest and is deliberately last: a user's
// own settings can start remote control without any flag, in which case the
// runtime writes the enable entry and (2) takes over. Until it does, a session
// reads `off`; that window is a documented property of the claim, not a hidden
// one.

// controlNoticeSettle is how long a disconnection notice may stand as the
// newest channel entry before it reads as `failed`. Measured over a large set
// of runtime records (#270): a channel the runtime brought back by itself wrote
// a fresh enable entry within 116 seconds in every one of 77 cases; the
// longer gaps were somebody restoring it by hand. Five minutes sits well above
// the first and well below the second. The rule is deliberately about time and
// not about the notice's wording, which is the classification controlchannel.go
// declined to infer (#65) and which new wording would silently defeat.
const controlNoticeSettle = 5 * time.Minute

// launchRemoteControlOption is the tmux user option Create sets on every
// session it starts, recording whether the remote-control flag was actually
// put on the command line: "1" or "0". Absent (a session an older build
// started, or one this driver did not start) means the launch is unknown and
// supports no claim.
const launchRemoteControlOption = "@colab-launch-rc"

// resolveControlChannel applies the order above. footer is whatever the screen
// reader found (nil for none).
func (d *Driver) resolveControlChannel(footer *fleet.ControlChannel, conv *fleet.ConversationRef, row paneRow) *fleet.ControlChannel {
	if footer != nil {
		return footer
	}
	if d.conversations == nil || conv == nil || !conv.Known {
		return nil
	}
	read := d.controlRecordCached(d.conversations.recordPath(row.cwd, conv.ID), row.created)
	switch {
	case read.state != "":
		return &fleet.ControlChannel{State: read.state}
	case read.notice != nil:
		// The verdict depends on the clock, so it is made here and never stored
		// in the cached read. Inside the window the answer is nothing, not `off`
		// from the launch and not `reconnecting`, which no record entry carries.
		if d.now().Sub(read.notice.at) >= controlNoticeSettle {
			return &fleet.ControlChannel{State: fleet.ControlChannelFailed, Reason: read.notice.reasonText()}
		}
		return nil
	case read.blocked:
		return nil
	case row.managed && row.launchRC == "0" && (!read.exists || read.covered):
		return &fleet.ControlChannel{State: fleet.ControlChannelOff}
	}
	return nil
}

// controlRecordCache keeps one fold per record (muster #271): what it has
// consumed of the file and the newest remote-control entry found, so a quiet
// session costs a stat and a growing one costs only what was appended.
type controlRecordCache struct {
	mu sync.Mutex
	m  map[string]*controlRecordFold
}

func (d *Driver) controlRecordCached(path string, since time.Time) controlRecordRead {
	c := &d.controlRecords
	c.mu.Lock()
	f := c.m[path]
	if f == nil {
		if c.m == nil {
			c.m = map[string]*controlRecordFold{}
		}
		f = &controlRecordFold{}
		c.m[path] = f
	}
	c.mu.Unlock()
	// The read happens under the fold's own lock, not the map's: the first read
	// of a large record is a full one and must not stall every other session.
	return f.advance(path, since)
}

// latchSurfaceFromChannel is List's #85 latch for a channel read from the
// record rather than the footer. SurfaceSeen is "the runtime has corroborated
// its surface", and an `active` entry written by the runtime is exactly that;
// the footer-driven latch in the row loop never fires on a runtime that draws
// its label somewhere the footer reader does not look.
func (d *Driver) latchSurfaceFromChannel(s *fleet.Session, r paneRow) {
	ch := s.State.ControlChannel
	if ch == nil || ch.State != fleet.ControlChannelActive {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cr, ok := d.createRecordForLocked(r.session, r.cwd)
	if !ok || cr.SurfaceSeen {
		return
	}
	d.noteSurfaceSeenLocked(r.session)
	cr.SurfaceSeen = true
	s.RuntimeSurface = runtimeSurfaceFor(cr, r.session)
}
