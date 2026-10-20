package tmux

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/compat"
	"github.com/futurelastic/muster/internal/trustseed"
)

// The evaluators. Each reads what a probe recorded and says what it means; none
// touches the multiplexer. Detail strings say what was seen, in numbers and
// names, so a failing report can be acted on without re-running anything.

// describePrompt names a prompt without quoting its question, which can be long
// and is prose.
func describePrompt(p *fleet.SessionPrompt) string {
	if p == nil {
		return "no prompt"
	}
	return fmt.Sprintf("kind %q with options %q", string(p.Kind), p.Options)
}

// recordFields decodes a per-process record into its raw fields.
func recordFields(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// screenTail is the last few non-empty rows of a screen, for a failure detail:
// what was actually on the pane is the first thing a reader of a failing report
// wants, and the banner rows above are not.
func screenTail(sc screen, n int) string {
	var rows []string
	for i := len(sc.lines) - 1; i >= 0 && len(rows) < n; i-- {
		if l := strings.TrimSpace(sc.lines[i]); l != "" && !isRule(sc.lines[i]) {
			if len([]rune(l)) > 80 {
				l = string([]rune(l)[:80]) + "…"
			}
			rows = append([]string{l}, rows...)
		}
	}
	return strings.Join(rows, " ⏎ ")
}

// shellMode reports whether the composer is showing the runtime's shell mode: a
// row that begins with "!" directly under a rule. In that mode the prompt glyph
// is replaced, which is exactly why the driver cannot read the composer and why
// it refuses to deliver text beginning with "!".
func shellMode(sc screen) bool {
	for i := len(sc.lines) - 1; i > 0 && i >= len(sc.lines)-12; i-- {
		if strings.HasPrefix(strings.TrimSpace(sc.lines[i]), "!") && strings.ContainsRune(sc.lines[i-1], ruleRune) {
			return true
		}
	}
	return false
}

// unrun is the verdict for a draft that was never made: the candidate showed no
// composer to draft into, which is a failure of what the checks assume.
func unrun(d *compatDraft) (compat.Verdict, bool) {
	if d.ran {
		return compat.Verdict{}, false
	}
	return compat.Failed(d.skipped), true
}

// plainComposerRow is the text after the prompt glyph on the composer's first
// row, without escape sequences: what a person would see written there.
func plainComposerRow(sc screen) string {
	for i := len(sc.lines) - 1; i >= 0 && i >= len(sc.lines)-promptScanDepth; i-- {
		if strings.Contains(sc.lines[i], composerRuneMarker) {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.lines[i]), composerRuneMarker))
		}
	}
	return ""
}

// composerRow returns the raw (escape-bearing) row that carries the prompt glyph.
func composerRow(sc screen) (string, bool) {
	for i := len(sc.raw) - 1; i >= 0 && i >= len(sc.raw)-promptScanDepth; i-- {
		if strings.Contains(sc.raw[i], composerRuneMarker) {
			return sc.raw[i], true
		}
	}
	return "", false
}

// addBootChecks registers the checks that read the five boot probes.
func (h *compatHarness) addBootChecks(s *compat.Suite) {
	s.Checks = append(s.Checks,
		compat.Check{ID: "C1", Probes: []string{"boot.a"}, Eval: func() compat.Verdict {
			b := h.ev.a
			switch p, _ := b.shot.prompt(); {
			case p != nil && p.Kind == fleet.PromptExternalImports:
				// The directory's instruction file imports a file from outside it (#212),
				// so this is the seeder's imports answer not being honoured — the runtime
				// renamed a key, or stopped reading one — and not a trust failure.
				return compat.Failed("a directory the driver seeded as trusted, whose instruction file imports a file from outside it, still asks about that import: " + describePrompt(p) +
					fmt.Sprintf(" — the runtime no longer honours the answer the seeder writes (%s and %s), or records it under another name", trustseed.ImportsApprovedKey, trustseed.ImportsShownKey))
			case p != nil:
				return compat.Failed("a directory the driver seeded as trusted still shows a prompt: " + describePrompt(p))
			case !b.ready:
				return compat.Failed(fmt.Sprintf("the session never showed a composer within %s (status %q)", compatBootWait, b.shot.state.Status))
			}
			return compat.Passed(fmt.Sprintf("the seeded directory reached its composer in %s with no dialog, although its instruction file imports a file from outside it", b.bootTime.Round(100*time.Millisecond)))
		}},

		compat.Check{ID: "F-TRUST", Probes: []string{"boot.u"}, Eval: func() compat.Verdict {
			u := h.ev.u
			p, unnumbered := u.shot.prompt()
			if p == nil {
				return compat.Failed(fmt.Sprintf("no dialog appeared for a directory outside the trust root (status %q; the pane shows: %s)", u.shot.state.Status, screenTail(u.shot.sc, 4)))
			}
			var bad []string
			if p.Kind != fleet.PromptFolderTrust {
				bad = append(bad, fmt.Sprintf("classified as %q, not %q", string(p.Kind), string(fleet.PromptFolderTrust)))
			}
			if !unnumbered {
				bad = append(bad, "the menu is numbered, so a digit would choose an option; the driver assumes it is not")
			}
			idx, ok := affirmativeOption(p)
			switch {
			case !ok:
				bad = append(bad, "the driver cannot find exactly one affirmative option in "+describePrompt(p))
			case idx < 1 || idx > len(p.Options) || !strings.Contains(strings.ToLower(p.Options[idx-1]), "trust"):
				bad = append(bad, fmt.Sprintf("the affirmative option (#%d) is not the trust option", idx))
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed(fmt.Sprintf("folder-trust dialog, unnumbered menu, one affirmative option (#%d of %d); observed only, never answered", idx, len(p.Options)))
		}},

		// F-IMPORTS (#212). The pair to C1: C1 shows that the answer the seeder writes
		// stops the imports question; this shows the question is still there to be
		// stopped, and is still read the way the classifier and the consent read it.
		// Observed only. The directory is trusted, not import-approved, and imports a
		// file from outside itself, so the only thing standing between it and its
		// composer is this one dialog.
		compat.Check{ID: "F-IMPORTS", Probes: []string{"boot.i"}, Eval: func() compat.Verdict {
			i := h.ev.i
			p, unnumbered := i.shot.prompt()
			if p == nil {
				return compat.Failed(fmt.Sprintf("no dialog appeared for a trusted directory whose instruction file imports a file from outside it and which was never approved to (status %q; the pane shows: %s) — the runtime no longer asks, or asks in a shape the driver does not read as a menu", i.shot.state.Status, screenTail(i.shot.sc, 4)))
			}
			var bad []string
			switch p.Kind {
			case fleet.PromptExternalImports:
			case fleet.PromptFolderTrust:
				// The question before this one is still standing, so the imports one was
				// never reached: the trust answer the harness wrote for this directory was
				// not honoured, which is the runtime's doing (see C1), not this dialog's.
				bad = append(bad, "the trust question was asked instead, so the trust answer the harness wrote for this directory was not honoured and the imports question was never reached")
			default:
				bad = append(bad, fmt.Sprintf("classified as %q, not %q", string(p.Kind), string(fleet.PromptExternalImports)))
			}
			if !unnumbered {
				bad = append(bad, "the menu is numbered, so a digit would choose an option; the driver assumes it is not")
			}
			idx, ok := affirmativeOption(p)
			switch {
			case !ok:
				bad = append(bad, "the driver cannot find exactly one affirmative option in "+describePrompt(p))
			case idx < 1 || idx > len(p.Options) || !strings.Contains(strings.ToLower(p.Options[idx-1]), "allow"):
				bad = append(bad, fmt.Sprintf("the affirmative option (#%d) is not the allow option", idx))
			}
			// The highlight is the SAFE default: a bare Enter, or anything that answers by
			// the highlighted row, declines. The driver answers by index and never relies
			// on it, but a build that moves it onto the allow row turns every stray Enter
			// into a consent, which is worth stopping a fleet for.
			if p.Selected < 1 || p.Selected > len(p.Options) || !strings.Contains(strings.ToLower(p.Options[p.Selected-1]), "disable") {
				bad = append(bad, fmt.Sprintf("the highlighted row (#%d) is not the decline, so a bare Enter would allow the import", p.Selected))
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed(fmt.Sprintf("external-imports dialog, unnumbered menu, the decline highlighted (#%d of %d) and one affirmative option (#%d); observed only, never answered", p.Selected, len(p.Options), idx))
		}},

		compat.Check{ID: "B5", Probes: []string{"boot.b"}, Eval: func() compat.Verdict {
			if h.ev.bypassSettingErr != nil {
				return compat.Errored("could not read the user settings to check the premise: " + h.ev.bypassSettingErr.Error())
			}
			if h.ev.bypassSetting != "true" {
				return compat.Errored(fmt.Sprintf("the premise is missing: the user setting skipDangerousModePermissionPrompt is %s, so a bypass session is expected to meet its acceptance screen and this check cannot judge the candidate", h.ev.bypassSetting))
			}
			b := h.ev.b
			switch p, _ := b.shot.prompt(); {
			case p != nil:
				return compat.Failed("a bypass session met a prompt although the setting that suppresses it is on: " + describePrompt(p))
			case !b.ready:
				return compat.Failed(fmt.Sprintf("a bypass session never showed a composer within %s (status %q)", compatDialogWait, b.shot.state.Status))
			}
			return compat.Passed(fmt.Sprintf("a bypass session reached its composer in %s with no acceptance screen", b.bootTime.Round(100*time.Millisecond)))
		}},

		compat.Check{ID: "F-BYPASS", Probes: []string{"boot.c", "static.markers"}, Eval: func() compat.Verdict {
			p, _ := h.ev.c.shot.prompt()
			if p != nil {
				spec := fleet.SessionSpec{PermissionMode: fleet.PermissionModeBypass, Consents: []fleet.PromptKind{fleet.PromptBypassAcceptance}}
				if acceptanceScreen(spec, p) {
					return compat.Passed("the acceptance screen was produced and the driver recognises it: " + describePrompt(p) + "; observed only, never answered")
				}
				return compat.Failed("a bypass session met a prompt the driver does not recognise as the acceptance screen: " + describePrompt(p))
			}
			var missing []string
			for _, m := range compatStaticMarkers["F-BYPASS"] {
				if !h.markers[m] {
					missing = append(missing, fmt.Sprintf("%q", m))
				}
			}
			if len(missing) > 0 {
				return compat.Failed("the screen could not be produced here, and the candidate no longer contains " + strings.Join(missing, ", "))
			}
			return compat.Passed("the screen could not be produced here (the runtime did not show it when told the setting is off); the wording of its two options is still in the candidate")
		}},

		// F-MODE (#194). Two of the five indicator rows are read off live sessions
		// the harness already boots — the default mode (session A) and bypass
		// (session B) — so what is asserted is the driver's own read of the
		// candidate's real screen, not the wording in a table. The other three
		// cannot be entered without pressing keys in a session other checks share,
		// so their wording is checked statically instead, the way H-RC does.
		//
		// Warn, not must: a candidate whose indicator wording changed still
		// delivers, confirms and classifies dialogs exactly as before — only the
		// mode read goes to `unknown`. That is loud enough to be seen in the report
		// before a fleet takes the build, and it is not a reason to reject one.
		// The gate is data, not shape (docs/compat.md, Schema): a maintainer who
		// decides a mode control is load-bearing can make it must without a bump.
		compat.Check{ID: "F-MODE", Probes: []string{"boot.a", "boot.b", "static.markers"}, Eval: func() compat.Verdict {
			var bad, seen []string
			live := func(name string, b compatBoot, want fleet.PermissionModeState) {
				switch {
				case !b.ready:
					// A session with no composer is reported by the check that owns
					// that (C1, B5); there is no indicator to read here.
					bad = append(bad, name+" never showed a composer, so its indicator could not be read")
				case b.mode.state.PermissionMode != want:
					got := string(b.mode.state.PermissionMode)
					if got == "" {
						got = fmt.Sprintf("nothing within %s (the row under the composer never became readable)", compatModeWait)
					}
					bad = append(bad, fmt.Sprintf("%s reads as %s, want %s (the pane shows: %s)",
						name, got, string(want), screenTail(b.mode.sc, 2)))
				default:
					seen = append(seen, fmt.Sprintf("%s reads as %s", name, string(want)))
				}
			}
			live("a session started in the default mode", h.ev.a, fleet.PermissionModeDefault)
			live("a session started in bypass-permissions mode", h.ev.b, fleet.PermissionModeBypass)
			var missing []string
			for _, m := range compatStaticMarkers["F-MODE"] {
				if !h.markers[m] {
					missing = append(missing, fmt.Sprintf("%q", m))
				}
			}
			if len(missing) > 0 {
				bad = append(bad, "the candidate no longer contains "+strings.Join(missing, ", "))
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; ") +
					": the mode reader may report `unknown` for sessions in a mode the candidate now words differently")
			}
			return compat.Passed(strings.Join(seen, "; ") + fmt.Sprintf("; the wording of the other %d indicator rows is still in the candidate", len(compatStaticMarkers["F-MODE"])))
		}},

		compat.Check{ID: "D1", Probes: []string{"boot.a"}, Eval: func() compat.Verdict {
			b := h.ev.a
			if !b.ran || b.rec == nil {
				return compat.Failed(fmt.Sprintf("no per-process record appeared within %s of the launch", compatRecordWait))
			}
			m, err := recordFields(b.rec)
			if err != nil {
				return compat.Failed("the record is not a JSON object: " + err.Error())
			}
			var bad []string
			str := func(k string) (string, bool) { v, ok := m[k].(string); return v, ok }
			num := func(k string) (float64, bool) { v, ok := m[k].(float64); return v, ok }
			if v, ok := num("pid"); !ok || int(v) != b.pid {
				bad = append(bad, fmt.Sprintf("pid is %v, want the pane's %d", m["pid"], b.pid))
			}
			if v, ok := str("sessionId"); !ok || !uuidShaped(v) {
				bad = append(bad, "sessionId is not a UUID-shaped string")
			}
			if v, ok := str("cwd"); !ok || v != h.world.dirs["a"] {
				bad = append(bad, "cwd is not the launch directory")
			}
			if v, ok := num("startedAt"); !ok {
				bad = append(bad, "startedAt is not a number")
			} else if ms := time.UnixMilli(int64(v)); ms.Before(b.created.Add(-time.Minute)) || ms.After(time.Now().Add(time.Minute)) {
				bad = append(bad, fmt.Sprintf("startedAt %s is not epoch milliseconds near the launch", ms.UTC().Format(time.RFC3339)))
			}
			if v, ok := str("name"); !ok || v != b.ref.ID {
				bad = append(bad, fmt.Sprintf("name is %q, want the -n value %q", m["name"], b.ref.ID))
			}
			if v, _ := str("kind"); v != "interactive" {
				bad = append(bad, fmt.Sprintf("kind is %q, want %q", m["kind"], "interactive"))
			}
			if v, _ := str("entrypoint"); v != "cli" {
				bad = append(bad, fmt.Sprintf("entrypoint is %q, want %q", m["entrypoint"], "cli"))
			}
			if v, ok := str("version"); !ok || v != h.cand.Version {
				bad = append(bad, fmt.Sprintf("version is %q, --version says %q", m["version"], h.cand.Version))
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed(fmt.Sprintf("record present %s after launch with pid, sessionId, cwd, startedAt, name, kind, entrypoint and version as expected", b.recAfter.Round(100*time.Millisecond)))
		}},

		compat.Check{ID: "D3", Probes: []string{"boot.a"}, Eval: func() compat.Verdict {
			b := h.ev.a
			if b.rec == nil {
				return compat.Failed("there is no record to corroborate")
			}
			m, err := recordFields(b.rec)
			if err != nil {
				return compat.Failed("the record is not a JSON object: " + err.Error())
			}
			if !b.liveOK {
				detail := "the driver could not corroborate the record against the running process"
				if b.identErr != nil {
					detail += ": " + b.identErr.Error()
				}
				if ps, _ := m["procStart"].(string); ps != "" {
					if _, perr := parseProcessSessionRecordStartTime(ps); perr != nil {
						detail += fmt.Sprintf("; procStart %q does not parse in the layout the driver expects", ps)
					} else {
						detail += "; procStart parses, so it does not match the process's own start time (UTC text, or Linux ticks since boot, assumed)"
					}
				} else {
					detail += "; the record has no procStart"
				}
				return compat.Failed(detail)
			}
			if id, _ := m["sessionId"].(string); id != b.live.sessionID {
				return compat.Failed("the corroborated session id differs from the record's")
			}
			return compat.Passed("procStart matches the running process's own start time, so the record is corroborated as this process's")
		}},

		compat.Check{ID: "D4", Probes: []string{"boot.a"}, Eval: func() compat.Verdict {
			b := h.ev.a
			if b.rec == nil {
				return compat.Failed("there is no record to read")
			}
			m, err := recordFields(b.rec)
			if err != nil {
				return compat.Failed("the record is not a JSON object: " + err.Error())
			}
			var bad []string
			if v, has := m["bridgeSessionId"]; has {
				bad = append(bad, fmt.Sprintf("the record carries a bridge id (%v) with remote control off", v))
			}
			if cc := b.shot.state.ControlChannel; cc != nil {
				bad = append(bad, fmt.Sprintf("the screen reports control channel %q with remote control off", string(cc.State)))
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed("remote control is off: no bridge id in the record and no control-channel label on the screen")
		}},
	)
}

// addComposerChecks registers the checks that read the draft probes.
func (h *compatHarness) addComposerChecks(s *compat.Suite) {
	s.Checks = append(s.Checks,
		compat.Check{ID: "F-COMPOSER", Probes: []string{"boot.a", "draft.line"}, Eval: func() compat.Verdict {
			b := h.ev.a
			if !b.ready {
				return compat.Failed(fmt.Sprintf("no composer within %s (status %q)", compatBootWait, b.shot.state.Status))
			}
			var bad []string
			if b.shot.composer != "" {
				bad = append(bad, fmt.Sprintf("an empty composer reads as %q, so its placeholder was taken for a draft", b.shot.composer))
			}
			d := h.draftOf("line")
			switch {
			case !d.ran:
				// Already reported above: there was no composer to draft into.
			case d.pasteErr != nil:
				bad = append(bad, "a one-line draft could not be pasted: "+d.pasteErr.Error())
			case d.shot.composer != d.text:
				bad = append(bad, fmt.Sprintf("a typed draft %q reads back as %q", d.text, d.shot.composer))
			case d.shot.state.ComposerDigest == "":
				bad = append(bad, "a draft is present but the driver reports no digest for it")
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			seen := "no placeholder was painted yet"
			if plainComposerRow(b.shot.sc) != "" {
				seen = "the placeholder shown reads as empty (it is dim)"
			}
			return compat.Passed("the composer sits between two rules, an empty composer reads as empty (" + seen + "), and a one-line draft reads back exactly")
		}},

		compat.Check{ID: "F-MLDRAFT", Probes: []string{"draft.ml3"}, Eval: func() compat.Verdict {
			d := h.draftOf("ml3")
			if v, skip := unrun(d); skip {
				return v
			}
			switch {
			case d.pasteErr != nil:
				return compat.Failed("a three-line draft could not be pasted: " + d.pasteErr.Error())
			case !d.matches:
				return compat.Failed(fmt.Sprintf("a three-line draft does not read back as what was pasted (the composer reads %q)", d.shot.composer))
			}
			return compat.Passed(fmt.Sprintf("a three-line draft reads back as pasted (%q)", d.shot.composer))
		}},

		compat.Check{ID: "F-PASTEMARK", Probes: []string{"draft.paste40", "draft.paste900"}, Eval: func() compat.Verdict {
			d40, d900 := h.draftOf("paste40"), h.draftOf("paste900")
			if v, skip := unrun(d40); skip {
				return v
			}
			if v, skip := unrun(d900); skip {
				return v
			}
			bad := collapseFindings(d40, d900)
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed("a 40-line paste collapses to one marker of +39 lines and a 900-byte single line to one bare marker, each counted as delivery confirmation counts them")
		}},

		compat.Check{ID: "F-WRAP", Probes: []string{"draft.wrap"}, Eval: func() compat.Verdict {
			d := h.draftOf("wrap")
			if v, skip := unrun(d); skip {
				return v
			}
			if d.pasteErr != nil {
				return compat.Failed("a 300-byte draft could not be pasted: " + d.pasteErr.Error())
			}
			if d.rowScan != composerFound || len(d.rows) < 2 {
				return compat.Failed(fmt.Sprintf("a 300-byte draft should wrap onto several rows at %d columns; the driver found %d row(s) (scan %v)", compatPaneCols, len(d.rows), d.rowScan))
			}
			var lens []int
			for _, r := range d.rows {
				lens = append(lens, len([]rune(r)))
			}
			for i := 1; i < len(lens)-1; i++ {
				if lens[i] != lens[0] {
					return compat.Failed(fmt.Sprintf("the wrapped rows are not a uniform width (%v), so they cannot be reassembled by width", lens))
				}
			}
			if !d.matches {
				return compat.Failed(fmt.Sprintf("the wrapped rows (widths %v) do not read back as the pasted text", lens))
			}
			return compat.Passed(fmt.Sprintf("a 300-byte draft wraps onto %d rows of width %v at %d columns and reads back as pasted", len(d.rows), lens, compatPaneCols))
		}},

		compat.Check{ID: "G6", Probes: []string{"draft.bang", "draft.space-bang", "draft.space-slash"}, Eval: func() compat.Verdict {
			bang := h.ev.bang
			if !bang.ran || !bang.ready {
				return compat.Failed("the session used to try shell mode never showed a composer (see F-COMPOSER)")
			}
			var bad []string
			if !shellMode(bang.shot.sc) {
				bad = append(bad, "a leading ! in an empty composer no longer enters shell mode, so the input guard refuses text it need not (the pane shows: "+screenTail(bang.shot.sc, 4)+")")
			} else if bang.shot.scan == composerFound {
				bad = append(bad, "in shell mode the driver still finds a composer, which is not what its refusal of a leading ! assumes")
			}
			for _, shape := range []struct{ name, want string }{{"space-bang", "!cfc-shape"}, {"space-slash", "/cfc-shape"}} {
				d := h.draftOf(shape.name)
				switch {
				case !d.ran:
					bad = append(bad, fmt.Sprintf("%q could not be tried: %s", d.text, d.skipped))
				case d.pasteErr != nil:
					bad = append(bad, fmt.Sprintf("%q could not be pasted: %v", d.text, d.pasteErr))
				case shellMode(d.shot.sc):
					bad = append(bad, fmt.Sprintf("%q entered shell mode: a leading space no longer defuses the prompt-mode character", d.text))
				case d.shot.scan != composerFound || d.shot.composer != shape.want:
					bad = append(bad, fmt.Sprintf("%q should stay a plain prompt reading %q, the composer reads %q", d.text, shape.want, d.shot.composer))
				}
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed("a leading ! enters shell mode (the composer becomes unreadable to the driver, as its guard assumes); the same text after a space, and a slash after a space, stay plain prompts")
		}},
	)
}

// collapseFindings is what the two long drafts must show for the paste to have
// been bracketed: the runtime collapses a bracketed 40-line paste to one marker
// of +39 lines and a bracketed 900-byte single line to one bare marker. Each
// draft must already have run.
func collapseFindings(d40, d900 *compatDraft) []string {
	var bad []string
	only := func(m map[pasteKey]int) (pasteKey, bool) {
		if len(m) != 1 {
			return pasteKey{}, false
		}
		for k, n := range m {
			return k, n == 1
		}
		return pasteKey{}, false
	}
	if d40.pasteErr != nil {
		bad = append(bad, "a 40-line paste could not be made: "+d40.pasteErr.Error())
	} else if k, ok := only(d40.markers); !ok || k.lines != 39 || !d40.holds {
		bad = append(bad, fmt.Sprintf("a 40-line paste should show one marker of +39 lines, found %v (holds a collapsed paste: %v)", d40.markers, d40.holds))
	}
	if d900.pasteErr != nil {
		bad = append(bad, "a 900-byte single-line paste could not be made: "+d900.pasteErr.Error())
	} else if k, ok := only(d900.markers); !ok || k.lines != 0 || !d900.holds {
		bad = append(bad, fmt.Sprintf("a 900-byte single line should show one bare marker, found %v (holds a collapsed paste: %v)", d900.markers, d900.holds))
	}
	return bad
}
