package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/futurelastic/muster/internal/grant"
)

// `muster grant` — sign this machine in to a ChatGPT subscription once (muster #451).
//
//	muster grant login            sign in with a code typed on any device with a browser
//	muster grant import PATH      take the sign-in out of an opencode auth.json
//	muster grant status           where the sign-in stands
//	muster grant logout           forget the sign-in
//
// The sign-in goes into an owner-only store under the state directory. The
// running service, and only the service, renews it, and hands each session an
// access token alone (internal/grant). Nothing here starts a session or reaches a
// running service: the service notices a change in the store within half a minute.

func usageGrant() string {
	return strings.Join([]string{
		"usage: muster grant login         [--state-dir=DIR]",
		"       muster grant import PATH   [--state-dir=DIR]",
		"       muster grant status        [--state-dir=DIR]",
		"       muster grant logout        [--state-dir=DIR]",
		"",
		"login prints a web address and a short code; open the address on any device, type the code, and approve.",
		"import reads the `openai` sign-in out of an opencode auth.json instead (for a machine with no browser flow",
		"you can reach). After an import, sign the source file out: muster rotates the refresh token from then on,",
		"so the source copy stops working the first time muster renews.",
		"status says whether the sign-in is ok, degraded (a renewal is failing and being retried), revoked, or",
		"absent; it exits 1 unless it is ok. logout deletes the stored sign-in; sessions already running keep the",
		"access token they hold until it lapses.",
		"",
		"A session gets this sign-in only on a machine whose FLEET_OPENCODE_PROVIDERS names openai (or names no",
		"provider at all). Whether one subscription may be shared by automated sessions is a question of the",
		"provider's terms; muster does not decide it for you.",
		"",
		"The state directory is --state-dir, else FLEET_STATE_DIR, else the service installer's default.",
	}, "\n")
}

// loginTimeout bounds how long `muster grant login` waits for an approval.
const loginTimeout = 15 * time.Minute

// importLimit bounds the file `muster grant import` reads.
const importLimit = 1 << 20

// runGrant handles `muster grant ...` and reports whether it consumed the invocation.
func runGrant(args []string, getenv func(string) string, stdout, stderr io.Writer) (handled bool, code int) {
	return runGrantWith(args, getenv, stdout, stderr, grant.NewClient(), time.Now)
}

func runGrantWith(args []string, getenv func(string) string, stdout, stderr io.Writer, client *grant.Client, now func() time.Time) (handled bool, code int) {
	if len(args) == 0 || args[0] != "grant" {
		return false, 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, usageGrant())
		return true, 2
	}
	sub := args[1]
	var stateFlag, importPath string
	for _, a := range args[2:] {
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(stdout, usageGrant())
			return true, 0
		case strings.HasPrefix(a, "--state-dir="):
			stateFlag = strings.TrimPrefix(a, "--state-dir=")
		case sub == "import" && importPath == "" && !strings.HasPrefix(a, "-"):
			importPath = a
		default:
			fmt.Fprintf(stderr, "muster grant: unknown argument %q\n%s\n", a, usageGrant())
			return true, 2
		}
	}
	dir, err := keyStateDir(stateFlag, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "muster grant:", err)
		return true, 2
	}
	store := grant.NewStore(dir)

	switch sub {
	case "login":
		return true, grantLogin(store, client, now, stdout, stderr)
	case "import":
		if importPath == "" {
			fmt.Fprintf(stderr, "muster grant import: name the auth.json to read\n%s\n", usageGrant())
			return true, 2
		}
		return true, grantImport(store, importPath, now, stdout, stderr)
	case "status":
		return true, grantStatus(store, now, stdout)
	case "logout":
		if err := store.Remove(); err != nil {
			fmt.Fprintln(stderr, "muster grant logout:", err)
			return true, 1
		}
		fmt.Fprintln(stdout, "signed out: the stored sign-in is deleted (sessions already running keep the access token they hold until it lapses)")
		return true, 0
	}
	fmt.Fprintln(stderr, usageGrant())
	return true, 2
}

func grantLogin(store *grant.Store, client *grant.Client, now func() time.Time, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	dev, err := client.StartDevice(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "muster grant login:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Open %s on any device and type this code:\n\n    %s\n\nWaiting for you to approve (up to %d minutes)...\n",
		dev.URL, dev.Code, int(loginTimeout/time.Minute))
	toks, err := client.PollDevice(ctx, dev)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("nobody approved the code in time; run the command again")
		} else if errors.Is(err, context.Canceled) {
			err = errors.New("cancelled")
		}
		fmt.Fprintln(stderr, "muster grant login:", err)
		return 1
	}
	t := now()
	saved, err := store.Replace(grant.Grant{
		Refresh:      toks.Refresh,
		Access:       toks.Access,
		ExpiresAtMs:  t.Add(toks.ExpiresIn).UnixMilli(),
		AccountID:    toks.AccountID,
		SignedInAtMs: t.UnixMilli(),
	})
	if err != nil {
		fmt.Fprintln(stderr, "muster grant login:", err)
		return 1
	}
	fmt.Fprintf(stdout, "signed in. The access token is valid until %s (%s); the running service renews it before then.\n",
		saved.ExpiresAt().Format(time.RFC3339), until(saved.ExpiresAt(), t))
	fmt.Fprintln(stdout, "A session gets it only where FLEET_OPENCODE_PROVIDERS names openai (or names nothing). Check: muster grant status; muster doctor.")
	return 0
}

// importedAuth is the part of an opencode auth.json entry this command reads.
type importedAuth struct {
	Type      string `json:"type"`
	Refresh   string `json:"refresh"`
	Access    string `json:"access"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId"`
}

func grantImport(store *grant.Store, path string, now func() time.Time, stdout, stderr io.Writer) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(stderr, "muster grant import:", err)
		return 1
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, importLimit+1))
	if err != nil || len(raw) > importLimit {
		fmt.Fprintf(stderr, "muster grant import: %s is unreadable or larger than %d bytes\n", path, importLimit)
		return 1
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		fmt.Fprintf(stderr, "muster grant import: %s is not an opencode auth.json\n", path)
		return 1
	}
	var e importedAuth
	if rawEntry, ok := entries[grant.ProviderOpenAI]; !ok || json.Unmarshal(rawEntry, &e) != nil {
		fmt.Fprintf(stderr, "muster grant import: %s has no %q sign-in (sign in with: opencode auth login, choosing a ChatGPT method)\n", path, grant.ProviderOpenAI)
		return 1
	}
	switch {
	case e.Type != "oauth":
		fmt.Fprintf(stderr, "muster grant import: the %q entry is a %q credential, not a ChatGPT sign-in\n", grant.ProviderOpenAI, e.Type)
		return 1
	case e.Refresh == "":
		fmt.Fprintln(stderr, "muster grant import: the entry holds no refresh token, so it cannot be renewed")
		return 1
	case e.Refresh == grant.Placeholder:
		fmt.Fprintln(stderr, "muster grant import: that is a session's copy (it holds a placeholder, not a refresh token); import the original sign-in")
		return 1
	}
	t := now()
	saved, err := store.Replace(grant.Grant{
		Refresh:      e.Refresh,
		Access:       e.Access,
		ExpiresAtMs:  e.Expires,
		AccountID:    e.AccountID,
		SignedInAtMs: t.UnixMilli(),
	})
	if err != nil {
		fmt.Fprintln(stderr, "muster grant import:", err)
		return 1
	}
	fmt.Fprintf(stdout, "imported the %s sign-in (access token valid until %s).\n", grant.ProviderOpenAI, saved.ExpiresAt().Format(time.RFC3339))
	fmt.Fprintf(stderr, "now sign %s out of the source (opencode auth logout): muster rotates the refresh token from here on, so that copy stops working the first time muster renews.\n", path)
	return 0
}

func grantStatus(store *grant.Store, now func() time.Time, stdout io.Writer) int {
	g, err := store.Load()
	switch {
	case errors.Is(err, grant.ErrNoGrant):
		fmt.Fprintln(stdout, "openai: signed out — no sign-in is stored (run: muster grant login)")
		return 1
	case err != nil:
		fmt.Fprintln(stdout, "openai: unreadable —", err)
		return 1
	}
	t := now()
	if g.Revoked != nil {
		fmt.Fprintf(stdout, "openai: revoked — %s\n", (&grant.RevokedError{Status: g.Revoked.Status, Code: g.Revoked.Code}).Error())
		return 1
	}
	fmt.Fprintf(stdout, "openai: signed in; access token valid until %s (%s)\n", g.ExpiresAt().Format(time.RFC3339), until(g.ExpiresAt(), t))
	fmt.Fprintf(stdout, "  signed in %s", time.UnixMilli(g.SignedInAtMs).Format(time.RFC3339))
	if g.RefreshedAtMs > 0 {
		fmt.Fprintf(stdout, ", last renewed %s", time.UnixMilli(g.RefreshedAtMs).Format(time.RFC3339))
	}
	fmt.Fprintf(stdout, "\n  store %s\n", store.Path())
	if !g.ExpiresAt().After(t) {
		fmt.Fprintln(stdout, "  the access token has lapsed: the service renews it on its next pass, or is not running")
		return 1
	}
	return 0
}

// until renders how far off, or how long ago, an instant is.
func until(at, now time.Time) string {
	d := at.Sub(now)
	if d < 0 {
		return round(-d) + " ago"
	}
	return "in " + round(d)
}

func round(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}
