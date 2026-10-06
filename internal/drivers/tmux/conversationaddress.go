package tmux

import (
	"context"
	"fmt"
	"sort"
	"strings"

	fleet "github.com/futurelastic/muster"
)

// A session-keyed route addressed with a live session's CONVERSATION id
// (muster #268).
//
// A session knows its own conversation id and does not naturally know this
// service's id for itself, so a reply-to address written into a brief tends to
// carry the former. Both can be UUID-shaped, so nothing about the address says
// which kind it is. The answer used to be "no session with this id" — which
// reads as "the session is gone", and the reply was dropped rather than
// retried.
//
// Ids stay strict: a conversation id is NOT accepted as an address (it is not a
// stable handle — a runtime can start a new conversation inside the same
// process, and two live sessions can hold one conversation after a resume). The
// refusal only changes what it SAYS: it names the session id(s) to use, and an
// id that matches nothing keeps the old wording.

// sessionsHoldingConversation returns the sorted ids of the LIVE sessions in
// rows whose resolved conversation is id. A dead row is skipped: the point is
// to name a session a retry can reach. Resolution is exactly List's — the
// record store lookup, then the create-intent overlay — so a caller is never
// told a session holds a conversation that its own listing does not show.
func (d *Driver) sessionsHoldingConversation(ctx context.Context, rows []paneRow, id string) []string {
	if d.conversations == nil || !uuidShaped(id) {
		return nil
	}
	var out []string
	for _, r := range rows {
		if r.dead {
			continue
		}
		conv := d.conversations.lookup(conversationKey{pane: r.paneID, created: r.created}, r.cwd, r.session, r.created,
			processGeneration{pid: r.pid}, d.liveConversationSource(ctx, r.pid, r.cwd))
		if requested, ok := d.conversationIntentFor(r.session, r.cwd); ok {
			conv = conversationIntentOutcome(requested, conv)
		}
		if conv != nil && conv.Known && strings.EqualFold(conv.ID, id) {
			out = append(out, r.session)
		}
	}
	sort.Strings(out)
	return out
}

// noSuchSession is the error every session-keyed read returns for an id no row
// carries: a ConversationIdError when the id is a live session's conversation,
// else the plain ErrNoSuchSession it always was.
func (d *Driver) noSuchSession(ctx context.Context, rows []paneRow, id string) error {
	if ids := d.sessionsHoldingConversation(ctx, rows, id); len(ids) > 0 {
		return &fleet.ConversationIdError{ID: id, SessionIds: ids}
	}
	return fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, id)
}

// noSuchSessionReceipt is the same decision for a send, which answers with a
// refused receipt instead of an error. The structured SessionIds field is what
// a client parses; the reason is prose.
func (d *Driver) noSuchSessionReceipt(ctx context.Context, rows []paneRow, id string) fleet.DeliveryReceipt {
	if ids := d.sessionsHoldingConversation(ctx, rows, id); len(ids) > 0 {
		return fleet.DeliveryReceipt{
			Outcome:    fleet.OutcomeRefused,
			Reason:     fleet.ConversationIdReason(id, ids),
			SessionIds: ids,
		}
	}
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "no session with this id"}
}
