package tmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/futurelastic/muster/internal/compat"
)

// This file is the part of `muster compat` that needs no session: who
// the candidate is, and which of the runtime's own words it still contains.

// compatVersionTimeout bounds `<candidate> --version`.
const compatVersionTimeout = 15 * time.Second

// resolveCandidate turns the caller's path into the path the candidate is
// launched by. It must be absolute — a relative path would resolve against the
// current directory and a bare name would be looked up on PATH, either of which
// can put a different build under test than the one named — and it is resolved
// through symlinks so a link swapped mid-run cannot change the binary.
func resolveCandidate(path string) (string, error) {
	if path == "" {
		return "", compatUsagef("--claude is required")
	}
	if !filepath.IsAbs(path) {
		return "", compatUsagef("--claude must be an absolute path (got %q): a relative path or a bare name would not name one definite build", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", compatUsagef("--claude %q: %v", path, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", compatUsagef("--claude %q: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", compatUsagef("--claude %q is not a regular file", path)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return "", compatUsagef("--claude %q is not executable", path)
	}
	return resolved, nil
}

// describeCandidate identifies the binary: resolved path, digest, architecture
// and the version it reports for itself. It fills in as much as it can before
// returning an error, so a report for an unidentifiable candidate still says
// what was learned.
func describeCandidate(ctx context.Context, given, resolved string, getenv func(string) string) (compat.Candidate, error) {
	c := compat.Candidate{Path: given, Resolved: resolved}
	sum, err := fileSHA256(ctx, resolved)
	if err != nil {
		return c, fmt.Errorf("digest: %w", err)
	}
	c.Sha256 = sum
	c.Arch = candidateArch(resolved)
	v, err := candidateVersion(ctx, resolved, getenv)
	if err != nil {
		return c, err
	}
	c.Version = v
	return c, nil
}

func fileSHA256(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := f.Read(buf)
		h.Write(buf[:n])
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// candidateArch names the architecture in the binary's own header, so the
// report says which build of the file was tested: a verdict for one
// architecture says nothing about another. A universal binary lists every
// slice.
func candidateArch(path string) string {
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		return machoCPU(f.Cpu)
	}
	if ff, err := macho.OpenFat(path); err == nil {
		defer ff.Close()
		var names []string
		for _, a := range ff.Arches {
			names = append(names, machoCPU(a.Cpu))
		}
		return strings.Join(names, "+")
	}
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		switch f.Machine {
		case elf.EM_X86_64:
			return "x86_64"
		case elf.EM_AARCH64:
			return "arm64"
		case elf.EM_386:
			return "i386"
		case elf.EM_ARM:
			return "arm"
		}
		return strings.ToLower(f.Machine.String())
	}
	return "unknown"
}

func machoCPU(c macho.Cpu) string {
	switch c {
	case macho.CpuAmd64:
		return "x86_64"
	case macho.CpuArm64:
		return "arm64"
	case macho.Cpu386:
		return "i386"
	case macho.CpuArm:
		return "arm"
	}
	return fmt.Sprintf("cpu-%d", uint32(c))
}

// compatBaseEnv is the whole environment a candidate is given: a minimal set
// plus the two variables that stop the runtime from updating the very binary
// being checked. Nothing else is inherited — a check must not be able to pick
// up a variable the caller happened to have set.
func compatBaseEnv(getenv func(string) string) []string {
	first := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	user := first(getenv("USER"), getenv("LOGNAME"))
	env := []string{"HOME=" + home}
	if user != "" {
		env = append(env, "USER="+user, "LOGNAME="+user)
	}
	env = append(env,
		"SHELL="+first(getenv("SHELL"), "/bin/sh"),
		"LANG="+first(getenv("LANG"), "en_US.UTF-8"),
		"TMPDIR="+first(getenv("TMPDIR"), os.TempDir()),
		"TERM=xterm-256color",
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"DISABLE_UPDATES=1",
		"DISABLE_AUTOUPDATER=1",
	)
	return env
}

// candidateVersion runs `<candidate> --version` under the minimal environment.
func candidateVersion(ctx context.Context, resolved string, getenv func(string) string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, compatVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "--version")
	cmd.Env = compatBaseEnv(getenv)
	cmd.Dir = os.TempDir()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("--version did not finish within %s", compatVersionTimeout)
		}
		return "", fmt.Errorf("--version: %w", err)
	}
	v := parseVersionLine(out.String())
	if v == "" {
		return "", errors.New("--version printed nothing")
	}
	return v, nil
}

// parseVersionLine takes the first token of the first non-empty line: the
// runtime prints "<version> (<product name>)".
func parseVersionLine(s string) string {
	line := trimLine(s)
	if line == "" {
		return ""
	}
	return strings.Fields(line)[0]
}

// compatStaticMarkers are the runtime's own words that this driver's screen
// classifiers read. A screen for quota and API-error states cannot be
// produced on demand, so all this can assert is that the WORDING is still in
// the candidate. A missing marker means the classifier is probably reading
// text the runtime no longer prints.
//
// Every marker is lower-case ASCII and is searched case-insensitively, and
// every one was measured present in a known-good build before being required.
// A unit test runs each through the classifier function that relies on it, so
// this vocabulary cannot drift from what the classifier actually matches.
var compatStaticMarkers = map[string][]string{
	// usageLimit: `limit` with `hit your`/`reached`/`usage`, or
	// `/usage-credits`; resetHintIn reads `resets `, `try again `,
	// `available again `.
	"F-LIMIT": {"hit your", "/usage-credits", "resets ", "available again "},
	// lastTurnFailed: `api error`; retryableWords: `temporary`, `try again`.
	"F-APIERR": {"api error", "try again ", "temporary"},
	// controlStateIn: the label `/rc` followed by one of four state words.
	"H-RC": {"/rc active", "/rc failed", "/rc reconnecting", "/rc connecting"},
	// classifyControlRecordLine (#270): the wording of the record entries the
	// control-channel reader keys on. Each was measured present in the runtime
	// binary the reader was built against.
	"H-RCREC": {"/remote-control is active", "remote control disconnected", "bridge_status"},
	// permissionModeLabels: the wording of the three indicator rows that exist
	// as literal strings in the candidate. The other two — the default mode's
	// `manual mode on` and `bypass permissions on` — are composed at runtime
	// and are not searchable; F-MODE reads those two off live sessions.
	"F-MODE": {"accept edits on", "plan mode on", "auto mode on"},
	// parseFeedbackStatus, feedbackQuestionRow and the panel's title:
	// the runtime's feedback-draft card in the states a person answers at the
	// terminal (muster#217). The key row's own words are composed at
	// runtime ("1", " to ", "review") and are not searchable; the rest are
	// whole literals, each measured present in a known-good build. No probe can
	// make an agent draft feedback, so this is wording only.
	"F-FEEDBACK": {
		"send without reviewing", "(full draft + env, no transcript)", "review & retry",
		"couldn't send feedback", "turn off claude-drafted feedback?", " more queued",
		"feedback drafts",
	},
	// acceptanceScreen: two options, one containing `accept`, one `exit` or `no,`.
	// F-BYPASS reads these only when the screen itself cannot be produced.
	"F-BYPASS": {"yes, i accept", "no, exit"},
}

// staticMarkerList is every marker once, sorted.
func staticMarkerList() []string {
	seen := map[string]bool{}
	var out []string
	for _, ms := range compatStaticMarkers {
		for _, m := range ms {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// scanMarkers reports which of markers occur in the file, ignoring ASCII case.
// It streams the file in chunks with an overlap of one marker length, so a
// marker straddling a chunk boundary is still found and a 200 MB executable
// costs a few megabytes of memory. Presence is all that is reported, so the
// overlap counting a marker twice is harmless.
//
// Only UTF-8/ASCII occurrences are searched: the classifier's vocabulary is
// ASCII, and the runtime carries its own message text that way.
func scanMarkers(ctx context.Context, path string, markers []string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	found := map[string]bool{}
	maxLen := 0
	for _, m := range markers {
		if len(m) > maxLen {
			maxLen = len(m)
		}
	}
	if maxLen == 0 {
		return found, nil
	}
	const chunk = 4 << 20
	buf := make([]byte, chunk+maxLen)
	fold := make([]byte, chunk+maxLen)
	keep := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, rerr := io.ReadFull(f, buf[keep:keep+chunk])
		total := keep + n
		if n > 0 {
			for i := 0; i < total; i++ {
				b := buf[i]
				if 'A' <= b && b <= 'Z' {
					b += 'a' - 'A'
				}
				fold[i] = b
			}
			for _, m := range markers {
				if !found[m] && bytes.Contains(fold[:total], []byte(m)) {
					found[m] = true
				}
			}
			if len(found) == len(markers) {
				return found, nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				return found, nil
			}
			return nil, rerr
		}
		keep = maxLen - 1
		if keep > total {
			keep = total
		}
		copy(buf, buf[total-keep:total])
	}
}

// addStatic registers the probe that scans the candidate once, and the three
// checks that read its result.
func (h *compatHarness) addStatic(s *compat.Suite) {
	const probe = "static.markers"
	s.Probes = append(s.Probes, compat.Probe{
		ID: probe, Stage: 0, Budget: 3 * time.Minute,
		Run: func(ctx context.Context) error {
			found, err := scanMarkers(ctx, h.cand.Resolved, staticMarkerList())
			if err != nil {
				return fmt.Errorf("scanning %s: %w", filepath.Base(h.cand.Resolved), err)
			}
			h.markers = found
			return nil
		},
	})
	for _, id := range []string{"F-LIMIT", "F-APIERR", "H-RC", "H-RCREC", "F-FEEDBACK"} {
		id := id
		s.Checks = append(s.Checks, compat.Check{
			ID: id, Probes: []string{probe},
			Eval: func() compat.Verdict {
				var missing []string
				for _, m := range compatStaticMarkers[id] {
					if !h.markers[m] {
						missing = append(missing, fmt.Sprintf("%q", m))
					}
				}
				if len(missing) > 0 {
					return compat.Failed("the candidate no longer contains " + strings.Join(missing, ", ") +
						": the classifier may be reading wording the runtime no longer prints")
				}
				return compat.Passed(fmt.Sprintf("all %d marker strings are present", len(compatStaticMarkers[id])))
			},
		})
	}
}
