package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/machinelist"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/service"
)

// The owner-signed machine list in the service (muster #351).
//
// config.json says where the list is and what to trust:
//
//	"machineList": { "mode": "shadow", "path": "/etc/muster/machines.json",
//	                 "fleet": "my-fleet", "signer": "ed25519:...", "nextSigner": "ed25519:..." }
//
// The signer is pinned HERE, by a person; it is never read from the list.
//
// Modes:
//
//	off      the list is ignored (the default, and what an absent block means)
//	shadow   verify and report; peers and principals are exactly what the config says
//	prefer   the list's machines win over a hand-written peer of the same id;
//	         hand-written peers it does not name stay. A list that fails to verify is
//	         reported loudly and the hand-written config is used unchanged.
//	only     the list is the sole source of peers. A list that fails to verify stops
//	         the service from starting: it never falls back to hand-written peers.

const (
	listModeOff    = "off"
	listModeShadow = "shadow"
	listModePrefer = "prefer"
	listModeOnly   = "only"
)

// machineListConfig is the "machineList" block of the config file.
type machineListConfig struct {
	Mode  string `json:"mode,omitempty"`
	Path  string `json:"path,omitempty"`
	Fleet string `json:"fleet,omitempty"`
	// Signer is the pinned public key, "ed25519:<base64url>".
	Signer string `json:"signer,omitempty"`
	// NextSigner is the pre-pinned recovery key, optional.
	NextSigner string `json:"nextSigner,omitempty"`
}

// validate is the shape check loadConfig runs: nothing here reads the list.
func (m *machineListConfig) validate() error {
	if m == nil {
		return nil
	}
	switch m.Mode {
	case "", listModeOff, listModeShadow, listModePrefer, listModeOnly:
	default:
		return fmt.Errorf("machineList: mode %q is not off, shadow, prefer or only", m.Mode)
	}
	if m.mode() == listModeOff {
		return nil
	}
	if m.Path == "" {
		return errors.New("machineList: a mode other than off needs the list's path")
	}
	if strings.TrimSpace(m.Fleet) == "" {
		return errors.New("machineList: a mode other than off needs the fleet id this machine belongs to")
	}
	if _, err := m.pinned(); err != nil {
		return err
	}
	return nil
}

func (m *machineListConfig) mode() string {
	if m == nil || m.Mode == "" {
		return listModeOff
	}
	return m.Mode
}

// pinned is the locally pinned signing keys: the signer, then the recovery key.
func (m *machineListConfig) pinned() ([]ed25519.PublicKey, error) {
	if m.Signer == "" {
		return nil, errors.New("machineList: a mode other than off needs the signer's public key, pinned here")
	}
	k, err := peerkey.ParsePublicKey(m.Signer)
	if err != nil {
		return nil, fmt.Errorf("machineList: signer: %w", err)
	}
	out := []ed25519.PublicKey{k}
	if m.NextSigner != "" {
		n, err := peerkey.ParsePublicKey(m.NextSigner)
		if err != nil {
			return nil, fmt.Errorf("machineList: nextSigner: %w", err)
		}
		out = append(out, n)
	}
	return out, nil
}

// listOutcome is what reading the list gave: verified, or the reason it was not.
type listOutcome struct {
	mode     string
	path     string
	stateDir string
	self     fleet.MachineId

	verified *machinelist.Verified
	// principals are the list's machines as principals, grants already checked.
	principals []service.Principal
	err        error
}

// loadMachineList reads and verifies the list the config points at. It never
// fails the caller: a failure is the outcome's err, and what to do about it is
// decided by mode (fatal, or reported).
func loadMachineList(cfg *fileConfig, stateDir string, self fleet.MachineId) listOutcome {
	o := listOutcome{mode: listModeOff, stateDir: stateDir, self: self}
	if cfg == nil || cfg.MachineList == nil {
		return o
	}
	ml := cfg.MachineList
	o.mode, o.path = ml.mode(), ml.Path
	if o.mode == listModeOff {
		return o
	}
	pins, err := ml.pinned()
	if err != nil {
		o.err = err
		return o
	}
	// Rollback protection needs somewhere durable to remember the highest list
	// accepted. Without it prefer and only could be fed an old genuine file.
	if stateDir == "" && (o.mode == listModePrefer || o.mode == listModeOnly) {
		o.err = errors.New("machine list: FLEET_STATE_DIR is not set, and the highest list accepted is remembered there (without it an older genuine list could be replayed)")
		return o
	}
	var minSeq uint64
	if stateDir != "" {
		if minSeq, err = machinelist.ReadSeq(stateDir); err != nil {
			o.err = err
			return o
		}
	}
	raw, err := os.ReadFile(ml.Path)
	if err != nil {
		o.err = fmt.Errorf("machine list: %w", err)
		return o
	}
	v, err := machinelist.Verify(raw, machinelist.Verifier{Fleet: ml.Fleet, Signers: pins, MinSeq: minSeq})
	if err != nil {
		o.err = err
		return o
	}
	principals, err := machinePrincipals(v.Document)
	if err != nil {
		o.err = err
		return o
	}
	o.verified, o.principals = &v, principals
	return o
}

// machinePrincipals turns the list's machines into principals: a machine in the
// list may call this one with the grants the list gives it, proving itself by
// its key. Every grant must be one this build knows, or the whole list is
// refused — a grant silently dropped would be a machine quietly given less, or
// one that a newer owner meant to withhold, given more by a typo.
func machinePrincipals(doc machinelist.Document) ([]service.Principal, error) {
	var out []service.Principal
	for _, m := range doc.Machines {
		var grants []service.Grant
		for _, g := range m.Grants {
			gr := service.Grant(g)
			if !service.ValidGrant(gr) {
				return nil, fmt.Errorf("machine list: machine %q: unknown grant %q", m.ID, g)
			}
			grants = append(grants, gr)
		}
		k, err := peerkey.ParsePublicKey(m.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("machine list: machine %q: %w", m.ID, err)
		}
		out = append(out, service.Principal{Name: m.ID, PublicKey: k, Grants: grants})
	}
	return out, nil
}

// fatal reports whether the service must refuse to start: only-mode, and the
// list did not verify.
func (o listOutcome) fatal() error {
	if o.mode == listModeOnly && o.err != nil {
		return fmt.Errorf("machine list is the only source of peers and does not verify: %w", o.err)
	}
	return nil
}

// active reports whether a verified list is changing anything.
func (o listOutcome) active() bool {
	return o.verified != nil && (o.mode == listModePrefer || o.mode == listModeOnly)
}

// peers is the effective peer list. In shadow and off it is base, untouched.
func (o listOutcome) peers(base []peerEntry) []peerEntry {
	if o.fatal() != nil {
		return nil
	}
	if !o.active() {
		return base
	}
	fromList := map[fleet.MachineId]peerEntry{}
	var order []fleet.MachineId
	for _, m := range o.verified.Document.Machines {
		id := fleet.MachineId(m.ID)
		// This machine is in its own list; it is not its own peer. A machine with
		// no address can call others but cannot be called.
		if id == o.self || len(m.Addresses) == 0 {
			continue
		}
		fromList[id] = peerEntry{machine: id, urls: append([]string(nil), m.Addresses...), auth: peerAuthKey}
		order = append(order, id)
	}
	var out []peerEntry
	if o.mode == listModePrefer {
		for _, b := range base {
			if _, ok := fromList[b.machine]; !ok {
				out = append(out, b)
			}
		}
	}
	for _, id := range order {
		out = append(out, fromList[id])
	}
	return out
}

// principalsFor is the effective principal table. Principals that are clients
// (an operator's token, say) are never touched: the list is about machines. A
// principal the list names replaces a hand-written one of the same name; in
// only mode a hand-written principal named after a hand-written peer is dropped
// too, since that peer no longer exists. A machine removed from the list keeps
// any principal the config gave it under another route — the config owns those.
func (o listOutcome) principalsFor(base []service.Principal, handPeers []peerEntry) []service.Principal {
	if !o.active() {
		return base
	}
	listed := map[string]bool{}
	for _, p := range o.principals {
		if fleet.MachineId(p.Name) != o.self {
			listed[p.Name] = true
		}
	}
	dropped := map[string]bool{}
	if o.mode == listModeOnly {
		for _, h := range handPeers {
			dropped[string(h.machine)] = true
		}
	}
	var out []service.Principal
	for _, p := range base {
		if listed[p.Name] || dropped[p.Name] {
			continue
		}
		out = append(out, p)
	}
	for _, p := range o.principals {
		if listed[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

// commit records the list's seq as the highest accepted, so an older genuine
// list is refused from now on. Shadow does not: it changes nothing.
func (o listOutcome) commit() error {
	if !o.active() || o.stateDir == "" {
		return nil
	}
	return machinelist.WriteSeq(o.stateDir, o.verified.Document.Seq)
}

// resolveEffectivePeers is the one reading of the peer list the service and
// `muster doctor` share: resolvePeers, then the machine list over it.
func resolveEffectivePeers(getenv func(string) string, cfg *fileConfig) ([]peerEntry, listOutcome, error) {
	self := fleet.MachineId(getenv("FLEET_MACHINE"))
	if self == "" {
		self = "local"
	}
	base, err := resolvePeers(getenv("FLEET_PEERS"), cfg)
	o := loadMachineList(cfg, getenv("FLEET_STATE_DIR"), self)
	return o.peers(base), o, err
}

// describe is the one-line account of an outcome, for the log and for doctor.
func (o listOutcome) describe() string {
	switch {
	case o.mode == listModeOff:
		return "machine list off"
	case o.err != nil:
		return fmt.Sprintf("machine list (%s) does not verify: %v", o.mode, o.err)
	default:
		d := o.verified.Document
		s := fmt.Sprintf("machine list (%s): fleet %q seq %d, signed by %s, %d machine(s)",
			o.mode, d.Fleet, d.Seq, peerkey.Fingerprint(o.verified.Signer), len(d.Machines))
		if o.verified.Recovery {
			s += " — signed by the recovery key: pin it as the signer"
		}
		return s
	}
}

// shadowDiff says what prefer or only would change, for a machine running in
// shadow. It is the whole reason shadow exists: read this, then switch.
func (o listOutcome) shadowDiff(base []peerEntry) []string {
	if o.verified == nil {
		return nil
	}
	have := map[fleet.MachineId]peerEntry{}
	for _, b := range base {
		have[b.machine] = b
	}
	var out []string
	listed := map[fleet.MachineId]bool{}
	for _, m := range o.verified.Document.Machines {
		id := fleet.MachineId(m.ID)
		if id == o.self || len(m.Addresses) == 0 {
			continue
		}
		listed[id] = true
		h, ok := have[id]
		switch {
		case !ok:
			out = append(out, "+ "+m.ID+" (in the list, not in the config)")
		case strings.Join(h.urls, ",") != strings.Join(m.Addresses, ","):
			out = append(out, "~ "+m.ID+" (addresses differ from the config)")
		}
	}
	for _, b := range base {
		if !listed[b.machine] {
			out = append(out, "- "+string(b.machine)+" (in the config, not in the list: kept by prefer, dropped by only)")
		}
	}
	return out
}

// --- the muster machine-list command ----------------------------------------

func usageMachineList() string {
	return strings.Join([]string{
		"usage: muster machine-list keygen --out=FILE",
		"       muster machine-list create --fleet=ID (--key=FILE | --signer=PUBKEY) [--next-signer=PUBKEY] [--seq=N]",
		"                                  [--machine=ID,PUBKEY,ADDR|ADDR,GRANT|GRANT]... [--issuer=ID,PUBKEY]... [--out=FILE]",
		"       muster machine-list sign   --in=DRAFT --key=FILE [--seq=N] [--out=FILE]",
		"       muster machine-list verify FILE [--config=PATH | --fleet=ID --signer=PUBKEY [--min-seq=N]]",
		"       muster machine-list show   FILE [--config=PATH | --fleet=ID --signer=PUBKEY]",
		"",
		"keygen makes the owner's signing key. Keep it OFFLINE: the service never reads it.",
		"create writes an unsigned draft; edit it by hand if you prefer. sign signs it with the key.",
		"verify checks a list against what you pin (flags, or the machineList block of the config) and",
		"exits 1 if it does not verify. show prints what a list says; it marks the contents unverified",
		"unless you gave it something to verify against.",
	}, "\n")
}

type listFlags struct {
	one  map[string]string
	many map[string][]string
	pos  []string
}

func parseListFlags(args []string, repeatable ...string) (listFlags, error) {
	f := listFlags{one: map[string]string{}, many: map[string][]string{}}
	rep := map[string]bool{}
	for _, r := range repeatable {
		rep[r] = true
	}
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			f.pos = append(f.pos, a)
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !ok {
			return f, fmt.Errorf("%s needs a value: %s=VALUE", a, a)
		}
		if rep[k] {
			f.many[k] = append(f.many[k], v)
		} else {
			f.one[k] = v
		}
	}
	return f, nil
}

// runMachineList handles `muster machine-list ...`.
func runMachineList(args []string, getenv func(string) string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(args) == 0 || args[0] != "machine-list" {
		return false, 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, usageMachineList())
		return true, 2
	}
	for _, a := range args[2:] {
		if a == "-h" || a == "--help" {
			fmt.Fprintln(stdout, usageMachineList())
			return true, 0
		}
	}
	f, err := parseListFlags(args[2:], "machine", "issuer")
	if err == nil {
		switch args[1] {
		case "keygen":
			err = listKeygen(f, stdout)
		case "create":
			err = listCreate(f, stdout)
		case "sign":
			err = listSign(f, stdout)
		case "verify":
			err = listVerify(f, getenv, stdout)
		case "show":
			err = listShow(f, getenv, stdout)
		case "-h", "--help":
			fmt.Fprintln(stdout, usageMachineList())
			return true, 0
		default:
			fmt.Fprintf(stderr, "muster machine-list: unknown command %q\n%s\n", args[1], usageMachineList())
			return true, 2
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "muster machine-list:", err)
		return true, 1
	}
	return true, 0
}

func listKeygen(f listFlags, stdout io.Writer) error {
	out := f.one["out"]
	if out == "" {
		return errors.New("keygen needs --out=FILE for the private key")
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s already exists: this never replaces a key", out)
	}
	pub, priv, err := peerkey.Generate()
	if err != nil {
		return err
	}
	if err := peerkey.SavePrivate(out, priv); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\n", peerkey.FormatPublicKey(pub))
	return nil
}

func splitBar(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "|") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func listCreate(f listFlags, stdout io.Writer) error {
	signer := f.one["signer"]
	if keyPath := f.one["key"]; keyPath != "" {
		priv, err := peerkey.LoadPrivate(keyPath)
		if err != nil {
			return err
		}
		signer = peerkey.FormatPublicKey(priv.Public().(ed25519.PublicKey))
	}
	if signer == "" {
		return errors.New("create needs the signer: --key=FILE or --signer=PUBKEY")
	}
	seq := uint64(1)
	if s := f.one["seq"]; s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("--seq=%s is not a number", s)
		}
		seq = n
	}
	doc := machinelist.Document{
		Format:     machinelist.Format,
		Fleet:      f.one["fleet"],
		Seq:        seq,
		IssuedAt:   time.Now().UTC().Format(time.RFC3339),
		Signers:    []string{signer},
		NextSigner: f.one["next-signer"],
		Machines:   []machinelist.Machine{},
		Issuers:    []machinelist.Issuer{},
	}
	for _, spec := range f.many["machine"] {
		parts := strings.Split(spec, ",")
		if len(parts) != 4 {
			return fmt.Errorf("--machine=%s: want ID,PUBKEY,ADDR|ADDR,GRANT|GRANT", spec)
		}
		doc.Machines = append(doc.Machines, machinelist.Machine{
			ID: strings.TrimSpace(parts[0]), PublicKey: strings.TrimSpace(parts[1]),
			Addresses: splitBar(parts[2]), Grants: splitBar(parts[3]),
		})
	}
	for _, spec := range f.many["issuer"] {
		id, key, ok := strings.Cut(spec, ",")
		if !ok {
			return fmt.Errorf("--issuer=%s: want ID,PUBKEY", spec)
		}
		doc.Issuers = append(doc.Issuers, machinelist.Issuer{ID: strings.TrimSpace(id), PublicKey: strings.TrimSpace(key)})
	}
	if err := machinelist.Validate(doc); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeOut(f.one["out"], append(raw, '\n'), stdout)
}

func listSign(f listFlags, stdout io.Writer) error {
	if f.one["in"] == "" || f.one["key"] == "" {
		return errors.New("sign needs --in=DRAFT and --key=FILE")
	}
	rawDraft, err := os.ReadFile(f.one["in"])
	if err != nil {
		return err
	}
	var doc machinelist.Document
	dec := json.NewDecoder(strings.NewReader(string(rawDraft)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("%s: %w", f.one["in"], err)
	}
	if s := f.one["seq"]; s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("--seq=%s is not a number", s)
		}
		doc.Seq = n
	}
	priv, err := peerkey.LoadPrivate(f.one["key"])
	if err != nil {
		return err
	}
	signed, err := machinelist.Sign(doc, priv)
	if err != nil {
		return err
	}
	return writeOut(f.one["out"], signed, stdout)
}

func writeOut(path string, data []byte, stdout io.Writer) error {
	if path == "" {
		_, err := stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// pinsFor reads what to verify against: the flags, else the config's block.
func pinsFor(f listFlags, getenv func(string) string) (machinelist.Verifier, bool, error) {
	if f.one["fleet"] != "" || f.one["signer"] != "" {
		if f.one["fleet"] == "" || f.one["signer"] == "" {
			return machinelist.Verifier{}, false, errors.New("--fleet and --signer go together")
		}
		var pins []ed25519.PublicKey
		for _, s := range strings.Split(f.one["signer"], ",") {
			k, err := peerkey.ParsePublicKey(s)
			if err != nil {
				return machinelist.Verifier{}, false, fmt.Errorf("--signer: %w", err)
			}
			pins = append(pins, k)
		}
		v := machinelist.Verifier{Fleet: f.one["fleet"], Signers: pins}
		if s := f.one["min-seq"]; s != "" {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return v, false, fmt.Errorf("--min-seq=%s is not a number", s)
			}
			v.MinSeq = n
		}
		return v, true, nil
	}
	cfgPath := f.one["config"]
	if cfgPath == "" {
		cfgPath = getenv("FLEET_CONFIG")
	}
	if cfgPath == "" {
		return machinelist.Verifier{}, false, nil
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return machinelist.Verifier{}, false, err
	}
	if cfg.MachineList == nil || cfg.MachineList.Signer == "" {
		return machinelist.Verifier{}, false, errors.New("the config has no machineList block with a pinned signer")
	}
	pins, err := cfg.MachineList.pinned()
	if err != nil {
		return machinelist.Verifier{}, false, err
	}
	v := machinelist.Verifier{Fleet: cfg.MachineList.Fleet, Signers: pins}
	if dir := getenv("FLEET_STATE_DIR"); dir != "" {
		if v.MinSeq, err = machinelist.ReadSeq(dir); err != nil {
			return v, false, err
		}
	}
	return v, true, nil
}

func listVerify(f listFlags, getenv func(string) string, stdout io.Writer) error {
	if len(f.pos) != 1 {
		return errors.New("verify takes one file")
	}
	v, ok, err := pinsFor(f, getenv)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("nothing to verify against: pass --fleet and --signer, or --config=PATH with a machineList block")
	}
	raw, err := os.ReadFile(f.pos[0])
	if err != nil {
		return err
	}
	got, err := machinelist.Verify(raw, v)
	if err != nil {
		return err
	}
	if _, err := machinePrincipals(got.Document); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: fleet %q seq %d signed by %s, %d machine(s)\n",
		got.Document.Fleet, got.Document.Seq, peerkey.Fingerprint(got.Signer), len(got.Document.Machines))
	if got.Recovery {
		fmt.Fprintln(stdout, "note: signed by the recovery key; pin it as the signer")
	}
	return nil
}

func listShow(f listFlags, getenv func(string) string, stdout io.Writer) error {
	if len(f.pos) != 1 {
		return errors.New("show takes one file")
	}
	raw, err := os.ReadFile(f.pos[0])
	if err != nil {
		return err
	}
	env, doc, err := machinelist.Decode(raw)
	if err != nil {
		return err
	}
	status := "NOT VERIFIED (nothing to verify against)"
	if v, ok, perr := pinsFor(f, getenv); perr != nil {
		status = "NOT VERIFIED (" + perr.Error() + ")"
	} else if ok {
		if _, verr := machinelist.Verify(raw, v); verr != nil {
			status = "REFUSED: " + verr.Error()
		} else {
			status = "verified"
		}
	}
	fmt.Fprintf(stdout, "signature: %s\n", status)
	fmt.Fprintf(stdout, "fleet:     %s\nseq:       %d\nissued:    %s\n", doc.Fleet, doc.Seq, doc.IssuedAt)
	if k, err := peerkey.ParsePublicKey(env.Signer); err == nil {
		fmt.Fprintf(stdout, "signed by: %s (%s)\n", env.Signer, peerkey.Fingerprint(k))
	}
	fmt.Fprintf(stdout, "signers:   %s\n", strings.Join(doc.Signers, ", "))
	if doc.NextSigner != "" {
		fmt.Fprintf(stdout, "next:      %s\n", doc.NextSigner)
	}
	fmt.Fprintf(stdout, "machines:  %d\n", len(doc.Machines))
	for _, m := range doc.Machines {
		k, _ := peerkey.ParsePublicKey(m.PublicKey)
		fmt.Fprintf(stdout, "  %-20s key %s  grants [%s]\n", m.ID, peerkey.Fingerprint(k), strings.Join(m.Grants, " "))
		for _, a := range m.Addresses {
			fmt.Fprintf(stdout, "  %-20s   %s\n", "", a)
		}
	}
	fmt.Fprintf(stdout, "issuers:   %d\n", len(doc.Issuers))
	for _, is := range doc.Issuers {
		k, _ := peerkey.ParsePublicKey(is.PublicKey)
		fmt.Fprintf(stdout, "  %-20s key %s\n", is.ID, peerkey.Fingerprint(k))
	}
	return nil
}

// listDoctorRow is `muster doctor`'s account of the list.
func listDoctorRow(o listOutcome, base []peerEntry) doctorRow {
	row := doctorRow{ID: "machine-list"}
	switch {
	case o.mode == listModeOff:
		row.Status, row.Summary = statusSkip, "no machine list configured"
	case o.err != nil && o.mode == listModeOnly:
		row.Status, row.Summary = statusFail, "the machine list does not verify — the service refuses to start (mode only)"
		row.Detail = o.err.Error()
	case o.err != nil:
		row.Status, row.Summary = statusWarn, fmt.Sprintf("the machine list does not verify (mode %s)", o.mode)
		row.Detail = o.err.Error()
		if o.mode == listModePrefer {
			row.Detail += "; the hand-written peers are used unchanged"
		}
	default:
		row.Status = statusPass
		if o.verified.Recovery {
			row.Status = statusWarn
		}
		row.Summary = o.describe()
		if o.mode == listModeShadow {
			if diff := o.shadowDiff(base); len(diff) > 0 {
				row.Detail = "shadow changes nothing; prefer/only would change:\n" + strings.Join(diff, "\n")
			} else {
				row.Detail = "shadow changes nothing; the list and the config agree"
			}
		}
	}
	return row
}
