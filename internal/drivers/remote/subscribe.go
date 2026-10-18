package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Subscribing to a peer: the federated half of the event plane (§5.5, §13).
//
// # What a relayed event carries
//
// A peer's cursor belongs to the peer's sequence, so it cannot be handed to a
// caller of THIS service as though it were ours — "resume from cursor N" would
// be ambiguous about whose N. Nor can it simply be discarded, because a caller
// that later talks to that peer directly should be able to resume there rather
// than refetch.
//
// So a relayed event keeps the peer's coordinates in Origin and leaves Cursor
// and Epoch unset for the local hub to stamp. Adopt what the peer said about
// itself; add only what the relaying service is uniquely positioned to know —
// the same split §13.2 uses for source status and F20 for error kinds.
//
// # One hop, here too
//
// The subscription asks the peer for scope=local. Without it, two
// mutually-configured peers would each open a stream to the other and relay
// each other's events indefinitely — §13.1's rule arriving in the event plane,
// where it is easier to violate because a subscription is long-lived and the
// loop would not show up as a failed request.
//
// # A dropped stream is news, not silence
//
// If the connection dies, the subscriber is told: a source.status event marks
// the peer degraded before any reconnection is attempted. Reconnecting quietly
// would leave a caller unable to distinguish "the peer has nothing to say"
// from "we stopped listening", which is §5.7 wearing a stream for a costume.
//
// On reconnect the peer's last seen cursor is offered, so an interruption
// shorter than the peer's retention window resumes exactly. A longer one gets
// the peer's own control.resync, relayed — which is the peer telling this
// caller about a gap in that source alone.

// Subscribe opens a stream of a peer's events (§3, §5.5).
func (d *Driver) Subscribe(ctx context.Context, req fleet.Request, filter driver.SubscribeFilter) (driver.EventStream, error) {
	if _, _, ok := d.bearerFor(req); !ok {
		return nil, ErrNoCallerAuthority
	}
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &peerStream{d: d, req: req, filter: filter, out: make(chan fleet.Event, 64), cancel: cancel}
	go s.run(streamCtx)
	return s, nil
}

type peerStream struct {
	d      *Driver
	req    fleet.Request
	filter driver.SubscribeFilter
	out    chan fleet.Event
	cancel context.CancelFunc
}

func (s *peerStream) Next(ctx context.Context) (fleet.Event, error) {
	select {
	case <-ctx.Done():
		return fleet.Event{}, ctx.Err()
	case ev, ok := <-s.out:
		if !ok {
			return fleet.Event{}, fmt.Errorf("remote: peer stream closed")
		}
		return ev, nil
	}
}

func (s *peerStream) Close() error {
	s.cancel()
	return nil
}

// run keeps a stream to the peer alive, announcing every interruption.
func (s *peerStream) run(ctx context.Context) {
	defer close(s.out)
	var lastCursor int64
	var lastEpoch string
	backoff := 500 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return
		}
		cursor, epoch, err := s.consume(ctx, lastCursor, lastEpoch)
		if cursor > 0 {
			lastCursor, lastEpoch = cursor, epoch
		}
		if ctx.Err() != nil {
			return
		}
		// The peer went away mid-stream. Say so before trying again: a
		// silent reconnect leaves a caller unable to tell a quiet peer
		// from a lost one.
		s.emit(ctx, fleet.Event{
			Machine: s.d.machine,
			Kind:    fleet.EventSourceStatus,
			Payload: fleet.SourceStatus{
				Machine: s.d.machine, Status: fleet.SourceUnreachable,
				Error:      fmt.Sprintf("peer event stream ended: %v", err),
				ObservedAt: s.d.now(),
			},
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// consume reads one connection to exhaustion, returning the last cursor seen.
func (s *peerStream) consume(ctx context.Context, fromCursor int64, fromEpoch string) (int64, string, error) {
	q := url.Values{}
	q.Set("scope", "local") // §13.1 — the peer answers for itself and never forwards
	for _, id := range s.filter.Sessions {
		q.Add("session", id)
	}
	if s.filter.CwdPrefix != "" {
		q.Set("cwdPrefix", s.filter.CwdPrefix)
	}
	if fromCursor > 0 {
		q.Set("cursor", fmt.Sprint(fromCursor))
		q.Set("epoch", fromEpoch)
	}

	eventsPath := "/v1/events?" + q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.d.current()+eventsPath, nil)
	if err != nil {
		return 0, "", err
	}
	token, behalf, ok := s.d.bearerFor(s.req)
	if !ok {
		return 0, "", ErrNoCallerAuthority
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if behalf != "" {
		httpReq.Header.Set("Fleet-On-Behalf-Of", behalf)
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	// No deadline on the request itself: a subscription is meant to stay
	// open, and §4.4's per-call bound describes calls that must return.
	// Liveness is the stream's own business — an idle peer is not a failed
	// one, and cancelling ctx is how a caller ends this.
	resp, err := s.d.roundTrip(httpReq, eventsPath)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, "", decodeError(resp, s.d.machine)
	}

	var lastCursor int64
	var lastEpoch string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev fleet.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		lastCursor, lastEpoch = ev.Cursor, ev.Epoch

		// The peer's coordinates become provenance; the local hub assigns
		// ours. Machine is left as the peer reported it, so an event
		// relayed through several services still names where it happened.
		ev.Origin = &fleet.EventOrigin{Cursor: ev.Cursor, Epoch: ev.Epoch}
		ev.Cursor, ev.Epoch = 0, ""
		if ev.Machine == "" {
			ev.Machine = s.d.machine
		}
		if !s.emit(ctx, ev) {
			return lastCursor, lastEpoch, ctx.Err()
		}
	}
	return lastCursor, lastEpoch, sc.Err()
}

func (s *peerStream) emit(ctx context.Context, ev fleet.Event) bool {
	select {
	case s.out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
