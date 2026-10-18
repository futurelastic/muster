package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// A peer may be known by more than one address (#335): a machine that joins the
// private network through one of two connections has a different fixed address
// on each, and only one exists at a time. The driver holds the addresses in the
// order they were configured and keeps one "current" — the last that answered.
//
// # The rule that makes failing over safe
//
// A request is tried on the next address ONLY when it never got a connection:
// the dial was refused, timed out, found no route, or failed its TLS handshake.
// Once a connection exists the request may have reached the peer, and sending
// it again somewhere else could run a create or a send twice. So an error after
// the connection is reported as it always was, and never retried here. That is
// also what keeps a peer with ONE address exactly as it was: with one address
// there is no second dial to make, and the code below is not entered at all.
//
// # Why a connect budget
//
// A dead address often does not refuse — it swallows the SYN and says nothing.
// Without a bound of its own the first such address would consume the whole of
// the caller's deadline and the second would never be tried. The budget bounds
// only the dial; once connected, the caller's deadline governs as before.

// defaultConnectBudget is how long one address gets to accept a connection
// before the next is tried.
const defaultConnectBudget = 3 * time.Second

// WithFallbackURLs adds addresses to try, in order, after the one passed to New
// — all of them for the same peer. A call fails over to the next one only when
// it could not connect; the one that answers last is tried first next time.
func WithFallbackURLs(bases ...string) Option {
	return func(d *Driver) {
		for _, b := range bases {
			if b = strings.TrimRight(strings.TrimSpace(b), "/"); b != "" {
				d.bases = append(d.bases, b)
			}
		}
	}
}

// WithConnectBudget sets how long one address gets to accept a connection when
// the peer has more than one. It has no effect on a peer with a single address.
func WithConnectBudget(dur time.Duration) Option {
	return func(d *Driver) { d.connectBudget = dur }
}

// current is the address a new request is built against.
func (d *Driver) current() string {
	if len(d.bases) <= 1 {
		return d.base
	}
	return d.bases[atomic.LoadInt32(&d.cur)]
}

// Address is the address this driver currently uses to reach the peer, and ok
// reports whether there is a choice at all: a peer with a single address has
// nothing to say that its configuration does not already. Implements
// driver.AddressReporter (#335). Any credential in the URL is dropped.
func (d *Driver) Address() (addr string, ok bool) {
	if len(d.bases) <= 1 {
		return "", false
	}
	b := d.current()
	if u, err := url.Parse(b); err == nil {
		u.User = nil
		b = u.String()
	}
	return b, true
}

// order is every address, current first, the rest in configured order.
func (d *Driver) order() []string {
	cur := int(atomic.LoadInt32(&d.cur))
	out := make([]string, 0, len(d.bases))
	out = append(out, d.bases[cur])
	for i, b := range d.bases {
		if i != cur {
			out = append(out, b)
		}
	}
	return out
}

// roundTrip is client.Do for a request built against current()+rel, trying the
// peer's other addresses when this one cannot be connected to.
func (d *Driver) roundTrip(req *http.Request, rel string) (*http.Response, error) {
	if len(d.bases) <= 1 {
		return d.client.Do(req)
	}
	parent := req.Context()
	candidates := d.order()
	var lastErr error
	for i, base := range candidates {
		if err := parent.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		attempt := req
		if req.URL.String() != base+rel {
			u, err := url.Parse(base + rel)
			if err != nil {
				return nil, err
			}
			attempt = req.Clone(parent)
			attempt.URL, attempt.Host = u, ""
			if req.GetBody != nil && req.Body != nil && req.Body != http.NoBody {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				attempt.Body = body
			}
		}
		resp, connected, err := d.attemptDial(attempt, len(candidates)-i)
		if err == nil {
			d.noteAnswered(base)
			return resp, nil
		}
		lastErr = err
		if connected {
			// A connection existed: the request may have been delivered.
			return nil, err
		}
	}
	return nil, lastErr
}

// attemptDial makes one try on one address. connected reports whether a
// connection to it was ever established — the line between "safe to try
// elsewhere" and "not".
func (d *Driver) attemptDial(req *http.Request, left int) (resp *http.Response, connected bool, err error) {
	budget := d.connectBudget
	if budget <= 0 {
		budget = defaultConnectBudget
	}
	if dl, ok := req.Context().Deadline(); ok {
		if share := time.Until(dl) / time.Duration(left); share > 0 && share < budget {
			budget = share
		}
	}
	ctx, cancel := context.WithCancel(req.Context())
	var got atomic.Bool
	var timedOut atomic.Bool
	timer := time.AfterFunc(budget, func() {
		if !got.Load() {
			timedOut.Store(true)
			cancel()
		}
	})
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
		got.Store(true)
		timer.Stop()
	}}
	resp, err = d.client.Do(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
	if err != nil {
		timer.Stop()
		cancel()
		if timedOut.Load() && !got.Load() {
			return nil, false, fmt.Errorf("no connection within %s: %w", budget.Round(time.Millisecond), errConnectTimeout)
		}
		return nil, got.Load(), err
	}
	timer.Stop()
	// The attempt's context must outlive this call: cancelling it would cut the
	// body off. It is released when the body is.
	resp.Body = &releaseOnClose{ReadCloser: resp.Body, release: cancel}
	return resp, true, nil
}

// errConnectTimeout is why an address was passed over when it neither accepted
// nor refused. Deliberately not context.DeadlineExceeded: that would read as the
// caller's own budget running out (see transportFailure).
var errConnectTimeout = errors.New("connect timeout")

type releaseOnClose struct {
	io.ReadCloser
	release func()
}

func (r *releaseOnClose) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}

// noteAnswered remembers the address that just answered and says so once when
// that is a change.
func (d *Driver) noteAnswered(base string) {
	for i, b := range d.bases {
		if b != base {
			continue
		}
		if prev := atomic.SwapInt32(&d.cur, int32(i)); int(prev) != i {
			log.Printf("remote: peer %s now reached at a different address (address %d of %d in its configuration)",
				d.machine, i+1, len(d.bases))
		}
		return
	}
}
