package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/machinelist"
	"github.com/futurelastic/muster/internal/peerkey"
)

// `muster machine-list push` and `fetch` (muster #352): the client side of
// handing a signed list from one machine to another.
//
//	push  MACHINE...   send the list this machine holds to each named peer
//	fetch MACHINE      read the list a peer has in force, and verify it here
//
// Both are signed with THIS machine's key, so a peer sees which machine is
// asking and judges it by what that machine is granted there (read to fetch,
// push-list to push). They are one-shot commands an operator or a script runs;
// the service itself never pushes or pulls a list.
//
// fetch never installs anything. A list is put in force by the operator copying
// the file into place, or by a peer holding push-list handing it over.

// listClient is what both commands need: this machine's identity, its pins and
// its signing key.
type listClient struct {
	cfg    *fileConfig
	self   string
	dir    string
	signer *peerkey.Signer
	http   *http.Client
}

func newListClient(f listFlags, getenv func(string) string) (*listClient, error) {
	cfgPath := f.one["config"]
	if cfgPath == "" {
		cfgPath = getenv("FLEET_CONFIG")
	}
	if cfgPath == "" {
		return nil, errors.New("no config: set FLEET_CONFIG or pass --config=PATH")
	}
	self := f.one["self"]
	if self == "" {
		self = getenv("FLEET_MACHINE")
	}
	if self == "" {
		return nil, errors.New("this machine's id is needed: set FLEET_MACHINE or pass --self=ID")
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	if cfg.MachineList == nil || cfg.MachineList.mode() == listModeOff {
		return nil, errors.New("the config has no machineList block (or its mode is off): the signer to trust is pinned there")
	}
	dir, err := keyStateDir(f.one["state-dir"], getenv)
	if err != nil {
		return nil, err
	}
	signer, err := peerkey.NewSigner(filepath.Join(dir, machineKeyFile), self)
	if err != nil {
		return nil, fmt.Errorf("this machine's signing key: %w (`muster key init` creates it)", err)
	}
	return &listClient{cfg: cfg, self: self, dir: dir, signer: signer,
		http: &http.Client{Timeout: 15 * time.Second}}, nil
}

// target finds the peer to talk to: the effective peers (the config's, with the
// list over them in prefer and only), and failing that the machine the list this
// command is about to send names. It must authenticate by key, since the request
// is signed.
func (c *listClient) target(id string, fromList *machinelist.Document, getenv func(string) string) (peerEntry, error) {
	hand, err := resolvePeers(getenv("FLEET_PEERS"), c.cfg)
	if err != nil {
		return peerEntry{}, err
	}
	held := loadMachineList(c.cfg, c.dir, fleet.MachineId(c.self))
	for _, e := range held.peers(hand) {
		if string(e.machine) == id {
			return c.keyed(e)
		}
	}
	// The list being sent names the machine too. This is what makes push usable
	// while every machine is still in shadow, when the list adds no peer yet.
	// A machine is not its own peer, and one with no address cannot be called.
	if fromList != nil && id != c.self {
		for _, m := range fromList.Machines {
			if m.ID == id && len(m.Addresses) > 0 {
				return c.keyed(peerEntry{machine: fleet.MachineId(id), urls: append([]string(nil), m.Addresses...), auth: peerAuthKey})
			}
		}
	}
	return peerEntry{}, fmt.Errorf("%q is not a peer of this machine (neither in the config's peers nor in the machine list)", id)
}

func (c *listClient) keyed(e peerEntry) (peerEntry, error) {
	if e.auth != peerAuthKey && c.cfg.peerAuthFor(e.machine) != peerAuthKey {
		return peerEntry{}, fmt.Errorf("peer %q authenticates by token here; handing a list over is signed with this machine's key, "+
			"so the peer must be a key peer (\"auth\": \"key\")", e.machine)
	}
	if len(e.urls) == 0 {
		return peerEntry{}, fmt.Errorf("peer %q has no address", e.machine)
	}
	return e, nil
}

// apiError reads the error envelope a refusal carries.
func apiError(resp *http.Response) *fleet.Error {
	var env fleet.ErrorEnvelope
	if json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&env) == nil && env.Error.Message != "" {
		return &env.Error
	}
	return &fleet.Error{Kind: fleet.ErrorUnreachable, Message: resp.Status}
}

// sendList PUTs a list to one peer, trying its addresses in order. It moves to
// the next address ONLY when no response came back: once a peer has answered,
// accepted or refused, sending the same list to another of its addresses would
// be a second attempt at something already decided.
func (c *listClient) sendList(ctx context.Context, e peerEntry, raw []byte) (fleet.MachineListReceipt, *fleet.Error, error) {
	var lastTransport error
	for _, base := range e.urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(base, "/")+"/v1/machine-list", bytes.NewReader(raw))
		if err != nil {
			return fleet.MachineListReceipt{}, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.signer.Sign(req, raw, string(e.machine)); err != nil {
			return fleet.MachineListReceipt{}, nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastTransport = err
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var rec fleet.MachineListReceipt
			if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&rec); err != nil {
				return fleet.MachineListReceipt{}, nil, fmt.Errorf("%s answered 200 with a body that is not a receipt: %w", base, err)
			}
			return rec, nil, nil
		}
		return fleet.MachineListReceipt{}, apiError(resp), nil
	}
	return fleet.MachineListReceipt{}, nil, fmt.Errorf("no address of %s answered: %w", e.machine, lastTransport)
}

// listPush is `muster machine-list push MACHINE... [--file=PATH]`.
func listPush(f listFlags, getenv func(string) string, stdout io.Writer) error {
	if len(f.pos) == 0 {
		return errors.New("push needs at least one MACHINE to hand the list to")
	}
	c, err := newListClient(f, getenv)
	if err != nil {
		return err
	}
	path := f.one["file"]
	if path == "" {
		path = c.cfg.MachineList.Path
	}
	raw, err := readListFile(path)
	if err != nil {
		return err
	}
	// Refuse to send a list this machine would not take itself: the same
	// verification a start does, against the same pins and the highest sequence
	// accepted here, so a typo or a stale file fails on this side with the reason
	// rather than on every peer.
	pins, err := c.cfg.MachineList.pinned()
	if err != nil {
		return err
	}
	var minSeq uint64
	if minSeq, err = machinelist.ReadSeq(c.dir); err != nil {
		return err
	}
	cand := listOutcome{self: fleet.MachineId(c.self)}.verifyRaw(c.cfg.MachineList, pins, raw, minSeq)
	if cand.err != nil {
		return fmt.Errorf("this machine would not take that list itself, so it is not sent: %w", cand.err)
	}
	doc := cand.verified.Document
	fmt.Fprintf(stdout, "sending fleet %q seq %d (%d machine(s), signed by %s)\n",
		doc.Fleet, doc.Seq, len(doc.Machines), peerkey.Fingerprint(cand.verified.Signer))

	refused := 0
	for _, id := range f.pos {
		e, err := c.target(id, &doc, getenv)
		if err != nil {
			refused++
			fmt.Fprintf(stdout, "  %-20s NOT sent: %v\n", id, err)
			continue
		}
		rec, ferr, terr := c.sendList(context.Background(), e, raw)
		switch {
		case terr != nil:
			refused++
			fmt.Fprintf(stdout, "  %-20s NOT sent: %v\n", id, terr)
		case ferr != nil:
			refused++
			reason := ferr.Reason
			if reason == "" {
				reason = string(ferr.Kind)
			}
			fmt.Fprintf(stdout, "  %-20s REFUSED %s: %s\n", id, reason, ferr.Message)
		default:
			fmt.Fprintf(stdout, "  %-20s accepted: seq %d now in force (was %d, mode %s)\n", id, rec.Seq, rec.PreviousSeq, rec.Mode)
		}
	}
	if refused > 0 {
		return fmt.Errorf("%d of %d machine(s) did not take the list", refused, len(f.pos))
	}
	return nil
}

// listFetch is `muster machine-list fetch MACHINE [--out=FILE]`: read the list a
// peer has in force, verify it against what THIS machine pins, and write it
// out. It never installs it.
func listFetch(f listFlags, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(f.pos) != 1 {
		return errors.New("fetch takes one MACHINE")
	}
	c, err := newListClient(f, getenv)
	if err != nil {
		return err
	}
	e, err := c.target(f.pos[0], nil, getenv)
	if err != nil {
		return err
	}
	var raw []byte
	var lastTransport error
	for _, base := range e.urls {
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/v1/machine-list", nil)
		if err != nil {
			return err
		}
		if err := c.signer.Sign(req, nil, string(e.machine)); err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastTransport = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			ferr := apiError(resp)
			resp.Body.Close()
			reason := ferr.Reason
			if reason == "" {
				reason = string(ferr.Kind)
			}
			return fmt.Errorf("%s refused: %s: %s", e.machine, reason, ferr.Message)
		}
		raw, err = io.ReadAll(io.LimitReader(resp.Body, machinelist.MaxBytes+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		lastTransport = nil
		break
	}
	if raw == nil {
		return fmt.Errorf("no address of %s answered: %w", e.machine, lastTransport)
	}
	pins, err := c.cfg.MachineList.pinned()
	if err != nil {
		return err
	}
	v, err := machinelist.Verify(raw, machinelist.Verifier{Fleet: c.cfg.MachineList.Fleet, Signers: pins})
	if err != nil {
		return fmt.Errorf("what %s served does not verify against what this machine pins, so it is not written: %w", e.machine, err)
	}
	d := v.Document
	fmt.Fprintf(stderr, "%s holds fleet %q seq %d (%d machine(s), signed by %s); verified here\n",
		e.machine, d.Fleet, d.Seq, len(d.Machines), peerkey.Fingerprint(v.Signer))
	if out := f.one["out"]; out != "" {
		return machinelist.WriteFile(out, raw)
	}
	_, err = stdout.Write(raw)
	return err
}

func readListFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("no list to send: the config's machineList has no path, and no --file was given")
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, machinelist.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > machinelist.MaxBytes {
		return nil, fmt.Errorf("%s is larger than a machine list may be (%d bytes)", path, machinelist.MaxBytes)
	}
	return raw, nil
}
