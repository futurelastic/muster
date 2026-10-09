package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Event subscription over the runtime's own event bus (muster #284).
//
// # What the substrate offers
//
// Every opencode server publishes a server-sent-events stream at GET /event:
// one JSON object per event, {"type": ..., "properties": {...}}, covering every
// session that server holds. Unlike the multiplexer driver's control mode, it
// pushes the change itself (a status, a failure) rather than a hint to go and
// look, so this driver maps it directly instead of re-reading state on every
// notification.
//
// # One connection per session process
//
// Each session has its own server (muster #280), so a subscription holds one
// bus connection per server that has a session the filter admits, and fans them
// into one stream. Sessions created while the subscription is open attach the
// moment they exist (announceCreated, before the opening prompt is sent), so
// nothing the new session does is missed. In shared mode — the test seam — there
// is one connection for all.
//
// # Mapping (§5.6: map what has an honest analogue, drop the rest)
//
//	session.created   by Create itself, not the bus          -> session.created
//	session.status    busy | retry | idle                     -> session.state
//	session.idle      the older spelling of idle              -> session.state
//	session.error     the runtime's own failure account       -> the LastTurn of the
//	                                                             idle state it ends in
//	session.deleted   gone on the runtime                     -> session.closed
//	Close, process exit                                       -> session.closed
//
// Everything else the bus publishes — message and part updates, file and LSP
// events, session.updated (a title change; rename is unsupported here),
// permission and question prompts (Respond is unsupported here), heartbeats — is
// DROPPED. None of it is invented into an existing kind. Events about a session
// this driver did not create (a child session the runtime started for itself)
// are dropped too: they are not in this driver's session universe.
//
// A state is emitted only when it CHANGES. The runtime reports a turn's end
// twice (session.status idle, then session.idle); the second is not news.
//
// # A dropped bus is a gap, never silence (§5.7)
//
// When a bus connection ends while its server is still alive, the stream ends
// with an error. The service reads that as a gap — it announces control.resync
// with feed_gap and subscribes again from a fresh baseline — so a consumer is
// told it missed something rather than left to take quiet for idleness. The same
// happens when a consumer falls behind the stream's buffer.
//
// Not detected: a connection that stays open but stops delivering. The runtime
// sends a periodic heartbeat that would expose it, but not every release does,
// and a timeout tuned on a release that does would call a quiet session on the
// others a gap. Over loopback to a child process this driver started, it is a
// risk accepted knowingly, not a case overlooked.
//
// Cursor and Epoch are left zero: the service stamps them (§7.3).

const (
	// subBuffer is how many events a stream holds for a consumer that is not
	// reading. Past it the stream ends with a gap rather than drop one.
	subBuffer = 256
	// busConnectTimeout bounds opening one bus connection.
	busConnectTimeout = 5 * time.Second
	// busExitWait is how long, after a bus connection ends, the driver waits to
	// learn whether the process behind it exited before calling it a gap.
	busExitWait = 2 * time.Second
	// errorNameWait bounds how long a failure waits for the runtime's log line
	// that names it.
	errorNameWait = 400 * time.Millisecond
	// busReadTimeout bounds the read a state event may make of its own.
	busReadTimeout = 5 * time.Second
)

// busMaxEventBytes bounds one event's data. An event past it (a large tool
// output in a message part) is skipped, never decoded: nothing this driver maps
// is anywhere near that large. A variable only so a test can lower it.
var busMaxEventBytes = 8 << 20

// Subscribe opens a stream of this driver's session events (§3, §5.5). The
// filter decides which servers it connects to and which sessions' events pass.
func (d *Driver) Subscribe(ctx context.Context, req fleet.Request, filter driver.SubscribeFilter) (driver.EventStream, error) {
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		return nil, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: "subscribe: this driver has been shut down", Machine: d.machine}
	}

	sctx, cancel := context.WithCancel(ctx)
	s := &subscription{
		d:        d,
		filter:   filter,
		ctx:      sctx,
		cancel:   cancel,
		out:      make(chan fleet.Event, subBuffer),
		failed:   make(chan struct{}),
		attached: map[*server]struct{}{},
		track:    map[string]*sessionTrack{},
	}
	d.subMu.Lock()
	d.subs[s] = struct{}{}
	d.subMu.Unlock()

	// Registered before attaching, so a session created in between is seen by
	// announceCreated rather than falling in the gap between the two.
	servers := map[*server]struct{}{}
	for id, info := range d.knownIDs() {
		if !filter.Matches(id, string(info.cwd)) {
			continue
		}
		if info.srv == nil || (d.shared == nil && info.srv.proc.exited()) {
			continue // a dead or unlaunched session has no bus to listen to
		}
		servers[info.srv] = struct{}{}
	}
	for srv := range servers {
		if err := s.attach(srv); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

// sessionTrack is what one subscription remembers about one session: the last
// state it emitted (so a repeat is not news) and a failure the runtime reported
// that the session's next idle state should carry.
type sessionTrack struct {
	last    string
	pending *fleet.TurnEnd
}

type subscription struct {
	d      *Driver
	filter driver.SubscribeFilter
	ctx    context.Context
	cancel context.CancelFunc
	out    chan fleet.Event

	failOnce sync.Once
	failed   chan struct{}
	failErr  error

	mu       sync.Mutex
	attached map[*server]struct{}
	track    map[string]*sessionTrack
}

// Next implements driver.EventStream. Events already buffered are delivered
// before a failure is reported, so the gap is announced after what preceded it.
func (s *subscription) Next(ctx context.Context) (fleet.Event, error) {
	select {
	case ev := <-s.out:
		return ev, nil
	default:
	}
	select {
	case ev := <-s.out:
		return ev, nil
	case <-ctx.Done():
		return fleet.Event{}, ctx.Err()
	case <-s.failed:
		return s.ended()
	case <-s.ctx.Done():
		return s.ended()
	}
}

// ended is Next's answer once the stream is over: what was buffered first, then
// the failure that ended it, or the cancellation that closed it.
func (s *subscription) ended() (fleet.Event, error) {
	select {
	case ev := <-s.out:
		return ev, nil
	default:
	}
	select {
	case <-s.failed:
		return fleet.Event{}, s.failErr
	default:
		return fleet.Event{}, s.ctx.Err()
	}
}

// Close implements driver.EventStream: it drops every bus connection.
func (s *subscription) Close() error {
	s.d.subMu.Lock()
	delete(s.d.subs, s)
	s.d.subMu.Unlock()
	s.cancel()
	return nil
}

// fail ends the stream with err, once. The service treats the end of a stream
// as a gap and announces it.
func (s *subscription) fail(err error) {
	s.failOnce.Do(func() {
		s.failErr = err
		close(s.failed)
		// A dead stream holds no connections: the service closes it, but a
		// connection kept until then is a connection to a runtime nobody reads.
		s.cancel()
	})
}

// push delivers one event, or ends the stream if the consumer cannot keep up:
// an event dropped here would be a hole nobody could see.
func (s *subscription) push(ev fleet.Event) {
	select {
	case s.out <- ev:
	default:
		s.fail(&fleet.Error{
			Kind:      fleet.ErrorUnreachable,
			Message:   "the driver's event stream fell behind its consumer; events after this point were not delivered",
			Machine:   s.d.machine,
			Retryable: true,
		})
	}
}

// attach opens the bus of srv for this subscription, once per server.
func (s *subscription) attach(srv *server) error {
	s.mu.Lock()
	if _, ok := s.attached[srv]; ok {
		s.mu.Unlock()
		return nil
	}
	s.attached[srv] = struct{}{}
	s.mu.Unlock()

	body, err := s.d.openBus(s.ctx, srv)
	if err != nil {
		s.mu.Lock()
		delete(s.attached, srv)
		s.mu.Unlock()
		return err
	}
	go s.read(srv, body)
	return nil
}

// openBus connects to srv's event stream and returns its body once the runtime
// has accepted the connection.
func (d *Driver) openBus(ctx context.Context, srv *server) (io.ReadCloser, error) {
	rctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(busConnectTimeout, cancel)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, srv.baseURL+"/event", nil)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, fmt.Errorf("opencode: building the event request: %w", err)
	}
	req.SetBasicAuth(srv.username, srv.password)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := d.client.Do(req)
	timer.Stop()
	if err != nil {
		cancel()
		return nil, &fleet.Error{
			Kind:      fleet.ErrorUnreachable,
			Message:   fmt.Sprintf("no answer from the local opencode server's event bus: %v", err),
			Machine:   d.machine,
			Retryable: true,
		}
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return &busBody{ReadCloser: resp.Body, cancel: cancel}, nil
	case resp.StatusCode == http.StatusUnauthorized:
		resp.Body.Close()
		cancel()
		return nil, &fleet.Error{
			Kind:    fleet.ErrorUnauthorized,
			Message: "opencode: the local server rejected this driver's own credential on its event bus",
			Machine: d.machine,
		}
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		cancel()
		// A runtime release with no event bus cannot be asked again into
		// having one: a statement about the substrate (§5.6), not the moment.
		return nil, fmt.Errorf("%w: this opencode server has no event bus (GET /event answered 404)", driver.ErrUnsupported)
	default:
		resp.Body.Close()
		cancel()
		return nil, &fleet.Error{
			Kind:      fleet.ErrorUnreachable,
			Message:   fmt.Sprintf("opencode: GET /event answered %d", resp.StatusCode),
			Machine:   d.machine,
			Retryable: true,
		}
	}
}

// busBody ties a response body to the cancel of its request context.
type busBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *busBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// busEvent is one bus event's envelope.
type busEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// busProps is the union of the properties of the events this driver maps. Each
// event type reads only the fields it documents; the rest stay zero.
type busProps struct {
	SessionID string `json:"sessionID"`
	Info      *struct {
		ID string `json:"id"`
	} `json:"info"`
	Status *wireStatus         `json:"status"`
	Error  *wireAssistantError `json:"error"`
}

// read consumes one server's bus until it ends, then decides whether the end
// was a gap.
func (s *subscription) read(srv *server, body io.ReadCloser) {
	defer body.Close()
	err := readSSE(body, func(data []byte) {
		var ev busEvent
		if json.Unmarshal(data, &ev) != nil || ev.Type == "" {
			return
		}
		s.handle(srv, ev)
	})
	if s.ctx.Err() != nil {
		return
	}
	s.d.busEnded(s, srv, err)
}

// readSSE parses a server-sent-events body, calling emit once per event with its
// data. Fields other than data are ignored. An event whose data exceeds
// busMaxEventBytes is skipped. It returns when the body ends or errors; a clean
// end is io.EOF.
func readSSE(r io.Reader, emit func(data []byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var data []byte
	over := false
	for {
		line, err := readBoundedLine(br)
		if err != nil {
			return err
		}
		if len(line) == 0 { // blank line: the event is complete
			if !over && len(data) > 0 {
				emit(data)
			}
			data, over = data[:0], false
			continue
		}
		if line[0] == ':' {
			continue // comment
		}
		field, value, _ := strings.Cut(string(line), ":")
		if field != "data" {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if over || len(data)+len(value)+1 > busMaxEventBytes {
			over = true
			continue
		}
		if len(data) > 0 {
			data = append(data, '\n')
		}
		data = append(data, value...)
	}
}

// readBoundedLine reads one line without its terminator. A line longer than
// busMaxEventBytes is consumed in full but returned truncated, so one huge event
// cannot grow memory without bound.
func readBoundedLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line) < busMaxEventBytes {
			line = append(line, chunk...)
		}
		switch {
		case err == nil:
			return []byte(strings.TrimRight(string(line), "\r\n")), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// handle maps one bus event. It runs on the reader goroutine of its server.
func (s *subscription) handle(srv *server, ev busEvent) {
	var p busProps
	if len(ev.Properties) > 0 {
		_ = json.Unmarshal(ev.Properties, &p)
	}
	id := p.SessionID
	if strings.HasPrefix(ev.Type, "session.") && id == "" && p.Info != nil {
		id = p.Info.ID // session.* events carry the session as info; message.* carry a MESSAGE here
	}
	if id == "" {
		return
	}
	known, ok := s.d.wasSeen(id)
	if !ok || known.srv != srv || !s.filter.Matches(id, string(known.cwd)) {
		return
	}
	ref := fleet.SessionRef{Machine: s.d.machine, ID: id, Name: known.name}

	switch ev.Type {
	case "session.status":
		if p.Status == nil {
			return
		}
		switch p.Status.Type {
		case "busy", "retry":
			key := p.Status.Type
			if p.Status.Type == "retry" {
				key = fmt.Sprintf("retry:%d", p.Status.Attempt)
			}
			st := classify(true, *p.Status)
			s.state(ref, srv, key, st)
		case "idle":
			s.idle(ref, srv)
		default:
			// A status type this driver does not know is not mapped into a
			// guessed one; classify would call it unknown, which would claim a
			// change nobody can name.
		}
	case "session.idle":
		s.idle(ref, srv)
	case "session.error":
		s.failure(ref, srv, p.Error)
	case "session.deleted":
		// The runtime's own word that the session is gone, the same authority
		// Close's 404 path acts on (#78).
		s.d.forgetSeen(id)
		if s.d.shared == nil {
			srv.gone.Store(true)
		}
		s.d.discardServer(srv)
		s.d.publish(known, ref, fleet.Event{
			Machine: s.d.machine,
			Kind:    fleet.EventSessionClosed,
			Payload: fleet.SessionStatePayload{Ref: ref, State: fleet.InferredState(fleet.StatusDead,
				"the runtime reported the session deleted", nil)},
		})
	}
}

// state emits a session.state if it differs from the last one emitted.
func (s *subscription) state(ref fleet.SessionRef, srv *server, key string, st fleet.SessionState) {
	s.mu.Lock()
	t := s.track[ref.ID]
	if t == nil {
		t = &sessionTrack{}
		s.track[ref.ID] = t
	}
	if t.last == key {
		s.mu.Unlock()
		return
	}
	t.last = key
	s.mu.Unlock()
	st.Sandbox = srv.sandbox
	s.push(fleet.Event{
		Machine: s.d.machine,
		Kind:    fleet.EventSessionState,
		Payload: fleet.SessionStatePayload{Ref: ref, State: st},
	})
}

// idle emits the idle state a turn ends in. Its LastTurn is the failure the
// runtime reported for that turn, if it did, else what the session's own message
// record says — the same read State makes.
func (s *subscription) idle(ref fleet.SessionRef, srv *server) {
	s.mu.Lock()
	t := s.track[ref.ID]
	if t != nil && t.last == "idle" {
		s.mu.Unlock()
		return
	}
	var turn *fleet.TurnEnd
	if t != nil {
		turn, t.pending = t.pending, nil
	}
	s.mu.Unlock()

	st := classify(false, wireStatus{})
	if turn != nil {
		st.LastTurn = turn
	} else {
		ctx, cancel := context.WithTimeout(s.ctx, busReadTimeout)
		st.LastTurn = s.d.lastTurnFailure(ctx, srv, ref.ID)
		cancel()
	}
	s.state(ref, srv, "idle", st)
}

// failure records a turn failure the runtime reported. If the session is not
// mid-turn it is emitted at once as an idle state carrying it (a failure before
// any turn began, such as a model the provider does not know); otherwise it
// waits for the idle state that ends the turn.
func (s *subscription) failure(ref fleet.SessionRef, srv *server, e *wireAssistantError) {
	if e == nil {
		return
	}
	turn := s.d.turnFailure(s.ctx, srv, e, time.Now(), true)

	s.mu.Lock()
	t := s.track[ref.ID]
	if t == nil {
		t = &sessionTrack{}
		s.track[ref.ID] = t
	}
	midTurn := t.last == "busy" || strings.HasPrefix(t.last, "retry:")
	if midTurn {
		t.pending = turn
		s.mu.Unlock()
		return
	}
	t.last = "" // a failure is news even to a session already reported idle
	s.mu.Unlock()

	st := classify(false, wireStatus{})
	st.LastTurn = turn
	s.state(ref, srv, "idle", st)
}

// turnFailure builds the TurnEnd for a failure the runtime reported. The
// runtime's API names a configuration or model error only "UnknownError"; its
// own log names the real one, so when the API's name is that placeholder the
// log's is carried instead (muster #284, session-abstraction §5.8: the
// runtime's account of itself). wait lets a live failure give the log a moment
// to catch up with the bus.
func (d *Driver) turnFailure(ctx context.Context, srv *server, e *wireAssistantError, at time.Time, wait bool) *fleet.TurnEnd {
	name := e.Name
	if name == "" || genericErrorNames[name] {
		var better string
		if wait {
			better = srv.logs.waitErrorName(ctx, at, errorNameWait)
		} else {
			better = srv.logs.errorName(at)
		}
		if better != "" {
			name = better
		}
	}
	reason := name
	if e.Data.Message != "" {
		reason += ": " + e.Data.Message
	}
	retryable := e.Name == "APIError" && e.Data.IsRetryable
	return &fleet.TurnEnd{Outcome: "failed", Reason: reason, Retryable: retryable}
}

// busEnded decides what the end of srv's bus connection means.
func (d *Driver) busEnded(s *subscription, srv *server, cause error) {
	if d.shared == nil && srv.proc != nil {
		// The connection ends when the process does. Wait briefly for the
		// process to be reaped so a close or a crash is not mistaken for a gap.
		select {
		case <-srv.proc.done:
		case <-time.After(busExitWait):
		case <-s.ctx.Done():
			return
		}
		d.mu.RLock()
		shut := d.closed
		d.mu.RUnlock()
		switch {
		case srv.gone.Load() || shut:
			return // closed on purpose, announced by whoever closed it
		case srv.proc.exited():
			d.announceProcessGone(srv)
			return
		}
	}
	if errors.Is(cause, io.EOF) || cause == nil {
		cause = errors.New("the server closed the stream")
	}
	s.fail(&fleet.Error{
		Kind:      fleet.ErrorUnreachable,
		Message:   fmt.Sprintf("the runtime's event bus connection ended (%v); changes after this point were not seen", cause),
		Machine:   d.machine,
		Retryable: true,
	})
}

// publish hands ev to every open subscription whose filter admits the session,
// and forgets what each remembered about a session that closed.
func (d *Driver) publish(known knownSession, ref fleet.SessionRef, ev fleet.Event) {
	d.subMu.Lock()
	subs := make([]*subscription, 0, len(d.subs))
	for s := range d.subs {
		subs = append(subs, s)
	}
	d.subMu.Unlock()
	for _, s := range subs {
		if !s.filter.Matches(ref.ID, string(known.cwd)) {
			continue
		}
		if ev.Kind == fleet.EventSessionClosed {
			s.mu.Lock()
			delete(s.track, ref.ID)
			s.mu.Unlock()
		}
		s.push(ev)
	}
}

// announceCreated is called by Create once a session exists and before its
// opening prompt is sent. Each admitting subscription connects to the session's
// server first, so the prompt's first change is not missed, then reports the
// session.
func (d *Driver) announceCreated(srv *server, info knownSession, ref fleet.SessionRef) {
	d.subMu.Lock()
	subs := make([]*subscription, 0, len(d.subs))
	for s := range d.subs {
		subs = append(subs, s)
	}
	d.subMu.Unlock()

	started := info.startedAt
	sess := fleet.Session{
		SessionRef: ref,
		StartedAt:  &started,
		Runtime:    d.runtime,
		Cwd:        info.cwd,
		Agent:      fleet.AgentId(info.agent),
		State:      classify(false, wireStatus{}),
	}
	sess.State.Sandbox = srv.sandbox
	for _, s := range subs {
		if !s.filter.Matches(ref.ID, string(info.cwd)) {
			continue
		}
		if err := s.attach(srv); err != nil {
			// Not Create's failure: the session exists. The subscription is the
			// one that cannot see it, and says so by ending.
			s.fail(err)
			continue
		}
		s.push(fleet.Event{Machine: d.machine, Kind: fleet.EventSessionCreated, Payload: sess})
	}
}

// announceClosed reports a session this driver just closed.
func (d *Driver) announceClosed(srv *server, known knownSession, ref fleet.SessionRef) {
	if d.shared == nil {
		srv.gone.Store(true)
	}
	d.publish(known, ref, fleet.Event{
		Machine: d.machine,
		Kind:    fleet.EventSessionClosed,
		Payload: fleet.SessionStatePayload{Ref: ref, State: fleet.InferredState(fleet.StatusDead,
			"the session was closed", nil)},
	})
}

// announceProcessGone reports every session on srv as closed, once, when the
// process behind them exited without anyone closing it. The sessions stay in the
// driver's cache, as State and List already report them dead.
func (d *Driver) announceProcessGone(srv *server) {
	if !srv.gone.CompareAndSwap(false, true) {
		return
	}
	for id, info := range d.knownIDs() {
		if info.srv != srv {
			continue
		}
		ref := fleet.SessionRef{Machine: d.machine, ID: id, Name: info.name}
		d.publish(info, ref, fleet.Event{
			Machine: d.machine,
			Kind:    fleet.EventSessionClosed,
			Payload: fleet.SessionStatePayload{Ref: ref, State: fleet.InferredState(fleet.StatusDead,
				"the session's runtime process has exited", nil)},
		})
	}
}

// endSubscriptions ends every open stream; Shutdown calls it.
func (d *Driver) endSubscriptions() {
	d.subMu.Lock()
	subs := make([]*subscription, 0, len(d.subs))
	for s := range d.subs {
		subs = append(subs, s)
	}
	d.subMu.Unlock()
	for _, s := range subs {
		s.fail(&fleet.Error{Kind: fleet.ErrorUnreachable, Message: "this driver has been shut down", Machine: d.machine})
	}
}
