package sandbox

import (
	"runtime"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

func TestProfileDeniesTheThreeClassesAndGrantsOnlyWhatItIsGiven(t *testing.T) {
	p, err := (&Plan{Read: []string{"/usr", "/work"}, Write: []string{"/work"}}).Profile()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(deny file-read* file-write*)",
		"(deny network-outbound (remote unix-socket))",
		"(deny mach-lookup)",
		`(subpath "/work")`,
		`(allow network-outbound (literal "` + ResolverSocket + `"))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile lacks %q:\n%s", want, p)
		}
	}
	// The keychain's services are the most valuable thing a mach lookup reaches
	// on a user's machine; they must never ride in on the shared allow-list.
	for _, n := range MachAllow {
		if strings.Contains(n, "SecurityServer") || strings.Contains(n, "securityd") || strings.Contains(n, "secd") {
			t.Errorf("machAllow carries a keychain service: %s", n)
		}
	}
}

func TestProfileClosedNetworkIsLoopbackOnlyAndDropsTheResolver(t *testing.T) {
	p, err := (&Plan{Network: fleet.SandboxNetworkClosed, Read: []string{"/usr"}}).Profile()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, ResolverSocket) {
		t.Errorf("closed network still allows the resolver:\n%s", p)
	}
	if !strings.Contains(p, `(allow network-outbound (remote ip "localhost:*"))`) {
		t.Errorf("closed network lacks loopback:\n%s", p)
	}
}

func TestProfileRefusesAPathItCannotQuote(t *testing.T) {
	if _, err := (&Plan{Read: []string{"/a\nb"}}).Profile(); err == nil {
		t.Fatal("a path with a control character was rendered into a profile")
	}
}

func TestStateReportsWhatIsInForceDefaultsIncluded(t *testing.T) {
	st := (&Plan{Read: []string{"/usr", "/w"}, Write: []string{"/w"}}).State()
	if st.Network != fleet.SandboxNetworkOpen {
		t.Errorf("network = %q, want the default %q", st.Network, fleet.SandboxNetworkOpen)
	}
	if st.Mechanism != Mechanism || len(st.Denies) != 3 {
		t.Errorf("state = %+v", st)
	}
	if len(st.ReadPaths) != 2 || len(st.WritePaths) != 1 {
		t.Errorf("paths = %v / %v", st.ReadPaths, st.WritePaths)
	}
}

func TestInstallRootsNeverOpensTheHome(t *testing.T) {
	got := InstallRoots("/opt/x/bin/agent", "/opt/x")
	if len(got) != 1 {
		t.Fatalf("a layout whose prefix is the home was opened: %v", got)
	}
	got = InstallRoots("/opt/x/bin/agent", "/home/u")
	if len(got) != 2 || got[1] != "/opt/x" {
		t.Fatalf("conventional <prefix>/bin layout: %v", got)
	}
}

func TestCoversOrIsAndDedupe(t *testing.T) {
	if !CoversOrIs("/a", "/a") || !CoversOrIs("/a", "/a/b") || CoversOrIs("/a", "/ab") {
		t.Error("CoversOrIs is wrong about prefixes")
	}
	if got := Dedupe([]string{"/b", "/a", "/b"}); len(got) != 2 || got[0] != "/a" {
		t.Errorf("Dedupe = %v", got)
	}
}

func TestHostSupportIsDeclaredOnlyWhereItCanBeEnforced(t *testing.T) {
	sup, why := HostSupport()
	if runtime.GOOS != "darwin" {
		if sup != nil || why == "" {
			t.Fatalf("declared a sandbox on %s, or gave no reason: %+v %q", runtime.GOOS, sup, why)
		}
		return
	}
	if sup == nil {
		t.Skipf("macOS host that cannot apply a profile (nested sandbox?): %s", why)
	}
	if len(sup.Denies) != 3 {
		t.Fatalf("a host support that does not deny every class: %+v", sup)
	}
}
