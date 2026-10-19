package tmux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Event subscription over the multiplexer's control mode.
//
// # What the substrate actually offers, measured
//
// Control mode is a real push channel — no polling, notifications arrive
// sub-second. But its scoping is not what a naive reading assumes, and the
// difference decides the whole design. Probed directly:
//
//	%output for the ATTACHED session ........................ delivered
//	%output for a sibling session .......................... NOT delivered
//	%subscription-changed (refresh-client -B, %pane) ....... attached only
//	%subscription-changed targeting a sibling's pane by id . NOT delivered
//	%sessions-changed / %unlinked-window-add / -close ...... DELIVERED (global)
//
// So content events are per-attachment and lifecycle events are global.
// That asymmetry is the entire architecture:
//
//   - ONE always-on control client yields every session appearing and
//     disappearing, fleet-wide, regardless of which session it is attached
//     to.
//   - Content changes need a client attached to the session in question, so
//     those are opened on demand — when a req subscribes — and reaped on
//     unsubscribe. Cost is O(subscribers), typically one or two, rather than
//     O(sessions).
//
// The rejected alternative was a client per session at startup. It works and
// it is simpler, but it restores exactly the per-session process cost the
// batched enumeration exists to avoid, and it buys fidelity nobody asked
// for: %output is a raw byte firehose, and this driver does not want bytes.
//
// Verified separately, because it would have been disqualifying: attaching a
// control client does NOT renegotiate the session's size. A session created
// at 200x50 measured 200x50 before attach, during attach, and after detach.
// Subscribing to somebody's live session does not reflow their terminal.
//
// # Push-triggered pull
//
// Notifications are used only as CHANGE TRIGGERS. When one arrives, this
// code re-runs the ordinary batched enumeration and diffs the result. It
// never parses %output's payload to infer what happened.
//
// That is deliberate, and it is not the polling §5.5 forbids: reads are
// edge-triggered by the substrate, never timer-triggered. Nothing wakes up
// on an interval, and an idle fleet costs nothing. What it buys is that
// exactly one code path decides what a session's state is — the same
// classifier List and State use. A second, stream-only interpretation of
// pane bytes would be a second source of truth about status, free to drift
// from the first, and the two would disagree only under load, which is when
// anybody would care.
//
// # FINDING: Event carries fields a driver cannot fill
//
// fleet.Event has Cursor and Epoch, which §7.3 defines as assigned by "each
// service instance" — a driver has no access to either, and two drivers
// under one service must not be minting competing cursor sequences. This
// driver therefore leaves both zero and the service is expected to stamp
// them on the way out. Recorded because the type's shape implies drivers
// fill it in, and any driver that tried would be wrong in a way that only
// shows up as a subscriber silently missing a resync.

// lifecycleKey is the conns map key for the lifecycle client. It is not a
// valid session name, so it can never collide with one.
const lifecycleKey = "\x00lifecycle"

const (
	// coalesceWindow batches a burst of notifications into one enumeration.
	// An agent producing output emits many %output notifications per
	// second; the state it is in changes far more slowly. Without this,
	// a chatty session would drive one full enumeration per output chunk.
	coalesceWindow = 150 * time.Millisecond

	// streamRecordMaxAge is how stale a session's record-derived fields
	// (conversation, turns, usage) may be in a listing the stream takes, when
	// that session's screen has not changed since they were read (#339). The
	// stream lists the whole fleet after every burst, which without this
	// statted every session's transcript several times a second for one busy
	// session. A record that grows behind a static screen is noticed within
	// this bound; a session that is actually working changes its screen and is
	// read every time.
	streamRecordMaxAge = 5 * time.Second
)

// ctlNote is one parsed control-mode notification: the leading %name and
// its whitespace-separated arguments. The payload after the arguments is
// deliberately discarded — see "push-triggered pull" above.
type ctlNote struct {
	Name string
	Args []string
}

// ctlConn is one live control-mode client.
type ctlConn interface {
	Notes() <-chan ctlNote
	Close() error
}

// maxContentClients bounds how many per-session control clients ONE
// subscription may open.
//
// The number is chosen against the multiplexer server's descriptor budget,
// not against this process's: a server holding ~80 descriptors idle was
// pushed past its limit by 62 of these, and the failure surfaced as every
// new client being refused — including a human's terminal. 16 leaves an
// order of magnitude of headroom for several concurrent subscribers plus
// everything else on the machine.
const maxContentClients = 16

// maxFutileReattach bounds how many times ONE subscription re-dials a content
// client for the same session id without the client it opened ever delivering a
// notification.
//
// The re-attach this bounds is driven by a client's death (#172), and a client
// that attaches and dies at once produces one death per attempt — so "re-attach
// once per death" is not by itself a bound: a session that keeps enumerating but
// whose attachment never survives would be re-dialled every coalesce window,
// forever, with a full enumeration behind each one. That is the re-dial storm the
// rejected option was rejected for, arriving by a different route, so it is
// bounded here rather than argued away.
//
// The budget is renewable, and what renews it is the only evidence available
// without consulting a clock: a client that delivered at least one notification
// demonstrably worked, so its eventual death is an ordinary session exit and
// costs nothing. Only CONSECUTIVE silent attempts count against the budget.
// Exhausting it is said out loud and then stops — the session keeps its
// fleet-wide triggers and loses only its own push latency, which is exactly the
// behaviour it had before #172 was fixed, for that one session.
const maxFutileReattach = 3

// ctlDialer opens a control-mode client attached to one session. Injected
// so tests can drive subscription logic without spawning a multiplexer.
type ctlDialer func(ctx context.Context, bin, session string) (ctlConn, error)

// withCtlDialer injects a fake control-mode transport. Unexported: tests only.
func withCtlDialer(f ctlDialer) Option { return func(d *Driver) { d.dial = f } }

// realCtlConn attaches a control-mode client as a subprocess.
type realCtlConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	notes chan ctlNote
	once  sync.Once
}

func dialReal(ctx context.Context, bin, session string) (ctlConn, error) {
	// -C is control mode. Attaching is what scopes content notifications to
	// this session; there is no unattached form that sees them.
	cmd := exec.Command(bin, "-C", "attach-session", "-t", session)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stdin must stay open for the lifetime of the client: closing it is
	// how the client is told to detach, so it doubles as the shutdown
	// signal in Close.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &realCtlConn{cmd: cmd, stdin: stdin, notes: make(chan ctlNote, 64)}
	go func() {
		defer close(c.notes)
		sc := bufio.NewScanner(stdout)
		// Control-mode lines can be long: %output carries a pane's entire
		// output chunk octal-escaped on one line.
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			note, ok := parseNote(sc.Text())
			if !ok {
				continue
			}
			select {
			case c.notes <- note:
			default:
				// A full buffer means the consumer is behind. Dropping is
				// correct here rather than blocking: these are triggers,
				// not data, and a dropped trigger is covered by the next
				// one — every notification causes the same full
				// re-enumeration. Blocking would stall the multiplexer's
				// client instead.
			}
		}
	}()
	return c, nil
}

func (c *realCtlConn) Notes() <-chan ctlNote { return c.notes }

func (c *realCtlConn) Close() error {
	c.once.Do(func() {
		// Closing stdin asks the client to detach cleanly; killing is the
		// fallback if it does not.
		_ = c.stdin.Close()
		done := make(chan struct{})
		go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
		}
	})
	return nil
}

// parseNote splits a control-mode line into its notification name and
// arguments. Lines that are not notifications (command output, %begin/%end
// bodies) are rejected.
func parseNote(line string) (ctlNote, bool) {
	if !strings.HasPrefix(line, "%") {
		return ctlNote{}, false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ctlNote{}, false
	}
	name := strings.TrimPrefix(fields[0], "%")
	if name == "" {
		return ctlNote{}, false
	}
	return ctlNote{Name: name, Args: fields[1:]}, true
}

// isLifecycleNote reports whether a notification means the set of sessions
// may have changed. These arrive on ANY attached client, fleet-wide.
func isLifecycleNote(name string) bool {
	switch name {
	case "sessions-changed", "unlinked-window-add", "unlinked-window-close",
		"window-add", "window-close", "session-closed", "exit":
		return true
	}
	return false
}

// isContentNote reports whether a notification means a session's screen may
// have changed. Scoped to the client's attached session.
func isContentNote(name string) bool {
	switch name {
	case "output", "subscription-changed", "layout-change", "window-renamed":
		return true
	}
	return false
}

// eventStream implements driver.EventStream over control-mode clients.
type eventStream struct {
	d       *Driver
	req     fleet.Request
	filter  driver.SubscribeFilter
	cancel  context.CancelFunc
	out     chan fleet.Event
	errc    chan error
	trigger chan struct{}

	// quota tracking, owned by run() alone — no lock.
	quotaKnown bool
	quotaBlock *fleet.QuotaBlock

	// credential generation tracking (#12), owned by run() alone — no lock.
	// See run()'s own comment for why, unlike quota, the FIRST read never
	// emits: there is no unremarkable baseline generation to stay silent
	// about, so a first read only seeds the comparison.
	credentialKnown bool
	credentialGen   *fleet.Timestamp

	// senders counts the goroutines OTHER than run() that write to out — today
	// only the lifecycle supervisor. run() owns closing out, and must not do so
	// while one of them can still send: closing a channel under a concurrent
	// send is a data race, and outside the race detector the same interleaving
	// is a send on a closed channel, which panics the whole service rather
	// than ending one subscription (#162). Every such sender returns once the
	// stream context is cancelled, and run() itself only returns after that,
	// so the wait is bounded.
	senders sync.WaitGroup

	mu    sync.Mutex
	conns map[string]ctlConn // session id -> content client
	// reattach holds the re-attach bookkeeping for session ids whose content
	// client died on its own (#172). Written by the content pumps, read and
	// cleared by run(), under mu.
	reattach map[string]reattachState
	closed   bool
	closeCh  chan struct{}
}

// reattachState is one session id's re-attach bookkeeping (#172).
//
// It exists because the engine's diff cannot see the case it covers. A content
// client dies with its session; a session that exits and is re-created under the
// same id between two reads is never observed to close, so the id never leaves
// `known` and the diff never takes its !had branch — the only branch that opens a
// content client. The pump that died is the only thing that knows, so it leaves a
// mark here and the next read acts on it.
type reattachState struct {
	// pending is set by a pump whose client died while the stream still
	// believed that client was the live one for this id. Spent by the next
	// read, whether or not it re-attaches: one attempt per observed death,
	// never one per pass.
	pending bool
	// futile counts CONSECUTIVE re-dials for this id whose client delivered no
	// notification at all before dying. Reset by any client that delivered
	// one. See maxFutileReattach.
	futile int
}

// Subscribe opens a live event stream (§3, §5.5).
//
// Sessions matching the filter at subscribe time get a content client each;
// sessions that appear later and match are picked up as the lifecycle client
// reports them. Both kinds of notification are triggers for the same
// enumerate-and-diff.
func (d *Driver) Subscribe(ctx context.Context, req fleet.Request, filter driver.SubscribeFilter) (driver.EventStream, error) {
	// The lifecycle client must attach to *some* session, because there is
	// no unattached control-mode form that receives notifications. With no
	// sessions on the machine there is nothing to attach to — and also
	// nothing to report, so this is not as circular as it looks: the first
	// session's creation is missed, and every subsequent change is seen.
	// Documented rather than papered over; closing it would need a
	// keep-alive session this driver has no business creating.
	listCtx, cancelList := d.bounded(ctx)
	rows, _, err := d.enumerate(listCtx)
	cancelList()
	if err != nil {
		return nil, fmt.Errorf("subscribe: enumerating: %w", err)
	}
	if len(rows) == 0 {
		// ErrNotReady, not ErrUnsupported. This substrate streams perfectly
		// well; it has nothing to attach to at this instant, and the two
		// answers ask opposite things of the caller. Reported as the latter,
		// the service's pump gave up permanently and every subscriber held an
		// open, empty stream while the machine went on to start sessions
		// nobody was told about — see driver.ErrNotReady.
		return nil, fmt.Errorf("subscribe: no sessions to attach a control client to; "+
			"control mode has no unattached form (%w)", driver.ErrNotReady)
	}

	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &eventStream{
		d:        d,
		req:      req,
		filter:   filter,
		cancel:   cancel,
		out:      make(chan fleet.Event, 64),
		errc:     make(chan error, 1),
		conns:    map[string]ctlConn{},
		reattach: map[string]reattachState{},
		closeCh:  make(chan struct{}),
		trigger:  make(chan struct{}, 1),
	}

	trigger := s.trigger

	// The lifecycle client: attached to an arbitrary session, listened to
	// for fleet-wide session appearance and disappearance.
	life, err := d.dial(streamCtx, d.bin, rows[0].session)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("subscribe: opening lifecycle client: %w", err)
	}
	s.senders.Add(1)
	go func() {
		defer s.senders.Done()
		s.superviseLifecycle(streamCtx, trigger, life)
	}()

	// Content clients for the sessions this subscription cares about — and
	// only those. This is where filter granularity turns into cost: one
	// connection per watched session, so a caller that named two sessions
	// opens two, not one per session on the machine.
	//
	// Bounded, because the cost is not paid in this process. Each client is a
	// connection to a multiplexer server that other tools — launchers,
	// supervisors, a human's terminal — also depend on, and that server has a
	// file-descriptor budget shared by all of them. An unbounded subscription
	// therefore does not degrade itself, it degrades the MACHINE: measured
	// during an incident where one forgotten subscriber held 62 clients on a
	// 69-session host, exhausted the server's descriptors, and left every new
	// attach failing with "server exited unexpectedly" — while every session
	// was in fact alive and healthy.
	//
	// Capping is safe because of how this driver uses notifications. They are
	// triggers, never data: ANY notification causes a full enumerate-and-diff
	// across every session (see "push-triggered pull"). So watching a subset
	// still detects changes everywhere; what degrades is latency on sessions
	// that are quiet while the watched ones are also quiet — and lifecycle
	// events, which are global, are unaffected either way.
	//
	// A caller that names sessions gets exactly those, up to the cap, because
	// naming is a statement about what matters.
	opened := 0
	for _, r := range rows {
		if !filter.Matches(r.session, r.cwd) {
			continue
		}
		if opened >= maxContentClients {
			// Said out loud rather than silently truncated: a subscriber
			// that believes it has per-session triggers for everything and
			// does not is exactly the "confident report on evidence the
			// reporter manufactured" this project keeps meeting.
			log.Printf("tmux: subscription watching %d sessions, capped at %d content clients; "+
				"changes are still detected fleet-wide (notifications are triggers, not data), "+
				"latency may rise for unwatched sessions",
				len(rows), maxContentClients)
			break
		}
		conn, err := d.dial(streamCtx, d.bin, r.session)
		if err != nil {
			continue // a session that vanished between enumerate and dial
		}
		s.mu.Lock()
		s.conns[r.session] = conn
		s.mu.Unlock()
		opened++
		go s.pumpContent(streamCtx, r.session, conn)
	}
	s.mu.Lock()
	s.conns[lifecycleKey] = life
	s.mu.Unlock()

	// Seed the baseline BEFORE returning, not inside the engine goroutine.
	//
	// If the engine took its own first reading, everything that happened
	// between Subscribe returning and that reading would be absorbed into
	// the baseline and never reported — a subscriber would hold a stream it
	// believes is complete, with a silent hole at the front of it. §7.3
	// draws exactly this line: "announced gaps are recoverable; silent gaps
	// are not." A gap here cannot even be announced, because nothing knows
	// it happened.
	//
	// Seeding synchronously makes the guarantee stateable: every change
	// after Subscribe returns is either delivered or is a bug.
	seed := map[string]fleet.Session{}
	if base, err := d.List(ctx, req, driver.ListFilter{CwdPrefix: filter.CwdPrefix}); err == nil {
		for _, sess := range base.Items() {
			if !filter.Matches(sess.ID, string(sess.Cwd)) {
				continue
			}
			seed[sess.ID] = sess
		}
	}

	go s.run(streamCtx, trigger, seed)
	return s, nil
}

// superviseLifecycle keeps a lifecycle client alive across the death of
// whichever session it happens to be attached to.
//
// This exists because of an asymmetry that is easy to miss: the lifecycle
// client is attached to a session the driver does not own and did not
// create. Control mode has no unattached form, so *some* arbitrary session
// has to host the connection — and when that session exits, the client exits
// with it.
//
// Without supervision the failure is silent and total. The client's
// notification channel closes, the pump returns, and the stream keeps
// running while receiving nothing. Every subscriber then sees a healthy,
// open, permanently empty stream. That is the worst failure shape available
// to this design, because a stream delivering nothing is indistinguishable
// from a fleet in which nothing is happening — the observer cannot tell
// "quiet" from "deaf", which is §5.7's confusion wearing a different hat.
//
// So: when the host session dies, re-attach to another one, and say so.
// If nothing can be attached to, report the source degraded rather than
// going quiet. A subscriber that is told it is degraded can refetch; a
// subscriber that is told nothing cannot.
func (s *eventStream) superviseLifecycle(ctx context.Context, trigger chan<- struct{}, first ctlConn) {
	conn := first
	for {
		// Drain this client until it dies or the stream is closed.
		pump(conn.Notes(), trigger, isLifecycleNote, ctx)
		if ctx.Err() != nil {
			return
		}

		// The host session went away. Everything about the fleet is now
		// unobserved until a new client is attached, so say so before
		// trying.
		s.emit(ctx, fleet.Event{
			Machine: s.d.machine,
			Kind:    fleet.EventSourceStatus,
			Payload: fleet.SourceStatus{
				Machine:    s.d.machine,
				Status:     fleet.SourceDegraded,
				Error:      "lifecycle control client lost its host session; re-attaching",
				ObservedAt: s.d.now(),
			},
		})
		// A trigger too: the set of sessions demonstrably just changed.
		select {
		case trigger <- struct{}{}:
		default:
		}

		// Reap the client that died before replacing it. Its notifications
		// ending does not mean its process was waited on: for the real
		// transport Close is the only place that happens, and this client is
		// about to be overwritten in s.conns, so the stream's own Close will
		// never see it. Skipping this leaves one unreaped process per
		// host-session death, per live subscription, for the life of the
		// service. Close is idempotent, so a stream Close racing this one is
		// harmless. It comes after the announcement because Close may wait
		// briefly for a client that has not fully exited, and a subscriber
		// should not learn it is degraded late because of that.
		_ = conn.Close()

		next, err := s.reattachLifecycle(ctx)
		if err != nil {
			return
		}
		conn = next
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[lifecycleKey] = conn
		s.mu.Unlock()
	}
}

// reattachLifecycle finds any surviving session and attaches to it, backing
// off while none exists. It gives up only when the stream is closed.
func (s *eventStream) reattachLifecycle(ctx context.Context) (ctlConn, error) {
	backoff := 200 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		listCtx, cancel := s.d.bounded(ctx)
		rows, _, err := s.d.enumerate(listCtx)
		cancel()
		if err == nil {
			for _, r := range rows {
				if conn, derr := s.d.dial(ctx, s.d.bin, r.session); derr == nil {
					return conn, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// pump forwards interesting notifications to the trigger channel, coalescing
// by virtue of the channel holding at most one pending trigger.
//
// It reports whether it consumed ANY notification — interesting or not — which
// is the only clock-free evidence that the client on the other end ever worked.
// Deliberately not "forwarded a trigger": a client's own filter decides what is
// interesting, and a client that spoke and was ignored is still a client that
// attached successfully. Callers with nothing to decide from it ignore it; see
// maxFutileReattach for the one that does not.
func pump(notes <-chan ctlNote, trigger chan<- struct{}, want func(string) bool, ctx context.Context) bool {
	spoke := false
	for {
		select {
		case <-ctx.Done():
			return spoke
		case n, ok := <-notes:
			if !ok {
				return spoke
			}
			spoke = true
			if !want(n.Name) {
				continue
			}
			select {
			case trigger <- struct{}{}:
			default: // one pending trigger is as good as ten
			}
		}
	}
}

// run is the stream's engine: wait for a trigger, let a burst settle,
// enumerate once, diff, emit.
func (s *eventStream) run(ctx context.Context, trigger <-chan struct{}, known map[string]fleet.Session) {
	// The last writer out closes the channel — see senders.
	defer func() {
		s.senders.Wait()
		close(s.out)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
		}

		// Let a burst settle. An agent mid-turn emits many notifications
		// per second; its state changes far more slowly.
		select {
		case <-ctx.Done():
			return
		case <-time.After(coalesceWindow):
		}
		drain(trigger)

		cur, err := s.d.List(ctx, s.req, driver.ListFilter{CwdPrefix: s.filter.CwdPrefix, RecordMaxAge: streamRecordMaxAge})
		if err != nil {
			continue
		}
		if !cur.Complete() {
			// §5.7 on the event stream: a read that did not fully succeed
			// must not be diffed as though it had, or every unreadable
			// session emits a spurious "closed".
			for _, src := range cur.Sources() {
				if src.Status != fleet.SourceOK {
					s.emit(ctx, fleet.Event{
						Machine: s.d.machine,
						Kind:    fleet.EventSourceStatus,
						Payload: src,
					})
				}
			}
			continue
		}

		// The account's own state, before the per-session diff. A supervisor
		// that acts on this stops dispatching; a supervisor that waits for
		// the session diff learns the same thing one stalled session at a
		// time, which is how the fact was learned 48 times before.
		//
		// The first pass announces a block that is already in force — a
		// subscriber connecting mid-outage must not have to wait for a
		// transition that already happened — but says nothing when there is
		// none, because "not blocked" is the unremarkable case and an event
		// for it on every new subscription is noise.
		if q, _ := s.d.quotaBlock(); (q != nil) != (s.quotaBlock != nil) || !s.quotaKnown {
			if q != nil || s.quotaKnown {
				s.emit(ctx, fleet.Event{
					Machine: s.d.machine,
					Kind:    fleet.EventMachineQuota,
					Payload: fleet.MachineQuotaPayload{
						Machine: s.d.machine,
						Blocked: q != nil,
						Quota:   q,
					},
				})
			}
			s.quotaKnown = true
			s.quotaBlock = q
		}

		// The credential store's own generation (#12), compared against what
		// this stream last saw — the second machine-level fact on this loop
		// and, unlike quota, one with no unremarkable baseline to stay quiet
		// about: every machine has SOME generation the instant a store
		// exists, so re-announcing "the current one" on the seed read would
		// make a machine.account event fire on every new subscription rather
		// than only at an actual transition. The first read therefore only
		// seeds the comparison and never emits; only an observed CHANGE
		// after that is a transition worth telling a subscriber about.
		//
		// This is a report, not a repair (#12.c, ruled report-only,
		// consistent with #10.c's identical question): nothing here touches
		// session status or attempts to rebind anything.
		if g := s.d.credentialGeneration(); s.credentialKnown {
			if g != nil && (s.credentialGen == nil || !g.Equal(*s.credentialGen)) {
				s.emit(ctx, fleet.Event{
					Machine: s.d.machine,
					Kind:    fleet.EventMachineAccount,
					Payload: fleet.MachineAccountPayload{
						Machine:    s.d.machine,
						Generation: *g,
					},
				})
			}
			s.credentialGen = g
		} else {
			s.credentialGen = g
			s.credentialKnown = true
		}

		seen := map[string]bool{}
		for _, sess := range cur.Items() {
			// Attachments are filtered, but lifecycle notifications are
			// fleet-wide, so the diff sees everything. Narrow here too or a
			// subscription that named one session would still be told about
			// every session appearing anywhere.
			if !s.filter.Matches(sess.ID, string(sess.Cwd)) {
				continue
			}
			seen[sess.ID] = true
			prev, had := known[sess.ID]
			switch {
			case !had:
				s.emit(ctx, fleet.Event{
					Machine: s.d.machine,
					Kind:    fleet.EventSessionCreated,
					Payload: sess,
				})
				s.attachContent(ctx, sess)
			case sess.State.MateriallyDiffers(prev.State):
				// Any material change, not only a change of Status. A
				// subscriber maintaining a mirror off this feed renders the
				// prompt, its nonce, the composer digest and how the last turn
				// ended — every one of which moves without Status moving. See
				// fleet.SessionState.MateriallyDiffers for what counts and,
				// more importantly, for what deliberately does not: the
				// evidence prose repaints constantly and may not be parsed, so
				// including it would emit an event per keystroke.
				s.emit(ctx, fleet.Event{
					Machine: s.d.machine,
					Kind:    fleet.EventSessionState,
					Payload: fleet.SessionStatePayload{Ref: sess.SessionRef, State: sess.State},
				})
			}
			// A content client that died on its own leaves a mark (#172), and
			// neither branch of the switch above can act on it: !had is false,
			// because a session re-created under the same id between two reads
			// is never observed to close and so never left `known`. Without
			// this the id keeps its place in the diff and silently loses its
			// push triggers for the life of the subscription.
			//
			// One attempt per observed death, bounded — see maxFutileReattach.
			// The mark is spent here whether or not an attempt follows, which is
			// also what keeps a session that lists but cannot be attached from
			// being re-dialled on every pass.
			if s.takeReattach(sess.ID) {
				s.attachContent(ctx, sess)
			}
			known[sess.ID] = sess
		}
		for id, prev := range known {
			if seen[id] {
				continue
			}
			s.emit(ctx, fleet.Event{
				Machine: s.d.machine,
				Kind:    fleet.EventSessionClosed,
				Payload: fleet.SessionStatePayload{
					Ref: prev.SessionRef,
					State: fleet.InferredState(fleet.StatusDead,
						"session no longer present in the multiplexer", nil),
				},
			})
			delete(known, id)
			s.detachContent(id)
			// The session is gone, so any re-attach mark for it is stale, and
			// its futile count must not be carried into a future incarnation.
			//
			// Order matters: this comes AFTER detachContent, not before. A pump
			// only marks an id while the stream still holds its client, so once
			// detachContent has removed that entry no new mark can appear —
			// while clearing first would lose a mark a pump sets in the gap.
			s.clearReattach(id)
		}
	}
}

func drain(c <-chan struct{}) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

// takeReattach reports whether a content client should be re-dialled for id
// now, spending the mark either way (#172).
//
// Spending it unconditionally is the whole bound on the cheap axis: the mark is
// set once per observed death, so an attempt costs one dial per death rather
// than one per enumeration pass. The futile budget bounds the other axis, where
// the deaths themselves are what repeats.
func (s *eventStream) takeReattach(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.reattach[id]
	if !ok || !st.pending {
		return false
	}
	st.pending = false
	if st.futile == 0 {
		// The ordinary case — a session that restarted and whose previous
		// client had been working — leaves no residue in the map at all.
		delete(s.reattach, id)
	} else {
		s.reattach[id] = st
	}
	if st.futile > maxFutileReattach {
		// Said out loud rather than retried forever or dropped in silence.
		// This stops on its own: with no client dialled there is no further
		// death, so no further mark, so this line is printed once per id.
		log.Printf("tmux: giving up re-attaching a content client session=%s after %d "+
			"consecutive attempts that delivered nothing; its state changes are still "+
			"detected fleet-wide (notifications are triggers, not data), only its own "+
			"push latency is lost", id, maxFutileReattach)
		return false
	}
	return true
}

// clearReattach forgets a session id's re-attach bookkeeping. Called when the
// session is observed gone, so a later incarnation of the same id starts with a
// full budget.
func (s *eventStream) clearReattach(id string) {
	s.mu.Lock()
	delete(s.reattach, id)
	s.mu.Unlock()
}

// attachContent opens a content client for a session that appeared after
// the subscription started. Best effort: without it, the session's
// lifecycle is still tracked, only its state changes are not pushed.
func (s *eventStream) attachContent(ctx context.Context, sess fleet.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if _, ok := s.conns[sess.ID]; ok {
		return
	}
	if !s.filter.Matches(sess.ID, string(sess.Cwd)) {
		return
	}
	// The same bound as Subscribe's initial pass, and this is the path that
	// actually matters over time: a subscription held for hours meets every
	// session the machine ever creates. Capping only the initial pass would
	// bound the wrong thing — the fleet at t=0 rather than the fleet's
	// accumulated history — and the leak would simply arrive more slowly.
	//
	// conns includes the lifecycle client, hence the +1.
	//
	// Since #172 a slot freed by a client's death can be refilled, by the
	// session that vacated it, on the next read. That is a deliberate change to
	// what the cap means over the life of a subscription — it bounds live
	// clients rather than total dials — and it is the reading the descriptor
	// budget behind the number actually cares about. It does not open the cap to
	// new competition: only a marked id re-attaches, never every matching
	// session on every pass.
	if len(s.conns) >= maxContentClients+1 {
		return
	}
	conn, err := s.d.dial(ctx, s.d.bin, sess.ID)
	if err != nil {
		return
	}
	s.conns[sess.ID] = conn
	// Content notifications share the same trigger path; this stream's
	// engine is already draining it.
	go s.pumpContent(ctx, sess.ID, conn)
}

// pumpContent drains one content client and, when it dies on its own, reaps
// it.
//
// A content client exits with the session it is attached to. Its
// notifications ending does not mean its process was waited on: for the real
// transport Close is the only place that happens. That is the lifecycle
// supervisor's defect over again, on the content side — dying is not being
// reaped.
//
// The engine's diff covers the plain case: a read that finds the session
// gone calls detachContent, which closes the client. It covers nothing a read
// does not SEE. A session that exits and is re-created under the same id
// between two reads is never observed to close, so its dead client stayed in
// s.conns — a zombie process until the subscription itself closed (measured
// on a real multiplexer: the kill-only case was already reaped, the
// kill-and-recreate case was not). Only this goroutine knows the client died,
// so the reap belongs here rather than in the diff.
//
// Reaping alone left the second half of that case open (#172): the id stays in
// the engine's `known` map, so the diff's !had branch — the only thing that opens
// a content client — never fires for the new incarnation, and the session keeps
// its place in the feed while silently losing its own push triggers. Before the
// reap this was invisible, because the dead client sat in s.conns and the gap
// looked like a live attachment. So this goroutine also leaves the re-attach mark
// the next read acts on; see reattachState.
//
// Bounded where the lifecycle one was not (at most maxContentClients per
// subscription), but not harmless: a dead entry also holds one of those
// slots.
//
// Cancellation is not a death: the stream's own Close cancels first and then
// reaps every client it holds, so returning here avoids a second Close on the
// same shutdown path. Every other return — the session exited, or
// detachContent already closed the client — removes the entry only if it
// still points at THIS client, since the session may have been re-attached
// under the same id in between, and closes it. Close is idempotent on both
// transports, so detachContent having got there first is harmless.
func (s *eventStream) pumpContent(ctx context.Context, session string, conn ctlConn) {
	spoke := pump(conn.Notes(), s.trigger, isContentNote, ctx)
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if cur, ok := s.conns[session]; ok && cur == conn {
		delete(s.conns, session)
		// This client died while the stream still believed it was the live one
		// for this id, so the session may well still be there — re-created
		// under the same id (#172). Mark it for one re-attach on the next read.
		//
		// Marking only inside this branch is load-bearing twice over. The entry
		// having already moved on means either detachContent closed this client
		// because the engine saw the session gone, or a re-attach replaced it
		// under the same id; in both cases somebody else already decided, and in
		// the first the engine is about to clear this id's bookkeeping — so a
		// mark set outside this branch could be one nothing ever clears.
		st := s.reattach[session]
		st.pending = true
		if spoke {
			st.futile = 0
		} else {
			st.futile++
		}
		s.reattach[session] = st
	}
	s.mu.Unlock()
	_ = conn.Close()

	// A trigger too: the session this client was attached to has demonstrably
	// changed. The lifecycle client normally reports the same exit, but it may
	// itself have been hosted by this session and be mid re-attach.
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *eventStream) detachContent(id string) {
	s.mu.Lock()
	conn, ok := s.conns[id]
	delete(s.conns, id)
	s.mu.Unlock()
	if ok {
		_ = conn.Close()
	}
}

func (s *eventStream) emit(ctx context.Context, ev fleet.Event) {
	// Cursor and Epoch are deliberately left zero — §7.3 assigns them per
	// service instance, and a driver has neither. See this file's FINDING.
	select {
	case s.out <- ev:
	case <-ctx.Done():
	}
}

// Next blocks until an event is available, ctx is cancelled, or the stream
// ends (§3, §5.5: a req is never expected to poll).
func (s *eventStream) Next(ctx context.Context) (fleet.Event, error) {
	select {
	case <-ctx.Done():
		return fleet.Event{}, ctx.Err()
	case ev, ok := <-s.out:
		if !ok {
			return fleet.Event{}, io.EOF
		}
		return ev, nil
	}
}

// Close reaps every control client this subscription opened. A subscription
// that leaked clients would accumulate one attached process per subscribe
// call, which is the failure mode the demand-driven design exists to avoid.
func (s *eventStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]ctlConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = map[string]ctlConn{}
	s.reattach = map[string]reattachState{}
	s.mu.Unlock()

	s.cancel()
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}
