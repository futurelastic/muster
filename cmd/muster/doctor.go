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
	"strconv"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/credrule"
	"github.com/futurelastic/muster/internal/drivers/opencode"
	tmuxdriver "github.com/futurelastic/muster/internal/drivers/tmux"
	"github.com/futurelastic/muster/internal/grant"
	"github.com/futurelastic/muster/internal/inboxclient"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/privfs"
	"github.com/futurelastic/muster/internal/sandbox"
	"github.com/futurelastic/muster/internal/service"
	"github.com/futurelastic/muster/internal/sessionenv"
	"github.com/futurelastic/muster/internal/trustseed"
)

// doctorServiceTask answers the service.task row. A variable so the generic
// doctor tests are not at the mercy of whether the machine they run on has a
// scheduled task of that name; main_run_test.go replaces it for them, and the
// Windows tests that are about the row put the real one back.
var doctorServiceTask = checkServiceTask

// doctorTmuxVersion runs `<bin> -V`. A variable for the same reason as
// doctorServiceTask: the generic doctor tests must not depend on which tmux, if
// any, the machine they run on has. main_run_test.go replaces it; the tests that
// are about the runtime.tmux-version row pass their own.
var doctorTmuxVersion = runTmuxVersion

// doctorAgentProgram asks whether the program a terminal session runs is found
// the way a session finds it. A variable for the same reason as
// doctorTmuxVersion: the generic doctor tests must not depend on whether the
// machine they run on has the agent CLI. main_run_test.go replaces it.
var doctorAgentProgram = tmuxdriver.ProbeAgentProgram

// doctorSandboxHost is the host check the sandbox.host row reads (muster #457). A
// variable for the same reason as the three above: on Linux the real one starts a
// confined child, and on macOS it applies a profile, and the generic doctor tests
// must depend on neither. main_run_test.go replaces it; the tests that are about
// the row pass their own report.
var doctorSandboxHost = sandbox.CheckHost

func runTmuxVersion(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "-V").Output()
	return strings.TrimSpace(string(out)), err
}

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
// principal names are printed; `principal list` already prints names. The one
// exception is trust.roots (#469), which lists the configured trust roots because
// confirming which folders are covered is its whole job, and shows them relative
// to home ("~/projects") rather than as absolute paths.
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
	// OpencodeOrphans lists the opencode servers running with no session record
	// (muster #398). Nil means the real scan of this machine; a test stubs it so
	// the row never depends on what else is running.
	OpencodeOrphans func(stateDir string) (pids []int, uninspected int, err error)
	// OpencodeDirs counts the opencode session directories no record names
	// (muster #399). Nil means the real scan of this machine, stubbed in a test
	// for the same reason as OpencodeOrphans.
	OpencodeDirs func(stateDir string) (opencode.OrphanDirReport, error)
	// OpencodeUnknown lists the opencode sessions a restart cannot relaunch, with
	// their sizes (muster #445). Nil means the real scan of this machine, stubbed in
	// a test for the same reason as the two above — and so that a test never walks a
	// directory.
	OpencodeUnknown func(stateDir string) (opencode.UnknownReport, error)
	// Now is the clock the age of a session is read against. Zero means time.Now.
	Now func() time.Time
}

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
	// configRefused is "config.load failed", from either cause: the file did not load,
	// or the start-up settings that read only the environment were refused. It is
	// what the trust.roots row waits for. It is NOT cfgFailed: a refused environment
	// leaves every other row decidable, because with no file they read nothing else.
	configRefused := false
	switch {
	case cfgPath == "":
		// The start runs the same checks with a nil config (main.go), so an
		// environment-only machine is refused there for exactly what this refuses
		// (muster #471): a relative, home or "/" trust root, an invalid
		// FLEET_OPENCODE_PROVIDERS entry. Until this call they passed the doctor, which
		// stopped at the single-token warning below.
		if err := validateConfigExtras(nil, getenv); err != nil {
			configRefused = true
			// The row stays config.load, as it is when a file is refused, and still says
			// "single-token mode": the mode is true of this machine and is the reason there
			// is no file to look in, so a refusal must not read as if it were gone.
			c.add(doctorRow{ID: "config.load", Status: statusFail,
				Summary: "FLEET_CONFIG unset (single-token mode), and the environment's own start-up settings are refused — the service refuses to start",
				Detail:  err.Error(),
				Refs:    []int{95, 471}})
			break
		}
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
			configRefused = true
			c.add(doctorRow{ID: "config.load", Status: statusFail,
				Summary: "FLEET_CONFIG does not load — the service refuses to start",
				Detail:  redact(err.Error(), cfgPath, "FLEET_CONFIG"),
				Refs:    []int{95}})
		} else {
			cfg = loaded
			c.add(doctorRow{ID: "config.load", Status: statusPass,
				Summary: fmt.Sprintf("principal table loaded: %d principal(s)", len(principals)),
				Refs:    []int{95}})
			c.add(checkReloadStatus(getenv("FLEET_STATE_DIR"), cfgPath))
		}
	}

	// --- trust roots (muster #469) -----------------------------------------
	// After config because the roots may come from it. No row on a machine that
	// has none configured, so a report from one is unchanged. A root the service
	// refuses is config.load's failure, not this row's (#471): the row lists the
	// roots once config.load has passed them.
	if row, ok := checkTrustRoots(cfg, configRefused, getenv); ok {
		c.add(row)
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
		runtime = defaultRuntime()
	}
	c.add(checkRuntime(runtime, getenv("FLEET_TMUX_BIN"), env.LookPath))
	c.add(checkTmuxVersion(ctx, runtime, getenv("FLEET_TMUX_BIN"), env))
	c.add(checkAgentProgram(ctx, runtime, env.Timeout))
	c.add(checkOpencodeRuntime(getenv("FLEET_RUNTIME_OPENCODE") == "1", getenv("FLEET_OPENCODE_BIN")))
	c.add(checkOpencodeProviders(getenv("FLEET_RUNTIME_OPENCODE") == "1", getenv("FLEET_OPENCODE_PROVIDERS")))
	c.add(checkOpencodeGrant(getenv("FLEET_RUNTIME_OPENCODE") == "1", getenv("FLEET_STATE_DIR"), getenv("FLEET_OPENCODE_PROVIDERS"), time.Now()))
	var credRules credrule.Rules
	var credEnv []sessionenv.Entry
	if cfg != nil {
		credRules, credEnv = cfg.credentialRules(), cfg.sessionEnv()
	}
	c.add(checkOpencodeCredentials(getenv("FLEET_RUNTIME_OPENCODE") == "1", getenv("FLEET_STATE_DIR"), getenv("FLEET_OPENCODE_PROVIDERS"), credRules, credEnv))
	orphans := env.OpencodeOrphans
	if orphans == nil {
		orphans = opencode.OrphanServers
	}
	c.add(checkOpencodeServers(getenv("FLEET_RUNTIME_OPENCODE") == "1", getenv("FLEET_STATE_DIR"), orphans))
	orphanDirs := env.OpencodeDirs
	if orphanDirs == nil {
		orphanDirs = opencode.OrphanDirs
	}
	c.add(checkOpencodeDirs(getenv("FLEET_RUNTIME_OPENCODE") == "1", getenv("FLEET_STATE_DIR"), orphanDirs))
	// The sessions row is the one that measures directories, which can take a while
	// on a machine that has many, so a row named in --skip is not computed at all.
	if c.skipped("runtime.opencode.sessions") {
		c.add(doctorRow{ID: "runtime.opencode.sessions"})
	} else {
		unknownSessions := env.OpencodeUnknown
		if unknownSessions == nil {
			unknownSessions = opencode.UnknownSessions
		}
		now := env.Now
		if now == nil {
			now = time.Now
		}
		c.add(checkOpencodeSessions(getenv("FLEET_RUNTIME_OPENCODE") == "1", getenv("FLEET_STATE_DIR"), now(), unknownSessions))
	}

	// --- the sandbox this host can offer (#457) ------------------------------------
	// Beside the runtime rows because it is the host's half of what the runtime
	// capability declares. On Linux it starts a child to confine, so a row named in
	// --skip is not computed at all.
	if c.skipped("sandbox.host") {
		c.add(doctorRow{ID: "sandbox.host"})
	} else {
		c.add(checkSandboxHost(doctorSandboxHost()))
	}

	// --- the service manager's own record (Windows only) -----------------------
	// muster installs no service manager; where the supported one is a scheduled
	// task, this row reads what that task says. No row elsewhere, so a Unix
	// report is unchanged.
	if row, ok := doctorServiceTask(getenv); ok {
		c.add(row)
	}

	// --- which launchd domain the service is in (macOS only) -----------------------
	// A copy in gui/<uid> breaks the next update; see doctor_servicedomain.go.
	if d, ok := doctorServiceDomain(); ok {
		c.add(judgeServiceDomain(d)) // add turns it into a skip row when --skip names it
	}

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

	// --- the owner-signed machine list (#351) ----------------------------------
	{
		base, _ := resolvePeers(getenv("FLEET_PEERS"), cfg)
		var handPrincipals []service.Principal
		if cfg != nil {
			handPrincipals, _ = cfg.principals()
		}
		listSelf := self
		c.add(listDoctorRow(loadMachineList(cfg, getenv("FLEET_STATE_DIR"), listSelf), base, handPrincipals))
	}

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
		urlOK := true
		var schemes []string
		for _, b := range p.bases {
			u, err := url.Parse(b)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				urlOK = false
				break
			}
			schemes = append(schemes, u.Scheme)
		}
		switch {
		case urlOK && len(p.bases) == 1:
			c.add(doctorRow{ID: prefix + ".url", Status: statusPass, Summary: "address parses as " + schemes[0]})
		case urlOK:
			c.add(doctorRow{ID: prefix + ".url", Status: statusPass,
				Summary: fmt.Sprintf("%d addresses, tried in order; each parses as %s", len(p.bases), strings.Join(schemes, "/")),
				Refs:    []int{335}})
		case len(p.bases) == 1:
			c.add(doctorRow{ID: prefix + ".url", Status: statusFail,
				Summary: "address is not an http(s) URL with a host"})
		default:
			c.add(doctorRow{ID: prefix + ".url", Status: statusFail,
				Summary: "an address is not an http(s) URL with a host", Refs: []int{335}})
		}

		credential := peerCred{bearer: p.token, peer: p.machine}
		var keyRow doctorRow
		// The address that answered the reachability probe, so the grants read
		// asks the same one; the first when none did or none was probed.
		answered := p.bases[0]
		switch {
		case p.auth == peerAuthKey:
			credential, keyRow = keyCredential(getenv, self, p.machine)
			c.add(keyRow)
		case p.token != "":
			c.add(doctorRow{ID: prefix + ".credential", Status: statusPass,
				Summary: "per-peer credential configured (token)"})
		case token != "":
			credential.bearer = token
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
		case !urlOK || credential.empty():
			c.add(doctorRow{ID: prefix + ".reachable", Status: statusUnknown,
				Summary: "not probed — no usable address or credential"})
		default:
			if c.skipped(prefix + ".reachable") {
				c.add(doctorRow{ID: prefix + ".reachable"})
			} else {
				var row doctorRow
				row, answered = probePeerAny(ctx, env, prefix+".reachable", p.bases, credential)
				c.add(row)
			}
		}

		// This machine's standing on the peer, as the peer reports it
		// (muster #154). The row id is the one it has always had.
		switch {
		case env.Offline:
			c.add(doctorRow{ID: prefix + ".grants", Status: statusUnknown,
				Summary: "not probed (--offline)", Refs: []int{154}})
		case !urlOK || credential.empty():
			c.add(doctorRow{ID: prefix + ".grants", Status: statusUnknown,
				Summary: "not probed — no usable address or credential", Refs: []int{154}})
		case c.skipped(prefix + ".grants"):
			c.add(doctorRow{ID: prefix + ".grants"})
		default:
			c.add(probePeerGrants(ctx, env, prefix+".grants", strings.TrimRight(answered, "/"), credential, self, p.machine))
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

// validateConfigExtras is every start-up check that depends only on the file and
// the environment, apart from the two that already have a function of their own
// (cfg.principals and resolvePeers). Each is a refusal to start in main.go, so
// each is a fail in `muster doctor`'s config.load row and a "a restart would
// fail" in a reload's restart-only refusal.
//
// It is ONE function that all three call (muster #374). The checks used to be
// written out in main.go and repeated here, and the copies drifted: a file with
// closedRetentionDays: -1 passed config.load and the restart then stopped on it.
// Adding a start-up check means adding it here, not beside the code that uses
// the setting.
//
// What it does not answer is what only the running process can: whether the
// default runtime's driver is registered right now (opencode enabled but its
// binary missing, say). That stays at the start, in SetDefaultRuntime, and
// docs/install.md says so. cfg may be nil (no FLEET_CONFIG): the environment's
// own settings are still checked, by the start and, since muster #471, by the
// doctor, which used to stop at the single-token warning instead of calling this.
// The error is safe to show: a sessionEnv fromFile path is replaced.
func validateConfigExtras(cfg *fileConfig, getenv func(string) string) error {
	// Trust roots are the tmux driver's: a start with another primary runtime does
	// not read them, and so does not stop on one that is not absolute.
	if name, ok := primaryRuntime(getenv); ok && name == runtimeTmux {
		roots := configuredTrustRoots(cfg, getenv)
		// Home is not known to a getenv-only caller, so it is read the way main.go
		// reads it. When it cannot be (no HOME), the home comparison is simply not
		// made here: main.go refuses that start on its own, and "/" is still caught.
		home, _ := os.UserHomeDir()
		for _, r := range roots {
			if !filepath.IsAbs(r) {
				return fmt.Errorf("trust root %q must be an absolute path", r)
			}
			// muster #469. A root that is the home directory (spelled any way, or by a
			// link) or "/" was dropped by the seeder without a word while the start-up
			// line counted it as configured. It can never do what the operator wrote it
			// for — seeding home would vouch for everything under it — so it stops the
			// start like a relative root does, and shows in the doctor and in a reload's
			// refusal. Refusing costs no behaviour that worked: such a root has never
			// seeded anything. The folder to name is the one that holds the working
			// folders.
			if why := trustseed.RootRefusal(home, r); why != "" {
				// Named home-relative, like the trust.roots row: this error reaches the
				// doctor's config.load detail, which does not print an absolute path.
				return fmt.Errorf("trust root %q is %s, which cannot be a trust root: it would "+
					"vouch for everything under it; name the folder that holds the working "+
					"folders instead (a projects directory under home, for example)", homeRelative(home, r), why)
			}
		}
	}
	// The opencode provider policy (muster #401): an entry that cannot be one stops
	// the start, naming it, and shows in the doctor and a reload's refusal. Checked
	// only where the runtime is enabled; a policy no runtime reads is not judged. It
	// lives in the environment, so it is checked with no config file too.
	if getenv("FLEET_RUNTIME_OPENCODE") == "1" {
		if _, err := opencode.DescribeProviderPolicy(splitList(getenv("FLEET_OPENCODE_PROVIDERS"))); err != nil {
			return fmt.Errorf("FLEET_OPENCODE_PROVIDERS: %w", err)
		}
	}
	if cfg == nil {
		return nil
	}
	if len(cfg.SessionEnv) > 0 {
		entries := cfg.sessionEnv()
		if err := sessionenv.Validate(entries, knownRuntimeIDs()); err != nil {
			msg := err.Error()
			for _, e := range entries {
				msg = redact(msg, e.FromFile, "<fromFile>")
			}
			return errors.New(msg)
		}
	}
	// The operator's credential rule (muster #458): an entry that cannot be one, or can
	// never apply, stops the start naming it, and shows in the doctor and a reload's
	// refusal.
	if err := credrule.Validate(cfg.credentialRules(), knownRuntimeIDs()); err != nil {
		return err
	}
	// The operator's opencode agent file (muster #396): only the path is judged
	// here. Whether the file exists and holds usable agents is judged on every
	// create, because what it holds is read then and not before.
	if p := cfg.operatorAgentsFile(); p != "" && !filepath.IsAbs(p) {
		return fmt.Errorf("opencode.agents %q must be an absolute path", p)
	}
	// Declared model lists (muster #400): the shape is judged here so the start,
	// the doctor and a reload agree.
	if err := validateModelsConfig(cfg.Models); err != nil {
		return err
	}
	// The default runtime must be one this machine runs. Whether its driver is up
	// is the start's to say (above).
	if cfg.DefaultRuntime != "" {
		if ids, ok := enabledLocalRuntimes(getenv); ok && !containsRuntime(ids, fleet.RuntimeId(cfg.DefaultRuntime)) {
			have := make([]string, len(ids))
			for i, id := range ids {
				have[i] = string(id)
			}
			if len(have) == 0 {
				have = []string{"none"}
			}
			return fmt.Errorf("defaultRuntime %q is not a runtime this machine runs (it runs: %s)",
				cfg.DefaultRuntime, strings.Join(have, ", "))
		}
	}
	// Zero is "not set"; the setter's own range decides anything else.
	if cfg.MaxInputBytes != 0 {
		if err := service.ValidateMaxInputBytes(cfg.MaxInputBytes); err != nil {
			return err
		}
	}
	if cfg.ClosedRetentionDays < 0 {
		return fmt.Errorf("closedRetentionDays must be positive, got %d", cfg.ClosedRetentionDays)
	}
	if cfg.InputAuditRetentionDays < 0 {
		return fmt.Errorf("inputAuditRetentionDays must be positive, got %d", cfg.InputAuditRetentionDays)
	}
	// A machine list that is the only source of peers and does not verify stops
	// the start. In prefer and shadow mode it is reported and the start goes on, so
	// it is not a failure here (the machine-list row says it).
	self := fleet.MachineId(getenv("FLEET_MACHINE"))
	if self == "" {
		self = "local"
	}
	return loadMachineList(cfg, getenv("FLEET_STATE_DIR"), self).fatal()
}

func containsRuntime(ids []fleet.RuntimeId, id fleet.RuntimeId) bool {
	for _, have := range ids {
		if have == id {
			return true
		}
	}
	return false
}

type doctorPeer struct {
	machine fleet.MachineId
	// bases is every address the peer is known by, in the order tried. One is
	// the ordinary case (#335).
	bases []string
	token string
	// auth is how this machine authenticates there: peerAuthToken or peerAuthKey.
	auth string
}

// doctorPeers resolves the peer list exactly as main.go does — through the
// same resolvePeers, so the two cannot disagree: FLEET_PEERS wins, the config
// file's peers otherwise; the per-peer credential always comes from the config
// file.
func doctorPeers(getenv func(string) string, cfg *fileConfig) ([]doctorPeer, error) {
	entries, _, err := resolveEffectivePeers(getenv, cfg)
	var out []doctorPeer
	for _, e := range entries {
		p := doctorPeer{machine: e.machine, bases: e.urls, auth: e.auth}
		if p.auth == "" {
			p.auth = cfg.peerAuthFor(e.machine)
		}
		if cfg != nil {
			if tok, ok := cfg.peerTokenFor(p.machine); ok {
				p.token = tok
			}
		}
		out = append(out, p)
	}
	return out, err
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
		//
		// acting-person (muster #349) is excluded the same way: it lets a client
		// name the human it acts for in the audit, and drives nothing.
		//
		// push-list (muster #352) is excluded for a stronger reason than the
		// others: it lets a holder replace this machine's peer table, and
		// `muster service install` enrols the local service principal with THIS
		// set, so counting it would grant it by default on every new install.
		// It is given per peer, by an operator, and nowhere else.
		//
		// debug (muster #450) is excluded for the same stronger reason: it reads the
		// service's own process internals and drives no session, and the service
		// principal `service install` enrols must not hold it by default.
		//
		// record (muster #441) is excluded because the owner ruled that nobody holds
		// it by default: it reads a session's whole record, tool output included,
		// and the service principal `service install` enrols must not hold it.
		if g != service.GrantRelay && g != service.GrantLabel && g != service.GrantRemoteControl && g != service.GrantHumanRelay && g != service.GrantActingPerson && g != service.GrantPushList && g != service.GrantDebug && g != service.GrantRecord {
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

func checkBind(raw string) doctorRow { return checkBindWith(raw, localAddrPresent) }

// checkBindWith is checkBind with the question "is this address assigned to
// this machine right now" injectable. The service treats each FLEET_ADDR entry
// as wanted, not required (#335): an address that is not here yet is waited
// for, so a row that only parsed the list could not tell an operator which
// entries the service has bound and which it is waiting on. This one reads the
// interface list, which is what the service's own retry loop reads — so it
// reports the state the service converges to, without needing the service to
// be running.
func checkBindWith(raw string, hasAddr func(net.IP) bool) doctorRow {
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
	waiting, routableNow := 0, 0
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
			routableNow++
		case "127.0.0.1", "localhost", "::1":
		default:
			routable = true
			if ip := checkableIP(a); ip != nil && !isLoopbackHost(host) && hasAddr != nil && !hasAddr(ip) {
				waiting++
				notes = append(notes, fmt.Sprintf("entry %d is not assigned to this machine now — the service waits for it and binds it when it appears", i+1))
			} else {
				routableNow++
				if ip != nil {
					notes = append(notes, fmt.Sprintf("entry %d is assigned here — the service binds it", i+1))
				}
			}
		}
	}
	row.Status = status
	if routable && routableNow == 0 {
		// Every routable entry is absent: the service starts, on loopback, and
		// no peer can reach it until one of the addresses appears.
		status = worse(status, statusWarn)
		row.Status = status
		row.Summary = fmt.Sprintf("%d address(es), none of the routable ones is on this machine now — reachable only from itself until one appears", len(addrs))
	} else if routable && waiting > 0 {
		row.Summary = fmt.Sprintf("%d address(es), routable; %d of them waiting for their address to appear (loopback is added automatically)", len(addrs), waiting)
	} else if routable {
		row.Summary = fmt.Sprintf("%d address(es), routable (loopback is added automatically)", len(addrs))
	} else {
		row.Summary = fmt.Sprintf("%d address(es), loopback-only — no peer can reach this machine", len(addrs))
	}
	row.Detail = strings.Join(notes, "; ")
	return row
}

func checkRuntime(runtime, tmuxBin string, lookPath func(string) (string, error)) doctorRow {
	return checkRuntimeOn(hostOS(), runtime, tmuxBin, lookPath)
}

// checkRuntimeOn is checkRuntime with the operating system named, so a test can
// ask what Windows would say from any machine.
func checkRuntimeOn(goos, runtime, tmuxBin string, lookPath func(string) (string, error)) doctorRow {
	row := doctorRow{ID: "runtime"}
	if err := checkRuntimeName(goos, runtime); err != nil {
		row.Status = statusFail
		row.Summary = err.Error() + " — the service refuses to start"
		return row
	}
	switch runtime {
	case "stub":
		row.Status = statusPass
		row.Summary = "FLEET_RUNTIME=stub"
	case "none":
		row.Status = statusPass
		row.Summary = "FLEET_RUNTIME=none — no primary local runtime"
		row.Detail = "the local runtimes are the optional ones enabled below (FLEET_RUNTIME_OPENCODE); with none enabled the service has no local sessions and fronts its peers"
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
			if !isExecutableFile(tmuxBin) {
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
		row.Summary = fmt.Sprintf("unknown FLEET_RUNTIME %q (want tmux, stub or none) — the service refuses to start", runtime)
	}
	return row
}

// checkTmuxVersion is the runtime.tmux-version row (muster #333): the terminal
// path pastes with bracketed paste, and tmux first can say whether the pane
// asked for it in 3.7 (`#{bracket_paste_flag}`). On an older tmux the pre-check
// is not possible; since #334 a send there goes out as `paste-buffer -p` without
// it, so this row is a `warn` naming what is not being checked rather than a
// failure (it was a `fail` while every send there was refused, #333).
//
// It runs the binary the service will run (FLEET_TMUX_BIN, else the PATH
// lookup the runtime row already did) with `-V`, which touches no server. A
// version it cannot read is `unknown`, never a pass: a development build or a
// wrapper script says nothing about the flag.
//
// `warn`, by ADR 160's definition: the service works, with a check missing that
// the operator can restore. A version it cannot read stays `unknown`.
func checkTmuxVersion(ctx context.Context, runtime, tmuxBin string, env doctorEnv) doctorRow {
	row := doctorRow{ID: "runtime.tmux-version", Refs: []int{333, 334}}
	if runtime != "tmux" {
		row.Status = statusSkip
		row.Summary = fmt.Sprintf("FLEET_RUNTIME=%s does not run tmux", runtime)
		return row
	}
	bin := tmuxBin
	if bin == "" {
		lookPath := env.LookPath
		if lookPath == nil {
			lookPath = exec.LookPath
		}
		p, err := lookPath("tmux")
		if err != nil {
			row.Status = statusSkip
			row.Summary = "no tmux to ask — the runtime row says why"
			return row
		}
		bin = p
	}
	vctx, cancel := context.WithTimeout(ctx, env.Timeout)
	defer cancel()
	out, err := doctorTmuxVersion(vctx, bin)
	if err != nil {
		row.Status = statusUnknown
		row.Summary = "could not run the tmux binary with -V"
		row.Detail = redact(err.Error(), bin, "FLEET_TMUX_BIN")
		return row
	}
	provides, known := tmuxdriver.TmuxProvidesBracketPasteFlag(out)
	switch {
	case !known:
		row.Status = statusUnknown
		row.Summary = fmt.Sprintf("tmux version line not recognised (%q)", out)
		row.Detail = "terminal-path delivery needs a tmux that provides #{bracket_paste_flag}, first released in tmux " +
			tmuxdriver.BracketPasteFlagSince + "; a development build or a wrapper script cannot be judged from here"
	case provides:
		row.Status = statusPass
		row.Summary = fmt.Sprintf("%s provides #{bracket_paste_flag}", out)
	default:
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%s cannot report #{bracket_paste_flag} — terminal-path sends go out without that check", out)
		row.Detail = "that variable first exists in tmux " + tmuxdriver.BracketPasteFlagSince + " (measured: 3.4, 3.5a and 3.6a read it empty). " +
			"Here the driver pastes with `paste-buffer -p`, which brackets only when the pane asked, and relies on the composer read-back " +
			"before it presses Enter (muster #334; docs/compat.md, G2). Run `muster compat` against this tmux to prove it for your runtime build, " +
			"or use tmux " + tmuxdriver.BracketPasteFlagSince + " or newer via FLEET_TMUX_BIN to restore the check"
	}
	return row
}

// checkAgentProgram is the runtime.agent-program row (muster #342): the
// terminal runtime registers with nothing but its multiplexer, so a machine
// with tmux and without the agent CLI looked ready, answered a create 201, and
// lost the session at once. GET /v1/runtimes now says `unavailable` /
// `not-installed` for it; this row says the same before the service starts.
//
// It asks the way a session will start the program — through the login shell
// the driver wraps it in — so a PATH that only the user's startup files extend
// counts, and a bare service-manager PATH does not hide it. It names the
// program, never a path.
func checkAgentProgram(ctx context.Context, runtime string, timeout time.Duration) doctorRow {
	row := doctorRow{ID: "runtime.agent-program", Refs: []int{342}}
	if runtime != "tmux" {
		row.Status = statusSkip
		row.Summary = fmt.Sprintf("FLEET_RUNTIME=%s does not start an agent program", runtime)
		return row
	}
	const program = "claude"
	pctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	found, known := doctorAgentProgram(pctx, program)
	switch {
	case !known:
		row.Status = statusUnknown
		row.Summary = fmt.Sprintf("could not tell whether %s is found where a session starts it", program)
		row.Detail = "the check runs the login shell the service wraps sessions in; it did not answer in time or could not run on this platform"
	case found:
		row.Status = statusPass
		row.Summary = fmt.Sprintf("%s is found where a session starts it", program)
	default:
		// A warning, not a failure: the service is healthy and says so itself
		// (the runtime is listed unavailable, a create is refused), and the
		// program may be installed after the service, which needs no restart.
		// A failing row would also stop `muster service install` from starting
		// the unit on a machine that gets the agent CLI a minute later.
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%s was not found where a session starts it (not-installed)", program)
		row.Detail = "sessions are started through $SHELL -lic, so the program must be on that shell's PATH. " +
			"Until it is, GET /v1/runtimes lists the terminal runtime as unavailable / not-installed and a create is refused with runtime-unavailable; " +
			"installing it makes the runtime available again without a restart"
	}
	return row
}

// checkOpencodeRuntime is the runtime.opencode row (muster #297): an operator
// who set FLEET_RUNTIME_OPENCODE=1 and whose runtime is not coming up should
// not have to read the service log to find out why.
//
// It answers the half of the question that is a property of the install —
// found, runnable — and uses the same resolver the service's own probe starts
// with, so the two cannot disagree. It deliberately does NOT run the binary:
// "did not answer in time" is a property of the machine's load at the moment,
// and it is reported by the running service itself, per runtime, as `status`
// and `cause` on GET /v1/runtimes. Like every row here it names the variable,
// never the path.
func checkOpencodeRuntime(enabled bool, bin string) doctorRow {
	row := doctorRow{ID: "runtime.opencode"}
	if !enabled {
		row.Status = statusSkip
		row.Summary = "FLEET_RUNTIME_OPENCODE is not 1 — the opencode runtime is not enabled"
		return row
	}
	path, cause, err := opencode.Locate(bin)
	switch {
	case cause == fleet.CauseNotExecutable, err == nil && !isExecutableFile(path):
		// The second arm is Windows's: Locate resolves through PATHEXT but
		// accepts any file that already has an extension, so a path to a
		// document resolves. Judging the resolved path here keeps "executable"
		// one question per platform (execbit_*.go, #312). On Unix Locate has
		// already checked the mode bit and the arm never fires.
		row.Status = statusFail
		row.Summary = "opencode runtime enabled, but its binary cannot be run (not-executable)"
		row.Detail = "the file exists and is not an executable the service user can run — check FLEET_OPENCODE_BIN, its mode, and on Windows that its extension is in PATHEXT"
	case err == nil:
		if bin == "" || !strings.ContainsRune(bin, filepath.Separator) {
			row.Status = statusWarn
			row.Summary = "opencode runtime enabled; its binary is a bare name, found on this shell's PATH"
			row.Detail = "a service manager usually starts the service with a bare PATH that lacks it — set FLEET_OPENCODE_BIN to an absolute path in the unit. " +
				"Whether the binary answers its version check in time is reported by the running service: GET /v1/runtimes, status and cause"
			return row
		}
		row.Status = statusPass
		row.Summary = "opencode runtime enabled; FLEET_OPENCODE_BIN names an executable"
		row.Detail = "whether the binary answers its version check in time is reported by the running service: GET /v1/runtimes, status and cause"
	case cause != fleet.CauseNotInstalled:
		row.Status = statusFail
		row.Summary = "opencode runtime enabled, but its binary could not be resolved"
		row.Detail = "check FLEET_OPENCODE_BIN"
	default:
		row.Status = statusFail
		row.Summary = "opencode runtime enabled, but its binary was not found (not-installed)"
		row.Detail = "install opencode on the service's PATH or set FLEET_OPENCODE_BIN to its absolute path; the runtime is listed as unavailable on GET /v1/runtimes until then"
	}
	return row
}

// checkOpencodeProviders is the runtime.opencode.providers row (muster #293,
// #401): which providers, and which models of them, an opencode session may use
// on this machine — the policy the service will enforce, parsed the way the
// service parses it. A malformed entry is a failure that names the entry, because
// the service refuses to start on it; it runs whether or not FLEET_CONFIG is set,
// since the policy lives in the environment. The policy itself is not a secret
// and the row quotes it.
func checkOpencodeProviders(enabled bool, raw string) doctorRow {
	row := doctorRow{ID: "runtime.opencode.providers", Refs: []int{293, 401}}
	if !enabled {
		row.Status = statusSkip
		row.Summary = "FLEET_RUNTIME_OPENCODE is not 1 — the opencode runtime is not enabled"
		return row
	}
	entries := splitList(raw)
	desc, err := opencode.DescribeProviderPolicy(entries)
	if err != nil {
		row.Status = statusFail
		row.Summary = "FLEET_OPENCODE_PROVIDERS does not parse — the service refuses to start"
		row.Detail = err.Error()
		return row
	}
	row.Status = statusPass
	row.Summary = "opencode sessions may use: " + desc
	if opencode.NarrowsModels(entries) {
		row.Detail = "model rules are checked when a session is created and on each turn's record; " +
			"a session's own configuration enables whole providers only. " +
			"Name exact models to allow: a name pattern is not a free-tier or data-retention filter"
	}
	return row
}

// checkOpencodeGrant is the runtime.opencode.grant row (muster #451): the state of
// the machine's ChatGPT sign-in, read from its store, so it needs no running
// service. A store that anyone but its owner can read is a failure — it holds a
// refresh token. A revoked sign-in is a warning that says what to run: sessions
// on the provider fail until someone signs in again, and this is the row that says
// why, instead of a run of failed turns. A machine that never signed in is a skip,
// not a finding. Like every row here it names no path.
func checkOpencodeGrant(enabled bool, stateDir, providers string, now time.Time) doctorRow {
	row := doctorRow{ID: "runtime.opencode.grant", Refs: []int{451}}
	switch {
	case !enabled:
		row.Status = statusSkip
		row.Summary = "FLEET_RUNTIME_OPENCODE is not 1 — the opencode runtime is not enabled"
		return row
	case stateDir == "":
		row.Status = statusSkip
		row.Summary = "FLEET_STATE_DIR is unset — a ChatGPT sign-in is kept there, so none can be used"
		return row
	}
	store := grant.NewStore(stateDir)
	g, err := store.Load()
	switch {
	case errors.Is(err, grant.ErrNoGrant):
		row.Status = statusSkip
		row.Summary = "no ChatGPT sign-in is stored (optional: `muster grant login` signs this machine in once)"
		return row
	case err != nil:
		row.Status = statusFail
		row.Summary = "the stored ChatGPT sign-in cannot be read"
		row.Detail = err.Error()
		return row
	}
	if rep, ok := store.Check(); ok && rep.Verdict == privfs.Loose {
		row.Status = statusFail
		row.Summary = "the stored ChatGPT sign-in is readable by more than its owner"
		row.Detail = rep.Detail + "; it holds a refresh token — fix the access, or sign in again after `muster grant logout`"
		return row
	}
	if g.Revoked != nil {
		row.Status = statusWarn
		row.Summary = "the ChatGPT sign-in is revoked — sign in again (`muster grant login`)"
		row.Detail = (&grant.RevokedError{Status: g.Revoked.Status, Code: g.Revoked.Code}).Error() +
			"; sessions on the openai provider fail until then, and nothing renews it"
		return row
	}
	exp := g.ExpiresAt()
	if !opencode.ProviderPermitted(splitList(providers), grant.ProviderOpenAI) {
		row.Status = statusWarn
		row.Summary = "a ChatGPT sign-in is stored but FLEET_OPENCODE_PROVIDERS does not allow openai — no session will use it"
		row.Detail = "a sign-in makes openai usable, it does not allow it: name openai in FLEET_OPENCODE_PROVIDERS to use it"
		return row
	}
	if !exp.After(now) {
		row.Status = statusWarn
		row.Summary = "the ChatGPT access token has lapsed"
		row.Detail = "the service renews it before it lapses; if the service is running and this persists, " +
			"check that it can reach the provider (`muster grant status`)"
		return row
	}
	// muster #458: the provider keeps refusing requests made with the sign-in. The mark
	// is the running service's observation, refreshed while it stays true, so one that
	// has not been refreshed lately was left by a service that has since stopped and is
	// said so rather than reported as current.
	if g.Limit != nil {
		seen := time.UnixMilli(g.Limit.UpdatedMs)
		since := time.UnixMilli(g.Limit.SinceMs)
		if now.Sub(seen) <= grantLimitFresh {
			row.Status = statusWarn
			row.Summary = "the provider is refusing requests on the ChatGPT sign-in (since " + since.UTC().Format(time.RFC3339) + ")"
			row.Detail = "every session on the subscription shares one plan and is affected at once; they wait in the runtime's retry loop " +
				"and recover on their own when the provider lets requests through (an exhausted usage window is the usual cause)"
			if g.Limit.Message != "" {
				row.Detail += "; the provider said: " + g.Limit.Message
			}
			return row
		}
		row.Detail = "a usage mark from " + since.UTC().Format(time.RFC3339) + " was last confirmed " + until(seen, now) +
			" and is not current: the service that wrote it is probably stopped"
	}
	row.Status = statusPass
	row.Summary = "ChatGPT sign-in present; access token valid until " + exp.UTC().Format(time.RFC3339) +
		" (" + until(exp, now) + "); the service renews it"
	return row
}

// grantLimitFresh is how recent a usage mark must have been confirmed for the doctor
// to report it as happening now: the service writes it again every minute while true.
const grantLimitFresh = 3 * time.Minute

// checkOpencodeCredentials is the runtime.opencode.credentials row (muster #458):
// what the operator's providerCredentials rule says, and whether the machine can
// satisfy it. A rule that names a credential this machine does not hold is a warning,
// because a session bound for openai is then refused, which is the point of the rule
// and also not what the operator hoped to find out at the first create. Both
// credentials reaching sessions with no rule to choose is the situation the rule
// exists for, said once here. Read from disk like its neighbour: no service needed.
func checkOpencodeCredentials(enabled bool, stateDir, providers string, rules credrule.Rules, env []sessionenv.Entry) doctorRow {
	row := doctorRow{ID: "runtime.opencode.credentials", Refs: []int{458}}
	if !enabled {
		row.Status = statusSkip
		row.Summary = "FLEET_RUNTIME_OPENCODE is not 1 — the opencode runtime is not enabled"
		return row
	}
	if err := credrule.Validate(rules, knownRuntimeIDs()); err != nil {
		row.Status = statusFail
		row.Summary = "providerCredentials cannot be used"
		row.Detail = err.Error()
		return row
	}
	signedIn := false
	if stateDir != "" {
		if g, err := grant.NewStore(stateDir).Load(); err == nil && g.Revoked == nil {
			signedIn = true
		}
	}
	keyEntry := false
	for _, e := range env {
		if e.Name == "OPENAI_API_KEY" {
			keyEntry = true
		}
	}
	permitted := opencode.ProviderPermitted(splitList(providers), grant.ProviderOpenAI)
	switch {
	case len(rules) == 0 && signedIn && keyEntry && permitted:
		row.Status = statusWarn
		row.Summary = "a ChatGPT sign-in and an OPENAI_API_KEY both reach sessions and no providerCredentials rule chooses"
		row.Detail = "with no rule the sign-in is used and the key is withheld, so the subscription pays; " +
			"name the credential per machine, agent or marker in providerCredentials to decide it"
		return row
	case len(rules) == 0:
		row.Status = statusSkip
		row.Summary = "no providerCredentials rule — a usable sign-in is used, else the OPENAI_API_KEY a session carries"
		return row
	case !permitted:
		row.Status = statusWarn
		row.Summary = "providerCredentials names openai but FLEET_OPENCODE_PROVIDERS does not allow it — the rule applies to no session"
		row.Detail = "a rule chooses between credentials, it does not allow a provider"
		return row
	}
	desc := rules.Describe()
	for _, r := range rules {
		switch {
		case r.Use == fleet.CredentialSubscription && !signedIn:
			row.Status = statusWarn
			row.Summary = "providerCredentials runs openai on the subscription but no usable ChatGPT sign-in is stored"
			row.Detail = desc + "; a session bound for openai is refused until `muster grant login`, and the key is not used in its place"
			return row
		case r.Use == fleet.CredentialAPIKey && !keyEntry:
			row.Status = statusWarn
			row.Summary = "providerCredentials runs openai on an API key but no sessionEnv entry supplies OPENAI_API_KEY"
			row.Detail = desc + "; a session bound for openai is refused unless its create carries the key, and the sign-in is not used in its place"
			return row
		}
	}
	row.Status = statusPass
	row.Summary = "openai credential: " + desc
	return row
}

// checkOpencodeServers is the runtime.opencode.servers row (muster #398): how
// many opencode server processes run under the service's session directory with
// no session record naming them. Such a process holds a conversation database
// and 0.3-0.9 GB of memory for a session nobody can reach, and before this row a
// leak was visible only to someone who counted processes by hand.
//
// The doctor is a separate process and cannot see which servers the running
// service holds, so a server is "with no session" when no record under the state
// directory names its pid. The one false positive is a create in flight — its
// server is up a moment before its record is written — which is why a count is a
// warning and never a failure. Like every row here it names no path.
func checkOpencodeServers(enabled bool, stateDir string, scan func(string) ([]int, int, error)) doctorRow {
	row := doctorRow{ID: "runtime.opencode.servers", Refs: []int{398}}
	switch {
	case !enabled:
		row.Status = statusSkip
		row.Summary = "FLEET_RUNTIME_OPENCODE is not 1 — there are no opencode servers to count"
		return row
	case stateDir == "":
		row.Status = statusSkip
		row.Summary = "FLEET_STATE_DIR is not set — opencode sessions are not remembered, so no server outlives the service to be counted"
		return row
	}
	pids, uninspected, err := scan(stateDir)
	switch {
	case errors.Is(err, opencode.ErrScanUnsupported):
		row.Status = statusSkip
		row.Summary = "this platform cannot list running processes with their environment — opencode servers are not counted"
	case err != nil:
		row.Status = statusUnknown
		row.Summary = "the running opencode servers could not be listed"
		row.Detail = err.Error()
	case len(pids) > 0:
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%d opencode server process(es) run with no session", len(pids))
		row.Detail = fmt.Sprintf("pid %s. Each holds a conversation database and memory for a session nobody can reach. "+
			"Restarting the service stops them (its start-up sweep does), or stop those process groups by hand. "+
			"A server whose create is still in flight is counted for a moment before its record is written", joinInts(pids))
		if uninspected > 0 {
			row.Detail += fmt.Sprintf("; %d more look like servers but could not be inspected (run the doctor as the service user)", uninspected)
		}
	case uninspected > 0:
		row.Status = statusWarn
		row.Summary = fmt.Sprintf("%d process(es) look like opencode servers but could not be inspected", uninspected)
		row.Detail = "another user's process, or one whose environment cannot be read — run the doctor as the service user"
	default:
		row.Status = statusPass
		row.Summary = "no opencode server runs without a session"
	}
	return row
}

// checkOpencodeDirs is the runtime.opencode.dirs row (muster #399): how many
// opencode session directories under the service's state directory no session
// record names and no running server holds. A session's directory holds the
// runtime's conversation database, so a count is a warning, never a failure, and
// the row splits what the service removes by itself from what it never does:
//
//   - a directory holding nothing a session owns (only scratch, caches and lock
//     files — what a removal that did not finish leaves) is removed by the service,
//     at its next start and then hourly while it runs, once nothing has written to
//     it for an hour;
//   - a directory that may hold a session's own data stays until a person removes
//     it, because deleting a conversation is not something the service decides.
//
// Like the servers row it cannot see a create in flight, whose directory exists a
// moment before its record, and like every row here it names no path.
func checkOpencodeDirs(enabled bool, stateDir string, scan func(string) (opencode.OrphanDirReport, error)) doctorRow {
	row := doctorRow{ID: "runtime.opencode.dirs", Refs: []int{399}}
	switch {
	case !enabled:
		row.Status = statusSkip
		row.Summary = "FLEET_RUNTIME_OPENCODE is not 1 — there are no opencode session directories to count"
		return row
	case stateDir == "":
		row.Status = statusSkip
		row.Summary = "FLEET_STATE_DIR is not set — opencode sessions are not remembered, so no session directory outlives its session to be counted"
		return row
	}
	rep, err := scan(stateDir)
	switch {
	case errors.Is(err, opencode.ErrScanUnsupported):
		row.Status = statusSkip
		row.Summary = "this platform cannot list running processes with their environment — opencode session directories are not counted"
		return row
	case err != nil:
		row.Status = statusUnknown
		row.Summary = "the opencode session directories could not be listed"
		row.Detail = err.Error()
		return row
	case rep.Total() == 0 && rep.Unreadable == 0 && rep.Uninspected == 0:
		row.Status = statusPass
		row.Summary = "no opencode session directory is left without a session record"
		return row
	}
	row.Status = statusWarn
	var detail []string
	if rep.Total() == 0 {
		row.Summary = "no session directory without a record was counted, but it could not be checked completely"
	} else {
		row.Summary = fmt.Sprintf("%d opencode session directory(ies) without a session record (%s)", rep.Total(), humanSize(rep.RemovableBytes+rep.HoldStoreBytes))
		if rep.Removable > 0 {
			detail = append(detail, fmt.Sprintf("%d (%s) hold nothing a session owns — leftovers of a removal that did not finish. "+
				"The service removes those nothing has written to for an hour, at its next start and hourly while it runs", rep.Removable, humanSize(rep.RemovableBytes)))
		}
		if rep.HoldStore > 0 {
			detail = append(detail, fmt.Sprintf("%d (%s) may hold a session's conversation database and are never removed automatically; "+
				"remove one by hand once nobody needs that conversation", rep.HoldStore, humanSize(rep.HoldStoreBytes)))
		}
	}
	if rep.Unreadable > 0 {
		detail = append(detail, fmt.Sprintf("%d session record(s) could not be read, and the service removes nothing while one cannot, "+
			"because it might name a directory counted here", rep.Unreadable))
	}
	if rep.Uninspected > 0 {
		detail = append(detail, fmt.Sprintf("%d process(es) look like opencode servers but could not be inspected, so a counted "+
			"directory may be in use (run the doctor as the service user)", rep.Uninspected))
	}
	row.Detail = strings.Join(detail, ". ")
	return row
}

// unknownSessionsListed is how many sessions the row's detail names before it
// says how many more there are. The list is oldest first, so the ones a person
// most likely means to close are the ones shown.
const unknownSessionsListed = 10

// checkOpencodeSessions is the runtime.opencode.sessions row (muster #445, #446): the
// opencode sessions that run no server and that a restart did not or cannot relaunch —
// listed `unknown` after a restart, or `dead` if their server exited since — each with
// its age and the size of the directory it holds.
//
// A restart adopts such a session and lists it `unknown` (spec §7.5): its record and
// its directory — a whole runtime store and package cache — stay until the caller
// that created it closes it. Nothing else reported them: `runtime.opencode.dirs` only
// counts directories no record names, and every one of these has a record. So a
// caller that never comes back leaves hundreds of megabytes each, silently.
//
// It is a warning and nothing more, and it deletes nothing. Reaping stays with the
// caller, because a session listed `unknown` is still a session somebody created and
// may want to read; the row names each id so the caller can close it, and Close is
// what removes the record and the directory. Like every row here it names no path
// and no session title, only ids.
//
// Two classes are counted, and each is decided by the record alone. The create carried
// caller-supplied environment, which this driver never kept, so no restart can relaunch
// it (#445). Or the last start of the service tried to relaunch it and failed, for a
// reason that may pass — the runtime would not start, a sandbox that cannot be enforced
// now — and wrote that onto the record (#446). A session with neither is not counted: a
// stopped service leaves every session without a server, and a restart relaunches it —
// the detail says so. The second class is only as current as the last start, which the
// detail also says.
func checkOpencodeSessions(enabled bool, stateDir string, now time.Time, scan func(string) (opencode.UnknownReport, error)) doctorRow {
	row := doctorRow{ID: "runtime.opencode.sessions", Refs: []int{445, 446}}
	switch {
	case !enabled:
		row.Status = statusSkip
		row.Summary = "FLEET_RUNTIME_OPENCODE is not 1 — there are no opencode sessions to count"
		return row
	case stateDir == "":
		row.Status = statusSkip
		row.Summary = "FLEET_STATE_DIR is not set — opencode sessions are not remembered, so none is left unknown by a restart"
		return row
	}
	rep, err := scan(stateDir)
	switch {
	case errors.Is(err, opencode.ErrScanUnsupported):
		row.Status = statusSkip
		row.Summary = "this platform cannot list running processes with their environment — sessions left unknown by a restart are not counted"
		return row
	case err != nil:
		row.Status = statusUnknown
		row.Summary = "the opencode sessions could not be listed"
		row.Detail = redact(err.Error(), stateDir, "FLEET_STATE_DIR")
		return row
	case len(rep.Sessions) == 0 && rep.Unreadable == 0:
		row.Status = statusPass
		row.Summary = "no opencode session without a server is waiting for its caller to re-create or close it"
		return row
	}
	row.Status = statusWarn
	var detail []string
	if len(rep.Sessions) == 0 {
		row.Summary = fmt.Sprintf("%d opencode session record(s) could not be read, so each is listed unknown", rep.Unreadable)
	} else {
		total, exact := rep.Bytes()
		size := humanSize(total)
		switch {
		case total == 0 && !exact:
			size = "an unmeasured amount"
		case !exact:
			size = "at least " + size
		}
		row.Summary = fmt.Sprintf("%d opencode session(s) run no server and were not relaunched by a restart; they hold %s", len(rep.Sessions), size)
		// The list is oldest first, but a record with no start time sorts before every
		// other; the oldest age worth saying is the first one that is known.
		for _, s := range rep.Sessions {
			if !s.StartedAt.IsZero() {
				row.Summary += fmt.Sprintf(" (the oldest %s)", sessionAge(now, s))
				break
			}
		}
		shown := rep.Sessions
		if len(shown) > unknownSessionsListed {
			shown = shown[:unknownSessionsListed]
		}
		items := make([]string, len(shown))
		for i, s := range shown {
			items[i] = fmt.Sprintf("%s (%s, %s)", s.ID, sessionAge(now, s), sessionSize(s))
			if s.RelaunchFailed {
				items[i] = fmt.Sprintf("%s (%s, %s, relaunch failed)", s.ID, sessionAge(now, s), sessionSize(s))
			}
		}
		list := strings.Join(items, "; ")
		if more := len(rep.Sessions) - len(shown); more > 0 {
			list += fmt.Sprintf("; and %d more", more)
		}
		failed := rep.RelaunchFailedCount()
		detail = append(detail, "oldest first: "+list)
		if callerEnv := len(rep.Sessions) - failed; callerEnv > 0 {
			detail = append(detail, fmt.Sprintf("%s created with caller-supplied environment, which is never kept: a restart lists such "+
				"a session unknown and it needs re-creation (one whose server exited since is listed dead)", countOf(callerEnv, len(rep.Sessions))))
		}
		if failed > 0 {
			detail = append(detail, fmt.Sprintf("%s marked \"relaunch failed\": the last start of the service listed such a session unknown "+
				"because its relaunch failed (the runtime would not start, a sandbox that cannot be enforced, a context file that is gone); the "+
				"listing gives the reason, and every start tries again and takes the mark off the first time one works — so a cause fixed since "+
				"clears this after the next start, not before", countOf(failed, len(rep.Sessions))))
		}
		detail = append(detail, "either way its record and directory stay until its caller closes it. "+
			"Nothing here removes them — a session left unknown is still one somebody created")
	}
	if rep.Unreadable > 0 && len(rep.Sessions) > 0 {
		detail = append(detail, fmt.Sprintf("%d session record(s) could not be read; the service lists each unknown too, but there is nothing to measure", rep.Unreadable))
	}
	if rep.Unreadable > 0 && len(rep.Sessions) == 0 {
		detail = append(detail, "each is listed unknown and has no readable record to measure; closing the session removes it")
	}
	if rep.Uninspected > 0 && len(rep.Sessions) > 0 {
		detail = append(detail, fmt.Sprintf("%d process(es) look like opencode servers but could not be inspected, so a counted session may be running "+
			"(run the doctor as the service user)", rep.Uninspected))
	}
	detail = append(detail, "a session of a stopped service that no start has failed to relaunch is not counted here: a restart relaunches it")
	row.Detail = strings.Join(detail, ". ")
	return row
}

// checkSandboxHost is the sandbox.host row (muster #457): what this host can offer
// as a sandbox, from the same host check the runtime capability reads, so the row and
// `GET /v1/runtimes` cannot disagree. Offline-capable: nothing here leaves the machine.
//
// It is `pass` when the host can enforce the mechanism and `warn` when the platform has
// one and the host refuses it, with the reason the capability carries verbatim in the
// summary and every observation behind it in the detail, so an operator sees the Landlock
// ABI against its floor and the active security modules without reading a kernel file. It
// is never `fail`: an unavailable sandbox only matters to a caller that asks for one, and
// a create that asks is refused with that reason, so a host that offers none is not a
// broken installation. It is `skip` — not applicable — on a platform with no mechanism.
//
// The row names no path: the launcher is reported as named or not.
func checkSandboxHost(rep *sandbox.HostReport) doctorRow {
	row := doctorRow{ID: "sandbox.host", Refs: []int{439, 457}}
	facts := make([]string, 0, len(rep.Facts)+1)
	if rep.Mechanism != "" {
		facts = append(facts, "mechanism: "+rep.Mechanism)
	}
	for _, f := range rep.Facts {
		facts = append(facts, f.Name+": "+f.Value)
	}
	switch {
	case rep.Mechanism == "":
		row.Status = statusSkip
		row.Summary = "n/a — " + rep.Reason
	case rep.Support != nil:
		row.Status = statusPass
		row.Summary = fmt.Sprintf("this host can enforce the sandbox (%s)", rep.Mechanism)
		row.Detail = strings.Join(facts, "; ") + ". This is the host's half: the runtime measures its own agent on top of it " +
			"(muster #345, #456), so `GET /v1/runtimes` can still say `sandboxUnavailable` for a reason about the agent"
	default:
		row.Status = statusWarn
		row.Summary = "this host is not offered the sandbox — " + rep.Reason
		row.Detail = strings.Join(facts, "; ") + ". `GET /v1/runtimes` shows this same reason as `sandboxUnavailable`, " +
			"and a create that asks for a sandbox is refused until it is fixed; a session that asks for none is unaffected"
	}
	return row
}

// countOf says how many of the n counted sessions a sentence is about, so a row of one
// class does not say "all" of a list that has a second one.
func countOf(k, n int) string {
	switch {
	case k == n && n == 1:
		return "it was"
	case k == n:
		return "each was"
	case k == 1:
		return "1 was"
	}
	return fmt.Sprintf("%d were", k)
}

// sessionSize is one session's size as the row writes it.
func sessionSize(s opencode.UnknownSession) string {
	switch {
	case !s.Measured:
		return "size not measured"
	case s.Partial:
		return "at least " + humanSize(s.Bytes)
	}
	return humanSize(s.Bytes)
}

// sessionAge is one session's age as the row writes it: days from two days, hours
// from one, minutes below. A record with no start time has no age, which is said, not
// guessed. A start time ahead of this clock (the clocks differ) reads as no time at all.
func sessionAge(now time.Time, s opencode.UnknownSession) string {
	if s.StartedAt.IsZero() {
		return "age unknown"
	}
	d := now.Sub(s.StartedAt)
	switch {
	case d < time.Hour:
		if d < 0 {
			d = 0
		}
		return fmt.Sprintf("%dm old", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh old", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd old", int(d/(24*time.Hour)))
	}
}

// humanSize writes a byte count the way a person reads a disk: one unit, no
// fraction below a megabyte.
func humanSize(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%d KiB", (n+k-1)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.1f GiB", float64(n)/(k*k*k))
	}
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
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
// probePeerAny probes a peer known by several addresses in the order they are
// tried and reports the first that answers, naming (by position, never by
// value) any before it that did not. An absent address is the expected state of
// a machine on one of its connections, so it is detail, not a warning. When
// none answers the row fails and the second return is the first address.
// peerCred is what a probe presents to a peer: a bearer token, or a signature
// made with this machine's key (muster #350).
type peerCred struct {
	bearer string
	signer *peerkey.Signer
	peer   fleet.MachineId
}

func (c peerCred) empty() bool { return c.signer == nil && c.bearer == "" }

func (c peerCred) apply(req *http.Request) error {
	if c.signer != nil {
		return c.signer.Sign(req, nil, string(c.peer))
	}
	req.Header.Set("Authorization", "Bearer "+c.bearer)
	return nil
}

func probePeerAny(ctx context.Context, env doctorEnv, id string, bases []string, credential peerCred) (doctorRow, string) {
	if len(bases) == 1 {
		return probePeer(ctx, env, id, strings.TrimRight(bases[0], "/"), credential), bases[0]
	}
	var missed []string
	for i, b := range bases {
		row := probePeer(ctx, env, id, strings.TrimRight(b, "/"), credential)
		if strings.HasPrefix(row.Summary, "unreachable:") {
			missed = append(missed, fmt.Sprintf("address %d: %s", i+1, strings.TrimPrefix(row.Summary, "unreachable: ")))
			continue
		}
		row.Refs = append(row.Refs, 335)
		row.Summary = fmt.Sprintf("address %d of %d: %s", i+1, len(bases), row.Summary)
		if len(missed) > 0 {
			row.Detail = strings.TrimSpace(row.Detail + " " + "not answering before it — " + strings.Join(missed, "; "))
		}
		return row, b
	}
	return doctorRow{ID: id, Status: statusFail, Refs: []int{98, 335},
		Summary: fmt.Sprintf("unreachable on all %d addresses", len(bases)),
		Detail:  strings.Join(missed, "; ") + " — the bind address or the network path is wrong on every one — not the credential"}, bases[0]
}

func probePeer(ctx context.Context, env doctorEnv, id, base string, credential peerCred) doctorRow {
	row := doctorRow{ID: id, Refs: []int{98}}
	ctx, cancel := context.WithTimeout(ctx, env.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/health", nil)
	if err != nil {
		row.Status = statusFail
		row.Summary = "request could not be built from the configured address"
		return row
	}
	if err := credential.apply(req); err != nil {
		row.Status = statusFail
		row.Summary = "could not sign the request: " + err.Error()
		return row
	}
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
		if credential.signer != nil {
			row.Detail = "the peer does not list this machine's public key as a principal (muster key show; muster principal add <this-machine> --public-key=...), " +
				"or its clock differs from this one by more than a minute, or that principal lacks read"
		}
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
func probePeerGrants(ctx context.Context, env doctorEnv, id, base string, credential peerCred, self, peer fleet.MachineId) doctorRow {
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
	if err := credential.apply(req); err != nil {
		row.Status = statusUnknown
		row.Summary = "not read — could not sign the request: " + err.Error()
		return row
	}
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
	case isConnRefused(err):
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

// validateModelsConfig judges the "models" object of the config file (muster
// #400): which runtime a list may be declared for, and what an id may be.
//
// Only the Claude Code runtime takes a declared list. The opencode runtime
// reports what its own runtime lists, and a declared list there would be a
// second source for an answer that has one, drifting from what a create accepts.
// An id is a value handed to the CLI's model flag, so it follows the rule the
// driver applies where it builds that argv: non-empty, not starting with "-",
// and, because a client offers these as choices, no whitespace and no repeats.
func validateModelsConfig(models map[string][]string) error {
	runtimes := make([]string, 0, len(models))
	for rt := range models {
		runtimes = append(runtimes, rt)
	}
	sort.Strings(runtimes)
	for _, rt := range runtimes {
		switch fleet.RuntimeId(rt) {
		case tmuxdriver.DefaultRuntime:
		case opencode.DefaultRuntime:
			return fmt.Errorf("models.%s: the %s runtime reports the models its own runtime lists; "+
				"a declared list would be a second source for the same answer", rt, rt)
		default:
			return fmt.Errorf("models.%s: %q has no declared model list (only %q takes one)", rt, rt, string(tmuxdriver.DefaultRuntime))
		}
		seen := map[string]bool{}
		for _, id := range models[rt] {
			switch {
			case id == "" || strings.HasPrefix(id, "-"):
				return fmt.Errorf("models.%s: %q is not a model id (it is empty or starts with \"-\")", rt, id)
			case strings.ContainsAny(id, " \t\r\n"):
				return fmt.Errorf("models.%s: %q is not a model id (it contains whitespace)", rt, id)
			case seen[id]:
				return fmt.Errorf("models.%s: %q is listed more than once", rt, id)
			}
			seen[id] = true
		}
	}
	return nil
}
