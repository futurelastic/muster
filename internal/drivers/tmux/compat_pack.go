package tmux

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/futurelastic/muster/internal/compat"
)

// compatPack is the directory `--pack` writes: the raw evidence each check
// produced, so a tool outside this repository can replay its own classifiers
// over the same states without driving the candidate again. What those tools
// are is none of this repository's business.
//
// It is local data, created 0700. Captures that reach it pass through
// RedactCapture first (see the step that adds them); nothing here is meant to
// leave the machine unread.
type compatPack struct{ dir string }

// newCompatPack creates (or reuses) the directory. It refuses one that already
// holds anything else: mixing a run's evidence into a directory that has other
// contents is how a pack ends up describing two different candidates.
func newCompatPack(dir string) (*compatPack, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, compatUsagef("--pack %q: %v", dir, err)
	}
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		return nil, compatUsagef("--pack %q already has contents; give a new or empty directory", dir)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, compatUsagef("--pack %q: %v", dir, err)
	}
	return &compatPack{dir: abs}, nil
}

// writeReport stores the finished report alongside the evidence.
func (p *compatPack) writeReport(r compat.Report) error {
	f, err := os.OpenFile(filepath.Join(p.dir, "report.json"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing the report into the pack: %w", err)
	}
	defer f.Close()
	return compat.WriteJSON(f, r)
}

// packState writes one named screen state: the pane as the driver captured it,
// with and without its escape sequences, both through RedactCapture, and the few
// facts that were true of the pane when it was taken. RedactCapture keeps only
// fixed runtime vocabulary and structure and discards everything else, so a
// pane's prose — a path, a name, a reply — never reaches the pack.
func (p *compatPack) packState(name string, s compatShot) (string, error) {
	if !s.ok {
		return "", nil
	}
	dir := filepath.Join(p.dir, "states", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	facts, err := json.MarshalIndent(map[string]any{
		"width":            compatPaneCols,
		"height":           compatPaneRows,
		"bracketPasteFlag": s.bracket,
		"status":           string(s.state.Status),
	}, "", "  ")
	if err != nil {
		return "", err
	}
	for file, content := range map[string]string{
		"pane.txt":      RedactCapture(s.plain()),
		"pane.ansi.txt": RedactCapture(s.ansi()),
		"pane.json":     string(facts) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
			return "", err
		}
	}
	return filepath.Join("states", name), nil
}

// packTranscriptTypes are the transcript entries worth keeping: the ones the
// checks read. Everything else the runtime records (attachments, snapshots)
// can carry environment content and is dropped.
var packTranscriptTypes = map[string]bool{
	"user": true, "assistant": true, "queue-operation": true, "custom-title": true, "system": true,
}

// writeEvidence stores what the checks were judged on. The manifest lists every
// file, so a reader never has to guess what a pack holds.
func (p *compatPack) writeEvidence(h *compatHarness) error {
	var files []string
	add := func(rel string, err error) error {
		if err != nil {
			return err
		}
		if rel != "" {
			files = append(files, rel)
		}
		return nil
	}
	home := ""
	if h.world != nil {
		home = h.world.store.home
	}
	scrub := func(s string) string {
		if home != "" {
			s = strings.ReplaceAll(s, home, "~")
		}
		if h.world != nil {
			s = strings.ReplaceAll(s, h.world.scratch, "<scratch>")
		}
		return s
	}

	for name, b := range map[string]compatBoot{
		"boot": h.ev.a, "trust-dialog": h.ev.u, "imports-dialog": h.ev.i, "boot-bypass": h.ev.b, "bypass-attempt": h.ev.c, "shell-mode": h.ev.bang,
		"configuration-given": h.ev.g.boot, "configuration-control": h.ev.gc.boot,
	} {
		rel, err := p.packState(name, b.shot)
		if e := add(rel, err); e != nil {
			return e
		}
	}
	for name, d := range h.ev.drafts {
		rel, err := p.packState("draft-"+name, d.shot)
		if e := add(rel, err); e != nil {
			return e
		}
	}

	if h.ev.a.rec != nil {
		dir := filepath.Join(p.dir, "sessions", "a")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte(scrub(string(h.ev.a.rec))), 0o600); err != nil {
			return err
		}
		files = append(files, filepath.Join("sessions", "a", "record.json"))

		var lines []string
		for _, n := range []string{"warm", "short", "long", "multi", "control"} {
			sd, ok := h.ev.sends[n]
			if !ok {
				continue
			}
			for _, e := range sd.entries {
				if t, _ := e["type"].(string); !packTranscriptTypes[t] {
					continue
				}
				if b, err := json.Marshal(e); err == nil {
					lines = append(lines, scrub(string(b)))
				}
			}
		}
		if len(lines) > 0 {
			if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				return err
			}
			files = append(files, filepath.Join("sessions", "a", "transcript.jsonl"))
		}
	}

	sort.Strings(files)
	manifest, err := json.MarshalIndent(map[string]any{
		"schema": compat.Schema,
		"note":   "Local evidence for one compat run. Pane captures pass through RedactCapture; the record and transcript have the home directory and scratch path rewritten, and only the entries the checks read are kept.",
		"files":  files,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.dir, "manifest.json"), append(manifest, '\n'), 0o600)
}
