package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// fakeHost stands in for a machine whose addresses come and go. present maps
// an entry to whether its address is assigned right now; taken maps an entry
// to a bind error that is not "absent".
type fakeHost struct {
	mu      sync.Mutex
	present map[string]bool
	taken   map[string]error
	opened  []net.Listener
	logs    []string
}

func (h *fakeHost) listen(_, addr string) (net.Listener, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.taken[addr]; err != nil {
		return nil, &net.OpError{Op: "listen", Err: err}
	}
	if p, ok := h.present[addr]; ok && !p {
		return nil, &net.OpError{Op: "listen", Err: syscall.EADDRNOTAVAIL}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		h.opened = append(h.opened, ln)
	}
	return ln, err
}

func (h *fakeHost) hasAddr(ip net.IP) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for a, p := range h.present {
		host, _, _ := net.SplitHostPort(a)
		if net.ParseIP(host).Equal(ip) {
			return p
		}
	}
	return true
}

func (h *fakeHost) set(addr string, present bool) {
	h.mu.Lock()
	h.present[addr] = present
	h.mu.Unlock()
}

func (h *fakeHost) logf(format string, args ...any) {
	h.mu.Lock()
	h.logs = append(h.logs, fmt.Sprintf(format, args...))
	h.mu.Unlock()
}

func (h *fakeHost) logged(sub string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func newTestSet(h *fakeHost) *listenerSet {
	return &listenerSet{
		listen:  h.listen,
		hasAddr: h.hasAddr,
		serve: func(ln net.Listener) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		},
		logf:   h.logf,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

func states(s *listenerSet) string {
	var parts []string
	for _, st := range s.status() {
		parts = append(parts, st.Addr+"="+st.State)
	}
	return strings.Join(parts, " ")
}

const (
	addrA = "10.1.1.5:7000" // one connection's fixed address
	addrB = "10.2.2.5:7000" // the other's
	addrL = "127.0.0.1:7000"
)

// The incident: two addresses listed, exactly one exists. The service comes up
// on it and on loopback, reports the other as waiting, and binds it when it
// appears.
func TestListenerSetBindsWhatExistsAndWaitsForTheRest(t *testing.T) {
	h := &fakeHost{present: map[string]bool{addrA: false, addrB: true}}
	s := newTestSet(h)
	if err := s.start([]string{addrA, addrB, addrL}); err != nil {
		t.Fatalf("start with one absent address must succeed, got %v", err)
	}
	defer s.stop()
	if got, want := states(s), addrA+"=waiting "+addrB+"=bound "+addrL+"=bound"; got != want {
		t.Fatalf("states = %q, want %q", got, want)
	}

	// Still absent: stays waiting, and is not re-logged every interval.
	s.tick()
	s.tick()
	if got := states(s); !strings.Contains(got, addrA+"=waiting") {
		t.Fatalf("still-absent entry must keep waiting, got %q", got)
	}
	if n := h.logged("not available"); n != 1 {
		t.Errorf("a persistent absence must be logged once (at startup), logged %d times", n)
	}

	h.set(addrA, true)
	s.tick()
	if got, want := states(s), addrA+"=bound "+addrB+"=bound "+addrL+"=bound"; got != want {
		t.Fatalf("after the address appears: %q, want %q", got, want)
	}
	if h.logged("is now present") != 1 {
		t.Errorf("the transition to bound must be logged exactly once; logs: %v", h.logs)
	}
}

// The other half of the incident: a listener whose address disappears is
// closed, not left holding a dead address, and binds again on return.
func TestListenerSetClosesAListenerWhoseAddressVanished(t *testing.T) {
	h := &fakeHost{present: map[string]bool{addrB: true}}
	s := newTestSet(h)
	if err := s.start([]string{addrB, addrL}); err != nil {
		t.Fatal(err)
	}
	defer s.stop()
	bound := s.status()[0].Listening

	h.set(addrB, false)
	s.tick()
	if got := states(s); !strings.Contains(got, addrB+"=waiting") {
		t.Fatalf("a vanished address must go back to waiting, got %q", got)
	}
	if c, err := net.Dial("tcp", bound); err == nil {
		_ = c.Close()
		t.Errorf("the listener for a vanished address must be closed, but %s still accepts", bound)
	}
	if h.logged("no longer present") != 1 {
		t.Errorf("the loss must be logged once; logs: %v", h.logs)
	}

	h.set(addrB, true)
	s.tick()
	if got := states(s); !strings.Contains(got, addrB+"=bound") {
		t.Fatalf("a returned address must bind again, got %q", got)
	}
}

// Loopback is mandatory: it never waits, and neither does a configuration error.
func TestListenerSetStillRefusesWhatWasFatalBefore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		host  *fakeHost
		addrs []string
		want  string
	}{
		{"port taken on an existing address", &fakeHost{present: map[string]bool{addrB: true}, taken: map[string]error{addrB: syscall.EADDRINUSE}},
			[]string{addrB, addrL}, "listen " + addrB},
		{"loopback cannot bind", &fakeHost{present: map[string]bool{addrB: true}, taken: map[string]error{addrL: syscall.EADDRINUSE}},
			[]string{addrB, addrL}, "listen " + addrL},
		{"absent loopback is not waited for", &fakeHost{present: map[string]bool{addrL: false}},
			[]string{addrL}, "listen " + addrL},
		{"permission refused", &fakeHost{present: map[string]bool{addrB: true}, taken: map[string]error{addrB: syscall.EACCES}},
			[]string{addrB}, "listen " + addrB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSet(tc.host)
			err := s.start(tc.addrs)
			if err == nil {
				s.stop()
				t.Fatal("start must fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			for _, ln := range tc.host.opened {
				if c, derr := net.Dial("tcp", ln.Addr().String()); derr == nil {
					_ = c.Close()
					t.Errorf("a failed start must leave nothing listening, %s still accepts", ln.Addr())
				}
			}
		})
	}
}

// An entry that is not host:port is a configuration error, not a wait.
func TestListenerSetMalformedEntryIsFatal(t *testing.T) {
	s := newTestSet(&fakeHost{present: map[string]bool{}})
	s.listen = net.Listen
	if err := s.start([]string{"not-an-address"}); err == nil {
		s.stop()
		t.Fatal("an entry that is not host:port must stop the service")
	}
}

// After startup nothing is fatal: a bind that fails for another reason is
// reported once and retried.
func TestListenerSetLaterBindFailureIsRetriedNotFatal(t *testing.T) {
	h := &fakeHost{present: map[string]bool{addrA: false}, taken: map[string]error{}}
	s := newTestSet(h)
	if err := s.start([]string{addrA, addrL}); err != nil {
		t.Fatal(err)
	}
	defer s.stop()
	h.mu.Lock()
	delete(h.present, addrA)
	h.taken[addrA] = syscall.EADDRINUSE
	h.mu.Unlock()
	s.tick()
	s.tick()
	if got := states(s); !strings.Contains(got, addrA+"=waiting") {
		t.Fatalf("got %q", got)
	}
	if n := h.logged("cannot be bound yet"); n != 1 {
		t.Errorf("logged %d times, want once", n)
	}
	h.mu.Lock()
	delete(h.taken, addrA)
	h.mu.Unlock()
	s.tick()
	if got := states(s); !strings.Contains(got, addrA+"=bound") {
		t.Fatalf("once the port frees the entry must bind, got %q", got)
	}
}

// Hostname and wildcard entries cannot be judged from the interface list, so
// the loss check must leave them alone.
func TestListenerSetNeverClosesAnEntryItCannotJudge(t *testing.T) {
	h := &fakeHost{present: map[string]bool{}}
	s := newTestSet(h)
	if err := s.start([]string{"0.0.0.0:7000", "localhost:7001"}); err != nil {
		t.Fatal(err)
	}
	defer s.stop()
	s.tick()
	if got, want := states(s), "0.0.0.0:7000=bound localhost:7001=bound"; got != want {
		t.Errorf("states = %q, want %q", got, want)
	}
}

func TestCheckableIP(t *testing.T) {
	for addr, want := range map[string]bool{
		"10.1.1.5:7000": true, "[fd00::5]:7000": true,
		"0.0.0.0:7000": false, "[::]:7000": false, ":7000": false,
		"host.example:7000": false, "[fe80::1%en0]:7000": false, "garbage": false,
	} {
		if got := checkableIP(addr) != nil; got != want {
			t.Errorf("checkableIP(%q) = %v, want %v", addr, got, want)
		}
	}
}
