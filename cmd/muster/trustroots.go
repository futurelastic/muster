package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/futurelastic/muster/internal/trustseed"
)

// trustSeedStartLine is the one line the service logs when trust roots are
// configured. It counts the roots the seeder is ACTIVE under — what New kept after
// its own validation — and not the length of the list the operator wrote, which is
// what it counted before muster #469 and so reported a single root equal to the home
// directory as "configured for 1 root" while none was active. Zero active is said
// as such rather than as a number a reader can take for a feature that is on.
func trustSeedStartLine(configured int, active []string) string {
	switch {
	case len(active) == 0:
		return fmt.Sprintf("muster: trust-seed has NO active root (%d configured, none usable): nothing will be seeded", configured)
	case len(active) == configured:
		return fmt.Sprintf("muster: trust-seed active for %d root(s)", len(active))
	default:
		return fmt.Sprintf("muster: trust-seed active for %d of %d configured root(s) (a repeated root counts once)", len(active), configured)
	}
}

// configuredTrustRoots is the roots a start with this environment would hand the
// seeder, read the way main.go reads them: FLEET_TRUST_ROOTS when set, else the
// config file's trustRoots. cfg may be nil.
func configuredTrustRoots(cfg *fileConfig, getenv func(string) string) []string {
	roots := splitList(getenv("FLEET_TRUST_ROOTS"))
	if len(roots) == 0 && cfg != nil {
		roots = cfg.TrustRoots
	}
	return roots
}

// checkTrustRoots answers the trust.roots row (muster #469): which trust roots are
// active, so an operator can check them without reading a log. ok is false where
// there is nothing to say — trust seeding is the tmux driver's, and a machine with
// no roots has the feature off — so a report from such a machine is unchanged.
//
// A root the service refuses to start on (not absolute, or the home directory or
// "/", spelled any way) fails the row naming it and why. That is the same judgement
// validateConfigExtras makes, repeated here because the doctor does not run that
// function when there is no FLEET_CONFIG, and roots set in the environment alone
// are exactly the case where the file cannot have caught it.
//
// Roots are shown relative to home ("~/projects"), not as absolute paths: the row
// exists to let an operator confirm which folders are covered, and a leading home
// path says nothing more while making the output less safe to paste.
func checkTrustRoots(cfg *fileConfig, cfgFailed bool, getenv func(string) string) (doctorRow, bool) {
	if name, ok := primaryRuntime(getenv); !ok || name != runtimeTmux {
		return doctorRow{}, false
	}
	roots := configuredTrustRoots(cfg, getenv)
	row := doctorRow{ID: "trust.roots", Refs: []int{47, 469}}
	if len(roots) == 0 {
		if cfgFailed {
			row.Status = statusSkip
			row.Summary = "undecidable until config.load passes"
			return row, true
		}
		return doctorRow{}, false
	}
	home, _ := os.UserHomeDir()
	var refused []string
	for _, r := range roots {
		switch {
		case !filepath.IsAbs(r):
			refused = append(refused, fmt.Sprintf("%q is not an absolute path", r))
		default:
			if why := trustseed.RootRefusal(home, r); why != "" {
				refused = append(refused, fmt.Sprintf("%s is %s", homeRelative(home, r), why))
			}
		}
	}
	active := trustseed.ActiveRoots(home, roots)
	if len(refused) > 0 {
		row.Status = statusFail
		row.Summary = fmt.Sprintf("%d of %d configured trust root(s) cannot be used — the service refuses to start", len(refused), len(roots))
		row.Detail = strings.Join(refused, "; ") + ". A trust root is the folder that holds the working folders " +
			"(a projects directory under home, for example), never home itself or \"/\""
		return row, true
	}
	row.Status = statusPass
	row.Summary = fmt.Sprintf("%d trust root(s) active of %d configured", len(active), len(roots))
	shown := make([]string, len(active))
	for i, a := range active {
		shown[i] = homeRelative(home, a)
	}
	row.Detail = strings.Join(shown, ", ")
	if len(active) < len(roots) {
		row.Detail += " (a root written twice counts once)"
	}
	return row, true
}

// homeRelative spells path relative to home as "~/…" when it is under it, and as
// given otherwise.
func homeRelative(home, path string) string {
	c := filepath.Clean(path)
	if home == "" {
		return c
	}
	h := filepath.Clean(home)
	if c == h {
		return "~"
	}
	if rel, err := filepath.Rel(h, c); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
		return "~/" + filepath.ToSlash(rel)
	}
	return c
}
