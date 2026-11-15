//go:build !darwin

package opencode

// Only macOS has a shim that loads its toolchain from a chosen directory.
func hostDeveloperToolchain() (selected, clt string) { return "", "" }
