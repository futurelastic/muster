package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/grant"
	"github.com/futurelastic/muster/internal/privfs"
)

// A ChatGPT sign-in shared by every session on the machine (muster #451).
//
// The runtime ships sign-in for its `openai` provider and stores the result in
// `$XDG_DATA_HOME/opencode/auth.json`. Every session here has its own
// XDG_DATA_HOME (env.go), so a session never sees a sign-in the service user made,
// and copying one in would break it: the refresh token rotates on each use, so the
// first session to refresh would leave every other copy holding a dead token.
//
// So one party refreshes — grant.Refresher, in this process — and a session's
// auth file holds only a current ACCESS token, with a placeholder where the
// refresh token goes. This driver does two things with it:
//
//   - At a create and at a relaunch it writes that file, before the server starts.
//     The runtime decides whether `openai` is usable (and which of its models are
//     offered to a sign-in rather than a key) when the server starts, so the entry
//     has to be there by then.
//   - Whenever the access token changes it rewrites the file of every live session,
//     in place. Measured on the real runtime (docs/adr/451-*): a running server
//     reads the file again before every provider request, so the new token is in
//     use on the next turn and nothing is relaunched.
//
// The provider policy (provider.go) applies unchanged. A sign-in makes `openai`
// USABLE; it never makes it ALLOWED: the entry is written only into a session
// whose policy permits `openai`, so a machine that does not name it hands the
// credential to nobody.
//
// What a session can learn from its own copy: an access token that is good for
// the hours or days it has left, and an account id. Not the refresh token, so it
// cannot keep the sign-in alive after it ends, and a session that tried to
// refresh with the placeholder would post a token the provider rejects, rotating
// nothing. The access token is in the session's reach, as a model-provider key
// always is (env.go says so plainly); the sandbox is what confines the filesystem.

// GrantSource is what this driver needs of the machine's sign-in: the current
// access token (renewed first if it is about to lapse), a notice whenever it
// changes, and the state for a listing. *grant.Refresher is one.
type GrantSource interface {
	Access(ctx context.Context) (grant.Access, error)
	Subscribe(fn func(grant.Access)) (cancel func())
	State() grant.State
}

// WithGrant gives every session whose provider policy permits `openai` the
// machine's ChatGPT sign-in (muster #451). Ignored in shared mode, which has no
// per-session directory to put it in. Nil leaves the driver as it was.
func WithGrant(src GrantSource) Option { return func(d *Driver) { d.grants = src } }

// authPath is the session's auth file: the runtime's own location under the
// session's XDG_DATA_HOME.
func authPath(dir string) string {
	return filepath.Join(scopedEnv(dir)["XDG_DATA_HOME"], "opencode", "auth.json")
}

// grantUsable reports whether sessions of this driver may be given the sign-in at
// all: one is configured, this is the isolated mode, and the provider policy
// permits the provider.
func (d *Driver) grantUsable() bool {
	return d.grants != nil && d.shared == nil && d.providers.permits(grant.ProviderOpenAI)
}

// writeSessionAuth puts access into the session's own auth file under dir, in the
// shape the runtime stores a sign-in in, replacing only the provider's entry.
//
// The file is written whole beside the target and renamed over it, so the runtime
// — which reads it before every request — never sees half of one. A file the
// runtime could not parse is replaced rather than extended: it is this driver's
// file, and what was in it is not worth failing a session for.
//
// create says whether the file's directory may be made. A start makes it; an
// update of a live session never does: a session being closed is removing its
// directory, and an update that recreated it would leave one behind that no
// record names.
func writeSessionAuth(dir string, a grant.Access, create bool) error {
	path := authPath(dir)
	if create {
		if err := privfs.MkdirAll(filepath.Dir(path)); err != nil {
			return fmt.Errorf("opencode: creating the session's auth directory: %w", err)
		}
	} else if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return nil // the session's directory is gone; there is nothing to update
	}
	entries := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(raw, &entries) != nil || entries == nil {
			entries = map[string]json.RawMessage{}
		}
	}
	entry := map[string]any{
		"type":    "oauth",
		"access":  a.Token,
		"refresh": grant.Placeholder,
		"expires": a.ExpiresAt.UnixMilli(),
	}
	if a.AccountID != "" {
		entry["accountId"] = a.AccountID
	}
	rawEntry, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("opencode: encoding the session's sign-in: %w", err)
	}
	entries[grant.ProviderOpenAI] = rawEntry
	return writeAuthFile(path, entries)
}

// writeAuthFile writes the session's auth file whole beside the target and renames
// it over, so the runtime — which reads it before every request — never sees half
// of one.
func writeAuthFile(path string, entries map[string]json.RawMessage) error {
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("opencode: encoding the session's auth file: %w", err)
	}
	tmp, err := privfs.CreateTemp(filepath.Dir(path), "auth-*.tmp")
	if err != nil {
		return fmt.Errorf("opencode: writing the session's auth file: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	for _, e := range []error{werr, cerr} {
		if e != nil {
			_ = os.Remove(name)
			return fmt.Errorf("opencode: writing the session's auth file: %w", e)
		}
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("opencode: replacing the session's auth file: %w", err)
	}
	return nil
}

// pushAccess is the subscription: the access token changed, so every live
// session's file is rewritten in place. A failure on one session is said and does
// not stop the others; a session that missed it keeps a token that is good until
// its own expiry, and the next change reaches it.
func (d *Driver) pushAccess(a grant.Access) {
	if !d.grantUsable() {
		return
	}
	d.mu.RLock()
	dirs := make([]string, 0, len(d.live))
	for s := range d.live {
		// muster #458: only a session the service put on the subscription holds its
		// token. One on an API key was given no sign-in, and a rewrite here would
		// hand it the other account.
		if s.dir != "" && s.credential == fleet.CredentialSubscription {
			dirs = append(dirs, s.dir)
		}
	}
	d.mu.RUnlock()
	wrote := 0
	for _, dir := range dirs {
		if err := writeSessionAuth(dir, a, false); err != nil {
			d.say("opencode: could not update a session's openai sign-in: %v", err)
			continue
		}
		wrote++
	}
	if len(dirs) > 0 {
		d.say("opencode: the openai access token was renewed; %d of %d live sessions have it", wrote, len(dirs))
	}
}

// catchUpGrant closes the window between a start writing its auth file and the
// server becoming visible to pushAccess: a renewal in that window reached nobody
// who was not yet tracked. Called after the server is tracked; the access token is
// read again and written if it differs from what the start wrote.
func (d *Driver) catchUpGrant(ctx context.Context, dir string) {
	if !d.grantUsable() {
		return
	}
	a, err := d.grants.Access(ctx)
	if err != nil {
		return
	}
	if cur, ok := readSessionAccess(dir); ok && cur == a.Token {
		return
	}
	if err := writeSessionAuth(dir, a, false); err != nil {
		d.say("opencode: could not update a session's openai sign-in: %v", err)
	}
}

// readSessionAccess is the access token in the session's auth file, if it has one.
func readSessionAccess(dir string) (string, bool) {
	raw, err := os.ReadFile(authPath(dir))
	if err != nil {
		return "", false
	}
	var entries map[string]struct {
		Access string `json:"access"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return "", false
	}
	e, ok := entries[grant.ProviderOpenAI]
	return e.Access, ok
}

// Grants implements driver.GrantReporter: where the machine's sign-in stands, for
// GET /v1/runtimes. Nothing when none is configured here or the provider policy
// does not name `openai` — a sign-in that cannot be used is not this runtime's.
func (d *Driver) Grants() []fleet.RuntimeGrant {
	if !d.grantUsable() {
		return nil
	}
	st := d.grants.State()
	g := fleet.RuntimeGrant{Provider: st.Provider, Reason: st.Reason}
	switch st.Phase {
	case grant.PhaseOK:
		g.State = fleet.GrantOK
	case grant.PhaseDegraded:
		g.State = fleet.GrantDegraded
	case grant.PhaseRevoked:
		g.State = fleet.GrantRevoked
	default:
		g.State = fleet.GrantSignedOut
	}
	if !st.AccessExpiresAt.IsZero() {
		t := st.AccessExpiresAt.UTC().Truncate(time.Second)
		g.AccessExpiresAt = &t
	}
	switch {
	case !st.RefreshedAt.IsZero():
		t := st.RefreshedAt.UTC().Truncate(time.Second)
		g.RefreshedAt = &t
	case !st.SignedInAt.IsZero():
		t := st.SignedInAt.UTC().Truncate(time.Second)
		g.RefreshedAt = &t
	}
	g.Quota = d.liveQuota() // muster #458
	return []fleet.RuntimeGrant{g}
}

var _ driver.GrantReporter = (*Driver)(nil)

// grantHint is what a refusal adds when the model it refuses is on the provider
// the machine's sign-in serves and that sign-in is not working: the reason the
// session has no such model is the sign-in, and nothing else in the message says so.
func (d *Driver) grantHint(model string) string {
	if !d.grantUsable() || providerOfModel(model) != grant.ProviderOpenAI {
		return ""
	}
	st := d.grants.State()
	if st.Phase == grant.PhaseOK {
		return ""
	}
	return "; this machine's " + grant.ProviderOpenAI + " sign-in is " + string(st.Phase) + ": " + st.Reason
}
