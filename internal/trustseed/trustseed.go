// Package trustseed pre-answers the boot questions a session-running runtime
// asks about a directory, by writing the answers into the runtime's own state
// file before the questions are ever raised.
//
// # The questions
//
// The runtime this fleet drives asks, the first time it is pointed at a
// directory, "do you trust the files in this folder" — and blocks until a
// human answers. muster already knows how to answer that question for a
// session IT created, because the caller that asked for the session named
// that directory in the same request and consented to it there (see the root
// package's SessionSpec.Consents and prompt.go's doctrine on why that is the
// caller's decision to make, never this layer's). Most sessions on a real
// fleet are not created that way — a supervisor spawns the runtime directly —
// so no create request exists for the consent to travel on.
//
// It asks a second question of the same shape whenever the instruction files
// the directory carries import a file from OUTSIDE it: "allow external
// imports". Like the first it holds a new session forever, with no bridge and
// no conversation id for a host to link to, and like the first it is asked
// again for every directory on a workspace whose folders all import the same
// shared file from one place (muster #211).
//
// # What this package does instead
//
// The runtime remembers each answer as a per-directory key in its own state
// file: projects["<dir>"].hasTrustDialogAccepted for the first question, and
// projects["<dir>"].hasClaudeMdExternalIncludesApproved (with its companion
// hasClaudeMdExternalIncludesWarningShown) for the second. This package
// writes those keys ahead of time, under a machine-local,
// operator-configured set of roots — so neither question gets asked in the
// first place, for any session, whoever started it. It is the same shape the
// consent table already commits to on create, extended from one request to a
// standing policy: the operator, not this service, is the party saying which
// directories are theirs, once, in the machine's own configuration.
//
// One statement covers both questions on purpose. The roots hold the
// operator's own work, and the files those directories import are the
// operator's own shared files; there is no second list to keep in step with
// the first, and no directory the operator called theirs but not theirs to
// import from.
//
// # The hazard this is built against
//
// The state file is a single JSON document the runtime rewrites wholesale
// while dozens of sessions hold it open. A write based on a read that is no
// longer current would silently discard whatever changed in between —
// somebody's fresh credential, a newly-visited project, another one of this
// package's own writes racing it from a second pass. So every write here is
// add-only (a key is only ever set to true, never removed, never rewritten
// once already true), and every write is abandoned rather than forced the
// moment the file is found to have changed since it was read — see
// ensureKeys. Losing that race costs nothing: the next pass (on start, on an
// interval, or on the next session create) reads fresh and tries again.
package trustseed

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The fields this package ensures on a project entry. Named once so a future
// rename shows every place it matters.
const (
	// trustKey answers "do you trust the files in this folder".
	trustKey = "hasTrustDialogAccepted"
	// importsApprovedKey answers "allow external CLAUDE.md file imports" —
	// the runtime saves it per project, and the project is the enclosing
	// repository root when there is one, otherwise the directory itself
	// (muster #211), which is the same key-per-root rule trustKey follows.
	importsApprovedKey = "hasClaudeMdExternalIncludesApproved"
	// importsShownKey is the runtime's record that it has already put that
	// question on screen. It is set beside importsApprovedKey so the two never
	// disagree about a project this package answered.
	importsShownKey = "hasClaudeMdExternalIncludesWarningShown"
)

// The same names, exported for the one reader that has to print them: a compat
// report that says which key the runtime stopped honouring (muster #212).
// Aliases, not a second copy, so a rename still shows every place it matters.
const (
	TrustKey           = trustKey
	ImportsApprovedKey = importsApprovedKey
	ImportsShownKey    = importsShownKey
)

// Counter names. Kept beside the registry rather than scattered at call
// sites, the same discipline the tmux driver's counters.go applies to its
// own names.
const (
	// CounterGranted counts every folder-trust key this package actually set
	// to true — the number that says the feature is doing its job. Compare
	// its rate against uptime (as GET /v1/health already lets a caller do for
	// the tmux driver's own counters) to notice it stopping.
	//
	// It counts the TRUST key only. The imports key has a counter of its own,
	// so a caller can tell which of the two keys is being written
	// (muster #211): a fleet whose trust counter moves and whose imports
	// counter never does is one whose runtime stopped storing the second
	// answer where this package writes it.
	CounterGranted = "trust_seed.granted"
	// CounterImportsGranted counts every project entry whose external-imports
	// approval this package set to true. The companion "warning shown" key is
	// written in the same commit and is not counted apart: it is bookkeeping
	// for the question, and never moves without the approval it accompanies.
	CounterImportsGranted = "trust_seed.imports_granted"
	// CounterLostRace counts a write abandoned because the state file
	// changed between being read and being committed — see the package doc.
	// Expected to be rare and always eventually followed by a granted count
	// once a later pass wins the race; a rate that never resolves is the
	// regression worth a human's attention.
	CounterLostRace = "trust_seed.lost_race"
	// CounterRootMissing counts a configured root not found on disk during a
	// pass. Not a silent skip (see New's doc comment): every pass a missing
	// root is still configured, it is counted again, so the rate is the
	// duration of the misconfiguration, not just its first occurrence.
	CounterRootMissing = "trust_seed.root_missing"
	// CounterRefused counts a path SeedPath was asked to seed that resolves
	// outside every configured root, or to the home directory or filesystem
	// root itself — the scope guards refusing to widen what gets seeded
	// beyond what the operator configured.
	CounterRefused = "trust_seed.refused"
)

// errLostRace is ensureKeys' internal signal that a write was abandoned
// because the file changed since it was read. Never returned to a caller of
// SeedAll/SeedPath as an error — see their doc comments: losing this race is
// an expected, harmless outcome, not a failure.
var errLostRace = errors.New("trustseed: state file changed since it was read; abandoning this write")

// Seeder maintains the trust and external-imports keys in projects[...] of one
// runtime state file, for every repository or worktree root under a fixed set
// of configured roots.
//
// The zero value is not usable; use New. A nil *Seeder is a valid, disabled
// one everywhere a method is called on it — the same off-by-default contract
// the tmux driver's WithRecordRoot/WithCredentialPath already use, so a
// caller that never configured this feature never has to check for nil
// before asking it to do nothing.
type Seeder struct {
	statePath string
	roots     []configuredRoot
	home      string // absolute, cleaned; "" if unknown
	// keys are the fields this Seeder sets on a project entry: seededKeys for
	// every Seeder New builds, the trust key alone for NewTrustOnly.
	keys []string

	// mu serializes this Seeder's own read-modify-write passes. It closes
	// the race between this daemon's OWN callers (a startup pass, an
	// interval tick, and a per-create seed can all fire close together) —
	// the mtime/size check in ensureKeys is what handles a change made by
	// something outside this process, including another instance of this
	// service on a shared file, which no in-process mutex can see.
	mu sync.Mutex

	counters   sync.Mutex
	counterMap map[string]int64

	// logf writes the one line a create-time seed that set a key leaves
	// behind (muster #469). log.Printf in production, the same writer the
	// tmux driver's own trust-seed refusal line goes to; a test swaps it to
	// read what was said. Never nil on a Seeder New built.
	logf func(format string, args ...any)

	// afterRead is a test hook: called once, after the state file has been
	// read and before ensureKeys decides what to write, so a test can inject
	// a concurrent external write into the exact window this package exists
	// to survive. Nil in production.
	afterRead func()

	// onList and onLstat are test hooks for the periodic walk: onList is called
	// with every directory it lists, onLstat with every directory it had to ask
	// about directly instead. A test pins what a pass costs by counting them —
	// the cost #406 was about. Nil in production.
	onList  func(dir string)
	onLstat func(dir string)
}

// New builds a Seeder. statePath is the runtime's own state file (in
// practice, the same path a driver already stats for #12's credential
// generation). roots are the configured roots — this package learns the
// mechanism, not the paths; where a real deployment's roots come from is
// machine-local configuration this repository never commits (see New's
// caller). home is the operator's home directory, when known; both it and
// the OS path separator alone are refused as seeding targets regardless of
// what roots say (see SeedPath).
//
// Any root that is relative, empty, a repeat, or the home directory or the
// filesystem root (RootRefusal: spelled any way, or through a symlink) is
// dropped here rather than reported per-pass — that is a configuration
// mistake at construction time, not the "root not found yet" case
// CounterRootMissing exists for. It is dropped, not fatal, because this
// constructor has no way to say so; the operator is told by whoever builds
// the roots list (RootRefusal, called by the service's config validation
// before this is reached) and by ActiveRoots, which is what the start-up line
// and `muster doctor` report — never the configured count.
//
// Each surviving root is resolved once, here, into a configuredRoot — see
// its doc comment for why discoverIslands needs both forms (muster
// #213).
func New(statePath, home string, roots []string) *Seeder {
	s := &Seeder{
		statePath:  statePath,
		home:       cleanAbs(home),
		counterMap: map[string]int64{},
		keys:       seededKeys,
		logf:       log.Printf,
	}
	for _, c := range ActiveRoots(home, roots) {
		s.roots = append(s.roots, configuredRoot{spelling: c, walk: resolveRootForWalk(c)})
	}
	return s
}

// ActiveRoots returns the roots New would keep out of roots, cleaned and in
// the order given: absolute, not repeated, and not refused by RootRefusal. It
// is the one definition of "active", so the count the service reports at
// start-up and the roots `muster doctor` lists are the roots the Seeder seeds
// under, not the length of the list the operator wrote (muster #469 — a
// single root equal to home was reported as "configured for 1 root" while
// none was active).
func ActiveRoots(home string, roots []string) []string {
	var active []string
	seen := map[string]bool{}
	for _, r := range roots {
		c := cleanAbs(r)
		if c == "" || !filepath.IsAbs(c) || seen[c] {
			continue
		}
		if RootRefusal(home, c) != "" {
			continue
		}
		seen[c] = true
		active = append(active, c)
	}
	return active
}

// RootRefusal says why root can never be a trust root, or returns "" when it
// may. A root that is the home directory or the filesystem root is refused
// because seeding it would vouch for everything under it — every repository
// the account will ever clone, not the folder the operator meant. The guard
// is deliberate and unchanged; what this adds is that it can be asked, so a
// caller can say so instead of the root vanishing (muster #469).
//
// root is judged as the operator spelled it (cleaned, so "~/." style
// spellings and a trailing separator count) AND with its symlinks resolved,
// against home in both forms too: a link that points at home, or a home that
// is itself reached through a link, is the same directory. A path that does
// not exist yet resolves to itself, so only a spelling that already is home
// or "/" is refused on that ground, which is also all a not-yet-created
// folder can be. A relative or empty root has no answer here: it is not an
// absolute path at all, which the service's own validation reports apart.
func RootRefusal(home, root string) string {
	c := cleanAbs(root)
	if c == "" || !filepath.IsAbs(c) {
		return ""
	}
	sys := string(filepath.Separator)
	h := cleanAbs(home)
	var homes []string
	if h != "" && filepath.IsAbs(h) {
		homes = []string{h, resolveRootForWalk(h)}
	}
	for _, f := range []string{c, resolveRootForWalk(c)} {
		if f == sys {
			return "the filesystem root"
		}
		for _, hh := range homes {
			if f == hh {
				return "the home directory"
			}
		}
	}
	return ""
}

// configuredRoot carries one configured trust root in the two forms this
// package needs kept apart (muster #213):
//
//   - spelling is exactly what the operator wrote (cleaned, absolute). Every
//     "is this directory under a configured root" check — underConfiguredRoot,
//     and so SeedPath's scope guard — compares against this form, because a
//     caller hands SeedPath a path built the way the operator's own
//     configuration and tooling spell it, not a resolved one.
//   - walk is spelling with its symlinks resolved once, at construction. It
//     exists because filepath.WalkDir opens its root argument with os.Lstat
//     to build the entry it hands its own callback, and a root that is
//     itself a symlink therefore arrives at that callback as "not a
//     directory" — WalkDir never descends past it, so a SeedAll pass over a
//     symlinked root silently finds nothing. Resolving up front, once,
//     rather than re-resolving every pass, is what makes discoverIslands
//     walk the real directory a symlinked root points at.
//
// A root that fails to resolve (does not exist yet, or any other os.Lstat
// error) gets walk == spelling: discoverIslands's own os.Stat on that path
// then fails the same way it always did, and the root is reported through
// CounterRootMissing exactly as before — resolution failing here does not
// invent a new failure shape.
type configuredRoot struct {
	spelling string
	walk     string
}

func resolveRootForWalk(spelling string) string {
	resolved, err := filepath.EvalSymlinks(spelling)
	if err != nil {
		return spelling
	}
	return resolved
}

// NewTrustOnly builds a Seeder that answers the folder-trust question and
// leaves the external-imports one standing, for a directory the runtime should
// still ask about.
//
// It exists for one caller: the compat harness (`muster compat`), which
// has to observe the imports question on a real candidate build and so needs a
// directory that is trusted but not import-approved. The standing seeder can
// never produce one, by design — it writes both answers, so that no directory
// under a configured root ever asks either. The write itself is the same one
// New's Seeder makes (add-only, abandoned on a lost race, atomic), because the
// state file is the runtime's own and rewritten under live sessions; only the
// set of keys differs.
//
// Nothing in the service's own wiring builds one, and nothing should: a
// Seeder that leaves a question standing is the opposite of the standing
// policy this package exists to express.
func NewTrustOnly(statePath, home string, roots []string) *Seeder {
	s := New(statePath, home, roots)
	s.keys = []string{trustKey}
	return s
}

func cleanAbs(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// Enabled reports whether this Seeder has anything to do. False for a nil
// Seeder, an empty statePath, or a roots list that ended up empty after
// New's validation — every other method is a no-op in that case.
func (s *Seeder) Enabled() bool {
	return s != nil && s.statePath != "" && len(s.roots) > 0
}

// Counters returns a snapshot of this Seeder's own counts, in the shape
// driver.CounterReporter expects — see the tmux driver's Counters, which
// merges this in.
func (s *Seeder) Counters() map[string]int64 {
	if s == nil {
		return nil
	}
	s.counters.Lock()
	defer s.counters.Unlock()
	out := make(map[string]int64, len(s.counterMap))
	for k, v := range s.counterMap {
		out[k] = v
	}
	return out
}

func (s *Seeder) incr(name string) {
	s.counters.Lock()
	defer s.counters.Unlock()
	s.counterMap[name]++
}

func (s *Seeder) add(name string, n int) {
	if n <= 0 {
		return
	}
	s.counters.Lock()
	defer s.counters.Unlock()
	s.counterMap[name] += int64(n)
}

// Result reports what one SeedAll pass did — countable, per point 6 of
// muster issue #47's proposed shape, so a regression here is visible
// without a human noticing that work stopped.
type Result struct {
	// Islands is how many repository/worktree roots were found under every
	// configured root this pass.
	Islands int
	// Granted is how many of those actually had the trust key written this
	// pass — zero on a steady-state pass where everything was already set.
	Granted int
	// ImportsGranted is how many project entries had the external-imports
	// approval written this pass. Counted apart from Granted so a caller can
	// see which of the two keys moved (muster #211).
	ImportsGranted int
	// RootsMissing lists configured roots not found on disk this pass. See
	// CounterRootMissing.
	RootsMissing []string
	// LostRace is true when a write this pass was abandoned because the
	// state file changed underneath it. Not an error — see ensureKeys — but
	// worth a caller logging it, since a rate that never resolves is the
	// regression this whole mechanism is countable against.
	LostRace bool
}

func (r Result) String() string {
	return fmt.Sprintf("islands=%d granted=%d imports_granted=%d roots_missing=%d lost_race=%v",
		r.Islands, r.Granted, r.ImportsGranted, len(r.RootsMissing), r.LostRace)
}

// SeedAll walks every configured root, finds every repository and worktree
// root under it (point 2 of the issue's proposed shape: "only roots need a
// key; ordinary subdirectories inherit"), and ensures each carries the trust
// key and the two external-imports keys. Meant to be called once at startup and again on an interval, so a
// worktree created a minute ago is seeded before anything launches into it.
//
// The walk is bounded (maxDepthBelowIsland): a repository nested deeper than
// that below its enclosing repository is left to SeedPath, which seeds it just
// before a session starts there.
//
// Never returns an error for a missing root or a lost race — both are
// ordinary, expected outcomes of running against a filesystem and a file
// something else keeps rewriting, and both are visible in the returned
// Result and in Counters instead. An error return means something this
// package cannot explain, such as the state file being unreadable or not
// the expected JSON shape.
func (s *Seeder) SeedAll() (Result, error) {
	if !s.Enabled() {
		return Result{}, nil
	}
	islands, missing := s.discoverIslands()
	result := Result{Islands: len(islands), RootsMissing: missing}
	s.add(CounterRootMissing, len(missing))
	if len(islands) == 0 {
		return result, nil
	}
	granted, err := s.ensureKeys(islands)
	if errors.Is(err, errLostRace) {
		result.LostRace = true
		s.incr(CounterLostRace)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Granted = granted.trust
	result.ImportsGranted = granted.imports
	s.add(CounterGranted, granted.trust)
	s.add(CounterImportsGranted, granted.imports)
	return result, nil
}

// SeedPath ensures the trust and external-imports keys for one directory's
// enclosing repository or worktree root — or the directory itself, when it sits inside no
// repository at all, the same "only roots need a key" rule SeedAll applies
// to a full sweep. Meant to be called just before a session is started at
// this directory, closing the race a periodic-only pass leaves open for a
// worktree younger than the interval.
//
// Refuses, rather than seeding, a directory whose owning root does not fall
// under any configured root, or that resolves to the home directory or the
// filesystem root — the scope guards in the issue's "Scope guards" section.
// A refusal is not escalated to a hard failure a caller must handle
// specially: it is reported through CounterRefused and a returned error the
// caller is expected to log and otherwise ignore, exactly like a lost race.
// This package seeds a standing policy an operator wrote into configuration;
// it does not get to decide, session by session, that some other directory
// should count as trusted too — that would be this layer making the
// decision prompt.go says belongs to whoever named the directory.
func (s *Seeder) SeedPath(dir string) error {
	if !s.Enabled() || dir == "" {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("trustseed: %s: %w", dir, err)
	}
	abs = filepath.Clean(abs)

	root := s.enclosingRoot(abs)
	if RootRefusal(s.home, root) != "" {
		s.incr(CounterRefused)
		return fmt.Errorf("trustseed: refusing %s: resolves to the home directory or filesystem root", root)
	}
	if !s.underConfiguredRoot(root) {
		s.incr(CounterRefused)
		return fmt.Errorf("trustseed: refusing %s: outside every configured root", root)
	}

	granted, err := s.ensureKeys([]string{root})
	if errors.Is(err, errLostRace) {
		s.incr(CounterLostRace)
		return nil
	}
	if err != nil {
		return err
	}
	s.add(CounterGranted, granted.trust)
	s.add(CounterImportsGranted, granted.imports)
	s.logGranted(root, granted)
	return nil
}

// logGranted says that a create-time seed wrote a key: which directory and
// which questions it answered ahead of time (muster #469). Before this only a
// refusal was logged, so a success could be confirmed only by watching a
// session start without being asked. A seed that wrote nothing — the keys were
// already there, as on every create after the first in a folder — says
// nothing, so the line means "this create changed the state file" and a log
// that is read for it stays short.
//
// Only SeedPath calls it: a SeedAll pass already has its own Result for the
// caller to report, and one line per directory a sweep found would drown
// the log on the first pass of a large root.
func (s *Seeder) logGranted(dir string, g grants) {
	var questions []string
	if g.trust > 0 {
		questions = append(questions, "folder trust")
	}
	if g.imports > 0 {
		questions = append(questions, "external imports")
	}
	if len(questions) == 0 || s.logf == nil {
		return
	}
	s.logf("trustseed: seeded %s at create: answered %s", dir, strings.Join(questions, " and "))
}

// enclosingRoot walks up from dir looking for the nearest ancestor carrying
// a .git entry — a directory or a file, since a worktree's .git is a file
// pointing at its parent repository's internal worktree record, and per the
// issue's own reading of the runtime, "a worktree root is a repository root
// for this purpose". Returns dir itself, unchanged, when no ancestor up to
// the filesystem root carries one — mirroring the runtime's own unbounded
// walk for a directory in no repository at all, per the issue's "What the
// runtime actually checks".
func (s *Seeder) enclosingRoot(dir string) string {
	cur := dir
	for {
		if hasGitEntry(cur) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		cur = parent
	}
}

func hasGitEntry(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

func (s *Seeder) underConfiguredRoot(dir string) bool {
	for _, r := range s.roots {
		if dir == r.spelling || withinRoot(dir, r.spelling) {
			return true
		}
	}
	return false
}

func withinRoot(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// maxDepthBelowIsland bounds how far below its nearest enclosing repository
// (or, for a stretch of the tree no repository encloses, below the configured
// root) the periodic walk descends looking for a repository of its own. The
// count restarts at every repository it finds, so a root that is itself a
// repository, holding repositories, holding worktrees, is walked a few levels
// per tier and never as one deep tree.
//
// Five is one more than the deepest nesting measured on a real fleet's roots
// (four: an unrelated clone and its worktrees four directories under a
// repository), and it is what keeps one application's generated fan-out out of
// the pass: a file cache that names its entries by hash prefix is two hex
// levels of up to 256 directories each, 65 536 directories no repository will
// ever live in, and on that fleet it was 63 % of every pass (#406).
//
// A repository deeper than this is not missed by the service, only by this
// pass: SeedPath seeds the enclosing repository of any directory a session is
// started in, unbounded, just before the start.
const maxDepthBelowIsland = 5

// discoverIslands walks every configured root and returns every directory
// under it that carries a .git entry — point 2's "islands", not every
// directory. A root not found on disk is reported in missing rather than
// silently skipped (the issue: "a configured root that does not exist is a
// configuration error worth reporting"); every OTHER configured root is
// still walked.
//
// The walk itself runs against root.walk — the resolved form — never
// root.spelling, so a root that is itself a symlink is actually descended
// into (muster #213; see configuredRoot's doc comment for why
// WalkDir(spelling) alone cannot do this). Every island the walk finds is
// then re-spelled back onto root.spelling before being returned, so the
// rest of this package keeps working with the path form the operator
// configured and a caller's directory arguments actually use — and so that
// lookupKeys, given that spelling, can resolve its own symlink prefix
// forward and recover the walked form as the second key it writes. Islands
// are reported once each even when two configured roots happen to overlap.
//
// What a pass costs is the number of directories it opens, and nothing is
// spent beyond one listing each (#406). Whether a directory is an island is
// read off the listing already taken to descend into it — a ".git" entry of
// any kind, which is what the Lstat this used to make per directory also
// accepted — instead of a second syscall on the same directory; a directory
// whose listing cannot be taken is the one place that still asks directly. And
// the descent is bounded by maxDepthBelowIsland. Symbolic links are not
// followed, as before.
func (s *Seeder) discoverIslands() (islands []string, missing []string) {
	seen := map[string]bool{}
	for _, root := range s.roots {
		info, err := os.Stat(root.walk)
		if err != nil || !info.IsDir() {
			missing = append(missing, root.spelling)
			continue
		}
		s.walkForIslands(root.walk, 0, func(path string) {
			spelled := reSpell(path, root.walk, root.spelling)
			if !seen[spelled] {
				seen[spelled] = true
				islands = append(islands, spelled)
			}
		})
	}
	return islands, missing
}

// walkForIslands lists dir once, reports dir when it is an island, and
// descends into each subdirectory it may. depth counts directory levels below
// the nearest enclosing island (or the walked root), and restarts at zero in
// every island.
//
// A listing that fails is not fatal to the rest of the walk, the same "report,
// don't abort" shape as a missing root: an unreadable subtree is skipped, and
// whatever entries were read before the failure are still used, as
// filepath.WalkDir does. A directory that cannot be listed but can still be
// searched can hold a .git entry, so that one case asks Lstat directly rather
// than reporting it as not an island.
func (s *Seeder) walkForIslands(dir string, depth int, found func(path string)) {
	entries, err := os.ReadDir(dir)
	if s.onList != nil {
		s.onList(dir)
	}
	island := hasGitName(entries)
	if !island && err != nil {
		island = s.lstatHasGit(dir)
	}
	if island {
		found(dir)
		depth = 0
	}
	for _, e := range entries {
		if !e.IsDir() || skipDescent(e.Name()) {
			continue
		}
		if depth >= maxDepthBelowIsland {
			continue
		}
		s.walkForIslands(filepath.Join(dir, e.Name()), depth+1, found)
	}
}

// hasGitName reports whether a directory listing carries a .git entry: a
// directory (a repository) or a file (a worktree, whose .git points at the
// repository's own record of it), or anything else under that name.
func hasGitName(entries []fs.DirEntry) bool {
	for _, e := range entries {
		if e.Name() == ".git" {
			return true
		}
	}
	return false
}

// lstatHasGit is hasGitEntry through the Seeder's test hook, for the walk's
// one remaining direct question.
func (s *Seeder) lstatHasGit(dir string) bool {
	if s.onLstat != nil {
		s.onLstat(dir)
	}
	return hasGitEntry(dir)
}

// reSpell rewrites path — found by walking walkRoot, the resolved form of a
// configured root — back into the operator's own spelling of that root, by
// replacing the walkRoot prefix with spelling. When walkRoot and spelling are
// equal (the ordinary case: no symlink was involved) this returns path
// unchanged. Falls back to path unchanged if path does not actually fall
// under walkRoot, which should not happen since it was produced by walking
// walkRoot in the first place.
func reSpell(path, walkRoot, spelling string) string {
	rel, err := filepath.Rel(walkRoot, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	if rel == "." {
		return spelling
	}
	return filepath.Join(spelling, rel)
}

// skipDescent names directories that are never themselves a repository root
// and are expensive to walk into — a performance guard, not a correctness
// one: nothing here is treated as a root regardless of this list, it only
// controls whether the walk continues underneath a name known never to hold
// one. Checked against every directory except the configured root itself, so
// an operator who names an unusually-named root is never silently excluded.
func skipDescent(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".venv", "venv",
		"dist", "build", ".next", "target", "__pycache__":
		return true
	}
	return false
}

// grants is what one committed write set, by which key. The two are counted
// apart (see CounterGranted and CounterImportsGranted) so a caller can tell
// which of the questions this package is answering.
type grants struct {
	trust   int // project entries whose folder-trust key was set to true
	imports int // project entries whose external-imports approval was set to true
}

// seededKeys are the fields ensureKeys sets on every target's project entry
// (for a Seeder New built; NewTrustOnly narrows it), each independently: a project the runtime already knows to be trusted still
// gets the imports keys, and one whose imports were already approved still gets
// the trust key. A key that is already true is left exactly as it is.
var seededKeys = []string{trustKey, importsApprovedKey, importsShownKey}

// ensureKeys is the add-only, race-tolerant writer every exported method
// funnels through. It sets every key in s.keys true for every path variant
// of every target that does not already carry it, and commits the whole batch
// in one write — or abandons the whole batch (errLostRace) the moment the state
// file is found to have changed since it was read. There is no partial
// commit: a caller that wants some of a batch seeded even when the rest
// raced would be reading a promise this function does not make.
//
// Each key is judged on its own, so a project entry the runtime wrote with the
// imports keys present and false (its default, measured on a real state file)
// is answered, not skipped because a neighbouring key was already true. A key
// already true is never rewritten and no key is ever removed.
func (s *Seeder) ensureKeys(targets []string) (granted grants, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	before, err := os.Stat(s.statePath)
	if err != nil {
		return grants{}, fmt.Errorf("trustseed: %s: %w", s.statePath, err)
	}
	raw, err := os.ReadFile(s.statePath)
	if err != nil {
		return grants{}, fmt.Errorf("trustseed: reading %s: %w", s.statePath, err)
	}
	if s.afterRead != nil {
		s.afterRead()
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return grants{}, fmt.Errorf("trustseed: %s is not the expected object shape: %w", s.statePath, err)
	}
	projects := map[string]json.RawMessage{}
	if pr, ok := top["projects"]; ok {
		if err := json.Unmarshal(pr, &projects); err != nil {
			return grants{}, fmt.Errorf("trustseed: %s#projects is not the expected object shape: %w", s.statePath, err)
		}
	}

	dirty := false
	for _, target := range targets {
		for _, key := range lookupKeys(target) {
			entry := map[string]json.RawMessage{}
			if existing, ok := projects[key]; ok {
				if err := json.Unmarshal(existing, &entry); err != nil {
					// A project entry that will not parse is a fact worth
					// leaving alone rather than guessing at — skip it,
					// never overwrite a shape this package does not
					// understand with one it invented.
					continue
				}
				if entry == nil {
					// A JSON null decodes to a nil map, which cannot be
					// assigned into. It is not a shape this package would
					// have invented either, but it holds no information a
					// fresh entry would overwrite.
					entry = map[string]json.RawMessage{}
				}
			}
			var set grants
			changed := false
			for _, k := range s.keys {
				if isTrue(entry[k]) {
					continue
				}
				entry[k] = json.RawMessage("true")
				changed = true
				switch k {
				case trustKey:
					set.trust++
				case importsApprovedKey:
					set.imports++
				}
			}
			if !changed {
				continue
			}
			encoded, err := json.Marshal(entry)
			if err != nil {
				// Nothing is committed for this entry, so nothing it would have
				// set is counted: a failure to encode is not a grant.
				continue
			}
			granted.trust += set.trust
			granted.imports += set.imports
			projects[key] = encoded
			dirty = true
		}
	}

	if !dirty {
		return grants{}, nil
	}

	encodedProjects, err := json.Marshal(projects)
	if err != nil {
		return grants{}, fmt.Errorf("trustseed: encoding projects: %w", err)
	}
	top["projects"] = encodedProjects
	out, err := json.Marshal(top)
	if err != nil {
		return grants{}, fmt.Errorf("trustseed: encoding %s: %w", s.statePath, err)
	}

	// The check this whole package exists for: the state file is rewritten
	// wholesale by something else entirely while this was being computed.
	// Committing anyway would silently discard whatever that something else
	// just wrote. Abandoning costs nothing — the merge above is add-only, so
	// nothing this pass would have contributed is lost, only delayed to the
	// next one.
	after, statErr := os.Stat(s.statePath)
	if statErr != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		return grants{}, errLostRace
	}

	if err := writeFileAtomic(s.statePath, out, before.Mode()); err != nil {
		return grants{}, fmt.Errorf("trustseed: writing %s: %w", s.statePath, err)
	}
	return granted, nil
}

// isTrue reports whether one project-entry field is the JSON literal true.
// Absent, false, null and any other type all read as "not yet answered" — the
// runtime itself writes the imports keys as false before they are answered.
func isTrue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false
	}
	return b
}

// lookupKeys returns the path forms the runtime is known to check a project
// key under — see the issue's "What the runtime actually checks": the plain
// path and the resolved-symlink path. Deliberately missing the third form
// the issue names, the runtime's own unicode-NFC-normalized path: the
// standard library has no Unicode-normalization routine, and go.mod's own
// zero requirements are, per internal/state's package doc, a standing
// decision this package keeps rather than one it would be adding an
// exception to. For a path made only of ASCII bytes — every root this
// fleet has configured, to date — a string already equals its own NFC form,
// so the plain key already covers it; a path containing a non-ASCII byte
// gets only the forms below, a known, documented gap rather than a silently
// wrong guess. If a non-ASCII root is ever configured, this is the function
// to revisit.
func lookupKeys(path string) []string {
	keys := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		keys = append(keys, resolved)
	}
	return keys
}

// writeFileAtomic mirrors internal/state.Store.Save's write discipline (temp
// file in the same directory, fsynced, then renamed over the target) against
// an arbitrary path rather than one inside a state directory this service
// owns outright — this file is the RUNTIME's, so its existing permission
// bits are preserved explicitly rather than left to os.CreateTemp's default.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".trustseed-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	return os.Rename(tmpName, path)
}
