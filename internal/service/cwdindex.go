package service

import (
	"context"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// The cwd index: which directory each known session runs in.
//
// # Why the event plane needs it
//
// A subscriber that asks for `cwdPrefix=<dir>` wants every frame about the
// sessions under that directory. Only one frame says where a session runs —
// session.created carries the whole Session. session.state, session.closed and
// session.labels name the session and nothing else, so the prefix cannot be
// applied to them from the frame alone. Applied to an empty cwd, the filter
// fails, and the subscriber saw `created` and then nothing: it never learned
// that its session went working, idle or closed. The same blindness made
// peers' frames, which arrive as generic JSON maps rather than typed payloads,
// pass the filter unexamined, so a subscriber also received a peer's entire
// fleet (#361).
//
// The hub therefore remembers, per (machine, id), the directory it has seen
// that session in, and resolves every event against it once, when the event is
// published.
//
// # Where it is learned
//
//   - session.created — the frame carries the Session.
//   - every session listing this service performs, filtered or not: a listing
//     is a sighting of (machine, id, cwd) whatever it was asked.
//   - session.renamed — the id changes, the directory does not.
//   - on demand — a frame for a session the index has never seen (one that
//     existed before this service started watching, whose created frame was
//     never in the stream) makes the service ask the source for it, once.
//
// # Where it is forgotten
//
// On session.closed, after that frame has been matched. Ids are recyclable
// (§5.4); a recycled id arrives with its own session.created, which replaces
// the entry. An entry for a session that vanished without a close frame (a
// feed gap) lingers until its id is seen again — bounded by the number of
// sessions, which is why this is a map and not a cache with an eviction policy.

// sessionKey identifies a session across machines: an id is only meaningful
// per machine (§5.4).
type sessionKey struct {
	machine fleet.MachineId
	id      string
}

// eventFact is what one event was about, as resolved when it was published.
type eventFact struct {
	id       string // "" when the event is not about one session
	cwd      string
	cwdKnown bool
}

// cwdRetry is how long a failed on-demand lookup is remembered, so a busy
// session with an unresolvable directory costs one listing per window rather
// than one per frame.
const cwdRetry = 30 * time.Second

func (h *hub) keyFor(machine fleet.MachineId, id string) sessionKey {
	if machine == "" {
		machine = h.self
	}
	return sessionKey{machine: machine, id: id}
}

// factFor resolves an event about to be retained and fanned out. Called with
// h.mu held. A frame that carries a directory teaches the index; one that does
// not is answered from it.
func (h *hub) factFor(ev fleet.Event) eventFact {
	id, cwd, has := eventSubject(ev)
	if id == "" {
		h.noteRename(ev)
		return eventFact{}
	}
	key := h.keyFor(ev.Machine, id)
	if has {
		h.cwds[key] = cwd
		delete(h.cwdTried, key)
		return eventFact{id: id, cwd: cwd, cwdKnown: true}
	}
	if known, ok := h.cwds[key]; ok {
		return eventFact{id: id, cwd: known, cwdKnown: true}
	}
	return eventFact{id: id}
}

// forget drops a closed session from the index. Called with h.mu held, after
// the close frame was fanned out.
func (h *hub) forget(ev fleet.Event) {
	if ev.Kind != fleet.EventSessionClosed {
		return
	}
	if id, _, _ := eventSubject(ev); id != "" {
		key := h.keyFor(ev.Machine, id)
		delete(h.cwds, key)
		delete(h.cwdTried, key)
	}
}

// noteRename carries a session's directory across a rename. Called with h.mu
// held, on a frame that is about no single session — which a rename is.
//
// Both directions are copied, and nothing is deleted here: a rename that is
// later contested (#103) puts the old id back, and the directory was the same
// under either name throughout. The retired id's entry goes when its own close
// frame arrives, or is overwritten if the id is recycled.
func (h *hub) noteRename(ev fleet.Event) {
	if ev.Kind != fleet.EventSessionRenamed {
		return
	}
	var from, to string
	switch p := ev.Payload.(type) {
	case fleet.SessionRenamed:
		from, to = p.From, p.To
	case map[string]any:
		from, _ = p["from"].(string)
		to, _ = p["to"].(string)
	}
	if from == "" || to == "" {
		return
	}
	a, b := h.keyFor(ev.Machine, from), h.keyFor(ev.Machine, to)
	if cwd, ok := h.cwds[a]; ok {
		if _, has := h.cwds[b]; !has {
			h.cwds[b] = cwd
		}
	} else if cwd, ok := h.cwds[b]; ok {
		h.cwds[a] = cwd
	}
}

// observeSessions records the directories a listing reported. The machine is
// the source that answered, used for any item that does not name its own.
func (h *hub) observeSessions(machine fleet.MachineId, sessions []fleet.Session) {
	if len(sessions) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sess := range sessions {
		m := sess.Machine
		if m == "" {
			m = machine
		}
		key := h.keyFor(m, sess.ID)
		h.cwds[key] = string(sess.Cwd)
		delete(h.cwdTried, key)
	}
}

// wantsCwd reports whether this event is worth a lookup: somebody is filtering
// by directory, the event is about one session without saying where, and the
// index does not know. It also claims the lookup, so a burst of frames for the
// same session triggers one listing and not one each; observeSessions releases
// the claim when the lookup succeeds, and cwdRetry when it does not.
func (h *hub) wantsCwd(ev fleet.Event) (sessionKey, bool) {
	id, _, has := eventSubject(ev)
	if id == "" || has {
		return sessionKey{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := h.keyFor(ev.Machine, id)
	if _, ok := h.cwds[key]; ok {
		return sessionKey{}, false
	}
	if at, ok := h.cwdTried[key]; ok && time.Since(at) < cwdRetry {
		return sessionKey{}, false
	}
	for _, s := range h.subs {
		if s.filter.CwdPrefix != "" {
			h.cwdTried[key] = time.Now()
			return key, true
		}
	}
	return sessionKey{}, false
}

// resolveCwd asks the source an event came from where its session runs, when a
// cwdPrefix subscriber is waiting on the answer and nothing else has said.
//
// It runs in the pump, before the event is published and outside every hub
// lock: a listing is a subprocess or an HTTP call, and the hub's lock is held
// across a fan-out to every subscriber. The cost lands on the one stream that
// needs it, and only for a session this service has never seen: once per
// session, never per frame (wantsCwd).
//
// A local driver narrows to the one id, which is cheap by design (#339). The
// remote driver does not forward the id, so the answer for a peer is its whole
// listing; observeSessions takes all of it, and the peer's other sessions are
// placed by the same call.
func (s *Service) resolveCwd(ctx context.Context, machine fleet.MachineId, d driver.Driver, req fleet.Request, ev fleet.Event) {
	if _, ok := s.events.wantsCwd(ev); !ok {
		return
	}
	id, _, _ := eventSubject(ev)
	// callList records what it finds (observeSessions), so there is nothing to
	// do with the result here.
	s.callList(ctx, req, machine, d, driver.ListFilter{ID: id}, 0)
}
