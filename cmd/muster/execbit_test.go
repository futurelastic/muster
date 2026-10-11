package main

import "testing"

// pathExtMatches is the Windows half of "is this executable" (#312). It is pure
// so it is pinned here on every platform, not only on the Windows job.
func TestPathExtMatches(t *testing.T) {
	cases := []struct {
		name, path, pathext string
		want                bool
	}{
		{"exe in the list", `C:\tools\opencode.exe`, ".COM;.EXE;.BAT;.CMD", true},
		{"case does not matter on either side", `C:\tools\opencode.EXE`, ".com;.exe", true},
		{"a forward-slash path", "C:/tools/opencode.cmd", ".COM;.EXE;.BAT;.CMD", true},
		{"extension not in the list", `C:\tools\opencode.txt`, ".COM;.EXE;.BAT;.CMD", false},
		{"no extension", `C:\tools\opencode`, ".COM;.EXE;.BAT;.CMD", false},
		{"empty list means the Windows default", `C:\tools\run.bat`, "", true},
		{"whitespace-only list means the default too", `C:\tools\run.exe`, "  ", true},
		{"default does not include everything", `C:\tools\run.ps1`, "", false},
		{"a configured list replaces the default", `C:\tools\run.exe`, ".PY", false},
		{"empty entries are ignored", `C:\tools\run.py`, ";.PY;;", true},
		{"spaces around entries are ignored", `C:\tools\run.py`, " .PY ; .RB ", true},
		{"a dot in a directory is not the extension", `C:\v1.exe\opencode`, ".EXE", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathExtMatches(tc.path, tc.pathext); got != tc.want {
				t.Errorf("pathExtMatches(%q, %q) = %v, want %v", tc.path, tc.pathext, got, tc.want)
			}
		})
	}
}
