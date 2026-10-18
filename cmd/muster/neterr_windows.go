package main

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is WSAECONNREFUSED, which is what a refused connection is on
// Windows: Winsock's own error number. syscall.ECONNREFUSED there is a value Go
// invents for portable code, never produced by the operating system, so
// comparing against it never matches a real refusal.
const wsaeconnrefused syscall.Errno = 10061

// isConnRefused reports whether err is a refused TCP connection: nothing is
// listening where the client dialled.
func isConnRefused(err error) bool {
	return errors.Is(err, wsaeconnrefused) || errors.Is(err, syscall.ECONNREFUSED)
}

// wsaeaddrnotavail is WSAEADDRNOTAVAIL, Winsock's own number for a bind to an
// address no interface holds (see wsaeconnrefused for why the portable
// syscall value is not enough).
const wsaeaddrnotavail syscall.Errno = 10049

// isAddrNotAvailable reports whether err is a bind to an address that is not
// assigned to this machine.
func isAddrNotAvailable(err error) bool {
	return errors.Is(err, wsaeaddrnotavail) || errors.Is(err, syscall.EADDRNOTAVAIL)
}
