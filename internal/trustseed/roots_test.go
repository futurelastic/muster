package trustseed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// linkTo makes link a symlink to target, skipping the test where the platform
// will not let an unprivileged test create one.
func linkTo(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

// TestRootRefusalNamesAHomeOrFilesystemRoot is muster #469: a root equal to the
// home directory or "/" can never be a trust root — spelled plainly, spelled with
// a trailing separator or a dot, or reached through a symlink — and says why, so
// the caller can tell the operator instead of the root vanishing.
func TestRootRefusalNamesAHomeOrFilesystemRoot(t *testing.T) {
	home := tempHome(t)
	sep := string(filepath.Separator)
	link := filepath.Join(tempHome(t), "to-home")
	linkTo(t, home, link)

	// A home that is itself reached through a link: the operator's environment
	// says one spelling, the root names the other.
	homeLink := filepath.Join(tempHome(t), "home-by-link")
	linkTo(t, home, homeLink)

	cases := []struct {
		name, home, root string
		want             string
	}{
		{"home itself", home, home, "the home directory"},
		{"home with a trailing separator", home, home + sep, "the home directory"},
		{"home spelled through a dot", home, filepath.Join(home, "sub", ".."), "the home directory"},
		{"a symlink to home", home, link, "the home directory"},
		{"a symlink to home, as the root spelling of a home reached by link", homeLink, home, "the home directory"},
		{"the filesystem root", home, sep, "the filesystem root"},
		{"a folder under home", home, filepath.Join(home, "projects"), ""},
		{"a folder that does not exist yet", home, filepath.Join(home, "not", "yet"), ""},
		{"a relative root has no answer here", home, "relative/path", ""},
		{"an empty root has no answer here", home, "", ""},
		{"home unknown", "", filepath.Join(home, "projects"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RootRefusal(c.home, c.root); got != c.want {
				t.Errorf("RootRefusal(%q, %q) = %q, want %q", c.home, c.root, got, c.want)
			}
		})
	}
}

// TestActiveRootsKeepsAValidRootBesideADroppedOne is the reading #469 asked for:
// what is active is what New keeps, so the one dropped root does not take its
// valid neighbour with it, and the count is of the roots, not the list.
func TestActiveRootsKeepsAValidRootBesideADroppedOne(t *testing.T) {
	home := tempHome(t)
	valid := filepath.Join(home, "projects")
	link := filepath.Join(tempHome(t), "to-home")
	linkTo(t, home, link)

	cases := []struct {
		name  string
		roots []string
		want  []string
	}{
		{"home alone", []string{home}, nil},
		{"the filesystem root alone", []string{string(filepath.Separator)}, nil},
		{"a symlink to home alone", []string{link}, nil},
		{"home beside a valid root", []string{home, valid}, []string{valid}},
		{"a symlink to home beside a valid root", []string{link, valid}, []string{valid}},
		{"the filesystem root beside a valid root", []string{string(filepath.Separator), valid}, []string{valid}},
		{"a valid root repeated", []string{valid, valid + string(filepath.Separator)}, []string{valid}},
		{"a relative and an empty root are not active", []string{"relative", "", valid}, []string{valid}},
		{"two valid roots keep their order", []string{valid, filepath.Join(home, "work")}, []string{valid, filepath.Join(home, "work")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ActiveRoots(home, c.roots)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ActiveRoots(%v) = %v, want %v", c.roots, got, c.want)
			}
		})
	}
}

// TestNewSeedsUnderTheValidRootBesideAHomeRoot pins that dropping home does not
// disable the feature: the seeder built from [home, valid] is enabled, and a
// session created under valid is seeded.
func TestNewSeedsUnderTheValidRootBesideAHomeRoot(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	valid := filepath.Join(home, "projects")
	folder := filepath.Join(valid, "plain-folder")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(statePath, home, []string{home, valid})
	if !s.Enabled() {
		t.Fatal("a valid root beside a dropped one must stay active")
	}
	if err := s.SeedPath(folder); err != nil {
		t.Fatalf("SeedPath under the valid root: %v", err)
	}
	var projects map[string]json.RawMessage
	_ = json.Unmarshal(readState(t, statePath)["projects"], &projects)
	if _, ok := projects[folder]; !ok {
		t.Errorf("the folder under the valid root was not seeded; projects = %v", projects)
	}
}

// TestNewWithOnlyAHomeRootIsDisabledAndSaysNothingActive: the case in the field
// report. Every spelling of home or "/" leaves the seeder with nothing to do.
func TestNewWithOnlyAHomeRootIsDisabledAndSaysNothingActive(t *testing.T) {
	home := tempHome(t)
	link := filepath.Join(tempHome(t), "to-home")
	linkTo(t, home, link)
	for name, root := range map[string]string{
		"home":       home,
		"filesystem": string(filepath.Separator),
		"symlink":    link,
	} {
		t.Run(name, func(t *testing.T) {
			s := New(filepath.Join(home, ".claude.json"), home, []string{root})
			if s.Enabled() {
				t.Error("a seeder whose only root is refused must not be enabled")
			}
			if got := ActiveRoots(home, []string{root}); len(got) != 0 {
				t.Errorf("ActiveRoots = %v, want none", got)
			}
		})
	}
}

// TestSeedPathRefusesAnEnclosingRootThatResolvesToHome: the guard in SeedPath is
// the same definition as the one in New. A folder under a valid root that is a
// link to home, with a repository in home, has an enclosing root whose spelling
// is not home but whose directory is — it is refused like home is.
func TestSeedPathRefusesAnEnclosingRootThatResolvesToHome(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	mkRepo(t, home) // home is itself a repository, as a dotfiles checkout makes it
	valid := filepath.Join(tempHome(t), "projects")
	if err := os.MkdirAll(valid, 0o755); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(valid, "via-link")
	linkTo(t, home, via)

	s := New(statePath, home, []string{valid})
	if err := s.SeedPath(via); err == nil {
		t.Error("a directory whose enclosing root resolves to home must be refused")
	}
	if s.Counters()[CounterRefused] != 1 {
		t.Errorf("CounterRefused = %d, want 1", s.Counters()[CounterRefused])
	}
	var projects map[string]json.RawMessage
	_ = json.Unmarshal(readState(t, statePath)["projects"], &projects)
	if _, ok := projects[via]; ok {
		t.Error("a refused path must never be written")
	}
}

// capturedLog collects what a Seeder says through logf.
type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLog) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *capturedLog) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// TestSeedPathLogsOneLineWhenItWritesAKeyAndNothingWhenItDoesNot is the ask of
// #469's item 6: a create-time seed that set a key names the directory and the
// questions it answered, once; a create whose keys were already there is
// silent, so the line means "this create changed the state file".
func TestSeedPathLogsOneLineWhenItWritesAKeyAndNothingWhenItDoesNot(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "projects")
	folder := filepath.Join(root, "plain-folder")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}

	var cl capturedLog
	s := New(statePath, home, []string{root})
	s.logf = cl.logf

	if err := s.SeedPath(folder); err != nil {
		t.Fatal(err)
	}
	lines := cl.all()
	if len(lines) != 1 {
		t.Fatalf("first create logged %d lines, want 1: %q", len(lines), lines)
	}
	for _, want := range []string{folder, "folder trust", "external imports"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line %q does not say %q", lines[0], want)
		}
	}

	if err := s.SeedPath(folder); err != nil {
		t.Fatal(err)
	}
	if got := cl.all(); len(got) != 1 {
		t.Errorf("a create whose keys were already present logged again: %q", got)
	}
}

// TestSeedPathLogNamesOnlyTheQuestionItAnswered: a folder the runtime already had
// trusted but never asked the imports question of is answered for the imports
// alone, and the line says that, not both.
func TestSeedPathLogNamesOnlyTheQuestionItAnswered(t *testing.T) {
	home := tempHome(t)
	root := filepath.Join(home, "projects")
	folder := filepath.Join(root, "half-known")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, map[string]any{
		"projects": map[string]any{
			folder: map[string]any{"hasTrustDialogAccepted": true},
		},
	})

	var cl capturedLog
	s := New(statePath, home, []string{root})
	s.logf = cl.logf
	if err := s.SeedPath(folder); err != nil {
		t.Fatal(err)
	}
	lines := cl.all()
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want 1: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "folder trust") || !strings.Contains(lines[0], "external imports") {
		t.Errorf("the line %q should name the imports question alone", lines[0])
	}
}

// TestSeedPathRefusalStaysSilentInTheSuccessLog: the success line is not a second
// voice for an outcome that already has one (the refusal's returned error, which
// the caller logs).
func TestSeedPathRefusalStaysSilentInTheSuccessLog(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var cl capturedLog
	s := New(statePath, home, []string{root})
	s.logf = cl.logf
	if err := s.SeedPath(filepath.Join(home, "elsewhere")); err == nil {
		t.Fatal("expected a refusal outside every configured root")
	}
	if got := cl.all(); len(got) != 0 {
		t.Errorf("a refusal wrote a success line: %q", got)
	}
}
