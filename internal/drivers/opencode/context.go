package opencode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
)

// A session's context (muster #371).
//
// The terminal driver honours SessionSpec.ContextRef by handing the path to the
// agent's own CLI as an appended system prompt (§5.3): the file's text becomes
// system context for the whole session, not a turn the user typed. This runtime
// has no such flag, but it takes system text on the prompt call itself, so the
// same meaning is kept here:
//
//   - at create the file is read once, here, and its text is kept with the
//     session;
//   - that text is sent as the `system` field of EVERY prompt this driver sends
//     for the session — the create's own opening prompt and every later Send. It
//     has to be every one: measured on the runtime this driver is built for
//     (1.18.3), `system` applies to the one prompt that carries it and is not
//     sticky, so a follow-up sent without it runs without the context;
//   - the user part of the prompt carries only the prompt's own text, so the
//     context is never shown to the session, or to anyone reading its history, as
//     something a person said.
//
// What is stored is the PATH (the session record), never the text: a restart
// reads the file again (resume.go), so the context is what the file holds when
// the service starts, and a file that has gone is a session listed `unknown`
// with the reason rather than one quietly relaunched without its brief.
//
// The file is read by this service with its own access, exactly as a prompt
// handed to the create is — the caller names what its session is told. It is
// not read by the session, so a sandbox around the session's process neither
// applies to it nor widens what the session itself may open.

// maxContextBytes bounds a context file. It is the ceiling any single input has
// at the HTTP layer (a configured maxInputBytes is strictly below it), so a
// context is never allowed to be larger than the largest prompt could be, and the
// whole text rides on every prompt the session is sent.
const maxContextBytes = 1 << 20

// readContext validates a create's contextRef and returns the text it holds. An
// empty ref is no context. Every refusal is `invalid` and happens before any
// process is started; none of them quotes the file's content.
func (d *Driver) readContext(ref fleet.AbsolutePath) (string, error) {
	if ref == "" {
		return "", nil
	}
	invalid := func(format string, args ...any) error {
		return &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + fmt.Sprintf(format, args...), Machine: d.machine}
	}
	path := string(ref)
	if !filepath.IsAbs(path) {
		return "", invalid("contextRef must be absolute, got %q", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", invalid("contextRef %q cannot be read: %s", path, describeFileError(err))
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", invalid("contextRef %q cannot be read: %s", path, describeFileError(err))
	}
	if !info.Mode().IsRegular() {
		return "", invalid("contextRef %q is not a regular file", path)
	}
	// One byte past the cap: a file that grows between the stat and the read, or
	// one the stat under-reports, is still caught, and nothing larger than the cap
	// is ever held in memory.
	raw, err := io.ReadAll(io.LimitReader(f, maxContextBytes+1))
	if err != nil {
		return "", invalid("contextRef %q cannot be read: %s", path, describeFileError(err))
	}
	if len(raw) > maxContextBytes {
		return "", invalid("contextRef %q is larger than the %d bytes a context may be", path, maxContextBytes)
	}
	// The text travels as a JSON string. Bytes that are not UTF-8 would be
	// rewritten on the way, so what the session was told would not be what the
	// file holds — refused instead.
	if !utf8.Valid(raw) {
		return "", invalid("contextRef %q is not UTF-8 text", path)
	}
	return string(raw), nil
}

// describeFileError is the operating system's reason without the path it
// repeats from the caller's own message.
func describeFileError(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}
