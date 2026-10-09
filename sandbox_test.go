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
