package main

import (
	"net"
	"testing"
)

// A real refusal, not a constructed error: the point of isConnRefused is what
// THIS operating system reports for a port nobody listens on.
func TestIsConnRefusedMatchesARealRefusal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	_, err = net.Dial("tcp", addr)
	if err == nil {
		t.Skip("something is listening on the port that was just closed")
	}
	if !isConnRefused(err) {
		t.Errorf("isConnRefused(%v) = false for a refused dial", err)
	}
	if describeNetErr(err) != "connection refused" {
		t.Errorf("describeNetErr = %q", describeNetErr(err))
	}
}
