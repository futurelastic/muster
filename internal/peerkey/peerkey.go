// Package peerkey is the signed peer request (muster #350): a key pair per
// machine, and a request form that carries a signature instead of a reusable
// secret.
//
// # Why
//
// A bearer token sent on every request is a credential a passive observer
// keeps. Peers talk over plain http on a LAN or VPN, so "captured once, works
// forever" is the ordinary failure. A signature fixes it by what it is bound
// to, not by hiding it: each one covers the method, the path and query, a
// digest of the body, the machine it is addressed to, the sender, a time and a
// nonce, so a captured request can be replayed to nobody — not to the same
// machine (the nonce is spent), not later (the time has passed), not to another
// machine (the target is in the signature), and not with anything altered.
// Plain http stays safe against capture; it does NOT become private. A passive
// observer still reads the traffic.
//
// # What is signed
//
// The canonical string (Canonical) is the whole contract between a signer and
// a verifier. It is plain text with one field per line so that two
// implementations cannot disagree about a delimiter, and it begins with a
// version line so that a future form cannot be mistaken for this one.
//
// The request-assertion headers a relay adds (on-behalf-of, human-relay,
// remote-control, acting person) are part of it. They are claims the receiver
// weighs, and on a plain-http hop an attacker who cannot forge a signature
// could otherwise still swap one of them in a captured request they have
// learned to relay.
//
// # What is not here
//
// Response signing. A signature authenticates the caller to the peer; it does
// not prove to the caller that the peer is the peer. That is a separate
// property with a separate cost (a response can be a stream), and this package
// does not pretend to give it.
package peerkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/futurelastic/muster/internal/privfs"
)

const (
	// Scheme is the Authorization scheme a signed request uses.
	Scheme = "Muster-Sig"
	// Version is the canonical form's version, carried in the header and in the
	// signed string.
	Version = "v1"
	// DigestHeader carries the sender's sha256 of the body. It is a claim the
	// verifier checks against the body it actually read, never a value it trusts:
	// it exists so a mismatch can be told apart from a bad signature.
	DigestHeader = "Fleet-Content-Digest"
	// MaxSkew is how far a signature's time may be from the verifier's clock, in
	// either direction. It bounds the replay window and is the clock tolerance
	// at once; a verifier outside it reports its own time so a skewed clock is
	// diagnosable rather than a mystery 401.
	MaxSkew = 60 * time.Second
	// MaxBodyBytes bounds the body a verifier reads to digest it.
	MaxBodyBytes = 8 << 20

	publicPrefix  = "ed25519:"
	privatePrefix = "ed25519-private:"
)

// AssertionHeaders are the relay assertions covered by the signature, in the
// order they are signed.
var AssertionHeaders = []string{
	"Fleet-On-Behalf-Of",
	"Fleet-Human-Relay",
	"Fleet-Remote-Control",
	"Fleet-Acting-Person",
	"Fleet-Acting-Issuer",
}

// Why a signature was refused. The service turns these into a 401 whose message
// says which, because "unauthorized" alone sends an operator to compare keys
// when the actual fault is a clock.
var (
	ErrMalformed = errors.New("malformed signature header")
	ErrExpired   = errors.New("signature time is outside the allowed window")
	ErrReplay    = errors.New("signature already used")
	ErrBadSig    = errors.New("signature does not verify")
	ErrDigest    = errors.New("body does not match the digest the sender signed")
)

// ParsePublicKey reads "ed25519:<base64url>".
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), publicPrefix)
	if !ok {
		return nil, fmt.Errorf("a public key reads %s<base64url>", publicPrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("not a %d-byte ed25519 public key", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// FormatPublicKey is ParsePublicKey's inverse.
func FormatPublicKey(k ed25519.PublicKey) string {
	return publicPrefix + base64.RawURLEncoding.EncodeToString(k)
}

// Fingerprint is a short, stable name for a key, for a human comparing two
// screens. It is not a security boundary.
func Fingerprint(k ed25519.PublicKey) string {
	sum := sha256.Sum256(k)
	return hex.EncodeToString(sum[:8])
}

// Generate makes a key pair.
func Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// SavePrivate writes a private key owner-only, atomically, and never reports
// its content. The seed alone is stored: the public half derives from it.
func SavePrivate(path string, priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("not an ed25519 private key")
	}
	if err := privfs.MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	tmp, err := privfs.CreateTemp(filepath.Dir(path), ".key.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	line := privatePrefix + base64.RawURLEncoding.EncodeToString(priv.Seed()) + "\n"
	if _, err := tmp.WriteString(line); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadPrivate reads a key SavePrivate wrote. The error never includes the file's
// content.
func LoadPrivate(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	enc, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), privatePrefix)
	if !ok {
		return nil, fmt.Errorf("%s is not a muster private key", path)
	}
	seed, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a muster private key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Canonical is the string that is signed. See the package comment.
func Canonical(method, requestURI, bodySHA256Hex, target, sender string, ts int64, nonce string, assertions []string) []byte {
	var b strings.Builder
	b.WriteString("muster-sig-" + Version + "\n")
	for _, f := range []string{method, requestURI, bodySHA256Hex, target, sender, strconv.FormatInt(ts, 10), nonce} {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	for _, a := range assertions {
		b.WriteString(a)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func assertionsOf(h http.Header) []string {
	out := make([]string, len(AssertionHeaders))
	for i, name := range AssertionHeaders {
		out[i] = h.Get(name)
	}
	return out
}

// BodyDigest is the lowercase hex sha256 of a body.
func BodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Signer signs this machine's outgoing requests with its key file. The file is
// re-read when it changes, so `muster key rotate` takes effect in a running
// service without a restart.
type Signer struct {
	path string
	self string

	mu    sync.Mutex
	key   ed25519.PrivateKey
	mtime time.Time
	size  int64
	now   func() time.Time
}

// NewSigner opens the key at path as machine self. It fails if the key is not
// there: a service configured to sign that cannot is a fault to report at
// start, not a request to refuse later.
func NewSigner(path, self string) (*Signer, error) {
	s := &Signer{path: path, self: self, now: time.Now}
	if _, err := s.current(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetClock replaces the signer's clock. For tests that need a stale or future
// signature; a service never calls it.
func (s *Signer) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Public is the public half of the key currently in use.
func (s *Signer) Public() (ed25519.PublicKey, error) {
	k, err := s.current()
	if err != nil {
		return nil, err
	}
	return k.Public().(ed25519.PublicKey), nil
}

func (s *Signer) current() (ed25519.PrivateKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.path)
	if err != nil {
		return nil, err
	}
	if s.key == nil || !info.ModTime().Equal(s.mtime) || info.Size() != s.size {
		k, err := LoadPrivate(s.path)
		if err != nil {
			return nil, err
		}
		s.key, s.mtime, s.size = k, info.ModTime(), info.Size()
	}
	return s.key, nil
}

// Sign adds the signature headers to req, which is addressed to machine target.
// body is the exact bytes that will be sent (nil for none). It replaces any
// Authorization the request carried.
func (s *Signer) Sign(req *http.Request, body []byte, target string) error {
	key, err := s.current()
	if err != nil {
		return fmt.Errorf("signing a peer request: %w", err)
	}
	nonceRaw := make([]byte, 16)
	if _, err := rand.Read(nonceRaw); err != nil {
		return err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceRaw)
	s.mu.Lock()
	ts := s.now().Unix()
	s.mu.Unlock()
	digest := BodyDigest(body)
	req.Header.Set(DigestHeader, "sha256="+digest)
	sig := ed25519.Sign(key, Canonical(req.Method, req.URL.RequestURI(), digest, target, s.self, ts, nonce, assertionsOf(req.Header)))
	req.Header.Set("Authorization", fmt.Sprintf("%s %s machine=%s, ts=%d, nonce=%s, sig=%s",
		Scheme, Version, s.self, ts, nonce, base64.RawURLEncoding.EncodeToString(sig)))
	return nil
}

// Header is a parsed signature Authorization header.
type Header struct {
	Machine string
	TS      int64
	Nonce   string
	Sig     []byte
}

// IsSigned reports whether an Authorization value uses this scheme.
func IsSigned(authorization string) bool {
	return strings.HasPrefix(authorization, Scheme+" ")
}

// ParseAuthorization reads a header Sign wrote.
func ParseAuthorization(v string) (Header, error) {
	rest, ok := strings.CutPrefix(v, Scheme+" "+Version+" ")
	if !ok {
		return Header{}, fmt.Errorf("%w: unknown scheme or version", ErrMalformed)
	}
	var h Header
	seen := map[string]bool{}
	for _, part := range strings.Split(rest, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || seen[k] {
			return Header{}, ErrMalformed
		}
		seen[k] = true
		switch k {
		case "machine":
			h.Machine = val
		case "ts":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return Header{}, ErrMalformed
			}
			h.TS = n
		case "nonce":
			h.Nonce = val
		case "sig":
			b, err := base64.RawURLEncoding.DecodeString(val)
			if err != nil || len(b) != ed25519.SignatureSize {
				return Header{}, ErrMalformed
			}
			h.Sig = b
		default:
			return Header{}, ErrMalformed
		}
	}
	if h.Machine == "" || h.Nonce == "" || len(h.Nonce) > 64 || len(h.Sig) == 0 || h.TS == 0 {
		return Header{}, ErrMalformed
	}
	return h, nil
}

// Verify checks a parsed header against a request, for a verifier that is
// machine self. keys are the public keys acceptable for the claimed sender, the
// current one first; the index of the one that verified is returned, so a caller
// can tell a request signed with the current key from one signed with a retired
// key still in its grace period. body is what was read from the request.
//
// Order matters: the time and the digest are checked before the signature so the
// error says what is actually wrong, and the replay cache is consulted only
// AFTER the signature verifies, so an attacker cannot fill it with nonces that
// were never signed.
func Verify(h Header, r *http.Request, body []byte, self string, keys []ed25519.PublicKey, now time.Time, replay *ReplayCache) (int, error) {
	skew := now.Sub(time.Unix(h.TS, 0))
	if skew > MaxSkew || skew < -MaxSkew {
		return -1, fmt.Errorf("%w (this machine's clock reads %s, the signature says %s)",
			ErrExpired, now.UTC().Format(time.RFC3339), time.Unix(h.TS, 0).UTC().Format(time.RFC3339))
	}
	digest := BodyDigest(body)
	if claimed := r.Header.Get(DigestHeader); claimed != "sha256="+digest {
		return -1, ErrDigest
	}
	msg := Canonical(r.Method, r.RequestURI, digest, self, h.Machine, h.TS, h.Nonce, assertionsOf(r.Header))
	matched := -1
	for i, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, msg, h.Sig) {
			matched = i
			break
		}
	}
	if matched < 0 {
		return -1, ErrBadSig
	}
	if err := replay.Use(h.Machine, h.Nonce, now); err != nil {
		return -1, err
	}
	return matched, nil
}

// ErrReplayFull means the replay cache is at its bound and failing closed.
var ErrReplayFull = errors.New("too many signed requests in the replay window")

// ReplayCache remembers the nonces of verified requests for as long as a
// signature could still be accepted. It is bounded and fails closed: past the
// bound it refuses new signatures rather than forgetting old ones, because
// forgetting is exactly the replay it exists to stop.
type ReplayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	max  int
}

// NewReplayCache makes a cache holding at most max nonces.
func NewReplayCache(max int) *ReplayCache {
	return &ReplayCache{seen: map[string]time.Time{}, max: max}
}

// Use records a nonce. A nonce is scoped to its sender. It returns ErrReplay for
// a repeat and, failing closed, ErrReplayFull when the cache has no room.
func (c *ReplayCache) Use(sender, nonce string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) >= c.max {
		for k, exp := range c.seen {
			if !exp.After(now) {
				delete(c.seen, k)
			}
		}
		if len(c.seen) >= c.max {
			return ErrReplayFull
		}
	}
	k := sender + "\x00" + nonce
	if exp, ok := c.seen[k]; ok && exp.After(now) {
		return ErrReplay
	}
	// Twice the skew: a signature stamped at the far edge of the future is
	// still acceptable until its own time plus the window.
	c.seen[k] = now.Add(2*MaxSkew + time.Second)
	return nil
}
