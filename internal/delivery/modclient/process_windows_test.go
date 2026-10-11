//go:build windows

package modclient_test

import "testing"

// assertLeadsItsOwnGroup has nothing to read on Windows: the equivalent
// property — everything the child starts dies with it — is what the
// kill-reaches-the-tree tests assert, through the job the child is in.
func assertLeadsItsOwnGroup(t *testing.T, pid int) { t.Helper() }
