package service

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

// PrincipalTable is the principal table of a RUNNING service (muster #347).
//
// Config is copied by value into every handler, so a slice in it can never
// change after the mux is built. The table is the one shared, mutable thing:
// Config.Table points at it, every request reads the current snapshot through
// Config.principals, and Replace swaps the whole snapshot in one step. A request
// therefore sees either the old table or the new one, never a mixture.
//
// It also remembers the long-lived requests (event streams, long polls) that
// authenticated against it, so a replacement can close exactly the ones whose
// credential is gone while every other stream is left alone.
type PrincipalTable struct {
	cur atomic.Pointer[[]Principal]

	mu      sync.Mutex
	streams map[*streamBinding]struct{}
}

type streamBinding struct {
	p      Principal
	cancel context.CancelFunc
}

// NewPrincipalTable starts a table holding initial.
func NewPrincipalTable(initial []Principal) *PrincipalTable {
	t := &PrincipalTable{streams: map[*streamBinding]struct{}{}}
	t.cur.Store(&initial)
	return t
}

// Snapshot is the table as it is now. Callers must not modify it.
func (t *PrincipalTable) Snapshot() []Principal {
	if t == nil {
		return nil
	}
	if p := t.cur.Load(); p != nil {
		return *p
	}
	return nil
}

// Replace installs next and closes every bound stream whose principal no
// longer holds the credential it authenticated with, or no longer holds the
// read grant a stream needs. It reports how many it closed, by principal name.
func (t *PrincipalTable) Replace(next []Principal) map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cur.Store(&next)
	closed := map[string]int{}
	for b := range t.streams {
		if !stillValid(next, b.p) {
			b.cancel()
			delete(t.streams, b)
			closed[b.p.Name]++
		}
	}
	return closed
}

// stillValid reports whether a principal that authenticated earlier would
// authenticate, and may still read, against table.
func stillValid(table []Principal, was Principal) bool {
	for _, p := range table {
		if p.Name != was.Name {
			continue
		}
		if p.Token != was.Token || !bytes.Equal(p.PublicKey, was.PublicKey) {
			continue
		}
		return p.Allows(GrantRead)
	}
	return false
}

// bind ties ctx to the principal that authenticated it. The returned context
// is cancelled when a Replace revokes that principal; release must be called
// when the request ends.
//
// The check and the registration happen under the lock Replace holds, so a
// request that authenticated against the old table and reaches bind after the
// swap is closed at once rather than slipping through the gap.
func (t *PrincipalTable) bind(ctx context.Context, p Principal) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	if !stillValid(t.Snapshot(), p) {
		t.mu.Unlock()
		cancel()
		return ctx, func() {}
	}
	b := &streamBinding{p: p, cancel: cancel}
	t.streams[b] = struct{}{}
	t.mu.Unlock()
	return ctx, func() {
		t.mu.Lock()
		delete(t.streams, b)
		t.mu.Unlock()
		cancel()
	}
}

// principals is the table a request is judged against: the live one when the
// deployment has one, else the fixed slice the Config was built with.
func (c Config) principals() []Principal {
	if c.Table != nil {
		return c.Table.Snapshot()
	}
	return c.Principals
}

// setPrincipalTable remembers the live table so long-lived handlers can bind
// to it. Called by NewMux.
func (s *Service) setPrincipalTable(t *PrincipalTable) {
	s.principalTable.Store(t)
}

// streamContext is r's context for a long-lived response. When the caller
// authenticated against a live table it is cancelled if that caller is
// revoked; otherwise it is r.Context() unchanged.
func (s *Service) streamContext(r *http.Request) (context.Context, func()) {
	t := s.principalTable.Load()
	p, ok := principalOf(r)
	if t == nil || !ok {
		return r.Context(), func() {}
	}
	return t.bind(r.Context(), p)
}
