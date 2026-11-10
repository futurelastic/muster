//go:build !darwin

package opencode

// cloneDirectory is darwin's clonefileat(2) (clone_darwin.go). Elsewhere there
// is no such call this driver reaches, so copyTree goes straight to its copies.
func cloneDirectory(src, dst string) error { return errCloneUnsupported }
