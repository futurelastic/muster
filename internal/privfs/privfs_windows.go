//go:build windows

package privfs

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Label is how a user-facing message names what these functions create.
const Label = "owner-only"

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procConvertStringSDToSD    = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procConvertSDToStringSD    = advapi32.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	procGetSecurityDescriptorD = advapi32.NewProc("GetSecurityDescriptorDacl")
	procSetNamedSecurityInfo   = advapi32.NewProc("SetNamedSecurityInfoW")
	procGetNamedSecurityInfo   = advapi32.NewProc("GetNamedSecurityInfoW")
)

const (
	sddlRevision1 = 1

	seFileObject = 1

	daclSecurityInformation          = 0x00000004
	protectedDACLSecurityInformation = 0x80000000

	genericRead  = 0x80000000
	genericWrite = 0x40000000

	fileShareRead  = 0x1
	fileShareWrite = 0x2
	createNew      = 1

	fileAttributeNormal = 0x80
)

// userSID is the SID of the account this process runs as, in S- form.
var userSID = sync.OnceValues(func() (string, error) {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", fmt.Errorf("privfs: opening the process token: %w", err)
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("privfs: reading the token's user: %w", err)
	}
	return u.User.Sid.String()
})

// A protected list naming only the current user. The directory form also
// carries OBJECT_INHERIT and CONTAINER_INHERIT, so what is created inside it by
// a process that knows nothing of this package is owner-only too. No owner
// section: the owner is the token's default one, which removes a way to fail.
func sddlFor(sid string, dir bool) string {
	if dir {
		return "D:P(A;OICI;FA;;;" + sid + ")"
	}
	return "D:P(A;;FA;;;" + sid + ")"
}

// attrs is a security descriptor built from SDDL, ready to hand to a create
// call. Free it when the create is done.
type attrs struct {
	sd syscall.Handle // LocalAlloc'd self-relative descriptor
	sa syscall.SecurityAttributes
}

func newAttrs(dir bool) (*attrs, error) {
	sid, err := userSID()
	if err != nil {
		return nil, err
	}
	str, err := syscall.UTF16PtrFromString(sddlFor(sid, dir))
	if err != nil {
		return nil, err
	}
	var sd syscall.Handle
	r, _, e := procConvertStringSDToSD.Call(
		uintptr(unsafe.Pointer(str)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		return nil, fmt.Errorf("privfs: building the access list: %w", e)
	}
	a := &attrs{sd: sd}
	a.sa = syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: uintptr(sd),
	}
	return a, nil
}

func (a *attrs) free() { _, _ = syscall.LocalFree(a.sd) }

// MkdirAll creates path and any missing parents, each with the owner-only
// protected list applied at creation. A directory that already exists is left
// as it is (as os.MkdirAll leaves it); Check reports what it is.
func MkdirAll(path string) error {
	if fi, err := os.Stat(path); err == nil {
		if fi.IsDir() {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}
	if parent := filepath.Dir(path); parent != path && parent != "." {
		if err := MkdirAll(parent); err != nil {
			return err
		}
	}
	a, err := newAttrs(true)
	if err != nil {
		return err
	}
	defer a.free()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	if err := syscall.CreateDirectory(p, &a.sa); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if fi, serr := os.Stat(path); serr == nil && fi.IsDir() {
				return nil
			}
		}
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

// createNewFile creates a file that must not exist, with the owner-only list
// applied at creation.
func createNewFile(path string, access uint32) (*os.File, error) {
	a, err := newAttrs(false)
	if err != nil {
		return nil, err
	}
	defer a.free()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, access, fileShareRead|fileShareWrite, &a.sa, createNew, fileAttributeNormal, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// OpenExcl creates path for writing and fails with an error satisfying
// errors.Is(err, fs.ErrExist) if it exists.
func OpenExcl(path string) (*os.File, error) {
	return createNewFile(path, genericWrite)
}

// WriteFile writes data to path. A new file is created owner-only; one that
// exists is made owner-only first and then truncated, so its contents are
// never readable under the old list.
func WriteFile(path string, data []byte) error {
	f, err := createNewFile(path, genericWrite)
	if err == nil {
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		return cerr
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := restrict(path, false); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// CreateTemp creates a new file in dir (the temporary directory when empty)
// whose name is pattern with its last "*" replaced by a random string, opened
// for reading and writing, as os.CreateTemp does — owner-only.
func CreateTemp(dir, pattern string) (*os.File, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	prefix, suffix := splitPattern(pattern)
	for try := 0; try < 10000; try++ {
		name := filepath.Join(dir, prefix+randomString()+suffix)
		f, err := createNewFile(name, genericRead|genericWrite)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: fs.ErrExist}
}

// MkdirTemp creates a new directory in dir whose name is pattern with its last
// "*" replaced by a random string, as os.MkdirTemp does — owner-only.
func MkdirTemp(dir, pattern string) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	prefix, suffix := splitPattern(pattern)
	a, err := newAttrs(true)
	if err != nil {
		return "", err
	}
	defer a.free()
	for try := 0; try < 10000; try++ {
		name := filepath.Join(dir, prefix+randomString()+suffix)
		p, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			return "", &os.PathError{Op: "mkdirtemp", Path: name, Err: err}
		}
		err = syscall.CreateDirectory(p, &a.sa)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", &os.PathError{Op: "mkdirtemp", Path: name, Err: err}
		}
		return name, nil
	}
	return "", &os.PathError{Op: "mkdirtemp", Path: filepath.Join(dir, pattern), Err: fs.ErrExist}
}

func splitPattern(pattern string) (prefix, suffix string) {
	if i := strings.LastIndexByte(pattern, '*'); i >= 0 {
		return pattern[:i], pattern[i+1:]
	}
	return pattern, ""
}

func randomString() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return n.String()
}

// restrict replaces the list of an object that already exists with the
// owner-only protected one.
func restrict(path string, dir bool) error {
	a, err := newAttrs(dir)
	if err != nil {
		return err
	}
	defer a.free()
	var present, defaulted int32
	var acl uintptr
	r, _, e := procGetSecurityDescriptorD.Call(
		uintptr(a.sd),
		uintptr(unsafe.Pointer(&present)),
		uintptr(unsafe.Pointer(&acl)),
		uintptr(unsafe.Pointer(&defaulted)),
	)
	if r == 0 {
		return fmt.Errorf("privfs: reading the access list: %w", e)
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "chmod", Path: path, Err: err}
	}
	// SetNamedSecurityInfoW returns the error code itself, not a BOOL.
	code, _, _ := procSetNamedSecurityInfo.Call(
		uintptr(unsafe.Pointer(p)),
		seFileObject,
		daclSecurityInformation|protectedDACLSecurityInformation,
		0, 0, acl, 0,
	)
	if code != 0 {
		return &os.PathError{Op: "chmod", Path: path, Err: syscall.Errno(code)}
	}
	return nil
}

// Check reads path's access list and says whether it is owner-only.
func Check(path string) Report {
	if _, err := os.Stat(path); err != nil {
		return Report{Verdict: Unknown, Detail: err.Error()}
	}
	text, err := daclText(path)
	if err != nil {
		return Report{Verdict: Unknown, Detail: err.Error()}
	}
	d, err := parseSDDL(text)
	if err != nil {
		return Report{Verdict: Unknown, Detail: err.Error()}
	}
	sid, err := userSID()
	if err != nil {
		return Report{Verdict: Unknown, Detail: err.Error()}
	}
	v, why := judge(d, sid)
	return Report{Verdict: v, Detail: why}
}

// WritableByOthers reads path's access list and returns who, other than the
// current user and the machine's own accounts (system, administrators), is
// granted a right to change the file: write or append data, delete, change its
// permissions or take its ownership — directly or by inheritance. An empty
// result means nobody is. The error is non-nil when the list could not be read
// or understood; the caller decides whether that refuses (a check guarding code
// that will be run should).
func WritableByOthers(path string) ([]string, error) {
	text, err := daclText(path)
	if err != nil {
		return nil, err
	}
	d, err := parseSDDL(text)
	if err != nil {
		return nil, err
	}
	sid, err := userSID()
	if err != nil {
		return nil, err
	}
	return writableBy(d, sid), nil
}

// daclText returns the SDDL text of path's discretionary access list.
func daclText(path string) (string, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var sd syscall.Handle
	code, _, _ := procGetNamedSecurityInfo.Call(
		uintptr(unsafe.Pointer(p)), seFileObject, daclSecurityInformation,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&sd)),
	)
	if code != 0 {
		return "", fmt.Errorf("privfs: reading the access list of %s: %w", path, syscall.Errno(code))
	}
	defer syscall.LocalFree(sd)
	var out *uint16
	r, _, e := procConvertSDToStringSD.Call(
		uintptr(sd), sddlRevision1, daclSecurityInformation,
		uintptr(unsafe.Pointer(&out)), 0,
	)
	if r == 0 {
		return "", fmt.Errorf("privfs: rendering the access list of %s: %w", path, e)
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(out)))
	return utf16PtrToString(out), nil
}

// utf16PtrToString reads a NUL-terminated UTF-16 string the system allocated.
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*uint16)(unsafe.Add(unsafe.Pointer(p), n*2)) != 0 {
		n++
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}
