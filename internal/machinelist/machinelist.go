// Package machinelist is the owner-signed machine list (muster #351): one
// document, signed once, that says which machines belong to a fleet, how to
// reach them, and what each may do — so that nobody has to write every machine
// into every other machine by hand.
//
// # What is signed
//
// The file is an envelope around a payload. The payload is the document's JSON,
// carried as base64url so that its bytes cannot change in transit or be
// re-serialised by anything that opens the envelope. The signature covers
// SigningPrefix followed by those exact bytes. A verifier checks the signature
// over the bytes it was handed and only then parses those same bytes, so there is
// no canonical form to implement and no second parse that could disagree with the
// first. Any edit — a field changed, a machine spliced in from another genuine
// list, a signature taken from a different list — alters the bytes or the pairing
// and the check fails.
//
// # Trust
//
// The key that may sign is pinned locally by a person (Verifier.Signers). It is
// never read from the file, from wherever the file was delivered, or from the
// document: the channel is untrusted by design. The document names its own
// signers too, but only so that it is self-describing; a key the document names
// and the verifier did not pin is refused.
//
// # Rollback
//
// seq must strictly increase from one list to the next. A verifier remembers the
// highest it has accepted (SeqStore) and refuses anything lower; an old genuine
// file is exactly what an attacker holding a delivery channel would replay.
//
// # What is not built
//
// Only the single-signer path. The format carries a set of signers and a fleet
// id so that a later version can grow into several, and today a document naming
// more than one signer is refused with ErrUnsupported rather than half-honoured.
package machinelist

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/privfs"
)

const (
	// Format names the envelope and the document, and its version. A change that
	// an old verifier could misread gets a new value.
	Format = "muster-machine-list/1"
	// SigningPrefix is put in front of the payload bytes before signing, so a
	// signature made for a list can never be mistaken for one made for anything
	// else this key signs (a signed peer request begins differently).
	SigningPrefix = Format + "\n"
	// MaxBytes bounds a list a verifier will read.
	MaxBytes = 1 << 20
	// SeqFile is the name of the high-water mark inside a state directory.
	SeqFile = "machine-list.seq"
)

// Errors a verifier reports. Each names one failed check, so a caller (and a
// test) can tell a forged file from a stale one from a list for another fleet.
var (
	ErrFormat        = errors.New("machine list: malformed")
	ErrSignature     = errors.New("machine list: signature does not verify")
	ErrUnknownSigner = errors.New("machine list: signed by a key that is not pinned here")
	ErrFleet         = errors.New("machine list: belongs to a different fleet")
	ErrRollback      = errors.New("machine list: older than one already accepted")
	ErrUnsupported   = errors.New("machine list: not supported yet")
)

// Machine is one member of the fleet.
type Machine struct {
	// ID is the machine's id exactly as it names itself (FLEET_MACHINE).
	ID string `json:"id"`
	// PublicKey is the machine's signing key, "ed25519:<base64url>" (muster key show).
	PublicKey string `json:"publicKey"`
	// Addresses are the base URLs the machine is reached at, in the order to try.
	Addresses []string `json:"addresses"`
	// Grants is what this machine may do on the others — the grants it holds as a
	// principal there.
	Grants []string `json:"grants"`
}

// Issuer is a sign-in issuer the fleet trusts, for clients that verify people.
// Muster records and shows it; it does not itself verify a person.
type Issuer struct {
	ID        string `json:"id"`
	PublicKey string `json:"publicKey"`
}

// Document is what the owner signs.
type Document struct {
	Format string `json:"format"`
	// Fleet identifies the fleet. A verifier pinned to one refuses another's list.
	Fleet string `json:"fleet"`
	// Seq increases by at least one with every list the owner issues.
	Seq      uint64    `json:"seq"`
	IssuedAt string    `json:"issuedAt,omitempty"`
	Machines []Machine `json:"machines"`
	Issuers  []Issuer  `json:"issuers"`
	// Signers is the set of keys that may sign this fleet's lists. Exactly one is
	// supported today.
	Signers []string `json:"signers"`
	// NextSigner is the key the owner has pre-pinned for recovery: a list signed
	// by it is accepted by a verifier that pinned it too.
	NextSigner string `json:"nextSigner,omitempty"`
}

// Envelope is the file.
type Envelope struct {
	Format    string `json:"format"`
	Payload   string `json:"payload"`
	Signer    string `json:"signer"`
	Signature string `json:"signature"`
}

// Verifier is what a machine pins locally about the list it will accept.
type Verifier struct {
	// Fleet the list must belong to. Required.
	Fleet string
	// Signers are the pinned public keys — the current signer and, optionally, the
	// recovery key. At least one is required.
	Signers []ed25519.PublicKey
	// MinSeq is the highest seq already accepted; a list below it is refused.
	MinSeq uint64
}

// Verified is a list that passed every check.
type Verified struct {
	Document Document
	// Signer is the pinned key that signed it.
	Signer ed25519.PublicKey
	// Recovery reports that Signer is not the first pinned key: the owner signed
	// with the recovery key, and the operator should re-pin.
	Recovery bool
}

func readEnvelope(raw []byte) (Envelope, []byte, error) {
	if len(raw) > MaxBytes {
		return Envelope{}, nil, fmt.Errorf("%w: larger than %d bytes", ErrFormat, MaxBytes)
	}
	var env Envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return Envelope{}, nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if dec.More() {
		return Envelope{}, nil, fmt.Errorf("%w: trailing content after the envelope", ErrFormat)
	}
	if env.Format != Format {
		return Envelope{}, nil, fmt.Errorf("%w: format %q is not %q", ErrFormat, env.Format, Format)
	}
	payload, err := base64.RawURLEncoding.DecodeString(env.Payload)
	if err != nil || len(payload) == 0 {
		return Envelope{}, nil, fmt.Errorf("%w: payload is not base64url", ErrFormat)
	}
	return env, payload, nil
}

// Decode reads an envelope and its document WITHOUT checking the signature. It
// exists for showing a list; nothing may act on what it returns.
func Decode(raw []byte) (Envelope, Document, error) {
	env, payload, err := readEnvelope(raw)
	if err != nil {
		return Envelope{}, Document{}, err
	}
	doc, err := parseDocument(payload)
	return env, doc, err
}

func parseDocument(payload []byte) (Document, error) {
	var doc Document
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("%w: document: %v", ErrFormat, err)
	}
	if dec.More() {
		return Document{}, fmt.Errorf("%w: trailing content after the document", ErrFormat)
	}
	return doc, Validate(doc)
}

// Validate checks a document's shape: what a verifier or a signer can say about
// it without any key.
func Validate(doc Document) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrFormat, fmt.Sprintf(format, a...))
	}
	if doc.Format != Format {
		return bad("document format %q is not %q", doc.Format, Format)
	}
	if strings.TrimSpace(doc.Fleet) == "" {
		return bad("a list needs a fleet id")
	}
	if doc.Seq == 0 {
		return bad("seq starts at 1")
	}
	if len(doc.Signers) == 0 {
		return bad("a list names the key that signs it")
	}
	for _, s := range doc.Signers {
		if _, err := peerkey.ParsePublicKey(s); err != nil {
			return bad("signer: %v", err)
		}
	}
	if doc.NextSigner != "" {
		if _, err := peerkey.ParsePublicKey(doc.NextSigner); err != nil {
			return bad("nextSigner: %v", err)
		}
	}
	seen := map[string]bool{}
	for i, m := range doc.Machines {
		if strings.TrimSpace(m.ID) == "" {
			return bad("machine %d has no id", i+1)
		}
		if seen[m.ID] {
			return bad("machine %q is listed twice", m.ID)
		}
		seen[m.ID] = true
		if _, err := peerkey.ParsePublicKey(m.PublicKey); err != nil {
			return bad("machine %q: publicKey: %v", m.ID, err)
		}
		for _, a := range m.Addresses {
			if strings.TrimSpace(a) == "" {
				return bad("machine %q has an empty address", m.ID)
			}
		}
	}
	seenIssuer := map[string]bool{}
	for i, is := range doc.Issuers {
		if strings.TrimSpace(is.ID) == "" {
			return bad("issuer %d has no id", i+1)
		}
		if seenIssuer[is.ID] {
			return bad("issuer %q is listed twice", is.ID)
		}
		seenIssuer[is.ID] = true
		if _, err := peerkey.ParsePublicKey(is.PublicKey); err != nil {
			return bad("issuer %q: publicKey: %v", is.ID, err)
		}
	}
	return nil
}

// Verify checks a list against what this machine pinned, in an order that
// reports the most basic failure first: shape, signer, signature, then the
// document's own claims. It returns the list only if every check passed.
func Verify(raw []byte, v Verifier) (Verified, error) {
	if strings.TrimSpace(v.Fleet) == "" || len(v.Signers) == 0 {
		return Verified{}, errors.New("machine list: a verifier needs a pinned fleet and a pinned signer")
	}
	env, payload, err := readEnvelope(raw)
	if err != nil {
		return Verified{}, err
	}
	signer, err := peerkey.ParsePublicKey(env.Signer)
	if err != nil {
		return Verified{}, fmt.Errorf("%w: signer: %v", ErrFormat, err)
	}
	pinned := -1
	for i, k := range v.Signers {
		if bytes.Equal(k, signer) {
			pinned = i
			break
		}
	}
	if pinned < 0 {
		return Verified{}, ErrUnknownSigner
	}
	sig, err := base64.RawURLEncoding.DecodeString(env.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Verified{}, fmt.Errorf("%w: signature is not a %d-byte base64url value", ErrFormat, ed25519.SignatureSize)
	}
	if !ed25519.Verify(signer, signedBytes(payload), sig) {
		return Verified{}, ErrSignature
	}
	// The bytes are now known to be the owner's. Only now are they read.
	doc, err := parseDocument(payload)
	if err != nil {
		return Verified{}, err
	}
	if len(doc.Signers) != 1 {
		return Verified{}, fmt.Errorf("%w: a list naming %d signers (only a single signer is supported)", ErrUnsupported, len(doc.Signers))
	}
	named, _ := peerkey.ParsePublicKey(doc.Signers[0])
	if !bytes.Equal(named, signer) {
		// The recovery path: the owner's current key is gone and the list is
		// signed by the pre-pinned next key, which the list must then name.
		if doc.NextSigner == "" || peerkey.FormatPublicKey(signer) != doc.NextSigner {
			return Verified{}, fmt.Errorf("%w: the signing key is not the one the list names", ErrUnknownSigner)
		}
	}
	if doc.Fleet != v.Fleet {
		return Verified{}, ErrFleet
	}
	if doc.Seq < v.MinSeq {
		return Verified{}, fmt.Errorf("%w (seq %d, already accepted %d)", ErrRollback, doc.Seq, v.MinSeq)
	}
	return Verified{Document: doc, Signer: signer, Recovery: pinned > 0}, nil
}

func signedBytes(payload []byte) []byte {
	return append([]byte(SigningPrefix), payload...)
}

// Marshal is how a document becomes payload bytes: compact JSON in a fixed field
// order, machines and issuers sorted by id so that the same content always gives
// the same bytes. A verifier does not depend on this — it reads whatever bytes
// were signed — but a deterministic signer makes lists diff-able and gives the
// reference document its test vectors.
func Marshal(doc Document) ([]byte, error) {
	if doc.Format == "" {
		doc.Format = Format
	}
	if err := Validate(doc); err != nil {
		return nil, err
	}
	doc.Machines = append([]Machine(nil), doc.Machines...)
	sort.Slice(doc.Machines, func(i, j int) bool { return doc.Machines[i].ID < doc.Machines[j].ID })
	for i := range doc.Machines {
		if doc.Machines[i].Addresses == nil {
			doc.Machines[i].Addresses = []string{}
		}
		if doc.Machines[i].Grants == nil {
			doc.Machines[i].Grants = []string{}
		}
	}
	doc.Issuers = append([]Issuer(nil), doc.Issuers...)
	sort.Slice(doc.Issuers, func(i, j int) bool { return doc.Issuers[i].ID < doc.Issuers[j].ID })
	if doc.Issuers == nil {
		doc.Issuers = []Issuer{}
	}
	if doc.Machines == nil {
		doc.Machines = []Machine{}
	}
	return json.Marshal(doc)
}

// Sign wraps a document in a signed envelope. The key is the owner's, kept
// offline: this is the only place it is used, and it is not read by the service.
func Sign(doc Document, key ed25519.PrivateKey) ([]byte, error) {
	payload, err := Marshal(doc)
	if err != nil {
		return nil, err
	}
	pub := key.Public().(ed25519.PublicKey)
	named := false
	for _, s := range doc.Signers {
		if k, _ := peerkey.ParsePublicKey(s); bytes.Equal(k, pub) {
			named = true
		}
	}
	if !named && peerkey.FormatPublicKey(pub) != doc.NextSigner {
		return nil, errors.New("machine list: the signing key is neither a listed signer nor the next signer")
	}
	env := Envelope{
		Format:    Format,
		Payload:   base64.RawURLEncoding.EncodeToString(payload),
		Signer:    peerkey.FormatPublicKey(pub),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, signedBytes(payload))),
	}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ReadSeq returns the highest seq accepted in dir, or 0 when none has been.
func ReadSeq(dir string) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(dir, SeqFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: not a sequence number", filepath.Join(dir, SeqFile))
	}
	return n, nil
}

// WriteSeq records seq as the highest accepted. It never lowers the mark.
func WriteSeq(dir string, seq uint64) error {
	cur, err := ReadSeq(dir)
	if err != nil {
		return err
	}
	if seq <= cur {
		return nil
	}
	if err := privfs.MkdirAll(dir); err != nil {
		return err
	}
	tmp, err := privfs.CreateTemp(dir, ".seq.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strconv.FormatUint(seq, 10) + "\n"); err != nil {
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
	return os.Rename(tmp.Name(), filepath.Join(dir, SeqFile))
}
