package tmux

import (
	"strings"
	"unicode"
)

// The refusal seam for text this driver's runtime does not treat as a
// message (#53).
//
// # Why this belongs to the driver
//
// `send` delivers caller text into a runtime that may read the SAME bytes
// two different ways: as a message, or as its own local syntax — and which
// one is a property of the runtime, not of this API. §2.1 draws the
// identical line for naming a session: a rule enforced by one caller is not
// a rule, it is a convention that holds until a second caller forgets it.
// The runtime a driver drives is the one thing only that driver knows for
// certain, so the pattern list lives here, not in the service that merely
// forwards `text` on to whichever driver is registered for the request.
//
// # Fail closed, and mean it
//
// A pattern that matches refuses outright — never escaped, never mangled,
// never delivered and hoped about. There is no repair here that turns
// hazardous text into safe text and still says what the caller meant; the
// only honest move is the one `respond` already models for prompts: refuse,
// and say why (§2.4, §5.7 — a refusal must not render like a delivery, and
// DeliveryReceipt already keeps the two apart by Outcome).
//
// # The leading-whitespace trap
//
// #53 measured this against seven runtimes: every one of them trims a
// message before testing its first character, so `" !rm -rf ..."` is read
// exactly as `"!rm -rf ..."` — a caller "defusing" the pattern with a
// leading space defuses nothing, because the trim happens on the runtime's
// side, not the sender's. A check here that tested text[0] directly would
// be exactly as blind as the composer-echo bug this package has already
// paid for once (F55): correct against the bytes a human typed, wrong
// against what the runtime actually reads. matchesRuntimeSyntax reproduces
// the SAME liberty the runtime takes, so a pattern here sees what the
// runtime sees.

// nonMessageInput is one shape of caller text this driver's runtime treats
// as its own local syntax rather than as a message to relay.
type nonMessageInput struct {
	// name distinguishes one refusal from another for a test's benefit; it
	// is never shown to a caller — reason is what explains the refusal.
	name string
	// matches reports whether text — after the same leading-whitespace trim
	// the runtime itself applies before reading its first character — is
	// this runtime's own syntax.
	matches func(trimmed string) bool
	reason  string
}

// runtimeTrimCutset reports whether r is a rune the runtime's own trim
// (JavaScript's `String.prototype.trim()`) removes from the front of a
// string before reading its first character.
//
// # Why this is wider than " \t\r\n"
//
// The original cutset covered the four bytes #53 measured directly. A
// review found the gap that leaves open: this guard's OWN trim ran on
// bytes that a LATER step (pasteBracketed's sanitizer, terminalpath2.go)
// still had to strip for an unrelated reason — bracket-escape injection —
// and the two trim sets disagreed. `"\v!id"` is not trimmed to `"!id"` by
// " \t\r\n" alone, so the guard saw `"\v!id"` and let it through; the
// sanitizer then dropped the \v anyway and pasted `"!id"` — the exact
// shell-command hazard this guard exists to refuse, reaching the pane
// having never been refused at all.
//
// Sanitising before this guard runs (see Send's own ordering, tmux.go) closes
// that specific hole structurally: the guard now sees whatever text will
// actually be pasted, not an earlier draft of it. This cutset is kept wider
// than " \t\r\n" anyway, as a second, independent line of defence for any
// byte JavaScript's own trim() strips that this driver's sanitiser has no
// occasion to touch — a leading NBSP (U+00A0) or BOM (U+FEFF) is not a
// control byte, so pasteBracketed never removes it, but the runtime reading
// the pasted text still trims it before deciding whether the first
// character is "!". unicode.IsSpace already covers the ASCII whitespace,
// U+0085 (NEL), U+00A0 (NBSP) and the Zs (space-separator) runes JavaScript's
// own WhiteSpace/LineTerminator production names; U+FEFF (BOM) is added
// explicitly because Go's unicode tables do not classify it as space.
func runtimeTrimCutset(r rune) bool {
	// #180 M7: skip everything that renders as nothing before deciding what
	// the first visible character is — whitespace, control characters (C0
	// and C1), format characters (zero-width space, joiner, soft hyphen,
	// the byte-order mark) and the rest of Unicode's default-ignorables. A
	// runtime that drops any of them before its own "!" check would
	// otherwise see a "!" this guard never did.
	return unicode.IsSpace(r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r)
}

// nonMessagePatterns is what THIS driver's runtime — the one it actually
// drives, not any runtime a future driver might — reads as something other
// than a message.
//
// #53's own scope note: deciding a pattern list for a runtime nobody here
// drives is explicitly NOT this issue's job. "The driver that adds a
// runtime brings its own patterns." So this list holds exactly one entry,
// because exactly one hazard is established for this runtime: a message
// beginning with "!" is read as a shell command to run directly, no
// approval prompt, the same shape #53 measured elsewhere and the reason
// this seam exists at all. Nothing else is added on suspicion.
var nonMessagePatterns = []nonMessageInput{
	{
		name: "bash-mode",
		matches: func(trimmed string) bool {
			return strings.HasPrefix(trimmed, "!")
		},
		reason: "this runtime reads a message beginning with \"!\" as a shell " +
			"command to run directly, with no approval prompt — refusing rather " +
			"than delivering it (#53); resend without the leading \"!\" if a " +
			"message was intended",
	},
}

// refuseAsRuntimeSyntax reports the reason for the first declared pattern
// that text matches, if any — checked against the SAME leading-whitespace
// trim the runtime applies, per the package comment above.
//
// The caller (Send) is required to pass already-sanitised text — see this
// function's own note on runtimeTrimCutset for why relying on this trim
// alone, against text a later step will still mutate, is exactly the gap
// that made the bypass possible.
func refuseAsRuntimeSyntax(text string, humanRelay bool) (reason string, refused bool) {
	trimmed := strings.TrimLeftFunc(text, runtimeTrimCutset)
	for _, p := range nonMessagePatterns {
		if p.matches(trimmed) {
			return p.reason, true
		}
	}
	if reason, refused := refuseSlashCommand(trimmed, humanRelay); refused {
		return reason, true
	}
	return "", false
}

// sessionSlashCommands are the slash commands any caller holding the send
// grant may deliver (#180 L6): they manage the session itself — its name,
// its remote-control link — and a consumer that is not a human relay already
// sends them. Every other command is refused unless the caller relays a
// human (driver.SendOptions.HumanRelay).
//
// Since #272 the two remote-control commands are narrower than this table
// says when the driver's gate is on: refuseRemoteControlCommand then requires
// the remote-control or human-relay grant on top of send. This table still
// answers "is it a session command" (the #111 delivery-mark site below), which
// is a question about the command, not about who sent it.
var sessionSlashCommands = map[string]bool{
	"/rename":         true,
	"/rc":             true,
	"/remote-control": true,
}

// isSessionCommand reports whether trimmed (the same, already-sanitised
// text Send is about to paste — see this file's own trim discipline above)
// is one of sessionSlashCommands, without regard for who is allowed to send
// it (refuseSlashCommand already decided that before this ever runs).
//
// Used at the #111 delivery-mark write site (tmux.go): a session-management
// command like `/rename` produces no agent turn, so marking `turns` for it
// would read as "a delivery was made and nothing has completed since" —
// exactly the false work-lost signal #111 exists to prevent — the moment
// muster #222 made a `/rename` delivery a guaranteed side effect of
// every API rename rather than a rare, deliberate `/input` call.
func isSessionCommand(text string) bool {
	trimmed := strings.TrimLeftFunc(text, runtimeTrimCutset)
	name, _, _ := strings.Cut(trimmed, " ")
	if i := strings.IndexAny(name, "\n\t"); i >= 0 {
		name = name[:i]
	}
	return sessionSlashCommands[name]
}

// remoteControlCommands are the runtime's own commands that turn the
// remote-control link on or off (#272). A subset of sessionSlashCommands: they
// are deliverable by any caller holding the send grant, which is the hole the
// remote-control grant (#269) would otherwise have beside it.
var remoteControlCommands = map[string]bool{
	"/rc":             true,
	"/remote-control": true,
}

// leadingCommand returns the slash command text begins with, after the same
// leading-invisible trim the runtime applies, or "" when it does not begin
// with one. text must already be sanitised (see refuseAsRuntimeSyntax).
func leadingCommand(text string) string {
	trimmed := strings.TrimLeftFunc(text, runtimeTrimCutset)
	if !strings.HasPrefix(trimmed, "/") {
		return ""
	}
	name, _, _ := strings.Cut(trimmed, " ")
	if i := strings.IndexAny(name, "\n\t"); i >= 0 {
		name = name[:i]
	}
	return name
}

// refuseRemoteControlCommand is #272: when the gate is on, the runtime's
// remote-control commands are delivered only for a caller who holds the
// remote-control grant or relays a human (humanRelay — what a person sending
// the command at a keyboard is). Every other caller holding only `send` is
// refused, with a reason that names both grants so an operator knows what to
// add. With the gate off it never refuses: that is the behaviour before #272,
// kept for one release so a caller still typing the command through `input`
// has time to move to POST …/remote-control.
//
// text is the same already-sanitised text refuseAsRuntimeSyntax judges, for
// the same reason: a control byte the sanitiser drops must not be able to hide
// a leading "/rc" from this check.
func refuseRemoteControlCommand(text string, gate, humanRelay, remoteControl bool) (reason string, refused bool) {
	if !gate || humanRelay || remoteControl {
		return "", false
	}
	name := leadingCommand(text)
	if !remoteControlCommands[name] {
		return "", false
	}
	return "the " + name + " command turns the session's remote control on or off, which makes it " +
		"drivable from off the machine — an exposure change, not a message. This caller holds neither " +
		"the remote-control grant nor the human-relay grant, so it is refused rather than delivered " +
		"(#272). Use POST …/remote-control with the remote-control grant", true
}

// refuseSlashCommand is #180 L6: a message beginning with "/" is read by
// this runtime as a command — /clear, /exit, or a custom command whose
// template runs whatever it says — not delivered as a message. trimmed has
// already been through the same trim the "!" guard uses.
func refuseSlashCommand(trimmed string, humanRelay bool) (reason string, refused bool) {
	if !strings.HasPrefix(trimmed, "/") || humanRelay {
		return "", false
	}
	name, _, _ := strings.Cut(trimmed, " ")
	if i := strings.IndexAny(name, "\n\t"); i >= 0 {
		name = name[:i]
	}
	if sessionSlashCommands[name] {
		return "", false
	}
	return "this runtime reads a message beginning with \"/\" as a command (" + name + "), not " +
		"as a message — refusing rather than delivering it (#180). Only the session-management " +
		"commands /rename, /rc and /remote-control are delivered for any caller; others need a " +
		"caller holding the human-relay grant. Rephrase so the message does not begin with \"/\" " +
		"if a message was intended", true
}
