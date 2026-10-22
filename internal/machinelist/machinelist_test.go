package machinelist

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/futurelastic/muster/internal/peerkey"
)

// seedKey is a deterministic key for tests and for the reference document's
// vectors: ed25519 signatures are deterministic, so the same document under the
// same seed always gives the same bytes.
func seedKey(b byte) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = b
	}
	return ed25519.NewKeyFromSeed(seed)
}

func pub(k ed25519.PrivateKey) string {
	return peerkey.FormatPublicKey(k.Public().(ed25519.PublicKey))
}

func sampleDoc(owner ed25519.PrivateKey, seq uint64) Document {
	return Document{
		Fleet: "fleet-1",
		Seq:   seq,
		Machines: []Machine{
			{ID: "machine-b", PublicKey: pub(seedKey(0x22)), Addresses: []string{"http://10.8.0.12:9000"}, Grants: []string{"read", "create"}},
			{ID: "machine-a", PublicKey: pub(seedKey(0x11)), Addresses: []string{"http://10.8.0.11:9000", "http://192.168.1.11:9000"}, Grants: []string{"read", "send", "relay"}},
		},
		Issuers: []Issuer{{ID: "sso", PublicKey: pub(seedKey(0x33))}},
		Signers: []string{pub(owner)},
	}
}

func verifier(owner ed25519.PrivateKey, minSeq uint64) Verifier {
	return Verifier{Fleet: "fleet-1", Signers: []ed25519.PublicKey{owner.Public().(ed25519.PublicKey)}, MinSeq: minSeq}
}

func mustSign(t *testing.T, doc Document, key ed25519.PrivateKey) []byte {
	t.Helper()
	raw, err := Sign(doc, key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRoundTrip(t *testing.T) {
	owner := seedKey(0x01)
	raw := mustSign(t, sampleDoc(owner, 3), owner)
	got, err := Verify(raw, verifier(owner, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got.Document.Seq != 3 || len(got.Document.Machines) != 2 || got.Document.Machines[0].ID != "machine-a" {
		t.Fatalf("document = %+v", got.Document)
	}
	if got.Recovery {
		t.Fatal("the first pinned signer is not a recovery")
	}
	// Deterministic: the same document under the same key is the same bytes.
	if again := mustSign(t, sampleDoc(owner, 3), owner); string(again) != string(raw) {
		t.Fatal("signing is not deterministic")
	}
}

func TestRollbackRefused(t *testing.T) {
	owner := seedKey(0x01)
	old := mustSign(t, sampleDoc(owner, 2), owner)
	if _, err := Verify(old, verifier(owner, 5)); !errors.Is(err, ErrRollback) {
		t.Fatalf("an older genuine list: err = %v, want ErrRollback", err)
	}
	// The mark itself is accepted: reloading the same file is not a rollback.
	same := mustSign(t, sampleDoc(owner, 5), owner)
	if _, err := Verify(same, verifier(owner, 5)); err != nil {
		t.Fatalf("seq equal to the mark: %v", err)
	}
}

func TestSpliceAndPartialEditRefused(t *testing.T) {
	owner := seedKey(0x01)
	raw := mustSign(t, sampleDoc(owner, 3), owner)

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(env.Payload)

	cases := map[string]func(Envelope, []byte) Envelope{
		"a field edited in the payload": func(e Envelope, p []byte) Envelope {
			e.Payload = base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(p), "machine-b", "machine-z", 1)))
			return e
		},
		"an address changed": func(e Envelope, p []byte) Envelope {
			e.Payload = base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(p), "10.8.0.12", "10.8.0.99", 1)))
			return e
		},
		"seq raised": func(e Envelope, p []byte) Envelope {
			e.Payload = base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(p), `"seq":3`, `"seq":9`, 1)))
			return e
		},
		"a trailing byte": func(e Envelope, p []byte) Envelope {
			e.Payload = base64.RawURLEncoding.EncodeToString(append(append([]byte{}, p...), ' '))
			return e
		},
		"the signature of another genuine list": func(e Envelope, p []byte) Envelope {
			other := mustSign(t, sampleDoc(seedKey(0x01), 4), seedKey(0x01))
			var oe Envelope
			_ = json.Unmarshal(other, &oe)
			e.Signature = oe.Signature
			return e
		},
		"a payload spliced from another genuine list": func(e Envelope, p []byte) Envelope {
			other := mustSign(t, sampleDoc(seedKey(0x01), 4), seedKey(0x01))
			var oe Envelope
			_ = json.Unmarshal(other, &oe)
			e.Payload = oe.Payload
			return e
		},
		"a truncated signature": func(e Envelope, p []byte) Envelope {
			e.Signature = e.Signature[:len(e.Signature)-4]
			return e
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			bad, _ := json.Marshal(edit(env, payload))
			if _, err := Verify(bad, verifier(owner, 0)); err == nil {
				t.Fatal("an edited list verified")
			}
		})
	}
}

func TestUnknownSignerRefused(t *testing.T) {
	owner, stranger := seedKey(0x01), seedKey(0x02)
	// Perfectly signed, by a key nobody here pinned — even though the document
	// names that same key as its signer. The document does not choose its signer.
	raw := mustSign(t, sampleDoc(stranger, 3), stranger)
	if _, err := Verify(raw, verifier(owner, 0)); !errors.Is(err, ErrUnknownSigner) {
		t.Fatalf("err = %v, want ErrUnknownSigner", err)
	}
}

func TestWrongFleetRefused(t *testing.T) {
	owner := seedKey(0x01)
	doc := sampleDoc(owner, 3)
	doc.Fleet = "fleet-2"
	if _, err := Verify(mustSign(t, doc, owner), verifier(owner, 0)); !errors.Is(err, ErrFleet) {
		t.Fatalf("err = %v, want ErrFleet", err)
	}
}

func TestMultiSignerNotSupportedYet(t *testing.T) {
	owner := seedKey(0x01)
	doc := sampleDoc(owner, 3)
	doc.Signers = append(doc.Signers, pub(seedKey(0x04)))
	_, err := Verify(mustSign(t, doc, owner), verifier(owner, 0))
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestRecoveryByNextSigner(t *testing.T) {
	owner, next := seedKey(0x01), seedKey(0x05)
	doc := sampleDoc(owner, 7)
	doc.NextSigner = pub(next)
	// The current key is lost; the owner signs with the pre-pinned next key.
	raw := mustSign(t, doc, next)
	v := Verifier{Fleet: "fleet-1", Signers: []ed25519.PublicKey{owner.Public().(ed25519.PublicKey), next.Public().(ed25519.PublicKey)}}
	got, err := Verify(raw, v)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Recovery {
		t.Fatal("a list signed by the second pinned key is a recovery")
	}
	// A verifier that did not pin the next key refuses it.
	if _, err := Verify(raw, verifier(owner, 0)); !errors.Is(err, ErrUnknownSigner) {
		t.Fatalf("unpinned next signer: err = %v", err)
	}
	// And the document must itself name it as next: pinning is not enough.
	doc.NextSigner = ""
	if _, err := Sign(doc, next); err == nil {
		t.Fatal("signed by a key the document does not name")
	}
}

func TestShapeRefused(t *testing.T) {
	owner := seedKey(0x01)
	mut := map[string]func(*Document){
		"no fleet":        func(d *Document) { d.Fleet = "" },
		"seq zero":        func(d *Document) { d.Seq = 0 },
		"duplicate id":    func(d *Document) { d.Machines[1].ID = d.Machines[0].ID },
		"bad machine key": func(d *Document) { d.Machines[0].PublicKey = "ed25519:AAAA" },
		"no signers":      func(d *Document) { d.Signers = nil },
		"bad issuer key":  func(d *Document) { d.Issuers[0].PublicKey = "nope" },
		"empty address":   func(d *Document) { d.Machines[0].Addresses = []string{" "} },
	}
	for name, f := range mut {
		t.Run(name, func(t *testing.T) {
			d := sampleDoc(owner, 1)
			f(&d)
			if _, err := Sign(d, owner); !errors.Is(err, ErrFormat) {
				t.Fatalf("err = %v, want ErrFormat", err)
			}
		})
	}
	// Unknown fields and junk are refused on the way in.
	for _, raw := range []string{``, `{}`, `[]`, `{"format":"x"}`, `{"format":"muster-machine-list/1","payload":"!!","signer":"","signature":""}`, `{"format":"muster-machine-list/1","payload":"e30","signer":"","signature":"","extra":1}`} {
		if _, err := Verify([]byte(raw), verifier(owner, 0)); err == nil {
			t.Fatalf("%q verified", raw)
		}
	}
}

func TestSeqStore(t *testing.T) {
	dir := t.TempDir()
	if n, err := ReadSeq(dir); err != nil || n != 0 {
		t.Fatalf("fresh: %d, %v", n, err)
	}
	if err := WriteSeq(dir, 4); err != nil {
		t.Fatal(err)
	}
	if err := WriteSeq(dir, 2); err != nil { // never lowers
		t.Fatal(err)
	}
	if n, _ := ReadSeq(dir); n != 4 {
		t.Fatalf("high-water = %d, want 4", n)
	}
}

// TestReferenceVector pins the vector printed in docs/machine-list.md. If this
// changes, the document must change with it, because other programs verify
// against it.
func TestReferenceVector(t *testing.T) {
	owner := seedKey(0x01)
	doc := Document{
		Fleet: "example-fleet",
		Seq:   1,
		Machines: []Machine{{
			ID: "machine-a", PublicKey: pub(seedKey(0x11)),
			Addresses: []string{"http://10.8.0.11:9000"}, Grants: []string{"read"},
		}},
		Issuers: []Issuer{},
		Signers: []string{pub(owner)},
	}
	payload, err := Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	raw := mustSign(t, doc, owner)
	var env Envelope
	_ = json.Unmarshal(raw, &env)
	t.Logf("signer      %s", env.Signer)
	t.Logf("payload     %s", payload)
	t.Logf("signature   %s", env.Signature)
	for _, c := range [][2]string{
		{vectorSigner, env.Signer},
		{vectorPayload, string(payload)},
		{vectorSignature, env.Signature},
	} {
		if c[0] != c[1] {
			t.Fatalf("reference vector drifted:\n want %s\n  got %s", c[0], c[1])
		}
	}
	if _, err := Verify(raw, verifier(owner, 0)); err == nil {
		t.Fatal("the vector fleet is example-fleet; verifier pinned fleet-1 must refuse")
	}
	v := Verifier{Fleet: "example-fleet", Signers: []ed25519.PublicKey{owner.Public().(ed25519.PublicKey)}}
	if _, err := Verify(raw, v); err != nil {
		t.Fatal(err)
	}
}

// The vector's expected values, the seed is thirty-two bytes of 0x01.
const (
	vectorSigner    = "ed25519:iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w"
	vectorPayload   = `{"format":"muster-machine-list/1","fleet":"example-fleet","seq":1,"machines":[{"id":"machine-a","publicKey":"ed25519:0EqyMnQrtKs6E2i9RhXk5tAiSrcaAWuvhSCjMsl3hzc","addresses":["http://10.8.0.11:9000"],"grants":["read"]}],"issuers":[],"signers":["ed25519:iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w"]}`
	vectorSignature = "c6xQZfPYQ_KaZCXa_IFNBg1NvdP94I-qL1AkrfaK0UFtrpN_kI4MCmK9UpPFirj4d8HVreFOU6JcPM1YXasKAQ"
)
