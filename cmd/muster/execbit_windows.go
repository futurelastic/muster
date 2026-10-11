//go:build !unix

package main

import "os"

// isExecutableFile reports whether path names a file this process could run.
// Windows has no execute bit — Go reports none for any file — so the question
// is the one the command processor asks: a regular file whose extension is in
// PATHEXT (or its default list). The doctor's runtime rows ask it so that
// "executable" is judged in one place per platform (#312); the Unix answer is
// in execbit_unix.go.
//
// A path with no extension is not executable here even when a sibling with one
// exists: the rows judge the path the operator configured, as written.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && pathExtMatches(path, os.Getenv("PATHEXT"))
}
