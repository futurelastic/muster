package tmux

// The rig for #185's driver-level tests: a driver over the fake multiplexer,
// with one external delivery module served in-process by modtest. Everything the
// module says is scripted; everything the driver does about it is asserted.
//
// The fake multiplexer's new-session is a no-op against its own session table,
// so a session a test creates is given a pane afterwards (create), which is also
// what makes the driver's asynchronous attach find a pid to attach with.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery/modclient"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
)

const (
	modName   = "relay-a"
	modPrefix = "RELAYA_"
)

// rigOptions varies the rig. The zero value is one installed module named
// modName, started, with the fake's default (happy) behaviour.
type rigOptions struct {
	behaviour modtest.Behaviour
	// enabled overrides the enabled list (default: [modName]).
	enabled []string
	// notInstalled leaves the enabled name with no client at all.
	notInstalled bool
	// noStart builds the driver but does not start the modules.
	noStart bool
	// driverOpts are extra driver options.
	driverOpts []Option
	// seed is written to the lanes file before the driver is built, to model a
	// state left by an earlier run.
	seed *lanesFile
	// storeDir reuses a state directory (a "restart" of the same machine).
	storeDir string
	// tune adjusts the modules config before the driver is built.
	tune func(*ModulesConfig)
	// mux replaces the rig's fake multiplexer, for a test that has to wrap its
	// exec (swallowFirstSubmitOnly) before the driver is built.
	mux *fakeMux
	// realPath runs a REAL child process (a modtest.Install wrapper) through the
	// real launcher instead of the in-process fake.
	realPath string
}

type modRig struct {
	t     *testing.T
	mux   *fakeMux
	d     *Driver
	fake  *modtest.Fake
	store *state.Store
	dir   string
	pids  int
}

// reserving is a behaviour whose hello reserves modPrefix.
func reserving(b modtest.Behaviour) modtest.Behaviour {
	if b.Hello == nil {
		b.Hello = &modtest.HelloScript{}
	}
	b.Hello.ReservedEnvPrefixes = []string{modPrefix}
	return b
}

func newModRig(t *testing.T, o rigOptions) *modRig {
	t.Helper()
	r := &modRig{t: t, mux: twoSessions(), fake: modtest.NewFake(o.behaviour), pids: 5000}
	if o.mux != nil {
		r.mux = o.mux
	}
	r.dir = o.storeDir
	if r.dir == "" {
		r.dir = t.TempDir()
	}
	if o.seed != nil {
		raw, err := json.Marshal(o.seed)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.dir+"/"+lanesFileName+".json", raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if r.store, err = state.Open(r.dir); err != nil {
		t.Fatal(err)
	}
	enabled := o.enabled
	if enabled == nil {
		enabled = []string{modName}
	}
	cfg := ModulesConfig{
		Enabled:          enabled,
		AttachWait:       200 * time.Millisecond,
		AttachWindow:     2 * time.Second,
		AttachRetryEvery: 20 * time.Millisecond,
		LaneTick:         time.Hour, // the lane pass is driven by tests
	}
	if !o.notInstalled {
		cc := modclient.Config{
			Name: modName, Path: "/fake/" + modName, Env: []string{"PATH=/usr/bin"},
			Launcher:     r.fake.Launcher(),
			HelloTimeout: 2 * time.Second,
			BackoffMin:   10 * time.Millisecond, BackoffMax: 50 * time.Millisecond,
			HealthInterval: -1,
		}
		if o.realPath != "" {
			env, _ := modclient.ChildEnv(os.Getenv, r.dir, nil)
			cc.Path, cc.Env, cc.Launcher = o.realPath, env, nil
			// A real child is a re-exec of this race-instrumented test binary
			// through a shell, so how long it takes to say hello is the host's
			// business and shows a tail: about a second in one start in twelve at a
			// load average near 20, on a host where the median is 30ms. The 2s
			// above suits the in-memory fake, which has no process to start; here
			// the test is about the whole path working, not about start-up
			// being quick. And a missed hello is not retried: the client refuses
			// the child and disables the module until the daemon restarts, so one
			// slow start fails the whole test rather than costing a retry (#207).
			cc.HelloTimeout = 15 * time.Second
		}
		cfg.Clients = []modclient.Config{cc}
	}
	if o.tune != nil {
		o.tune(&cfg)
	}
	opts := []Option{
		withExec(r.mux.exec),
		withNonce(func() string { return testNonce }),
		WithState(r.store),
		WithDeliveryModules(cfg),
	}
	opts = append(opts, o.driverOpts...)
	r.d = New("testbox", opts...)
	t.Cleanup(r.d.StopDeliveryModules)
	if !o.noStart {
		r.d.StartDeliveryModules()
		if !o.notInstalled {
			r.waitUsable()
		}
	}
	return r
}

func (r *modRig) client() *modclient.Client { return r.d.mods.clients[modName] }

func (r *modRig) waitUsable() {
	r.t.Helper()
	waitFor(r.t, "the module to become usable", func() bool { return r.client().Usable() })
}

// create launches a session through the driver, then gives it a pane.
func (r *modRig) create(name string, env map[string]string) fleet.Session {
	r.t.Helper()
	sess, err := r.d.Create(context.Background(), testCaller, "key-"+name,
		fleet.SessionSpec{Name: name, Cwd: fleet.AbsolutePath("/work/" + name), Env: env})
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	r.addPane(sess.ID, "/work/"+name)
	return sess
}

func (r *modRig) addPane(id, cwd string) int {
	r.pids++
	r.mux.addSession(fakeSession{
		name: id, paneID: "%" + intToStr(r.pids), cwd: cwd, pid: r.pids,
		created: 1785600002, title: "2_1_220",
	}, idleFixtureFor(id))
	return r.pids
}

func (r *modRig) record(id string) (laneRecord, bool) { return r.d.mods.snapshot(id) }

func (r *modRig) view(id string) *fleet.DeliveryLane { return r.d.mods.laneView(id) }

// waitLive waits for the session's lane to read live through the public view.
func (r *modRig) waitLive(id string) {
	r.t.Helper()
	waitFor(r.t, "the session's lane to read live", func() bool {
		v := r.view(id)
		return v != nil && v.ClientConnected
	})
}

// waitAllLive waits for every listed session's lane to read live, under one
// deadline scaled to how many are attaching at once (muster#221).
// TestModuleSend_ManySessionsConcurrent attaches n lanes together, and they
// contend for the same locks the attach path holds — waiting on them one at
// a time with waitLive's fixed per-lane budget still bounds each lane to that
// same fixed budget, however many others are attaching alongside it. A
// loaded -race run pushed one lane past that budget once (the same class of
// flake as #214 and #186); scaling the shared deadline by n gives every lane
// room for the contention the others add. A healthy run still returns in
// well under a second — this only widens the ceiling nothing should hit.
func (r *modRig) waitAllLive(ids []string) {
	r.t.Helper()
	deadline := time.Now().Add(time.Duration(len(ids)) * waitForTimeout)
	waitForDeadline(r.t, fmt.Sprintf("all %d sessions' lanes to read live", len(ids)), deadline, func() bool {
		for _, id := range ids {
			v := r.view(id)
			if v == nil || !v.ClientConnected {
				return false
			}
		}
		return true
	})
}

func (r *modRig) counter(name string) int64 { return r.d.Counters()[name] }

func (r *modRig) modCounter(name string) int64 { return r.counter("module." + modName + "." + name) }

// sends returns the number of `send` operations the module received.
func (r *modRig) sends() int { return r.fake.CountOp("send") }

// pastes returns how many payloads reached a pane through the built-in path.
func (r *modRig) pastes() int { return r.mux.pasteLogLen() }

var agentFrom = &fleet.MessageFrom{Agent: "agent-x", Session: "s1"}

// send is one Send with a printable label.
func (r *modRig) send(id, text string, o driver.SendOptions) fleet.DeliveryReceipt {
	r.t.Helper()
	o.Submit = true
	if o.From == nil && !o.HumanRelay {
		o.From = agentFrom
	}
	got, err := r.d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id}, text, o)
	if err != nil {
		r.t.Fatalf("Send: %v", err)
	}
	return got
}

// lastSendText is the text the module was last asked to send.
func (r *modRig) lastSendText() string {
	r.t.Helper()
	var text string
	for _, rec := range r.fake.Requests() {
		if rec.Op == "send" {
			var a modclient.SendArgs
			if err := json.Unmarshal(rec.Args, &a); err != nil {
				r.t.Fatal(err)
			}
			text = a.Text
		}
	}
	return text
}

// lastSendArgs is the arguments of the module's last `send`.
func (r *modRig) lastSendArgs() modclient.SendArgs {
	r.t.Helper()
	var out modclient.SendArgs
	for _, rec := range r.fake.Requests() {
		if rec.Op == "send" {
			out = modclient.SendArgs{} // Unmarshal leaves absent (omitempty) fields as they were
			if err := json.Unmarshal(rec.Args, &out); err != nil {
				r.t.Fatal(err)
			}
		}
	}
	return out
}

// stagedEnv reads the environment file the last create staged for its agent.
func (r *modRig) stagedEnv() map[string]string {
	r.t.Helper()
	var path string
	for _, c := range r.mux.callsSnapshot() {
		if len(c) == 0 || c[0] != "new-session" {
			continue
		}
		for _, a := range c {
			if strings.Contains(a, "spec-env-") {
				path = a
			}
		}
	}
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatalf("reading the staged env: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

// keysOf is a map's keys, sorted.
func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// gate lets a test hold one op's answers until released.
type gate struct {
	mu   sync.Mutex
	open chan struct{}
}

func newGate() *gate { return &gate{open: make(chan struct{})} }
func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
	default:
		close(g.open)
	}
}
func (g *gate) wait(ctx context.Context) {
	select {
	case <-g.open:
	case <-ctx.Done():
	}
}
