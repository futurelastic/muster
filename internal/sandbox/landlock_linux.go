package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"syscall"
	"unsafe"
)

// Landlock, the Linux kernel's unprivileged file-access rules (muster #439).
//
// The service's binary applies a ruleset to ITSELF and then replaces itself with
// the agent, so the ruleset is inherited by every process the agent starts and
// cannot be removed or widened by any of them: it is the only thing the
// facility offers, and it is the property the macOS profile has too.
//
// There is no library here on purpose: the three calls are made by number, with
// the structures written into byte arrays. Two layouts are not what a Go struct
// would give, and both have a test that fails if they drift:
//
//   - the path rule's attribute is PACKED, twelve bytes (a 64-bit mask and a
//     32-bit descriptor), where a Go struct of the same two fields is sixteen;
//   - the ruleset's attribute grew by whole 64-bit words with each ABI, and the
//     kernel accepts a size only if everything beyond what it knows is zero, so
//     the size passed follows the ABI that was queried.

const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	landlockCreateRulesetVersion = 1 << 0

	landlockRulePathBeneath = 1
	landlockRuleNetPort     = 2

	prSetNoNewPrivs = 38

	// oPath opens a path without opening what it names (O_PATH): the descriptor a
	// rule is attached to. The value is the same on amd64 and arm64.
	oPath = 0o10000000
)

// Filesystem access rights, by the ABI that introduced them.
const (
	fsExecute    = 1 << 0
	fsWriteFile  = 1 << 1
	fsReadFile   = 1 << 2
	fsReadDir    = 1 << 3
	fsRemoveDir  = 1 << 4
	fsRemoveFile = 1 << 5
	fsMakeSym    = 1 << 12 // the last of ABI 1's thirteen
	fsRefer      = 1 << 13 // ABI 2
	fsTruncate   = 1 << 14 // ABI 3
	fsIoctlDev   = 1 << 15 // ABI 5
)

// Network and scope rights.
const (
	netBindTCP    = 1 << 0 // ABI 4
	netConnectTCP = 1 << 1 // ABI 4

	scopeAbstractUnixSocket = 1 << 0 // ABI 6
	scopeSignal             = 1 << 1 // ABI 6
)

// landlockABI asks the kernel which Landlock ABI it implements. The errors are
// the kernel's own, because the two that matter say different things: ENOSYS is a
// kernel built without it (or a container profile that blocks the call), and
// EOPNOTSUPP is one built with it but booted without it in the active modules.
func landlockABI() (int, error) {
	v, _, e := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if e != 0 {
		return 0, e
	}
	return int(v), nil
}

// handledFS is every filesystem right the kernel of this ABI can restrict. Every
// one is handled, so each is denied unless a rule grants it; ABI 3's truncate in
// particular, without which `truncate(2)` would empty any file of the same user.
func handledFS(abi int) uint64 {
	h := uint64(1<<13 - 1) // ABI 1: execute .. make-sym
	if abi >= 2 {
		h |= fsRefer
	}
	if abi >= 3 {
		h |= fsTruncate
	}
	if abi >= 5 {
		h |= fsIoctlDev
	}
	return h
}

// rulesetAttrSize is the size of landlock_ruleset_attr this ABI's kernel knows.
func rulesetAttrSize(abi int) uintptr {
	switch {
	case abi >= 6:
		return 24
	case abi >= 4:
		return 16
	}
	return 8
}

// rights are the access rights a rule grants, masked to what is handled: a rule
// naming a right the ruleset does not handle is refused by the kernel.
type rights struct{ fs uint64 }

// read is what reading grants: files, listings and execution, on a
// directory; on anything else only the file rights a rule may carry.
func (r rights) read(isDir bool) uint64 {
	if isDir {
		return (fsReadFile | fsReadDir | fsExecute) & r.fs
	}
	return (fsReadFile | fsExecute) & r.fs
}

// write is what writing grants: everything handled, on a directory;
// reading, writing and truncating, on a file.
func (r rights) write(isDir bool) uint64 {
	if isDir {
		return r.fs
	}
	return (fsReadFile | fsWriteFile | fsTruncate | fsExecute) & r.fs
}

// device is what a device file or directory is granted: to read it, write
// it and, where the kernel can tell, to control it.
func (r rights) device() uint64 {
	return (fsReadFile | fsWriteFile | fsIoctlDev) & r.fs
}

// ptsPath matches the pseudo-terminal a standard stream of this process is.
var ptsPath = regexp.MustCompile(`^/dev/pts/[0-9]+$`)

// ownTerminals are the pseudo-terminals the launcher itself was started on.
func ownTerminals() []string {
	var out []string
	seen := map[string]bool{}
	for fd := 0; fd <= 2; fd++ {
		p, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		if err == nil && ptsPath.MatchString(p) && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// addGrant attaches a rule to a path that exists, with the rights pick chooses
// once it knows whether the path is a directory.
func addGrant(ruleset int, path string, pick func(isDir bool) uint64) error {
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// struct landlock_path_beneath_attr is packed: a 64-bit mask, then a 32-bit fd.
	var attr [12]byte
	binary.LittleEndian.PutUint64(attr[0:], pick(st.Mode&syscall.S_IFMT == syscall.S_IFDIR))
	binary.LittleEndian.PutUint32(attr[8:], uint32(fd))
	if _, _, e := syscall.Syscall6(sysLandlockAddRule, uintptr(ruleset), landlockRulePathBeneath, uintptr(unsafe.Pointer(&attr[0])), 0, 0, 0); e != 0 {
		return fmt.Errorf("granting %s: %w", path, e)
	}
	return nil
}

// applyLandlock builds the ruleset a policy describes and restricts the CALLING
// THREAD to it, after setting no_new_privs (which the kernel requires, and which
// also stops a set-user-id helper from lifting the confinement). The caller has
// locked its thread to the OS thread, and is about to execve from it: execve
// drops every other thread, so the new program carries exactly this thread's
// restriction.
//
// A path that does not exist refuses: the driver settled the plan before it
// wrote the policy, and a grant that silently did not take is a weaker profile
// than the one reported. A device that does not exist on this host is skipped,
// because a host without a given device is not a host with a hole.
func applyLandlock(pol LinuxPolicy, abi int, terminals []string) error {
	rt := rights{fs: handledFS(abi)}
	var net, scoped uint64
	if abi >= 4 && pol.Network == "closed" {
		// No connect rule is ever added: not one outbound TCP connection, loopback
		// included. The only way to talk is to be connected to, and a session
		// may bind only the ports the policy names.
		net = netBindTCP | netConnectTCP
	}
	if abi >= 6 {
		scoped = scopeAbstractUnixSocket | scopeSignal
	}
	var attr [24]byte
	binary.LittleEndian.PutUint64(attr[0:], rt.fs)
	binary.LittleEndian.PutUint64(attr[8:], net)
	binary.LittleEndian.PutUint64(attr[16:], scoped)
	fd, _, e := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), rulesetAttrSize(abi), 0)
	if e != 0 {
		return fmt.Errorf("creating the ruleset: %w", e)
	}
	ruleset := int(fd)
	defer syscall.Close(ruleset)

	for _, p := range pol.Read {
		if err := addGrant(ruleset, p, rt.read); err != nil {
			return err
		}
	}
	for _, p := range pol.Write {
		if err := addGrant(ruleset, p, rt.write); err != nil {
			return err
		}
	}
	for _, p := range append(append([]string(nil), pol.Devices...), terminals...) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := addGrant(ruleset, p, func(bool) uint64 { return rt.device() }); err != nil {
			return err
		}
	}
	if net != 0 {
		for _, port := range pol.BindTCP {
			// struct landlock_net_port_attr: a 64-bit mask, a 64-bit port.
			var rule [16]byte
			binary.LittleEndian.PutUint64(rule[0:], netBindTCP)
			binary.LittleEndian.PutUint64(rule[8:], uint64(port))
			if _, _, e := syscall.Syscall6(sysLandlockAddRule, uintptr(ruleset), landlockRuleNetPort, uintptr(unsafe.Pointer(&rule[0])), 0, 0, 0); e != 0 {
				return fmt.Errorf("granting TCP port %d: %w", port, e)
			}
		}
	}

	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("setting no_new_privs: %w", e)
	}
	if _, _, e := syscall.Syscall(sysLandlockRestrictSelf, uintptr(ruleset), 0, 0); e != 0 {
		return fmt.Errorf("restricting the process: %w", e)
	}
	return nil
}
