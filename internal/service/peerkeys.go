package service

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/state"
)

// Signed peer requests, service side (muster #350). The wire form and the
// reasoning for it are in internal/peerkey; this file is what the service does
// with it: authenticate a signed request as a principal, and let a peer rotate
// the key it signs with.

// peerKeysFile is the rotation overlay in the state directory.
const peerKeysFile = "peer-keys"

// keyGrace is how long the key a peer rotated AWAY from is still accepted. It
// covers the requests the peer had already signed or queued when the rotation
// was acknowledged, and a peer whose key file has not been picked up yet; it is
// short because a retired key is exactly what a thief would hold.
const keyGrace = 10 * time.Minute

// maxSignedNonces bounds the replay cache: a machine accepting more signed
// requests than this inside two minutes refuses further ones, rather than
// forgetting a nonce.
const maxSignedNonces = 100000

// peerKeyRecord is one principal's rotated key. Base is the configured key the
// rotation started from: an operator who later changes the configured key by
// hand makes Base stale, and then the configuration wins and the record is
// ignored. A rotation is a runtime fact; the configuration file stays the
// operator's.
type peerKeyRecord struct {
	Base          string    `json:"base"`
	Current       string    `json:"current"`
	Previous      string    `json:"previous,omitempty"`
	PreviousUntil time.Time `json:"previousUntil,omitempty"`
}

type peerKeyStore struct {
	mu   sync.Mutex
	st   *state.Store
	recs map[string]peerKeyRecord
}

func newPeerKeyStore() *peerKeyStore { return &peerKeyStore{recs: map[string]peerKeyRecord{}} }

func (s *peerKeyStore) load(st *state.Store) error {
	if st == nil || st.Dir() == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = st
	var recs map[string]peerKeyRecord
	if _, err := st.Load(peerKeysFile, &recs); err != nil {
		return err
	}
	if recs != nil {
		s.recs = recs
	}
	return nil
}

// durable reports whether a rotation would survive a restart.
func (s *peerKeyStore) durable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st != nil
}

// keys are the public keys a principal's signature may verify against right
// now, current first.
func (s *peerKeyStore) keys(name string, configured ed25519.PublicKey, now time.Time) []ed25519.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[name]
	if !ok || rec.Base != peerkey.FormatPublicKey(configured) {
		return []ed25519.PublicKey{configured}
	}
	var out []ed25519.PublicKey
	if k, err := peerkey.ParsePublicKey(rec.Current); err == nil {
		out = append(out, k)
	}
	if rec.Previous != "" && now.Before(rec.PreviousUntil) {
		if k, err := peerkey.ParsePublicKey(rec.Previous); err == nil {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return []ed25519.PublicKey{configured}
	}
	return out
}

// rotate makes next the principal's current key. It is idempotent for the key
// that is already current. The old current stays acceptable for keyGrace.
func (s *peerKeyStore) rotate(name string, configured, next ed25519.PublicKey, now time.Time) (peerKeyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := peerkey.FormatPublicKey(configured)
	cur := base
	rec, ok := s.recs[name]
	if ok && rec.Base == base {
		cur = rec.Current
	} else {
		rec = peerKeyRecord{Base: base}
	}
	nextText := peerkey.FormatPublicKey(next)
	if cur == nextText {
		return rec, nil
	}
	rec.Previous, rec.PreviousUntil, rec.Current = cur, now.Add(keyGrace), nextText
	recs := make(map[string]peerKeyRecord, len(s.recs)+1)
	for k, v := range s.recs {
		recs[k] = v
	}
	recs[name] = rec
	if err := s.st.Save(peerKeysFile, recs); err != nil {
		return peerKeyRecord{}, err
	}
	s.recs = recs
	return rec, nil
}

// signedKeyFor finds the configured public key of the principal that claims to
// be machine.
func signedKeyFor(cfg Config, machine string) (Principal, bool) {
	for _, p := range cfg.principals() {
		if p.Name == machine && len(p.PublicKey) > 0 {
			return p, true
		}
	}
	return Principal{}, false
}

// authenticateSigned resolves a Muster-Sig request to its principal, or says
// why not. It reads the body (bounded) to digest it and puts it back.
//
// Every refusal is the same kind — unauthorized, a 401 — with a message that
// says which check failed, except that a sender no table lists and a signature
// that does not verify read the same: telling them apart would let a stranger
// enumerate machine names.
func (svc *Service) authenticateSigned(cfg Config, r *http.Request) (Principal, *fleet.Error) {
	refuse := func(msg string) (Principal, *fleet.Error) {
		return Principal{}, &fleet.Error{Kind: fleet.ErrorUnauthorized, Message: msg}
	}
	h, err := peerkey.ParseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return refuse("malformed signed-request header")
	}
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		body, err = io.ReadAll(io.LimitReader(r.Body, peerkey.MaxBodyBytes+1))
		if err != nil {
			return refuse("the request body could not be read")
		}
		if len(body) > peerkey.MaxBodyBytes {
			return Principal{}, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "a signed request body is limited to 8 MiB"}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	p, ok := signedKeyFor(cfg, h.Machine)
	if !ok {
		return refuse("signature does not verify")
	}
	keys := svc.peerKeys.keys(p.Name, p.PublicKey, time.Now())
	matched, err := peerkey.Verify(h, r, body, string(svc.Self()), keys, time.Now(), svc.sigReplay)
	if err != nil {
		switch {
		case errors.Is(err, peerkey.ErrExpired):
			return refuse(err.Error())
		case errors.Is(err, peerkey.ErrReplay):
			return refuse("signature already used: a signed request is accepted once")
		case errors.Is(err, peerkey.ErrReplayFull):
			return refuse("too many signed requests in the replay window: try again shortly")
		case errors.Is(err, peerkey.ErrDigest):
			return refuse("the body does not match the digest the sender signed")
		}
		return refuse("signature does not verify")
	}
	p.signed = true
	// A key past the first is a retired one still in its grace period. It may
	// make requests, but may not rotate (handleRotatePeerKey).
	p.signedRetired = matched > 0
	return p, nil
}

// handleRotatePeerKey answers POST /v1/peer-key: the calling peer replaces the
// public key this machine verifies its signatures with. The old key signs the
// request (authenticateSigned already checked it), so only the holder of the
// current key can move a peer to a new one; the old key keeps working for
// keyGrace, so a peer that has not yet switched is not dropped.
func handleRotatePeerKey(svc *Service, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalOf(r)
		if !ok || !p.signed {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnauthorized,
				Message: "a key rotation must be signed with the key being replaced: a bearer token cannot change a signing key"})
			return
		}
		var body struct {
			PublicKey string `json:"publicKey"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "body must be {\"publicKey\": \"ed25519:...\"}"})
			return
		}
		next, err := peerkey.ParsePublicKey(body.PublicKey)
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		if !svc.peerKeys.durable() {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported,
				Message: "this machine has no state directory, so a rotated key would be forgotten at the next restart and lock the peer out"})
			return
		}
		// Only the CURRENT key may move a peer to a different key. A retired key
		// in its grace period is exactly what a thief would be holding, and
		// letting it rotate would turn ten minutes of tolerance into a takeover.
		// It may repeat a rotation that already happened (the same key), which is
		// how a re-run after a partial failure finds a peer already done.
		if current := svc.peerKeys.keys(p.Name, p.PublicKey, time.Now())[0]; p.signedRetired && !current.Equal(next) {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnauthorized,
				Message: "this request was signed with a retired key: only the current key may replace it"})
			return
		}
		rec, err := svc.peerKeys.rotate(p.Name, p.PublicKey, next, time.Now())
		if err != nil {
			log.Printf("audit: peer-key rotation for %q failed: %v", p.Name, err)
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: "the new key could not be saved"})
			return
		}
		log.Printf("audit: peer %q rotated its signing key to %s (previous accepted until %s)",
			p.Name, peerkey.Fingerprint(next), rec.PreviousUntil.UTC().Format(time.RFC3339))
		writeJSON(w, http.StatusOK, struct {
			Machine       string     `json:"machine"`
			Fingerprint   string     `json:"fingerprint"`
			PreviousUntil *time.Time `json:"previousKeyAcceptedUntil,omitempty"`
		}{string(p.Name), peerkey.Fingerprint(next), previousUntil(rec)})
	}
}

func previousUntil(rec peerKeyRecord) *time.Time {
	if rec.Previous == "" || rec.PreviousUntil.IsZero() {
		return nil
	}
	t := rec.PreviousUntil.UTC()
	return &t
}
