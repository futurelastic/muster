package grant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	s.lockWait = 300 * time.Millisecond
	s.lockStale = 200 * time.Millisecond
	return s
}

func sampleGrant(now time.Time, life time.Duration) Grant {
	return Grant{
		Refresh:      "R1",
		Access:       "A1",
		ExpiresAtMs:  now.Add(life).UnixMilli(),
		AccountID:    "acct",
		SignedInAtMs: now.UnixMilli(),
	}
}

func TestStoreIsOwnerOnlyAndReplaceBumpsTheGeneration(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	if _, err := s.Load(); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("empty store: want ErrNoGrant, got %v", err)
	}
	g1, err := s.Replace(sampleGrant(now, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if g1.Generation != 1 {
		t.Fatalf("first sign-in generation = %d, want 1", g1.Generation)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(s.Path())
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("store mode %#o is not owner-only", perm)
		}
		dinfo, _ := os.Stat(filepath.Dir(s.Path()))
		if perm := dinfo.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("store directory mode %#o is not owner-only", perm)
		}
	}
	if rep, ok := s.Check(); !ok || rep.Verdict.String() != "owner-only" {
		t.Fatalf("Check = %v %v, want owner-only", rep, ok)
	}
	// A revocation mark is cleared by the next sign-in.
	if _, ok, err := s.Update(1, func(g *Grant) { g.Revoked = &Revoked{Status: 401} }); err != nil || !ok {
		t.Fatalf("Update: %v %v", err, ok)
	}
	g2, err := s.Replace(sampleGrant(now, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if g2.Generation != 2 || g2.Revoked != nil {
		t.Fatalf("second sign-in = gen %d revoked %v, want gen 2 and no revocation", g2.Generation, g2.Revoked)
	}
	// No temporary or lock file is left behind.
	entries, _ := os.ReadDir(filepath.Dir(s.Path()))
	for _, e := range entries {
		if e.Name() != "openai.json" {
			t.Fatalf("stray file %q left in the store directory", e.Name())
		}
	}
}

func TestStoreUpdateRefusesAStaleGeneration(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	if _, err := s.Replace(sampleGrant(now, time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(sampleGrant(now, 2*time.Hour)); err != nil { // generation 2
		t.Fatal(err)
	}
	_, ok, err := s.Update(1, func(g *Grant) { g.Access = "STALE" })
	if err != nil || ok {
		t.Fatalf("Update on a stale generation = %v %v, want false with no error", ok, err)
	}
	g, _ := s.Load()
	if g.Access == "STALE" {
		t.Fatal("a stale update was written")
	}
}

func TestStoreBreaksALockLeftByADeadProcess(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.lockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(s.lockPath(), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(sampleGrant(time.Now(), time.Hour)); err != nil {
		t.Fatalf("a stale lock blocked the write: %v", err)
	}
}

func TestStoreRefusesAGarbledFileRatherThanReadingItAsSignedOut(t *testing.T) {
	s := newTestStore(t)
	_ = os.MkdirAll(filepath.Dir(s.Path()), 0o700)
	_ = os.WriteFile(s.Path(), []byte("{not json"), 0o600)
	if _, err := s.Load(); err == nil || errors.Is(err, ErrNoGrant) {
		t.Fatalf("garbled store: want a real error, got %v", err)
	}
}

// fakeProvider is a token endpoint a test controls.
type fakeProvider struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls int
	// token answers /oauth/token; nil means a rotating success.
	token func(form map[string]string, call int) (int, string)
	// device endpoints
	pendingPolls int
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		p.mu.Lock()
		p.calls++
		n := p.calls
		fn := p.token
		p.mu.Unlock()
		status, body := 200, ""
		if fn != nil {
			status, body = fn(form, n)
		} else {
			body = `{"access_token":"A` + strconv.Itoa(n+1) + `","refresh_token":"R` + strconv.Itoa(n+1) + `","expires_in":3600}`
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_auth_id":"dev-1","user_code":"ABCD-1234","interval":"1"}`))
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		pending := p.pendingPolls
		if pending > 0 {
			p.pendingPolls--
		}
		p.mu.Unlock()
		if pending > 0 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"authorization_code":"code-1","code_verifier":"verifier-1"}`))
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProvider) client() *Client {
	return &Client{Issuer: p.srv.URL, ClientID: "test-client", HTTP: p.srv.Client(),
		Sleep: func(context.Context, time.Duration) error { return nil }}
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestRefreshClassifiesTheProvidersAnswer(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		rejected bool
	}{
		{"401 is the grant's end", 401, `{"error":{"message":"Could not validate your token.","type":"invalid_request_error","code":"token_expired"}}`, true},
		{"401 with no code is still the grant's end", 401, `nope`, true},
		{"400 invalid_grant is the grant's end", 400, `{"error":"invalid_grant"}`, true},
		{"403 refresh_token_reused is the grant's end", 403, `{"error":{"code":"refresh_token_reused"}}`, true},
		{"400 that names no grant failure is not", 400, `{"error":{"code":"invalid_request"}}`, false},
		{"429 is throttling", 429, ``, false},
		{"500 is the provider's trouble", 500, `oops`, false},
		{"404 is a wrong address, not a revoked grant", 404, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newFakeProvider(t)
			p.token = func(map[string]string, int) (int, string) { return tc.status, tc.body }
			_, err := p.client().Refresh(context.Background(), "R")
			var rej *RejectedError
			var tr *TransientError
			switch {
			case tc.rejected && !errors.As(err, &rej):
				t.Fatalf("want RejectedError, got %T %v", err, err)
			case !tc.rejected && !errors.As(err, &tr):
				t.Fatalf("want TransientError, got %T %v", err, err)
			}
		})
	}
}

func TestRefreshSendsTheRefreshTokenAndKeepsTheOldOneWhenNoneIsReturned(t *testing.T) {
	p := newFakeProvider(t)
	var got map[string]string
	p.token = func(f map[string]string, _ int) (int, string) {
		got = f
		return 200, `{"access_token":"A2","expires_in":120}`
	}
	tok, err := p.client().Refresh(context.Background(), "R-old")
	if err != nil {
		t.Fatal(err)
	}
	if got["grant_type"] != "refresh_token" || got["refresh_token"] != "R-old" || got["client_id"] != "test-client" {
		t.Fatalf("request form = %v", got)
	}
	if tok.Refresh != "R-old" || tok.Access != "A2" || tok.ExpiresIn != 2*time.Minute {
		t.Fatalf("tokens = %+v", tok)
	}
}

func TestAnUnreachableProviderIsTransientAndNamesNoAddress(t *testing.T) {
	p := newFakeProvider(t)
	c := p.client()
	p.srv.Close()
	_, err := c.Refresh(context.Background(), "R")
	var tr *TransientError
	if !errors.As(err, &tr) {
		t.Fatalf("want TransientError, got %T %v", err, err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the error names the address: %v", err)
	}
}

func jwt(claims map[string]any) string {
	raw, _ := json.Marshal(claims)
	return "h." + base64.RawURLEncoding.EncodeToString(raw) + ".s"
}

func TestAccountIDIsReadFromEitherToken(t *testing.T) {
	if got := accountID(jwt(map[string]any{"chatgpt_account_id": "A"})); got != "A" {
		t.Fatalf("flat claim = %q", got)
	}
	if got := accountID("", jwt(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "B"}})); got != "B" {
		t.Fatalf("namespaced claim = %q", got)
	}
	if got := accountID(jwt(map[string]any{"organizations": []map[string]string{{"id": "C"}}})); got != "C" {
		t.Fatalf("organization = %q", got)
	}
	if got := accountID("not-a-jwt", "a.b"); got != "" {
		t.Fatalf("garbage = %q", got)
	}
}

func TestDeviceSignInWaitsForApprovalThenExchanges(t *testing.T) {
	p := newFakeProvider(t)
	p.pendingPolls = 2
	var exchange map[string]string
	p.token = func(f map[string]string, _ int) (int, string) {
		exchange = f
		return 200, `{"access_token":"A1","refresh_token":"R1","expires_in":3600,"id_token":"` + jwt(map[string]any{"chatgpt_account_id": "acct-9"}) + `"}`
	}
	c := p.client()
	var slept []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	dev, err := c.StartDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dev.Code != "ABCD-1234" || !strings.HasSuffix(dev.URL, "/codex/device") {
		t.Fatalf("device = %+v", dev)
	}
	tok, err := c.PollDevice(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(slept) != 2 || slept[0] != 4*time.Second {
		t.Fatalf("polled with waits %v, want two of 4s (interval 1s + 3s)", slept)
	}
	if exchange["grant_type"] != "authorization_code" || exchange["code"] != "code-1" || exchange["code_verifier"] != "verifier-1" ||
		!strings.HasSuffix(exchange["redirect_uri"], "/deviceauth/callback") {
		t.Fatalf("exchange form = %v", exchange)
	}
	if tok.Refresh != "R1" || tok.Access != "A1" || tok.AccountID != "acct-9" {
		t.Fatalf("tokens = %+v", tok)
	}
}

func TestDeviceSignInStopsWhenTheContextEnds(t *testing.T) {
	p := newFakeProvider(t)
	p.pendingPolls = 1 << 20
	c := p.client()
	ctx, cancel := context.WithCancel(context.Background())
	c.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	dev, err := c.StartDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollDevice(ctx, dev); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// refresherFixture is a refresher over a fake provider with a movable clock.
type refresherFixture struct {
	r     *Refresher
	s     *Store
	p     *fakeProvider
	clock *atomic.Int64 // unix ms
	logs  *[]string
}

func newRefresherFixture(t *testing.T, life time.Duration) *refresherFixture {
	t.Helper()
	p := newFakeProvider(t)
	s := newTestStore(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixMilli())
	now := func() time.Time { return time.UnixMilli(clock.Load()) }
	var logs []string
	var mu sync.Mutex
	r := NewRefresher(s, p.client(), WithClock(now), WithLogf(func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, f)
	}))
	r.poll, r.minGap, r.backoffBase, r.backoffMax = 10*time.Minute, time.Second, 10*time.Second, 80*time.Second
	if _, err := s.Replace(sampleGrant(now(), life)); err != nil {
		t.Fatal(err)
	}
	return &refresherFixture{r: r, s: s, p: p, clock: &clock, logs: &logs}
}

func (f *refresherFixture) advance(d time.Duration) { f.clock.Add(d.Milliseconds()) }

func TestRefresherWaitsUntilAQuarterOfTheLifetimeIsLeft(t *testing.T) {
	f := newRefresherFixture(t, 8*time.Hour) // refresh at 6h
	wait := f.r.step(context.Background())
	if f.p.callCount() != 0 {
		t.Fatal("refreshed a fresh token")
	}
	if wait != f.r.poll {
		t.Fatalf("wait = %v, want the poll interval", wait)
	}
	f.advance(5*time.Hour + 59*time.Minute)
	f.r.step(context.Background())
	if f.p.callCount() != 0 {
		t.Fatal("refreshed before the renewal point")
	}
	f.advance(2 * time.Minute)
	f.r.step(context.Background())
	if f.p.callCount() != 1 {
		t.Fatalf("calls = %d, want 1 at the renewal point", f.p.callCount())
	}
}

func TestRefresherPersistsTheRotatedTokenBeforeAnySubscriberHearsOfIt(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	var sawInStore []string
	f.r.Subscribe(func(a Access) {
		g, err := f.s.Load()
		if err != nil {
			t.Errorf("subscriber could not read the store: %v", err)
			return
		}
		sawInStore = append(sawInStore, g.Refresh+"/"+g.Access+"/"+a.Token)
	})
	f.advance(50 * time.Minute) // past the 45-minute renewal point
	f.r.step(context.Background())
	if len(sawInStore) != 1 {
		t.Fatalf("subscriber called %d times, want 1", len(sawInStore))
	}
	if sawInStore[0] != "R2/A2/A2" {
		t.Fatalf("at the moment the subscriber ran, store/handed token = %q, want R2/A2/A2 (rotated refresh already stored)", sawInStore[0])
	}
}

func TestRefresherHoldsARotatedGrantInMemoryWhenTheStoreCannotTakeIt(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	f.advance(50 * time.Minute)
	// Make the store unwritable by taking write permission off its directory.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory modes do not stop this account writing")
	}
	dir := filepath.Dir(f.s.Path())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make the directory read-only here")
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var heard []string
	f.r.Subscribe(func(a Access) { heard = append(heard, a.Token) })
	f.r.step(context.Background())
	if f.p.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", f.p.callCount())
	}
	if f.r.unsaved == nil || f.r.unsaved.Refresh != "R2" {
		t.Fatalf("the rotated grant was not kept in memory: %+v", f.r.unsaved)
	}
	// The directory comes back: the next pass writes it.
	_ = os.Chmod(dir, 0o700)
	f.r.step(context.Background())
	g, err := f.s.Load()
	if err != nil || g.Refresh != "R2" {
		t.Fatalf("after recovery the store holds %+v (%v), want refresh R2", g, err)
	}
	if f.r.unsaved != nil {
		t.Fatal("unsaved was not cleared once the store took it")
	}
}

func TestRefresherMarksTheGrantRevokedAndStopsUsingTheDeadToken(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	f.p.token = func(map[string]string, int) (int, string) {
		return 401, `{"error":{"code":"token_expired"}}`
	}
	f.advance(50 * time.Minute)
	f.r.step(context.Background())
	if f.p.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", f.p.callCount())
	}
	st := f.r.State()
	if st.Phase != PhaseRevoked || !strings.Contains(st.Reason, "sign in again") {
		t.Fatalf("state = %+v, want revoked with a sign-in-again reason", st)
	}
	// It does not keep posting the dead token.
	f.advance(time.Hour)
	f.r.step(context.Background())
	if f.p.callCount() != 1 {
		t.Fatalf("a revoked grant was refreshed again (%d calls)", f.p.callCount())
	}
	if _, err := f.r.Access(context.Background()); err == nil {
		t.Fatal("Access handed out a token for a revoked grant")
	} else {
		var re *RevokedError
		if !errors.As(err, &re) {
			t.Fatalf("Access error = %T %v, want *RevokedError", err, err)
		}
	}
	// A new sign-in clears it and is passed on to subscribers on the next pass.
	var heard []string
	f.r.Subscribe(func(a Access) { heard = append(heard, a.Token) })
	g := sampleGrant(f.r.now(), time.Hour)
	g.Access, g.Refresh = "A-new", "R-new"
	if _, err := f.s.Replace(g); err != nil {
		t.Fatal(err)
	}
	f.p.token = nil
	f.r.step(context.Background())
	if st := f.r.State(); st.Phase != PhaseOK {
		t.Fatalf("after a new sign-in state = %+v, want ok", st)
	}
	if len(heard) != 1 || heard[0] != "A-new" {
		t.Fatalf("subscribers heard %v, want the new sign-in's access token", heard)
	}
}

func TestRefresherBacksOffOnATransientFailureAndNeverRevokes(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	f.p.token = func(map[string]string, int) (int, string) { return 503, `` }
	f.advance(50 * time.Minute)
	var waits []time.Duration
	for i := 0; i < 4; i++ {
		waits = append(waits, f.r.step(context.Background()))
		f.advance(waits[len(waits)-1])
	}
	if got := f.p.callCount(); got != 4 {
		t.Fatalf("calls = %d, want 4 (each retry after its backoff)", got)
	}
	// 10s, 20s, 40s, then the 80s cap is not reached yet; each wait is the next backoff.
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
	st := f.r.State()
	if st.Phase != PhaseDegraded || !strings.Contains(st.Reason, "503") {
		t.Fatalf("state = %+v, want degraded naming the status", st)
	}
	if g, _ := f.s.Load(); g.Revoked != nil {
		t.Fatal("a transient failure revoked the grant")
	}
	// Recovery clears the degraded state.
	f.p.token = nil
	f.advance(2 * time.Minute)
	f.r.step(context.Background())
	if st := f.r.State(); st.Phase != PhaseOK {
		t.Fatalf("after recovery state = %+v, want ok", st)
	}
}

func TestAccessRefreshesInlineWhenTheTokenIsAboutToLapse(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	a, err := f.r.Access(context.Background())
	if err != nil || a.Token != "A1" {
		t.Fatalf("Access = %+v %v, want the stored token", a, err)
	}
	if f.p.callCount() != 0 {
		t.Fatal("refreshed a fresh token")
	}
	f.advance(59*time.Minute + 30*time.Second) // inside accessGrace
	var heard []string
	f.r.Subscribe(func(a Access) { heard = append(heard, a.Token) })
	a, err = f.r.Access(context.Background())
	if err != nil || a.Token != "A2" {
		t.Fatalf("Access = %+v %v, want a renewed token A2", a, err)
	}
	if len(heard) != 1 || heard[0] != "A2" {
		t.Fatalf("subscribers heard %v, want [A2]", heard)
	}
}

func TestAccessStillHandsOutAValidTokenWhenTheProviderIsDown(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	f.p.token = func(map[string]string, int) (int, string) { return 503, `` }
	f.advance(59*time.Minute + 30*time.Second)
	a, err := f.r.Access(context.Background())
	if err != nil || a.Token != "A1" {
		t.Fatalf("Access = %+v %v, want the still-valid stored token", a, err)
	}
	f.advance(time.Minute) // now lapsed
	if _, err := f.r.Access(context.Background()); err == nil {
		t.Fatal("Access handed out a lapsed token while the provider is down")
	}
}

func TestAccessOnAnEmptyStoreIsSignedOut(t *testing.T) {
	r := NewRefresher(newTestStore(t), NewClient())
	if _, err := r.Access(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("want ErrSignedOut, got %v", err)
	}
	if st := r.State(); st.Phase != PhaseSignedOut {
		t.Fatalf("state = %+v", st)
	}
}

func TestASignInDuringARefreshWinsOverTheRefreshResult(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	f.advance(50 * time.Minute)
	f.p.token = func(map[string]string, int) (int, string) {
		// While the refresh is in flight the operator signs in again.
		g := sampleGrant(f.r.now(), 2*time.Hour)
		g.Access, g.Refresh = "A-signin", "R-signin"
		if _, err := f.s.Replace(g); err != nil {
			t.Error(err)
		}
		return 200, `{"access_token":"A-old-chain","refresh_token":"R-old-chain","expires_in":3600}`
	}
	var heard []string
	f.r.Subscribe(func(a Access) { heard = append(heard, a.Token) })
	f.r.step(context.Background())
	g, _ := f.s.Load()
	if g.Refresh != "R-signin" || g.Access != "A-signin" {
		t.Fatalf("store = %+v, the refresh result overwrote the newer sign-in", g)
	}
	if len(heard) != 0 {
		t.Fatalf("subscribers heard %v from a superseded refresh", heard)
	}
}

func TestRefreshAtReadsTheTokensOwnLifetime(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	g := Grant{SignedInAtMs: base.UnixMilli(), ExpiresAtMs: base.Add(8 * time.Hour).UnixMilli()}
	if got := refreshAt(g); !got.Equal(base.Add(6 * time.Hour)) {
		t.Fatalf("8h token renews at %v after start, want 6h", got.Sub(base))
	}
	g.ExpiresAtMs = base.Add(10 * 24 * time.Hour).UnixMilli() // a quarter would be 2.5 days; capped at 1 day
	if got := refreshAt(g); !got.Equal(base.Add(9 * 24 * time.Hour)) {
		t.Fatalf("10d token renews at %v after start, want 9d", got.Sub(base))
	}
	g.ExpiresAtMs = base.Add(10 * time.Minute).UnixMilli() // a quarter would be 2.5 min; floored at 5
	if got := refreshAt(g); !got.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("10m token renews at %v after start, want 5m", got.Sub(base))
	}
	// The span runs from the later of sign-in and last refresh.
	g.SignedInAtMs = base.Add(-100 * time.Hour).UnixMilli()
	g.RefreshedAtMs = base.UnixMilli()
	g.ExpiresAtMs = base.Add(8 * time.Hour).UnixMilli()
	if got := refreshAt(g); !got.Equal(base.Add(6 * time.Hour)) {
		t.Fatalf("renewal point ignored RefreshedAt: %v", got.Sub(base))
	}
}

// ---- the usage mark (muster #458) ----------------------------------------------

func TestNoteLimitKeepsWhenTheEpisodeBeganAndStampsEachSighting(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	began := f.r.now()
	if err := f.r.NoteLimit(began, "The usage limit has been reached"); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Minute)
	// A later call carries a later "since" (the driver's own clock may restart an episode
	// view) but the store keeps the first: it is when the account was first seen refused.
	if err := f.r.NoteLimit(f.r.now(), "still: "+strings.Repeat("x", 500)); err != nil {
		t.Fatal(err)
	}
	g, err := f.s.Load()
	if err != nil || g.Limit == nil {
		t.Fatalf("Load = %+v, %v", g, err)
	}
	if g.Limit.SinceMs != began.UnixMilli() {
		t.Fatalf("since = %d, want the first sighting %d", g.Limit.SinceMs, began.UnixMilli())
	}
	if g.Limit.UpdatedMs != f.r.now().UnixMilli() {
		t.Fatalf("updated = %d, want the latest sighting %d", g.Limit.UpdatedMs, f.r.now().UnixMilli())
	}
	if n := len([]rune(g.Limit.Message)); n > 200 {
		t.Fatalf("message is %d runes, want it cut", n)
	}
	// Writing the mark is not a sign-in: the generation a refresh checks is unchanged.
	if g.Generation != 1 {
		t.Fatalf("generation = %d, want 1", g.Generation)
	}
}

func TestClearLimitRemovesTheMarkAndNoneIsNotAnError(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	if err := f.r.ClearLimit(); err != nil {
		t.Fatalf("clearing nothing: %v", err)
	}
	if err := f.r.NoteLimit(f.r.now(), "m"); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ClearLimit(); err != nil {
		t.Fatal(err)
	}
	if g, _ := f.s.Load(); g.Limit != nil {
		t.Fatalf("limit = %+v after a clear", g.Limit)
	}
	// And with no sign-in stored there is no account to describe.
	empty := NewRefresher(newTestStore(t), nil)
	if err := empty.NoteLimit(time.Now(), "m"); err != nil {
		t.Fatalf("NoteLimit on an empty store: %v", err)
	}
}

func TestARefreshAndANewSignInTreatTheUsageMarkDifferently(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	if err := f.r.NoteLimit(f.r.now(), "m"); err != nil {
		t.Fatal(err)
	}
	// Renewing the tokens does not reset the plan's usage window.
	f.advance(50 * time.Minute)
	f.r.step(context.Background())
	if g, _ := f.s.Load(); g.Access != "A2" || g.Limit == nil {
		t.Fatalf("after a refresh: access %q, limit %+v; want the new tokens and the mark kept", g.Access, g.Limit)
	}
	// A new sign-in may be a different account.
	if _, err := f.s.Replace(sampleGrant(f.r.now(), time.Hour)); err != nil {
		t.Fatal(err)
	}
	if g, _ := f.s.Load(); g.Limit != nil {
		t.Fatalf("limit = %+v after a new sign-in, want none", g.Limit)
	}
}

func TestAMarkWrittenWhileARefreshIsInFlightSurvivesIt(t *testing.T) {
	f := newRefresherFixture(t, time.Hour)
	f.advance(50 * time.Minute)
	f.p.token = func(map[string]string, int) (int, string) {
		// The service notes the limit between the refresh's read of the grant and its write.
		if err := f.r.NoteLimit(f.r.now(), "mid-refresh"); err != nil {
			t.Error(err)
		}
		return 200, `{"access_token":"A-new","refresh_token":"R-new","expires_in":3600}`
	}
	f.r.step(context.Background())
	g, _ := f.s.Load()
	if g.Access != "A-new" || g.Limit == nil || g.Limit.Message != "mid-refresh" {
		t.Fatalf("store = %+v, the refresh's write erased the mark made while it ran", g)
	}
}
