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
		if err := Validate(c.in); (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", name, err, c.wantErr)
		}
	}
}

func TestProvisionPrecedence(t *testing.T) {
	file := writeValue(t, "from-file\n")
	spec := fleet.SessionSpec{Cwd: "/w"}

	got, err := Provision([]Entry{{Name: "K", FromFile: file}}, spec, "m", nil)
	if err != nil || got["K"] != "from-file" {
		t.Fatalf("silent caller: %v, %v", got, err)
	}

	// Non-required: the caller wins.
	spec.Env = map[string]string{"K": "caller"}
	got, err = Provision([]Entry{{Name: "K", FromFile: file}}, spec, "m", nil)
	if err != nil || got["K"] != "caller" {
		t.Fatalf("caller should win: %v, %v", got, err)
	}

	// Required + caller value: refused without reading the file (a missing
	// file would otherwise be an unsupported error, not an invalid one).
	_, err = Provision([]Entry{{Name: "K", FromFile: "/nonexistent/x", Required: true}}, spec, "m", nil)
	var fe *fleet.Error
	if err == nil || errors.As(err, &fe) || !strings.Contains(err.Error(), "must be omitted") {
		t.Fatalf("required+caller = %v, want a bare 'must be omitted' error", err)
	}

	// Required, caller silent, file missing: typed unsupported.
	_, err = Provision([]Entry{{Name: "K", FromFile: "/nonexistent/x", Required: true}}, fleet.SessionSpec{}, "m", nil)
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("missing required file = %v, want ErrorUnsupported", err)
	}
}

func TestProvisionReadsTheFileFreshAndHonoursScopeAndSkip(t *testing.T) {
	file := writeValue(t, "one")
	entries := []Entry{{Name: "K", FromFile: file}}
	got, _ := Provision(entries, fleet.SessionSpec{}, "m", nil)
	if got["K"] != "one" {
		t.Fatalf("first = %v", got)
	}
	if err := os.WriteFile(file, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ = Provision(entries, fleet.SessionSpec{}, "m", nil)
	if got["K"] != "two" {
		t.Fatalf("a rotated file must be picked up on the next call, got %v", got)
	}

	scoped := []Entry{{Name: "K", FromFile: file, AppliesTo: Scope{Agents: []string{"sid"}}}}
	if got, _ := Provision(scoped, fleet.SessionSpec{Agent: "alex"}, "m", nil); len(got) != 0 {
		t.Errorf("out-of-scope entry applied: %v", got)
	}
	if got, _ := Provision(scoped, fleet.SessionSpec{Agent: "sid"}, "m", nil); got["K"] != "two" {
		t.Errorf("in-scope entry missing: %v", got)
	}

	skipped, err := Provision([]Entry{{Name: "K", FromFile: "/nonexistent", Required: true}}, fleet.SessionSpec{}, "m",
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
