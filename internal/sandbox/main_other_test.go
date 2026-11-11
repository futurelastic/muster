//go:build !linux

package sandbox

import "testing"

func testMain(m *testing.M) int { return m.Run() }
