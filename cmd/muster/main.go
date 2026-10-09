// Command muster runs the machine-local muster service.
//
// # Configuration
//
// Everything is environment-driven, because the operational facts — which
// address, which port, which peers — are machine-specific and do not belong
// in a public repository.
//
//	FLEET_MACHINE          this machine's id in the fleet. Required in
//	                       practice; defaults to "local", which is only
//	                       useful for a single-machine trial.
//	FLEET_TOKEN            shared bearer token. Required. No default, no
//	                       unauthenticated mode (api-http.md §5).
//	FLEET_ADDR             listen address, or a comma-separated list of
//	                       them. Defaults to loopback on an ephemeral port
//	                       (§6.1: exposure beyond loopback is explicit
//	                       configuration, never a side effect). Bind a
//	                       specific interface, never 0.0.0.0. Loopback is
//	                       added automatically on the same port whenever it
//	                       is not already covered — a service is not
//	                       diagnosable if it can be cut off from its own
//	                       machine by the failure being diagnosed.
//	FLEET_RUNTIME          "tmux" (default) or "stub".
//	FLEET_TMUX_BIN         path to the multiplexer binary. Defaults to
//	                       "tmux" on PATH — which a non-interactive ssh
//	                       session may not have.
//	FLEET_RUNTIME_OPENCODE set to 1 to ALSO register a second local driver
//	                       (muster issue #55) over opencode
//	                       (github.com/sst/opencode), a third-party agent
//	                       this process spawns as a subprocess and talks
//	                       to over HTTP. Additive, not a replacement for
//	                       FLEET_RUNTIME — a machine may run both at once,
//	                       which is the whole proof #55 exists to make.
//	                       Absence of the opencode binary on this machine
//	                       is logged and skipped, never fatal: this is an
//	                       optional runtime, unlike FLEET_RUNTIME's tmux.
//	FLEET_OPENCODE_BIN     path to the opencode binary. Defaults to
//	                       "opencode" on PATH.
//	FLEET_PEERS            comma-separated name=url list, e.g.
//	                       "other=https://other.example:PORT". Peers are
//	                       statically configured; there is no discovery
//	                       (§7.2), and the address is one the OPERATOR has
//	                       confirmed reachable from THIS machine — never the
//	                       peer's own idea of its name.
//	FLEET_STATE_DIR        directory for durable state: idempotency keys
//	                       (§10) and the event sequence (§7.3). Absent means
//	                       in-memory only, which is honest for a throwaway
//	                       instance and is the defect D5 described for a real
//	                       one. Created if missing, mode 0700.
//	FLEET_CONFIG           path to a JSON file carrying the principal table,
//	                       per-peer credentials (§6), this machine's
//	                       defaultRuntime (§60: the bare-id tiebreak once a
//	                       second local driver is registered — absent means
//	                       bare-id addressing among more than one stays
//	                       refused, the older behaviour), and this machine's
//	                       sessionEnv (muster issue #94: an identity
//	                       declared for every session this machine starts,
//	                       rather than every caller having to pass it on
//	                       every create forever). When present the
//	                       principal table is authoritative and FLEET_TOKEN /
//	                       FLEET_ALLOW_* are ignored. Absent means
//	                       single-token mode.
//
//	                       muster #98: a table-only deployment (no
//	                       FLEET_TOKEN) has nothing to present for its OWN
//	                       long-lived peer reads unless the table names a
//	                       principal for this machine's system identity —
//	                       "system:"+FLEET_MACHINE — and gives it a token.
//	                       Without that entry, peer subscriptions are
//	                       refused (fail closed); local requests are
//	                       unaffected either way.
//
//	                       This is a TWO-SIDED requirement, and only the local
//	                       half is described above. The token from that entry
//	                       is presented to the PEER as a bearer credential, and
//	                       the peer resolves it by comparing the TOKEN VALUE
//	                       against its own principal table (constant-time,
//	                       §6) — the peer never looks at the name "system:"+
//	                       FLEET_MACHINE at all, so a name mismatch on the far
//	                       side is harmless and a token mismatch is fatal. So
//	                       the SAME token value has to appear in both tables:
//	                       locally under "system:"+FLEET_MACHINE (so this
//	                       daemon can find it and offer it), and on the peer
//	                       under some principal holding at least the read
//	                       grant (GrantRead — GET /v1/events is gated by
//	                       reading(), muster #80) — grants beyond that
//	                       are unneeded for a subscription and are the
//	                       peer-side operator's call, same as any other
//	                       principal. Configuring only the local half leaves
//	                       every peer subscription refused exactly as before
//	                       #98's fix, with a doc that reads as if it should
//	                       already work.
//	FLEET_ALLOW_MUTATIONS  set to 1 to permit create/input/interrupt/close
//	                       against sessions ON THIS MACHINE. Defaults OFF.
//	FLEET_ALLOW_RELAY      set to 1 to permit forwarding a mutation to a
//	                       PEER. Defaults OFF. Separate from the above on
//	                       purpose: a hardened host can still be a
//	                       full-featured client (§6, defect D6).
//	FLEET_TRUST_ROOTS      comma-separated list of absolute directories.
//	                       #47: every repository and worktree root under
//	                       one of these has the runtime's own folder-trust
//	                       question pre-answered, so no session under it —
//	                       whoever started it — ever meets that screen.
//	                       #211: the same roots pre-answer its second boot
//	                       question, "allow external imports", which a
//	                       directory raises when its instruction files
//	                       import a file from outside it. One list, one
//	                       statement: there is no separate setting.
//	                       Absent means the feature does nothing; FLEET_
//	                       CONFIG's own trustRoots is read when this is
//	                       unset, same precedence as FLEET_PEERS below.
//	FLEET_TRUST_STATE_PATH overrides where the above is written — the
//	                       runtime's own state file. Defaults to
//	                       ~/.claude.json, the same file FLEET_CREDENTIAL_
//	                       PATH already points at by default.
//	FLEET_TRUST_SEED_INTERVAL
//	                       how often the above re-scans for a worktree
//	                       created since the last pass. Defaults to 2m;
//	                       Go duration syntax.
//	FLEET_INBOX_INDEX      directory an operator points at muster #119's
//	                       delivery path (tmux.WithInboxResolver). Absent
//	                       means the feature does nothing — #122 found it
//	                       shipped and deployed with nothing ever calling
//	                       this option, so every delivery fell through to
//	                       the pane path silently. The directory holds one
//	                       JSON file per numeric process id
//	                       ("<pid>.json": network, socket, token_path,
//	                       started_at, mode_class) — a shape this repository
//	                       defines for itself, never the real third-party
//	                       address convention (see
//	                       cmd/muster/inboxresolver.go). GET
//	                       /v1/runtimes reports whether this wiring is live
//	                       as deliversToInbox, so an operator never has to
//	                       infer it from a receipt's wording.
//	                       muster #148: mode_class names the permission-
//	                       mode class the target session RUNS IN, and without
//	                       it a send cannot be attested and falls back to the
//	                       pane path — so an index whose writer does not yet
//	                       emit it leaves deliversToInbox reading true while
//	                       nothing actually uses the inbox. That is the
//	                       intended day-one state, not a fault; an
//	                       unrecognised value is rejected outright rather
//	                       than guessed at, because the receiving runtime
//	                       holds a wrong class as firmly as a missing one.
//	                       muster #163: setting this also adds the
//	                       resolver's inbox_index.* counters to the terminal
//	                       runtime's counters in GET /v1/health.
//	                       muster #196: setting this REQUIRES a principal
//	                       table (FLEET_CONFIG) on the tmux runtime — the
//	                       service refuses to start otherwise, because who
//	                       relays a person's messages must be a grant, not a
//	                       header (cmd/muster/inboxgate.go).
//	FLEET_DELIVERY_MODULES ordered, comma-separated names of OPTIONAL external
//	                       delivery modules to enable (muster #185), in order
//	                       of preference. Empty or unset means none, and this
//	                       daemon behaves exactly as it did before they existed.
//	                       Set the same value in every machine's service
//	                       environment for a fleet-wide choice; override it in one
//	                       machine's own for a per-machine one. A name whose
//	                       executable is absent is logged once and skipped: a
//	                       machine that could not fetch a module is a supported
//	                       state.
//	FLEET_MODULES_DIR      where module executables live, one file per module named
//	                       by the module. Defaults to
//	                       <prefix>/libexec/muster/modules, <prefix> being the
//	                       parent of this binary's directory after symlinks
//	                       resolve — set it explicitly when the binary is reached
//	                       through a symlink.
//	FLEET_DELIVERY_MODULE_ENV
//	                       comma-separated environment names forwarded to a module
//	                       child; nothing else is, beyond PATH HOME USER LANG
//	                       TMPDIR FLEET_STATE_DIR, and a FLEET_ name never is.
//	FLEET_CAPTURE_LINES    how many lines of each pane this driver captures
//	                       to classify it. Absent, or not a positive
//	                       integer, means the built-in default is used and
//	                       this option is never called. muster #148:
//	                       the default was effectively hard-coded — the
//	                       option existed with no caller — and a composer
//	                       pushed past the capture window by accumulated
//	                       notices becomes unreadable, at which point the
//	                       driver correctly refuses to act on what it cannot
//	                       see (#134) and an operator's only exit was to
//	                       attach to the multiplexer by hand. This is the
//	                       lever that was missing. muster #169: a
//	                       composer is now read only from the visible
//	                       pane, so a wider margin no longer makes a tall
//	                       composer readable; it widens the transcript
//	                       context above the pane for the rest of the
//	                       classification.
//
// # Subcommands
//
// Operator subcommands run and exit without starting the service:
//
//	muster principal add|list|grant|revoke   enrol a client, change its grants (enrol.go, grants.go)
//	muster doctor [--json]      read-only check that this installation is
//	                                  complete — token, config, grants, state
//	                                  directory, inbox index, peers (doctor.go,
//	                                  muster #160). Run it under the service
//	                                  unit's environment.
//	muster compat --claude PATH check a candidate build of the agent runtime
//	                                  against the assumptions this service makes
//	                                  about it, in an isolated multiplexer
//	                                  server, and print a versioned report
//	                                  (compat.go, docs/compat.md, muster
//	                                  #183). Reports only; never installs,
//	                                  pins or promotes a build.
//	muster serve                      start the service
//	muster --version                  print the release tag (when stamped) and the build, exit 0
//	muster -h | --help          print usage and exit 0
//
// Any other argument is a usage error (exit 2) and starts nothing
// (muster #177). The service takes no further arguments: it is started with
// `serve` alone, and configured entirely by its environment. A bare `muster`
// is a usage error too; only the legacy name (below) still starts bare.
//
// # Legacy name
//
// The service was released as colab-fleetd and started with no arguments.
// When the binary is invoked under that name with no arguments it behaves as
// `muster serve`, so an installed unit or symlink keeps working while a
// machine moves to the new name. This compatibility is removed in the release
// after every machine has moved.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/drivers/opencode"
	"github.com/futurelastic/muster/internal/drivers/remote"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/drivers/tmux"
	"github.com/futurelastic/muster/internal/service"
	"github.com/futurelastic/muster/internal/state"
)

func main() {
	args := os.Args[1:]
	if legacyBare(os.Args) {
		args = []string{"serve"}
	}
	// Operator subcommands run and exit — they never start a service. Handled
	// before anything else so that enrolling a principal does not require the
	// environment a running instance needs.
	//
	// doctor (muster #160) is read-only and never needs the environment
	// to be complete either — reporting that it is not is its whole job.
	if handled, code := runDoctor(args, os.Getenv, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	// compat (muster #183) checks a candidate runtime build in a private
	// multiplexer server. It never starts the service and never touches the
	// service's own sessions, so like doctor it needs none of its environment.
	if handled, code := runCompat(args, os.Getenv, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if handled, err := runPrincipal(args); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	// Anything else on the command line is refused here, before any startup
	// work (muster #177). The service is configured by its environment
	// alone, so the only way to start it is with no arguments at all; a
	// typo or a help flag used to fall through and count as "start", which
	// from an operator's shell means starting as the installed service.
	if handled, code := runUsage(args, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}

	self := fleet.MachineId(getenv("FLEET_MACHINE", "local"))

	// Logged before anything else can fail, so a crash report says which
	// code crashed. See fleet.Build.
	selfBuild := fleet.SelfBuild()
	log.Printf("muster: build %s (%s)", selfBuild.Short(), selfBuild.Go)

	// A principal table, when configured, decides everything about who may
	// do what (§6). Without one the service runs in single-token mode,
	// which is honest for one machine and unstatable for a fleet.
	//
	// Loaded BEFORE the token gate below (muster #95): the doc comment
	// on FLEET_CONFIG promises that a configured principal table makes
	// FLEET_TOKEN irrelevant, but a fatal token check that runs first can
	// never see that table, so it refused a deployment the documentation
	// said was fine. loadConfig itself is already a closed gate — it fatals
	// on a file that cannot be read, cannot be parsed, or names no
	// principals at all (config.go) — so reaching the token check below
	// with cfgFile != nil means a non-empty, validated principal table is
	// in hand, not merely a path that was set.
	var cfgFile *fileConfig
	if path := os.Getenv("FLEET_CONFIG"); path != "" {
		var err error
		cfgFile, err = loadConfig(path)
		if err != nil {
			log.Fatalf("muster: %v", err)
		}
	}

	// There is no unauthenticated mode (api-http.md §5) — not for
	// loopback, not for development. A missing token is a refusal to
	// start UNLESS the principal table loaded above already decides who
	// may do what, per FLEET_CONFIG's own doc comment. requireToken is a
	// pure function precisely so this gate has a test that does not have
	// to invoke main()'s os.Exit path.
	token := os.Getenv("FLEET_TOKEN")
	if requireToken(cfgFile) && token == "" {
		log.Fatal("muster: FLEET_TOKEN must be set — there is no unauthenticated mode (api-http.md §5)")
	}

	// Durable state (§7.3, §10, §12). Nil is a valid configuration; a
	// configured directory that cannot be opened is not, because starting
	// anyway would silently be the in-memory behaviour this replaces.
	store, err := state.Open(os.Getenv("FLEET_STATE_DIR"))
	if err != nil {
		log.Fatalf("muster: %v", err)
	}
	if store != nil {
		log.Printf("muster: state directory %s", store.Dir())
	}

	svc, err := service.NewWithState(self, store)
	if err != nil {
		log.Fatalf("muster: %v", err)
	}
	// The service's own authority for long-lived peer reads (event
	// subscriptions, §14 D9). Never used for a proxied unary call — those
	// authenticate as this machine and assert the caller (§6, §13).
	svc.SetPeerCredential(peerCredential(token, cfgFile, self))

	// --- local runtime -------------------------------------------------
	var (
		localDriver driver.Driver
		runtimeID   fleet.RuntimeId
		// #185: the optional external delivery modules this machine enabled.
		// The zero value (nothing enabled) is the default and changes nothing.
		deliveryModules deliveryModuleSetup
	)
	switch name := getenv("FLEET_RUNTIME", "tmux"); name {
	case "tmux":
		// FLEET_TMUX_BIN exists because a non-interactive ssh session gets a
		// bare PATH (/usr/bin:/bin:/usr/sbin:/sbin on the machines this was
		// deployed to), and the multiplexer lives in a package manager's
		// prefix that differs by architecture. Relying on PATH resolution
		// here fails only under remote invocation, which is precisely how
		// this service gets deployed.
		opts := []tmux.Option{tmux.WithState(store)}
		// #272: opt in to requiring the remote-control (or human-relay) grant
		// for /rc and /remote-control typed through `input`. Off by default for
		// one release; see fileConfig.GateRemoteControlInput.
		if cfgFile != nil && cfgFile.GateRemoteControlInput {
			opts = append(opts, tmux.WithRemoteControlInputGate(true))
			log.Printf("muster: input gate on — /rc and /remote-control need the remote-control or human-relay grant (#272)")
		}
		if bin := os.Getenv("FLEET_TMUX_BIN"); bin != "" {
			opts = append(opts, tmux.WithBinary(bin))
		}
		// Where the runtime keeps its own record of each conversation. The
		// driver defaults this to OFF so that constructing one never reads a
		// real store; enabling it is a deployment decision, taken here.
		//
		// FLEET_RECORD_ROOT overrides the location, and setting it empty
		// turns the lookup off — at which point every session reports the
		// absent field, meaning nobody looked, rather than claiming no record
		// exists.
		if root, ok := os.LookupEnv("FLEET_RECORD_ROOT"); ok {
			opts = append(opts, tmux.WithRecordRoot(root))
		} else if home, err := os.UserHomeDir(); err == nil {
			opts = append(opts, tmux.WithRecordRoot(filepath.Join(home, ".claude", "projects")))
		}
		// Terminal path v2 (item c / D6) and #182: the runtime's per-process
		// identity directory. It names the conversation each running process
		// is in, which is how a session is identified that the record-root
		// lookup above cannot answer by name — a resumed session, or one
		// created without a name — and how a delivery is confirmed against the
		// right transcript. Same override/off pattern as FLEET_RECORD_ROOT
		// immediately above — FLEET_PROCESS_SESSIONS_ROOT set empty turns it
		// off, and every session then resolves by name alone.
		if root, ok := os.LookupEnv("FLEET_PROCESS_SESSIONS_ROOT"); ok {
			opts = append(opts, tmux.WithProcessSessionsRoot(root))
		} else if home, err := os.UserHomeDir(); err == nil {
			opts = append(opts, tmux.WithProcessSessionsRoot(filepath.Join(home, ".claude", "sessions")))
		}
		// Where the runtime keeps its own local credential material —
		// stat'ed, never read, to answer #12 (SessionState.CredentialGeneration,
		// EventMachineAccount). Same off-by-default reasoning as
		// FLEET_RECORD_ROOT above: a driver built for a test must not go
		// stat'ing a real file merely because it was constructed.
		//
		// FLEET_CREDENTIAL_PATH overrides the location, and setting it empty
		// turns the feature off — every session then reports the field
		// absent rather than a guessed value.
		if path, ok := os.LookupEnv("FLEET_CREDENTIAL_PATH"); ok {
			opts = append(opts, tmux.WithCredentialPath(path))
		} else if home, err := os.UserHomeDir(); err == nil {
			opts = append(opts, tmux.WithCredentialPath(filepath.Join(home, ".claude.json")))
		}
		// #47: pre-answer the runtime's folder-trust question — and, since
		// #211, its external-imports question — for every
		// session under a configured root, whoever started it — see
		// internal/trustseed and tmux.WithTrustSeed. Off by default, the
		// same way every feature above it is: no roots configured means no
		// state file is ever opened.
		//
		// FLEET_TRUST_ROOTS is a comma list, same shape as FLEET_PEERS
		// below; FLEET_CONFIG's own trustRoots is read when the env var is
		// unset, same precedence as peers. Neither ever names a real path
		// in this repository — both are machine-local configuration an
		// operator supplies, exactly like FLEET_PEERS' addresses.
		trustRoots := splitList(os.Getenv("FLEET_TRUST_ROOTS"))
		if cfgFile != nil && len(trustRoots) == 0 {
			trustRoots = cfgFile.TrustRoots
		}
		for _, r := range trustRoots {
			if !filepath.IsAbs(r) {
				log.Fatalf("muster: trust root %q must be an absolute path", r)
			}
		}
		if len(trustRoots) > 0 {
			trustStatePath := os.Getenv("FLEET_TRUST_STATE_PATH")
			home, homeErr := os.UserHomeDir()
			if trustStatePath == "" {
				if homeErr != nil {
					log.Fatalf("muster: trust roots are configured but the home directory "+
						"could not be determined (needed for the state file path and the "+
						"never-seed-home guard): %v", homeErr)
				}
				trustStatePath = filepath.Join(home, ".claude.json")
			}
			if homeErr != nil {
				log.Fatalf("muster: trust roots are configured but the home directory "+
					"could not be determined (needed for the never-seed-home guard): %v", homeErr)
			}
			opts = append(opts, tmux.WithTrustSeed(trustStatePath, home, trustRoots))
			log.Printf("muster: trust-seed configured for %d root(s)", len(trustRoots))
		}
		// muster issue #94: an identity this machine's sessions carry,
		// declared once here instead of every caller passing the same
		// variable on every create forever. Config-file-only, like
		// TrustRoots and DefaultRuntime above — which credential a
		// machine's sessions should hold is a fact about this machine, not
		// the fleet. Validated once at startup (ValidateSessionEnv) for the
		// same reason DefaultRuntime is: a typo here should be a message an
		// operator reads once, not a refusal every later caller meets.
		if cfgFile != nil && len(cfgFile.SessionEnv) > 0 {
			entries := cfgFile.sessionEnv()
			if err := tmux.ValidateSessionEnv(entries); err != nil {
				log.Fatalf("muster: %v", err)
			}
			opts = append(opts, tmux.WithSessionEnv(entries))
			log.Printf("muster: sessionEnv configured for %d entry(ies) (#94)", len(entries))
		}
		// muster #122: #119's inbox delivery path only engages once a
		// resolver is actually wired — verified live after #119's own
		// deploy that nothing did this. Off by default like every option
		// above it: an absent FLEET_INBOX_INDEX means WithInboxResolver is
		// never called and this driver behaves exactly as it did before
		// #119 existed. See inboxresolver.go for what the directory holds
		// and why its shape is not the real runtime's own convention.
		if dir := os.Getenv("FLEET_INBOX_INDEX"); dir != "" {
			// muster #196 (ruled on #195): the inbox route is refused
			// without a principal table, because who relays a person's
			// messages has to be a grant and not a header. A refusal to start,
			// like the token gate above — see inboxgate.go. Placed before the
			// resolver is wired so nothing half-configured is left behind.
			if err := requireTableForInbox(dir, cfgFile != nil); err != nil {
				log.Fatalf("muster: %v", err)
			}
			opts = append(opts, tmux.WithInboxResolver(newFileInboxResolver(dir)))
			// muster #163: the resolver's own index counters join the
			// terminal driver's counters in GET /v1/health. Wired only here,
			// beside the resolver, so a machine with no index reports no
			// inbox_index.* name at all rather than a zero it never measured.
			opts = append(opts, tmux.WithCounterSource(inboxResolverCounters))
			// The path itself is not logged — same discipline FLEET_TRUST_ROOTS
			// and FLEET_CREDENTIAL_PATH already follow above: this process's
			// own stdout is not the place machine-local filesystem layout
			// belongs, committed repo or not.
			log.Print("muster: inbox delivery configured (#119, #122)")
		}
		// muster #148: the capture window had no caller, so its default
		// was hard-coded in practice. A pane whose composer has been pushed
		// past the window cannot be classified, #134's guard then correctly
		// refuses to send a key it cannot reason about, and the documented
		// way out ("wait for the composer to shrink") never arrives on an
		// unattended session. This lever keeps a composer from crossing the
		// window; it does not recover one already past it — #149 found no
		// in-driver proof that would, and the refusal now says so. Off by
		// default — an absent or non-positive value calls nothing and leaves
		// the built-in default in place.
		if raw := os.Getenv("FLEET_CAPTURE_LINES"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				log.Printf("muster: ignoring FLEET_CAPTURE_LINES=%q (want a positive integer)", raw)
			} else {
				opts = append(opts, tmux.WithCaptureLines(n))
				log.Printf("muster: pane capture window set to %d lines (#148)", n)
			}
		}
		// #185: optional external delivery modules. Off unless
		// FLEET_DELIVERY_MODULES names one; resolved here, started after
		// reconciliation below, so a slow module never delays startup.
		exe, _ := os.Executable()
		deliveryModules = loadDeliveryModules(os.Getenv, exe, os.Getenv("FLEET_STATE_DIR"))
		for _, line := range deliveryModules.describe() {
			log.Print("muster: " + line)
		}
		if len(deliveryModules.Enabled) > 0 {
			deliveryModules.Config.Logf = log.Printf
			opts = append(opts, tmux.WithDeliveryModules(deliveryModules.Config))
		}
		d := tmux.New(self, opts...)
		// #180: this machine's own sessionEnv may not name a variable the
		// delivery module reserves either — the module is the sole setter.
		if err := d.ValidateSessionEnvReserved(); err != nil {
			log.Fatalf("muster: %v", err)
		}
		// An unreadable key table is surfaced, never absorbed: continuing
		// with an empty one is exactly the behaviour §10 calls a disaster.
		if err := d.StateError(); err != nil {
			log.Fatalf("muster: %v", err)
		}
		// Startup is reconciliation for trust seeding too: a worktree that
		// existed before this process started should not have to wait for
		// the first interval tick, any more than the reconciliation block
		// below waits to report what it found.
		if got, err := d.SeedTrustRoots(); err != nil {
			log.Printf("muster: trust-seed: %v", err)
		} else if got.Islands > 0 {
			log.Printf("muster: trust-seed: startup pass — %s", got)
		}
		if len(trustRoots) > 0 {
			go runTrustSeedLoop(d, trustSeedInterval())
		}
		localDriver, runtimeID = d, tmux.DefaultRuntime
	case "stub":
		localDriver, runtimeID = &stub.Driver{DeadlineMs: 5000}, "stub"
	default:
		log.Fatalf("muster: unknown FLEET_RUNTIME %q (want tmux or stub)", name)
	}
	if err := svc.RegisterLocalDriver(runtimeID, localDriver); err != nil {
		log.Fatalf("muster: registering local runtime: %v", err)
	}

	// --- second local driver (muster issue #55) --------------------
	//
	// Additive and optional, unlike the switch above: this machine may run
	// tmux (or stub) AND opencode at once, which is the entire proof #55
	// exists to make — a second local driver, registered alongside the
	// first, against a genuinely different runtime.
	//
	// A missing opencode binary is logged and skipped, never fatal
	// (log.Fatalf above, for FLEET_RUNTIME, is deliberately NOT mirrored
	// here). tmux is this daemon's baseline runtime, present at every
	// deployment; opencode is optional and additive, so its absence must
	// not take the whole fleet daemon down. This is opencode.New /
	// opencode.Probe's own "absent install is a first-class answer, not a
	// startup crash" contract, discharged at the one place that decides
	// what to do about it.
	var opencodeDriver *opencode.Driver
	if getenv("FLEET_RUNTIME_OPENCODE", "") == "1" {
		bin := os.Getenv("FLEET_OPENCODE_BIN")
		d, err := opencode.New(context.Background(), self, opencode.WithBinary(bin))
		if err != nil {
			log.Printf("muster: opencode runtime not started, continuing without it: %v", err)
		} else if err := svc.RegisterLocalDriver(opencode.DefaultRuntime, d); err != nil {
			log.Printf("muster: registering opencode runtime: %v", err)
			_ = d.Shutdown()
		} else {
			opencodeDriver = d
			log.Printf("muster: opencode runtime registered (#55)")
		}
	}

	// The bare-id tiebreak once a second local driver is registered
	// (muster issue #60, ⚖ ruling). Config-file-only, like TrustRoots
	// and Peers: which runtimes exist is a fact about this machine, not the
	// fleet, and belongs in the file an operator already edits per machine.
	//
	// Validated HERE, against every local driver this instance will ever
	// register, so a typo fails startup once (guardrail 1) instead of
	// turning every ambiguous bare-id call into a `not_found` that reads
	// exactly like sessions having disappeared.
	if cfgFile != nil && cfgFile.DefaultRuntime != "" {
		if err := svc.SetDefaultRuntime(fleet.RuntimeId(cfgFile.DefaultRuntime)); err != nil {
			log.Fatalf("muster: %v", err)
		}
		log.Printf("muster: default runtime %q configured for bare-id resolution (§60)", cfgFile.DefaultRuntime)
	}

	// The `prompt` (create) / `text` (input) length limit, made a machine
	// setting by muster #130 instead of a compiled-in constant.
	// Config-file-only and validated here for the same reason DefaultRuntime
	// is just above: a typo or a meaningless value should fail startup once,
	// not turn into a confusing refusal on the first caller that hits it.
	// Zero (absent from the file) is left alone deliberately — Service
	// already starts with the shipped default in force, and #130 requires
	// an unconfigured deployment to behave exactly as it did before this
	// setting existed.
	if cfgFile != nil && cfgFile.MaxInputBytes != 0 {
		if err := svc.SetMaxInputBytes(cfgFile.MaxInputBytes); err != nil {
			log.Fatalf("muster: %v", err)
		}
		log.Printf("muster: input length limit %d bytes configured (#130)", cfgFile.MaxInputBytes)
	}
	// #185: the names a caller may force with `route`. The service checks them
	// in its request-shape switch, before any driver is resolved, so this is
	// set whether or not the module's executable is present.
	if err := svc.SetDeliveryModuleRoutes(deliveryModules.Enabled); err != nil {
		log.Fatalf("muster: %v", err)
	}
	if cfgFile != nil && cfgFile.ClosedRetentionDays != 0 {
		if cfgFile.ClosedRetentionDays < 0 {
			log.Fatalf("muster: closedRetentionDays must be positive, got %d", cfgFile.ClosedRetentionDays)
		}
		svc.SetClosedRetention(time.Duration(cfgFile.ClosedRetentionDays) * 24 * time.Hour)
		log.Printf("muster: closed-session records kept %d days (#179)", cfgFile.ClosedRetentionDays)
	}

	// --- peers ---------------------------------------------------------
	//
	// A peer that is unreachable right now is still a configured peer:
	// §5.7 requires it to surface as a source reporting unreachable, not to
	// vanish from the fleet because it happened to be down at startup. So
	// registration never probes and never fails on reachability.
	peerSpecs := splitList(os.Getenv("FLEET_PEERS"))
	if cfgFile != nil && len(peerSpecs) == 0 {
		for _, p := range cfgFile.Peers {
			peerSpecs = append(peerSpecs, p.Machine+"="+p.URL)
		}
	}
	for _, spec := range peerSpecs {
		name, base, ok := strings.Cut(spec, "=")
		if !ok || name == "" || base == "" {
			log.Fatalf("muster: bad FLEET_PEERS entry %q (want name=url)", spec)
		}
		machine := fleet.MachineId(strings.TrimSpace(name))
		if machine == self {
			log.Fatalf("muster: peer %q is this machine; fan-out is one hop and peers never recurse (§13.1)", machine)
		}
		// No credential is handed to the peer driver, and none exists to
		// hand: it presents the authority of whoever made the request
		// (§13). A proxy holding its own identity is the confused deputy
		// this design forbids.
		// WithSelf lets the probe ask the peer whether it lists this machine
		// back (muster #154).
		opts := []remote.Option{remote.WithDeadline(3 * time.Second), remote.WithSelf(self)}
		// The credential THIS machine holds on that peer. Distinct from
		// anything a caller presents here, and distinct from the peer's
		// own credential on us — conflating those is how a fleet ends up
		// with one shared secret again by accident.
		if cfgFile != nil {
			if _, tok, ok := cfgFile.peerFor(machine); ok && tok != "" {
				opts = append(opts, remote.WithIdentity(tok))
			}
		}
		peer := remote.New(machine, strings.TrimSpace(base), opts...)
		if err := svc.RegisterPeerDriver(machine, peer); err != nil {
			log.Fatalf("muster: registering peer %q: %v", machine, err)
		}
		log.Printf("muster: peer %s configured", machine)

		// Learn the peer's declared deadline so this driver does not
		// abandon calls the peer would have completed (§14 D7). Best
		// effort: a peer that is down stays configured (§5.7), and the
		// driver falls back to its floor until the peer answers.
		go func(p *remote.Driver, m fleet.MachineId) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := p.RefreshCapabilities(ctx, fleet.Request{
				Caller: fleet.Caller{Principal: "system:self", Credential: token},
			}); err != nil {
				log.Printf("muster: peer %s capabilities unknown for now: %v", m, err)
				return
			}
			log.Printf("muster: peer %s deadline learned: %dms",
				m, p.Capabilities().DeadlineMs)

			// Version skew, said out loud. Two machines ran different
			// builds silently, and the older one still had a bug the
			// newer had fixed — the symptom looked like a defect in code
			// that no longer existed. SameAs deliberately refuses to call
			// unknown or dirty builds equal, so this warns in the cases
			// where a comparison cannot be trusted rather than staying
			// quiet about them.
			if why := selfBuild.DifferenceFrom(p.Build()); why != "" {
				log.Printf("muster: NOTE peer %s build %s vs ours %s — %s; "+
					"a disagreement between these two may be skew rather than a bug",
					m, p.Build().Short(), selfBuild.Short(), why)
			}
		}(peer, machine)
	}

	// Two independent grants (§6, and defect D6 for why one was not enough):
	// what this HOST exposes, and what this instance may do as a CLIENT.
	allowLocal := os.Getenv("FLEET_ALLOW_MUTATIONS") == "1"
	allowRelay := os.Getenv("FLEET_ALLOW_RELAY") == "1"
	log.Printf("muster: local mutations=%v · relay to peers=%v (§6; both default off)",
		allowLocal, allowRelay)
	svcCfg := service.Config{
		Token:               token,
		AllowLocalMutations: allowLocal,
		AllowPeerRelay:      allowRelay,
	}
	if cfgFile != nil {
		principals, err := cfgFile.principals()
		if err != nil {
			log.Fatalf("muster: %v", err)
		}
		svcCfg.Principals = principals
		for _, p := range principals {
			log.Printf("muster: principal %q grants=%v", p.Name, p.Grants)
		}
	}
	mux := service.NewMux(svc, svcCfg)

	// --- reconciliation (§12) ------------------------------------------
	//
	// Startup is reconciliation, not initialisation: sessions outlive the
	// service that manages them. Nothing is destroyed here, and nothing
	// can be — this only reports what was found, which is the whole of
	// rule 4 ("a session the service cannot explain is a session for a
	// human to look at, not one to clean up").
	if rec, ok := localDriver.(interface {
		Reconcile(context.Context) (tmux.Reconciliation, error)
	}); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		got, err := rec.Reconcile(ctx)
		cancel()
		if err != nil {
			log.Printf("muster: reconciliation failed: %v", err)
		} else {
			log.Printf("muster: reconciled — %s", got)
			// Orphans and disappearances are named individually. §12 rule 4
			// forbids acting on them, which makes reporting them the entire
			// value: a session this service cannot explain is one for a
			// human to look at, and a human cannot look at a count.
			for _, s := range got.Orphaned {
				log.Printf("muster:   orphaned %q cwd=%s (%s)", s.ID, s.Cwd, s.State.Evidence)
			}
			for _, s := range got.Vanished {
				log.Printf("muster:   vanished %q (%s)", s.ID, s.State.Evidence)
			}
			// #185: an adopted session's delivery lane stays, to be re-attached
			// when its module is ready; a vanished one's is closed and dropped.
			if lr, ok := localDriver.(interface{ ReconcileLanes(tmux.Reconciliation) }); ok {
				lr.ReconcileLanes(got)
			}
		}
	}

	// #185: start the delivery modules last among the startup work, without
	// blocking: each child comes up on its own schedule and a create that lands
	// first takes the built-in lane.
	if sm, ok := localDriver.(interface{ StartDeliveryModules() }); ok {
		sm.StartDeliveryModules()
	}

	// Sessions that ended while this service was down are found missing by
	// one complete local read, and get their closed-session record (#179).
	// Runs for every runtime, reconciling driver or not.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		svc.SweepLocal(ctx)
		cancel()
	}

	// Bind narrowly by default (§6.1: "Default to loopback. Exposure
	// beyond it is explicit configuration, never a side effect of
	// enabling federation.") No specific host or port is hardcoded here —
	// the fleet's actual port assignment is an operational fact, not a
	// specification one.
	addrs := splitList(getenv("FLEET_ADDR", "127.0.0.1:0"))
	for _, a := range addrs {
		if strings.HasPrefix(a, "0.0.0.0:") {
			log.Print("muster: WARNING binding 0.0.0.0 — this service can read paths and (when mutations are enabled) start processes; bind a specific interface instead")
		}
	}
	addrs = withLoopback(addrs)

	// Listen on every configured address, and always on loopback (see
	// withLoopback). The first bind failure is fatal: a service that came up
	// on some of its addresses is a service whose reachability depends on
	// which client you ask, and that is the ambiguity F36 was about.
	srv := &http.Server{Handler: mux}
	var listeners []net.Listener
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			for _, prev := range listeners {
				_ = prev.Close()
			}
			log.Fatalf("muster: listen %s: %v", a, err)
		}
		listeners = append(listeners, ln)
		log.Printf("muster: listening on %s (machine=%s runtime=%s)", ln.Addr(), self, runtimeID)
	}

	for _, ln := range listeners {
		go func(ln net.Listener) {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Fatalf("muster: serve %s: %v", ln.Addr(), err)
			}
		}(ln)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	// #185: closing a module's stdin makes it drop its connections and keep
	// its on-disk state, so the next start can re-attach.
	if sm, ok := localDriver.(interface{ StopDeliveryModules() }); ok {
		sm.StopDeliveryModules()
	}

	// This process spawned the opencode server; nothing else will stop it.
	if opencodeDriver != nil {
		if err := opencodeDriver.Shutdown(); err != nil {
			log.Printf("muster: stopping opencode server: %v", err)
		}
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// requireToken reports whether the absence of FLEET_TOKEN is fatal, given
// the outcome of loading FLEET_CONFIG (muster #95). cfgFile is nil for
// two different reasons that must be treated alike here: FLEET_CONFIG was
// never set, or main already called log.Fatal on a loadConfig error before
// this is ever consulted — either way, nil means no validated principal
// table is in hand, so a token is the only authentication left and its
// absence must refuse to start.
//
// cfgFile non-nil means loadConfig returned successfully, which — see
// config.go's loadConfig — is only possible for a file that parsed and
// named at least one principal with both a name and a token. So a non-nil
// cfgFile is never "an empty or malformed table slipped through"; it is
// always a validated one, and FLEET_TOKEN becomes optional exactly as
// FLEET_CONFIG's doc comment promises.
func requireToken(cfgFile *fileConfig) bool {
	return cfgFile == nil
}

// peerCredential decides what this service presents for its OWN long-lived
// peer subscriptions (internal/service.Service.SetPeerCredential, §14 D9;
// muster #98). token is FLEET_TOKEN as read in main; self is this
// machine's own id, the same one Service.peerRequest names ("system:"+self).
//
// FLEET_TOKEN wins when set — single-token mode, and the "config and token
// both set" state main_test.go's TestStartupAuthGateNeverStartsUnauthenticated
// already exercises, are unchanged by #98 and stay exactly as they were.
//
// Only when there is no token does the principal table get a say: a
// table-only deployment (FLEET_CONFIG present, FLEET_TOKEN absent) may name
// its own system identity as a principal and give it a credential there
// (fileConfig.selfCredential) — closing the #98 gap without adding a new
// config surface for a fact the table can already state.
//
// No token AND no matching principal returns "", same as before #98: this
// function never invents a credential, so the fail-closed posture from #95
// (an absent credential refuses the peer subscription rather than attempting
// it unauthenticated — enforced downstream by internal/drivers/remote.Driver.
// bearerFor and the peer's own principalFor, neither of which this function
// touches) is exactly as strict for a deployment that never opts into a
// self-credential as it always was.
func peerCredential(token string, cfgFile *fileConfig, self fleet.MachineId) string {
	if token != "" {
		return token
	}
	if cfgFile == nil {
		return ""
	}
	v, _ := cfgFile.selfCredential(self)
	return v
}

// withLoopback guarantees the service is reachable from its own machine.
//
// # Why this is not just a convenience
//
// A service bound only to a tunnel address disappears when the tunnel does —
// and it disappears from ITS OWN MACHINE, where every diagnostic is run. The
// observed incident: the interface still reported UP and RUNNING while passing
// nothing, so the process looked wedged. It was not; it was unaddressable. The
// thing that would have distinguished those two in one command was a loopback
// listener, which is precisely what was missing.
//
// So loopback is not an optional extra binding, it is the binding that keeps
// the failure diagnosable. An operator who configures a specific interface is
// making a statement about who ELSE may reach the service, never about whether
// the machine may reach itself.
//
// The added listener reuses the configured port so that a local probe is the
// same URL with the host swapped — a diagnostic nobody has to look up. When
// the configured port is ephemeral there is nothing to mirror, and the
// configured address is loopback anyway in the only case that produces one.
func withLoopback(addrs []string) []string {
	const loopback = "127.0.0.1"
	port := ""
	for _, a := range addrs {
		host, p, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		// Already reachable locally: an explicit loopback bind, or a
		// wildcard, which includes it.
		if host == "" || host == loopback || host == "localhost" ||
			host == "::1" || host == "0.0.0.0" || host == "::" {
			return addrs
		}
		if p != "" && p != "0" && port == "" {
			port = p
		}
	}
	if port == "" {
		return addrs
	}
	return append(addrs, net.JoinHostPort(loopback, port))
}

func splitList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// trustSeedInterval reads FLEET_TRUST_SEED_INTERVAL, defaulting to two
// minutes — frequent enough that a worktree created between passes is still
// well within the window the per-create seed (Driver.Create, #47 point 5)
// closes on its own, rare enough that the periodic pass is genuinely the
// secondary mechanism its doc comment says it is.
func trustSeedInterval() time.Duration {
	const def = 2 * time.Minute
	raw := os.Getenv("FLEET_TRUST_SEED_INTERVAL")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("muster: FLEET_TRUST_SEED_INTERVAL %q invalid, using %s", raw, def)
		return def
	}
	return d
}

// runTrustSeedLoop is #47's "on an interval" half of the trust-seed
// maintainer (point 4 of the issue's proposed shape); the startup pass
// above is the other half. Runs for the life of the process — there is
// nothing held here that process exit does not already release, unlike the
// HTTP listeners below, which is why this has no shutdown signal wired to
// it the way srv.Shutdown does.
func runTrustSeedLoop(d *tmux.Driver, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		got, err := d.SeedTrustRoots()
		if err != nil {
			log.Printf("muster: trust-seed: %v", err)
			continue
		}
		if got.Granted > 0 || got.ImportsGranted > 0 || len(got.RootsMissing) > 0 || got.LostRace {
			log.Printf("muster: trust-seed: %s", got)
		}
	}
}

// usageTop is the binary's own usage: the service form, then the operator
// subcommands, whose detailed usage each prints for itself.
func usageTop() string {
	return strings.Join([]string{
		"usage: muster serve                    start the service (configured by FLEET_* environment only)",
		"       muster doctor [--json] ...      read-only installation check (muster doctor --help)",
		"       muster compat --claude PATH     check a candidate runtime build (muster compat --help)",
		"       muster principal add|list|grant|revoke   enrol, list or re-grant clients (muster principal)",
		"       muster --version                print the build",
		"       muster -h | --help              print this usage",
		"",
		"The service takes no arguments beyond `serve`. Anything else is refused before any startup work.",
	}, "\n")
}

// legacyBare reports whether the binary was invoked under its former name
// with no arguments, which used to mean "start the service".
func legacyBare(argv []string) bool {
	return len(argv) == 1 && filepath.Base(argv[0]) == "colab-fleetd"
}

// runUsage decides what remains of the command line once the subcommands
// have had their turn. `serve` alone means "start the service" and is left to
// main. A help flag prints usage and exits 0; --version prints the build and
// exits 0. Anything else — a bare invocation, a typo, an unknown flag, a stray
// word — is refused with exit 2, because the alternative is starting a full
// instance nobody asked for (muster #177). Pure, so the gate has a test that
// does not go through os.Exit.
func runUsage(args []string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(args) == 1 && args[0] == "serve" {
		return false, 0
	}
	if len(args) == 0 {
		fmt.Fprintf(stderr, "muster: no command given — nothing was started\n%s\n", usageTop())
		return true, 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, usageTop())
		return true, 0
	case "--version", "version":
		b := fleet.SelfBuild()
		// The release tag leads when the build was stamped with one: it is
		// the only part of this line a downloader can compare against the
		// name a release binary was published under (#241).
		if b.Version != nil {
			fmt.Fprintf(stdout, "muster %s (%s, %s)\n", *b.Version, b.Short(), b.Go)
		} else {
			fmt.Fprintf(stdout, "muster %s (%s)\n", b.Short(), b.Go)
		}
		return true, 0
	}
	fmt.Fprintf(stderr, "muster: unknown argument %q — nothing was started\n%s\n", args[0], usageTop())
	return true, 2
}
