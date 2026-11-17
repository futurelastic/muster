package grant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// waitLogin polls Get until the attempt leaves pending, or fails the test.
func waitLogin(t *testing.T, l *Logins, id string) LoginView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := l.Get(id)
		if !ok {
			t.Fatalf("login %s is unknown", id)
		}
		if v.State != LoginPending {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the login never left pending")
	return LoginView{}
}

func TestLoginStartsPendingThenStoresTheApprovedSignIn(t *testing.T) {
	p := newFakeProvider(t)
	p.pendingPolls = 2
	p.token = func(map[string]string, int) (int, string) {
		return 200, `{"access_token":"ACCESS-1","refresh_token":"REFRESH-1","expires_in":3600}`
	}
	store := newTestStore(t)
	l := NewLogins(context.Background(), store, p.client())

	v, started, err := l.Start()
	if err != nil || !started {
		t.Fatalf("Start: started=%v err=%v", started, err)
	}
	if v.State != LoginPending || v.Code != "ABCD-1234" || !strings.HasSuffix(v.URL, "/codex/device") || v.ID == "" || v.ExpiresAt.IsZero() {
		t.Fatalf("pending view = %+v", v)
	}
	done := waitLogin(t, l, v.ID)
	if done.State != LoginApproved || done.Reason != "" {
		t.Fatalf("outcome = %+v", done)
	}
	if done.Code != "" || done.URL != "" {
		t.Fatalf("a finished login must not show its code again: %+v", done)
	}
	g, err := store.Load()
	if err != nil || g.Refresh != "REFRESH-1" || g.Access != "ACCESS-1" || g.Generation != 1 {
		t.Fatalf("stored grant = %+v err=%v", g, err)
	}
	// No view, in any state, can carry a token: the type has no field for one, and
	// its JSON does not mention one either.
	raw, _ := json.Marshal(done)
	for _, secret := range []string{"ACCESS-1", "REFRESH-1"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("view carries %q: %s", secret, raw)
		}
	}
}

func TestLoginOnePendingAtATime(t *testing.T) {
	p := newFakeProvider(t)
	p.pendingPolls = 1 << 20
	l := NewLogins(context.Background(), newTestStore(t), p.client())
	l.client.Sleep = func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, 5*time.Millisecond) }

	first, started, err := l.Start()
	if err != nil || !started {
		t.Fatal(err)
	}
	again, started, err := l.Start()
	if err != nil || started || again.ID != first.ID || again.Code != first.Code {
		t.Fatalf("a second Start while pending must return the first: started=%v %+v vs %+v err=%v", started, again, first, err)
	}
	if _, ok := l.Get("not-an-id"); ok {
		t.Fatal("Get answered for an id that was never issued")
	}
}

func TestLoginExpiresWhenNobodyApproves(t *testing.T) {
	p := newFakeProvider(t)
	p.pendingPolls = 1 << 20
	l := NewLogins(context.Background(), newTestStore(t), p.client())
	l.timeout = 50 * time.Millisecond
	l.client.Sleep = func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, 10*time.Millisecond) }

	v, _, err := l.Start()
	if err != nil {
		t.Fatal(err)
	}
	done := waitLogin(t, l, v.ID)
	if done.State != LoginExpired || done.Reason == "" {
		t.Fatalf("outcome = %+v", done)
	}
	// A fresh start is allowed after it lapsed, and is a new attempt.
	next, started, err := l.Start()
	if err != nil || !started || next.ID == v.ID {
		t.Fatalf("restart after expiry: started=%v next=%+v err=%v", started, next, err)
	}
}

func TestLoginFailsWhenTheProviderRefuses(t *testing.T) {
	p := newFakeProvider(t)
	p.token = func(map[string]string, int) (int, string) { return 400, `{"error":"invalid_grant"}` }
	store := newTestStore(t)
	l := NewLogins(context.Background(), store, p.client())
	v, _, err := l.Start()
	if err != nil {
		t.Fatal(err)
	}
	done := waitLogin(t, l, v.ID)
	if done.State != LoginFailed || done.Reason == "" {
		t.Fatalf("outcome = %+v", done)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("a failed login stored a grant")
	}
}

func TestLoginEndsWithTheService(t *testing.T) {
	p := newFakeProvider(t)
	p.pendingPolls = 1 << 20
	ctx, stop := context.WithCancel(context.Background())
	l := NewLogins(ctx, newTestStore(t), p.client())
	l.client.Sleep = func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, 10*time.Millisecond) }
	v, _, err := l.Start()
	if err != nil {
		t.Fatal(err)
	}
	stop()
	done := waitLogin(t, l, v.ID)
	if done.State != LoginFailed {
		t.Fatalf("outcome = %+v", done)
	}
}

func TestStoreSignInIsTheOnePlaceATokenSetBecomesAGrant(t *testing.T) {
	s := newTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	g, err := s.SignIn(Tokens{Access: "A", Refresh: "R", ExpiresIn: time.Hour, AccountID: "acct"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if g.Refresh != "R" || g.Access != "A" || g.AccountID != "acct" || g.SignedInAtMs != now.UnixMilli() ||
		g.ExpiresAtMs != now.Add(time.Hour).UnixMilli() || g.Generation != 1 {
		t.Fatalf("grant = %+v", g)
	}
}
