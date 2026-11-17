package grant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// LoginState is where a device-code sign-in started through Logins stands.
type LoginState string

const (
	// LoginPending: the code was issued and nobody has approved it.
	LoginPending LoginState = "pending"
	// LoginApproved: the sign-in is stored.
	LoginApproved LoginState = "approved"
	// LoginExpired: nobody approved the code in time.
	LoginExpired LoginState = "expired"
	// LoginFailed: the provider refused it, or it could not be stored.
	LoginFailed LoginState = "failed"
)

// LoginTimeout bounds how long one sign-in waits for an approval. It is the
// command line's bound, so the two ways in behave alike.
const LoginTimeout = 15 * time.Minute

// LoginView is what a caller may know of a sign-in: what to show a person and where
// it stands. It never holds a token. URL and Code are set only while it is pending.
type LoginView struct {
	ID        string
	State     LoginState
	URL       string
	Code      string
	ExpiresAt time.Time
	// Reason is one sentence, set when State is failed or expired. It is the
	// package's own error text, which carries no token (the command line prints the
	// same).
	Reason string
}

// Logins runs the device-code sign-in as a start-then-poll pair, for a caller that
// cannot block for the minutes a person takes to approve (muster #498): the HTTP
// route. `muster grant login` is the same flow done synchronously in the command
// line; both end in Store.SignIn.
//
// One sign-in is pending at a time. A second Start while one is waiting returns the
// waiting one, so a client that reloads its page does not mint a second code and
// two people cannot race to approve different ones. The latest attempt's outcome is
// kept so a poll after approval still answers; an older attempt is forgotten. A
// pending sign-in is held in memory only: a restart loses it, its code just expires,
// and the person starts again.
//
// The zero value is not usable; NewLogins fills it.
type Logins struct {
	store  *Store
	client *Client
	base   context.Context
	now    func() time.Time
	// timeout bounds a wait for approval. A field so a test does not wait minutes.
	timeout time.Duration

	mu  sync.Mutex
	cur *login
}

type login struct {
	view LoginView
}

// NewLogins returns a Logins that stores a finished sign-in in store and ends its
// waits when base does (the service's own lifetime).
func NewLogins(base context.Context, store *Store, client *Client) *Logins {
	return &Logins{store: store, client: client, base: base, now: time.Now, timeout: LoginTimeout}
}

// Start begins a sign-in, or returns the one already waiting. started says which.
func (l *Logins) Start() (view LoginView, started bool, err error) {
	l.mu.Lock()
	if l.cur != nil && l.cur.view.State == LoginPending {
		v := l.cur.view
		l.mu.Unlock()
		return v, false, nil
	}
	l.mu.Unlock()

	dev, err := l.client.StartDevice(l.base)
	if err != nil {
		return LoginView{}, false, err
	}
	id, err := newLoginID()
	if err != nil {
		return LoginView{}, false, err
	}
	cur := &login{view: LoginView{
		ID: id, State: LoginPending, URL: dev.URL, Code: dev.Code,
		ExpiresAt: l.now().Add(l.timeout),
	}}

	l.mu.Lock()
	if l.cur != nil && l.cur.view.State == LoginPending {
		// Another Start won while this one asked the provider. Keep that one; the
		// code just issued is never shown and lapses unused.
		v := l.cur.view
		l.mu.Unlock()
		return v, false, nil
	}
	l.cur = cur
	view = cur.view
	l.mu.Unlock()

	go l.wait(cur, dev)
	return view, true, nil
}

// Get is the attempt with id, or false when it is not the latest one.
func (l *Logins) Get(id string) (LoginView, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur == nil || l.cur.view.ID != id {
		return LoginView{}, false
	}
	return l.cur.view, true
}

// SignOut forgets the stored sign-in and reports whether there was one (an
// unreadable store counts as one: it is removed all the same). Sessions already
// running keep the access token they hold until it lapses, as after
// `muster grant logout`.
func (l *Logins) SignOut() (removed bool, err error) {
	_, lerr := l.store.Load()
	removed = !errors.Is(lerr, ErrNoGrant)
	if err := l.store.Remove(); err != nil {
		return false, err
	}
	return removed, nil
}

// wait polls the provider until the person approves, the bound passes or the
// service ends, then records the outcome.
func (l *Logins) wait(cur *login, dev Device) {
	ctx, cancel := context.WithTimeout(l.base, l.timeout)
	defer cancel()
	toks, err := l.client.PollDevice(ctx, dev)
	state, reason := LoginApproved, ""
	switch {
	case err == nil:
		if _, serr := l.store.SignIn(toks, l.now()); serr != nil {
			state, reason = LoginFailed, serr.Error()
		}
	case errors.Is(err, context.DeadlineExceeded):
		state, reason = LoginExpired, "nobody approved the code in time; start the sign-in again"
	case errors.Is(err, context.Canceled):
		state, reason = LoginFailed, "the service stopped before the code was approved; start the sign-in again"
	default:
		state, reason = LoginFailed, err.Error()
	}
	l.mu.Lock()
	cur.view.State, cur.view.Reason = state, reason
	cur.view.URL, cur.view.Code = "", ""
	l.mu.Unlock()
}

func newLoginID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
