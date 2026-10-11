// Package privfs creates files and directories only their owner can use.
//
// Several places write something that must not be readable by the machine's
// other accounts: an enrolled principal's token, the backup of the principal
// table, the state directory, and every session's own directory. They used to
// say so with a POSIX mode (0700, 0600), which Windows does not have: Go
// reports and honours no such mode there, so the same call quietly produced a
// file that inherited whatever its parent's access list said.
//
// One package answers it per platform:
//
//   - Unix: exactly the calls this code made before — mode 0700 for a
//     directory, 0600 for a file, and nothing else.
//   - Windows: a protected access list naming only the current user, applied AT
//     CREATION through the security attributes of the create call, so there is
//     no moment at which the object exists with the inherited list. An object
//     that already exists is fixed in place (WriteFile does it).
//
// Check reads the answer back, so a diagnostic does not have to guess it from
// a mode that is not there. The Windows access-list text is parsed by a pure
// function (sddl.go) that is tested on every platform.
//
// Standard library only: the Windows calls go through advapi32.dll.
package privfs

// Verdict is what Check concluded about a path.
type Verdict int

const (
	// Unknown means the answer could not be read, so nothing is claimed.
	Unknown Verdict = iota
	// OwnerOnly means only the owning user is granted any access.
	OwnerOnly
	// Loose means someone else is granted access, or access is not restricted.
	Loose
)

func (v Verdict) String() string {
	switch v {
	case OwnerOnly:
		return "owner-only"
	case Loose:
		return "loose"
	}
	return "unknown"
}

// Report is Check's answer.
type Report struct {
	Verdict Verdict
	// Detail says what makes it Loose or Unknown, in a form fit for one line of
	// a diagnostic. Empty for OwnerOnly.
	Detail string
}
