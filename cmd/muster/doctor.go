package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/tmux"
	"github.com/futurelastic/muster/internal/inboxclient"
	"github.com/futurelastic/muster/internal/service"
)

// `muster doctor` — is this installation complete? (muster #160)
//
// # Why this exists
//
// This repository documents several ways an installation can verify clean and
// still not work, and every one of them is silent: a principal table without
// the keys grant (#68) verifies clean and then refuses every keypress; an
// unset FLEET_INBOX_INDEX (#122) leaves every delivery falling through to the
// pane path; an index whose entries carry no permission-mode class (#148)
// reports deliversToInbox while nothing uses the inbox. A machine added to a
// fleet once came up with a supervising principal that lacked one grant its
// peers held, and nothing surfaced it for weeks.
//
// A deploy verifies by asking the running service what it IS. Nothing asked
// whether what it is was complete. This command asks, one row per dependency,
// before the service starts and at any time after.
//
// # What it deliberately is not
//
// Read-only. It never writes config, never creates the state directory (so it
// never calls state.Open, which does), never reads an inbox entry's token
// file, and never mutates anything on a peer — the only network call is the
// same authenticated GET /v1/health a deploy already makes, and --offline
// removes even that.
//
// The one thing it runs besides reading files is `git rev-parse --git-path`,
// for the hooks.pre-commit row (#201) — a question about the CLONE a person
// commits in rather than about the installation, answered only when doctor is
// run from inside one, and read-only like everything else here.
//
// Paste-safe. It never prints a token value, a filesystem path, or a peer's
// address — rows name the VARIABLE instead (FLEET_STATE_DIR, not its value),
// the same discipline main.go's own startup logging follows. Machine ids and
// principal names are printed; `principal list` already prints names.
//
// It reads the environment of the shell that runs it. A service manager hands
// the service a different, usually barer, environment — run doctor under the
// unit's environment, not your login shell's, or it will answer for a service
// that does not exist (docs/install.md).
//
// # Row ids are a contract
//
// Scripts gate on them (`--json | jq`), and --skip names them. Once #154 makes
// a peer's grants readable, peer.<m>.grants changes its answer, not its id.

type rowStatus string

const (
	statusPass    rowStatus = "pass"
	statusWarn    rowStatus = "warn"
	statusFail    rowStatus = "fail"
	statusUnknown rowStatus = "unknown"
	statusSkip    rowStatus = "skip"
)

var statusOrder = []rowStatus{statusPass, statusWarn, statusFail, statusUnknown, statusSkip}

type doctorRow struct {
	ID      string    `json:"id"`
	Status  rowStatus `json:"status"`
	Summary string    `json:"summary"`
	Detail  string    `json:"detail,omitempty"`
	Refs    []int     `json:"refs,omitempty"`
}

// doctorEnv is everything runChecks reads that is not the filesystem. Getenv
// and HTTP are injected so a test never touches the real environment or the
// network.
type doctorEnv struct {
	Getenv    func(string) string
	HTTP      *http.Client
	Offline   bool
	Principal string
	Skip      map[string]bool
	Timeout   time.Duration
	// LookPath resolves the multiplexer when FLEET_TMUX_BIN is unset, and git
	// and gitleaks for the hooks.pre-commit row.
	LookPath func(string) (string, error)
	// Dir is where the command was run from. Only hooks.pre-commit (#201)
	// reads it: that row is about the clone a person commits in, not about the
	// installation, so it needs somewhere to look. Empty means "not
	// applicable" — which is what keeps a test independent of whichever clone
	// it happens to run in.
	Dir string
}

// W_OK for access(2). Spelled out because the syscall package does not export
// it on every platform this builds for.
const accessWriteOK = 0x2

func usageDoctor() string {
	return strings.Join([]string{
		"usage: muster doctor [--json] [--offline] [--principal=NAME] [--skip=ID[,ID]] [--timeout=3s]",
		"",
		"Read-only. One row per dependency of a working installation; exits 1 when any",
		"row fails, 0 otherwise (warn, unknown and skip never change the exit code), 2 on",
		"a usage error. Run it under the service unit's environment, not a login shell's.",
		"",
		"  --principal=NAME  the supervising client's principal; its grants are checked",
		"                    against every verb a supervisor performs",
		"  --offline         do not probe peers",
		"  --skip=ID         report a row as skipped (e.g. inbox.index on a machine that",
		"                    deliberately has no inbox)",
		"",
		"hooks.pre-commit is the one row about a source clone rather than the installation:",
		"it is answered only when run from inside a clone of this repository, and is",
		"skipped anywhere else.",
	}, "\n")
}

// runDoctor handles `muster doctor ...` and reports whether it consumed
// the invocation, and the exit code to use when it did.
func runDoctor(args []string, getenv func(string) string, stdout, stderr io.Writer) (handled bool, code int) {
	dir, _ := os.Getwd() // an unreadable working directory leaves it empty: not applicable
	return runDoctorAt(dir, args, getenv, stdout, stderr)
}

// runDoctorAt is runDoctor with the working directory named, so a test can
// point the clone row at a repository it built, or at nothing.
func runDoctorAt(dir string, args []string, getenv func(string) string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(args) == 0 || args[0] != "doctor" {
		return false, 0
	}
	env := doctorEnv{
		Getenv:   getenv,
		Skip:     map[string]bool{},
		Timeout:  3 * time.Second,
		LookPath: exec.LookPath,
		Dir:      dir,
	}
	asJSON := false
	for _, a := range args[1:] {
		switch {
		case a == "--json":
			asJSON = true
		case a == "--offline":
			env.Offline = true
		case strings.HasPrefix(a, "--principal="):
			env.Principal = strings.TrimPrefix(a, "--principal=")
		case strings.HasPrefix(a, "--skip="):
			for _, id := range splitList(strings.TrimPrefix(a, "--skip=")) {
				env.Skip[id] = true
			}
		case strings.HasPrefix(a, "--timeout="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--timeout="))
			if err != nil || d <= 0 {
				fmt.Fprintf(stderr, "bad --timeout %q\n%s\n", strings.TrimPrefix(a, "--timeout="), usageDoctor())
				return true, 2
			}
			env.Timeout = d
		case a == "-h" || a == "--help":
			fmt.Fprintln(stdout, usageDoctor())
			return true, 0
		default:
			fmt.Fprintf(stderr, "unknown argument %q\n%s\n", a, usageDoctor())
			return true, 2
		}
	}
	env.HTTP = &http.Client{Timeout: env.Timeout}

	rows := runChecks(context.Background(), env)

	// A --skip that names no row is a typo, and a typo that silently does
	// nothing is the exact failure class this command exists to name.
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.ID] = true
	}
	var unmatched []string
	for id := range env.Skip {
		if !seen[id] {
			unmatched = append(unmatched, id)
		}
	}
	if len(unmatched) > 0 {
		sort.Strings(unmatched)
		fmt.Fprintf(stderr, "--skip names no row: %s\n", strings.Join(unmatched, ", "))
		return true, 2
	}

	self := getenv("FLEET_MACHINE")
	if self == "" {
		self = "local"
	}
	if asJSON {
		if err := writeDoctorJSON(stdout, self, rows); err != nil {
			fmt.Fprintln(stderr, err)
			return true, 2
		}
	} else {
		writeDoctorText(stdout, self, rows)
	}
	for _, r := range rows {
		if r.Status == statusFail {
			return true, 1
		}
	}
	return true, 0
}

// runChecks evaluates every row. Pure over env, the filesystem and env.HTTP:
// no globals, no process exit, no writes.
func runChecks(ctx context.Context, env doctorEnv) []doctorRow {
	c := &checker{env: env}
	getenv := env.Getenv

	// --- config ---------------------------------------------------------
	cfgPath := getenv("FLEET_CONFIG")
	var cfg *fileConfig
	var principals []service.Principal
	cfgFailed := false
	switch {
	case cfgPath == "":
		c.add(doctorRow{ID: "config.load", Status: statusWarn,
			Summary: "FLEET_CONFIG unset — single-token mode",
			Detail: "one shared secret, no per-verb grants and no per-peer credentials; " +
				"reasonable for one machine, unstatable for a fleet",
			Refs: []int{95}})
	default:
		loaded, err := loadConfig(cfgPath)
		if err == nil {
			principals, err = loaded.principals()
		}
		if err == nil {
			err = validateConfigExtras(loaded, getenv)
		}
		if err != nil {
			cfgFailed = true
			c.add(doctorRow{ID: "config.load", Status: statusFail,
				Summary: "FLEET_CONFIG does not load — the service refuses to start",
				Detail:  redact(err.Error(), cfgPath, "FLEET_CONFIG"),
				Refs:    []int{95}})
		} else {
			cfg = loaded
			c.add(doctorRow{ID: "config.load", Status: statusPass,
				Summary: fmt.Sprintf("principal table loaded: %d principal(s)", len(principals)),
				Refs:    []int{95}})
		}
	}

	// --- machine id and peers ---------------------------------------------
	self := fleet.MachineId(getenv("FLEET_MACHINE"))
	peers, peerErr := doctorPeers(getenv, cfg)
	switch {
	case self == "" && len(peers) > 0:
		c.add(doctorRow{ID: "machine.id", Status: statusFail,
			Summary: "FLEET_MACHINE unset while peers are configured",
			Detail:  `the id defaults to "local", which no peer names in its own FLEET_PEERS`})
	case self == "":
		c.add(doctorRow{ID: "machine.id", Status: statusWarn,
			Summary: `FLEET_MACHINE unset — defaults to "local", useful only for a single-machine trial`})
	default:
		c.add(doctorRow{ID: "machine.id", Status: statusPass,
			Summary: fmt.Sprintf("machine id %q", self)})
	}
	if self == "" {
		self = "local"
	}

	// --- token ------------------------------------------------------------
	token := getenv("FLEET_TOKEN")
	switch {
	case cfgFailed:
		c.add(doctorRow{ID: "token.source", Status: statusSkip,
			Summary: "undecidable until config.load passes"})
	case cfg != nil:
		c.add(doctorRow{ID: "token.source", Status: statusPass,
			Summary: "principal table authenticates callers; FLEET_TOKEN not required",
			Refs:    []int{95}})
	case token != "":
		c.add(doctorRow{ID: "token.source", Status: statusPass,
			Summary: "FLEET_TOKEN set (single-token mode)"})
	default:
		c.add(doctorRow{ID: "token.source", Status: statusFail,
			Summary: "no FLEET_TOKEN and no principal table — the service refuses to start",
			Detail:  "there is no unauthenticated mode (api-http.md §5)"})
	}

	// --- bind address -------------------------------------------------------
	c.add(checkBind(getenv("FLEET_ADDR")))

	// --- runtime ------------------------------------------------------------
	runtime := getenv("FLEET_RUNTIME")
	if runtime == "" {
		runtime = "tmux"
	}
	c.add(checkRuntime(runtime, getenv("FLEET_TMUX_BIN"), env.LookPath))

	// --- grants -------------------------------------------------------------
	supervisorSet := supervisorGrants()
	allowMut := getenv("FLEET_ALLOW_MUTATIONS") == "1"
	allowRelay := getenv("FLEET_ALLOW_RELAY") == "1"
	switch {
	case cfgFailed:
		c.add(doctorRow{ID: "principals.supervisor", Status: statusSkip, Summary: "undecidable until config.load passes"})
		c.add(doctorRow{ID: "principals.relay", Status: statusSkip, Summary: "undecidable until config.load passes"})
		c.add(doctorRow{ID: "local.mutations", Status: statusSkip, Summary: "undecidable until config.load passes"})
	case cfg == nil:
		c.add(doctorRow{ID: "principals.supervisor", Status: statusSkip,
			Summary: "single-token mode has no per-verb grants — see local.mutations"})
		switch {
		case len(peers) == 0:
			c.add(doctorRow{ID: "principals.relay", Status: statusSkip, Summary: "no peers configured"})
		case allowRelay:
			c.add(doctorRow{ID: "principals.relay", Status: statusPass,
				Summary: "FLEET_ALLOW_RELAY=1 — writes may be forwarded to peers"})
		default:
			c.add(doctorRow{ID: "principals.relay", Status: statusFail,
				Summary: "peers configured but FLEET_ALLOW_RELAY is not 1 — every write to a peer is refused here",
				Refs:    []int{68}})
		}
		if allowMut {
			c.add(doctorRow{ID: "local.mutations", Status: statusPass,
				Summary: "FLEET_ALLOW_MUTATIONS=1 — sessions on this machine may be driven"})
		} else {
			c.add(doctorRow{ID: "local.mutations", Status: statusWarn,
				Summary: "FLEET_ALLOW_MUTATIONS is not 1 — every create/input/interrupt/close/keys here is refused",
				Detail:  "right for a hardened read-only host (D6); wrong for a machine whose sessions are meant to be driven"})
		}
	default:
		c.add(checkSupervisor(principals, supervisorSet, env.Principal))
		c.add(checkRelay(principals, env.Principal, len(peers)))
		if allowMut || allowRelay {
			c.add(doctorRow{ID: "local.mutations", Status: statusWarn,
				Summary: "FLEET_ALLOW_MUTATIONS / FLEET_ALLOW_RELAY are set but ignored — the principal table decides",
				Detail:  "remove them from the environment so nobody reads them as being in force"})
		} else {
			c.add(doctorRow{ID: "local.mutations", Status: statusSkip,
				Summary: "principal table mode — per-principal grants decide"})
		}
	}

	// --- who relays a person's messages (#189) --------------------------------
	// Its own row, after the grants block, because it reads two things that
	// block does not: the inbox index and the runtime. --principal deliberately
	// does not narrow it (see checkHumanRelay).
	c.add(checkHumanRelay(runtime, getenv("FLEET_INBOX_INDEX"), cfgFailed, cfg != nil, principals))

	// --- this machine's own credential for peer reads (#98) -------------------
	switch {
	case len(peers) == 0:
		c.add(doctorRow{ID: "peer.self-credential", Status: statusSkip, Summary: "no peers configured"})
	case token != "":
		c.add(doctorRow{ID: "peer.self-credential", Status: statusPass,
			Summary: "FLEET_TOKEN is presented for this machine's own peer subscriptions"})
	case cfgFailed:
		c.add(doctorRow{ID: "peer.self-credential", Status: statusSkip, Summary: "undecidable until config.load passes"})
	case cfg != nil:
		if _, ok := cfg.selfCredential(self); ok {
			c.add(doctorRow{ID: "peer.self-credential", Status: statusPass,
				Summary: fmt.Sprintf("principal %q exists — this machine has a credential to offer peers", "system:"+string(self)),
				Detail: "two-sided: the SAME token must be held on each peer by a principal with at least read; " +
					"that half is not visible from here",
				Refs: []int{98}})
		} else {
			c.add(doctorRow{ID: "peer.self-credential", Status: statusFail,
				Summary: fmt.Sprintf("no FLEET_TOKEN and no principal %q — every peer subscription is refused", "system:"+string(self)),
				Refs:    []int{98}})
		}
	}

	// --- state directory ----------------------------------------------------
	c.add(checkStateDir(getenv("FLEET_STATE_DIR")))

	// --- inbox ----------------------------------------------------------------
	indexDir := getenv("FLEET_INBOX_INDEX")
	indexRow := checkInboxIndex(runtime, indexDir)
	c.add(indexRow)
	if c.last().Status != statusPass {
		c.add(doctorRow{ID: "inbox.mode-class", Status: statusSkip,
			Summary: "undecidable until inbox.index passes", Refs: []int{148}})
	} else {
		c.add(checkModeClass(indexDir))
	}

	// --- delivery modules (#185) -----------------------------------------------
	c.add(checkDeliveryModules(getenv))

	// --- peers ----------------------------------------------------------------
	if peerErr != nil {
		c.add(doctorRow{ID: "peers.config", Status: statusFail,
			Summary: "peer list does not parse — the service refuses to start",
			Detail:  peerErr.Error()})
	}
	for _, p := range peers {
		prefix := "peer." + string(p.machine)
		if p.machine == self {
			c.add(doctorRow{ID: prefix + ".url", Status: statusFail,
				Summary: "this peer is this machine — the service refuses to start",
				Detail:  "fan-out is one hop and peers never recurse (§13.1)"})
			continue
		}
		u, err := url.Parse(p.base)
		urlOK := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
		if urlOK {
			c.add(doctorRow{ID: prefix + ".url", Status: statusPass, Summary: "address parses as " + u.Scheme})
		} else {
			c.add(doctorRow{ID: prefix + ".url", Status: statusFail,
				Summary: "address is not an http(s) URL with a host"})
		}

		credential := p.token
		switch {
		case p.token != "":
			c.add(doctorRow{ID: prefix + ".credential", Status: statusPass,
				Summary: "per-peer credential configured"})
		case token != "":
			credential = token
			c.add(doctorRow{ID: prefix + ".credential", Status: statusWarn,
				Summary: "no per-peer credential — the caller's credential is forwarded (shared-token mode)",
				Detail:  "works only while every machine accepts the same secret"})
		default:
			c.add(doctorRow{ID: prefix + ".credential", Status: statusFail,
				Summary: "nothing to present to this peer — every relayed call is refused there",
				Refs:    []int{98}})
		}

		switch {
		case env.Offline:
			c.add(doctorRow{ID: prefix + ".reachable", Status: statusUnknown, Summary: "not probed (--offline)"})
		case !urlOK || credential == "":
			c.add(doctorRow{ID: prefix + ".reachable", Status: statusUnknown,
				Summary: "not probed — no usable address or credential"})
		default:
			if c.skipped(prefix + ".reachable") {
				c.add(doctorRow{ID: prefix + ".reachable"})
			} else {
				c.add(probePeer(ctx, env, prefix+".reachable", strings.TrimRight(p.base, "/"), credential))
			}
		}

		// This machine's standing on the peer, as the peer reports it
		// (muster #154). The row id is the one it has always had.
		switch {
		case env.Offline:
			c.add(doctorRow{ID: prefix + ".grants", Status: statusUnknown,
				Summary: "not probed (--offline)", Refs: []int{154}})
		case !urlOK || credential == "":
			c.add(doctorRow{ID: prefix + ".grants", Status: statusUnknown,
				Summary: "not probed — no usable address or credential", Refs: []int{154}})
		case c.skipped(prefix + ".grants"):
			c.add(doctorRow{ID: prefix + ".grants"})
		default:
			c.add(probePeerGrants(ctx, env, prefix+".grants", strings.TrimRight(p.base, "/"), credential, self, p.machine))
		}
	}

	// --- the clone (#201) ---------------------------------------------------------
	// Last, because it is the one row that is not about the installation.
	if c.skipped("hooks.pre-commit") {
		c.add(doctorRow{ID: "hooks.pre-commit"})
	} else {
		c.add(checkPreCommitHook(ctx, env))
	}

	return c.rows
}

type checker struct {
	env  doctorEnv
	rows []doctorRow
}

func (c *checker) skipped(id string) bool { return c.env.Skip[id] }

func (c *checker) add(r doctorRow) {
	if c.skipped(r.ID) {
		r = doctorRow{ID: r.ID, Status: statusSkip, Summary: "skipped (--skip)", Refs: r.Refs}
	}
	c.rows = append(c.rows, r)
}

func (c *checker) last() doctorRow { return c.rows[len(c.rows)-1] }

// validateConfigExtras repeats the startup checks main.go makes on the rest of
// the file — each of them a log.Fatalf there, so each a fail here.
func validateConfigExtras(cfg *fileConfig, getenv func(string) string) error {
	roots := splitList(getenv("FLEET_TRUST_ROOTS"))
	if len(roots) == 0 {
		roots = cfg.TrustRoots
	}
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			return errors.New("a trust root is not an absolute path")
		}
	}
	if len(cfg.SessionEnv) > 0 {
		entries := cfg.sessionEnv()
		if err := tmux.ValidateSessionEnv(entries); err != nil {
			msg := err.Error()
			for _, e := range entries {
				msg = redact(msg, e.FromFile, "<fromFile>")
			}
			return errors.New(msg)
		}
	}
	return nil
}

type doctorPeer struct {
	machine fleet.MachineId
	base    string
	token   string
}

// doctorPeers resolves the peer list exactly as main.go does: FLEET_PEERS
// wins, the config file's peers otherwise; the per-peer credential always
// comes from the config file.
func doctorPeers(getenv func(string) string, cfg *fileConfig) ([]doctorPeer, error) {
	specs := splitList(getenv("FLEET_PEERS"))
	if cfg != nil && len(specs) == 0 {
		for _, p := range cfg.Peers {
			specs = append(specs, p.Machine+"="+p.URL)
		}
	}
	var out []doctorPeer
	for i, spec := range specs {
		name, base, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(base) == "" {
			return out, fmt.Errorf("peer entry %d is not name=url", i+1)
		}
		p := doctorPeer{machine: fleet.MachineId(strings.TrimSpace(name)), base: strings.TrimSpace(base)}
		if cfg != nil {
			if _, tok, ok := cfg.peerFor(p.machine); ok {
				p.token = tok
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// supervisorGrants is every verb a supervising client performs on a machine's
// own sessions: every grant the service defines except relay, which only
// matters once peers exist and is checked on its own row. Derived rather than
// listed, so a grant added later joins the set without anyone remembering to.
func supervisorGrants() []service.Grant {
	var out []service.Grant
	for _, g := range service.Grants() {
		// label (muster #153) is left out for the same kind of reason
		// relay is: binding a session to its work after the fact is something
		// a supervisor MAY do, not a verb it needs to drive sessions at all.
		// Counting it would turn every existing full supervisor into a warning
		// the day this build lands.
		//
		// remote-control (muster #269) is left out the same way: publishing a
		// session to off-machine clients is something a supervisor MAY be
		// allowed, not a verb it needs to drive sessions, and requiring it would
		// turn every existing full supervisor into a warning the day this lands.
		//
		// human-relay (round-3 #180 review fix) is excluded the same way:
		// it names a caller's own claim to BE a human-facing relay for
		// route:"terminal" labelling, not a verb that drives sessions at
		// all — a principal can fully create/send/interrupt/close/rename/
		// discard/keys a session without ever being the one relay this
		// grant exists for, and requiring it here would turn every existing
		// full supervisor into a warning the day this build lands, same as
		// label/relay would.
		if g != service.GrantRelay && g != service.GrantLabel && g != service.GrantRemoteControl && g != service.GrantHumanRelay {
			out = append(out, g)
		}
	}
	return out
}

func missingGrants(p service.Principal, want []service.Grant) []string {
	var out []string
	for _, g := range want {
		if !p.Allows(g) {
			out = append(out, string(g))
		}
	}
	return out
}

func checkSupervisor(principals []service.Principal, want []service.Grant, named string) doctorRow {
	row := doctorRow{ID: "principals.supervisor", Refs: []int{68}}
	if named != "" {
		for _, p := range principals {
			if p.Name != named {
				continue
			}
			if miss := missingGrants(p, want); len(miss) > 0 {
				row.Status = statusFail
				row.Summary = fmt.Sprintf("principal %q lacks: %s", named, strings.Join(miss, ", "))
				row.Detail = "a supervising client meets each missing grant as a 401 on first use, never at install"
				return row
			}
			row.Status = statusPass
			row.Summary = fmt.Sprintf("principal %q holds every supervisor grant", named)
			return row
		}
		row.Status = statusFail
		row.Summary = fmt.Sprintf("no principal named %q", named)
		return row
	}

	var full, nearMiss []string
	for _, p := range principals {
		miss := missingGrants(p, want)
		switch len(miss) {
		case 0:
			full = append(full, p.Name)
		case 1:
			nearMiss = append(nearMiss, fmt.Sprintf("%q lacks %s", p.Name, miss[0]))
		}
	}
	switch {
	case len(full) == 0:
		row.Status = statusFail
		row.Summary = "no principal holds every supervisor grant — nobody can fully drive sessions here"
		row.Detail = "supervisor grants: " + joinGrants(want)
		if len(nearMiss) > 0 {
			row.Detail += "; closest: " + strings.Join(nearMiss, "; ")
		}
	case len(nearMiss) > 0:
		// One grant short of a full supervisor is the shape of drift, not of a
		// deliberately limited client — which usually lacks several.
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%d principal(s) are one grant short of a supervisor: %s",
			len(nearMiss), strings.Join(nearMiss, "; "))
		row.Detail = "full supervisors: " + quoteAll(full) + ". Pass --principal=NAME to check one by name"
	default:
		row.Status = statusPass
		row.Summary = "full supervisor grants held by " + quoteAll(full)
		row.Detail = "pass --principal=NAME to check the one your supervising client actually uses"
	}
	return row
}

func checkRelay(principals []service.Principal, named string, peerCount int) doctorRow {
	row := doctorRow{ID: "principals.relay", Refs: []int{68}}
	if peerCount == 0 {
		row.Status = statusSkip
		row.Summary = "no peers configured"
		return row
	}
	var holders []string
	for _, p := range principals {
		if named != "" && p.Name != named {
			continue
		}
		if p.Allows(service.GrantRelay) {
			holders = append(holders, p.Name)
		}
	}
	switch {
	case len(holders) > 0:
		row.Status = statusPass
		row.Summary = "relay held by " + quoteAll(holders)
		row.Detail = "the near half only: the verb itself (e.g. keys) must also be granted on the peer, to this machine's credential"
	case named != "":
		row.Status = statusFail
		row.Summary = fmt.Sprintf("peers configured but principal %q lacks relay — its writes to a peer are refused here", named)
	default:
		row.Status = statusFail
		row.Summary = "peers configured but no principal holds relay — every write to a peer is refused here"
	}
	return row
}

// checkHumanRelay names the configuration gap #184 leaves behind: an inbox
// index is configured, a principal table is configured, and nobody in the table
// holds the human-relay grant. Under `route: auto` every send from a principal
// WITHOUT the grant goes through the session's inbox whenever the session can
// take it, so once an index emits a permission-mode class a person's relayed
// message arrives as a peer message — which the receiving runtime treats as not
// coming from the user and which cannot grant escalation. That is the failure
// #184 was filed for, reproduced by a missing grant instead of a code path.
//
// With a table, a warning and never a failure (ADR 160): nothing is broken until
// an index also emits a class, and a fleet whose callers are all agents has no person to
// relay and is right to hold no such principal — --skip=principals.human-relay
// says so.
//
// --principal does NOT narrow this row, unlike principals.relay. The
// supervising client that flag names is the caller that drives sessions;
// human-relay is deliberately outside the supervisor set (supervisorGrants) and
// is held by whichever principal relays a PERSON's messages, usually another.
// "Does anyone hold it" is the question the row can answer; "is the holder the
// right one" is not, and a pass says so.
//
// Single-token mode is the other half (#196, ruled on #195). It has no
// per-caller identity, so there is no grant to hold — and the only way left to
// say "this is a person" is a header any bearer of the token can set. So the
// inbox route is not available there at all: the service refuses to start with
// FLEET_INBOX_INDEX set and no principal table (requireTableForInbox), and this
// row FAILS for that shape, under ADR 160's meaning of fail — "the service will
// refuse to start". A single-token machine with no index is unaffected and
// skips above: nothing is diverted into a peer message when there is no inbox.
func checkHumanRelay(runtime, indexDir string, cfgFailed, tableMode bool, principals []service.Principal) doctorRow {
	row := doctorRow{ID: "principals.human-relay", Refs: []int{184, 189, 195, 196}}
	switch {
	case cfgFailed:
		row.Status = statusSkip
		row.Summary = "undecidable until config.load passes"
		return row
	case runtime != "tmux":
		row.Status = statusSkip
		row.Summary = fmt.Sprintf("FLEET_RUNTIME=%s has no inbox delivery path", runtime)
		return row
	case indexDir == "":
		row.Status = statusSkip
		row.Summary = "FLEET_INBOX_INDEX unset — no delivery takes the inbox route, so who relays a person's messages does not matter here"
		return row
	case requireTableForInbox(indexDir, tableMode) != nil:
		row.Status = statusFail
		row.Summary = inboxNeedsTableSummary + " — the service refuses to start"
		row.Detail = inboxNeedsTableRemedy + ". Single-token mode has no per-caller identity, so there is no " +
			"human-relay grant to hold there; --skip=principals.human-relay hides this row but the service still refuses"
		return row
	}

	var holders []string
	for _, p := range principals {
		if p.Allows(service.GrantHumanRelay) {
			holders = append(holders, p.Name)
		}
	}
	if len(holders) > 0 {
		row.Status = statusPass
		row.Summary = "human-relay held by " + quoteAll(holders)
		row.Detail = "this row cannot tell which caller relays a person's messages — check the holder is that one; " +
			"--principal does not narrow it"
		return row
	}
	row.Status = statusWarn
	row.Summary = "FLEET_INBOX_INDEX is set but no principal holds human-relay — a person's relayed message will arrive as a peer message once an index emits a class"
	row.Detail = "the receiving runtime treats a peer message as not coming from the user, and it cannot grant escalation. " +
		"Grant human-relay to the principal that relays a person's messages BEFORE the index writer emits mode_class " +
		"(docs/deploy.md, \"Turning the inbox route on\"). Nothing is broken until a class is emitted; " +
		"--skip=principals.human-relay silences it where every caller is an agent"
	return row
}

func checkBind(raw string) doctorRow {
	row := doctorRow{ID: "bind.addr"}
	addrs := splitList(raw)
	if len(addrs) == 0 {
		row.Status = statusWarn
		row.Summary = "FLEET_ADDR unset — loopback on an ephemeral port"
		row.Detail = "no stable health URL for a deploy to verify, and no peer can reach it; pick a port and keep it"
		return row
	}
	status := statusPass
	var notes []string
	routable := false
	for i, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			row.Status = statusFail
			row.Summary = fmt.Sprintf("FLEET_ADDR entry %d is not host:port — the service refuses to start", i+1)
			return row
		}
		switch {
		case port == "0" || port == "":
			status = worse(status, statusWarn)
			notes = append(notes, fmt.Sprintf("entry %d uses an ephemeral port", i+1))
		}
		switch host {
		case "", "0.0.0.0", "::":
			status = worse(status, statusWarn)
			notes = append(notes, fmt.Sprintf("entry %d binds every interface — bind a specific one (§6.1)", i+1))
			routable = true
		case "127.0.0.1", "localhost", "::1":
		default:
			routable = true
		}
	}
	row.Status = status
	if routable {
		row.Summary = fmt.Sprintf("%d address(es), routable (loopback is added automatically)", len(addrs))
	} else {
		row.Summary = fmt.Sprintf("%d address(es), loopback-only — no peer can reach this machine", len(addrs))
	}
	row.Detail = strings.Join(notes, "; ")
	return row
}

func checkRuntime(runtime, tmuxBin string, lookPath func(string) (string, error)) doctorRow {
	row := doctorRow{ID: "runtime"}
	switch runtime {
	case "stub":
		row.Status = statusPass
		row.Summary = "FLEET_RUNTIME=stub"
	case "tmux":
		if lookPath == nil {
			lookPath = exec.LookPath
		}
		// A bare name is resolved on PATH by the service itself (exec does
		// that), so it has to be resolved the same way here — and it carries
		// the same bare-PATH trap as leaving the variable unset.
		if tmuxBin != "" && !strings.ContainsRune(tmuxBin, filepath.Separator) {
			if _, err := lookPath(tmuxBin); err != nil {
				row.Status = statusFail
				row.Summary = "FLEET_TMUX_BIN is a bare name not found on PATH"
				return row
			}
			row.Status = statusWarn
			row.Summary = "FLEET_TMUX_BIN is a bare name, found on this shell's PATH"
			row.Detail = "a service manager usually starts the service with a bare PATH that lacks it — use an absolute path"
			return row
		}
		if tmuxBin != "" {
			info, err := os.Stat(tmuxBin)
			if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				row.Status = statusFail
				row.Summary = "FLEET_TMUX_BIN does not name an executable file"
				return row
			}
			row.Status = statusPass
			row.Summary = "tmux runtime; FLEET_TMUX_BIN names an executable"
			return row
		}
		if _, err := lookPath("tmux"); err != nil {
			row.Status = statusFail
			row.Summary = "tmux runtime, but no tmux on PATH and FLEET_TMUX_BIN unset"
			return row
		}
		row.Status = statusWarn
		row.Summary = "tmux found on this shell's PATH; FLEET_TMUX_BIN unset"
		row.Detail = "a service manager usually starts the service with a bare PATH that lacks it — set FLEET_TMUX_BIN in the unit"
	default:
		row.Status = statusFail
		row.Summary = fmt.Sprintf("unknown FLEET_RUNTIME %q (want tmux or stub) — the service refuses to start", runtime)
	}
	return row
}

func checkStateDir(dir string) doctorRow {
	row := doctorRow{ID: "state.dir"}
	if dir == "" {
		row.Status = statusWarn
		row.Summary = "FLEET_STATE_DIR unset — in-memory only"
		row.Detail = "idempotency keys and the event sequence are lost on every restart (D5)"
		return row
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && !info.IsDir():
		row.Status = statusFail
		row.Summary = "FLEET_STATE_DIR names something that is not a directory"
	case err == nil:
		if syscall.Access(dir, accessWriteOK) != nil {
			row.Status = statusFail
			row.Summary = "FLEET_STATE_DIR is not writable by this user"
			return row
		}
		if info.Mode().Perm()&0o077 != 0 {
			row.Status = statusWarn
			row.Summary = fmt.Sprintf("FLEET_STATE_DIR is writable but mode %#o is looser than 0700", info.Mode().Perm())
			row.Detail = "it holds idempotency keys naming working directories and records of what runs here"
			return row
		}
		row.Status = statusPass
		row.Summary = "FLEET_STATE_DIR is a writable directory, mode 0700"
	case errors.Is(err, os.ErrNotExist):
		parent := filepath.Dir(filepath.Clean(dir))
		for {
			pi, perr := os.Stat(parent)
			if perr == nil {
				if pi.IsDir() && syscall.Access(parent, accessWriteOK) == nil {
					row.Status = statusPass
					row.Summary = "FLEET_STATE_DIR does not exist yet; the service creates it (mode 0700) on start"
				} else {
					row.Status = statusFail
					row.Summary = "FLEET_STATE_DIR does not exist and cannot be created by this user"
				}
				return row
			}
			next := filepath.Dir(parent)
			if next == parent {
				row.Status = statusFail
				row.Summary = "FLEET_STATE_DIR does not exist and has no existing ancestor"
				return row
			}
			parent = next
		}
	default:
		row.Status = statusFail
		row.Summary = "FLEET_STATE_DIR cannot be inspected by this user"
	}
	return row
}

func checkInboxIndex(runtime, dir string) doctorRow {
	row := doctorRow{ID: "inbox.index", Refs: []int{122}}
	if runtime != "tmux" {
		row.Status = statusSkip
		row.Summary = fmt.Sprintf("FLEET_RUNTIME=%s has no inbox delivery path", runtime)
		return row
	}
	if dir == "" {
		row.Status = statusWarn
		row.Summary = "FLEET_INBOX_INDEX unset — every delivery uses the pane path"
		row.Detail = "right for a machine with no index writer; on one you expect to deliver to an inbox, " +
			"this is #122. --skip=inbox.index silences it where it is deliberate"
		return row
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		row.Status = statusFail
		row.Summary = "FLEET_INBOX_INDEX is set but does not name a directory"
		return row
	}
	f, err := os.Open(dir)
	if err != nil {
		row.Status = statusFail
		row.Summary = "FLEET_INBOX_INDEX is not readable by this user"
		return row
	}
	f.Close()
	row.Status = statusPass
	row.Summary = "FLEET_INBOX_INDEX names a readable directory"
	return row
}

// checkModeClass is the one place anything in this repository lists the inbox
// index directory. The resolver deliberately never does (inboxresolver.go);
// a diagnostic that reports "how many entries are attestable" has to. It
// parses each entry and reads no further — token_path is never followed.
func checkModeClass(dir string) doctorRow {
	row := doctorRow{ID: "inbox.mode-class", Refs: []int{148}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		row.Status = statusFail
		row.Summary = "FLEET_INBOX_INDEX cannot be listed"
		return row
	}
	var total, valid, missing, bad, torn int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		total++
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		var entry inboxIndexEntry
		if err != nil || json.Unmarshal(data, &entry) != nil {
			// The writer rewrites these on every process transition, so a
			// torn read is traffic, not a broken index (#146).
			torn++
			continue
		}
		switch {
		case entry.ModeClass == "":
			missing++
		case inboxclient.ModeClass(entry.ModeClass).Valid():
			valid++
		default:
			bad++
		}
	}
	counts := fmt.Sprintf("%d entr(ies): %d attestable, %d without mode_class, %d unrecognised, %d unreadable",
		total, valid, missing, bad, torn)
	switch {
	case total == 0:
		row.Status = statusUnknown
		row.Summary = "index is empty — nothing to inspect until a session with an inbox runs"
	case bad > 0:
		row.Status = statusFail
		row.Summary = counts
		row.Detail = fmt.Sprintf("an unrecognised class is refused, never guessed at (want %q or %q)",
			inboxclient.ModeBypass, inboxclient.ModePrompting)
	case missing > 0:
		row.Status = statusWarn
		row.Summary = counts
		row.Detail = "an entry without mode_class cannot be attested, so its sends use the pane path while " +
			"deliversToInbox still reads true — the intended day-one state until the writer emits the field"
	case torn > 0:
		row.Status = statusWarn
		row.Summary = counts
		row.Detail = "an unreadable entry is usually a torn read of one being rewritten; re-run to confirm"
	default:
		row.Status = statusPass
		row.Summary = counts
	}
	return row
}

// probePeer makes the same authenticated GET /v1/health a deploy verifies
// with. It is routed through the peer's withAuth and reading() gates, so a 200
// proves more than reachability: the credential this machine presents is
// known to that peer and holds read.
func probePeer(ctx context.Context, env doctorEnv, id, base, credential string) doctorRow {
	row := doctorRow{ID: id, Refs: []int{98}}
	ctx, cancel := context.WithTimeout(ctx, env.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/health", nil)
	if err != nil {
		row.Status = statusFail
		row.Summary = "request could not be built from the configured address"
		return row
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	client := env.HTTP
	if client == nil {
		client = &http.Client{Timeout: env.Timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		row.Status = statusFail
		row.Summary = "unreachable: " + describeNetErr(err)
		row.Detail = "the bind address or the network path is wrong — not the credential: a bad credential is a 401, not a silence"
		return row
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		row.Status = statusFail
		row.Summary = fmt.Sprintf("reachable, but the credential is refused (%d)", resp.StatusCode)
		row.Detail = "the peer holds no principal with this token, or that principal lacks read (#98: token VALUE must match on both sides)"
		return row
	default:
		row.Status = statusFail
		row.Summary = fmt.Sprintf("reachable, but health answered %d", resp.StatusCode)
		return row
	}
	var body struct {
		Build fleet.Build `json:"build"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	row.Status = statusPass
	row.Summary = "reachable; credential accepted with read"
	if why := fleet.SelfBuild().DifferenceFrom(body.Build); why != "" {
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("reachable; credential accepted — but peer build %s vs ours %s", body.Build.Short(), fleet.SelfBuild().Short())
		row.Detail = why + "; a disagreement between these two may be skew rather than a bug"
	}
	return row
}

// probePeerGrants reads this machine's standing on a peer from that peer's own
// whoami, asked with the credential this machine presents there (muster
// #154): whether the peer lists this machine back, and what it grants.
//
// Read-only like every other row. A peer that cannot answer the question —
// unreachable, or on a build without the read — is unknown, never a failure:
// the reachable row already reports reachability.
func probePeerGrants(ctx context.Context, env doctorEnv, id, base, credential string, self, peer fleet.MachineId) doctorRow {
	row := doctorRow{ID: id, Refs: []int{154}}
	ctx, cancel := context.WithTimeout(ctx, env.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/v1/whoami?peer="+url.QueryEscape(string(self)), nil)
	if err != nil {
		row.Status = statusUnknown
		row.Summary = "not read — request could not be built from the configured address"
		return row
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	client := env.HTTP
	if client == nil {
		client = &http.Client{Timeout: env.Timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		row.Status = statusUnknown
		row.Summary = "not read — unreachable: " + describeNetErr(err)
		return row
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		row.Status = statusFail
		row.Summary = fmt.Sprintf("%s holds no principal for this machine's credential (%d)", peer, resp.StatusCode)
		row.Detail = "every relayed call from this machine is refused there (#98: token VALUE must match on both sides)"
		return row
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		row.Status = statusUnknown
		row.Summary = fmt.Sprintf("%s predates whoami — its grants cannot be read from here", peer)
		return row
	default:
		row.Status = statusUnknown
		row.Summary = fmt.Sprintf("not read — whoami answered %d", resp.StatusCode)
		return row
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		row.Status = statusUnknown
		row.Summary = "not read — whoami's answer did not parse"
		return row
	}
	var held []string
	_ = json.Unmarshal(raw["grants"], &held)
	grantText := "grants: none"
	if len(held) > 0 {
		grantText = "grants: " + strings.Join(held, ", ")
	}
	lists, has := raw["listsYou"]
	if !has {
		row.Status = statusUnknown
		row.Summary = fmt.Sprintf("%s predates the standing read; %s", peer, grantText)
		row.Detail = "whether it lists this machine back is not reported by that build"
		return row
	}
	var listsMe *bool
	_ = json.Unmarshal(lists, &listsMe)
	hasRead := false
	for _, g := range held {
		if g == string(service.GrantRead) {
			hasRead = true
		}
	}
	switch {
	case listsMe != nil && !*listsMe:
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%s does not list this machine back; %s", peer, grantText)
		row.Detail = "registration is one-sided: that machine's fleet-wide reads and relays never reach this one"
	case listsMe == nil:
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%s did not say whether it lists this machine — this credential lacks read there; %s", peer, grantText)
	case !hasRead:
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%s lists this machine back, but this credential lacks read there; %s", peer, grantText)
	default:
		row.Status = statusPass
		row.Summary = fmt.Sprintf("%s lists this machine back; %s", peer, grantText)
	}
	return row
}

// describeNetErr names the failure without echoing the address, which every
// net and url error embeds.
func describeNetErr(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "name does not resolve"
	}
	return "network error"
}

func worse(a, b rowStatus) rowStatus {
	rank := map[rowStatus]int{statusPass: 0, statusSkip: 0, statusUnknown: 1, statusWarn: 2, statusFail: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func redact(s, secret, replacement string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, replacement)
}

func joinGrants(gs []service.Grant) string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = string(g)
	}
	return strings.Join(out, ",")
}

func quoteAll(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(out, ", ")
}

func countRows(rows []doctorRow) map[rowStatus]int {
	counts := map[rowStatus]int{}
	for _, s := range statusOrder {
		counts[s] = 0
	}
	for _, r := range rows {
		counts[r.Status]++
	}
	return counts
}

func writeDoctorText(w io.Writer, self string, rows []doctorRow) {
	fmt.Fprintf(w, "muster doctor — build %s · machine %s\n\n", fleet.SelfBuild().Short(), self)
	width := 0
	for _, r := range rows {
		if len(r.ID) > width {
			width = len(r.ID)
		}
	}
	for _, r := range rows {
		line := fmt.Sprintf("%-7s  %-*s  %s", strings.ToUpper(string(r.Status)), width, r.ID, r.Summary)
		if len(r.Refs) > 0 {
			refs := make([]string, len(r.Refs))
			for i, n := range r.Refs {
				refs[i] = fmt.Sprintf("#%d", n)
			}
			line += "  (" + strings.Join(refs, " ") + ")"
		}
		fmt.Fprintln(w, line)
		if r.Detail != "" {
			fmt.Fprintf(w, "%-7s  %-*s  %s\n", "", width, "", r.Detail)
		}
	}
	counts := countRows(rows)
	parts := make([]string, 0, len(statusOrder))
	for _, s := range statusOrder {
		parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
	}
	fmt.Fprintf(w, "\n%s\n", strings.Join(parts, " · "))
}

func writeDoctorJSON(w io.Writer, self string, rows []doctorRow) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Build   string            `json:"build"`
		Machine string            `json:"machine"`
		Rows    []doctorRow       `json:"rows"`
		Counts  map[rowStatus]int `json:"counts"`
	}{fleet.SelfBuild().Short(), self, rows, countRows(rows)})
}
