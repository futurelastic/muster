package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/compat"
	"github.com/futurelastic/muster/internal/driver"
)

// # Probes and checks
//
// A probe drives a shared session into a state and records what it saw; a check
// judges that record. One booted session therefore serves every check that reads
// it, and the expensive part — booting a candidate, spending a model turn — is
// paid once. A probe returns an error only when the ENVIRONMENT failed. What the
// candidate did is evidence, however surprising, so the check that reads it can
// say "the candidate behaves differently" instead of "could not run".

// compatShot is one look at a session: the driver's own read of its state, and
// the screen that read came from. The screen is captured the way the driver
// captures it for delivery confirmation, so a check sees what the driver sees.
type compatShot struct {
	ok       bool
	state    fleet.SessionState
	stateErr error
	sc       screen
	bracket  string // #{bracket_paste_flag}
	composer string // composerText
	scan     composerScan
}

// prompt is the question on the screen, read from THIS shot's own capture and
// never from the driver's separate State call. Two captures a moment apart can
// catch a dialog at different stages of painting; evidence taken from one and a
// wait condition taken from the other would disagree with itself.
func (s compatShot) prompt() (p *fleet.SessionPrompt, unnumbered bool) {
	p, unnumbered = parsePromptMenu(s.sc)
	if p == nil || len(p.Options) == 0 {
		return nil, false
	}
	if p.Kind == "" {
		p.Kind = classifyPromptKind(p)
	}
	return p, unnumbered
}

// plain and ansi are the screen without and with its escape sequences.
func (s compatShot) plain() string { return strings.Join(s.sc.lines, "\n") }
func (s compatShot) ansi() string  { return strings.Join(s.sc.raw, "\n") }

// paneOf finds a session's pane on the private server.
func (w *compatWorld) paneOf(ctx context.Context, ref fleet.SessionRef) (paneRow, bool) {
	rows, _, err := w.d.enumerate(ctx)
	if err != nil {
		return paneRow{}, false
	}
	for _, r := range rows {
		if r.session == ref.ID {
			return r, true
		}
	}
	return paneRow{}, false
}

// shoot looks at a session now.
func (w *compatWorld) shoot(ctx context.Context, ref fleet.SessionRef) compatShot {
	var s compatShot
	row, ok := w.paneOf(ctx, ref)
	if !ok {
		return s
	}
	// The state is read first and the screen last, so the screen every check
	// judges is the newest look at the pane.
	s.state, s.stateErr = w.d.State(ctx, compatRequest, ref)
	if out, err := w.tmux(ctx, "display-message", "-p", "-t", row.paneID, "#{bracket_paste_flag}"); err == nil {
		s.bracket = strings.TrimSpace(out)
	}
	sc, ok := w.d.captureForClassify(ctx, row.paneID)
	if !ok {
		return s
	}
	s.ok, s.sc = true, sc
	s.composer, s.scan = composerText(sc)
	return s
}

// waitShot polls until ok(shot) holds or the time is up, and returns the last
// shot either way: a candidate that never gets there is evidence, not an error.
func (w *compatWorld) waitShot(ctx context.Context, ref fleet.SessionRef, d time.Duration, ok func(compatShot) bool) (compatShot, bool) {
	deadline := time.Now().Add(d)
	var s compatShot
	for {
		s = w.shoot(ctx, ref)
		if s.ok && ok(s) {
			return s, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return s, false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// compatReady means the driver can see a composer. That is all "ready" claims:
// whether the pane also accepts a bracketed paste is a separate fact, and
// conflating the two would report a candidate that switched bracketed paste off
// as one with no composer at all. See awaitBracket.
func compatReady(s compatShot) bool { return s.scan == composerFound }

// awaitBracket gives a pane that has a composer a short time to ask for
// bracketed paste. The driver refuses to paste before it does — measured: a
// paste into a session about a second old fails — so drafting waits for it. A
// candidate that never asks is not an error: the flag is recorded as it is, and
// the paste that follows fails with the driver's own explanation.
func (h *compatHarness) awaitBracket(ctx context.Context, b *compatBoot) {
	if !b.ready || b.shot.bracket == "1" {
		return
	}
	// A multiplexer older than 3.7 never answers, so there is nothing to wait for
	// (#334): the paste is sent without the pre-check, and G2 judges it by what
	// the runtime did with it.
	if flagUnreadableByVersion(b.shot.bracket, h.tmuxVersionText()) {
		return
	}
	if s, ok := h.world.waitShot(ctx, b.ref, compatBracketWait, func(s compatShot) bool { return s.bracket == "1" }); ok {
		b.shot = s
	} else {
		b.shot = s
	}
}

// awaitMode gives a session that has a composer a bounded time to paint the
// permission-mode indicator, and records the look that read it (#194).
//
// It records into b.mode and leaves b.shot alone: every other check reads b.shot
// as the moment the composer appeared, and this is a later look. A candidate that
// never paints a readable indicator is not an error — the last look is recorded as
// it is and F-MODE says what the driver read.
func (h *compatHarness) awaitMode(ctx context.Context, b *compatBoot) {
	if !b.ready {
		return
	}
	s, _ := h.world.waitShot(ctx, b.ref, compatModeWait, func(s compatShot) bool { return s.state.PermissionMode != "" })
	b.mode = s
}

// clearComposer discards whatever is drafted, through the driver's own Discard,
// and confirms the composer is empty. A draft that cannot be cleared would
// contaminate every later probe, so this is an environment error.
func (w *compatWorld) clearComposer(ctx context.Context, ref fleet.SessionRef) error {
	st, err := w.d.State(ctx, compatRequest, ref)
	if err != nil {
		return err
	}
	if _, err := w.d.Discard(ctx, compatRequest, ref, st.ComposerDigest, driver.DiscardOptions{}); err != nil {
		return fmt.Errorf("discard: %w", err)
	}
	if s, ok := w.waitShot(ctx, ref, 4*time.Second, func(s compatShot) bool { return s.scan == composerFound && s.composer == "" }); !ok {
		return fmt.Errorf("the composer still reads %q after a discard", s.composer)
	}
	return nil
}

// compatBoot is what booting one session showed.
type compatBoot struct {
	ran      bool
	ref      fleet.SessionRef
	created  time.Time
	shot     compatShot
	ready    bool
	bootTime time.Duration // until ready, when it got there
	// mode is a later look, taken once the indicator row had time to paint (#194).
	mode compatShot

	// The per-process record, read for the pid the multiplexer reports.
	pid      int
	rec      []byte
	recAfter time.Duration // how long after the launch request it appeared
	identity ProcessIdentity
	identErr error
	live     liveProcessIdentity
	liveOK   bool
}

// compatDraft is what pasting a draft into the composer showed.
type compatDraft struct {
	ran bool
	// skipped says why nothing was drafted, when nothing was. A candidate that
	// never shows a composer is not an environment problem — it will do the same
	// again — so this is evidence for the checks to fail on, not a probe error
	// that would report them as "could not run".
	skipped  string
	text     string
	pasteErr error
	shot     compatShot
	markers  map[pasteKey]int
	holds    bool
	rows     []string
	rowScan  composerScan
	matches  bool
	clearErr error
}

// compatEvidence is everything the boot and draft probes recorded.
type compatEvidence struct {
	// a, b, c, u and i are the five boot sessions; i is the one that meets the
	// external-imports dialog (#212).
	a, b, c, u, i compatBoot
	// bypassSetting: the user's own setting that suppresses the acceptance
	// screen, read (never written) so a missing premise is reported as such.
	bypassSetting    string // "true", "false", "absent" or an error text
	bypassSettingErr error
	drafts           map[string]*compatDraft
	sends            map[string]*compatSend
	// bang is the session used to enter shell mode; it is never reused.
	bang compatBoot
	// dirty is set when a draft could not be cleared. Every later draft would be
	// made into a composer that is not empty, so none is attempted: evidence from
	// a contaminated composer is worse than none.
	dirty string
	// rename is what driving one programmatic rename of session A showed
	// (muster #227). It runs last, after every send, because it changes
	// session A's own id.
	rename compatRename
}

func (h *compatHarness) draftOf(name string) *compatDraft {
	if h.ev.drafts == nil {
		h.ev.drafts = map[string]*compatDraft{}
	}
	if h.ev.drafts[name] == nil {
		h.ev.drafts[name] = &compatDraft{}
	}
	return h.ev.drafts[name]
}

// readBypassSetting reads the one user setting that decides whether a bypass
// session meets its acceptance screen. It only reads.
func (h *compatHarness) readBypassSetting() {
	b, err := os.ReadFile(filepath.Join(h.world.store.home, ".claude", "settings.json"))
	if err != nil {
		if os.IsNotExist(err) {
			h.ev.bypassSetting = "absent"
			return
		}
		h.ev.bypassSettingErr = err
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		h.ev.bypassSettingErr = errors.New("the user settings file is not valid JSON")
		return
	}
	v, ok := m["skipDangerousModePermissionPrompt"]
	if !ok {
		h.ev.bypassSetting = "absent"
		return
	}
	h.ev.bypassSetting = strings.TrimSpace(string(v))
}

// boot launches one session and records how it came up.
func (h *compatHarness) boot(ctx context.Context, into *compatBoot, label, dirRole string, mutate func(*fleet.SessionSpec), until func(compatShot) bool, wait time.Duration) error {
	w := h.world
	started := time.Now()
	ref, err := w.create(ctx, label, dirRole, mutate)
	if err != nil {
		return fmt.Errorf("creating session %s: %w", label, err)
	}
	into.ran, into.ref, into.created = true, ref, started
	shot, ok := w.waitShot(ctx, ref, wait, until)
	into.shot, into.ready = shot, ok
	if ok {
		into.bootTime = time.Since(started)
	}
	return nil
}

// recordFor reads the per-process record for a booted session, waiting up to
// the limit the check asserts.
func (h *compatHarness) recordFor(ctx context.Context, b *compatBoot, limit time.Duration) {
	w := h.world
	row, ok := w.paneOf(ctx, b.ref)
	if !ok {
		return
	}
	b.pid = row.pid
	b.identity, b.identErr = w.d.processIdentityOfPID(ctx, row.pid)
	deadline := b.created.Add(limit)
	for {
		if bytes, ok := w.d.processSessionRecordBytes(row.pid); ok {
			b.rec, b.recAfter = bytes, time.Since(b.created)
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	b.live, b.liveOK = w.d.resolveLiveProcessSessionID(ctx, b.ref, &row)
}

// Waits, in the same spirit as compatSettleAge: variables so a test with a
// synthetic runtime can shorten them.
var (
	compatBootWait    = 30 * time.Second
	compatDialogWait  = 15 * time.Second
	compatRecordWait  = 15 * time.Second
	compatBracketWait = 5 * time.Second
	// compatModeWait is how long a session with a composer is given to paint the
	// row under it. Measured on a real build: the composer is up about a second
	// before the indicator row is, so a snapshot taken at "ready" reads no mode at
	// all — which is the honest answer for that instant and the wrong one to
	// judge a candidate by.
	compatModeWait = 10 * time.Second
)

// addBoot registers the probes that launch the five kinds of session.
func (h *compatHarness) addBoot(s *compat.Suite) {
	up := []string{"world.up"}
	s.Probes = append(s.Probes,
		// A: trusted directory, default permission mode.
		compat.Probe{ID: "boot.a", Stage: 2, Needs: up, Budget: 90 * time.Second, Run: func(ctx context.Context) error {
			// Session A carries a system-prompt file, so B7a can check it is honoured.
			withContext := func(sp *fleet.SessionSpec) { sp.ContextRef = fleet.AbsolutePath(h.world.ctxFile) }
			if err := h.boot(ctx, &h.ev.a, "a", "a", withContext, compatReady, compatBootWait); err != nil {
				return err
			}
			h.awaitBracket(ctx, &h.ev.a)
			h.recordFor(ctx, &h.ev.a, compatRecordWait)
			h.awaitMode(ctx, &h.ev.a)
			return nil
		}},
		// U: a directory outside the trust root, so the trust dialog appears. It
		// is observed and never answered: an answer persists a setting.
		compat.Probe{ID: "boot.u", Stage: 2, Needs: up, Budget: 60 * time.Second, Run: func(ctx context.Context) error {
			return h.boot(ctx, &h.ev.u, "u", "u", nil, func(s compatShot) bool { p, _ := s.prompt(); return p != nil || compatReady(s) }, compatDialogWait)
		}},
		// I: a directory the runtime has been told to trust, and NOT told it may
		// import from outside itself, whose instruction file does (#212). The
		// harness writes the trust answer itself — through the same writer the
		// service's seeder uses, keys narrowed to that one — because the driver's own
		// seeder writes both answers and so can never leave the question standing.
		// Observed and never answered: an answer persists a setting.
		compat.Probe{ID: "boot.i", Stage: 2, Needs: up, Budget: 60 * time.Second, Run: func(ctx context.Context) error {
			if err := h.world.trustOnly.SeedPath(h.world.dirs["i"]); err != nil {
				return fmt.Errorf("writing the trust answer for the imports directory: %w", err)
			}
			return h.boot(ctx, &h.ev.i, "i", "i", nil, func(s compatShot) bool { p, _ := s.prompt(); return p != nil || compatReady(s) }, compatDialogWait)
		}},
		// B: bypass-permissions mode. Whether it meets the acceptance screen
		// depends on a user setting, which is read so its absence is reported.
		compat.Probe{ID: "boot.b", Stage: 2, Needs: up, Budget: 60 * time.Second, Run: func(ctx context.Context) error {
			h.readBypassSetting()
			if err := h.boot(ctx, &h.ev.b, "b", "b", func(sp *fleet.SessionSpec) { sp.PermissionMode = fleet.PermissionModeBypass },
				func(s compatShot) bool { p, _ := s.prompt(); return p != nil || compatReady(s) }, compatDialogWait); err != nil {
				return err
			}
			h.awaitMode(ctx, &h.ev.b)
			return nil
		}},
		// C: the same, told on its command line that the acceptance screen is NOT
		// suppressed. Whether the runtime honours that is exactly what this finds
		// out; if it does not, the screen is unproducible and the check says so.
		compat.Probe{ID: "boot.c", Stage: 2, Needs: up, Budget: 60 * time.Second, Run: func(ctx context.Context) error {
			h.world.extraArgs[h.world.label+"-c"] = []string{"--settings", `{"skipDangerousModePermissionPrompt":false}`}
			return h.boot(ctx, &h.ev.c, "c", "b", func(sp *fleet.SessionSpec) { sp.PermissionMode = fleet.PermissionModeBypass },
				func(s compatShot) bool { p, _ := s.prompt(); return p != nil }, compatDialogWait)
		}},
	)
}

// draftInto pastes text into session A as a bracketed paste, records what the
// composer then shows, and clears it again so the next probe starts clean.
func (h *compatHarness) draftInto(ctx context.Context, name, text string) error {
	d := h.draftOf(name)
	d.text = text
	if !h.ev.a.ready {
		d.skipped = "the trusted session never showed a composer (see F-COMPOSER), so nothing could be drafted into it"
		return nil
	}
	if h.ev.dirty != "" {
		d.skipped = "an earlier draft could not be cleared (" + h.ev.dirty + "), so this composer is not empty and nothing more was drafted into it"
		return nil
	}
	w := h.world
	row, ok := w.paneOf(ctx, h.ev.a.ref)
	if !ok {
		return errors.New("the trusted session is gone")
	}
	d.ran = true
	d.pasteErr = w.d.pasteBracketed(ctx, row.paneID, text)
	if d.pasteErr == nil {
		// Wait for the paste to land, without deciding what "landed" means: the
		// check reads the composer, and a paste that never shows is evidence.
		w.waitShot(ctx, h.ev.a.ref, 3*time.Second, func(s compatShot) bool { return s.composer != "" })
	}
	d.shot = w.shoot(ctx, h.ev.a.ref)
	d.markers = markerCounts(d.shot.plain())
	d.holds = composerHoldsCollapsedPaste(d.shot.plain())
	d.rows, d.rowScan = composerRegion(d.shot.sc)
	d.matches = composerMatchesText(d.shot.sc, text, false)
	if d.pasteErr == nil || d.shot.composer != "" {
		// A composer that cannot be cleared is the candidate's behaviour, not the
		// environment's: it is recorded, and the checks that depend on a clean
		// composer fail with the reason. Returning it as a probe error would report
		// them as "could not run" and hide what was seen.
		if err := w.clearComposer(ctx, h.ev.a.ref); err != nil {
			d.clearErr = err
			h.ev.dirty = fmt.Sprintf("after %s: %v", name, err)
		}
	}
	return nil
}

// Draft payloads. The multi-line one is short enough to stay inline; the long
// ones are long enough to collapse into a marker.
func compatMultiLine() string { return "line one\nline two\nline three" }
func compatLines(n int) string {
	var b []string
	for i := 1; i <= n; i++ {
		b = append(b, fmt.Sprintf("cfc line %02d", i))
	}
	return strings.Join(b, "\n")
}
func compatLongLine() string { return strings.Repeat("abcdefghij", 90) } // 900 bytes, one line
func compatWrapText() string { return strings.Repeat("0123456789", 30) } // 300 bytes, wraps at 120 columns

// addDrafts registers the probes that put text in the composer without
// submitting it: no model turn is spent.
func (h *compatHarness) addDrafts(s *compat.Suite) {
	need := []string{"boot.a"}
	draft := func(id, name string, text func() string) compat.Probe {
		return compat.Probe{ID: id, Stage: 3, Needs: need, Budget: 60 * time.Second, Run: func(ctx context.Context) error {
			return h.draftInto(ctx, name, text())
		}}
	}
	s.Probes = append(s.Probes,
		draft("draft.line", "line", func() string { return "cfc-draft-" + h.world.nonce }),
		draft("draft.ml3", "ml3", compatMultiLine),
		draft("draft.paste40", "paste40", func() string { return compatLines(40) }),
		draft("draft.paste900", "paste900", compatLongLine),
		draft("draft.wrap", "wrap", compatWrapText),
		// A leading space defuses the prompt-mode characters, on this runtime.
		draft("draft.space-bang", "space-bang", func() string { return " !cfc-shape" }),
		draft("draft.space-slash", "space-slash", func() string { return " /cfc-shape" }),
		// A leading "!" enters shell mode, which leaves the composer unreadable
		// by design — so it gets a session of its own and is never reused.
		compat.Probe{ID: "draft.bang", Stage: 3, Needs: []string{"world.up"}, Budget: 60 * time.Second, Run: func(ctx context.Context) error {
			w := h.world
			if err := h.boot(ctx, &h.ev.bang, "bang", "a", nil, compatReady, compatBootWait); err != nil {
				return err
			}
			if !h.ev.bang.ready {
				return nil // recorded: the check says the session never came up
			}
			row, ok := w.paneOf(ctx, h.ev.bang.ref)
			if !ok {
				return errors.New("the shell-mode session is gone")
			}
			if out, err := w.tmux(ctx, "send-keys", "-t", row.paneID, "-l", "!cfc-shape"); err != nil {
				return fmt.Errorf("typing into the shell-mode session: %v: %s", err, strings.TrimSpace(out))
			}
			// The mode appears anywhere from a third of a second to a second and a
			// half after the keystroke (measured over fifteen fresh sessions), so
			// poll for it rather than sleeping a fixed time and judging a screen
			// that has not repainted yet. A candidate that never shows it is
			// evidence: the last shot is kept either way.
			h.ev.bang.shot, _ = w.waitShot(ctx, h.ev.bang.ref, 5*time.Second, func(s compatShot) bool { return shellMode(s.sc) })
			return nil
		}},
	)
}
