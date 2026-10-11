//go:build !windows

package main

// detachConsole is Windows-only (console_windows.go): there is no console
// window to release anywhere else.
func detachConsole() {}
