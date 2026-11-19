package opencode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/privfs"
)

// Operator instruction files (muster #504).
//
// An isolated session has its own HOME and XDG directories (env.go), so it reads
// no instruction file the service user keeps (not ~/.config/opencode/AGENTS.md,
// not a rules folder), and follows only the project's own AGENTS.md. Claude
// sessions on the same machine get the operator's rules; an opencode session did
// not. WithOperatorInstructions names the files that close that gap. They work
// like the operator's agents (operatoragents.go, #396):
//
//   - The PATHS are fixed at start. What the files hold is read again on EVERY
//     create and every relaunch, so editing the machine's rules needs no restart.
//   - A file that cannot be used REFUSES the create, with the reason. It is never
//     skipped: a session that was meant to run under the operator's rules and
//     silently ran without them is the failure this exists to prevent. That is
//     why the driver reads the files itself instead of handing the runtime a
//     path: the runtime skips an instruction file it cannot find, silently.
//   - It reaches the runtime the way the agents and the provider policy do:
//     through the session's OWN opencode.json (writeSessionConfig), under the
//     runtime's own `instructions` key, so the runtime treats it as any other
//     instruction file.
//
// # Why a copy in the session's directory, not the operator's path
//
// The runtime is given a copy of each file, taken at the moment the session starts
// and kept in that session's own directory, not the operator's path:
//
//   - "A running session keeps what it started with" is then true by
//     construction. A path would be read by the runtime again on its own schedule,
//     so an edit would reach a session in the middle of its work, and a file
//     rewritten non-atomically could reach it half-written. The agents behave the
//     same way, because their text is written into the session's configuration.
//   - A confined session (sandbox.go) reads only the directories the sandbox
//     opens. The operator's file lives wherever the operator keeps it, and the
//     runtime's own read of a path it may not open is, again, silent.
//   - The refusal above and what the session sees are then one read, not two that
//     can disagree.
//
// The copy is owner-only (privfs) like everything else in the directory, and its
// name is built from the file's own name with every character a pattern could
// mean replaced: the runtime treats an instruction entry as a glob, so a file
// called `rules[1].md` must not be asked for as a pattern. The copy's name shows
// to the model in the runtime's own "Instructions from:" heading and carries
// nothing about the operator's directories.
//
// # What it does not do
//
// Instruction files only. No tool server, permission or provider rides this
// setting (the #396 exclusion stays as it is; tool servers are #466's), and the
// text is never logged or put in an error: a refusal says which file and what is
// wrong with it, and nothing of what it holds. The text is visible to the session,
// like any instruction file: it is not a secret channel.

// operatorInstructionsMaxBytes bounds one file. A rules file is prose a model will
// read on every turn; a megabyte is far past any real one and keeps a wrong path (a
// log, a database) from being read into memory.
const operatorInstructionsMaxBytes = 1 << 20

// operatorInstructionsDir is where a session's copies live, under its own directory.
const operatorInstructionsDir = "instructions"

// operatorInstruction is one file as it was read for one session start.
type operatorInstruction struct {
	source string // the operator's path, for a message
	text   []byte
}

// readOperatorInstruction reads and checks one file. The error says what is wrong
// with it in words an operator can act on; it never quotes the file's content.
func readOperatorInstruction(path string) (operatorInstruction, error) {
	info, err := os.Stat(path) // follows a link: the operator may keep one
	if err != nil {
		return operatorInstruction{}, fmt.Errorf("the operator's instruction file cannot be read: %w", err)
	}
	// Before opening: opening a named pipe blocks until something writes to it.
	if !info.Mode().IsRegular() {
		kind := "not a regular file"
		if info.IsDir() {
			kind = "a directory, not a file"
		}
		return operatorInstruction{}, fmt.Errorf("the operator's instruction file %q is %s", path, kind)
	}
	f, err := os.Open(path)
	if err != nil {
		return operatorInstruction{}, fmt.Errorf("the operator's instruction file cannot be read: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, operatorInstructionsMaxBytes+1))
	if err != nil {
		return operatorInstruction{}, fmt.Errorf("the operator's instruction file cannot be read: %w", err)
	}
	switch {
	case len(raw) > operatorInstructionsMaxBytes:
		return operatorInstruction{}, fmt.Errorf("the operator's instruction file %q is larger than %d bytes", path, operatorInstructionsMaxBytes)
	case len(strings.TrimSpace(string(raw))) == 0:
		// Nothing to follow is a file that was truncated or never filled in,
		// which is a session that would run without its rules.
		return operatorInstruction{}, fmt.Errorf("the operator's instruction file %q is empty", path)
	case !utf8.Valid(raw):
		return operatorInstruction{}, fmt.Errorf("the operator's instruction file %q is not UTF-8 text", path)
	}
	return operatorInstruction{source: path, text: raw}, nil
}

// operatorInstructionsFor reads this machine's instruction files for one session
// start. Called before any side effect, at create and again at relaunch. Nil when
// none are configured.
func (d *Driver) operatorInstructionsFor() ([]operatorInstruction, error) {
	if len(d.instructionFiles) == 0 {
		return nil, nil
	}
	out := make([]operatorInstruction, 0, len(d.instructionFiles))
	for _, path := range d.instructionFiles {
		in, err := readOperatorInstruction(path)
		if err != nil {
			// The operator's file is at fault, not the request: retrying the same
			// create cannot help until it is repaired (the same kind the operator's
			// agent file answers with).
			return nil, &fleet.Error{
				Kind: fleet.ErrorUnsupported,
				Message: "create: " + err.Error() + " (the machine's opencode instruction files); " +
					"an operator must repair that, and retrying this create will not help until they do",
				Machine: d.machine,
			}
		}
		out = append(out, in)
	}
	return out, nil
}

// copyName is the name a copy is given: its position, so two files with one name
// stay apart and the operator's order is the order the runtime reads them in,
// then the source's own name with anything outside a plain filename alphabet
// replaced. It is never longer than a name a filesystem takes.
func copyName(index int, source string) string {
	base := filepath.Base(source)
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > 64 {
		name = name[:64]
	}
	name = strings.TrimLeft(name, ".") // never a hidden file, never "." or ".."
	if name == "" {
		name = "instructions"
	}
	return fmt.Sprintf("%02d-%s", index+1, name)
}

// writeOperatorInstructions puts the copies in dir's own instructions directory
// and returns their absolute paths, in the operator's order, for the session's
// configuration. A relaunch passes the directory the previous process used, so the
// directory is emptied first: the copies are exactly this start's reading, and a
// file the operator has since dropped from the setting is not left behind.
func writeOperatorInstructions(dir string, files []operatorInstruction) ([]string, error) {
	target := filepath.Join(dir, operatorInstructionsDir)
	if err := privfs.RemoveAll(target); err != nil {
		return nil, fmt.Errorf("opencode: clearing the session's instruction files: %w", err)
	}
	if len(files) == 0 {
		return nil, nil
	}
	if err := privfs.MkdirAll(target); err != nil {
		return nil, fmt.Errorf("opencode: creating the session's instruction directory: %w", err)
	}
	paths := make([]string, 0, len(files))
	for i, f := range files {
		p := filepath.Join(target, copyName(i, f.source))
		if err := privfs.WriteFile(p, f.text); err != nil {
			return nil, fmt.Errorf("opencode: writing the session's instruction file: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, nil
}
