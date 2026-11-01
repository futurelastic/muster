package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sandbox"
)

// The sandbox a session may be started under (muster #281).
//
// The profile itself — the mechanism, the three classes it denies, the
// allow-lists and the host probe — lives in internal/sandbox, shared with the
// driver that starts sessions in the user's terminal multiplexer (muster #345),
// so the two cannot deny different things under the same name. What stays here
// is what only this runtime has: resolving a create's request against its own
// install, and the private package cache.
//
// A harness that ships its own sandbox must be told to stand down, because a
// macOS profile cannot be nested. This runtime has none, so there is nothing to
// switch off; the rule is recorded here because the next driver inherits it.

// sandboxExecPath, sandboxMechanism, resolverSocket and systemReadRoots are the
// shared package's, under the names this driver's code and tests already use.
const (
	sandboxExecPath  = sandbox.ExecPath
	sandboxMechanism = sandbox.Mechanism
	resolverSocket   = sandbox.ResolverSocket
)

var systemReadRoots = sandbox.SystemReadRoots

// packageCacheEnv names the variable that tells a session where its package
// cache is. A driver-owned name: a create may not set it.
const packageCacheEnv = "MUSTER_PACKAGE_CACHE"

// sandboxSupport reports what this driver can enforce on this host, or nil when
// it cannot — which is a statement the capability carries, not an error.
//
// The probe actually applies a profile once: a service that itself runs inside
// a sandbox cannot start a nested one, and discovering that at the first create
// would refuse a caller for a reason the capability should have told it.
func sandboxSupport(shared bool) (*fleet.SandboxSupport, string) {
	if shared {
		return nil, "this driver is in shared mode (one server for every session), which cannot confine one of them"
	}
	sup, why := sandbox.HostSupport()
	if sup == nil {
		return nil, why
	}
	sup.PackageCache = []fleet.PackageCacheMode{fleet.PackageCacheShared, fleet.PackageCachePrivate}
	return sup, ""
}

// sandboxPlan is a resolved sandbox request: every path real, every default
// applied, ready to be rendered as a profile and reported back.
type sandboxPlan struct {
	network fleet.SandboxNetwork
	read    []string
	write   []string
	cache   *fleet.SandboxPackageCacheState
}

// state renders the plan as the report a session carries.
func (p *sandboxPlan) state() *fleet.SandboxState {
	st := (&sandbox.Plan{Network: p.network, Read: p.read, Write: p.write}).State()
	if p.cache != nil {
		c := *p.cache
		st.PackageCache = &c
	}
	return st
}

// resolvePath, coversOrIs, installRoots and dedupe are the shared package's.
func resolvePath(p string) string            { return sandbox.ResolvePath(p) }
func coversOrIs(root, path string) bool      { return sandbox.CoversOrIs(root, path) }
func installRoots(bin, home string) []string { return sandbox.InstallRoots(bin, home) }
func dedupe(in []string) []string            { return sandbox.Dedupe(in) }

// planSandbox resolves a create's sandbox request against this driver's
// defaults, validates it, and — for a private package cache — makes the
// session's copy. It runs before the server starts and before any directory
// other than sessionDir exists, so a refusal has no side effect to undo beyond
// sessionDir, which the caller removes.
func (d *Driver) planSandbox(spec fleet.SessionSpec, sessionDir string) (*sandboxPlan, error) {
	return d.planSandboxFor(spec, sessionDir, "")
}

// planSandboxFor is planSandbox for a session that may already have a private
// package cache in its directory (muster #282): priorCopy, when set, is how that
// cache was made, and it is reused rather than copied over itself.
func (d *Driver) planSandboxFor(spec fleet.SessionSpec, sessionDir, priorCopy string) (*sandboxPlan, error) {
	sb := spec.Sandbox
	invalid := func(msg string, args ...any) error {
		return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + fmt.Sprintf(msg, args...), Machine: d.machine}
	}
	if err := sb.Validate(); err != nil {
		return nil, invalid("%s", err.Error())
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		home = resolvePath(home)
	}
	// A grant that would include the service user's home puts back exactly what
	// the profile exists to remove.
	check := func(field string, p fleet.AbsolutePath) (string, error) {
		r := resolvePath(string(p))
		if home != "" && coversOrIs(r, home) {
			return "", invalid("%s: %q is, or contains, the service user's home directory; grant the specific directory a session needs", field, string(p))
		}
		return r, nil
	}

	cwd := resolvePath(string(spec.Cwd))
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, invalid("cwd %q is not a directory that exists, which a sandboxed session needs to be started in", string(spec.Cwd))
	}
	if home != "" && coversOrIs(cwd, home) {
		return nil, invalid("cwd %q is, or contains, the service user's home directory, which a sandboxed session would then write to", string(spec.Cwd))
	}

	plan := &sandboxPlan{network: sb.Network}
	if plan.network == "" {
		plan.network = fleet.SandboxNetworkOpen
	}

	write := []string{cwd, resolvePath(sessionDir)}
	read := append([]string(nil), systemReadRoots...)
	read = append(read, installRoots(d.bin, home)...)
	for _, p := range d.sandboxRead {
		read = append(read, resolvePath(p))
	}
	for _, p := range sb.ReadPaths {
		r, err := check("sandbox.readPaths", p)
		if err != nil {
			return nil, err
		}
		read = append(read, r)
	}
	for _, p := range sb.WritePaths {
		r, err := check("sandbox.writePaths", p)
		if err != nil {
			return nil, err
		}
		write = append(write, r)
	}

	if pc := sb.PackageCache; pc != nil {
		src, err := check("sandbox.packageCache.path", pc.Path)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			return nil, invalid("sandbox.packageCache.path %q is not a directory that exists", string(pc.Path))
		}
		mode := pc.Mode
		if mode == "" {
			mode = fleet.PackageCachePrivate
		}
		st := &fleet.SandboxPackageCacheState{Mode: mode}
		switch mode {
		case fleet.PackageCacheShared:
			st.Path = fleet.AbsolutePath(src)
			write = append(write, src)
		case fleet.PackageCachePrivate:
			dst := filepath.Join(resolvePath(sessionDir), "pkgcache")
			how := priorCopy
			if how == "" {
				var err error
				how, err = copyTree(src, dst)
				if err != nil {
					return nil, &fleet.Error{Kind: fleet.ErrorUnreachable, Message: fmt.Sprintf("create: copying the package cache for this session: %v", err), Machine: d.machine}
				}
			} else if info, err := os.Stat(dst); err != nil || !info.IsDir() {
				return nil, invalid("the session's private package cache is gone from %q", dst)
			}
			st.Path, st.Copy = fleet.AbsolutePath(dst), how
			// dst is inside the session directory already granted writable.
		}
		plan.cache = st
	}

	plan.read = dedupe(append(read, write...))
	plan.write = dedupe(write)
	return plan, nil
}

// copyTree makes dst a copy of src, copy-on-write where the filesystem offers
// it, and reports which it was. `cp -c` fails rather than copying when the
// filesystem cannot clone, so the fallback is a separate, plain copy — and the
// report says so, since "instant" and "slow" are not the same promise.
func copyTree(src, dst string) (string, error) {
	clone, cloneErr := exec.Command("/bin/cp", "-cR", src, dst).CombinedOutput()
	if cloneErr == nil {
		return "clone", nil
	}
	_ = os.RemoveAll(dst)
	if plain, err := exec.Command("/bin/cp", "-R", src, dst).CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s / %s", err, strings.TrimSpace(string(clone)), strings.TrimSpace(string(plain)))
	}
	return "copy", nil
}

// profile renders the plan as sandbox-exec profile text.
func (p *sandboxPlan) profile() (string, error) {
	return (&sandbox.Plan{Network: p.network, Read: p.read, Write: p.write}).Profile()
}
