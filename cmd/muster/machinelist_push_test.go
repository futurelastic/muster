package main

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/machinelist"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/service"
)

// muster #352: a machine list handed over by a peer, on a running service. These
// drive the holder the HTTP routes ask (machineListAPI), over the same rig the
// file reload is tested on, so a pushed list and a changed file are held to the
// same rules.

func pushHolder(r *reloadRig) machineListAPI { return machineListAPI{r.rl} }

// signRaw signs a list naming machines (id -> address) at seq with owner, with
// edit given a chance to change the document before it is signed.
func signRaw(t *testing.T, owner ed25519.PrivateKey, seq uint64, machines map[string]string, edit func(*machinelist.Document)) []byte {
	t.Helper()
	doc := machinelist.Document{
		Fleet: "fleet-1", Seq: seq, Signers: []string{listPub(owner)},
		Machines: []machinelist.Machine{}, Issuers: []machinelist.Issuer{},
	}
	i := 0
	for _, id := range sortedKeys(machines) {
		doc.Machines = append(doc.Machines, machinelist.Machine{
			ID: id, PublicKey: listPub(listKey(byte(0x40 + i))),
			Addresses: []string{machines[id]}, Grants: []string{"read"},
		})
		i++
	}
	if edit != nil {
		edit(&doc)
	}
	raw, err := machinelist.Sign(doc, owner)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// snapshot is everything a refused push must leave exactly as it was.
type pushSnapshot struct {
	file   []byte
	mark   uint64
	status []byte
	inForc []byte
}

func takeSnapshot(t *testing.T, r *reloadRig, l liveList) pushSnapshot {
	t.Helper()
	file, _ := os.ReadFile(l.path)
	mark, err := machinelist.ReadSeq(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := os.ReadFile(filepath.Join(r.dir, reloadStatusFile))
	return pushSnapshot{file: file, mark: mark, status: status, inForc: append([]byte(nil), r.rl.listOut.raw...)}
}

func (s pushSnapshot) assertUnchanged(t *testing.T, r *reloadRig, l liveList) {
	t.Helper()
	now := takeSnapshot(t, r, l)
	if !bytes.Equal(now.file, s.file) {
		t.Error("the list file changed on a refused push")
	}
	if now.mark != s.mark {
		t.Errorf("the highest accepted sequence moved from %d to %d on a refused push", s.mark, now.mark)
	}
	if !bytes.Equal(now.status, s.status) {
		t.Errorf("the reload status changed on a refused push:\nbefore %s\nafter  %s", s.status, now.status)
	}
	if !bytes.Equal(now.inForc, s.inForc) {
		t.Error("the list in force changed on a refused push")
	}
	entries, _ := os.ReadDir(filepath.Dir(l.path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") && strings.Contains(e.Name(), "machines") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func wantRefusal(t *testing.T, ferr *fleet.Error, kind fleet.ErrorKind, reason string) {
	t.Helper()
	if ferr == nil {
		t.Fatalf("the push was taken, want %s %s", kind, reason)
	}
	if ferr.Kind != kind || ferr.Reason != reason {
		t.Fatalf("refusal = %s %q (%s), want %s %q", ferr.Kind, ferr.Reason, ferr.Message, kind, reason)
	}
}

func TestAcceptPushed_RefusalReasons(t *testing.T) {
	good := map[string]string{"m-a": "http://127.0.0.1:1"}
	cases := []struct {
		name   string
		raw    func(t *testing.T, r *reloadRig, l liveList) []byte
		before func(t *testing.T, r *reloadRig, l liveList)
		kind   fleet.ErrorKind
		reason string
	}{
		{"not json", func(*testing.T, *reloadRig, liveList) []byte { return []byte("{nope") }, nil,
			fleet.ErrorInvalid, fleet.ReasonMachineListMalformed},
		{"trailing content", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return append(signRaw(t, l.owner, 6, good, nil), []byte(`{"x":1}`)...)
		}, nil, fleet.ErrorInvalid, fleet.ReasonMachineListMalformed},
		{"signed by a stranger", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, listKey(0x77), 6, good, nil)
		}, nil, fleet.ErrorInvalid, fleet.ReasonMachineListSignerNotPinned},
		{"another fleet", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, l.owner, 6, good, func(d *machinelist.Document) { d.Fleet = "fleet-2" })
		}, nil, fleet.ErrorInvalid, fleet.ReasonMachineListOtherFleet},
		{"two signers", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, l.owner, 6, good, func(d *machinelist.Document) {
				d.Signers = append(d.Signers, listPub(listKey(0x78)))
			})
		}, nil, fleet.ErrorUnsupported, fleet.ReasonMachineListUnsupported},
		{"an unknown grant", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, l.owner, 6, good, func(d *machinelist.Document) { d.Machines[0].Grants = []string{"teleport"} })
		}, nil, fleet.ErrorInvalid, fleet.ReasonMachineListGrant},
		{"a list naming push-list", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, l.owner, 6, good, func(d *machinelist.Document) { d.Machines[0].Grants = []string{"read", "push-list"} })
		}, nil, fleet.ErrorInvalid, fleet.ReasonMachineListGrant},
		{"a lower sequence", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, l.owner, 4, good, nil)
		}, nil, fleet.ErrorConflict, fleet.ReasonMachineListNotNewer},
		{"the same sequence, different bytes", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, l.owner, 5, map[string]string{"m-a": "http://127.0.0.1:9"}, nil)
		}, nil, fleet.ErrorConflict, fleet.ReasonMachineListNotNewer},
		{"the very list in force", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return append([]byte(nil), r.rl.listOut.raw...)
		}, nil, fleet.ErrorConflict, fleet.ReasonMachineListNotNewer},
		{"a peer that cannot be built", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, l.owner, 6, map[string]string{"m-a": "http://127.0.0.1:1", "m-new": "http://127.0.0.1:2"}, nil)
		}, func(t *testing.T, r *reloadRig, l liveList) {
			// A key peer needs this machine's own signing key; take it away (and the
			// copy the builder already opened at the start).
			if err := os.Remove(filepath.Join(r.dir, machineKeyFile)); err != nil {
				t.Fatal(err)
			}
			r.rl.builder.mu.Lock()
			r.rl.builder.signer = nil
			r.rl.builder.mu.Unlock()
		}, fleet.ErrorUnsupported, fleet.ReasonMachineListNotApplied},
		{"a list file that cannot be written", func(t *testing.T, r *reloadRig, l liveList) []byte {
			return signRaw(t, l.owner, 6, good, nil)
		}, func(t *testing.T, r *reloadRig, l liveList) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("directory permissions do not stop this user")
			}
			// The list lives in its own directory so the state directory stays usable.
			sub := filepath.Join(r.dir, "listdir")
			if err := os.Mkdir(sub, 0o700); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(sub, "machines.json")
			if err := os.Rename(l.path, moved); err != nil {
				t.Fatal(err)
			}
			cfg := *r.rl.applied
			ml := *cfg.MachineList
			ml.Path = moved
			cfg.MachineList = &ml
			r.rl.applied = &cfg
			if err := os.Chmod(sub, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
		}, fleet.ErrorUnsupported, fleet.ReasonMachineListNotWritable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, l := newListRig(t, "only", 5, good)
			raw := tc.raw(t, r, l)
			if tc.before != nil {
				tc.before(t, r, l)
			}
			path := r.rl.applied.MachineList.Path
			l.path = path
			snap := takeSnapshot(t, r, l)
			_, ferr := pushHolder(r).Accept(raw, "tester")
			wantRefusal(t, ferr, tc.kind, tc.reason)
			if ferr.Message == "" {
				t.Error("a refusal with no message")
			}
			if strings.Contains(ferr.Message, r.dir) {
				t.Errorf("the message carries a filesystem path: %q", ferr.Message)
			}
			snap.assertUnchanged(t, r, l)
		})
	}
}

func TestAcceptPushed_RefusedWhenTheListIsOffOrHasNoStateDir(t *testing.T) {
	// Off: no machineList block at all.
	r := newReloadRig(t, rlDoc(rlPrincipal("op", "tok-op", "read")))
	if ferr := pushHolder(r).CanAccept(); ferr == nil || ferr.Reason != fleet.ReasonMachineListOff || ferr.Kind != fleet.ErrorUnsupported {
		t.Fatalf("CanAccept with no list = %+v", ferr)
	}
	if _, ferr := pushHolder(r).Accept([]byte("x"), "t"); ferr == nil || ferr.Reason != fleet.ReasonMachineListOff {
		t.Fatalf("Accept with no list = %+v", ferr)
	}
	if _, ferr := pushHolder(r).Held(); ferr == nil || ferr.Reason != fleet.ReasonMachineListOff {
		t.Fatalf("Held with no list = %+v", ferr)
	}

	// prefer and only need a state directory.
	for _, mode := range []string{"prefer", "only"} {
		t.Run(mode, func(t *testing.T) {
			rr, _ := newListRig(t, mode, 2, map[string]string{"m-a": "http://127.0.0.1:1"})
			rr.rl.statusDir = ""
			ferr := pushHolder(rr).CanAccept()
			if ferr == nil || ferr.Reason != fleet.ReasonMachineListNoStateDir || ferr.Kind != fleet.ErrorUnsupported {
				t.Fatalf("CanAccept = %+v", ferr)
			}
			if _, ferr := pushHolder(rr).Accept(signRaw(t, listKey(0x01), 3, map[string]string{"m-a": "http://127.0.0.1:1"}, nil), "t"); ferr == nil || ferr.Reason != fleet.ReasonMachineListNoStateDir {
				t.Fatalf("Accept = %+v", ferr)
			}
		})
	}
}

func TestAcceptPushed_StrictlyHigherSeq(t *testing.T) {
	good := map[string]string{"m-a": "http://127.0.0.1:1"}
	r, l := newListRig(t, "only", 5, good)
	h := pushHolder(r)

	try := func(seq uint64, addr string) *fleet.Error {
		_, ferr := h.Accept(signRaw(t, l.owner, seq, map[string]string{"m-a": addr}, nil), "tester")
		return ferr
	}
	wantRefusal(t, try(4, "http://127.0.0.1:1"), fleet.ErrorConflict, fleet.ReasonMachineListNotNewer)
	wantRefusal(t, try(5, "http://127.0.0.1:2"), fleet.ErrorConflict, fleet.ReasonMachineListNotNewer)
	_, ferr := h.Accept(append([]byte(nil), r.rl.listOut.raw...), "tester")
	wantRefusal(t, ferr, fleet.ErrorConflict, fleet.ReasonMachineListNotNewer)
	if !strings.Contains(ferr.Message, "(5)") {
		t.Errorf("the refusal should say what this machine has accepted: %q", ferr.Message)
	}

	if ferr := try(6, "http://127.0.0.1:3"); ferr != nil {
		t.Fatalf("seq 6 over 5: %+v", ferr)
	}
	wantRefusal(t, try(6, "http://127.0.0.1:4"), fleet.ErrorConflict, fleet.ReasonMachineListNotNewer)
	if mark, _ := machinelist.ReadSeq(r.dir); mark != 6 {
		t.Errorf("the highest accepted sequence = %d, want 6", mark)
	}
}

func TestHighestAcceptedIsOneNumber(t *testing.T) {
	good := map[string]string{"m-a": "http://127.0.0.1:1"}

	t.Run("a list in force: the reload and the push measure against the same number", func(t *testing.T) {
		r, _ := newListRig(t, "prefer", 7, good)
		r.rl.mu.Lock()
		hi, err := r.rl.highestAccepted()
		r.rl.mu.Unlock()
		if err != nil || hi != 7 || r.rl.listOut.verified.Document.Seq != 7 {
			t.Fatalf("highest = %d (%v), in force %d", hi, err, r.rl.listOut.verified.Document.Seq)
		}
	})

	t.Run("prefer: a mark with nothing in force is the floor", func(t *testing.T) {
		r, l := newListRig(t, "prefer", 7, good)
		r.rl.listOut = listOutcome{mode: listModePrefer} // the start found nothing usable
		wantRefusalOf(t, pushHolder(r), signRaw(t, l.owner, 7, map[string]string{"m-a": "http://127.0.0.1:2"}, nil), fleet.ReasonMachineListNotNewer)
		if _, ferr := pushHolder(r).Accept(signRaw(t, l.owner, 8, good, nil), "t"); ferr != nil {
			t.Fatalf("seq 8 over a mark of 7: %+v", ferr)
		}
	})

	t.Run("shadow without a state directory: the list in force is the floor, and no mark is written", func(t *testing.T) {
		r, l := newListRig(t, "shadow", 3, good)
		r.rl.statusDir = ""
		wantRefusalOf(t, pushHolder(r), signRaw(t, l.owner, 3, map[string]string{"m-a": "http://127.0.0.1:2"}, nil), fleet.ReasonMachineListNotNewer)
		rec, ferr := pushHolder(r).Accept(signRaw(t, l.owner, 4, good, nil), "t")
		if ferr != nil {
			t.Fatalf("seq 4 over 3: %+v", ferr)
		}
		if rec.Mode != "shadow" || rec.Seq != 4 || rec.PreviousSeq != 3 {
			t.Errorf("receipt = %+v", rec)
		}
		if _, err := os.Stat(filepath.Join(r.dir, machinelist.SeqFile)); !os.IsNotExist(err) {
			t.Error("shadow recorded a sequence mark: it must write nothing there")
		}
	})
}

func wantRefusalOf(t *testing.T, h machineListAPI, raw []byte, reason string) {
	t.Helper()
	_, ferr := h.Accept(raw, "tester")
	if ferr == nil || ferr.Reason != reason {
		t.Fatalf("refusal = %+v, want reason %q", ferr, reason)
	}
}

func TestAcceptPushed_AppliesLiveThroughTheReload(t *testing.T) {
	r, l := newListRig(t, "only", 1, map[string]string{"m-a": "http://127.0.0.1:1", "m-b": "http://127.0.0.1:2"})
	stream := r.stream("tok-op")
	if b := liveMachinesBody(r); !strings.Contains(b, `"m-b"`) {
		t.Fatalf("the start did not apply the list: %s", b)
	}

	raw := signRaw(t, l.owner, 2, map[string]string{"m-a": "http://127.0.0.1:1", "m-c": "http://127.0.0.1:3"}, nil)
	rec, ferr := pushHolder(r).Accept(raw, "peer-x")
	if ferr != nil {
		t.Fatalf("push: %+v", ferr)
	}
	// No rl.check: a push is applied in the request, not at the next poll.
	b := liveMachinesBody(r)
	if !strings.Contains(b, `"m-c"`) || strings.Contains(b, `"m-b"`) {
		t.Errorf("the pushed list is not in force: %s", b)
	}
	if !liveHasPrincipal(r, "m-c") || liveHasPrincipal(r, "m-b") {
		t.Errorf("the principal table does not follow the pushed list: %+v", r.rl.table.Snapshot())
	}
	if rec.Seq != 2 || rec.PreviousSeq != 1 || rec.Mode != "only" || rec.Machines != 2 || rec.Fleet != "fleet-1" {
		t.Errorf("receipt = %+v", rec)
	}
	if got, _ := os.ReadFile(l.path); !bytes.Equal(got, raw) {
		t.Error("the file on disk is not byte-for-byte what was pushed")
	}
	if mark, _ := machinelist.ReadSeq(r.dir); mark != 2 {
		t.Errorf("the highest accepted sequence = %d, want 2", mark)
	}
	st := r.rl.readStatus()
	if st.ListAppliedSeq != 2 || st.ListAppliedHash != contentHash(raw) || st.ListRefusedHash != "" || st.ListError != "" {
		t.Errorf("status = %+v", st)
	}
	if held, ferr := pushHolder(r).Held(); ferr != nil || !bytes.Equal(held, raw) {
		t.Errorf("Held = %q, %+v; want the pushed bytes", held, ferr)
	}

	// The poll that follows sees nothing new: no rewrite of the status, no rebuild.
	before, _ := os.ReadFile(filepath.Join(r.dir, reloadStatusFile))
	r.rl.check("poll")
	after, _ := os.ReadFile(filepath.Join(r.dir, reloadStatusFile))
	if !bytes.Equal(before, after) {
		t.Errorf("the poll after a push rewrote the status:\n%s\n%s", before, after)
	}
	if rlEnds(stream, 300*time.Millisecond) {
		t.Error("an unrelated stream was closed by a pushed list")
	}
}

func TestAcceptPushed_ReplacesAHandPlacedFile(t *testing.T) {
	good := map[string]string{"m-a": "http://127.0.0.1:1"}
	r, l := newListRig(t, "only", 5, good)
	// An operator drops in a list the service refuses (a rollback).
	l.put(t, 3, map[string]string{"m-z": "http://127.0.0.1:7"})
	r.rl.check("poll")
	if st := r.rl.readStatus(); st.ListRefusedHash == "" {
		t.Fatalf("the hand-placed list was not refused: %+v", st)
	}
	raw := signRaw(t, l.owner, 6, map[string]string{"m-a": "http://127.0.0.1:1", "m-b": "http://127.0.0.1:2"}, nil)
	if _, ferr := pushHolder(r).Accept(raw, "peer-x"); ferr != nil {
		t.Fatalf("push over a hand-placed file: %+v", ferr)
	}
	if got, _ := os.ReadFile(l.path); !bytes.Equal(got, raw) {
		t.Error("the hand-placed file was not replaced")
	}
	if st := r.rl.readStatus(); st.ListRefusedHash != "" || st.ListAppliedSeq != 6 {
		t.Errorf("status after the push = %+v", st)
	}
}

func TestAcceptPushed_RestoresTheFileWhenTheSequenceCannotBeRecorded(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop this user")
	}
	good := map[string]string{"m-a": "http://127.0.0.1:1"}
	r, l := newListRig(t, "only", 5, good)
	// The state directory moves to one that holds the mark and then becomes
	// read-only, so the sequence cannot be recorded after the file is written.
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, machinelist.SeqFile), []byte("5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.rl.statusDir = state
	if err := os.Chmod(state, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })

	prev, _ := os.ReadFile(l.path)
	inForce := append([]byte(nil), r.rl.listOut.raw...)
	_, ferr := pushHolder(r).Accept(signRaw(t, l.owner, 6, map[string]string{"m-a": "http://127.0.0.1:1", "m-b": "http://127.0.0.1:2"}, nil), "peer-x")
	wantRefusal(t, ferr, fleet.ErrorUnsupported, fleet.ReasonMachineListNotApplied)
	if got, _ := os.ReadFile(l.path); !bytes.Equal(got, prev) {
		t.Error("the previous list file was not restored")
	}
	if !bytes.Equal(r.rl.listOut.raw, inForce) {
		t.Error("the list in force changed")
	}
	if strings.Contains(liveMachinesBody(r), `"m-b"`) {
		t.Error("the refused list's machine is live")
	}
	// The next poll finds nothing new: the restored file is the one already seen.
	r.rl.check("poll")
	if st := r.rl.readStatus(); st.ListRefusedHash != "" || st.ListError != "" {
		t.Errorf("the poll after a restored push recorded a refusal: %+v", st)
	}
}

func TestAcceptPushed_ConcurrentPushesSerialize(t *testing.T) {
	good := map[string]string{"m-a": "http://127.0.0.1:1"}
	r, l := newListRig(t, "only", 1, good)
	h := pushHolder(r)
	pushes := [][]byte{
		signRaw(t, l.owner, 2, map[string]string{"m-a": "http://127.0.0.1:2"}, nil),
		signRaw(t, l.owner, 2, map[string]string{"m-a": "http://127.0.0.1:3"}, nil),
		signRaw(t, l.owner, 3, map[string]string{"m-a": "http://127.0.0.1:4"}, nil),
	}
	stop := make(chan struct{})
	var polling sync.WaitGroup
	polling.Add(1)
	go func() {
		defer polling.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.rl.check("poll")
			}
		}
	}()
	var wg sync.WaitGroup
	results := make([]*fleet.Error, len(pushes))
	for i := range pushes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = h.Accept(pushes[i], "peer")
		}(i)
	}
	wg.Wait()
	close(stop)
	polling.Wait()

	if results[0] == nil && results[1] == nil {
		t.Error("both pushes at seq 2 were taken: only one list can be the list at seq 2")
	}
	for i, ferr := range results {
		if ferr != nil && ferr.Reason != fleet.ReasonMachineListNotNewer {
			t.Errorf("push %d refused for %q, only 'not newer' is a legitimate loser: %s", i, ferr.Reason, ferr.Message)
		}
	}
	if results[2] != nil {
		t.Errorf("the highest push (seq 3) was refused: %+v", results[2])
	}
	file, _ := os.ReadFile(l.path)
	if !bytes.Equal(file, r.rl.listOut.raw) {
		t.Error("the file on disk is not the list in force")
	}
	if mark, _ := machinelist.ReadSeq(r.dir); mark != r.rl.listOut.verified.Document.Seq || mark != 3 {
		t.Errorf("mark %d, in force %d, want both 3", mark, r.rl.listOut.verified.Document.Seq)
	}
}

// --- push-list comes from the operator, never from the list --------------------

func TestPushListComesFromTheOperatorNotTheList(t *testing.T) {
	aKey := listKey(0x11)
	aPub := aKey.Public().(ed25519.PublicKey)

	cfgWith := func(f *listFixture, mode string, handKey ed25519.PublicKey) *fileConfig {
		cfg := peersFrom(t, fmt.Sprintf(`{"peers":[{"machine":"machine-b","url":"http://old-b:1","token":"tb"}],
		  "principals":[{"name":"operator","token":"op","grants":["read"]},
		                {"name":"machine-a","publicKey":%q,"grants":["read","push-list"]}]}`, peerkey.FormatPublicKey(handKey)))
		cfg.MachineList = &machineListConfig{Mode: mode, Path: f.listPath, Fleet: "fleet-1", Signer: listPub(f.owner)}
		return cfg
	}
	effective := func(cfg *fileConfig, o listOutcome) map[string][]service.Grant {
		base, err := cfg.principals()
		if err != nil {
			t.Fatal(err)
		}
		hand, _ := resolvePeers("", cfg)
		out := map[string][]service.Grant{}
		for _, p := range o.principalsFor(base, hand) {
			out[p.Name] = p.Grants
		}
		return out
	}
	has := func(gs []service.Grant, g service.Grant) bool {
		for _, x := range gs {
			if x == g {
				return true
			}
		}
		return false
	}

	t.Run("a list that names push-list is refused whole", func(t *testing.T) {
		f, cfg := newListFixture(t, "only", 4)
		cfg.MachineList.Path = f.listPath
		doc := machinelist.Document{Fleet: "fleet-1", Seq: 5, Signers: []string{listPub(f.owner)},
			Machines: []machinelist.Machine{{ID: "machine-a", PublicKey: listPub(aKey), Addresses: []string{"http://a:1"}, Grants: []string{"read", "push-list"}}},
			Issuers:  []machinelist.Issuer{}}
		raw, err := machinelist.Sign(doc, f.owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.listPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		o := loadMachineList(cfg, f.stateDir, "self")
		if o.err == nil || !strings.Contains(o.err.Error(), "push-list") || o.fatal() == nil {
			t.Fatalf("a list naming push-list loaded: err=%v", o.err)
		}
	})

	t.Run("prefer and only carry the operator's grant to the listed machine of the same key", func(t *testing.T) {
		for _, mode := range []string{"prefer", "only"} {
			f, _ := newListFixture(t, mode, 4)
			cfg := cfgWith(f, mode, aPub)
			o := loadMachineList(cfg, f.stateDir, "self")
			if o.err != nil {
				t.Fatalf("%s: %v", mode, o.err)
			}
			got := effective(cfg, o)["machine-a"]
			if !has(got, service.GrantPushList) || !has(got, service.GrantSend) {
				t.Errorf("%s: effective machine-a grants = %v, want the list's read+send and the operator's push-list", mode, got)
			}
			if row := listDoctorRow(o, nil, mustPrincipals(t, cfg)); row.Status != statusPass || !strings.Contains(row.Detail, "machine-a") {
				t.Errorf("%s: doctor row = %+v, want a pass naming machine-a as a push-list holder", mode, row)
			}
		}
	})

	t.Run("a different key in the list lets the grant lapse, and doctor says so", func(t *testing.T) {
		f, _ := newListFixture(t, "prefer", 4)
		cfg := cfgWith(f, "prefer", listKey(0x99).Public().(ed25519.PublicKey))
		o := loadMachineList(cfg, f.stateDir, "self")
		if o.err != nil {
			t.Fatal(o.err)
		}
		if has(effective(cfg, o)["machine-a"], service.GrantPushList) {
			t.Error("push-list was carried to a machine whose key is not the one the operator named")
		}
		row := listDoctorRow(o, nil, mustPrincipals(t, cfg))
		if row.Status != statusWarn || !strings.Contains(row.Detail, "NOT carried over") {
			t.Errorf("doctor row = %+v, want a warn about the lapsed grant", row)
		}
	})

	t.Run("off and shadow leave the hand-written principal exactly as written", func(t *testing.T) {
		for _, mode := range []string{"off", "shadow"} {
			f, _ := newListFixture(t, "shadow", 4)
			cfg := cfgWith(f, mode, aPub)
			o := loadMachineList(cfg, f.stateDir, "self")
			base, _ := cfg.principals()
			if got := o.principalsFor(base, nil); len(got) != len(base) {
				t.Fatalf("%s: principals changed", mode)
			}
			for _, p := range o.principalsFor(base, nil) {
				if p.Name == "machine-a" && (!has(p.Grants, service.GrantPushList) || has(p.Grants, service.GrantSend)) {
					t.Errorf("%s: machine-a grants = %v, want exactly what was written", mode, p.Grants)
				}
			}
		}
	})
}

func mustPrincipals(t *testing.T, cfg *fileConfig) []service.Principal {
	t.Helper()
	ps, err := cfg.principals()
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// --- the CLI, end to end ------------------------------------------------------

// pushRig is two machines: B, a running service that takes pushes (a reload rig
// served over HTTP with the holder wired in, and a key principal for A), and A, a
// config and a state directory a `muster machine-list` command runs from.
type pushRig struct {
	b      *reloadRig
	l      liveList
	srvURL string
	aCfg   string
	aState string
	aList  string
	getenv func(string) string
}

func newPushRig(t *testing.T, bMode string, bSeq uint64, aMode string) *pushRig {
	t.Helper()
	// A's identity: a signing key in A's state directory, named "machine-a".
	aState := t.TempDir()
	aPub, aPriv, err := peerkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := peerkey.SavePrivate(filepath.Join(aState, machineKeyFile), aPriv); err != nil {
		t.Fatal(err)
	}

	// B: a list at bSeq, a machine key (list peers authenticate by key), and a key
	// principal for machine-a holding read and push-list.
	dir := t.TempDir()
	l := liveList{owner: listKey(0x01), path: filepath.Join(dir, "machines.json")}
	l.put(t, bSeq, map[string]string{"m-a": "http://127.0.0.1:1"})
	if _, priv, err := peerkey.Generate(); err != nil {
		t.Fatal(err)
	} else if err := peerkey.SavePrivate(filepath.Join(dir, machineKeyFile), priv); err != nil {
		t.Fatal(err)
	}
	doc := rlDoc(rlPrincipal("op", "tok-op", "read"), map[string]any{
		"name": "machine-a", "publicKey": peerkey.FormatPublicKey(aPub), "grants": []string{"read", "push-list"},
	})
	doc["machineList"] = map[string]any{"mode": bMode, "path": l.path, "fleet": "fleet-1", "signer": listPub(l.owner)}
	b := newReloadRigIn(t, dir, doc)
	srv := httptest.NewServer(service.NewMux(b.svc, service.Config{Table: b.rl.table, MachineList: machineListAPI{b.rl}}))
	t.Cleanup(srv.Close)

	// A: pins the same owner, knows B as a key peer named after B's service.
	aList := filepath.Join(t.TempDir(), "machines.json")
	aCfg := filepath.Join(t.TempDir(), "fleet.json")
	putCfg(t, aCfg, map[string]any{
		"principals":  []any{rlPrincipal("op", "tok", "read")},
		"peers":       []any{map[string]any{"machine": "test-machine", "url": srv.URL, "auth": "key"}},
		"machineList": map[string]any{"mode": aMode, "path": aList, "fleet": "fleet-1", "signer": listPub(l.owner)},
	})
	return &pushRig{b: b, l: l, srvURL: srv.URL, aCfg: aCfg, aState: aState, aList: aList,
		getenv: func(string) string { return "" }}
}

func (p *pushRig) writeA(t *testing.T, seq uint64, machines map[string]string) []byte {
	t.Helper()
	raw := signRaw(t, p.l.owner, seq, machines, nil)
	if err := os.WriteFile(p.aList, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

func (p *pushRig) run(args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	full := append([]string{"machine-list"}, args...)
	full = append(full, "--config="+p.aCfg, "--state-dir="+p.aState, "--self=machine-a")
	_, code = runMachineList(full, p.getenv, &o, &e)
	return code, o.String(), e.String()
}

func TestMachineListPushAndFetch_EndToEnd(t *testing.T) {
	// B runs `only` at seq 1; A is in shadow, the phase in which a list adds no
	// peer yet, so B is found through the config's own peer entry.
	p := newPushRig(t, "only", 1, "shadow")
	stream := p.b.stream("tok-op")
	raw := p.writeA(t, 2, map[string]string{"m-a": "http://127.0.0.1:1", "m-c": "http://127.0.0.1:3"})

	code, out, errOut := p.run("push", "test-machine")
	if code != 0 {
		t.Fatalf("push exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "accepted: seq 2 now in force (was 1, mode only)") {
		t.Errorf("push output:\n%s", out)
	}
	// B has it, live: no poll in between.
	if got, ferr := pushHolder(p.b).Held(); ferr != nil || !bytes.Equal(got, raw) {
		t.Fatalf("B holds %q (%+v), want the pushed bytes", got, ferr)
	}
	if body := liveMachinesBody(p.b); !strings.Contains(body, `"m-c"`) {
		t.Errorf("B's peers do not include the pushed list's machine: %s", body)
	}
	if rlEnds(stream, 300*time.Millisecond) {
		t.Error("a stream was closed by the push")
	}

	// fetch: byte-equal to what B holds, and it verifies here.
	out2 := filepath.Join(t.TempDir(), "fetched.json")
	code, _, errOut = p.run("fetch", "test-machine", "--out="+out2)
	if code != 0 {
		t.Fatalf("fetch exit %d\nstderr: %s", code, errOut)
	}
	if got, _ := os.ReadFile(out2); !bytes.Equal(got, raw) {
		t.Error("fetch did not write the list B holds, byte for byte")
	}
	if !strings.Contains(errOut, "verified here") || !strings.Contains(errOut, "seq 2") {
		t.Errorf("fetch said: %s", errOut)
	}
	// The same list again is not newer: a refusal, exit 1, and B is unchanged.
	code, out, _ = p.run("push", "test-machine")
	if code != 1 || !strings.Contains(out, "REFUSED machine-list-not-newer") {
		t.Fatalf("second push exit %d:\n%s", code, out)
	}
	if mark, _ := machinelist.ReadSeq(p.b.dir); mark != 2 {
		t.Errorf("B's highest accepted sequence = %d, want 2", mark)
	}
}

func TestMachineListPush_RefusesWhatThisMachineWouldNotTakeItself(t *testing.T) {
	p := newPushRig(t, "only", 1, "shadow")
	// A list naming push-list: refused on this side, before any peer is asked.
	raw := signRaw(t, p.l.owner, 2, map[string]string{"m-a": "http://127.0.0.1:1"}, func(d *machinelist.Document) {
		d.Machines[0].Grants = []string{"read", "push-list"}
	})
	if err := os.WriteFile(p.aList, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := p.run("push", "test-machine")
	if code != 1 || !strings.Contains(errOut, "would not take that list itself") || strings.Contains(out, "accepted") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if mark, _ := machinelist.ReadSeq(p.b.dir); mark != 1 {
		t.Errorf("a refused push moved B's sequence to %d", mark)
	}

	// A list signed by a stranger.
	if err := os.WriteFile(p.aList, signRaw(t, listKey(0x77), 2, map[string]string{"m-a": "http://127.0.0.1:1"}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := p.run("push", "test-machine"); code != 1 || !strings.Contains(errOut, "not pinned") {
		t.Fatalf("a stranger's list: exit %d: %s", code, errOut)
	}
}

func TestMachineListPush_TheTargetMustBeAKeyPeerThatThisMachineKnows(t *testing.T) {
	p := newPushRig(t, "only", 1, "shadow")
	p.writeA(t, 2, map[string]string{"m-a": "http://127.0.0.1:1"})

	code, out, _ := p.run("push", "nobody")
	if code != 1 || !strings.Contains(out, "is not a peer of this machine") {
		t.Fatalf("an unknown machine: exit %d\n%s", code, out)
	}

	// A token peer: the request is signed, so the peer must be a key peer.
	putCfg(t, p.aCfg, map[string]any{
		"principals":  []any{rlPrincipal("op", "tok", "read")},
		"peers":       []any{map[string]any{"machine": "test-machine", "url": p.srvURL, "token": "t"}},
		"machineList": map[string]any{"mode": "shadow", "path": p.aList, "fleet": "fleet-1", "signer": listPub(p.l.owner)},
	})
	code, out, _ = p.run("push", "test-machine")
	if code != 1 || !strings.Contains(out, "authenticates by token") {
		t.Fatalf("a token peer: exit %d\n%s", code, out)
	}
	if mark, _ := machinelist.ReadSeq(p.b.dir); mark != 1 {
		t.Errorf("B's sequence moved to %d", mark)
	}
}

func TestMachineListPush_APeerWithoutThePushGrantRefusesIt(t *testing.T) {
	p := newPushRig(t, "only", 1, "shadow")
	// Take push-list away from machine-a on B, as an operator would.
	doc := rlDoc(rlPrincipal("op", "tok-op", "read"), map[string]any{
		"name": "machine-a", "publicKey": publicKeyOf(t, p.aState), "grants": []string{"read"},
	})
	doc["machineList"] = map[string]any{"mode": "only", "path": p.l.path, "fleet": "fleet-1", "signer": listPub(p.l.owner)}
	putCfg(t, p.b.path, doc)
	p.b.rl.check("poll")

	p.writeA(t, 2, map[string]string{"m-a": "http://127.0.0.1:1"})
	code, out, _ := p.run("push", "test-machine")
	if code != 1 || !strings.Contains(out, "push-list") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if mark, _ := machinelist.ReadSeq(p.b.dir); mark != 1 {
		t.Errorf("B took a list from a peer without the grant: sequence %d", mark)
	}
	// And fetching still works: read is a separate, weaker grant.
	if code, _, errOut := p.run("fetch", "test-machine"); code != 0 {
		t.Fatalf("fetch with read only: exit %d: %s", code, errOut)
	}
}

func publicKeyOf(t *testing.T, stateDir string) string {
	t.Helper()
	priv, err := peerkey.LoadPrivate(filepath.Join(stateDir, machineKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return peerkey.FormatPublicKey(priv.Public().(ed25519.PublicKey))
}
