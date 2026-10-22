package peerkey

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// muster #350: the signature itself, with no service around it.

func newKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, priv, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "machine.key")
	if err := SavePrivate(path, priv); err != nil {
		t.Fatal(err)
	}
	return pub, path
}

// signed builds a request the way a peer would send it and the same request as
// a server would receive it (RequestURI is set only on the server side).
func signed(t *testing.T, s *Signer, method, uri string, body []byte, target string, extra map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, uri, bytes.NewReader(body))
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	if err := s.Sign(req, body, target); err != nil {
		t.Fatal(err)
	}
	return req
}

func verify(t *testing.T, r *http.Request, body []byte, self string, keys []ed25519.PublicKey, now time.Time, c *ReplayCache) error {
	t.Helper()
	h, err := ParseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return err
	}
	_, err = Verify(h, r, body, self, keys, now, c)
	return err
}

func TestSignedRequestIsAccepted(t *testing.T) {
	pub, path := newKey(t)
	s, err := NewSigner(path, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"text":"hi"}`)
	r := signed(t, s, "POST", "/v1/machines/beta/sessions/s1/input?x=1", body, "beta", map[string]string{"Fleet-On-Behalf-Of": "someone"})
	if err := verify(t, r, body, "beta", []ed25519.PublicKey{pub}, time.Now(), NewReplayCache(10)); err != nil {
		t.Fatal(err)
	}
}

func TestRefusals(t *testing.T) {
	pub, path := newKey(t)
	otherPub, _ := newKey(t)
	s, _ := NewSigner(path, "alpha")
	body := []byte(`{"text":"hi"}`)
	keys := []ed25519.PublicKey{pub}
	const uri = "/v1/machines/beta/sessions/s1/input"

	cases := []struct {
		name   string
		mutate func(r *http.Request) []byte // returns the body to verify against
		self   string
		keys   []ed25519.PublicKey
		at     time.Time
		want   error
	}{
		{"wrong target", func(r *http.Request) []byte { return body }, "gamma", keys, time.Now(), ErrBadSig},
		{"wrong key", func(r *http.Request) []byte { return body }, "beta", []ed25519.PublicKey{otherPub}, time.Now(), ErrBadSig},
		{"tampered body", func(r *http.Request) []byte { return []byte(`{"text":"HI"}`) }, "beta", keys, time.Now(), ErrDigest},
		{"tampered digest header to match", func(r *http.Request) []byte {
			b := []byte(`{"text":"HI"}`)
			r.Header.Set(DigestHeader, "sha256="+BodyDigest(b))
			return b
		}, "beta", keys, time.Now(), ErrBadSig},
		{"tampered path", func(r *http.Request) []byte { r.RequestURI = "/v1/machines/beta/sessions/s2/input"; return body }, "beta", keys, time.Now(), ErrBadSig},
		{"tampered method", func(r *http.Request) []byte { r.Method = "DELETE"; return body }, "beta", keys, time.Now(), ErrBadSig},
		{"swapped on-behalf-of", func(r *http.Request) []byte { r.Header.Set("Fleet-On-Behalf-Of", "attacker"); return body }, "beta", keys, time.Now(), ErrBadSig},
		{"injected human-relay assertion", func(r *http.Request) []byte { r.Header.Set("Fleet-Human-Relay", "1"); return body }, "beta", keys, time.Now(), ErrBadSig},
		{"expired", func(r *http.Request) []byte { return body }, "beta", keys, time.Now().Add(MaxSkew + 5*time.Second), ErrExpired},
		{"from the future", func(r *http.Request) []byte { return body }, "beta", keys, time.Now().Add(-MaxSkew - 5*time.Second), ErrExpired},
		{"no keys", func(r *http.Request) []byte { return body }, "beta", nil, time.Now(), ErrBadSig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := signed(t, s, "POST", uri, body, "beta", map[string]string{"Fleet-On-Behalf-Of": "someone"})
			b := tc.mutate(r)
			err := verify(t, r, b, tc.self, tc.keys, tc.at, NewReplayCache(10))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestExpiryNamesTheClocks(t *testing.T) {
	pub, path := newKey(t)
	s, _ := NewSigner(path, "alpha")
	r := signed(t, s, "GET", "/v1/health", nil, "beta", nil)
	err := verify(t, r, nil, "beta", []ed25519.PublicKey{pub}, time.Now().Add(time.Hour), NewReplayCache(10))
	if !errors.Is(err, ErrExpired) || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("err = %v", err)
	}
}

func TestReplayIsRefused(t *testing.T) {
	pub, path := newKey(t)
	s, _ := NewSigner(path, "alpha")
	r := signed(t, s, "GET", "/v1/health", nil, "beta", nil)
	cache := NewReplayCache(10)
	keys := []ed25519.PublicKey{pub}
	if err := verify(t, r, nil, "beta", keys, time.Now(), cache); err != nil {
		t.Fatal(err)
	}
	if err := verify(t, r, nil, "beta", keys, time.Now(), cache); !errors.Is(err, ErrReplay) {
		t.Fatalf("second use: %v", err)
	}
}

// A signature that does not verify must not spend a nonce, or an attacker who
// cannot sign could fill the cache and lock real peers out.
func TestForgeryDoesNotFillTheReplayCache(t *testing.T) {
	_, path := newKey(t)
	otherPub, _ := newKey(t)
	s, _ := NewSigner(path, "alpha")
	cache := NewReplayCache(2)
	for i := 0; i < 5; i++ {
		r := signed(t, s, "GET", "/v1/health", nil, "beta", nil)
		if err := verify(t, r, nil, "beta", []ed25519.PublicKey{otherPub}, time.Now(), cache); !errors.Is(err, ErrBadSig) {
			t.Fatalf("forged: %v", err)
		}
	}
	if len(cache.seen) != 0 {
		t.Fatalf("cache holds %d nonces from signatures that never verified", len(cache.seen))
	}
}

func TestReplayCacheFailsClosedWhenFull(t *testing.T) {
	c := NewReplayCache(2)
	now := time.Now()
	if err := c.Use("a", "1", now); err != nil {
		t.Fatal(err)
	}
	if err := c.Use("a", "2", now); err != nil {
		t.Fatal(err)
	}
	if err := c.Use("a", "3", now); !errors.Is(err, ErrReplayFull) {
		t.Fatalf("a full cache must refuse, not forget: %v", err)
	}
	// Once the window has passed the room comes back.
	if err := c.Use("a", "3", now.Add(3*MaxSkew)); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestNonceIsScopedToItsSender(t *testing.T) {
	c := NewReplayCache(10)
	now := time.Now()
	if err := c.Use("a", "n", now); err != nil {
		t.Fatal(err)
	}
	if err := c.Use("b", "n", now); err != nil {
		t.Fatalf("another sender's identical nonce: %v", err)
	}
}

func TestParseAuthorizationRejectsJunk(t *testing.T) {
	for _, v := range []string{
		"", "Bearer abc", "Muster-Sig", "Muster-Sig v2 machine=a, ts=1, nonce=n, sig=AA",
		"Muster-Sig v1 machine=a, ts=x, nonce=n, sig=AA",
		"Muster-Sig v1 machine=a, ts=1, nonce=n",
		"Muster-Sig v1 machine=a, machine=b, ts=1, nonce=n, sig=AA",
		"Muster-Sig v1 machine=a, ts=1, nonce=n, sig=AA, extra=1",
	} {
		if _, err := ParseAuthorization(v); err == nil {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestPublicKeyRoundTripAndRejects(t *testing.T) {
	pub, _ := newKey(t)
	got, err := ParsePublicKey(FormatPublicKey(pub))
	if err != nil || !got.Equal(pub) {
		t.Fatalf("round trip: %v %v", got, err)
	}
	for _, bad := range []string{"", "abc", "ed25519:", "ed25519:AAAA", "rsa:" + strings.Repeat("A", 43)} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPrivateKeyFileIsOwnerOnlyAndNeverEchoed(t *testing.T) {
	_, path := newKey(t)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", info.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPrivate(path)
	if err == nil || strings.Contains(err.Error(), "garbage") || strings.Contains(err.Error(), strings.TrimSpace(string(raw))) {
		t.Fatalf("err = %v", err)
	}
}

// The signer notices a key file replaced under it (a rotation), without a restart.
func TestSignerPicksUpAReplacedKeyFile(t *testing.T) {
	pub1, path := newKey(t)
	s, _ := NewSigner(path, "alpha")
	got, _ := s.Public()
	if !got.Equal(pub1) {
		t.Fatal("initial key")
	}
	pub2, priv2, _ := Generate()
	if err := SavePrivate(path, priv2); err != nil {
		t.Fatal(err)
	}
	// Defeat a filesystem whose mtime is too coarse to tell two writes apart.
	_ = os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	got, _ = s.Public()
	if !got.Equal(pub2) {
		t.Fatal("the signer kept using the replaced key")
	}
}
