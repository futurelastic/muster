package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodPolicy = `{"version":1,"network":"open","read":["/usr"],"write":["/w"],"devices":[],"ownTTY":false,"bindTCP":[]}`

func TestLauncherArgsParseAPolicyAndAProgram(t *testing.T) {
	a, err := parseLauncherArgs([]string{"--policy", goodPolicy, "--", "/bin/agent", "--flag", "x y"})
	if err != nil {
		t.Fatal(err)
	}
	if a.selfTest || len(a.argv) != 3 || a.argv[0] != "/bin/agent" || a.argv[2] != "x y" {
		t.Errorf("args = %+v", a)
	}
	if a.policy.Network != "open" || len(a.policy.Read) != 1 {
		t.Errorf("policy = %+v", a.policy)
	}

	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(goodPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err = parseLauncherArgs([]string{"--policy-file", file, "--", "/bin/agent"})
	if err != nil || a.policy.Write[0] != "/w" {
		t.Errorf("file policy: %+v, %v", a, err)
	}
}

func TestLauncherArgsTakeASelfTestWithoutAProgram(t *testing.T) {
	a, err := parseLauncherArgs([]string{"--policy", goodPolicy, "--self-test", "/o/secret", "4100", "--"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.selfTest || a.outsideFile != "/o/secret" || a.tcpPort != 4100 || len(a.argv) != 0 {
		t.Errorf("args = %+v", a)
	}
}

func TestLauncherArgsRefuseWhatItWouldBeUnsafeToGuess(t *testing.T) {
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(goodPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", maxPolicyBytes+10)), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"no policy":               {"--", "/bin/agent"},
		"both policies":           {"--policy", goodPolicy, "--policy-file", file, "--", "/bin/agent"},
		"unknown flag":            {"--policy", goodPolicy, "--verbose", "--", "/bin/agent"},
		"no program":              {"--policy", goodPolicy, "--"},
		"no separator":            {"--policy", goodPolicy},
		"relative program":        {"--policy", goodPolicy, "--", "agent"},
		"missing policy value":    {"--policy"},
		"missing file":            {"--policy-file", filepath.Join(t.TempDir(), "absent"), "--", "/bin/agent"},
		"oversize file":           {"--policy-file", big, "--", "/bin/agent"},
		"not json":                {"--policy", "read /usr", "--", "/bin/agent"},
		"unknown field":           {"--policy", `{"version":1,"network":"open","read":[],"write":[],"devices":[],"ownTTY":false,"bindTCP":[],"allowAll":true}`, "--", "/bin/agent"},
		"another version":         {"--policy", `{"version":2,"network":"open","read":[],"write":[],"devices":[],"ownTTY":false,"bindTCP":[]}`, "--", "/bin/agent"},
		"relative path":           {"--policy", `{"version":1,"network":"open","read":["usr"],"write":[],"devices":[],"ownTTY":false,"bindTCP":[]}`, "--", "/bin/agent"},
		"self-test without port":  {"--policy", goodPolicy, "--self-test", "/o"},
		"self-test with bad port": {"--policy", goodPolicy, "--self-test", "/o", "http", "--"},
	}
	for name, args := range cases {
		if _, err := parseLauncherArgs(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSelfLauncherNamesTheRunningExecutable(t *testing.T) {
	got := SelfLauncher()
	if got == "" {
		t.Fatal("a test binary is a regular executable file, and SelfLauncher did not name it")
	}
	if info, err := os.Stat(got); err != nil || info.IsDir() {
		t.Errorf("SelfLauncher = %q: %v", got, err)
	}
}
