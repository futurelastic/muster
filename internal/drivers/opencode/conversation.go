package opencode

import (
	fleet "github.com/futurelastic/muster"
)

// What this driver reports as a session's conversation (muster #378, spec §2.9).
//
// On this runtime the conversation IS the runtime's session: the identifier the
// create call returns is the one the runtime files its record under, so there is
// nothing to derive and no title to match. A client that keys a per-session link
// on `conversation.known` would otherwise see an opencode session as one with no
// record at all, although the driver created it and holds the id.
//
// Three answers, and never a fourth:
//
//   - captured: the create response named the id. Reported from the create onward.
//   - confirmed after a restart: the relaunched runtime answered a read by that
//     same id, so the record is still held.
//   - unknown, with the reason: the id could not be confirmed. The driver wrote it
//     down once, which is not the same as the runtime still holding that record,
//     so the remembered identifier is not served on its own (§2.9: a guess shaped
//     like a reading).
//
// The field is stored as a pointer and copied on every read, so a caller cannot
// alter what the next one sees.

// capturedAtCreate is the answer for a session this driver just created.
func capturedAtCreate(id string) *fleet.ConversationRef {
	return fleet.ResolvedConversation(id, fleet.ConversationCaptured,
		"the runtime returned this session id when the session was created; on this runtime the conversation is the session itself")
}

// confirmedAfterRestart is the answer for a session relaunched after a service
// restart whose runtime answered a read by this id.
func confirmedAfterRestart(id string) *fleet.ConversationRef {
	return fleet.ResolvedConversation(id, fleet.ConversationCaptured,
		"captured when the session was created, and confirmed after the service restarted: the relaunched runtime answered a read by this id")
}

// unconfirmed is the answer for a session whose id could not be confirmed after
// a restart. reason is why, in the driver's own words; it is never the id.
func unconfirmed(reason string) *fleet.ConversationRef {
	return fleet.UnresolvedConversation(
		"the runtime's record of this session could not be confirmed after the service restarted: " + reason)
}

// conversationRef returns this session's conversation as a copy, nil when the
// driver never looked (a session it holds no record of).
func (k knownSession) conversationRef() *fleet.ConversationRef {
	if k.conversation == nil {
		return nil
	}
	c := *k.conversation
	return &c
}
