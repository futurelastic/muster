//go:build unix

package main

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether err is a refused TCP connection: nothing is
// listening where the client dialled. Split by platform because the number
// differs (neterr_windows.go).
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// isAddrNotAvailable reports whether err is a bind to an address that is not
// assigned to this machine (EADDRNOTAVAIL).
func isAddrNotAvailable(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL)
}
