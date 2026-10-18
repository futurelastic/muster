package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// addrRetryInterval is how often an absent FLEET_ADDR entry is tried again,
// and how often a bound one is checked to still exist. Short enough that a
// connection coming up is reachable within seconds; long enough that a machine
// that is permanently missing one address pays one failed bind per interval and
// nothing else.
const addrRetryInterval = 5 * time.Second

// Entry states. An entry is never "failed": a failure at startup that is not
// "the address is not on this machine" stops the service before any entry
// matters, and after startup every failure is just a reason to keep waiting.
const (
	addrBound   = "bound"
	addrWaiting = "waiting"
)

// listenerSet is the service's set of FLEET_ADDR listeners (#335).
//
// # Why entries are wanted, not required
//
// A machine can have more than one address it is known by, and only some of
// them exist at any moment: a laptop that joins the private network through
// one of two connections has a different fixed address on each, and exactly one
// is ever present. Before this, listing both meant the service never started
// (the absent one failed to bind, and the first failure was fatal); listing one
// meant the machine was unreachable whenever it was on the other.
//
// So an entry whose address is simply not on this machine yet is WAITING: the
// set keeps trying it and binds it the moment the address appears. An entry
// whose address later disappears goes back to waiting — the listener is closed
// rather than left holding an address nothing can reach, which is what made the
// old behaviour silent as well as fragile.
//
// # What stays fatal
//
// Only "this address does not exist here" is patient. Everything else is a
// statement about the configuration, and a service that comes up on a subset of
// what it was told is one whose reachability depends on which client asks (the
// F36 ambiguity): an entry that is not host:port, a port another process holds,
// a permission refusal, and — always — loopback, which is the binding that keeps
// a wedged-looking service diagnosable (see withLoopback). None of those wait.
type listenerSet struct {
	// listen, hasAddr and serve are the seams a test replaces. Production
	// leaves them as net.Listen, the interface check below, and a Serve
	// goroutine that dies loudly on an unexpected error.
	listen  func(network, addr string) (net.Listener, error)
	hasAddr func(ip net.IP) bool
	serve   func(net.Listener)
	logf    func(format string, args ...any)
	// announce is called for every listener that comes up, at startup or later.
	announce func(net.Listener)

	mu      sync.Mutex
	entries []*addrEntry
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

type addrEntry struct {
	addr     string
	loopback bool
	ln       net.Listener
	state    string
	// lastErr is the last reason a bind attempt failed, so a persistent failure
	// is logged once and not once per interval.
	lastErr string
	since   time.Time
}

// addrStatus is one entry's state, for a log line or a test.
type addrStatus struct {
	Addr  string
	State string
	// Listening is the bound address (the real port when the entry asked for 0).
	Listening string
}

func newListenerSet(srv *http.Server, announce func(net.Listener)) *listenerSet {
	return &listenerSet{
		listen:  net.Listen,
		hasAddr: localAddrPresent,
		serve: func(ln net.Listener) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				log.Fatalf("muster: serve %s: %v", ln.Addr(), err)
			}
		},
		logf:     log.Printf,
		announce: announce,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// isLoopbackHost reports whether host is the machine's own loopback. A wildcard
// is not listed: it includes loopback, but a bind on it is not "the loopback
// listener" in the sense the mandatory rule means — and it can never be absent.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// start binds every entry it can and returns an error naming the first entry
// that must stop the service. On error nothing is left listening.
func (s *listenerSet) start(addrs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range addrs {
		host, _, _ := net.SplitHostPort(a)
		e := &addrEntry{addr: a, loopback: isLoopbackHost(host), state: addrWaiting, since: time.Now()}
		ln, err := s.listen("tcp", a)
		switch {
		case err == nil:
			s.bindLocked(e, ln, true)
		case !e.loopback && bindIsWaiting(err):
			e.lastErr = err.Error()
			s.logf("muster: FLEET_ADDR entry %s is not available on this machine yet (%v) — waiting for it; the service is up on its other addresses", a, err)
		default:
			for _, prev := range s.entries {
				if prev.ln != nil {
					_ = prev.ln.Close()
				}
			}
			s.entries = nil
			return fmt.Errorf("listen %s: %w", a, err)
		}
		s.entries = append(s.entries, e)
	}
	for _, e := range s.entries {
		if e.state == addrWaiting {
			s.logf("muster: FLEET_ADDR: %s", describeAddrStatus(s.statusLocked()))
			break
		}
	}
	return nil
}

// bindLocked records ln as the entry's listener and starts serving it.
func (s *listenerSet) bindLocked(e *addrEntry, ln net.Listener, atStartup bool) {
	e.ln, e.state, e.lastErr, e.since = ln, addrBound, "", time.Now()
	if !atStartup {
		s.logf("muster: FLEET_ADDR entry %s is now present — bound", e.addr)
	}
	if s.announce != nil {
		s.announce(ln)
	}
	go s.serve(ln)
}

// run keeps the set honest until stop: it retries waiting entries and closes a
// bound one whose address has gone. It returns when stop is called.
func (s *listenerSet) run(interval time.Duration) {
	defer close(s.doneCh)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.tick()
		}
	}
}

// tick is one pass of run. Exported to the tests by being in the package.
func (s *listenerSet) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	for _, e := range s.entries {
		switch e.state {
		case addrBound:
			if e.loopback {
				continue
			}
			ip := checkableIP(e.addr)
			if ip == nil || s.hasAddr(ip) {
				continue
			}
			// The address left the machine under a live listener. Keeping the
			// socket would look healthy in every listing while serving nobody,
			// and could block a rebind when the address returns.
			_ = e.ln.Close()
			e.ln, e.state, e.since = nil, addrWaiting, time.Now()
			e.lastErr = ""
			s.logf("muster: FLEET_ADDR entry %s is no longer present on this machine — listener closed, waiting for it to return", e.addr)
		case addrWaiting:
			ln, err := s.listen("tcp", e.addr)
			if err == nil {
				s.bindLocked(e, ln, false)
				continue
			}
			if msg := err.Error(); msg != e.lastErr {
				e.lastErr = msg
				if bindIsWaiting(err) {
					s.logf("muster: FLEET_ADDR entry %s still not available (%v)", e.addr, err)
				} else {
					// After startup nothing here is fatal: the service is
					// already serving its other addresses. Say what is wrong
					// once, and keep trying.
					s.logf("muster: FLEET_ADDR entry %s cannot be bound yet (%v) — retrying", e.addr, err)
				}
			}
		}
	}
}

// stop ends the retry loop and closes every listener the set still holds.
func (s *listenerSet) stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stopCh)
	for _, e := range s.entries {
		if e.ln != nil {
			_ = e.ln.Close()
		}
	}
	s.mu.Unlock()
}

// status is a snapshot of every entry, in configuration order.
func (s *listenerSet) status() []addrStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *listenerSet) statusLocked() []addrStatus {
	out := make([]addrStatus, 0, len(s.entries))
	for _, e := range s.entries {
		st := addrStatus{Addr: e.addr, State: e.state}
		if e.ln != nil {
			st.Listening = e.ln.Addr().String()
		}
		out = append(out, st)
	}
	return out
}

// checkableIP is the IP an entry names, or nil when presence cannot be judged
// from the interface list: a hostname, a wildcard, or an address with a zone.
// Those entries are never closed for being "gone" — the bind itself is the only
// test of them.
func checkableIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return nil
	}
	return ip
}

// localAddrPresent reports whether ip is assigned to an interface of this
// machine right now.
func localAddrPresent(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		// Cannot tell. Claiming the address is gone would close a working
		// listener on a transient error; claiming it is present only delays a
		// close until the next successful read.
		return true
	}
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			if v.IP.Equal(ip) {
				return true
			}
		case *net.IPAddr:
			if v.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// bindIsWaiting reports whether a failed bind means "this address is not on
// this machine (yet)" — the only failure an entry is allowed to wait out.
func bindIsWaiting(err error) bool {
	if isAddrNotAvailable(err) {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns) && dns.IsNotFound
}

// describeAddrStatus renders the set for one log line.
func describeAddrStatus(st []addrStatus) string {
	parts := make([]string, 0, len(st))
	for _, s := range st {
		parts = append(parts, s.Addr+" "+s.State)
	}
	return strings.Join(parts, ", ")
}
