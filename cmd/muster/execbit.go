package main

import (
	"path/filepath"
	"strings"
)

// defaultPathExt is what Windows itself falls back to when PATHEXT is unset or
// empty: the extensions the command processor will run directly.
const defaultPathExt = ".COM;.EXE;.BAT;.CMD"

// pathExtMatches reports whether path ends in one of the extensions listed in
// pathext (the PATHEXT variable's form: ";"-separated, any case). An empty
// list means the Windows default.
//
// It is pure and lives outside the per-platform files so a test on any
// machine can pin what Windows would answer; only the Windows implementation of
// isExecutableFile calls it in production (execbit_windows.go).
func pathExtMatches(path, pathext string) bool {
	ext := strings.ToUpper(filepath.Ext(path))
	if ext == "" {
		return false
	}
	if strings.TrimSpace(pathext) == "" {
		pathext = defaultPathExt
	}
	for _, e := range strings.Split(pathext, ";") {
		if e = strings.TrimSpace(e); e != "" && strings.ToUpper(e) == ext {
			return true
		}
	}
	return false
}
