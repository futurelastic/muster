// Package modtest is a FAKE delivery module for tests: one executor that speaks
// the module protocol (see package modclient) in two forms.
//
// In-process, NewFake(...).Launcher() returns a modclient.Launcher whose
// processes are pairs of in-memory pipes served by the fake — each launch is a
// new incarnation, Kill() simulates a crash of the current one, and every
// request is recorded. It is fast, deterministic and race-detector friendly.
//
// As a real process, MaybeServe (called first in a test binary's TestMain)
// turns the test binary itself into the module when it is started with the
// behaviour in its environment, and Install writes the small shell wrapper that
// does exactly that. This exercises the real launcher: pipes, process groups,
// a minimal environment, stderr, kill and reap.
//
// It is a non-test package only so that tests in several packages can share it
// (the httptest pattern); nothing outside tests imports it. Behaviour is plain
// JSON-serialisable data so that the same script drives both forms.
package modtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/futurelastic/muster/internal/delivery/modclient"
)

// EnvBehaviour names the environment variable that carries a JSON Behaviour to
// a re-executed test binary.
const EnvBehaviour = "FLEET_FAKE_MODULE_BEHAVIOUR"

// WireError is the error object of an ok:false response.
type WireError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// HelloScript overrides parts of the hello line. Only the fields that are set
// change; the rest keep their defaults.
type HelloScript struct {
	// Raw, when non-empty, is emitted verbatim as the first line.
	Raw string `json:",omitempty"`
	// NoLine emits nothing at all: the silence case.
	NoLine bool `json:",omitempty"`
	// Protocol replaces the protocol number.
	Protocol *int `json:",omitempty"`
	// Ops replaces the advertised operations (nil keeps the six).
	Ops []string `json:",omitempty"`
	// ReservedEnvPrefixes replaces the reserved prefixes.
	ReservedEnvPrefixes []string `json:",omitempty"`
	Module              string   `json:",omitempty"`
	Version             string   `json:",omitempty"`
}

// OpScript scripts how one operation answers.
type OpScript struct {
	// Results are consumed in order, one per request of this op; the last
	// repeats. Empty means the default result.
	Results []json.RawMessage `json:",omitempty"`
	// Error, when set, answers ok:false instead of a result.
	Error *WireError `json:",omitempty"`
	// DelayMs delays the answer.
	DelayMs int `json:",omitempty"`
	// HangForever never answers this op.
	HangForever bool `json:",omitempty"`
	// ExitAfter makes the incarnation exit after answering this many requests
	// of ANY op in total, provided the request just answered was of this op.
	// With ExitOnceMarker set it fires in one incarnation only.
	ExitAfter int `json:",omitempty"`
	// OversizeBytes pads the answer past that many bytes.
	OversizeBytes int `json:",omitempty"`
	// ExtraFields adds fields the client must ignore, to the envelope and to
	// the result.
	ExtraFields bool `json:",omitempty"`
	// IDOverride answers with this id instead of the request's.
	IDOverride string `json:",omitempty"`
}

// Behaviour is everything a fake module does. All of it is JSON-serialisable.
type Behaviour struct {
	Hello *HelloScript        `json:",omitempty"`
	Ops   map[string]OpScript `json:",omitempty"`
	// Stderr is written to the module's stderr at start.
	Stderr string `json:",omitempty"`
	// IgnoreStdinClose keeps the module alive when its stdin reaches EOF: the
	// stubborn module that only a kill stops.
	IgnoreStdinClose bool `json:",omitempty"`
	// StallReads makes the in-process module say hello and then never read
	// another byte, until Resume. Requests written to it back up in the pipe.
	StallReads bool `json:",omitempty"`
	// DumpEnvTo, for the real-process form, is a file the module writes its
	// argv, environment and working directory to at start (see EnvDump).
	DumpEnvTo string `json:",omitempty"`
	// RecordTo, for the real-process form, is a file the module appends one
	// line per received request to ("<op>\n"), so a test can see that a request
	// reached a process it cannot otherwise observe.
	RecordTo string `json:",omitempty"`
	// ExitOnceMarker limits ExitAfter to one incarnation across processes: the
	// exit only happens if this file does not exist, and creates it.
	ExitOnceMarker string `json:",omitempty"`
}

// EnvDump is what DumpEnvTo receives.
type EnvDump struct {
	Args []string
	Env  []string
	Cwd  string
}

// ReadEnvDump reads a file written by DumpEnvTo.
func ReadEnvDump(path string) (EnvDump, error) {
	var d EnvDump
	b, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(b, &d)
}

// JSON marshals v for use in OpScript.Results; it panics on failure, which is a
// test bug.
func JSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Int returns a pointer to i, for HelloScript.Protocol.
func Int(i int) *int { return &i }

// Request is what a Handler is asked about.
type Request struct {
	ID          string
	Op          string
	Args        json.RawMessage
	Incarnation int
	// Ctx is done when the incarnation ends, so a blocking handler can leave.
	// What it returns once Ctx is done is discarded: an incarnation that has
	// ended answers nothing, so a handler may simply return (nil, nil, 0) there
	// without that turning into a scripted success on the wire.
	Ctx context.Context
}

// Recorded is one request the fake received.
type Recorded struct {
	ID          string
	Op          string
	Args        json.RawMessage
	Incarnation int
}

// Handler overrides the scripted behaviour for a request. It runs in the
// request's own goroutine, so blocking it holds only that request. Returning
// (nil, nil, d) declines: the scripted behaviour answers after the delay d.
type Handler func(Request) (result any, werr *WireError, delay time.Duration)

// ErrCrashed is what Wait returns for an incarnation ended by Fake.Kill.
var ErrCrashed = errors.New("modtest: the fake module crashed")

// ErrKilled is what Wait returns for an incarnation ended by Proc.Kill.
var ErrKilled = errors.New("modtest: the fake module was killed")

// ErrExited is what Wait returns for an incarnation that exited by ExitAfter.
var ErrExited = errors.New("modtest: the fake module exited by script")

// Fake is an in-process fake module. Use one per test.
type Fake struct {
	b Behaviour

	mu          sync.Mutex
	starts      int
	kills       int
	stdinClosed int
	startTimes  []time.Time
	recorded    []Recorded
	handler     Handler
	cur         *pipeProc
	failLaunch  int
	last        launchInfo
	gate        chan struct{} // closed by Resume; nil when not stalling
	laneSeq     atomic.Uint64
	sendSeq     atomic.Uint64
}

type launchInfo struct {
	path      string
	args, env []string
}

// NewFake returns a fake that follows b.
func NewFake(b Behaviour) *Fake {
	f := &Fake{b: b}
	if b.StallReads {
		f.gate = make(chan struct{})
	}
	return f
}

// SetHandler installs (or, with nil, removes) the programmatic override.
func (f *Fake) SetHandler(h Handler) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

// Starts is the number of incarnations launched so far.
func (f *Fake) Starts() int { f.mu.Lock(); defer f.mu.Unlock(); return f.starts }

// Kills is how many times the client killed a still-running incarnation through
// Proc.Kill. A crash simulated by Kill is not counted.
func (f *Fake) Kills() int { f.mu.Lock(); defer f.mu.Unlock(); return f.kills }

// StdinClosed is how many incarnations saw their stdin reach EOF.
func (f *Fake) StdinClosed() int { f.mu.Lock(); defer f.mu.Unlock(); return f.stdinClosed }

// StartTimes are the instants the incarnations were launched.
func (f *Fake) StartTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.startTimes...)
}

// Requests returns every request received so far, in arrival order.
func (f *Fake) Requests() []Recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Recorded(nil), f.recorded...)
}

// CountOp is the number of recorded requests for op.
func (f *Fake) CountOp(op string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Op == op {
			n++
		}
	}
	return n
}

// LastLaunch returns the arguments of the most recent launch.
func (f *Fake) LastLaunch() (path string, args, env []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last.path, append([]string(nil), f.last.args...), append([]string(nil), f.last.env...)
}

// FailNextLaunches makes the next n launches fail, as a missing binary would.
func (f *Fake) FailNextLaunches(n int) {
	f.mu.Lock()
	f.failLaunch = n
	f.mu.Unlock()
}

// Kill simulates a crash of the current incarnation: its pipes close and Wait
// returns ErrCrashed. It is a no-op when none is running.
func (f *Fake) Kill() {
	f.mu.Lock()
	p := f.cur
	f.mu.Unlock()
	if p != nil {
		p.terminate(ErrCrashed)
	}
}

// Resume lifts StallReads.
func (f *Fake) Resume() {
	f.mu.Lock()
	g := f.gate
	f.gate = nil
	f.mu.Unlock()
	if g != nil {
		close(g)
	}
}

// Inject writes a raw line (a newline is added) to the current incarnation's
// output, as though the module had produced it — for stale and garbage lines.
func (f *Fake) Inject(line string) error {
	f.mu.Lock()
	p := f.cur
	f.mu.Unlock()
	if p == nil || p.w == nil {
		return errors.New("modtest: no running incarnation")
	}
	_, err := p.w.Write([]byte(line + "\n"))
	return err
}

func (f *Fake) record(r Recorded) {
	f.mu.Lock()
	f.recorded = append(f.recorded, r)
	f.mu.Unlock()
}

func (f *Fake) getHandler() Handler { f.mu.Lock(); defer f.mu.Unlock(); return f.handler }

func (f *Fake) stalled() chan struct{} { f.mu.Lock(); defer f.mu.Unlock(); return f.gate }

// Serve runs the protocol over r and w until r ends, ctx is done or a script
// says exit. It is the executor both forms share; the real-process form calls
// it with stdin and stdout. It returns nil when r reached EOF.
func (f *Fake) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return f.serve(ctx, r, w, 1, func() {
		if c, ok := r.(io.Closer); ok {
			_ = c.Close()
		}
	})
}

// --- in-process launcher ---------------------------------------------------------

// Launcher returns a modclient.Launcher whose processes are served by f.
func (f *Fake) Launcher() modclient.Launcher {
	return func(ctx context.Context, path string, args []string, env []string) (modclient.Proc, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f.mu.Lock()
		if f.failLaunch > 0 {
			f.failLaunch--
			f.mu.Unlock()
			return nil, errors.New("modtest: launch refused")
		}
		f.starts++
		inc := f.starts
		f.startTimes = append(f.startTimes, time.Now())
		f.last = launchInfo{path: path, args: append([]string(nil), args...), env: append([]string(nil), env...)}
		p := newPipeProc(f, inc)
		f.cur = p
		f.mu.Unlock()
		go p.run()
		return p, nil
	}
}

type pipeProc struct {
	f   *Fake
	inc int

	inR  *io.PipeReader
	inW  *io.PipeWriter
	outR *io.PipeReader
	outW *io.PipeWriter
	errR *io.PipeReader
	errW *io.PipeWriter
	w    *syncWriter

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	err error
}

func newPipeProc(f *Fake, inc int) *pipeProc {
	p := &pipeProc{f: f, inc: inc, done: make(chan struct{})}
	p.inR, p.inW = io.Pipe()
	p.outR, p.outW = io.Pipe()
	p.errR, p.errW = io.Pipe()
	p.w = &syncWriter{w: p.outW}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	return p
}

func (p *pipeProc) run() {
	defer close(p.done)
	var wg sync.WaitGroup
	if s := p.f.b.Stderr; s != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.WriteString(p.errW, s)
		}()
	}
	err := p.f.serve(p.ctx, p.inR, p.w, p.inc, func() { p.terminate(ErrExited) })
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.cancel()
	_ = p.outW.Close()
	_ = p.errW.Close()
	_ = p.inR.Close()
	wg.Wait()
}

// terminate ends the incarnation with err (the first error wins).
func (p *pipeProc) terminate(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.cancel()
	_ = p.inR.CloseWithError(err)
	_ = p.outW.Close()
	_ = p.errW.Close()
}

func (p *pipeProc) Stdin() io.WriteCloser { return p.inW }
func (p *pipeProc) Stdout() io.Reader     { return p.outR }
func (p *pipeProc) Stderr() io.Reader     { return p.errR }
func (p *pipeProc) Pid() int              { return 10000 + p.inc }

func (p *pipeProc) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *pipeProc) Kill() {
	select {
	case <-p.done:
		return
	default:
	}
	p.f.mu.Lock()
	p.f.kills++
	p.f.mu.Unlock()
	p.terminate(ErrKilled)
}

// syncWriter serialises whole-line writes, so responses never interleave.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

// --- the executor ----------------------------------------------------------------

type executor struct {
	f        *Fake
	inc      int
	ctx      context.Context
	w        *syncWriter
	exit     func()
	hello    modclient.Hello
	mu       sync.Mutex
	answered int
	seq      map[string]int
}

var defaultOps = []string{"prepare-launch", "attach", "send", "confirm", "close", "health"}

func (f *Fake) serve(ctx context.Context, r io.Reader, w io.Writer, inc int, exit func()) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// Cancel BEFORE waiting: a request still hanging on the script must not
	// keep the incarnation from ending.
	defer func() {
		cancel()
		wg.Wait()
	}()
	sw, ok := w.(*syncWriter)
	if !ok {
		sw = &syncWriter{w: w}
	}
	e := &executor{f: f, inc: inc, ctx: ctx, w: sw, exit: exit, seq: map[string]int{}}
	e.hello = e.helloValue()
	if err := e.sayHello(); err != nil {
		return err
	}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		if g := f.stalled(); g != nil {
			select {
			case <-g:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		line, _, err := modclient.ReadLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				f.mu.Lock()
				f.stdinClosed++
				f.mu.Unlock()
				if f.b.IgnoreStdinClose {
					// A pending timer keeps the runtime's deadlock detector from
					// ending a process that is meant to stay alive.
					for {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(time.Hour):
						}
					}
				}
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.handle(line)
		}()
	}
}

func (e *executor) helloValue() modclient.Hello {
	h := modclient.Hello{
		Event: "hello", Module: "fake", Protocol: modclient.ProtocolVersion, Version: "0.0.1",
		Ops: defaultOps, ReservedEnvPrefixes: []string{},
	}
	if s := e.f.b.Hello; s != nil {
		if s.Protocol != nil {
			h.Protocol = *s.Protocol
		}
		if s.Ops != nil {
			h.Ops = s.Ops
		}
		if s.ReservedEnvPrefixes != nil {
			h.ReservedEnvPrefixes = s.ReservedEnvPrefixes
		}
		if s.Module != "" {
			h.Module = s.Module
		}
		if s.Version != "" {
			h.Version = s.Version
		}
	}
	return h
}

func (e *executor) sayHello() error {
	s := e.f.b.Hello
	switch {
	case s != nil && s.NoLine:
		return nil
	case s != nil && s.Raw != "":
		_, err := e.w.Write([]byte(s.Raw + "\n"))
		return err
	}
	b, err := json.Marshal(e.hello)
	if err != nil {
		return err
	}
	_, err = e.w.Write(append(b, '\n'))
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (e *executor) handle(line []byte) {
	var m struct {
		ID   json.RawMessage `json:"id"`
		Op   string          `json:"op"`
		Args json.RawMessage `json:"args"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	id := strings.Trim(string(m.ID), `"`)
	e.f.record(Recorded{ID: id, Op: m.Op, Args: m.Args, Incarnation: e.inc})
	e.recordToFile(m.Op)
	sc := e.f.b.Ops[m.Op]

	var (
		res   any
		werr  *WireError
		delay time.Duration
	)
	if h := e.f.getHandler(); h != nil {
		res, werr, delay = h(Request{ID: id, Op: m.Op, Args: m.Args, Incarnation: e.inc, Ctx: e.ctx})
	}
	if !sleepCtx(e.ctx, delay) {
		return
	}
	var result json.RawMessage
	switch {
	case res != nil || werr != nil:
		if res != nil {
			b, err := json.Marshal(res)
			if err != nil {
				werr = &WireError{Code: "internal", Message: err.Error()}
			} else {
				result = b
			}
		}
	default:
		if sc.HangForever {
			<-e.ctx.Done()
			return
		}
		if !sleepCtx(e.ctx, time.Duration(sc.DelayMs)*time.Millisecond) {
			return
		}
		if sc.Error != nil {
			werr = sc.Error
		} else {
			var err error
			result, werr, err = e.scripted(m.Op, sc, m.Args)
			if err != nil {
				werr = &WireError{Code: "internal", Message: err.Error()}
			}
		}
	}

	// An incarnation that has ended answers nothing. Ending cancels e.ctx before
	// the pipes close, so a handler released by that cancellation (the usual way
	// to hold a request until the module dies) returns while the output pipe is
	// still open; writing what it returned would hand the client a success
	// from a dead process, ahead of the exit it is meant to observe. Whatever the
	// handler returned after the end is discarded here, at the one place every
	// answer passes through.
	if e.ctx.Err() != nil {
		return
	}

	env := map[string]any{"id": id, "ok": werr == nil}
	if sc.IDOverride != "" {
		env["id"] = sc.IDOverride
	}
	if werr != nil {
		env["error"] = werr
	} else {
		env["result"] = e.decorate(sc, result)
	}
	if sc.ExtraFields {
		env["zzUnknownEnvelopeField"] = map[string]any{"n": 1}
	}
	b, err := json.Marshal(env)
	if err != nil {
		return
	}
	_, _ = e.w.Write(append(b, '\n'))

	e.mu.Lock()
	e.answered++
	exitNow := sc.ExitAfter > 0 && e.answered >= sc.ExitAfter
	e.mu.Unlock()
	if exitNow && e.exitAllowed() {
		e.exit()
	}
}

// decorate applies ExtraFields and OversizeBytes to a result object.
func (e *executor) decorate(sc OpScript, result json.RawMessage) any {
	if !sc.ExtraFields && sc.OversizeBytes == 0 {
		return result
	}
	m := map[string]json.RawMessage{}
	if json.Unmarshal(result, &m) != nil {
		return result
	}
	if sc.ExtraFields {
		m["zzUnknownResultField"] = JSON([]int{1, 2, 3})
	}
	if sc.OversizeBytes > 0 {
		m["zzPad"] = JSON(strings.Repeat("x", sc.OversizeBytes))
	}
	return m
}

func (e *executor) recordToFile(op string) {
	path := e.f.b.RecordTo
	if path == "" {
		return
	}
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fh.WriteString(op + "\n")
	_ = fh.Close()
}

func (e *executor) exitAllowed() bool {
	marker := e.f.b.ExitOnceMarker
	if marker == "" {
		return true
	}
	if _, err := os.Stat(marker); err == nil {
		return false
	}
	return os.WriteFile(marker, []byte("exited\n"), 0o600) == nil
}

// scripted computes the result of op from the script, else from the defaults.
func (e *executor) scripted(op string, sc OpScript, args json.RawMessage) (json.RawMessage, *WireError, error) {
	if n := len(sc.Results); n > 0 {
		e.mu.Lock()
		i := e.seq[op]
		e.seq[op]++
		e.mu.Unlock()
		if i >= n {
			i = n - 1
		}
		return sc.Results[i], nil, nil
	}
	var a struct {
		LaneKey       string `json:"laneKey"`
		ClaudeVersion string `json:"claudeVersion"`
	}
	_ = json.Unmarshal(args, &a)
	switch op {
	case "prepare-launch":
		key := fmt.Sprintf("%016x", uint64(e.inc)<<32|e.f.laneSeq.Add(1))
		return JSON(map[string]any{"laneKey": key, "claudeVersion": a.ClaudeVersion, "env": map[string]string{}}), nil, nil
	case "attach":
		return JSON(map[string]any{"laneKey": a.LaneKey, "live": true, "state": "live", "sinceMs": 1}), nil, nil
	case "send":
		return JSON(map[string]any{
			"sendId": fmt.Sprintf("s%d", e.f.sendSeq.Add(1)), "written": true,
			"transcript": map[string]any{"sessionId": "x", "offset": 0},
		}), nil, nil
	case "confirm":
		return JSON(map[string]any{"verdict": "confirmed", "enqueued": true, "elapsedMs": 1, "final": true}), nil, nil
	case "close":
		return JSON(map[string]any{"removed": true}), nil, nil
	case "health":
		return JSON(map[string]any{
			"ok": true, "module": "fake", "protocol": 1, "version": "0.0.1", "platform": "test",
			"peerCheck": true, "reservedEnvPrefixes": e.hello.ReservedEnvPrefixes,
			"lanes": []any{}, "counters": map[string]any{},
		}), nil, nil
	}
	return nil, &WireError{Code: "unknown-op", Message: "no such operation"}, nil
}

// --- the real-process form -------------------------------------------------------

// MaybeServe is called first in a test binary's TestMain. When EnvBehaviour is
// set the binary is being used as a fake module: it serves the protocol on
// stdin and stdout per that behaviour and exits. Otherwise it returns at once.
func MaybeServe() {
	v := os.Getenv(EnvBehaviour)
	if v == "" {
		// Windows cannot be given the behaviour through a shell wrapper, so
		// Install writes it beside the executable (install_windows.go). A
		// normal test run has no such file next to its binary.
		v = sidecarBehaviour()
	}
	if v == "" {
		return
	}
	var b Behaviour
	if err := json.Unmarshal([]byte(v), &b); err != nil {
		fmt.Fprintln(os.Stderr, "modtest: bad behaviour:", err)
		os.Exit(2)
	}
	if b.DumpEnvTo != "" {
		cwd, _ := os.Getwd()
		blob, _ := json.Marshal(EnvDump{Args: os.Args[1:], Env: os.Environ(), Cwd: cwd})
		if err := os.WriteFile(b.DumpEnvTo, blob, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "modtest: cannot write the env dump:", err)
			os.Exit(2)
		}
	}
	if b.Stderr != "" {
		fmt.Fprint(os.Stderr, b.Stderr)
	}
	f := NewFake(b)
	if err := f.serve(context.Background(), os.Stdin, os.Stdout, 1, func() { os.Exit(0) }); err != nil {
		fmt.Fprintln(os.Stderr, "modtest: serve ended:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
