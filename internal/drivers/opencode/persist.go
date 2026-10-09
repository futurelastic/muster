package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	fleet "github.com/futurelastic/muster"
)

// The per-session record (muster #282).
//
// A service restart used to drop every session from listings although the
// runtime's own store still had them. What lets this driver find a session
// again is small and none of it is secret: the runtime's session id, where the
// session works, the directory holding its database, the sandbox it was asked
// for, and the NAMES of the environment variables its create carried.
//
// What is never written, by construction rather than by care: any environment
// value, and the server credential. The record type below has nowhere to put
// either, and the test that creates a session with a recognisable value greps
// the file for it.
//
// # What a restart can and cannot give back
//
// The session's database lives in its own scoped directory (env.go), which is
// why that directory moves under the state directory and survives the service:
// a relaunched process reads the same store, and a by-id read confirms it. The
// process is new — new port, new credential — and its environment is rebuilt
// from the machine's sessionEnv files, which are read fresh exactly as at create.
// The one thing that cannot be rebuilt is a value the CALLER supplied: this
// driver never kept it, so a session created with one is listed `unknown`, with
// the reason, rather than relaunched with less environment than it was given.

// recordVersion is the format of the record; a file with another is not
// understood and is reported rather than guessed at.
const recordVersion = 1

type sessionRecord struct {
	Version int `json:"version"`
	// ID is the runtime's own session id.
	ID string `json:"id"`
	// Cwd is the working directory the session was created in.
	Cwd string `json:"cwd"`
	// Name and Agent are the title and agent the runtime reported at create.
	Name  string `json:"name,omitempty"`
	Agent string `json:"agent,omitempty"`
	// SpecAgent and SpecMarker are what the create asked for, kept because a
	// sessionEnv entry is scoped by them: rebuilding the environment needs the
	// same answers the create got.
	SpecAgent  string `json:"specAgent,omitempty"`
	SpecMarker string `json:"specMarker,omitempty"`
	// StartedAtMs is the runtime's own creation time, which Close corroborates.
	StartedAtMs int64 `json:"startedAtMs"`
	// Dir is the session's scoped directory (HOME, TMPDIR, XDG_*).
	Dir string `json:"dir"`
	// EnvNames are the NAMES of the variables the create carried. Never values.
	EnvNames []string `json:"envNames,omitempty"`
	// Sandbox is the profile the create asked for: paths and a network posture.
	Sandbox *fleet.SandboxSpec `json:"sandbox,omitempty"`
	// CacheCopy says how a private package cache in Dir was made, so a relaunch
	// reuses it instead of copying again.
	CacheCopy string `json:"cacheCopy,omitempty"`
	// Pid, Port and Bin identify the last server process, so a restart can tell
	// a survivor of the previous service from a stranger before relaunching.
	Pid  int    `json:"pid,omitempty"`
	Port int    `json:"port,omitempty"`
	Bin  string `json:"bin,omitempty"`
}

// recreateReason is what a session created with caller-supplied env is listed
// with after a restart.
func (r *sessionRecord) recreateReason() string {
	if len(r.EnvNames) == 0 {
		return ""
	}
	return fmt.Sprintf("the session was created with caller-supplied environment (%s), which this driver never "+
		"keeps, so it cannot be relaunched with the environment it was created with; it needs re-creation",
		strings.Join(r.EnvNames, ", "))
}

// recordStore is the directory of records.
type recordStore struct{ dir string }

func (s *recordStore) path(id string) string { return filepath.Join(s.dir, id+".json") }

// safeID reports whether id can be a file name: the runtime's ids are
// alphanumeric, and anything else is refused rather than escaped.
func safeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// write stores r atomically, readable by the service user alone.
func (s *recordStore) write(r *sessionRecord) error {
	if !safeID(r.ID) {
		return fmt.Errorf("opencode: the runtime returned a session id (%q) that cannot be stored", r.ID)
	}
	r.Version = recordVersion
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("opencode: creating the record directory: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".rec-*")
	if err != nil {
		return fmt.Errorf("opencode: writing the session record: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o600)
	}
	if werr == nil {
		werr = os.Rename(name, s.path(r.ID))
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("opencode: writing the session record: %w", werr)
	}
	return nil
}

// remove deletes the record of id; a record already gone is not an error.
func (s *recordStore) remove(id string) error {
	if !safeID(id) {
		return nil
	}
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// loaded is one record as read back; err is set when the file could not be
// understood, so the session is still reported (as unknown) rather than lost.
type loaded struct {
	rec  *sessionRecord
	file string
	err  error
}

// loadAll reads every record, in a stable order.
func (s *recordStore) loadAll() []loaded {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []loaded
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		file := filepath.Join(s.dir, name)
		raw, err := os.ReadFile(file)
		if err != nil {
			out = append(out, loaded{file: file, err: err})
			continue
		}
		var r sessionRecord
		switch {
		case json.Unmarshal(raw, &r) != nil:
			out = append(out, loaded{file: file, err: errors.New("the record is not valid JSON")})
		case r.Version != recordVersion:
			out = append(out, loaded{file: file, err: fmt.Errorf("the record has version %d, which this build does not read", r.Version)})
		case !safeID(r.ID) || r.ID+".json" != name:
			out = append(out, loaded{file: file, err: errors.New("the record's id does not match its file")})
		default:
			out = append(out, loaded{rec: &r, file: file})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].file < out[j].file })
	return out
}

// envNames returns the sorted names of env, never its values.
func envNames(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reapStale stops a server the PREVIOUS service left running, so a relaunch
// does not put two processes on one session database. A recorded pid is only
// acted on when the process answering to it is demonstrably the same server —
// the recorded binary serving the recorded port — because a pid alone may now
// belong to anything. Returns whether it stopped one.
func reapStale(r *sessionRecord) bool {
	if r.Pid <= 1 || r.Port == 0 {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(r.Pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	cmdline := string(out)
	if !strings.Contains(cmdline, " serve ") || !strings.Contains(cmdline, "--port "+strconv.Itoa(r.Port)) {
		return false
	}
	if r.Bin != "" && !strings.Contains(cmdline, r.Bin) {
		return false
	}
	_ = syscall.Kill(-r.Pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(r.Pid, 0) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-r.Pid, syscall.SIGKILL)
	return true
}
