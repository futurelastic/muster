// Package grant holds a model provider's OAuth grant for a whole machine and is
// the only thing that refreshes it (muster #451).
//
// A subscription sign-in is a refresh token plus a short-lived access token, and
// the refresh token ROTATES: every use returns a new one and kills the old. Two
// holders of one refresh token therefore cannot both be right, which is why the
// obvious shapes fail — a copy per session breaks every copy but the first to
// refresh, and one shared file has separate processes racing the same token.
// So exactly one party refreshes, and the sessions never hold a token they could
// refresh with:
//
//   - the sign-in goes into an owner-only STORE under the service's state
//     directory (store.go);
//   - the REFRESHER (refresher.go) renews it before it lapses and writes the
//     rotated refresh token to the store BEFORE anyone is told about the new
//     access token, so a crash between the two never strands the grant;
//   - a session's own auth file carries the current access token and a
//     placeholder in the refresh slot (Placeholder). A session that ever tried to
//     refresh with it would post a token the provider rejects, rotating nothing
//     and harming nothing.
//
// What the runtime does with that file was measured, not assumed: a running
// server re-reads it before every provider request, so a rewrite takes effect on
// the next turn with no relaunch (docs/adr/451-*).
//
// Standard library only.
package grant

import (
	"errors"
	"fmt"
	"time"
)

// ProviderOpenAI is the id of the provider this package holds a grant for: the
// runtime's own id for it, and the key of its entry in a session's auth file.
const ProviderOpenAI = "openai"

// Placeholder is what a session's auth file carries where a refresh token goes.
// The runtime's file format requires one; this is not a credential.
const Placeholder = "muster-holds-the-refresh-token"

// Grant is what the store holds: one sign-in and what became of it.
type Grant struct {
	// Version is the file's format. Only 1 exists.
	Version int `json:"version"`
	// Generation counts sign-ins. A refresh result is kept only if the stored
	// generation is still the one it started from: a sign-in that landed while it
	// was in flight wins.
	Generation int64 `json:"generation"`
	// Refresh and Access are the credential. Never logged, never put in an error.
	Refresh string `json:"refresh"`
	Access  string `json:"access"`
	// ExpiresAtMs is when Access stops working, in Unix milliseconds.
	ExpiresAtMs int64 `json:"expires"`
	// AccountID is the provider's account for the sign-in, when it named one.
	AccountID string `json:"accountId,omitempty"`
	// SignedInAtMs and RefreshedAtMs are Unix milliseconds. The access token's own
	// lifetime is read from the span between the later of the two and ExpiresAtMs,
	// so nothing here assumes how long a token lives.
	SignedInAtMs  int64 `json:"signedIn"`
	RefreshedAtMs int64 `json:"refreshed,omitempty"`
	// Revoked is set when the provider refused the refresh token. It stays until
	// a new sign-in replaces the grant.
	Revoked *Revoked `json:"revoked,omitempty"`
	// Limit is set while the provider keeps refusing requests made with the
	// sign-in (muster #458). It is the service's observation, written only through
	// the refresher, kept in the store so the offline doctor can read it, and it
	// describes the account, not the credential: a refresh does not clear it and a
	// new sign-in does.
	Limit *Limit `json:"limit,omitempty"`
}

// Limit records that sessions on the sign-in are being refused. It holds no
// credential.
type Limit struct {
	// SinceMs is when the first session was seen refused in this episode and
	// UpdatedMs when the service last saw it still so, both Unix milliseconds. A
	// mark whose UpdatedMs is old was left by a service that has since stopped.
	SinceMs   int64 `json:"since"`
	UpdatedMs int64 `json:"updated"`
	// Message is the provider's own message as the runtime reported it, cut short.
	// It is not parsed.
	Message string `json:"message,omitempty"`
}

// Revoked records the provider's refusal of a refresh token.
type Revoked struct {
	AtMs   int64  `json:"at"`
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
}

// ExpiresAt is Access's expiry.
func (g Grant) ExpiresAt() time.Time { return time.UnixMilli(g.ExpiresAtMs) }

// Access is what a session needs: the current access token and when it lapses.
// It carries no refresh token, by construction.
type Access struct {
	Token     string
	ExpiresAt time.Time
	AccountID string
}

// Phase is the machine-level state of a grant, the one word a person reads.
type Phase string

const (
	// PhaseSignedOut: no grant is stored.
	PhaseSignedOut Phase = "signed-out"
	// PhaseOK: a grant is stored and its access token is current.
	PhaseOK Phase = "ok"
	// PhaseDegraded: the last refresh failed for a reason that is not the grant's
	// fault (the provider unreachable, answering 5xx). It is being retried; the
	// access token may still be good or may have lapsed (ExpiresAt says).
	PhaseDegraded Phase = "degraded"
	// PhaseRevoked: the provider refused the refresh token. Nothing renews it; a
	// person signs in again.
	PhaseRevoked Phase = "revoked"
)

// State is a grant's state for a diagnostic or a listing. It never holds a
// credential.
type State struct {
	Provider        string
	Phase           Phase
	AccessExpiresAt time.Time
	SignedInAt      time.Time
	RefreshedAt     time.Time
	// Reason is one sentence for a person, set unless Phase is ok.
	Reason string
}

// ErrSignedOut is returned when no grant is stored.
var ErrSignedOut = errors.New("no ChatGPT grant is signed in on this machine (run: muster grant login)")

// RevokedError is returned when the provider has refused the stored refresh
// token, so nothing here can renew the grant.
type RevokedError struct {
	Status int
	Code   string
}

func (e *RevokedError) Error() string {
	why := fmt.Sprintf("the provider refused the stored refresh token (HTTP %d", e.Status)
	if e.Code != "" {
		why += ", " + e.Code
	}
	return why + "); sign in again (run: muster grant login)"
}

// stateOf describes g at now. A nil g is signed out.
func stateOf(g *Grant, now time.Time) State {
	st := State{Provider: ProviderOpenAI, Phase: PhaseSignedOut, Reason: ErrSignedOut.Error()}
	if g == nil {
		return st
	}
	st.AccessExpiresAt = g.ExpiresAt()
	st.SignedInAt = time.UnixMilli(g.SignedInAtMs)
	if g.RefreshedAtMs > 0 {
		st.RefreshedAt = time.UnixMilli(g.RefreshedAtMs)
	}
	if g.Revoked != nil {
		st.Phase = PhaseRevoked
		st.Reason = (&RevokedError{Status: g.Revoked.Status, Code: g.Revoked.Code}).Error()
		return st
	}
	st.Phase, st.Reason = PhaseOK, ""
	return st
}
