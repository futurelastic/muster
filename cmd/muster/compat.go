package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/signal"
	"strings"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/compat"
	"github.com/futurelastic/muster/internal/drivers/tmux"
)

// muster compat (muster #183): checks a candidate build of the agent
// runtime against this service's assumptions and prints a versioned report.
// The contract — report, catalogue, exit codes — is docs/compat.md and
// internal/compat; the checks themselves live beside the driver code they
// exercise (internal/drivers/tmux/compat*.go).

// compatDefaultTimeout bounds a whole run. A full run costs a handful of model
// turns and a few minutes; this is the ceiling, not the expectation.
const compatDefaultTimeout = 20 * time.Minute

// runCompatFn is the checker. A variable so a test can exercise flag parsing,
// output and exit codes without a runtime.
var runCompatFn = tmux.RunCompat

func usageCompat() string {
	return strings.Join([]string{
		"usage: muster compat --claude <absolute path to a claude binary> [--json]",
		"                           [--pack <dir>] [--only <id[,id…]>] [--timeout <duration>]",
		"",
		"Checks a CANDIDATE build of the agent runtime against the assumptions this service",
		"makes about it, in a private multiplexer server that never touches the service's",
		"own sessions, and prints a versioned report. It only reports: installing, pinning",
		"and promoting a build are the caller's decision.",
		"",
		"  --claude PATH   the candidate, launched by this absolute path — never looked up",
		"                  on PATH (required)",
		"  --json          print the schema 1 report as JSON instead of a table",
		"  --pack DIR      save the raw evidence each check produced (a new or empty",
		"                  directory; captures are redacted)",
		"  --only IDS      run only these check IDs (comma-separated); the report is then a",
		"                  partial run, not a certification",
		"  --timeout D     overall limit for the whole run (default 20m)",
		"",
		"Exit codes: 0 every must check passed · 1 a must check failed · 2 could not certify",
		"(a must check could not run) or a bad invocation. Progress goes to stderr.",
	}, "\n")
}

// runCompat handles `muster compat ...` and reports whether it consumed
// the invocation, and the exit code to use when it did.
func runCompat(args []string, getenv func(string) string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(args) == 0 || args[0] != "compat" {
		return false, 0
	}
	if why := compatRefusal(hostOS()); why != "" {
		fmt.Fprintf(stderr, "muster compat: %s\n", why)
		return true, 2
	}
	// A panic anywhere below has already run the harness's teardown while
	// unwinding (it is deferred), so all that is left to do is say so and exit
	// with "could not certify" rather than a stack trace and an ambiguous code.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "muster compat: internal error: %v\n", r)
			handled, code = true, 2
		}
	}()
	opts := tmux.CompatOptions{Getenv: getenv, Log: stderr}
	timeout := compatDefaultTimeout
	asJSON := false

	bad := func(format string, a ...any) (bool, int) {
		fmt.Fprintf(stderr, "muster compat: "+format+"\n\n%s\n", append(a, usageCompat())...)
		return true, 2
	}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		// Both `--flag=value` (as doctor takes them) and `--flag value` (as
		// the usage line spells them).
		name, val, hasVal := strings.Cut(a, "=")
		value := func() (string, bool) {
			if hasVal {
				return val, true
			}
			if i+1 < len(rest) {
				i++
				return rest[i], true
			}
			return "", false
		}
		switch name {
		case "-h", "--help":
			fmt.Fprintln(stdout, usageCompat())
			return true, 0
		case "--json":
			if hasVal {
				return bad("--json takes no value")
			}
			asJSON = true
		case "--claude":
			v, ok := value()
			if !ok || v == "" {
				return bad("--claude needs a value")
			}
			opts.Claude = v
		case "--pack":
			v, ok := value()
			if !ok || v == "" {
				return bad("--pack needs a value")
			}
			opts.PackDir = v
		case "--only":
			v, ok := value()
			if !ok || strings.TrimSpace(v) == "" {
				// An empty list must not mean "everything": a check list that
				// silently widens is the typo this flag exists to catch.
				return bad("--only needs at least one check id")
			}
			ids, err := compat.ResolveOnly(splitList(v))
			if err != nil {
				return bad("--only: %v", err)
			}
			opts.Only = ids
		case "--timeout":
			v, ok := value()
			d, err := time.ParseDuration(v)
			if !ok || err != nil || d <= 0 {
				return bad("bad --timeout %q", v)
			}
			timeout = d
		default:
			return bad("unknown argument %q", a)
		}
	}
	if opts.Claude == "" {
		return bad("--claude is required")
	}
	b := fleet.SelfBuild()
	opts.Build = compat.Build{Commit: b.Revision, Modified: b.Modified}
	if b.Version != nil {
		opts.Build.Version = *b.Version
	}

	// The driver logs through the standard logger; one line of it is expected in
	// every run (the trust seeder refusing the deliberately untrusted directory).
	// Give those lines this command's name and no timestamp, so they read as what
	// they are, and put the logger back afterwards for the tests that share it.
	oldFlags, oldPrefix := log.Flags(), log.Prefix()
	log.SetFlags(0)
	log.SetPrefix("muster compat: ")
	defer func() { log.SetFlags(oldFlags); log.SetPrefix(oldPrefix) }()

	// SIGINT, SIGTERM and SIGHUP cancel the run; the harness's teardown runs
	// under its own context, so a signal ends the checks and never the cleanup.
	// The handler stays installed until RunCompat has returned, so a second
	// signal cannot interrupt that teardown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rep, err := runCompatFn(ctx, opts)
	if rep.Schema != 0 {
		if asJSON {
			if werr := compat.WriteJSON(stdout, rep); werr != nil {
				fmt.Fprintln(stderr, werr)
				return true, 2
			}
		} else {
			compat.WriteText(stdout, rep)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "muster compat: %v\n", err)
		return true, 2
	}
	return true, rep.ExitCode()
}
