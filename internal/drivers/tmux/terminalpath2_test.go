package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// --- sanitizeForBracketedPaste -----------------------------------------

func TestSanitizeForBracketedPasteKeepsOrdinaryTextUnchanged(t *testing.T) {
	in := "Hello, 世界! Line one.\nLine two.\tTabbed."
	if got := sanitizeForBracketedPaste(in); got != in {
		t.Errorf("sanitizeForBracketedPaste(%q) = %q, want unchanged", in, got)
	}
}

func TestSanitizeForBracketedPasteStripsEscapeAndC0Controls(t *testing.T) {
	// An embedded ESC[201~ (the bracketed-paste END marker) must not survive
	// — this is the injection this function exists to close (terminal path
	// v2, item 2a).
	in := "before\x1b[201~after"
	got := sanitizeForBracketedPaste(in)
	if strings.Contains(got, "\x1b") {
		t.Errorf("sanitizeForBracketedPaste(%q) = %q, still contains ESC", in, got)
	}
	if got != "before[201~after" {
		t.Errorf("sanitizeForBracketedPaste(%q) = %q, want ESC dropped and the rest kept", in, got)
	}
}

func TestSanitizeForBracketedPasteKeepsNewlineAndTabDropsOtherControls(t *testing.T) {
	in := "a\nb\tc\x00d\x01e\x07f\x7fg\rh"
	got := sanitizeForBracketedPaste(in)
	want := "a\nb\tcdefgh" // \r dropped too — see the function's own doc comment
	if got != want {
		t.Errorf("sanitizeForBracketedPaste(%q) = %q, want %q", in, got, want)
	}
}

// --- composerRegionMatch (D1's fix, pure-function level) ----------------

// TestComposerRegionMatchSurvivesTheTUIsOwnWordWrap is the direct regression
// test for D1: the OLD confirmLanded needle (last line, last 24 bytes of the
// SOURCE text) straddles Claude Code's own word-wrap — a hard newline plus a
// 2-space continuation indent the TUI inserts itself, which is not something
// tmux's `-J` rejoin (a DIFFERENT kind of wrap) can undo. composerRegionMatch
// must still recognise the composer as holding this exact text.
func TestComposerRegionMatchSurvivesTheTUIsOwnWordWrap(t *testing.T) {
	sent := "please look at internal/drivers/tmux/tmux.go and check the confirmation logic"
	// The TUI's own wrap: broken at the LAST space (dropped), continuation
	// indented by two more spaces — round-1's own measured shape. Built by
	// splitting exactly at the space nearest the end, so the wrap point
	// falls INSIDE the old needle's own 24-byte tail window (verified by
	// the assertion below, not merely assumed).
	wrapAt := strings.LastIndex(sent, " ")
	rendered := sent[:wrapAt] + "\n  " + sent[wrapAt+1:]

	sc := newScreen("  transcript line\n✻ Brewed for 1m 0s\n" + rule + "\n❯ " + rendered + "\n" + rule + "\n  ⏵⏵ auto mode on")

	matched, evidence := composerRegionMatch(sc, sent, true)
	if !matched {
		t.Fatalf("composerRegionMatch did not match text wrapped by the TUI's own word-wrap")
	}
	if evidence == "" {
		t.Error("composerRegionMatch reported a match with no evidence string")
	}

	// The OLD needle (last line, tail 24 bytes of the SOURCE text) would
	// straddle exactly this wrap point and fail — demonstrating this is a
	// genuine fix, not a redundant check. Confirmed here directly, not
	// inferred: this is the precise mechanism D1 names.
	needle := sent
	if idx := strings.LastIndexByte(needle, '\n'); idx >= 0 {
		needle = needle[idx+1:]
	}
	if len(needle) > 24 {
		needle = needle[len(needle)-24:]
	}
	if strings.Contains(rendered, needle) {
		t.Fatalf("test is not exercising D1: the old needle %q still matches the rendered text; "+
			"strengthen the fixture so the wrap actually straddles the tail", needle)
	}
}

// TestComposerRegionMatchRejectsTextAboveTheComposer is round-1's OTHER D1
// finding: a raw, unstructured `-S -6` capture can span more than the
// composer, so a needle can match transcript text sitting ABOVE it. A
// composer-scoped match must not.
func TestComposerRegionMatchRejectsTextAboveTheComposer(t *testing.T) {
	transcriptText := "as discussed, the deploy window closes at 5pm sharp today"
	sc := newScreen("  " + transcriptText + "\n✻ Brewed for 1m 0s\n" + rule + "\n❯\n" + rule + "\n  ⏵⏵ auto mode on")

	matched, _ := composerRegionMatch(sc, transcriptText, true)
	if matched {
		t.Fatalf("composerRegionMatch matched text that sits in the TRANSCRIPT, above the composer " +
			"— the composer here is empty; this is exactly the false positive D1 names")
	}
}

// TestComposerRegionMatchExactAfterWhitespaceAndComposingFormDifferences
// exercises normalizeForMatch's own two jobs together: NFC composing and
// whitespace-insensitivity, through the composer scan rather than in
// isolation (nfclite_test.go already covers normalizeForMatch alone).
func TestComposerRegionMatchExactAfterWhitespaceAndComposingFormDifferences(t *testing.T) {
	sentNFD := "Xin chào, đây là một câu tiếng Việt."
	renderedNFC := "Xin  chào,\n  đây là một câu tiếng Việt. " // extra spaces + wrap + trailing space
	sc := newScreen("  transcript\n✻ Brewed for 1m 0s\n" + rule + "\n❯ " + renderedNFC + "\n" + rule + "\n  ⏵⏵ auto mode on")

	matched, _ := composerRegionMatch(sc, sentNFD, true)
	if !matched {
		t.Fatalf("composerRegionMatch did not match across composing form + whitespace differences")
	}
}

// TestComposerRegionMatchSuffixWhenScrolled models Claude Code's own INNER
// composer scroll (distinct from composerClipped's tmux-history-margin
// scroll, muster#169): the fence is fully visible, composerText reports
// composerFound, but only the TAIL of a very long single paste is on screen.
func TestComposerRegionMatchSuffixWhenScrolled(t *testing.T) {
	full := strings.Repeat("word ", 200) + "the last words visible after scrolling"
	visibleTail := "the last words visible after scrolling"
	sc := newScreen("  transcript\n✻ Brewed for 1m 0s\n" + rule + "\n❯ " + visibleTail + "\n" + rule + "\n  ⏵⏵ auto mode on")

	matched, evidence := composerRegionMatch(sc, full, true)
	if !matched {
		t.Fatalf("composerRegionMatch did not accept a substantial suffix of a scrolled composer")
	}
	if !strings.Contains(evidence, "suffix") {
		t.Errorf("evidence = %q, want it to name the suffix case", evidence)
	}
}

func TestComposerRegionMatchRejectsATrivialCoincidentalSuffix(t *testing.T) {
	sent := "deploy the staging branch now please."
	sc := newScreen("  transcript\n✻ Brewed for 1m 0s\n" + rule + "\n❯ please.\n" + rule + "\n  ⏵⏵ auto mode on")

	matched, _ := composerRegionMatch(sc, sent, true)
	if matched {
		t.Fatal("a short, coincidental trailing match (\"please.\") must not pass the suffix rule")
	}
}

func TestComposerRegionMatchEmptyComposerNeverMatches(t *testing.T) {
	sc := newScreen(idleFixtureFor("alpha"))
	matched, _ := composerRegionMatch(sc, "anything at all", true)
	if matched {
		t.Fatal("an empty composer must never match")
	}
}

// --- confirmLandedV2, driver-level: which signal actually decided --------

// TestConfirmLandedV2UsesComposerRegionMatchAsThePrimarySignal proves the
// NEW structural check is what confirms a properly-rendered composer,
// independent of the legacy needle fallback confirmLandedV2 also carries —
// asserted by checking the COUNTER the primary path increments, not merely
// the boolean result (which the fallback could also have produced).
func TestConfirmLandedV2UsesComposerRegionMatchAsThePrimarySignal(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	text := "properly rendered text"
	f.setCapture("%1", composerHolding(text))

	_, _, landed := d.confirmLandedV2(context.Background(), "%1", text, map[pasteKey]int{}, false, false)
	if !landed {
		t.Fatal("confirmLandedV2 did not confirm a composer holding exactly the sent text")
	}
	snap := d.counters.Snapshot()
	if snap[counterLandConfirmByComposerMatch] != 1 {
		t.Errorf("counterLandConfirmByComposerMatch = %d, want 1 (this call should have been decided "+
			"by the structural composer-region match, not the legacy needle fallback)", snap[counterLandConfirmByComposerMatch])
	}
	if snap[counterLandConfirmByLegacyNeedle] != 0 {
		t.Errorf("counterLandConfirmByLegacyNeedle = %d, want 0", snap[counterLandConfirmByLegacyNeedle])
	}
}

// TestLandedIsNeverConfirmedByTextOutsideTheComposer (#180 H2): text a pane
// shows below the composer's closing rule — a shell echoing a command under
// a composer frame the runtime left behind when it exited — must never read
// as this delivery having landed, whatever it says.
func TestLandedIsNeverConfirmedByTextOutsideTheComposer(t *testing.T) {
	f := twoSessions()
	f.echoOutsideFence = true
	d := newTestDriver(f)
	text := "touch a-file-that-must-not-be-created"
	if err := d.pasteBracketed(context.Background(), "%1", text); err != nil {
		t.Fatalf("pasteBracketed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, landed := d.confirmLandedV2(ctx, "%1", text, map[pasteKey]int{}, false, false); landed {
		t.Fatal("text echoed below the composer's closing rule was taken as landed")
	}
}

// TestConfirmLandedV2NeedleReadsOnlyTheComposersRows: the tail needle is
// still a fallback for a composer that renders the text in a shape the row
// model does not follow, but only inside the composer's own rows.
func TestConfirmLandedV2NeedleReadsOnlyTheComposersRows(t *testing.T) {
	text := "an opening line the composer shows differently\nand the closing line of the message"
	f := twoSessions()
	f.setCapture("%1", "  transcript\n"+rule+"\n❯ [Image #1] an opening line, re-rendered\n  and the closing line of the message\n"+rule+"\n  status")
	d := newTestDriver(f)
	if _, _, landed := d.confirmLandedV2(context.Background(), "%1", text, map[pasteKey]int{}, false, false); !landed {
		t.Fatal("the needle inside the composer's own rows did not confirm")
	}
	if n := d.counters.Snapshot()[counterLandConfirmByLegacyNeedle]; n != 1 {
		t.Fatalf("counterLandConfirmByLegacyNeedle = %d, want 1", n)
	}

	f2 := twoSessions()
	f2.setCapture("%1", "  and the closing line of the message\n"+rule+"\n❯ something else entirely\n"+rule+"\n  status")
	d2 := newTestDriver(f2)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, landed := d2.confirmLandedV2(ctx, "%1", text, map[pasteKey]int{}, false, false); landed {
		t.Fatal("the needle matched transcript text above the composer")
	}
}

// --- pasteBracketed / bracketPasteFlag, driver-level ---------------------

func TestPasteBracketedRefusesWhenFlagIsOff(t *testing.T) {
	f := twoSessions()
	f.setBracketPasteOff("%1", true)
	d := newTestDriver(f)

	err := d.pasteBracketed(context.Background(), "%1", "hello")
	if err == nil {
		t.Fatal("pasteBracketed did not refuse a pane with bracket_paste_flag=0")
	}
	if !strings.Contains(err.Error(), "bracketed paste mode") {
		t.Errorf("error = %q, want it to name bracketed paste mode", err.Error())
	}
	snap := d.counters.Snapshot()
	if snap[counterBracketPasteFlagOff] != 1 {
		t.Errorf("counterBracketPasteFlagOff = %d, want 1", snap[counterBracketPasteFlagOff])
	}
}

// #333: a tmux older than 3.7 prints an empty line for a format variable it does
// not know. That is not the "0" of a pane that declined bracketed paste, and the
// refusal must say which multiplexer is at fault instead of claiming the flag
// reads 0.
func TestPasteBracketedNamesAnUnsupportedFlagApartFromAnOffOne(t *testing.T) {
	f := twoSessions()
	f.mu.Lock()
	f.bracketPasteUnsupported = true
	f.mu.Unlock()
	d := newTestDriver(f)

	err := d.pasteBracketed(context.Background(), "%1", "hello")
	if err == nil {
		t.Fatal("pasteBracketed delivered through a tmux that cannot report the flag")
	}
	if !errors.Is(err, errBracketPasteUnavailable) {
		t.Errorf("error %v does not wrap errBracketPasteUnavailable, so Send cannot classify it", err)
	}
	for _, want := range []string{"tmux 3.4", "reads empty", "3.7", "FLEET_TMUX_BIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "reads 0") {
		t.Errorf("error = %q still blames a pane that reads 0", err.Error())
	}
	snap := d.counters.Snapshot()
	if snap[counterBracketPasteFlagUnsupported] != 1 || snap[counterBracketPasteFlagOff] != 0 {
		t.Errorf("counters unsupported=%d off=%d, want 1 and 0", snap[counterBracketPasteFlagUnsupported], snap[counterBracketPasteFlagOff])
	}
	if f.pasted["%1"] != "" {
		t.Errorf("a paste reached the pane despite the refusal")
	}
}

func TestPasteBracketedSucceedsAndDeliversLiteralNewlinesWhenFlagIsOn(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	text := "line one\nline two\nline three"
	if err := d.pasteBracketed(context.Background(), "%1", text); err != nil {
		t.Fatalf("pasteBracketed: %v", err)
	}
	if f.pasted["%1"] != text {
		t.Errorf("pasted content = %q, want %q (literal newlines preserved, no CR conversion at this layer)",
			f.pasted["%1"], text)
	}

	// The invocation must ask for -p (bracket) and -r (literal newline).
	calls := f.callsSnapshot()
	found := false
	for _, c := range calls {
		hasPaste, hasP, hasR := false, false, false
		for _, a := range c {
			switch a {
			case "paste-buffer":
				hasPaste = true
			case "-p":
				hasP = true
			case "-r":
				hasR = true
			}
		}
		if hasPaste {
			found = true
			if !hasP || !hasR {
				t.Errorf("paste-buffer call %v missing -p and/or -r", c)
			}
		}
	}
	if !found {
		t.Fatal("no paste-buffer call was made at all")
	}
}

// TestSendRefusesRatherThanFallingBackWhenBracketPasteUnavailable is the
// end-to-end version of the refusal, through Send itself.
func TestSendRefusesRatherThanFallingBackWhenBracketPasteUnavailable(t *testing.T) {
	f := twoSessions()
	f.setBracketPasteOff("%1", true)
	d := newTestDriver(f)

	got, err := d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		"hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused", got.Outcome)
	}
	if f.pasted["%1"] != "" {
		t.Error("text was delivered despite the refusal — refusing must mean nothing was pasted")
	}
}

// --- inboxEligible / terminal route (D7, #184) ---------------------------

// Since #257 only a NAMED inbox (route "inbox", submitting, no resume or
// replace) is inbox-eligible; the unset route, auto and terminal never are.
func TestInboxEligibleRespectsRouteTerminal(t *testing.T) {
	base := driver.SendOptions{Submit: true}
	named := base
	named.Route = fleet.RouteInbox
	if !inboxEligible(named) {
		t.Fatal("sanity: a named inbox, submit-only send should be inbox-eligible")
	}
	for _, route := range []fleet.Route{"", fleet.RouteAuto, fleet.RouteTerminal} {
		o := base
		o.Route = route
		if inboxEligible(o) {
			t.Fatalf("route %q must not be inbox-eligible: the inbox is used only when named (#257)", route)
		}
	}
	for name, o := range map[string]driver.SendOptions{
		"no submit": {Route: fleet.RouteInbox},
		"resume":    {Submit: true, ResumeIfStranded: true, Route: fleet.RouteInbox},
		"replace":   {Submit: true, ReplaceIfStranded: true, Route: fleet.RouteInbox},
	} {
		if inboxEligible(o) {
			t.Fatalf("a named inbox with %s must not be inbox-eligible (composer-only shape)", name)
		}
	}
}

// --- Discard / confirmed-send clear the stranded record (item e) ---------

// TestDiscardForgetsAStrandedRecordItClears is item e's Discard-side fix:
// before this change, Discard never called forgetStranded at all, so a
// cleared composer could still carry a stale stranded record forever
// (subject only to strandedRetention's timed sweep).
func TestDiscardForgetsAStrandedRecordItClears(t *testing.T) {
	f := twoSessions()
	f.noEcho = true // composer never renders the paste, so it strands
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "text that stranded"

	got, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text strands", got.Outcome)
	}
	if _, ok := d.strandedRecordFor("alpha💬", "/work/alpha"); !ok {
		t.Fatal("setup: no stranded record was recorded at all")
	}

	// The composer, per noEcho, still reads as holding nothing this driver
	// can attribute — but composerText/Discard read it as genuinely EMPTY
	// (noEcho means the pane never echoes the paste back at all, which is
	// indistinguishable on screen from an empty composer). Discard's own
	// "already clear" branch is exactly what should forget the record here.
	ack, err := d.Discard(context.Background(), testCaller, ref, "", driver.DiscardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted {
		t.Fatal("Discard was not accepted")
	}
	if _, ok := d.strandedRecordFor("alpha💬", "/work/alpha"); ok {
		t.Fatal("Discard cleared the composer but the stranded record survived — item e's own fix")
	}
}

// TestConfirmedSendForgetsAStalePriorStrandedRecord is item e's OTHER half:
// a fresh, fully-confirmed send (composer was empty beforehand, landed, and
// submitted) must forget any stranded record left over from an EARLIER,
// unrelated strand for the same session — otherwise a later resumeIfStranded
// could act on residue describing a composer state that no longer exists.
func TestConfirmedSendForgetsAStalePriorStrandedRecord(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	// Plant a stale record by hand — as if an earlier strand happened and
	// was never cleared (the exact gap item e closes).
	d.noteStranded("alpha💬", "/work/alpha", "some earlier stranded text", "")

	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	got, err := d.Send(context.Background(), testCaller, ref, "a brand new message", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("setup: outcome = %s (%s), want queued (a confirmed send)", got.Outcome, got.Reason)
	}
	if _, ok := d.strandedRecordFor("alpha💬", "/work/alpha"); ok {
		t.Fatal("a confirmed send must forget a stale prior stranded record for the same session")
	}
}

// --- resumeIfStranded's new composer-digest gate (item e) -----------------

// TestResumeIfStrandedRefusesWhenComposerDigestNoLongerMatches is item e's
// own regression test, mirroring TestReplaceIfStrandedRefusesWhenComposerDigestNoLongerMatches
// (stranded_test.go) for the RESUME door instead of the REPLACE one: a
// human attaching and typing over the composer in the gap between the
// strand and the resume call must not be silently overwritten by the wake
// key.
func TestResumeIfStrandedRefusesWhenComposerDigestNoLongerMatches(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true // strand via a submit-swallowed delivery, which gives noteStranded a
	// meaningful (non-empty) ComposerDigest to record — see confirmLandedV2's
	// own test file for why this shape, as opposed to noEcho, is what
	// populates one.
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the original instruction"

	strand, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if strand.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text strands", strand.Outcome)
	}
	if rec, ok := d.strandedRecordFor("alpha💬", "/work/alpha"); !ok || rec.ComposerDigest == "" {
		t.Fatalf("setup: stranded record = %+v, ok=%v — want a record with a non-empty ComposerDigest", rec, ok)
	}
	f.swallowSubmit = false

	// Something changed the composer since the strand was recorded — a
	// human attaching and typing over it (same shape stranded_test.go's own
	// replace-side test uses).
	f.setCapture("%1", "transcript\n"+rule+"\n❯ somebody else's half-typed line\n"+rule+"\n")
	f.pasted["%1"] = ""

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s (%s), want refused — the composer's content changed since the strand", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "digest") {
		t.Errorf("reason = %q, want it to name the digest mismatch", got.Reason)
	}
	if countClears(f.callsSnapshot()) != 0 {
		t.Errorf("a digest mismatch must be refused BEFORE pressing anything, got %d clear keystrokes",
			countClears(f.callsSnapshot()))
	}
}

// TestResumeIfStrandedProceedsWhenNoDigestWasRecordable is the OTHER half:
// an empty ComposerDigest (this driver could not read a definite composer
// state at strand time — e.g. the render simply had not finished yet) must
// NOT be treated as a mismatch, or resumeIfStranded's own primary purpose
// (finish a delivery that kept landing after this driver gave up watching)
// would break. TestNoteDeliveryDoesNotMoveWhenAResumeFinishesTheSameDelivery
// and TestStrandedRecordSurvivesARestart already exercise this end to end;
// this test names the property directly.
func TestResumeIfStrandedProceedsWhenNoDigestWasRecordable(t *testing.T) {
	f := twoSessions()
	f.noEcho = true // never echoes the paste — currentComposerDigest reads "" at strand time
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "text that stranded"

	strand, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if strand.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown", strand.Outcome)
	}
	rec, ok := d.strandedRecordFor("alpha💬", "/work/alpha")
	if !ok || rec.ComposerDigest != "" {
		t.Fatalf("setup: rec = %+v, ok=%v — want a record with an EMPTY ComposerDigest for this test to mean anything", rec, ok)
	}

	f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ "+text+"\n"+rule+"\n")
	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — an empty recorded digest must not block a legitimate resume", got.Outcome, got.Reason)
	}
}
