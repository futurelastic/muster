package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
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
	// muster #432: a session is handed the variable each of these reads.
	sup.PackageManagers = fleet.PackageManagers()
	return sup, ""
}

// sandboxPlan is a resolved sandbox request: every path real, every default
// applied, ready to be rendered as a profile and reported back.
type sandboxPlan struct {
	// mechanism is sandbox.MechanismLinux on a host that confines with the Linux
	// mechanism, and empty for macOS's (the only one before muster #439).
	mechanism string
	network   fleet.SandboxNetwork
	read      []string
	write     []string
	cache     *fleet.SandboxPackageCacheState
}

// shared is the plan as the shared package renders it.
func (p *sandboxPlan) shared() *sandbox.Plan {
	return &sandbox.Plan{Mechanism: p.mechanism, Network: p.network, Read: p.read, Write: p.write}
}

// state renders the plan as the report a session carries.
func (p *sandboxPlan) state() *fleet.SandboxState {
	st := p.shared().State()
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
	return d.planSandboxFor(context.Background(), spec, sessionDir, "")
}

// planSandboxFor is planSandbox for a session that may already have a private
// package cache in its directory (muster #282): priorCopy, when set, is how that
// cache was made, and it is reused rather than copied over itself.
//
// ctx bounds the copy (muster #433): a caller that gives up, or a service that
// shuts down, stops it, and the error is then ctx's own rather than a failure of
// the copy. A caller that cannot make a copy passes a context that is never
// cancelled and is protected from needing one by priorCopy.
func (d *Driver) planSandboxFor(ctx context.Context, spec fleet.SessionSpec, sessionDir, priorCopy string) (*sandboxPlan, error) {
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
	// Which facility confines this host decides how a grant is made: macOS matches
	// a profile against paths that need not exist, Linux attaches a rule to a path
	// that does.
	linux := d.sandbox != nil && d.sandbox.Mechanism == sandbox.MechanismLinux
	// A grant that would include the service user's home puts back exactly what
	// the profile exists to remove.
	check := func(field string, p fleet.AbsolutePath) (string, error) {
		r := resolvePath(string(p))
		if home != "" && coversOrIs(r, home) {
			return "", invalid("%s: %q is, or contains, the service user's home directory; grant the specific directory a session needs", field, string(p))
		}
		// A Linux grant is made by opening the path, so a path that is not there
		// cannot be granted, and starting without it would be a weaker profile than
		// the one asked for.
		if linux {
			if err := sandbox.RequireExist(field, []string{r}); err != nil {
				return "", invalid("%s", err.Error())
			}
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
	if linux {
		plan.mechanism = sandbox.MechanismLinux
	}

	write := []string{cwd, resolvePath(sessionDir)}
	read := sandbox.SystemReadRootsFor(plan.mechanism)
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
		// muster #432: a manager the cache is said to serve must have its directory
		// there. Checked on the seed before it is copied, so a refusal costs no copy
		// — and, for a session being relaunched, on the copy it already has.
		managersAt := src
		if priorCopy != "" && mode == fleet.PackageCachePrivate {
			managersAt = filepath.Join(resolvePath(sessionDir), "pkgcache")
		}
		if err := checkManagerDirs(managersAt, pc.Managers); err != nil {
			return nil, invalid("sandbox.packageCache: %v", err)
		}
		st.Managers = append([]fleet.PackageManager(nil), pc.Managers...)
		switch mode {
		case fleet.PackageCacheShared:
			st.Path = fleet.AbsolutePath(src)
			write = append(write, src)
		case fleet.PackageCachePrivate:
			dst := filepath.Join(resolvePath(sessionDir), "pkgcache")
			how := priorCopy
			if how == "" {
				// muster #448: a seed that cannot be cloned is known before the copy
				// starts, so the refusal costs nothing. A relaunch (priorCopy) already
				// has its copy and is not asked again.
				if !pc.AllowFullCopy {
					if err := checkSeedVolume(src, dst); err != nil {
						return nil, invalid("sandbox.packageCache: %v", err)
					}
				}
				var err error
				how, err = copyTree(ctx, src, dst)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
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

	if linux {
		// What the operator configured as extra reads may name something this host
		// does not have; the report is what is enforced, so a grant that could not be
		// made is not reported as one.
		read = sandbox.PruneMissing(read)
	}
	plan.read = dedupe(append(read, write...))
	plan.write = dedupe(write)
	return plan, nil
}

// checkManagerDirs refuses a cache that lacks the directory of a manager it is
// said to serve (muster #432). The directory must be a real one: a link there
// would be copied as a link, and what it pointed at would not be the cache the
// session was promised.
func checkManagerDirs(cache string, managers []fleet.PackageManager) error {
	for _, m := range managers {
		info, err := os.Lstat(filepath.Join(cache, string(m)))
		if err != nil || !info.IsDir() {
			return fmt.Errorf("it is said to serve %s but has no %s/ directory (a real directory, not a link)", string(m), string(m))
		}
	}
	return nil
}

// managerEnv is the environment a cache serving managers adds to a session whose
// copy of it is at cache: each manager's variable, pointing at its own directory.
func managerEnv(cache string, managers []fleet.PackageManager) map[string]string {
	out := make(map[string]string, len(managers))
	for _, m := range managers {
		out[m.EnvVar()] = filepath.Join(cache, string(m))
	}
	return out
}

// errCloneUnsupported is what cloneDirectory answers where there is no call to
// clone a directory as one operation.
var errCloneUnsupported = errors.New("this platform has no call that clones a directory as one operation")

// The seams copyTree's and checkSeedVolume's tests use: the one-call clone, the
// program that makes the per-file copies, whether this host clones at all, and
// which volume a path is on.
var (
	cloneDir   = cloneDirectory
	copyBinary = "/bin/cp"
	hostClone  = hostClones
	volumeOf   = volumeOfPath
)

// volume is a filesystem a path is on: id tells two apart, mount names it for a
// reader.
type volume struct {
	id    uint64
	mount string
}

// checkSeedVolume refuses a private copy that could only be a plain one because
// the seed and the session's directory are on different volumes (muster #448).
// A clone cannot cross a volume, so the session would hold the seed's whole size
// and the create would wait for all of it to be copied (6.6 GiB took 180 s).
//
// It answers only where the host can clone at all: on a host with no clone call
// every create is a full copy, a different volume or not, and refusing one case
// of two would describe a difference that is not there. A volume it cannot read
// is not a refusal either; the create goes on as it did before this check. A
// same-volume filesystem that cannot clone still copies in full and reports
// `copy` — the device is what is known before the copy, not the filesystem's
// capability.
//
// dst need not exist: its volume is its nearest existing parent's.
func checkSeedVolume(src, dst string) error {
	if !hostClone() {
		return nil
	}
	from, err := volumeOf(src)
	if err != nil {
		return nil
	}
	to, err := volumeOf(dst)
	if err != nil {
		return nil
	}
	if from.id == to.id {
		return nil
	}
	return fmt.Errorf("the seed %q is on volume %q and the session's directory is on volume %q; a clone cannot cross volumes, "+
		"so each session would hold the seed's full size and the create would copy all of it first. "+
		"Keep the seed on the session directory's volume, or set allowFullCopy to accept the full copy", src, from.mount, to.mount)
}

// copyTree makes dst a copy of src, copy-on-write where the filesystem offers
// it, and reports which it was: "clone" or "copy".
//
// Three ways, in order. One clone of the whole directory (muster #433: about ten
// times faster than the next, because the cost of `cp -c` is a call and a walk
// per file, not per byte). Failing that, `cp -cR`; `cp -c` fails rather than
// copying when the filesystem cannot clone, so the last way is a separate, plain
// copy — and the report says so, since "instant" and "slow" are not the same
// promise. A seed on another volume than dst can clone nowhere, so it reaches the
// plain copy and holds its full size — which planSandboxFor refuses before it gets
// here unless the request says allowFullCopy (muster #448).
//
// ctx stops it. The copies are child processes and are killed; the clone is one
// call that cannot be interrupted, so a cancel during it takes effect when it
// returns, which is one clone's time and not the whole copy's. Either way dst is
// removed before copyTree returns and the error is ctx's, so a caller removing
// the directory dst is in never races a copy still writing to it.
//
// dst is cleared first: `cp` given a dst that exists copies src INTO it rather
// than making it, so what an earlier attempt left would turn a copy into a
// nested one. That also leaves a relaunch with no recorded copy (resume.go) with
// nothing of a cancelled start.
func copyTree(ctx context.Context, src, dst string) (string, error) {
	abandon := func(err error) (string, error) {
		_ = privfs.RemoveAll(dst) // best effort: the caller removes what holds it
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := privfs.RemoveAll(dst); err != nil {
		return "", fmt.Errorf("clearing %s before the copy: %w", dst, err)
	}

	cloneErr := cloneDir(src, dst)
	if cloneErr == nil {
		if err := ctx.Err(); err != nil {
			return abandon(err)
		}
		return "clone", nil
	}
	if err := ctx.Err(); err != nil {
		return abandon(err)
	}
	_ = privfs.RemoveAll(dst)

	// `cp -c` is a clone only where the platform's copy program says so (macOS).
	// Where it names something else — GNU coreutils reads -c as --preserve=context
	// and succeeds on a plain copy — trying it would report "clone" for a full copy,
	// which is the one thing the report must not say (cpCloneFlag, below).
	var clone []byte
	cpCloneErr := errors.New("this platform's copy program has no clone flag")
	if cpCloneFlag != "" {
		clone, cpCloneErr = exec.CommandContext(ctx, copyBinary, cpCloneFlag, src, dst).CombinedOutput()
		if cpCloneErr == nil {
			if err := ctx.Err(); err != nil {
				return abandon(err)
			}
			return "clone", nil
		}
		if err := ctx.Err(); err != nil {
			return abandon(err)
		}
		_ = privfs.RemoveAll(dst)
	}

	if plain, err := exec.CommandContext(ctx, copyBinary, "-R", src, dst).CombinedOutput(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return abandon(cerr)
		}
		_ = privfs.RemoveAll(dst)
		why := fmt.Sprintf("%v: %s / %s", err, strings.TrimSpace(string(clone)), strings.TrimSpace(string(plain)))
		if !errors.Is(cloneErr, errCloneUnsupported) {
			why += " / " + cloneErr.Error()
		}
		return "", errors.New(why)
	}
	if err := ctx.Err(); err != nil {
		return abandon(err)
	}
	return "copy", nil
}

// profile renders the plan as sandbox-exec profile text.
func (p *sandboxPlan) profile() (string, error) {
	return (&sandbox.Plan{Network: p.network, Read: p.read, Write: p.write}).Profile()
}

// launchFunc gives the command prefix that starts the runtime under a plan, for
// the port the server is about to listen on. A start that fails and is retried on
// another port asks again.
type launchFunc func(port int) ([]string, error)

// launcher renders the plan once, so a request that cannot be written down is
// refused before a process exists, and returns the function a start calls with its
// port. On macOS the prefix is `sandbox-exec -p <profile>` whatever the port. On
// Linux it is the service's own binary as the launcher, and under a closed network
// the port is the one thing the session may bind: the runtime's server, which the
// service connects into.
func (p *sandboxPlan) launcher() (launchFunc, error) {
	base := p.shared()
	if _, err := base.WrapInline(nil); err != nil {
		return nil, err
	}
	return func(port int) ([]string, error) {
		pl := *base
		if pl.Mechanism == sandbox.MechanismLinux && pl.Network == fleet.SandboxNetworkClosed {
			pl.BindTCP = []int{port}
		}
		return pl.WrapInline(nil)
	}, nil
}
