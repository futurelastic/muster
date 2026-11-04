package trustseed

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// walkRecorder wires a Seeder's walk hooks and returns what the walk listed
// and what it had to ask about directly.
func walkRecorder(s *Seeder) (listed, lstatted *[]string) {
	var l, q []string
	s.onList = func(dir string) { l = append(l, dir) }
	s.onLstat = func(dir string) { q = append(q, dir) }
	return &l, &q
}

func relAll(t *testing.T, root string, paths []string) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		r, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(r))
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A pass lists each directory it visits once and asks about none of them
// separately. The Lstat this used to make per directory was 26 % of a pass's
// CPU on a real fleet's roots, on top of the listing already taken to descend
// (#406); whether a directory is an island is in that listing.
func TestSeedAllPassListsEachDirectoryOnceAndAsksAboutNone(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "workspace")

	mkRepo(t, filepath.Join(root, "acme", "widgets"))
	mkWorktree(t, filepath.Join(root, "acme", "widgets", ".claude", "worktrees", "feature-x"))
	if err := os.MkdirAll(filepath.Join(root, "acme", "notes", "2026"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(statePath, home, []string{root})
	listed, lstatted := walkRecorder(s)
	islands, missing := s.discoverIslands()
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if len(*lstatted) != 0 {
		t.Fatalf("the walk asked about %d directories directly, want 0: %v", len(*lstatted), *lstatted)
	}

	want := []string{
		".",
		"acme",
		"acme/notes",
		"acme/notes/2026",
		"acme/widgets",
		"acme/widgets/.claude",
		"acme/widgets/.claude/worktrees",
		"acme/widgets/.claude/worktrees/feature-x",
	}
	if got := relAll(t, root, *listed); !equalStrings(got, want) {
		t.Fatalf("listed = %v, want each once: %v", got, want)
	}
	wantIslands := []string{"acme/widgets", "acme/widgets/.claude/worktrees/feature-x"}
	if got := relAll(t, root, islands); !equalStrings(got, wantIslands) {
		t.Fatalf("islands = %v, want %v (a repository .git directory and a worktree .git file)", got, wantIslands)
	}
}

// The directories skipDescent names are neither entered nor listed, and a
// .git inside one is not an island — unchanged by #406, pinned because the
// walk is now hand-written rather than filepath.WalkDir's.
func TestSeedAllPassDoesNotEnterSkippedNames(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "workspace")

	mkRepo(t, filepath.Join(root, "app"))
	mkRepo(t, filepath.Join(root, "app", "node_modules", "dep"))
	mkRepo(t, filepath.Join(root, "app", "vendor", "dep"))

	s := New(statePath, home, []string{root})
	listed, _ := walkRecorder(s)
	islands, _ := s.discoverIslands()

	if got := relAll(t, root, islands); !equalStrings(got, []string{"app"}) {
		t.Fatalf("islands = %v, want only app", got)
	}
	for _, dir := range *listed {
		if strings.Contains(dir, "node_modules") || strings.Contains(dir, "vendor") {
			t.Fatalf("listed %s, which sits under a directory skipDescent names", dir)
		}
	}
}

// A symbolic link to a directory is not followed, as filepath.WalkDir never
// did, so a link that points back up the tree cannot make a pass endless and a
// link into another tree does not widen what is seeded.
func TestSeedAllPassDoesNotFollowASymlinkedDirectory(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	outside := filepath.Join(home, "elsewhere", "repo")
	mkRepo(t, outside)
	root := filepath.Join(home, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s := New(statePath, home, []string{root})
	islands, _ := s.discoverIslands()
	if len(islands) != 0 {
		t.Fatalf("islands = %v, want none — nothing real sits under the root", islands)
	}
}

// Depth is counted below the nearest enclosing repository, and the pass goes no
// deeper than maxDepthBelowIsland: a repository exactly that deep is found, one
// level more is not, and nothing past the bound is listed at all. The count
// restarts in every repository found, so a worktree the same distance below
// that repository is found too.
func TestSeedAllPassDescendsAtMostMaxDepthBelowAnIsland(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "workspace")

	chain := func(base string, levels int) string {
		p := base
		for i := 0; i < levels; i++ {
			p = filepath.Join(p, "d"+string(rune('a'+i)))
		}
		return p
	}
	atBound := chain(filepath.Join(root, "left"), maxDepthBelowIsland-1)
	pastBound := chain(filepath.Join(root, "right"), maxDepthBelowIsland)
	mkRepo(t, atBound)
	mkRepo(t, pastBound)
	mkWorktree(t, filepath.Join(atBound, "wt"))
	// A generated fan-out below the bound that no repository will ever sit in.
	for _, hex := range []string{"00", "01", "ff"} {
		if err := os.MkdirAll(filepath.Join(pastBound, "cache", "data", hex, hex), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	s := New(statePath, home, []string{root})
	listed, _ := walkRecorder(s)
	islands, _ := s.discoverIslands()

	got := relAll(t, root, islands)
	wantFound := filepath.ToSlash(strings.TrimPrefix(atBound, root+string(filepath.Separator)))
	want := []string{wantFound, wantFound + "/wt"}
	if !equalStrings(got, want) {
		t.Fatalf("islands = %v, want %v (the repository exactly at the bound, and a worktree below it; not the one a level past it)", got, want)
	}

	// The repository under "right" is maxDepthBelowIsland directories below the
	// root, one past the bound, so it and everything under it is never listed.
	for _, dir := range *listed {
		if dir == pastBound || strings.HasPrefix(dir, pastBound+string(filepath.Separator)) {
			t.Fatalf("listed %s, past the depth bound", dir)
		}
	}
}

// What the periodic pass leaves out for being deeper than the bound, SeedPath
// still seeds: it finds the enclosing repository of the directory it is given
// without any depth limit, just before a session starts there. That is why the
// bound can be where it is.
func TestSeedPathStillSeedsARepositoryTheBoundedPassLeavesOut(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "workspace")

	deep := root
	for i := 0; i < maxDepthBelowIsland+2; i++ {
		deep = filepath.Join(deep, "level")
	}
	mkRepo(t, deep)
	inside := filepath.Join(deep, "src")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(statePath, home, []string{root})
	result, err := s.SeedAll()
	if err != nil {
		t.Fatal(err)
	}
	if result.Islands != 0 {
		t.Fatalf("SeedAll found %d islands, want 0 — the repository is past the bound", result.Islands)
	}
	if err := s.SeedPath(inside); err != nil {
		t.Fatal(err)
	}
	wantAllThreeTrue(t, projectEntry(t, statePath, deep), "the repository enclosing the directory SeedPath was given")
}

// A directory whose listing cannot be taken is the one case the walk asks
// about directly: a directory that can be searched but not listed can still
// hold a .git entry, and the Lstat this replaced reported it as an island.
func TestSeedAllPassAsksDirectlyAboutADirectoryItCannotList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not describe listing on this platform")
	}
	if os.Geteuid() == 0 {
		t.Skip("root lists any directory")
	}
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "workspace")

	opaque := filepath.Join(root, "opaque")
	mkRepo(t, opaque)
	if err := os.Chmod(opaque, 0o111); err != nil { // searchable, not listable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(opaque, 0o755) })
	if _, err := os.ReadDir(opaque); err == nil {
		t.Skip("this filesystem lists a directory without read permission")
	}

	s := New(statePath, home, []string{root})
	_, lstatted := walkRecorder(s)
	islands, _ := s.discoverIslands()

	if got := relAll(t, root, islands); !equalStrings(got, []string{"opaque"}) {
		t.Fatalf("islands = %v, want [opaque] — it holds a .git entry although it cannot be listed", got)
	}
	if got := relAll(t, root, *lstatted); !equalStrings(got, []string{"opaque"}) {
		t.Fatalf("asked directly about %v, want only the directory that could not be listed", got)
	}
}
