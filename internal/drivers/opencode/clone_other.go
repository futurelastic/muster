//go:build !darwin

package opencode

// cloneDirectory is darwin's clonefileat(2) (clone_darwin.go). Elsewhere there
// is no such call this driver reaches, so copyTree goes straight to its copies.
func cloneDirectory(src, dst string) error { return errCloneUnsupported }

// hostClones: this host has no call that clones a directory, so every private
// copy is a full one and there is no cross-volume difference to refuse (#448).
func hostClones() bool { return false }

// cpCloneFlag is empty here: no `cp` flag means "clone" on this platform. GNU
// coreutils' -c is --preserve=context and succeeds on a plain copy, so passing it
// would make a full copy read as a clone.
const cpCloneFlag = ""
