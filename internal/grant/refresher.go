package grant

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Refresher is the one party that renews the stored grant (muster #451).
//
// It is built around three rules, each of which a test pins:
//
//   - One refresh at a time, in one process. The refresh token rotates, so a
//     second concurrent use of it would be a use of a dead token.
//   - Persist, then tell. The rotated refresh token is written to the store before
//     any subscriber hears of the new access token. If the write fails the rotated
//     grant is held in memory and the write retried, so a full disk does not strand
//     a grant the provider has already rotated.
//   - When to refresh is read from the token itself. Nothing here assumes how long
//     an access token lives: the renewal point is a quarter of the token's own
//     lifetime before it ends, kept between five minutes and a day.
type Refresher struct {
	store  *Store
	client *Client
	now    func() time.Time
	logf   func(format string, args ...any)

	// poll is how often the store is looked at when nothing is due, so a sign-in
	// or sign-out made by the operator's command is noticed. minGap spaces two
	// refreshes; backoffBase and backoffMax bound the retry after a transient
	// failure. Fields so a test does not wait.
	poll        time.Duration
	minGap      time.Duration
	backoffBase time.Duration
	backoffMax  time.Duration

	mu       sync.Mutex // one refresh at a time; guards everything below
	seen     int64      // the generation last looked at
	unsaved  *Grant     // a rotated grant the store has not taken yet
	failures int
	nextTry  time.Time
	lastErr  atomic.Pointer[string] // the last transient failure, for State; cleared on success
	logged   string                 // the last unusable-store message, so it is said once
	subs     map[int]func(Access)
	nextSub  int
}

// RefresherOption configures NewRefresher.
type RefresherOption func(*Refresher)

// WithLogf sets where the refresher's one-line notices go. They never carry a
// token.
func WithLogf(f func(format string, args ...any)) RefresherOption {
	return func(r *Refresher) { r.logf = f }
}

// WithClock sets the refresher's clock. A test seam.
func WithClock(now func() time.Time) RefresherOption {
	return func(r *Refresher) { r.now = now }
}

// NewRefresher returns a refresher over store, talking to the provider through
// client.
func NewRefresher(store *Store, client *Client, opts ...RefresherOption) *Refresher {
	r := &Refresher{
		store:       store,
		client:      client,
		now:         time.Now,
		poll:        30 * time.Second,
		minGap:      30 * time.Second,
		backoffBase: 30 * time.Second,
		backoffMax:  15 * time.Minute,
		subs:        map[int]func(Access){},
	}
	for _, o := range opts {
		o(r)
	}
	if r.logf == nil {
		r.logf = func(string, ...any) {}
	}
	return r
}

// Store is the store the refresher renews.
func (r *Refresher) Store() *Store { return r.store }

// Subscribe registers fn to be called, with no lock held, each time the current
// access token changes: a refresh, or a sign-in noticed in the store. The caller
// that wants the token it already has asks Access. It returns the function that
// removes the subscription.
func (r *Refresher) Subscribe(fn func(Access)) (cancel func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = fn
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.subs, id)
	}
}

// Run renews the grant until ctx ends.
func (r *Refresher) Run(ctx context.Context) {
	for {
		wait := r.step(ctx)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// State is the grant's state now, read from the store, with a transient refresh
// failure overlaid. It takes no lock a refresh holds, so a listing never waits
// on the provider. It never holds a credential.
func (r *Refresher) State() State {
	g, err := r.store.Load()
	if err != nil {
		st := stateOf(nil, r.now())
		if !errors.Is(err, ErrNoGrant) {
			st.Reason = err.Error()
		}
		return st
	}
	st := stateOf(&g, r.now())
	if p := r.lastErr.Load(); p != nil && st.Phase == PhaseOK {
		st.Phase = PhaseDegraded
		st.Reason = "the last refresh failed (" + *p + "); it is being retried"
		if !g.ExpiresAt().After(r.now()) {
			st.Reason += ", and the access token has lapsed"
		}
	}
	return st
}

// accessGrace is how close to its end an access token may be before Access
// refuses to hand it out without renewing it first.
const accessGrace = time.Minute

// Access returns the current access token, renewing the grant first when the
// token is about to lapse. It returns ErrSignedOut when no grant is stored and a
// *RevokedError when the provider has refused the refresh token. A renewal that
// failed for a transient reason still returns the current token if it has not
// lapsed: a provider that is down for a minute does not stop a create.
func (r *Refresher) Access(ctx context.Context) (Access, error) {
	r.mu.Lock()
	g, err := r.load()
	if err != nil {
		r.mu.Unlock()
		if errors.Is(err, ErrNoGrant) {
			return Access{}, ErrSignedOut
		}
		return Access{}, err
	}
	if g.Revoked != nil {
		r.mu.Unlock()
		return Access{}, &RevokedError{Status: g.Revoked.Status, Code: g.Revoked.Code}
	}
	if g.ExpiresAt().Sub(r.now()) > accessGrace {
		r.mu.Unlock()
		return toAccess(*g), nil
	}
	ng, rerr := r.refreshLocked(ctx, *g)
	if rerr == nil {
		subs := r.snapshotSubs()
		r.mu.Unlock()
		a := toAccess(ng)
		notify(subs, a)
		return a, nil
	}
	r.mu.Unlock()
	var rej *RejectedError
	if errors.As(rerr, &rej) {
		return Access{}, &RevokedError{Status: rej.Status, Code: rej.Code}
	}
	if g.ExpiresAt().After(r.now()) {
		return toAccess(*g), nil
	}
	return Access{}, rerr
}

// step does one pass and returns how long to wait before the next.
func (r *Refresher) step(ctx context.Context) time.Duration {
	r.mu.Lock()
	g, err := r.load()
	if err != nil {
		r.seen = 0
		msg := ""
		if !errors.Is(err, ErrNoGrant) {
			msg = err.Error()
		}
		if msg != r.logged {
			r.logged = msg
			if msg != "" {
				r.logf("grant: %s", msg)
			}
		}
		r.mu.Unlock()
		return r.poll
	}
	r.logged = ""
	adopted := g.Generation != r.seen
	r.seen = g.Generation
	if g.Revoked != nil {
		r.mu.Unlock()
		return r.poll
	}
	now := r.now()
	due := refreshAt(*g)
	if now.Before(due) || now.Before(r.nextTry) {
		var subs []func(Access)
		if adopted {
			// A sign-in made since the last pass: live sessions still hold the old one.
			subs = r.snapshotSubs()
		}
		wait := r.poll
		next := due
		if r.nextTry.After(next) {
			next = r.nextTry
		}
		if d := next.Sub(now); d > 0 && d < wait {
			wait = d
		}
		r.mu.Unlock()
		if adopted {
			notify(subs, toAccess(*g))
		}
		return wait
	}
	ng, rerr := r.refreshLocked(ctx, *g)
	if rerr != nil {
		wait := r.poll
		if d := r.nextTry.Sub(r.now()); d > 0 && d < wait {
			wait = d
		}
		r.mu.Unlock()
		return wait
	}
	subs := r.snapshotSubs()
	r.mu.Unlock()
	notify(subs, toAccess(ng))
	return r.poll
}

// load returns the grant to work from. A rotated grant the store has not taken
// yet is retried first; once it is in, or a new sign-in has superseded it, it is
// dropped. Called with r.mu held.
func (r *Refresher) load() (*Grant, error) {
	if r.unsaved != nil {
		u := *r.unsaved
		if _, ok, err := r.store.Update(u.Generation, func(g *Grant) { *g = u }); err == nil {
			r.unsaved = nil
			if ok {
				return &u, nil
			}
		} else {
			return &u, nil
		}
	}
	g, err := r.store.Load()
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// refreshLocked renews g and persists the result. Called with r.mu held.
func (r *Refresher) refreshLocked(ctx context.Context, g Grant) (Grant, error) {
	toks, err := r.client.Refresh(ctx, g.Refresh)
	now := r.now()
	if err != nil {
		var rej *RejectedError
		if errors.As(err, &rej) {
			_, _, serr := r.store.Update(g.Generation, func(x *Grant) {
				x.Revoked = &Revoked{AtMs: now.UnixMilli(), Status: rej.Status, Code: rej.Code}
			})
			if serr != nil {
				r.logf("grant: the provider refused the refresh token but the store could not record it: %v", serr)
			}
			r.logf("grant: %v; the grant is revoked until someone signs in again (muster grant login)", rej)
			return Grant{}, err
		}
		r.failures++
		r.nextTry = now.Add(r.backoff())
		msg := err.Error()
		r.lastErr.Store(&msg)
		if r.failures == 1 || r.failures&(r.failures-1) == 0 {
			r.logf("grant: refresh failed (attempt %d): %v; retrying", r.failures, err)
		}
		return Grant{}, err
	}
	ng := g
	ng.Access = toks.Access
	ng.Refresh = toks.Refresh
	ng.ExpiresAtMs = now.Add(toks.ExpiresIn).UnixMilli()
	ng.RefreshedAtMs = now.UnixMilli()
	if toks.AccountID != "" {
		ng.AccountID = toks.AccountID
	}
	ng.Revoked = nil
	r.failures = 0
	r.lastErr.Store(nil)
	r.nextTry = now.Add(r.minGap)
	saved, ok, serr := r.store.Update(g.Generation, func(x *Grant) { *x = ng })
	switch {
	case serr != nil:
		// The provider has rotated the token. Holding it in memory and retrying the
		// write is the only way the rotation is not lost.
		r.unsaved = &ng
		r.logf("grant: the rotated grant could not be written (%v); holding it in memory and retrying", serr)
		return ng, nil
	case !ok:
		// A sign-in landed while the refresh was in flight. It wins; this result
		// belongs to a grant that no longer exists.
		r.logf("grant: a new sign-in landed during a refresh; keeping the new sign-in")
		return Grant{}, errors.New("a new sign-in superseded this refresh")
	}
	r.seen = saved.Generation
	return saved, nil
}

func (r *Refresher) backoff() time.Duration {
	d := r.backoffBase
	for i := 1; i < r.failures && d < r.backoffMax; i++ {
		d *= 2
	}
	if d > r.backoffMax {
		d = r.backoffMax
	}
	return d
}

func (r *Refresher) snapshotSubs() []func(Access) {
	out := make([]func(Access), 0, len(r.subs))
	for _, f := range r.subs {
		out = append(out, f)
	}
	return out
}

func notify(subs []func(Access), a Access) {
	for _, f := range subs {
		f(a)
	}
}

func toAccess(g Grant) Access {
	return Access{Token: g.Access, ExpiresAt: g.ExpiresAt(), AccountID: g.AccountID}
}

// refreshAt is when g should next be renewed: a quarter of the access token's own
// lifetime before it ends, but never closer than five minutes to the end nor
// further than a day from it.
func refreshAt(g Grant) time.Time {
	exp := g.ExpiresAt()
	start := g.SignedInAtMs
	if g.RefreshedAtMs > start {
		start = g.RefreshedAtMs
	}
	life := exp.Sub(time.UnixMilli(start))
	if life < 0 {
		life = 0
	}
	margin := life / 4
	if margin < 5*time.Minute {
		margin = 5 * time.Minute
	}
	if margin > 24*time.Hour {
		margin = 24 * time.Hour
	}
	return exp.Add(-margin)
}
