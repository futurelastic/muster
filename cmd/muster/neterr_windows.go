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
