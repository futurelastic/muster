package tmux

import (
	"context"
	"strconv"
	"strings"
	"unicode"
)

// Session naming, owned by this driver.
//
// # Why the rules live here rather than in a client
//
// They were previously applied by the on-machine launcher and by nothing else.
// A session created through the API took `spec.Name` verbatim, so three things
// diverged at once: a name the launcher would have rewritten was accepted
// as-is, a name that collided was a hard failure where the launcher would have
// numbered it, and the result carried no type marker, so tooling that keys on
// one saw a different shape.
//
// Rules applied by whichever client got there first are not rules; they are a
// convention that holds until a second client exists. This driver is the one
// place every creation path passes through, so it is where they belong. A
// client MAY pre-sanitize — it will get the same answer, because these
// functions are idempotent — but it is no longer the only thing standing
// between a bad name and the multiplexer.
//
// # The invariant that matters most
//
// The resolved name is used for the multiplexer session, for the remote-control
// binding, and for the agent's own name — the SAME string in all three places,
// from birth. Those three drifting apart is what makes a session unreachable
// from a remote client while still looking healthy locally, and it is not
// something a caller can repair afterwards.

// isNameBody reports whether r belongs to the body of a name, as opposed to
// the decoration trailing it: a letter or a number in ANY script, or one of the
// separators '.', '_' and '-'.
//
// Not an ASCII set. A name is typed by a person, in the language they work in,
// so its last character is as likely to be 'ý', 'ь' or '章' as 'h'. Treating
// every non-ASCII rune as decoration read such a name as already marked: the
// caller's marker was silently dropped, and a counter was inserted inside the
// last word (#360). Markers are symbols and emoji, which are neither letters
// nor numbers, so they stay decoration.
//
// This classifies a single rune. A combining mark (accent, variation selector)
// has no class of its own; see bodyAt.
func isNameBody(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '.' || r == '_' || r == '-'
}

// bodyAt reports whether runes[i] belongs to the name body, with a combining
// mark taking the class of the rune it follows.
//
// A mark is neither a letter nor a number, so isNameBody alone would split the
// two halves of a character that is one unit to the eye. Both ways of getting
// it wrong are real: an accent written as base letter plus U+0301 (the form
// text copied from a file path often arrives in) must stay with its letter, and
// the emoji variation selector U+FE0F must stay with the symbol it selects the
// presentation of, so that a marker like "⚙️" is carried whole and not split
// with its selector left behind in the body.
//
// A mark with no base in front of it (the start of the string) has nothing to
// follow and counts as decoration.
func bodyAt(runes []rune, i int) bool {
	for i > 0 && unicode.IsMark(runes[i]) {
		i--
	}
	return !unicode.IsMark(runes[i]) && isNameBody(runes[i])
}

// sanitizeName removes what the multiplexer cannot hold, and nothing else.
//
// A denylist rather than an allowlist, which is safe here for a specific
// reason: names reach the multiplexer and the agent bound as ARGV, never
// spliced into a shell command string (see loginWrap), so an arbitrary
// printable byte is not an injection risk on the create path. Narrowing to an
// allowlist would instead break every name carrying a non-ASCII marker, which
// is most of them.
//
// Each removal has a cause:
//
//   - control characters: unrepresentable, and they corrupt the field-separated
//     enumeration this driver parses.
//   - ':' — the multiplexer's own target separator, so a name containing one
//     addresses something else.
//   - '.' becomes '-', because the multiplexer silently mangles '.' to '_' and
//     a name that changes underneath you is worse than one you chose.
//   - a LEADING '-', which any argv parser downstream reads as a flag.
func sanitizeName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r < 0x20 || r == 0x7f:
			// dropped
		case r == ':':
			// dropped
		case r == '.':
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "-")
}

// decoration returns the trailing run of characters that are not part of a
// name's body (isNameBody, bodyAt) — the session-type marker, or a suffix some
// other tool wrote.
//
// Stated structurally, WITHOUT naming any particular marker. Hardcoding a set
// of glyphs here would move the coupling rather than remove it, and a new
// session type would break a driver that has no business knowing the
// vocabulary. What this driver enforces is the SHAPE of the rule: a marker is
// carried, never stacked.
func decoration(name string) string {
	runes := []rune(name)
	i := len(runes)
	for i > 0 && !bodyAt(runes, i-1) {
		i--
	}
	// A name that is ENTIRELY decoration is not a decorated
	// name, it is just a name. Requiring at least one body rune in front is
	// what keeps "everything is decoration" from being the answer.
	if i == 0 {
		return ""
	}
	return string(runes[i:])
}

// markerState is muster #96's answer to "does name already carry
// marker" — exact when a session record is available to answer from,
// honest about not knowing when one is not.
//
// # Why a tri-state, not a bool
//
// The question has three real answers, not two. applyMarker used to
// collapse "confirmed absent" and "no idea, guess from the string" into one
// path, because a string is all it had. A session record (reconcile.go's
// sessionRecord) can now say which of the two it actually is, for any
// session THIS driver resolved a name for — and for one it did not (an
// adopted session, a foreign one, a cold store), markerUnknown falls back
// to exactly the string heuristic this driver already had.
type markerState int

const (
	// markerUnknown means no record answers the question: fall back to the
	// suffix heuristic (decoration/HasSuffix) — today's behaviour,
	// unchanged, for a name this driver has no record of having resolved
	// itself.
	markerUnknown markerState = iota
	// markerApplied means a session record says THIS driver already
	// appended marker to name. Exact for any alphabet — no string
	// comparison needed or made.
	markerApplied
	// markerAbsent means a session record says THIS driver resolved name
	// WITHOUT appending marker. Also exact: append unconditionally, even
	// if name happens to end in the same characters as marker — the
	// record, not the string, is what answers "already applied" now.
	markerAbsent
)

// applyMarker appends the caller's session-type marker, deciding whether it
// is already there the way known says to.
//
// known == markerUnknown reproduces the original heuristic byte-for-byte:
// unless the name already ends in that marker OR already carries any
// trailing decoration from an earlier type. Both conditions are "carry,
// never stack": the first catches a marker drawn only from the same
// alphabet as the name body, which decoration() alone cannot see; the
// second preserves the older rule that a name already ending in decoration
// is left alone. This is the ambiguous case #90 could not close
// from the string alone (see numberedName's own comment) — a record
// resolves it exactly instead; see markerState.
func applyMarker(name, marker string, known markerState) string {
	if marker == "" {
		return name
	}
	switch known {
	case markerApplied:
		return name
	case markerAbsent:
		return name + marker
	default: // markerUnknown
		if decoration(name) != "" || strings.HasSuffix(name, marker) {
			return name
		}
		return name + marker
	}
}

// numberedName produces the n-th candidate for a colliding name, inserting the
// counter BEFORE any trailing decoration.
//
// Before, not after, because the decoration is what tooling keys on: a marker
// that has a number after it is no longer a trailing marker, and the session
// stops being recognisable as its own type at exactly the moment there are two
// of them.
//
// This function relies only on decoration() to decide where a trailing marker
// sits, and never on strings.HasSuffix(name, marker). A suffix check cannot
// distinguish an applied ascii-alphabet marker from a name that coincidentally
// ends in the same characters; guessing wrong here doesn't merely skip a
// marker — it actively cuts the name apart and misplaces the counter. See issue #90.
func numberedName(name string, n int) string {
	if n < 2 {
		return name
	}
	deco := decoration(name)
	base := strings.TrimSuffix(name, deco)
	return base + "-" + strconv.Itoa(n) + deco
}

// liveNames lists the multiplexer's current session names.
//
// One subprocess, not one per candidate. The alternative — probing each
// candidate with has-session — costs a spawn per collision and needs the exact
// -t "=NAME" pin to be correct, because an unpinned target resolves by prefix
// and an AMBIGUOUS prefix reports "no such session" rather than a hit. That
// reads as "the name is free" and hands the multiplexer a name it will refuse.
// Enumerating sidesteps the whole trap.
func (d *Driver) liveNames(ctx context.Context) map[string]bool {
	out, err := d.run(ctx, d.bin, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		// No enumeration is not the same as no sessions, and this must not
		// invent an empty fleet: returning nil makes resolveName skip
		// numbering, and the create that follows fails loudly on a duplicate
		// name rather than silently targeting somebody else's session.
		return nil
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names[line] = true
		}
	}
	return names
}

// maxNameAttempts bounds the collision search. A machine with this many
// same-named sessions has a problem numbering cannot fix, and an unbounded
// loop here would spin instead of saying so.
const maxNameAttempts = 64

// resolveName turns a requested name into the one canonical string this
// session will carry everywhere.
//
// Order matters and is not arbitrary: sanitize, then mark, then number. A
// number applied before the marker would be swallowed by the marker guard, and
// a marker applied after numbering would sit behind the counter where nothing
// keying on a trailing marker can see it.
//
// The second return, applied, reports whether THIS call appended marker —
// muster #96/#97's fact for the caller (Create) to hand to
// noteAssertedName, so the NEXT resolveName for this same string can answer
// markerStateFor exactly instead of guessing again.
func (d *Driver) resolveName(ctx context.Context, requested, marker string) (name string, applied bool, ok bool) {
	marker = sanitizeName(marker)
	sanitized := sanitizeName(requested)
	known := d.markerStateFor(sanitized, marker)
	name = applyMarker(sanitized, marker, known)
	applied = marker != "" && name != sanitized
	if name == "" {
		return "", false, false
	}
	taken := d.liveNames(ctx)
	if taken == nil {
		return name, applied, true
	}
	for n := 1; n <= maxNameAttempts; n++ {
		candidate := numberedName(name, n)
		if !taken[candidate] {
			return candidate, applied, true
		}
	}
	return "", false, false
}
