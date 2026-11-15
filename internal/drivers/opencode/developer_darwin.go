//go:build darwin

package opencode

import (
	"os"
	"path/filepath"
)

// selectionLink is where `xcode-select --switch` records the chosen developer
// directory, as a symlink. Read directly rather than by running xcode-select:
// the answer is a path under /private/var/db, which every profile reads, and no
// child process is started for it.
const selectionLink = "/var/db/xcode_select_link"

func hostDeveloperToolchain() (selected, clt string) {
	if target, err := os.Readlink(selectionLink); err == nil && filepath.IsAbs(target) {
		selected = resolvePath(target)
	}
	// The shim needs git there, not only a directory: a half-installed package
	// would fail differently, with a message that names no sandbox.
	git := filepath.Join(commandLineToolsDir, "usr", "bin", "git")
	if info, err := os.Stat(git); err == nil && info.Mode().IsRegular() {
		clt = resolvePath(commandLineToolsDir)
	}
	return selected, clt
}
