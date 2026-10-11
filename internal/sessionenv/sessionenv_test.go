package sessionenv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

func writeValue(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate(t *testing.T) {
	good := Entry{Name: "FLEET_X", FromFile: "/etc/x"}
	for name, c := range map[string]struct {
		in      []Entry
		wantErr bool
	}{
		"good":      {[]Entry{good}, false},
		"bad name":  {[]Entry{{Name: "A-B", FromFile: "/x"}}, true},
		"no file":   {[]Entry{{Name: "A"}}, true},
		"relative":  {[]Entry{{Name: "A", FromFile: "x"}}, true},
		"duplicate": {[]Entry{good, good}, true},
	} {
		if err := Validate(c.in, nil); (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", name, err, c.wantErr)
		}
	}
}

func TestProvisionPrecedence(t *testing.T) {
	file := writeValue(t, "from-file\n")
	spec := fleet.SessionSpec{Cwd: "/w"}

	got, err := Provision([]Entry{{Name: "K", FromFile: file}}, spec, "m", "rt", nil)
	if err != nil || got["K"] != "from-file" {
		t.Fatalf("silent caller: %v, %v", got, err)
	}

	// Non-required: the caller wins.
	spec.Env = map[string]string{"K": "caller"}
	got, err = Provision([]Entry{{Name: "K", FromFile: file}}, spec, "m", "rt", nil)
	if err != nil || got["K"] != "caller" {
		t.Fatalf("caller should win: %v, %v", got, err)
	}

	// Required + caller value: refused without reading the file (a missing
	// file would otherwise be an unsupported error, not an invalid one).
	_, err = Provision([]Entry{{Name: "K", FromFile: "/nonexistent/x", Required: true}}, spec, "m", "rt", nil)
	var fe *fleet.Error
	if err == nil || errors.As(err, &fe) || !strings.Contains(err.Error(), "must be omitted") {
		t.Fatalf("required+caller = %v, want a bare 'must be omitted' error", err)
	}

	// Required, caller silent, file missing: typed unsupported.
	_, err = Provision([]Entry{{Name: "K", FromFile: "/nonexistent/x", Required: true}}, fleet.SessionSpec{}, "m", "rt", nil)
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("missing required file = %v, want ErrorUnsupported", err)
	}
}

func TestProvisionReadsTheFileFreshAndHonoursScopeAndSkip(t *testing.T) {
	file := writeValue(t, "one")
	entries := []Entry{{Name: "K", FromFile: file}}
	got, _ := Provision(entries, fleet.SessionSpec{}, "m", "rt", nil)
	if got["K"] != "one" {
		t.Fatalf("first = %v", got)
	}
	if err := os.WriteFile(file, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ = Provision(entries, fleet.SessionSpec{}, "m", "rt", nil)
	if got["K"] != "two" {
		t.Fatalf("a rotated file must be picked up on the next call, got %v", got)
	}

	scoped := []Entry{{Name: "K", FromFile: file, AppliesTo: Scope{Agents: []string{"sid"}}}}
	if got, _ := Provision(scoped, fleet.SessionSpec{Agent: "alex"}, "m", "rt", nil); len(got) != 0 {
		t.Errorf("out-of-scope entry applied: %v", got)
	}
	if got, _ := Provision(scoped, fleet.SessionSpec{Agent: "sid"}, "m", "rt", nil); got["K"] != "two" {
		t.Errorf("in-scope entry missing: %v", got)
	}

	skipped, err := Provision([]Entry{{Name: "K", FromFile: "/nonexistent", Required: true}}, fleet.SessionSpec{}, "m", "rt",
		func(n string) bool { return n == "K" })
	if err != nil || len(skipped) != 0 {
		t.Errorf("skipped entry = %v, %v", skipped, err)
	}
}

func TestCheckEnv(t *testing.T) {
	if err := CheckEnv(map[string]string{"A": "b"}); err != nil {
		t.Errorf("good env refused: %v", err)
	}
	if CheckEnv(map[string]string{"1A": "b"}) == nil {
		t.Error("bad name accepted")
	}
	if CheckEnv(map[string]string{"A": "b\nc"}) == nil {
		t.Error("newline value accepted")
	}
}

// --- appliesTo.runtimes (#294) --------------------------------------------

func TestScopeMatchesCombinesRuntimesAsAGateInFrontOfAgentsOrMarkers(t *testing.T) {
	spec := func(agent, marker string) fleet.SessionSpec {
		return fleet.SessionSpec{Agent: fleet.AgentId(agent), Marker: marker}
	}
	for name, c := range map[string]struct {
		scope   Scope
		runtime fleet.RuntimeId
		spec    fleet.SessionSpec
		want    bool
	}{
		"zero scope matches every runtime":             {Scope{}, "alpha", spec("", ""), true},
		"zero scope matches an unknown runtime":        {Scope{}, "", spec("", ""), true},
		"runtimes alone, runtime listed":               {Scope{Runtimes: []string{"alpha"}}, "alpha", spec("x", ""), true},
		"runtimes alone, another runtime":              {Scope{Runtimes: []string{"alpha"}}, "beta", spec("x", ""), false},
		"runtimes alone, runtime not known":            {Scope{Runtimes: []string{"alpha"}}, "", spec("x", ""), false},
		"several runtimes, second one":                 {Scope{Runtimes: []string{"alpha", "beta"}}, "beta", spec("", ""), true},
		"runtime matches, agent does not":              {Scope{Runtimes: []string{"alpha"}, Agents: []string{"sid"}}, "alpha", spec("alex", ""), false},
		"agent matches, runtime does not":              {Scope{Runtimes: []string{"alpha"}, Agents: []string{"sid"}}, "beta", spec("sid", ""), false},
		"runtime and agent both match":                 {Scope{Runtimes: []string{"alpha"}, Agents: []string{"sid"}}, "alpha", spec("sid", ""), true},
		"runtime matches, marker is the OR branch":     {Scope{Runtimes: []string{"alpha"}, Agents: []string{"sid"}, Markers: []string{"filing"}}, "alpha", spec("alex", "filing"), true},
		"runtime fails, marker would have matched":     {Scope{Runtimes: []string{"alpha"}, Agents: []string{"sid"}, Markers: []string{"filing"}}, "beta", spec("alex", "filing"), false},
		"agents-or-markers unchanged without runtimes": {Scope{Agents: []string{"sid"}, Markers: []string{"filing"}}, "beta", spec("alex", "filing"), true},
	} {
		if got := c.scope.Matches(c.runtime, c.spec); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", name, got, c.want)
		}
	}
}

// The runtime a scope is matched against is the one the driver passes, never
// the caller's hint on the spec: a caller that names no runtime still gets a
// session on the default, and a caller cannot talk its way into a credential
// scoped to another runtime by naming it.
func TestProvisionMatchesTheDriversRuntimeNotTheSpecs(t *testing.T) {
	file := writeValue(t, "secret")
	entries := []Entry{{Name: "TOKEN", FromFile: file, Required: true,
		AppliesTo: Scope{Runtimes: []string{"alpha"}}}}

	got, err := Provision(entries, fleet.SessionSpec{}, "m", "alpha", nil)
	if err != nil || got["TOKEN"] != "secret" {
		t.Fatalf("on the scoped runtime, hint empty: %v, %v", got, err)
	}
	got, err = Provision(entries, fleet.SessionSpec{Runtime: "alpha"}, "m", "beta", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("on another runtime the caller's hint must not deliver it: %v, %v", got, err)
	}
	// Out of scope is out of scope for `required` too: the entry is not
	// provisioned and does not refuse the caller's own value for the name.
	got, err = Provision(entries, fleet.SessionSpec{Env: map[string]string{"TOKEN": "callers"}}, "m", "beta", nil)
	if err != nil || got["TOKEN"] != "callers" {
		t.Fatalf("a required entry outside its runtime must not refuse a caller: %v, %v", got, err)
	}
	// An unscoped entry still reaches every runtime — the hazard docs/session-identity.md names.
	unscoped := []Entry{{Name: "TOKEN", FromFile: file}}
	for _, rt := range []fleet.RuntimeId{"alpha", "beta"} {
		if got, _ := Provision(unscoped, fleet.SessionSpec{}, "m", rt, nil); got["TOKEN"] != "secret" {
			t.Errorf("unscoped entry missing on %s: %v", rt, got)
		}
	}
}

func TestValidateChecksRuntimeIdsAgainstTheKnownSet(t *testing.T) {
	known := []fleet.RuntimeId{"alpha", "beta"}
	scoped := func(ids ...string) []Entry {
		return []Entry{{Name: "K", FromFile: "/etc/k", AppliesTo: Scope{Runtimes: ids}}}
	}
	if err := Validate(scoped("alpha", "beta"), known); err != nil {
		t.Errorf("known ids refused: %v", err)
	}
	err := Validate(scoped("alpha", "gamma"), known)
	if err == nil || !strings.Contains(err.Error(), `"gamma"`) || !strings.Contains(err.Error(), "alpha, beta") {
		t.Errorf("unknown id = %v, want an error naming gamma and the known ids", err)
	}
	if err := Validate(scoped("alpha", "gamma"), nil); err != nil {
		t.Errorf("nil known set must skip the id check: %v", err)
	}
	// An empty id is a fault whatever the known set is.
	if Validate(scoped(""), nil) == nil || Validate(scoped(""), known) == nil {
		t.Error("an empty runtime id was accepted")
	}
	// The scope check does not touch entries without runtimes.
	if err := Validate([]Entry{{Name: "K", FromFile: "/etc/k"}}, known); err != nil {
		t.Errorf("an unscoped entry refused: %v", err)
	}
}
