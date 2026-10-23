package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/remote"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/service"
)

// Live reload of principals and peers (muster #347).
//
// Enrolling a client, changing a grant, revoking one, or adding a peer used to
// mean a restart, and a restart drops every other client's open streams. The
// service now watches the config file it was started from and applies the two
// things that are tables — who may call it, and who it may call — to the running
// instance.
//
// # The rules the reload keeps
//
//   - All or nothing. The file is parsed and checked in full, every new peer
//     driver is built, and only then is anything swapped in. A file that fails
//     any step leaves the last good table in force and says why; there is no
//     state in which half a file is live.
//   - Only the two tables move. Every other setting in the file is read once at
//     startup, so a file that changes one of them is REFUSED, naming it, rather
//     than half-applied or silently ignored: running with a table the file no
//     longer describes would be the worse surprise. Bind addresses and the state
//     directory come from the environment, which an edit to this file cannot
//     change at all.
//   - A revoked credential loses its open streams and nobody else does.
//
// The trigger is the file itself, polled by content (so an atomic replace that
// keeps the modification time is still seen), plus SIGHUP for an operator who
// wants it applied now. `muster principal add` already writes atomically, so the
// poll can never read a half-written table.

const (
	// reloadPollInterval is how often the file is checked. The check is a read and
	// a hash of a small file.
	reloadPollInterval = 2 * time.Second

	// reloadStatusFile is where the running service records what it last did with
	// the file, for `muster doctor` (which runs in another process).
	reloadStatusFile = "config-reload.json"
)

// reloadStatus is the record `muster doctor` reads.
type reloadStatus struct {
	// AppliedHash identifies the file contents the service is running on.
	AppliedHash string `json:"appliedHash"`
	AppliedAt   string `json:"appliedAt"`
	// RefusedHash identifies the contents last refused, with Error saying why.
	// Empty when the file on disk is the applied one.
	RefusedHash string `json:"refusedHash,omitempty"`
	RefusedAt   string `json:"refusedAt,omitempty"`
	Error       string `json:"error,omitempty"`
}

func contentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// peerBuilder makes the driver for one peer. It is the startup code that used to
// sit inline in main, moved so a reload builds peers exactly the way a start does.
type peerBuilder struct {
	self     fleet.MachineId
	stateDir string
	// token is FLEET_TOKEN, presented on the capability probe.
	token     string
	selfBuild fleet.Build

	mu     sync.Mutex
	signer *peerkey.Signer
}

func (b *peerBuilder) signerFor() (*peerkey.Signer, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.signer == nil {
		s, err := openMachineSigner(b.stateDir, b.self)
		if err != nil {
			return nil, err
		}
		b.signer = s
	}
	return b.signer, nil
}

// peerSpec is everything that decides how a peer driver is built; two equal
// specs mean the running driver already is the one a reload would make.
type peerSpec struct {
	URLs  []string
	Auth  string
	Token string
}

func specFor(pe peerEntry, cfg *fileConfig) peerSpec {
	auth := pe.auth
	if auth == "" {
		auth = cfg.peerAuthFor(pe.machine)
	}
	spec := peerSpec{URLs: pe.urls, Auth: auth}
	if auth != peerAuthKey && cfg != nil {
		if tok, ok := cfg.peerTokenFor(pe.machine); ok {
			spec.Token = tok
		}
	}
	return spec
}

func (b *peerBuilder) build(pe peerEntry, spec peerSpec) (*remote.Driver, error) {
	if pe.machine == b.self {
		return nil, fmt.Errorf("peer %q is this machine; fan-out is one hop and peers never recurse (§13.1)", pe.machine)
	}
	// No credential is handed to the peer driver for forwarding a caller's
	// authority, and none exists to hand: it presents the authority of whoever
	// made the request (§13). WithSelf lets the probe ask the peer whether it
	// lists this machine back (muster #154).
	opts := []remote.Option{remote.WithDeadline(3 * time.Second), remote.WithSelf(b.self)}
	switch {
	case spec.Auth == peerAuthKey:
		// This machine signs its requests to that peer (muster #350): no token,
		// nothing reusable on the wire. One key serves every key peer.
		signer, err := b.signerFor()
		if err != nil {
			return nil, fmt.Errorf("peer %q authenticates by key: %w", pe.machine, err)
		}
		opts = append(opts, remote.WithSigner(signer))
	case spec.Token != "":
		opts = append(opts, remote.WithIdentity(spec.Token))
	}
	// More than one address: the first is the ordinary one, the rest are tried
	// only when it cannot be connected to (#335).
	if len(spec.URLs) > 1 {
		opts = append(opts, remote.WithFallbackURLs(spec.URLs[1:]...))
	}
	return remote.New(pe.machine, spec.URLs[0], opts...), nil
}

// announce learns the peer's declared deadline so this driver does not abandon
// calls the peer would have completed (§14 D7), and says out loud when the two
// builds differ. Best effort: a peer that is down stays configured (§5.7).
func (b *peerBuilder) announce(p *remote.Driver, m fleet.MachineId) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.RefreshCapabilities(ctx, fleet.Request{
			Caller: fleet.Caller{Principal: "system:self", Credential: b.token},
		}); err != nil {
			log.Printf("muster: peer %s capabilities unknown for now: %v", m, err)
			return
		}
		log.Printf("muster: peer %s deadline learned: %dms", m, p.Capabilities().DeadlineMs)

		// Version skew, said out loud. SameAs deliberately refuses to call unknown
		// or dirty builds equal, so this warns in the cases where a comparison
		// cannot be trusted rather than staying quiet about them.
		if why := b.selfBuild.DifferenceFrom(p.Build()); why != "" {
			log.Printf("muster: NOTE peer %s build %s vs ours %s — %s; "+
				"a disagreement between these two may be skew rather than a bug",
				m, p.Build().Short(), b.selfBuild.Short(), why)
		}
	}()
}

type appliedPeer struct {
	spec peerSpec
	drv  *remote.Driver
}

// reloader applies the config file's principals and peers to a running service.
type reloader struct {
	path    string
	getenv  func(string) string
	self    fleet.MachineId
	svc     *service.Service
	table   *service.PrincipalTable
	builder *peerBuilder
	// token is FLEET_TOKEN, which decides the service's own peer credential.
	token string
	// listOut is the machine list as it was read at startup. A list is a signed
	// document with its own anti-rollback sequence, so it is read once, at a
	// start; a reload re-applies it over the new hand-written peers.
	listOut listOutcome
	// statusDir is where the status file goes; empty means none is written.
	statusDir string

	mu       sync.Mutex
	applied  *fileConfig
	lastHash string
	peers    map[fleet.MachineId]appliedPeer
}

// setPeers registers the peers a start (or a reload) resolved, building every
// driver before registering any. At startup an error is fatal; at reload it
// leaves the running peer set as it was.
func (rl *reloader) planPeers(list []peerEntry, cfg *fileConfig) (build map[fleet.MachineId]appliedPeer, keep map[fleet.MachineId]bool, err error) {
	build = map[fleet.MachineId]appliedPeer{}
	keep = map[fleet.MachineId]bool{}
	for _, pe := range list {
		spec := specFor(pe, cfg)
		if cur, ok := rl.peers[pe.machine]; ok && reflect.DeepEqual(cur.spec, spec) {
			keep[pe.machine] = true
			continue
		}
		drv, berr := rl.builder.build(pe, spec)
		if berr != nil {
			for _, made := range build {
				made.drv.Shutdown()
			}
			return nil, nil, berr
		}
		build[pe.machine] = appliedPeer{spec: spec, drv: drv}
		keep[pe.machine] = true
	}
	return build, keep, nil
}

// commitPeers swaps a plan in and reports what changed.
func (rl *reloader) commitPeers(build map[fleet.MachineId]appliedPeer, keep map[fleet.MachineId]bool) (added, replaced, removed []string, err error) {
	for m, ap := range build {
		old, existed := rl.peers[m]
		if rerr := rl.svc.RegisterPeerDriver(m, ap.drv); rerr != nil {
			err = fmt.Errorf("registering peer %q: %w", m, rerr)
			continue
		}
		rl.peers[m] = ap
		rl.builder.announce(ap.drv, m)
		if existed {
			old.drv.Shutdown()
			replaced = append(replaced, string(m))
		} else {
			added = append(added, string(m))
		}
	}
	for m, old := range rl.peers {
		if keep[m] {
			continue
		}
		rl.svc.UnregisterPeerDriver(m)
		old.drv.Shutdown()
		delete(rl.peers, m)
		removed = append(removed, string(m))
	}
	sort.Strings(added)
	sort.Strings(replaced)
	sort.Strings(removed)
	return added, replaced, removed, err
}

// startPeers is the startup registration, logging the way a start always has.
func (rl *reloader) startPeers(list []peerEntry, cfg *fileConfig) error {
	build, keep, err := rl.planPeers(list, cfg)
	if err != nil {
		return err
	}
	if _, _, _, err := rl.commitPeers(build, keep); err != nil {
		return err
	}
	for _, pe := range list {
		log.Printf("muster: peer %s configured", pe.machine)
	}
	return nil
}

// restartOnly names the settings in next that differ from the running config and
// cannot take effect without a restart.
func restartOnly(running, next *fileConfig) []string {
	var out []string
	check := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, name)
		}
	}
	check("trustRoots", running.TrustRoots, next.TrustRoots)
	check("defaultRuntime", running.DefaultRuntime, next.DefaultRuntime)
	check("sessionEnv", running.SessionEnv, next.SessionEnv)
	check("maxInputBytes", running.MaxInputBytes, next.MaxInputBytes)
	check("gateRemoteControlInput", running.GateRemoteControlInput, next.GateRemoteControlInput)
	check("closedRetentionDays", running.ClosedRetentionDays, next.ClosedRetentionDays)
	check("inputAuditRetentionDays", running.InputAuditRetentionDays, next.InputAuditRetentionDays)
	check("machineList", running.MachineList, next.MachineList)
	return out
}

// principalDiff summarizes two principal tables for the log: names only, never a
// credential.
func principalDiff(before, after []service.Principal) (added, removed, changed []string) {
	prev := map[string]service.Principal{}
	for _, p := range before {
		prev[p.Name] = p
	}
	seen := map[string]bool{}
	for _, p := range after {
		seen[p.Name] = true
		old, ok := prev[p.Name]
		switch {
		case !ok:
			added = append(added, p.Name)
		case old.Token != p.Token || !bytes.Equal(old.PublicKey, p.PublicKey) || !reflect.DeepEqual(old.Grants, p.Grants):
			changed = append(changed, p.Name)
		}
	}
	for _, p := range before {
		if !seen[p.Name] {
			removed = append(removed, p.Name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return
}

// check reads the file and, if its contents differ from the last it looked at,
// tries to apply it. reason is for the log ("poll" is quiet unless something
// happens).
func (rl *reloader) check(reason string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	raw, err := os.ReadFile(rl.path)
	if err != nil {
		// A file briefly absent between an editor's unlink and rename is not news;
		// one that stays absent is, once.
		h := "unreadable:" + err.Error()
		if h != rl.lastHash {
			rl.lastHash = h
			rl.refuse(h, fmt.Errorf("cannot read %s: %w", filepath.Base(rl.path), err), reason)
		}
		return
	}
	hash := contentHash(raw)
	if hash == rl.lastHash {
		if reason == "SIGHUP" {
			log.Printf("muster: config reload (%s): the file is unchanged", reason)
		}
		return
	}
	rl.lastHash = hash

	if err := rl.apply(raw, hash, reason); err != nil {
		rl.refuse(hash, err, reason)
	}
}

func (rl *reloader) refuse(hash string, err error, reason string) {
	log.Printf("muster: config reload (%s) REFUSED, keeping the last good principals and peers: %v", reason, err)
	st := rl.readStatus()
	st.RefusedHash, st.RefusedAt, st.Error = hash, time.Now().UTC().Format(time.RFC3339), err.Error()
	rl.writeStatus(st)
}

func (rl *reloader) apply(raw []byte, hash, reason string) error {
	next, err := parseConfig(rl.path, raw)
	if err != nil {
		return redactErr(err, rl.path)
	}
	if names := restartOnly(rl.applied, next); len(names) > 0 {
		return fmt.Errorf("the file changes %s, which only a restart applies; "+
			"nothing from this edit was applied, including its principals and peers",
			strings.Join(names, ", "))
	}
	principals, err := next.principals()
	if err != nil {
		return err
	}
	handPeers, err := resolvePeers(rl.getenv("FLEET_PEERS"), next)
	if err != nil {
		return fmt.Errorf("bad peer list: %w", err)
	}
	principals = rl.listOut.principalsFor(principals, handPeers)
	peerList := rl.listOut.peers(handPeers)

	build, keep, err := rl.planPeers(peerList, next)
	if err != nil {
		return err
	}

	// Everything that can fail has passed: from here on nothing refuses.
	before := rl.table.Snapshot()
	oldCred := peerCredential(rl.token, rl.applied, rl.self)
	newCred := peerCredential(rl.token, next, rl.self)
	if oldCred != newCred {
		rl.svc.SetPeerCredential(newCred)
	}
	added, replaced, removed, perr := rl.commitPeers(build, keep)
	if perr != nil {
		// RegisterPeerDriver only refuses a driver that cannot describe itself,
		// which a remote driver always can; if it ever happens the table must
		// still agree with what is registered, so carry on and say so.
		log.Printf("muster: config reload: %v", perr)
	}
	closed := rl.table.Replace(principals)
	if len(added)+len(replaced)+len(removed) > 0 || oldCred != newCred {
		rl.svc.PeersChanged()
	}
	rl.applied = next

	pAdded, pRemoved, pChanged := principalDiff(before, principals)
	log.Printf("muster: config reloaded (%s): principals +%v -%v ~%v; peers +%v -%v ~%v",
		reason, pAdded, pRemoved, pChanged, added, removed, replaced)
	for name, n := range closed {
		log.Printf("muster:   closed %d open stream(s) of %q: its credential is no longer accepted", n, name)
	}
	rl.writeStatus(reloadStatus{AppliedHash: hash, AppliedAt: time.Now().UTC().Format(time.RFC3339)})
	return nil
}

func redactErr(err error, path string) error {
	return errors.New(strings.ReplaceAll(err.Error(), path, filepath.Base(path)))
}

func (rl *reloader) readStatus() reloadStatus {
	var st reloadStatus
	if rl.statusDir == "" {
		return st
	}
	if raw, err := os.ReadFile(filepath.Join(rl.statusDir, reloadStatusFile)); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func (rl *reloader) writeStatus(st reloadStatus) {
	if rl.statusDir == "" {
		return
	}
	if st.AppliedHash == "" {
		st.AppliedHash = rl.readStatus().AppliedHash
		st.AppliedAt = rl.readStatus().AppliedAt
	}
	if err := writeJSONAtomic(filepath.Join(rl.statusDir, reloadStatusFile), st); err != nil {
		log.Printf("muster: recording the reload status: %v", err)
	}
}

// run watches until ctx ends. A SIGHUP applies the file now.
func (rl *reloader) run(ctx context.Context, interval time.Duration) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			rl.check("SIGHUP")
		case <-t.C:
			rl.check("file changed")
		}
	}
}

// checkReloadStatus is the `muster doctor` row for live reload: what the running
// service did with the config file as it is on disk right now.
//
// It reads the status file the service writes, so it describes a service that is
// running, not one that merely could be. No state directory, or no status yet,
// is a skip: there is nothing to compare, and saying "fine" would claim a check
// that did not happen.
func checkReloadStatus(stateDir, cfgPath string) doctorRow {
	row := doctorRow{ID: "config.reload", Refs: []int{347}}
	if stateDir == "" {
		row.Status, row.Summary = statusSkip, "FLEET_STATE_DIR unset — the service has nowhere to record what it did with the config"
		return row
	}
	rawStatus, err := os.ReadFile(filepath.Join(stateDir, reloadStatusFile))
	if err != nil {
		row.Status, row.Summary = statusSkip, "no reload status recorded yet — the service has not started with this state directory"
		return row
	}
	var st reloadStatus
	if err := json.Unmarshal(rawStatus, &st); err != nil {
		row.Status, row.Summary = statusWarn, "the reload status file is unreadable"
		return row
	}
	rawCfg, err := os.ReadFile(cfgPath)
	if err != nil {
		row.Status, row.Summary = statusWarn, "the config file cannot be read to compare with what the service applied"
		return row
	}
	on := contentHash(rawCfg)
	switch {
	case on == st.AppliedHash:
		row.Status = statusPass
		row.Summary = "the running service is using the config as it is on disk (applied " + st.AppliedAt + ")"
	case st.Error != "" && on == st.RefusedHash:
		row.Status = statusWarn
		row.Summary = "the running service refused the config on disk and keeps the last good principals and peers"
		row.Detail = st.Error + " (refused " + st.RefusedAt + "; last applied " + st.AppliedAt + ")"
	default:
		row.Status = statusWarn
		row.Summary = "the config on disk is not the one the running service applied"
		row.Detail = "the service checks every " + reloadPollInterval.String() +
			"; if it has been longer, it is not running, or it was started before live reload (restart it once)"
	}
	return row
}
