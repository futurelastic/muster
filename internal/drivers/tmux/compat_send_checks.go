package tmux

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/compat"
)

// unranSend is the verdict for a send that was never made: the candidate showed
// no composer to send into (or an earlier draft left it unclean), which fails
// what the checks assume rather than being an environment problem.
func unranSend(s *compatSend) (compat.Verdict, bool) {
	if s.ran {
		return compat.Verdict{}, false
	}
	return compat.Failed(s.skipped), true
}

// entryText is the text the driver would read out of a user entry.
func entryText(e map[string]any) string {
	line, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	_, text, _, _ := extractTranscriptCandidate(line)
	return text
}

// deliveredOK lists every way a send's receipt and record fall short of "it
// arrived once, and the session is ready again".
func deliveredOK(name string, s *compatSend) []string {
	var bad []string
	switch {
	case s.err != nil:
		return []string{fmt.Sprintf("%s: Send returned an error: %v", name, s.err)}
	case s.rcpt.Outcome != fleet.OutcomeQueued && s.rcpt.Outcome != fleet.OutcomeSubmitted:
		return []string{fmt.Sprintf("%s: the receipt says %q, not queued or submitted (%s)", name, s.rcpt.Outcome, s.rcpt.Reason)}
	}
	if len(s.matches) != 1 {
		bad = append(bad, fmt.Sprintf("%s: the transcript records %d matching user turns, want exactly one", name, len(s.matches)))
	}
	if !s.idle || s.composer != "" {
		bad = append(bad, fmt.Sprintf("%s: the session did not come back to an empty composer (composer %q)", name, s.composer))
	}
	return bad
}

const transcriptConfirmed = "the runtime's own transcript recorded this exact text as a turn"

// addSendChecks registers the checks that read the send probes.
func (h *compatHarness) addSendChecks(s *compat.Suite) {
	all := []string{"send.warm", "send.short", "send.long", "send.multi", "send.control"}
	s.Checks = append(s.Checks,
		compat.Check{ID: "G1", Probes: all, Eval: func() compat.Verdict {
			short, warm := h.sendOf("short"), h.sendOf("warm")
			if v, skip := unranSend(short); skip {
				return v
			}
			var bad []string
			for _, n := range []string{"warm", "short", "long", "multi", "control"} {
				sd := h.sendOf(n)
				if !sd.ran {
					bad = append(bad, n+": not sent: "+sd.skipped)
					continue
				}
				bad = append(bad, deliveredOK(n, sd)...)
			}
			if !strings.Contains(short.rcpt.Reason, transcriptConfirmed) {
				bad = append(bad, "the second send was not confirmed by the transcript: "+short.rcpt.Reason)
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed(fmt.Sprintf("every send was received exactly once and the session came back ready; the second was confirmed by the runtime's own transcript in %s (the first, sent before any transcript existed, %s)",
				short.dur.Round(100*time.Millisecond), firstConfirmation(warm)))
		}},

		compat.Check{ID: "G2", Probes: append([]string{"boot.a", "draft.paste40", "draft.paste900"}, all...), Eval: func() compat.Verdict {
			var bad []string
			unreadable := false
			if f := h.ev.a.shot.bracket; f != "1" {
				if flagUnreadableByVersion(f, h.tmuxVersionText()) {
					// A multiplexer older than 3.7 cannot say whether the pane asked
					// (#334). The proof that it did is what only a bracketed paste
					// can do: a long paste collapses into the runtime's marker.
					unreadable = true
					bad = append(bad, bracketedArrivalProof(h.draftOf("paste40"), h.draftOf("paste900"))...)
				} else {
					bad = append(bad, bracketFlagFailure(f, h.tmuxVersionText()))
				}
			}
			for _, n := range []string{"long", "multi", "control"} {
				sd := h.sendOf(n)
				if !sd.ran {
					bad = append(bad, n+": not sent: "+sd.skipped)
					continue
				}
				bad = append(bad, deliveredOK(n, sd)...)
				if n == "control" {
					for _, m := range sd.matches {
						if strings.ContainsAny(entryText(m), "\x01\x1b\x7f") {
							bad = append(bad, "control: the recorded turn still contains control bytes the sanitiser removes")
						}
					}
				}
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			if unreadable {
				return compat.Passed("the multiplexer is older than " + BracketPasteFlagSince + " and cannot report the flag; a 40-line paste and a 900-byte single line still collapsed to the runtime's paste marker, which only a bracketed paste does, and a 900-byte single line, a 40-line paste and text with control bytes each arrived exactly once, the control bytes not reaching the runtime")
			}
			return compat.Passed("bracketed paste is on at the composer; a 900-byte single line, a 40-line paste and text with control bytes each arrived exactly once, and the control bytes did not reach the runtime")
		}},

		compat.Check{ID: "E-USER", Probes: all, Eval: func() compat.Verdict {
			var bad []string
			var sources = map[string]bool{}
			total := 0
			for _, n := range []string{"warm", "short", "long", "multi", "control"} {
				sd := h.sendOf(n)
				if !sd.ran {
					return compat.Failed(n + ": not sent: " + sd.skipped)
				}
				for _, m := range sd.matches {
					total++
					msg, _ := m["message"].(map[string]any)
					if msg == nil || msg["content"] == nil {
						bad = append(bad, n+": a user entry has no message.content")
					}
					if o, ok := m["origin"].(map[string]any); ok {
						if k, _ := o["kind"].(string); k != "human" {
							bad = append(bad, fmt.Sprintf("%s: origin.kind is %q, not human", n, k))
						}
					}
					if ps, ok := m["promptSource"].(string); ok {
						sources[ps] = true
						if ps != "typed" && ps != "queued" {
							bad = append(bad, fmt.Sprintf("%s: promptSource is %q, not typed or queued", n, ps))
						}
					}
					for _, k := range []string{"isMeta", "isSidechain", "isCompactSummary"} {
						if v, _ := m[k].(bool); v {
							bad = append(bad, fmt.Sprintf("%s: %s is set on the user's own turn", n, k))
						}
					}
				}
			}
			if total == 0 {
				return compat.Failed("no user entry was recorded for any send")
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			var ps []string
			for k := range sources {
				ps = append(ps, k)
			}
			return compat.Passed(fmt.Sprintf("%d user entries carry message.content, a human origin and promptSource %v, and none is marked meta, sidechain or a summary", total, ps))
		}},

		compat.Check{ID: "E-PASTE", Probes: []string{"send.long", "send.multi", "send.control"}, Eval: func() compat.Verdict {
			var bad []string
			var notes []string
			for _, n := range []string{"long", "multi", "control"} {
				sd := h.sendOf(n)
				if !sd.ran {
					return compat.Failed(n + ": not sent: " + sd.skipped)
				}
				if len(sd.matches) != 1 {
					bad = append(bad, fmt.Sprintf("%s: %d matching turns recorded, want one", n, len(sd.matches)))
					continue
				}
				text := entryText(sd.matches[0])
				// STRICT equality after unwrapping. The driver's own matcher also
				// accepts a bare "[Pasted text +N lines]" marker of the right line
				// count, which would let a runtime that recorded only the marker pass
				// unnoticed; this asserts the real text was recorded.
				if normalizeTranscriptText(text) != normalizeForMatch(sd.sent) {
					bad = append(bad, fmt.Sprintf("%s: the recorded turn does not unwrap to the text that was sent (recorded %d bytes, sent %d)", n, len(text), len(sd.sent)))
					continue
				}
				wrapped := pastedContentOpenTag.MatchString(text)
				notes = append(notes, fmt.Sprintf("%s wrapped=%v", n, wrapped))
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed("each long or collapsed paste was recorded as the real text, and unwraps to exactly what was sent (" + strings.Join(notes, ", ") + ")")
		}},

		compat.Check{ID: "E-NAME", Probes: []string{"boot.a", "send.warm"}, Eval: func() compat.Verdict {
			path, ok := h.transcriptPath()
			if !ok {
				return compat.Failed("there is no per-process record to name the transcript file from (see D1)")
			}
			entry, ok := readRecordEntry(path)
			if !ok {
				return compat.Failed(fmt.Sprintf("no transcript at <sessionId>.jsonl carries a session id and a custom-title within the first %d lines", recordScanLines))
			}
			m, _ := recordFields(h.ev.a.rec)
			var bad []string
			if id, _ := m["sessionId"].(string); entry.id != id {
				bad = append(bad, fmt.Sprintf("the transcript's session id %q differs from the record's %q", entry.id, id))
			}
			if entry.title != h.ev.a.ref.ID {
				bad = append(bad, fmt.Sprintf("the transcript's title is %q, want the -n value %q", entry.title, h.ev.a.ref.ID))
			}
			first := "the first line"
			if entries, err := readEntries(path, 0); err == nil && len(entries) > 0 {
				if t, _ := entries[0]["type"].(string); t != "custom-title" {
					first = fmt.Sprintf("a later line (the first is %q)", t)
				}
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed(fmt.Sprintf("the transcript is <sessionId>.jsonl and carries the session's name as its title on %s, within the %d lines the driver reads", first, recordScanLines))
		}},

		compat.Check{ID: "E-SLUG", Probes: []string{"boot.a", "send.warm"}, Eval: func() compat.Verdict {
			path, ok := h.transcriptPath()
			if !ok {
				return compat.Failed("there is no per-process record to name the transcript file from (see D1)")
			}
			if _, err := readEntries(path, 0); err != nil {
				return compat.Failed(fmt.Sprintf("no transcript at %s, the directory the driver derives for a working directory containing a dot, an underscore, a space and a non-ASCII letter; the runtime wrote it elsewhere or not at all", recordDirFor(h.world.dirs["a"])))
			}
			return compat.Passed(fmt.Sprintf("a working directory with a dot, an underscore, a space and a non-ASCII letter is recorded under %s, as the driver derives it", recordDirFor(h.world.dirs["a"])))
		}},

		compat.Check{ID: "B2", Probes: []string{"boot.a", "send.warm"}, Eval: func() compat.Verdict {
			a := h.ev.a
			m, err := recordFields(a.rec)
			if a.rec == nil || err != nil {
				return compat.Failed("there is no per-process record to read the name from (see D1)")
			}
			var bad []string
			if n, _ := m["name"].(string); n != a.ref.ID {
				bad = append(bad, fmt.Sprintf("the record's name is %q, want the -n value %q", n, a.ref.ID))
			}
			ref := h.world.d.conversations.derive(h.world.dirs["a"], a.ref.ID, a.created)
			id, _ := m["sessionId"].(string)
			switch {
			case ref == nil || !ref.Known:
				why := "unresolved"
				if ref != nil {
					why = ref.Evidence
				}
				bad = append(bad, "the driver cannot join the session to its conversation: "+why)
			case ref.ID != id:
				bad = append(bad, fmt.Sprintf("the driver joins the session to conversation %q, the record says %q", ref.ID, id))
			}
			if len(bad) > 0 {
				return compat.Failed(strings.Join(bad, "; "))
			}
			return compat.Passed("-n names the record and the transcript's title, and the driver joins the session to its conversation by that name (with remote control off)")
		}},

		compat.Check{ID: "B7a", Probes: []string{"send.short"}, Eval: func() compat.Verdict {
			short := h.sendOf("short")
			if v, skip := unranSend(short); skip {
				return v
			}
			token := "CFC-CTX-" + h.world.nonce
			var reply strings.Builder
			for _, e := range short.entries {
				if t, _ := e["type"].(string); t != "assistant" {
					continue
				}
				msg, _ := e["message"].(map[string]any)
				blocks, _ := msg["content"].([]any)
				for _, b := range blocks {
					if bm, ok := b.(map[string]any); ok {
						if txt, ok := bm["text"].(string); ok {
							reply.WriteString(txt)
						}
					}
				}
			}
			if reply.Len() == 0 {
				return compat.Failed("no assistant reply was recorded for the send that should have carried the instruction")
			}
			if !strings.Contains(reply.String(), token) {
				text := strings.Join(strings.Fields(reply.String()), " ")
				if len([]rune(text)) > 120 {
					text = string([]rune(text)[:120]) + "…"
				}
				return compat.Failed(fmt.Sprintf("the reply does not carry the token the system-prompt file asked for, so --append-system-prompt-file was not honoured (or its text was ignored); the reply reads %q", text))
			}
			return compat.Passed("the reply carries the token the appended system-prompt file asked for")
		}},

		compat.Check{ID: "D2", Probes: []string{"send.short"}, Eval: func() compat.Verdict {
			short := h.sendOf("short")
			if v, skip := unranSend(short); skip {
				return v
			}
			sm := short.samples
			if len(sm) == 0 {
				return compat.Failed("no reading of the record's status was possible while the turn ran")
			}
			var seq []string
			var last float64
			advanced := true
			for _, x := range sm {
				if len(seq) == 0 || seq[len(seq)-1] != x.status {
					seq = append(seq, x.status)
					if last != 0 && x.updatedAt <= last {
						advanced = false
					}
				}
				last = x.updatedAt
			}
			sawBusy := false
			for _, st := range seq[1:] {
				if st != "idle" {
					sawBusy = true
				}
			}
			if seq[0] != "idle" || !sawBusy || seq[len(seq)-1] != "idle" {
				return compat.Failed(fmt.Sprintf("the record's status did not go idle, busy, idle while a turn ran (saw %v)", seq))
			}
			if !advanced {
				return compat.Failed(fmt.Sprintf("statusUpdatedAt did not advance at each transition (status %v)", seq))
			}
			return compat.Passed(fmt.Sprintf("status moved %v while a turn ran, and statusUpdatedAt advanced at each transition", seq))
		}},
	)
}

// firstConfirmation says how the first send — the one before any transcript
// existed — was confirmed, from its own receipt.
func firstConfirmation(warm *compatSend) string {
	switch {
	case !warm.ran:
		return "was not sent"
	case strings.Contains(warm.rcpt.Reason, transcriptConfirmed):
		return "was confirmed by the transcript too"
	case strings.Contains(warm.rcpt.Reason, "fell back to the screen-based signal"):
		return "was confirmed by the screen, as expected"
	}
	return "was confirmed as " + string(warm.rcpt.Outcome)
}
