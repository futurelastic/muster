package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

// muster #281: the shape check that holds on every platform, so a malformed
// sandbox is refused the same way wherever the create lands.

func TestSandboxSpecValidate(t *testing.T) {
	cases := []struct {
		name string
		spec SandboxSpec
		bad  string // substring of the refusal; empty means valid
	}{
		{"empty is the default profile", SandboxSpec{}, ""},
		{"extra paths and closed network", SandboxSpec{
			ReadPaths: []AbsolutePath{"/srv/data"}, WritePaths: []AbsolutePath{"/srv/work"}, Network: SandboxNetworkClosed}, ""},
		{"unknown network", SandboxSpec{Network: "filtered"}, "sandbox.network"},
		{"relative read path", SandboxSpec{ReadPaths: []AbsolutePath{"data"}}, "absolute"},
		{"dot-dot is not clean", SandboxSpec{ReadPaths: []AbsolutePath{"/srv/../etc"}}, "clean"},
		{"trailing separator is not clean", SandboxSpec{ReadPaths: []AbsolutePath{"/srv/data/"}}, "clean"},
		{"the root cannot be granted", SandboxSpec{ReadPaths: []AbsolutePath{"/"}}, "root"},
		{"a quote cannot hide in a path", SandboxSpec{ReadPaths: []AbsolutePath{`/srv/a"b`}}, "quote"},
		{"a newline cannot hide in a path", SandboxSpec{ReadPaths: []AbsolutePath{"/srv/a\nb"}}, "control"},
		{"a parenthesis cannot hide in a path", SandboxSpec{WritePaths: []AbsolutePath{"/srv/a)(allow default"}}, "parenthesis"},
		{"shared tmp is never writable", SandboxSpec{WritePaths: []AbsolutePath{"/tmp"}}, "shared"},
		{"nor its real path", SandboxSpec{WritePaths: []AbsolutePath{"/private/tmp"}}, "shared"},
		{"nor a directory that contains it", SandboxSpec{WritePaths: []AbsolutePath{"/private/var"}}, "shared"},
		{"a directory under tmp can be named on purpose", SandboxSpec{WritePaths: []AbsolutePath{"/tmp/job-1"}}, ""},
		{"unknown cache mode", SandboxSpec{PackageCache: &SandboxPackageCache{Path: "/srv/c", Mode: "mirror"}}, "packageCache.mode"},
		{"cache path is checked too", SandboxSpec{PackageCache: &SandboxPackageCache{Path: "c"}}, "absolute"},
		{"a good cache", SandboxSpec{PackageCache: &SandboxPackageCache{Path: "/srv/c", Mode: PackageCacheShared}}, ""},
		{"a cache serving every manager", SandboxSpec{PackageCache: &SandboxPackageCache{Path: "/srv/c",
			Managers: []PackageManager{PackageManagerCargo, PackageManagerGo, PackageManagerNpm}}}, ""},
		{"an unknown manager", SandboxSpec{PackageCache: &SandboxPackageCache{Path: "/srv/c", Managers: []PackageManager{"pip"}}}, "packageCache.managers"},
		{"a manager named twice", SandboxSpec{PackageCache: &SandboxPackageCache{Path: "/srv/c",
			Managers: []PackageManager{PackageManagerGo, PackageManagerGo}}}, "twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			switch {
			case c.bad == "" && err != nil:
				t.Fatalf("refused a valid sandbox: %v", err)
			case c.bad != "" && err == nil:
				t.Fatalf("accepted a sandbox that should be refused (%s)", c.bad)
			case c.bad != "" && !strings.Contains(err.Error(), c.bad):
				t.Fatalf("refusal %q does not name %q", err, c.bad)
			}
		})
	}
}

// The wire names are the contract; a rename must fail here, not in a client.
func TestSandboxWireNames(t *testing.T) {
	spec := SessionSpec{Sandbox: &SandboxSpec{
		ReadPaths: []AbsolutePath{"/a"}, WritePaths: []AbsolutePath{"/b"}, Network: SandboxNetworkClosed,
		PackageCache: &SandboxPackageCache{Path: "/c", Mode: PackageCachePrivate},
	}}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"sandbox":{`, `"readPaths":["/a"]`, `"writePaths":["/b"]`, `"network":"closed"`,
		`"packageCache":{"path":"/c","mode":"private"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("wire form lacks %s: %s", want, b)
		}
	}
	b, _ = json.Marshal(SessionSpec{})
	if strings.Contains(string(b), "sandbox") {
		t.Errorf("an unsandboxed spec must not mention sandbox: %s", b)
	}
	caps, _ := json.Marshal(DriverCapabilities{DeadlineMs: 1, Source: CapabilitiesObserved})
	if strings.Contains(string(caps), "sandbox") {
		t.Errorf("a driver without support must omit the capability, not report a false: %s", caps)
	}
}

// muster #432: the variable each manager reads is part of the contract — a client
// reading `docs/api.md` sets nothing itself, so a wrong name here is a cache the
// manager never looks in.
func TestPackageManagerEnvVars(t *testing.T) {
	want := map[PackageManager]string{
		PackageManagerCargo: "CARGO_HOME", PackageManagerGo: "GOMODCACHE", PackageManagerNpm: "npm_config_cache",
	}
	if got := PackageManagers(); len(got) != len(want) {
		t.Fatalf("PackageManagers() = %v, want %d managers", got, len(want))
	}
	for m, v := range want {
		if !m.Known() || m.EnvVar() != v {
			t.Errorf("%s: known=%v EnvVar=%q, want %q", m, m.Known(), m.EnvVar(), v)
		}
	}
	if PackageManager("pip").Known() || PackageManager("pip").EnvVar() != "" {
		t.Error("an unknown manager must name no variable")
	}
}

func TestPackageCacheManagersWireNames(t *testing.T) {
	b, err := json.Marshal(SandboxPackageCache{Path: "/c", Managers: []PackageManager{PackageManagerCargo, PackageManagerNpm}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"path":"/c","managers":["cargo","npm"]}`; string(b) != want {
		t.Errorf("wire form = %s, want %s", b, want)
	}
	// A cache announcing itself only (the shape before #432) carries no managers key.
	b, _ = json.Marshal(SandboxPackageCache{Path: "/c", Mode: PackageCachePrivate})
	if strings.Contains(string(b), "managers") {
		t.Errorf("a cache with no managers must not mention them: %s", b)
	}
}

// muster #448: the opt-in to a full copy is named allowFullCopy on both spellings
// of the request, and an absent one is not on the wire at all.
func TestAllowFullCopyWireName(t *testing.T) {
	b, err := json.Marshal(SandboxPackageCache{Path: "/c", AllowFullCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"path":"/c","allowFullCopy":true}`; string(b) != want {
		t.Errorf("sandbox wire form = %s, want %s", b, want)
	}
	b, _ = json.Marshal(PosturePackageCache{Seed: "/s", Managers: []PackageManager{PackageManagerGo}, AllowFullCopy: true})
	if want := `{"seed":"/s","managers":["go"],"allowFullCopy":true}`; string(b) != want {
		t.Errorf("posture wire form = %s, want %s", b, want)
	}
	for _, v := range []any{SandboxPackageCache{Path: "/c"}, PosturePackageCache{Seed: "/s"}} {
		b, _ = json.Marshal(v)
		if strings.Contains(string(b), "allowFullCopy") {
			t.Errorf("a request that did not opt in must not mention it: %s", b)
		}
	}
}
