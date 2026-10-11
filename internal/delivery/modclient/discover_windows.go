//go:build windows

package modclient

import "os"

// modeProblem has nothing to judge on Windows. Go reports a made-up mode there
// (it never carries an execute bit, and "writable by others" is an ACL, not a
// bit), so applying the POSIX test would refuse every module, and any other
// reading of the mode would be a guess. What remains is that the entry is a
// regular file that is not a symbolic link, which checkExecutable has already
// established; whether the process may be created is decided by the operating
// system at exec time.
//
// What is NOT checked here, and is therefore the install's job: that the
// modules directory is writable only by the account that runs the service (its
// default ACL under a per-machine install directory does that). Judging a file's
// DACL for write access by a third party is a follow-up to the Windows port
// (#303), not something to approximate from mode bits.
func modeProblem(os.FileMode) string { return "" }
