//go:build !darwin && !linux

package lowprio

import "errors"

var errUnsupported = errors.New("lowprio: no background priority on this platform")

// lower has nothing to do on this platform; Go then runs fn at normal priority.
func lower() error { return errUnsupported }

func current() (bool, error) { return false, errUnsupported }
