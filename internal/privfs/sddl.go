package privfs

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file is the pure half of the Windows check: it parses the textual form
// of a security descriptor (SDDL) and decides whether the access list it names
// is owner-only. It has no operating-system dependency, so the parser — the part
// that is easy to get subtly wrong — is tested on every platform, not only on a
// Windows runner.

// The rights this check understands, as access masks.
var sddlRights = map[string]uint32{
	"GA": 0x10000000, "GR": 0x80000000, "GW": 0x40000000, "GX": 0x20000000,
	"RC": 0x00020000, "SD": 0x00010000, "WD": 0x00040000, "WO": 0x00080000,
	"FA": 0x001F01FF, "FR": 0x00120089, "FW": 0x00120116, "FX": 0x001200A0,
}

// sddlAliases are the two-letter trustee aliases SDDL uses for well-known SIDs.
// A service running as the local system account is rendered as "SY", not as
// "S-1-5-18", so a comparison of trustees has to normalise both sides.
var sddlAliases = map[string]string{
	"AN": "S-1-5-7", "AU": "S-1-5-11", "BA": "S-1-5-32-544", "BG": "S-1-5-32-546",
	"BU": "S-1-5-32-545", "CG": "S-1-3-1", "CO": "S-1-3-0", "IU": "S-1-5-4",
	"LS": "S-1-5-19", "NS": "S-1-5-20", "OW": "S-1-3-4", "PS": "S-1-5-10",
	"SY": "S-1-5-18", "WD": "S-1-1-0", "AO": "S-1-5-32-548", "RC": "S-1-5-12",
	"SU": "S-1-5-6", "NU": "S-1-5-2", "AC": "S-1-15-2-1", "ED": "S-1-5-9",
}

// ace is one access-control entry.
type ace struct {
	Type    string          // "A" allow, "D" deny, "AU" audit, ...
	Flags   map[string]bool // "OI", "CI", "ID", "NP", "IO", ...
	Rights  uint32
	Trustee string // a SID in S- form; an unknown alias is kept as written
}

// dacl is a parsed discretionary access list.
type dacl struct {
	// Null is a missing list ("D:NO_ACCESS_CONTROL"): everyone has every right.
	Null bool
	// Protected is SE_DACL_PROTECTED: the list does not inherit from the parent.
	Protected bool
	ACEs      []ace
}

// parseSDDL parses the DACL part of an SDDL string. Owner, group and system-ACL
// sections are skipped. It refuses text it does not understand rather than
// guessing: a check that cannot read the list must say Unknown.
func parseSDDL(s string) (dacl, error) {
	var d dacl
	i := strings.Index(s, "D:")
	if i < 0 {
		return d, fmt.Errorf("sddl: no DACL section in %q", s)
	}
	rest := s[i+2:]
	// The DACL section runs to the next section marker ("S:" system ACL), if any.
	if j := strings.Index(rest, "S:"); j >= 0 {
		// An ACE's trustee or rights cannot contain "S:" ("S-" is the SID prefix),
		// so the first occurrence outside parentheses ends the section.
		if k := indexOutsideParens(rest, "S:"); k >= 0 {
			rest = rest[:k]
		}
	}
	if strings.HasPrefix(rest, "NO_ACCESS_CONTROL") {
		d.Null = true
		return d, nil
	}
	// ACL flags precede the first '('.
	flagsEnd := strings.IndexByte(rest, '(')
	if flagsEnd < 0 {
		flagsEnd = len(rest)
	}
	flags := rest[:flagsEnd]
	for len(flags) > 0 {
		switch {
		case strings.HasPrefix(flags, "P"):
			d.Protected = true
			flags = flags[1:]
		case strings.HasPrefix(flags, "AI"), strings.HasPrefix(flags, "AR"), strings.HasPrefix(flags, "NO"):
			flags = flags[2:]
		default:
			return d, fmt.Errorf("sddl: unrecognised ACL flags %q", rest[:flagsEnd])
		}
	}
	body := rest[flagsEnd:]
	for len(body) > 0 {
		if body[0] != '(' {
			return d, fmt.Errorf("sddl: unexpected %q between entries", body)
		}
		end := strings.IndexByte(body, ')')
		if end < 0 {
			return d, fmt.Errorf("sddl: unterminated entry in %q", s)
		}
		a, err := parseACE(body[1:end])
		if err != nil {
			return d, err
		}
		d.ACEs = append(d.ACEs, a)
		body = body[end+1:]
	}
	return d, nil
}

func indexOutsideParens(s, sep string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && strings.HasPrefix(s[i:], sep) {
				return i
			}
		}
	}
	return -1
}

// parseACE parses "type;flags;rights;object-guid;inherit-guid;trustee[;...]".
func parseACE(s string) (ace, error) {
	parts := strings.Split(s, ";")
	if len(parts) < 6 {
		return ace{}, fmt.Errorf("sddl: entry %q has %d fields, want at least 6", s, len(parts))
	}
	a := ace{Type: parts[0], Flags: map[string]bool{}}
	for f := parts[1]; len(f) > 0; {
		if len(f) < 2 {
			return ace{}, fmt.Errorf("sddl: entry %q has malformed flags %q", s, parts[1])
		}
		a.Flags[f[:2]] = true
		f = f[2:]
	}
	rights, err := parseRights(parts[2])
	if err != nil {
		return ace{}, fmt.Errorf("sddl: entry %q: %w", s, err)
	}
	a.Rights = rights
	t := parts[5]
	if t == "" {
		return ace{}, fmt.Errorf("sddl: entry %q names no trustee", s)
	}
	if sid, ok := sddlAliases[t]; ok {
		t = sid
	}
	a.Trustee = strings.ToUpper(t)
	return a, nil
}

func parseRights(s string) (uint32, error) {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		v, err := strconv.ParseUint(s[2:], 16, 32)
		if err != nil {
			return 0, fmt.Errorf("rights %q: %w", s, err)
		}
		return uint32(v), nil
	}
	var mask uint32
	for r := s; len(r) > 0; {
		if len(r) < 2 {
			return 0, fmt.Errorf("rights %q are malformed", s)
		}
		m, ok := sddlRights[r[:2]]
		if !ok {
			return 0, fmt.Errorf("rights token %q is not one this check understands", r[:2])
		}
		mask |= m
		r = r[2:]
	}
	return mask, nil
}

// judge decides whether d grants access to the user whose SID is user, and to
// nobody else. It returns the verdict and, when that is not OwnerOnly, a line
// saying why.
//
// The trustees are compared, not the flags: an entry inherited from an
// owner-only parent is exactly as private as one set directly, and an entry for
// anyone else is not private however it got there. Protected says whether the
// list is cut off from its parent — reported by the caller, not required here.
func judge(d dacl, user string) (Verdict, string) {
	if d.Null {
		return Loose, "there is no access list, so everyone has every right"
	}
	user = strings.ToUpper(user)
	if len(d.ACEs) == 0 {
		return Loose, "the access list is empty, so the owner is the only one who can ever change it"
	}
	var others []string
	userSeen := false
	for _, a := range d.ACEs {
		switch {
		case a.Type != "A":
			others = append(others, fmt.Sprintf("a %q entry for %s", a.Type, a.Trustee))
		case !sameTrustee(a.Trustee, user):
			others = append(others, fmt.Sprintf("access for %s", a.Trustee))
		default:
			// Whatever rights it grants, an allow entry for the user alone is
			// private.
			userSeen = true
		}
	}
	if len(others) > 0 {
		sort.Strings(others)
		return Loose, "the access list also grants " + strings.Join(dedupe(others), ", ")
	}
	if !userSeen {
		return Loose, "the access list does not grant the owner any access"
	}
	return OwnerOnly, ""
}

// sameTrustee reports whether the trustee as rendered in an entry is the user.
//
// Two aliases are domain-relative, so they cannot be expanded without the
// machine's own SID: "LA" is the account whose relative id is 500 (the built-in
// administrator) and "LG" is 501. When the user IS that account, the system
// renders the user's own entry as the alias, never as the long SID — the
// Windows runner this was first measured on runs as exactly that account.
func sameTrustee(trustee, user string) bool {
	if trustee == user {
		return true
	}
	if !strings.HasPrefix(user, "S-1-5-21-") {
		return false
	}
	switch trustee {
	case "LA":
		return strings.HasSuffix(user, "-500")
	case "LG":
		return strings.HasSuffix(user, "-501")
	}
	return false
}

func dedupe(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// The access-mask bits that let a trustee change what a file runs as, or whose
// it is: write or append data, delete, change the list, take ownership, and the
// generic rights that expand into those. Writing extended attributes or
// attributes alone is not here — neither changes the code in the file.
const (
	rightWriteData    = 0x00000002
	rightAppendData   = 0x00000004
	rightDelete       = 0x00010000
	rightWriteDAC     = 0x00040000
	rightWriteOwner   = 0x00080000
	rightGenericAll   = 0x10000000
	rightGenericWrite = 0x40000000

	writeRights = rightWriteData | rightAppendData | rightDelete | rightWriteDAC |
		rightWriteOwner | rightGenericAll | rightGenericWrite
)

// trustedWriters are the trustees whose write access to a file does not count as
// "someone else": the machine's own accounts, which can run code as the service
// whatever this file says. The current user is added by the caller.
//
// "CO" (creator owner) is the owner of the object, which is the current user or
// an administrator for a file the service's install put there.
var trustedWriters = map[string]bool{
	"S-1-5-18":     true, // local system
	"S-1-5-32-544": true, // built-in administrators
	"S-1-3-0":      true, // creator owner
	// TrustedInstaller, which owns the operating system's own files and appears
	// on most files under the program-files directories.
	"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464": true,
}

// trusteeClasses names the well-known trustees a reason is likely to meet, so
// the message says what kind of account can write instead of printing a SID.
var trusteeClasses = map[string]string{
	"S-1-1-0":      "Everyone",
	"S-1-5-11":     "Authenticated Users",
	"S-1-5-32-545": "Users",
	"S-1-5-32-546": "Guests",
	"S-1-5-32-547": "Power Users",
	"S-1-5-32-551": "Backup Operators",
	"S-1-5-4":      "the interactive logon group",
	"S-1-5-2":      "the network logon group",
	"S-1-5-7":      "the anonymous logon",
	"S-1-5-19":     "the local service account",
	"S-1-5-20":     "the network service account",
	"S-1-15-2-1":   "all application packages",
}

// trusteeClass names a trustee for a diagnostic.
func trusteeClass(sid string) string {
	if c, ok := trusteeClasses[sid]; ok {
		return c
	}
	return "the account " + sid
}

// writableBy returns the trustees, other than user and the machine's own
// accounts, that d grants a write, delete, change-permissions or take-ownership
// right on the file it belongs to (nil when there are none). A missing list
// ("D:NO_ACCESS_CONTROL") grants everyone every right.
//
// Only what applies to the file itself counts: an entry flagged inherit-only
// (IO) is a template for children and says nothing about this object. An allow
// entry is reduced by an earlier deny entry for the same trustee, which is the
// order the system evaluates a well-formed list in; a deny for a group does not
// reduce an allow for a different trustee, because whether the two overlap
// depends on who is in which group, and this reading has no token to ask.
func writableBy(d dacl, user string) []string {
	if d.Null {
		return []string{trusteeClass("S-1-1-0")}
	}
	user = strings.ToUpper(user)
	denied := map[string]uint32{}
	var who []string
	for _, a := range d.ACEs {
		if a.Flags["IO"] {
			continue
		}
		switch a.Type {
		case "D":
			denied[a.Trustee] |= a.Rights
		case "A", "XA", "OA":
			// "LA" is the built-in administrator account, which is as trusted as
			// the administrators group.
			if sameTrustee(a.Trustee, user) || trustedWriters[a.Trustee] || a.Trustee == "LA" {
				continue
			}
			if a.Rights&^denied[a.Trustee]&writeRights != 0 {
				who = append(who, trusteeClass(a.Trustee))
			}
		}
	}
	sort.Strings(who)
	return dedupe(who)
}
