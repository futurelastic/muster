// Package agentdef is what the drivers share when they translate a portable agent
// definition (fleet.AgentSpec, muster #509) into their runtime's own form: reading
// the knowledge index a definition names, and the note that tells the agent where
// its memory is. The translation itself — a plugin directory, a session
// configuration entry — is each driver's own.
//
// It lives in its own package, not in a driver, for the reason internal/sessionenv
// does: more than one driver reads the index, and "how it is read, and when a read
// refuses a start" must be one answer, not two that drift.
package agentdef

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	fleet "github.com/futurelastic/muster"
)

// ReadIndex reads the knowledge index a definition names, live from the file. The
// error says what is wrong in words a caller can act on; it never quotes the
// file's content.
//
// An empty file is allowed: an agent whose memory has just been made has an index
// with nothing in it yet. A file that is not a regular file, is larger than
// fleet.AgentKnowledgeMaxIndexBytes, or is not UTF-8 text refuses, because a
// session that silently read half of its memory, or garbage, would look informed
// and not be.
func ReadIndex(path fleet.AbsolutePath) ([]byte, error) {
	p := string(path)
	info, err := os.Stat(p) // follows a link: a client may keep one
	if err != nil {
		return nil, fmt.Errorf("agentSpec.knowledge.index cannot be read: %w", err)
	}
	// Before opening: opening a named pipe blocks until something writes to it.
	if !info.Mode().IsRegular() {
		kind := "not a regular file"
		if info.IsDir() {
			kind = "a directory, not a file"
		}
		return nil, fmt.Errorf("agentSpec.knowledge.index %q is %s", p, kind)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("agentSpec.knowledge.index cannot be read: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, fleet.AgentKnowledgeMaxIndexBytes+1))
	if err != nil {
		return nil, fmt.Errorf("agentSpec.knowledge.index cannot be read: %w", err)
	}
	switch {
	case len(raw) > fleet.AgentKnowledgeMaxIndexBytes:
		return nil, fmt.Errorf("agentSpec.knowledge.index %q is larger than %d bytes", p, fleet.AgentKnowledgeMaxIndexBytes)
	case !utf8.Valid(raw):
		return nil, fmt.Errorf("agentSpec.knowledge.index %q is not UTF-8 text", p)
	}
	return raw, nil
}

// Note is the text the agent reads about its memory: a heading, the home path and
// what the session may do there, then the index as this start read it.
func Note(k fleet.AgentKnowledge, index []byte) string {
	var b strings.Builder
	b.WriteString("# Your memory\n\n")
	fmt.Fprintf(&b, "Your memory home is `%s`. ", string(k.Home))
	if k.Write {
		b.WriteString("This session may read and write files there.")
	} else {
		b.WriteString("This session may read files there.")
	}
	b.WriteString(" Its index, as it stood when this session started, follows.\n\n---\n\n")
	b.Write(index)
	if len(index) > 0 && index[len(index)-1] != '\n' {
		b.WriteByte('\n')
	}
	return b.String()
}
