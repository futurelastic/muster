package tmux

import (
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
)

// recordCache remembers, per session, what the last listing read out of that
// session's runtime record, so a listing may skip rereading a record that has
// no reason to have changed (muster #339).
//
// Why a screen digest is the key. The event stream lists the whole fleet after
// every output burst, and one busy session drove a full read of every idle
// session's record several times a second. The stream cannot tell which panes
// fired — notifications are triggers, content clients are capped, and a session
// with no client still changes — but a session whose visible screen is
// byte-identical to the last listing's has not been working, and a runtime that
// has not been working has not appended to its record. That is evidence, not a
// proof: a record can grow behind a static screen. So reuse is only ever taken
// when the caller named a freshness bound (ListFilter.RecordMaxAge), and the
// bound is measured from the last REAL read, never extended by a reuse.
type recordCache struct {
	mu   sync.Mutex
	rows map[string]recordRead
}

// recordRead is one session's record-derived fields as of a real read.
type recordRead struct {
	created time.Time // the pane's creation time: a recycled id is not the same session
	digest  string
	at      time.Time

	conversation *fleet.ConversationRef
	resume       *fleet.ResumeOutcome
	turns        *int
	usage        *fleet.Usage
}

// get returns the remembered read for a session when it may stand in for a new
// one: same incarnation, same non-empty screen, read less than maxAge ago.
func (c *recordCache) get(id string, created time.Time, digest string, now time.Time, maxAge time.Duration) (recordRead, bool) {
	if maxAge <= 0 || digest == "" {
		return recordRead{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rows[id]
	if !ok || !r.created.Equal(created) || r.digest != digest || now.Sub(r.at) >= maxAge || now.Before(r.at) {
		return recordRead{}, false
	}
	return r, true
}

// put replaces the remembered read for a session.
func (c *recordCache) put(id string, r recordRead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = map[string]recordRead{}
	}
	c.rows[id] = r
}

// keep drops every session not in live, so the cache cannot outgrow the fleet.
func (c *recordCache) keep(live map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.rows {
		if !live[id] {
			delete(c.rows, id)
		}
	}
}

// rememberRecords stores what this listing really read, for the sessions it
// really read it for, and forgets sessions that no longer exist. A session
// served from the cache keeps its original read time: a reuse never renews
// the freshness bound it was granted under.
func (d *Driver) rememberRecords(rows []paneRow, sessions []fleet.Session, reused map[string]bool, now time.Time) {
	created := make(map[string]time.Time, len(rows))
	live := make(map[string]bool, len(rows))
	for _, r := range rows {
		created[r.session] = r.created
		live[r.session] = true
	}
	d.records.keep(live)
	for _, s := range sessions {
		if reused[s.ID] || s.State.ScreenDigest == "" {
			continue
		}
		rr := recordRead{created: created[s.ID], digest: s.State.ScreenDigest, at: now}
		if s.Conversation != nil {
			c := *s.Conversation
			rr.conversation = &c
		}
		if s.ResumeOutcome != nil {
			o := *s.ResumeOutcome
			rr.resume = &o
		}
		if s.State.Turns != nil {
			t := *s.State.Turns
			rr.turns = &t
		}
		if s.State.Usage != nil {
			u := *s.State.Usage
			rr.usage = &u
		}
		d.records.put(s.ID, rr)
	}
}
