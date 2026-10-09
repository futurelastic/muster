package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/futurelastic/muster/internal/service"
)

// Changing an existing principal's grants (muster #274).
//
// # Why this exists
//
// `principal add` refused to touch a name that already existed ("remove it
// first if you mean to change its grants") and there was no remove. So every
// new verb that shipped with its own grant meant a hand-edit of the principal
// table on every machine: an untyped change to a security-relevant file, with
// no check that the grant name exists in the build that will read it.
//
// # What this deliberately is not
//
// Not a remote surface. It is a LOCAL subcommand that edits the file the
// operator could already edit, so it needs exactly the access that edit needs
// and grants nobody anything they could not already do. There is no HTTP route
// for it: a route that edits grants would change who may grant what, and that
// is a permission-model decision of its own.
//
// It does not reload a running service. The service reads its table at start,
// so the new set takes effect on the next start; the command says so rather
// than leave an operator believing it is already in force.

// humanRelayWarning is printed when --allow-human-relay is used.
//
// The grant makes whatever presents that principal's token indistinguishable
// from the person at the keyboard: unlabelled, slash commands delivered, no
// `from` required. That is only true of a person if the principal is theirs
// alone, so the precondition is stated at the moment it is overridden.
const humanRelayWarning = "WARNING: human-relay makes every caller presenting this principal's token " +
	"speak as the user's own typing — unlabelled, slash commands delivered, no `from` required. " +
	"Grant it only to a principal that relays a person's messages and is not shared with an agent."

// grantEdit is one grant/revoke request, kept apart from the flag parsing so
// a test can drive it without a process.
type grantEdit struct {
	cfgPath         string
	name            string
	grants          []string
	revoke          bool
	allowHumanRelay bool
	now             time.Time
}

func usageGrants() string {
	return strings.Join([]string{
		"usage: muster principal grant  <name> <grant>... [--config PATH] [--allow-human-relay]",
		"       muster principal revoke <name> <grant>... [--config PATH]",
		"",
		"grants: " + strings.Join(grantNames(), " · "),
		"",
		"Grants may be separated by spaces or commas. The config is rewritten atomically after a dated",
		"backup beside it. A running service keeps its old table until it is restarted.",
		"human-relay is refused unless --allow-human-relay is given.",
	}, "\n")
}

// runGrantEdit parses `muster principal grant|revoke ...` (args after the
// subcommand word) and applies it.
func runGrantEdit(revoke bool, args []string, out io.Writer) error {
	cfgPath := os.Getenv("FLEET_CONFIG")
	allow := false
	var positional []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case a == "--allow-human-relay":
			allow = true
		case strings.HasPrefix(a, "-") && len(a) > 1:
			return fmt.Errorf("unknown flag %q\n%s", a, usageGrants())
		default:
			positional = append(positional, a)
		}
	}
	if cfgPath == "" {
		return errors.New("no config: set FLEET_CONFIG or pass --config=PATH")
	}
	if len(positional) < 2 {
		return errors.New(usageGrants())
	}
	var grants []string
	for _, p := range positional[1:] {
		for _, g := range strings.Split(p, ",") {
			if g = strings.TrimSpace(g); g != "" {
				grants = append(grants, g)
			}
		}
	}
	return applyGrantEdit(grantEdit{
		cfgPath: cfgPath, name: positional[0], grants: grants,
		revoke: revoke, allowHumanRelay: allow, now: time.Now(),
	}, out)
}

func applyGrantEdit(e grantEdit, out io.Writer) error {
	raw, err := os.ReadFile(e.cfgPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", e.cfgPath, err)
	}
	// UseNumber: this rewrites the whole file, and a number that round-trips
	// through float64 can change (1e+06, lost precision) in a setting the
	// operator never asked to touch.
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("%s is not valid JSON, refusing to rewrite it: %w", e.cfgPath, err)
	}

	list, _ := doc["principals"].([]any)
	var target map[string]any
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok || m["name"] != e.name {
			continue
		}
		if target != nil {
			return fmt.Errorf("principal %q appears more than once in %s; fix that by hand first", e.name, e.cfgPath)
		}
		target = m
	}
	if target == nil {
		return fmt.Errorf("no principal %q in %s (see: muster principal list)", e.name, e.cfgPath)
	}

	var held []string
	if gs, ok := target["grants"].([]any); ok {
		for _, g := range gs {
			s, ok := g.(string)
			if !ok {
				return fmt.Errorf("principal %q has a grant that is not a string; fix that by hand first", e.name)
			}
			held = append(held, s)
		}
	}
	has := func(g string) bool {
		for _, h := range held {
			if h == g {
				return true
			}
		}
		return false
	}

	// Validated BEFORE anything is written, against the list the service loads
	// with. Revoking is the one place an unknown name is tolerated, and only a
	// name the principal already holds: a table carrying a grant this build no
	// longer knows would not start, and removing it is the repair.
	for _, g := range e.grants {
		if service.ValidGrant(service.Grant(g)) {
			continue
		}
		if e.revoke && has(g) {
			continue
		}
		return fmt.Errorf("unknown grant %q; available: %s", g, strings.Join(grantNames(), ", "))
	}
	warnHumanRelay := false
	if !e.revoke {
		for _, g := range e.grants {
			if g != string(service.GrantHumanRelay) || has(g) {
				continue
			}
			if !e.allowHumanRelay {
				return fmt.Errorf("refusing to grant %s: it makes the principal's token speak as the user. "+
					"Pass --allow-human-relay if this principal relays a person's messages and no agent shares it", g)
			}
			warnHumanRelay = true
		}
	}

	next := append([]string(nil), held...)
	changed := false
	for _, g := range e.grants {
		switch {
		case e.revoke:
			var kept []string
			for _, h := range next {
				if h != g {
					kept = append(kept, h)
				}
			}
			if len(kept) != len(next) {
				changed = true
				next = kept
			}
		case !contains(next, g):
			next = append(next, g)
			changed = true
		}
	}

	if !changed {
		state := "already holds"
		if e.revoke {
			state = "already lacks"
		}
		fmt.Fprintf(out, "no change: principal %q %s %s\n", e.name, state, strings.Join(e.grants, ","))
		printGrantSet(out, e.name, held)
		return nil
	}
	if next == nil {
		next = []string{}
	}
	target["grants"] = anySlice(next)

	// Validate the result the way the service will read it, on a temp file
	// beside the config, before the original is replaced.
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(e.cfgPath), ".config.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	cfg, err := loadConfig(tmp.Name())
	if err == nil {
		_, err = cfg.principals()
	}
	if err != nil {
		return fmt.Errorf("the edited table would not load, nothing written: %w", err)
	}

	// Backup first, with the original bytes and the original's tightness: the
	// config holds every token, so a backup must be no more readable than it.
	backup, err := writeBackup(e.cfgPath, e.now, raw)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), e.cfgPath); err != nil {
		return fmt.Errorf("writing %s: %w", e.cfgPath, err)
	}

	verb := "granted"
	if e.revoke {
		verb = "revoked"
	}
	fmt.Fprintf(out, "%s %s on principal %q (backup: %s)\n", verb, strings.Join(e.grants, ","), e.name, backup)
	if warnHumanRelay {
		fmt.Fprintln(out, humanRelayWarning)
	}
	if len(next) == 0 {
		fmt.Fprintf(out, "note: principal %q now holds no grants and can do nothing\n", e.name)
	}
	printGrantSet(out, e.name, next)
	fmt.Fprintln(out, "the running service still has the OLD table; restart it to apply")
	return nil
}

// printGrantSet prints the resulting set as whoami's JSON keys principal and
// grants (api.md, GET /v1/whoami). machine, source and listsYou are facts
// about a running service and a request, which this offline command has
// neither of.
func printGrantSet(out io.Writer, name string, grants []string) {
	if grants == nil {
		grants = []string{}
	}
	b, _ := json.Marshal(struct {
		Principal string   `json:"principal"`
		Grants    []string `json:"grants"`
	}{name, grants})
	fmt.Fprintf(out, "%s\n", b)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// writeBackup saves the original bytes beside the config as
// <config>.<YYYYMMDD-HHMMSS>.bak and never overwrites one: two edits inside a
// second would otherwise leave only the later original, and the earlier state
// — the one an operator may want back — would be gone. A taken name gets a
// numeric suffix.
func writeBackup(cfgPath string, now time.Time, raw []byte) (string, error) {
	base := fmt.Sprintf("%s.%s.bak", cfgPath, now.Format("20060102-150405"))
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s.%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("writing backup %s: %w", name, err)
		}
		if _, err := f.Write(raw); err != nil {
			f.Close()
			return "", fmt.Errorf("writing backup %s: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("writing backup %s: %w", name, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("writing backup beside %s: too many backups in one second", cfgPath)
}
