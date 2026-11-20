package opencode

import "path/filepath"

// The toolchain a confined session's `git` needs (muster #484).
//
// On macOS `/usr/bin/git` is not git: it is a shim that asks the developer
// directory `xcode-select` chose for the real one, and loads that directory's
// own library before it runs anything. The profile reads the system and the
// stock Xcode app and nothing else, so when the chosen directory is a versioned
// Xcode app (or anything else outside those roots) every `git` in a confined
// session dies with `xcrun: error: unable to load libxcrun`.
//
// The session cannot fix that itself: setting DEVELOPER_DIR is the workaround,
// and it has to be set on every command, in every session. So the driver sets it,
// where the answer is known — when it builds the profile and the environment
// together.

// developerDirEnv is the variable the shim reads before it consults the
// selection. A driver-owned name on macOS: a create may not set it.
const developerDirEnv = "DEVELOPER_DIR"

// xcrunDBEnv is the variable the toolchain shim reads for the path of its lookup
// cache (muster #505). Its name is lower case: the library spells it that way
// and the environment is case-sensitive. A driver-owned name on macOS: a create
// may not set it.
//
// The shim keeps that cache in the per-user temporary directory the system names
// (confstr's _CS_DARWIN_USER_TEMP_DIR), not in TMPDIR, so a confined session is
// refused the write on every `git`, `cc` or `python3` it runs — `couldn't create
// cache file '…/xcrun_db-…' (errno=Operation not permitted)` — and the message
// lands in every tool output the model reads. The directory also holds other
// processes' files, so it is not opened to the session; the variable moves the
// cache into the session's own tmp instead. It must be absolute: the library
// ignores a relative value and falls back to the refused directory.
const xcrunDBEnv = "xcrun_db"

// xcrunCacheEnv is the environment a session started under a sandbox profile is
// given so the toolchain shim's cache is inside the session's own directory dir
// (muster #505). Only macOS has the shim; an unconfined session is given none of
// it, for the reason DEVELOPER_DIR is not given to one: nothing refuses its write,
// and it keeps the cache the rest of the user's processes share.
func xcrunCacheEnv(goos, dir string) map[string]string {
	if goos != "darwin" {
		return nil
	}
	return map[string]string{xcrunDBEnv: filepath.Join(dir, "tmp", "xcrun_db")}
}

// commandLineToolsDir is where the Command Line Tools package installs. It sits
// under /Library, which the profile already reads.
const commandLineToolsDir = "/Library/Developer/CommandLineTools"

// developerToolchain reports the developer directory the host has selected (real
// path, empty when none is) and the Command Line Tools directory when it holds a
// git a session can run (empty when it does not). A variable so a test can state
// a host it is not running on; the per-platform answer is in developer_darwin.go
// and developer_other.go.
var developerToolchain = hostDeveloperToolchain

// developerAccess decides what a confined session needs so that the shim finds a
// toolchain it may read, given the profile's read set:
//
//   - the selected directory is already readable (or nothing is selected, so the
//     shim falls back to a default under a root the profile reads): nothing is
//     added, and the shim chooses as it always did;
//   - it is not, and the Command Line Tools are installed: env is that directory,
//     so DEVELOPER_DIR points the shim at a toolchain the profile reads already;
//   - it is not, and they are not: grant is the selected directory, read-only.
//
// Command Line Tools win over widening the profile, in that order, because the
// directory they live in is already readable: the session sees no more of the
// host than it did. The cost is that a session which needs the full Xcode
// toolchain (a simulator, a build of an app) gets the smaller one; that session
// is not served by this and needs the stock Xcode app, or a root it names itself.
func developerAccess(selected, clt string, read []string) (env, grant string) {
	if selected == "" || covered(read, selected) {
		return "", ""
	}
	if clt != "" && covered(read, clt) {
		return clt, ""
	}
	return "", selected
}

// covered reports whether any root in read includes path.
func covered(read []string, path string) bool {
	for _, r := range read {
		if coversOrIs(r, path) {
			return true
		}
	}
	return false
}
