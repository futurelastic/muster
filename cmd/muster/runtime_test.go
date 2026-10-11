package main

import (
	"strings"
	"testing"
)

func TestDefaultRuntimeIsNoneOnWindowsAndTmuxElsewhere(t *testing.T) {
	for goos, want := range map[string]string{"windows": "none", "linux": "tmux", "darwin": "tmux"} {
		if got := defaultRuntimeFor(goos); got != want {
			t.Errorf("defaultRuntimeFor(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestCheckRuntimeName(t *testing.T) {
	for _, tc := range []struct {
		goos, name string
		wantErr    string // "" = accepted; otherwise a substring of the refusal
	}{
		{"linux", "tmux", ""},
		{"darwin", "stub", ""},
		{"linux", "none", ""},
		{"windows", "none", ""},
		{"windows", "stub", ""},
		{"windows", "tmux", "ADR 303"},
		{"linux", "screen", `unknown FLEET_RUNTIME "screen"`},
	} {
		err := checkRuntimeName(tc.goos, tc.name)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s/%s refused: %v", tc.goos, tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s/%s: err = %v, want one containing %q", tc.goos, tc.name, err, tc.wantErr)
		}
	}
}

func TestCompatRefusesOnWindowsOnly(t *testing.T) {
	if why := compatRefusal("windows"); !strings.Contains(why, "ADR 303") {
		t.Errorf("windows refusal does not name the decision: %q", why)
	}
	if why := compatRefusal("linux"); why != "" {
		t.Errorf("linux refused: %q", why)
	}
}

func TestDoctorRuntimeRowFollowsTheRuntimeChoice(t *testing.T) {
	win := checkRuntimeOn("windows", "tmux", "", nil)
	if win.Status != statusFail || !strings.Contains(win.Summary, "ADR 303") {
		t.Errorf("tmux on windows: %+v, want a fail naming ADR 303", win)
	}
	none := checkRuntimeOn("windows", "none", "", nil)
	if none.Status != statusPass {
		t.Errorf("none on windows: %+v, want pass", none)
	}
	unknown := checkRuntimeOn("linux", "screen", "", nil)
	if unknown.Status != statusFail || !strings.Contains(unknown.Summary, "want tmux, stub or none") {
		t.Errorf("unknown runtime: %+v", unknown)
	}
}
