package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// hooks.pre-commit — will a commit made in this clone be scanned for secrets?
// (muster #201)
//
// # Why a row, and why here
//
// The secret guard that runs before a value is published is
// .githooks/pre-commit, and it is switched on by `git config core.hooksPath`,
// which lives in one clone's own .git/config. Nothing in the repository can
// carry it to another clone, so a clone that never ran .githooks/install.sh
// commits with no scan in front of it and says nothing: measured, a commit
// quoting a key-shaped value went through in silence in such a clone, where
// the scanner run by hand would have refused it. #199 ruled that a branch push
// is not scanned by CI *because* the hook is the guard that works first (#299
// has since put branch pushes through CI, but the hook is still the only guard
// that runs before a value is published) — so the hook being absent is not a
// small thing to leave unreported.
//
// Every other row here answers for the installation. This one answers for the
// clone doctor is run from, which is a different object: an installed
// service machine usually has no clone at all, and one that does may hold it
// only to build from. So the row is answered only when the working directory
// is inside a clone of this repository, and is `skip` everywhere else — an
// adopter running doctor on a service host sees a skipped row, never a
// finding about a repository they do not have.
//
// # What it asks
//
// Not "is core.hooksPath set" — that reads one spelling of the answer. It asks
// git which file it WOULD run as the pre-commit hook (`rev-parse --git-path`),
// which already folds in a relative or absolute hooksPath, a linked worktree,
// and a global setting, then compares that file with this repository's tracked
// hook, and checks the executable bit git requires before it will run a hook
// at all. Then it checks the hook's own dependency: with no gitleaks on PATH
// the hook prints a notice and lets the commit through, so a `pass` that
// ignored it would be the silent-verify shape this command exists to end.
//
// # Why warn, never fail (ADR 160, ADR 201)
//
// A clone kept only to read or to build from legitimately has no hook, and the
// hook is no dependency of the service. So the state is one that is right
// somewhere, and the row names why it might be wrong. The exit code is
// unchanged.
//
// It reads the invoking shell's git configuration and PATH, like every other
// row reads the invoking shell's environment. A commit made from an editor
// with a barer PATH can still find no gitleaks; the row says what THIS shell
// would do.

const (
	preCommitHookRel = ".githooks/pre-commit"
	// installer is repo-relative on purpose: rows never print a filesystem path.
	installerRel = ".githooks/install.sh"
)

func checkPreCommitHook(ctx context.Context, env doctorEnv) doctorRow {
	row := doctorRow{ID: "hooks.pre-commit", Refs: []int{201, 199}}

	top := cloneRoot(env.Dir)
	if top == "" {
		row.Status = statusSkip
		row.Summary = "not run from inside a clone of this repository"
		return row
	}

	lookPath := env.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	gitBin, err := lookPath("git")
	if err != nil {
		row.Status = statusUnknown
		row.Summary = "git is not on PATH, so what it would run at commit time cannot be asked"
		return row
	}

	hook, err := effectivePreCommit(ctx, env, gitBin, top)
	if err != nil {
		row.Status = statusUnknown
		row.Summary = "git could not say which pre-commit hook it would run here"
		return row
	}

	effective, err := os.Stat(hook)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		row.Status = statusWarn
		row.Summary = "git runs no pre-commit hook in this clone — a commit here is not scanned for secrets"
		row.Detail = "core.hooksPath is per-clone git config and does not travel with the repository; run " + installerRel + " once in this clone (--skip=hooks.pre-commit if this clone never commits)"
		return row
	case err != nil:
		row.Status = statusUnknown
		row.Summary = "the pre-commit hook git would run could not be read"
		return row
	}

	tracked, err := os.Stat(filepath.Join(top, filepath.FromSlash(preCommitHookRel)))
	if err != nil || !os.SameFile(effective, tracked) {
		row.Status = statusWarn
		row.Summary = "git runs a pre-commit hook here, but not this repository's " + preCommitHookRel
		row.Detail = "it may scan for secrets too, and this row cannot tell; run " + installerRel + " to use the repository's own"
		return row
	}

	if effective.Mode()&0o111 == 0 {
		row.Status = statusWarn
		row.Summary = "this repository's pre-commit hook is selected but is not executable, so git skips it"
		row.Detail = "run " + installerRel + ", which sets the executable bit"
		return row
	}

	if _, err := lookPath("gitleaks"); err != nil {
		row.Status = statusWarn
		row.Summary = "the pre-commit hook is on, but gitleaks is not on this PATH — the hook prints a notice and lets the commit through unscanned"
		row.Detail = "install gitleaks where commits are made; the hook's own message names how"
		return row
	}

	row.Status = statusPass
	row.Summary = "git runs this repository's pre-commit hook, and gitleaks is on this PATH"
	return row
}

// cloneRoot returns the nearest directory at or above dir that is the top of a
// checkout of this repository, or "" when there is none. The marker is the
// tracked hook plus the service's own command directory: the hook alone is
// shared by every repository cut from the same scaffold, and a finding about
// somebody else's hook is worse than none.
func cloneRoot(dir string) string {
	if dir == "" {
		return ""
	}
	for d := filepath.Clean(dir); ; {
		if isFile(filepath.Join(d, filepath.FromSlash(preCommitHookRel))) &&
			isDir(filepath.Join(d, "cmd", "muster")) {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// effectivePreCommit asks git for the path of the pre-commit hook it would run
// from top, made absolute. The answer is relative to the directory git ran in
// when core.hooksPath is a relative path, hence -C top.
func effectivePreCommit(ctx context.Context, env doctorEnv, gitBin, top string) (string, error) {
	if env.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, env.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", top, "rev-parse", "--git-path", "hooks/pre-commit")
	cmd.Env = withoutRepoSelectors(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errors.New("git printed no path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(top, path)
	}
	return path, nil
}

// withoutRepoSelectors drops the variables that make git ignore -C and answer
// for some other repository — set, for instance, when doctor is run from inside
// a git hook or an exec step of a rebase. Configuration variables are kept: the
// row is meant to answer with the caller's git configuration.
func withoutRepoSelectors(environ []string) []string {
	drop := []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_COMMON_DIR=", "GIT_INDEX_FILE=", "GIT_PREFIX="}
	kept := make([]string, 0, len(environ))
next:
	for _, kv := range environ {
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				continue next
			}
		}
		kept = append(kept, kv)
	}
	return kept
}
