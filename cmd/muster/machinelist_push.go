package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/machinelist"
	"github.com/futurelastic/muster/internal/service"
)

// A machine list handed to this machine by a peer (muster #352).
//
// The routes, the grant and the order of checks are in internal/service; this
// file is the holder they ask: it knows the pinned signer, the state directory,
// the list in force and the reload that puts a new one into force.
//
// The rule that shapes everything here: a list is trusted for what it is, never
// for who delivered it. It must verify against the signer pinned on THIS machine,
// and carry a sequence strictly higher than the highest this machine has
// accepted. Then it goes into force through swapIn, the same code a changed file
// goes through, so there is one way a list becomes the list in force.

// machineListAPI is service.MachineListHolder over the running reloader. Every
// method takes the reloader's lock, the one the file poll and SIGHUP already
// hold, so two pushes, a push and the poll, and a push and a SIGHUP are serialized.
type machineListAPI struct{ rl *reloader }

var _ service.MachineListHolder = machineListAPI{}

func (a machineListAPI) fe(kind fleet.ErrorKind, reason, msg string) *fleet.Error {
	return &fleet.Error{Kind: kind, Reason: reason, Message: msg, Machine: a.rl.self}
}

func (a machineListAPI) off() *fleet.Error {
	return a.fe(fleet.ErrorUnsupported, fleet.ReasonMachineListOff,
		"this machine does not use an owner-signed machine list (no machineList block in its config, or mode off)")
}

func (a machineListAPI) mode() string { return a.rl.applied.MachineList.mode() }

// Held is the exact bytes of the list in force.
func (a machineListAPI) Held() ([]byte, *fleet.Error) {
	rl := a.rl
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.applied == nil || a.mode() == listModeOff {
		return nil, a.off()
	}
	if rl.listOut.verified == nil || len(rl.listOut.raw) == 0 {
		why := "no list has verified yet"
		if rl.listOut.err != nil {
			why = redactErr(rl.listOut.err, rl.applied.MachineList.Path).Error()
		}
		return nil, a.fe(fleet.ErrorNotFound, fleet.ReasonMachineListNone,
			"this machine has no machine list in force: "+why)
	}
	return append([]byte(nil), rl.listOut.raw...), nil
}

// CanAccept answers before a body is read.
func (a machineListAPI) CanAccept() *fleet.Error {
	rl := a.rl
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.applied == nil || a.mode() == listModeOff {
		return a.off()
	}
	if rl.statusDir == "" && (a.mode() == listModePrefer || a.mode() == listModeOnly) {
		return a.fe(fleet.ErrorUnsupported, fleet.ReasonMachineListNoStateDir,
			"this machine has no state directory (FLEET_STATE_DIR), and the highest list accepted is remembered there: "+
				"without it an older genuine list could be replayed")
	}
	return nil
}

// Accept takes a pushed list or refuses it, and says why in the wire's terms.
func (a machineListAPI) Accept(raw []byte, by string) (fleet.MachineListReceipt, *fleet.Error) {
	rec, err := a.rl.acceptPushed(raw, by)
	if err == nil {
		return rec, nil
	}
	var ferr *fleet.Error
	if !errors.As(err, &ferr) {
		ferr = pushRefusal(err, a.rl.self, a.scrubber())
	}
	// The log is local and gets the whole error; what goes back to the peer is
	// scrubbed of this machine's paths.
	log.Printf("muster: machine list push from %q REFUSED (%s): %v; nothing written", by, ferr.Reason, err)
	return fleet.MachineListReceipt{}, ferr
}

// scrubber makes a message safe to send to a peer: where this machine keeps its
// list and its state is not the peer's business, and a file error names both.
func (a machineListAPI) scrubber() func(string) string {
	rl := a.rl
	var pairs []string
	if rl.applied != nil && rl.applied.MachineList != nil && rl.applied.MachineList.Path != "" {
		path := rl.applied.MachineList.Path
		pairs = append(pairs, path, filepath.Base(path), filepath.Dir(path), "<list directory>")
	}
	if rl.statusDir != "" {
		pairs = append(pairs, rl.statusDir, "<state directory>")
	}
	return strings.NewReplacer(pairs...).Replace
}

// notNewerError is a list whose sequence is not higher than the highest accepted.
type notNewerError struct{ seq, highest uint64 }

func (e notNewerError) Error() string {
	return fmt.Sprintf("machine list: sequence %d is not higher than the highest this machine has accepted (%d); "+
		"a list is replaced only by a newer one, so re-sign it with a higher sequence", e.seq, e.highest)
}

// notWritableError is a list that verified but whose file cannot be written.
type notWritableError struct{ err error }

func (e notWritableError) Error() string {
	return "machine list: the list's file cannot be written: " + e.err.Error() +
		" (the service must be able to write the directory the list lives in)"
}
func (e notWritableError) Unwrap() error { return e.err }

// notAppliedError is a list that verified but cannot be put in force right now.
type notAppliedError struct{ err error }

func (e notAppliedError) Error() string {
	return "machine list: this machine cannot put the list in force: " + e.err.Error()
}
func (e notAppliedError) Unwrap() error { return e.err }

// pushRefusal is the one place a refusal becomes a wire answer. The kind says
// whose fault it is: invalid, the list; conflict, a list no newer than the one
// accepted; unsupported, this machine's setup. An error nobody matched is
// "not applied": the list may be fine and this machine could not take it.
//
// The error is matched as it arrived, and only its message is made safe to send:
// redacting first would flatten it to a string and lose what it wraps.
func pushRefusal(err error, self fleet.MachineId, scrub func(string) string) *fleet.Error {
	msg := scrub(err.Error())
	mk := func(kind fleet.ErrorKind, reason string) *fleet.Error {
		return &fleet.Error{Kind: kind, Reason: reason, Message: msg, Machine: self}
	}
	var nn notNewerError
	switch {
	case errors.As(err, &nn), errors.Is(err, machinelist.ErrRollback):
		return mk(fleet.ErrorConflict, fleet.ReasonMachineListNotNewer)
	case errors.Is(err, machinelist.ErrFormat):
		return mk(fleet.ErrorInvalid, fleet.ReasonMachineListMalformed)
	case errors.Is(err, machinelist.ErrUnknownSigner):
		return mk(fleet.ErrorInvalid, fleet.ReasonMachineListSignerNotPinned)
	case errors.Is(err, machinelist.ErrSignature):
		return mk(fleet.ErrorInvalid, fleet.ReasonMachineListSignature)
	case errors.Is(err, machinelist.ErrFleet):
		return mk(fleet.ErrorInvalid, fleet.ReasonMachineListOtherFleet)
	case errors.Is(err, errListGrant):
		return mk(fleet.ErrorInvalid, fleet.ReasonMachineListGrant)
	case errors.Is(err, machinelist.ErrUnsupported):
		return mk(fleet.ErrorUnsupported, fleet.ReasonMachineListUnsupported)
	case errors.As(err, new(notWritableError)):
		return mk(fleet.ErrorUnsupported, fleet.ReasonMachineListNotWritable)
	}
	return mk(fleet.ErrorUnsupported, fleet.ReasonMachineListNotApplied)
}

// acceptPushed takes a list a peer pushed. It holds the reloader's lock for the
// whole of it, and the body has already been read in full, so a slow upload never
// holds that lock. It does not watch the request: once started it finishes, so a
// disconnect cannot leave it half done.
//
// The order is the point. Everything that can refuse comes before the file is
// touched, and the file write is the last step that can be undone; after it only
// the sequence record can fail, and that restores the file. So the answer is
// binary: a nil error means the list is written AND in force, and any error means
// nothing changed, on disk or in memory.
func (rl *reloader) acceptPushed(raw []byte, by string) (fleet.MachineListReceipt, error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cfg := rl.applied
	ml := cfg.MachineList
	mode := ml.mode()
	api := machineListAPI{rl}
	if mode == listModeOff {
		return fleet.MachineListReceipt{}, api.off()
	}
	if rl.statusDir == "" && (mode == listModePrefer || mode == listModeOnly) {
		return fleet.MachineListReceipt{}, api.fe(fleet.ErrorUnsupported, fleet.ReasonMachineListNoStateDir,
			"this machine has no state directory (FLEET_STATE_DIR): a list could not be remembered as the highest accepted")
	}

	hi, err := rl.highestAccepted()
	if err != nil {
		return fleet.MachineListReceipt{}, notAppliedError{err}
	}
	pins, err := ml.pinned()
	if err != nil {
		return fleet.MachineListReceipt{}, notAppliedError{err}
	}
	cand := listOutcome{mode: mode, path: ml.Path, stateDir: rl.statusDir, self: rl.self, rawHash: contentHash(raw)}.
		verifyRaw(ml, pins, raw, hi)
	if cand.err != nil {
		return fleet.MachineListReceipt{}, cand.err
	}
	doc := cand.verified.Document
	if doc.Seq <= hi {
		return fleet.MachineListReceipt{}, notNewerError{seq: doc.Seq, highest: hi}
	}

	// Everything the list would change is planned before anything is written, the
	// way a reload does it: a peer that cannot be built refuses the list now, not
	// after the file has been replaced.
	basePrincipals, err := cfg.principals()
	if err != nil {
		return fleet.MachineListReceipt{}, notAppliedError{err}
	}
	handPeers, err := resolvePeers(rl.getenv("FLEET_PEERS"), cfg)
	if err != nil {
		return fleet.MachineListReceipt{}, notAppliedError{fmt.Errorf("bad peer list: %w", err)}
	}
	build, keep, err := rl.planPeers(cand.peers(handPeers), cfg)
	if err != nil {
		return fleet.MachineListReceipt{}, notAppliedError{err}
	}
	shutdown := func() {
		for _, made := range build {
			made.drv.Shutdown()
		}
	}

	prev, readErr := os.ReadFile(ml.Path)
	hadFile := readErr == nil
	if err := machinelist.WriteFile(ml.Path, raw); err != nil {
		shutdown()
		return fleet.MachineListReceipt{}, notWritableError{err}
	}
	// The sequence is recorded as the last step that can fail, as in a reload, so
	// a list that was going to be refused never moves the rollback guard.
	if err := cand.commit(); err != nil {
		var rerr error
		if hadFile {
			rerr = machinelist.WriteFile(ml.Path, prev)
		} else {
			rerr = os.Remove(ml.Path)
		}
		if rerr != nil {
			// Not expected, and worth saying loudly: the next poll judges the
			// pushed file through the ordinary reload rules and doctor reports it.
			log.Printf("muster: machine list push from %q: could not restore the previous list file after a failed sequence record: %v", by, rerr)
		}
		shutdown()
		return fleet.MachineListReceipt{}, notAppliedError{fmt.Errorf("recording the machine list sequence: %w", err)}
	}

	listHash := contentHash(raw)
	rl.swapIn("push from "+by, cfg, "", basePrincipals, handPeers, cand, build, keep, false, true, listHash, nil)
	log.Printf("muster: machine list push from %q: applied fleet %q seq %d (was %d, mode %s)", by, doc.Fleet, doc.Seq, hi, mode)
	return fleet.MachineListReceipt{
		Fleet: doc.Fleet, Seq: doc.Seq, PreviousSeq: hi, Mode: mode, Machines: len(doc.Machines),
	}, nil
}
