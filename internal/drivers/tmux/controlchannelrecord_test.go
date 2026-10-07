package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// writeControlDisconnectFixture writes a minimal JSONL record file for
// latestControlDisconnect tests, the same low-level shape
// writeAPIErrorFixture (#56) already uses for latestAPIError: each line is a
// raw JSON object the test controls directly.
func writeControlDisconnectFixture(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "record.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing fixture record: %v", err)
	}
	return path
}

const terminalDisconnectLine = `{"type":"system","subtype":"informational","timestamp":"2026-08-19T13:40:00.123Z","content":"Remote Control disconnected — this session was ended or archived from another device or app (code 4090)"}`

const transientDisconnectLine = `{"type":"system","subtype":"informational","timestamp":"2026-08-19T13:45:00.000Z","content":"Remote Control disconnected — Couldn't reconnect after 3 attempts. Retry, or start a fresh session without --resume."}`

// The load-bearing fixture (#69): the SAME phrase, but landing in a
// user-role entry holding captured command output — the shape an agent's
// own actions (grepping a pane, say) would populate. This must never be
// read as the runtime's own report.
const userCapturedOutputLine = `{"type":"user","timestamp":"2026-08-19T13:39:00.000Z","message":{"role":"user","content":[{"type":"tool_result","content":"<local-command-stdout>\nfound: Remote Control disconnected — code 4090\n</local-command-stdout>"}]}}`

const unrelatedInformationalLine = `{"type":"system","subtype":"turn_duration","timestamp":"2026-08-19T13:40:00.130Z"}`

func TestLatestControlDisconnect_ReadsTheRuntimesOwnReason(t *testing.T) {
	path := writeControlDisconnectFixture(t, []string{terminalDisconnectLine})
	fact, ok := latestControlDisconnect(path)
	if !ok {
		t.Fatal("verdict = unavailable, want a match")
	}
	if !strings.Contains(fact.text, "Remote Control disconnected") {
		t.Errorf("text = %q, missing the runtime's own opening words", fact.text)
	}
	wantAt := time.Date(2026, 8, 19, 13, 40, 0, 123_000_000, time.UTC)
	if !fact.at.Equal(wantAt) {
		t.Errorf("at = %v, want %v", fact.at, wantAt)
	}
}

// THE test: a phrase match across entry types would reintroduce the exact
// forgeability the footer-only rule (#48) exists to prevent. This record has
// NO system/informational entry naming a disconnection — only a user-role
// entry holding captured command output that happens to contain the same
// words — and must yield no match at all.
func TestLatestControlDisconnect_UserRoleCapturedOutputDoesNotMatch(t *testing.T) {
	path := writeControlDisconnectFixture(t, []string{userCapturedOutputLine})
	if fact, ok := latestControlDisconnect(path); ok {
		t.Errorf("matched a user-role entry as the runtime's own report: %+v", fact)
	}
}

// The same point, with a REAL disconnect notice also present: the user-role
// entry must not shadow it, and must not be preferred over it either — the
// type/subtype filter, not recency alone, decides what counts.
func TestLatestControlDisconnect_UserRoleEntryIsSkippedEvenAlongsideARealOne(t *testing.T) {
	path := writeControlDisconnectFixture(t, []string{
		terminalDisconnectLine,
		userCapturedOutputLine, // appended after — closer to EOF, scanned first
	})
	fact, ok := latestControlDisconnect(path)
	if !ok {
		t.Fatal("expected the real system/informational entry to still be found")
	}
	if !strings.Contains(fact.text, "code 4090") {
		t.Errorf("text = %q, want the real notice's own words, not the captured output", fact.text)
	}
}

func TestLatestControlDisconnect_KeepsTheCloseCodeUncut(t *testing.T) {
	path := writeControlDisconnectFixture(t, []string{terminalDisconnectLine})
	fact, ok := latestControlDisconnect(path)
	if !ok {
		t.Fatal("expected a match")
	}
	if got := fact.reasonText(); !strings.Contains(got, "code 4090") {
		t.Errorf("reasonText() = %q, lost the close code a sentence-cut would have thrown away", got)
	}
}

func TestLatestControlDisconnect_KeepsTheRetryInstructionUncut(t *testing.T) {
	path := writeControlDisconnectFixture(t, []string{transientDisconnectLine})
	fact, ok := latestControlDisconnect(path)
	if !ok {
		t.Fatal("expected a match")
	}
	if got := fact.reasonText(); !strings.Contains(got, "Retry") {
		t.Errorf("reasonText() = %q, lost the retry instruction a sentence-cut would have thrown away", got)
	}
}

func TestLatestControlDisconnect_MostRecentEntryWins(t *testing.T) {
	path := writeControlDisconnectFixture(t, []string{terminalDisconnectLine, transientDisconnectLine})
	fact, ok := latestControlDisconnect(path)
	if !ok {
		t.Fatal("expected a match")
	}
	if !strings.Contains(fact.text, "Couldn't reconnect") {
		t.Errorf("text = %q, want the LATER entry, not the earlier one it superseded", fact.text)
	}
}

func TestLatestControlDisconnect_NoMatchingEntryIsUnavailable(t *testing.T) {
	path := writeControlDisconnectFixture(t, []string{unrelatedInformationalLine})
	if _, ok := latestControlDisconnect(path); ok {
		t.Error("matched an informational entry unrelated to a disconnection")
	}
}

func TestLatestControlDisconnect_MissingFileIsUnavailable(t *testing.T) {
	if _, ok := latestControlDisconnect(filepath.Join(t.TempDir(), "does-not-exist.jsonl")); ok {
		t.Error("expected unavailable for a missing file")
	}
}

// A torn line at the tail (the runtime caught mid-write) must not stop the
// scan from finding the real, complete entry just before it — the same
// allowance latestAPIError's own test pins.
func TestLatestControlDisconnect_TornTrailingLineIsSkipped(t *testing.T) {
	path := writeControlDisconnectFixture(t, []string{
		terminalDisconnectLine,
		`{"type":"system","subtype":"informat`,
	})
	fact, ok := latestControlDisconnect(path)
	if !ok {
		t.Fatal("expected the earlier complete entry to still be found")
	}
	if !strings.Contains(fact.text, "code 4090") {
		t.Errorf("text = %q", fact.text)
	}
}

// ---- Driver-level wiring: List and State (#69, the same split #56 made
// between recordFactFor and upgradeLastTurnFromRecord) ----

// footerWithControlState builds a pane whose footer reports the given
// control-channel label, the same shape controlchannel_test.go's
// paneWithFooter uses, reused here rather than duplicated.
func footerWithControlState(label, extra string) string {
	return paneWithFooter("                    /rc " + label + extra)
}

func TestListSurfacesControlChannelReasonFromTheRecord(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	f.captures["%1"] = footerWithControlState("failed", "")

	root := t.TempDir()
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		map[string]any{
			"type":      "system",
			"subtype":   "informational",
			"sessionId": "conv-alpha",
			"timestamp": sessionStart.Add(5 * time.Minute).UTC().Format(time.RFC3339Nano),
			"content":   "Remote Control disconnected — this session was ended or archived from another device or app (code 4090)",
		})

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(1 * time.Hour) }),
		WithRecordRoot(root))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.ControlChannel == nil || alpha.State.ControlChannel.State != fleet.ControlChannelFailed {
		t.Fatalf("ControlChannel = %+v, want state=failed", alpha.State.ControlChannel)
	}
	if !strings.Contains(alpha.State.ControlChannel.Reason, "code 4090") {
		t.Errorf("Reason = %q, want the runtime's own record-sourced words", alpha.State.ControlChannel.Reason)
	}
}

// The load-bearing property, exercised end to end: a record containing the
// phrase only in a user-role/captured-output entry must yield NO reason,
// even though the footer independently reports the channel failed.
func TestListDoesNotSurfaceAReasonFoundOnlyInUserRoleOutput(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	f.captures["%1"] = footerWithControlState("failed", "")

	root := t.TempDir()
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		map[string]any{
			"type":      "user",
			"sessionId": "conv-alpha",
			"timestamp": sessionStart.Add(5 * time.Minute).UTC().Format(time.RFC3339Nano),
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "content": "<local-command-stdout>\nfound: Remote Control disconnected — code 4090\n</local-command-stdout>"},
				},
			},
		})

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(1 * time.Hour) }),
		WithRecordRoot(root))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.ControlChannel == nil || alpha.State.ControlChannel.State != fleet.ControlChannelFailed {
		t.Fatalf("ControlChannel = %+v, want state=failed (unchanged by this)", alpha.State.ControlChannel)
	}
	if alpha.State.ControlChannel.Reason != "" {
		t.Errorf("Reason = %q, want empty — the only candidate was a user-role entry, "+
			"which must never be read as the runtime's own report", alpha.State.ControlChannel.Reason)
	}
}

// A channel that is not Failed must never grow a Reason, even if a record
// happens to carry a disconnect notice from some earlier, already-recovered
// episode — the field explains a CURRENT failure, and the footer is what
// decides there is one (#69's own scope note).
func TestListDoesNotAttachAReasonToAHealthyChannel(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	f.captures["%1"] = footerWithControlState("active", "")

	root := t.TempDir()
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		map[string]any{
			"type":      "system",
			"subtype":   "informational",
			"sessionId": "conv-alpha",
			"timestamp": sessionStart.Add(5 * time.Minute).UTC().Format(time.RFC3339Nano),
			"content":   "Remote Control disconnected — this session was ended or archived from another device or app (code 4090)",
		})

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(1 * time.Hour) }),
		WithRecordRoot(root))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.ControlChannel == nil || alpha.State.ControlChannel.State != fleet.ControlChannelActive {
		t.Fatalf("ControlChannel = %+v, want state=active", alpha.State.ControlChannel)
	}
	if alpha.State.ControlChannel.Reason != "" {
		t.Errorf("Reason = %q, want empty on a healthy channel", alpha.State.ControlChannel.Reason)
	}
}

// No record store configured at all: Reason stays empty, never a guess —
// the same fallback discipline recordFactFor's own callers already hold to.
func TestListLeavesReasonEmptyWithNoRecordStoreConfigured(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	f.captures["%1"] = footerWithControlState("failed", "")
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.ControlChannel == nil || alpha.State.ControlChannel.State != fleet.ControlChannelFailed {
		t.Fatalf("ControlChannel = %+v, want state=failed", alpha.State.ControlChannel)
	}
	if alpha.State.ControlChannel.Reason != "" {
		t.Errorf("Reason = %q, want empty with no record store configured", alpha.State.ControlChannel.Reason)
	}
}

// State (the single-session, targeted read) applies the same upgrade as
// List — it has no pre-resolved Conversation to reuse, so it must do its
// own lookup rather than silently skip it (the same parity #56's own test
// pins for LastTurn).
func TestStateAppliesTheSameControlChannelUpgradeAsList(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	f.captures["%1"] = footerWithControlState("failed", "")

	root := t.TempDir()
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		map[string]any{
			"type":      "system",
			"subtype":   "informational",
			"sessionId": "conv-alpha",
			"timestamp": sessionStart.Add(5 * time.Minute).UTC().Format(time.RFC3339Nano),
			"content":   "Remote Control disconnected — this session was ended or archived from another device or app (code 4090)",
		})

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(1 * time.Hour) }),
		WithRecordRoot(root))

	st, err := d.State(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if st.ControlChannel == nil || st.ControlChannel.State != fleet.ControlChannelFailed {
		t.Fatalf("ControlChannel = %+v, want state=failed", st.ControlChannel)
	}
	if !strings.Contains(st.ControlChannel.Reason, "code 4090") {
		t.Errorf("Reason = %q, State did not apply the same record upgrade List does", st.ControlChannel.Reason)
	}
}

// ---- #270: a channel the footer cannot show, read from the record ----------

func recordOnlyNoticeDriver(t *testing.T, now time.Time) (*Driver, *fakeMux) {
	t.Helper()
	f := twoSessions()
	f.captures["%1"] = paneWithFooter("") // a runtime that draws no label in the footer
	root := t.TempDir()
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		map[string]any{
			"type":      "system",
			"subtype":   "informational",
			"sessionId": "conv-alpha",
			"timestamp": sessionStart.Add(5 * time.Minute).UTC().Format(time.RFC3339Nano),
			"content":   "Remote Control disconnected — Session creation failed — see debug log",
		})
	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return now }),
		WithRecordRoot(root))
	return d, f
}

// With no footer label, a notice that stood past the settle window reads failed
// in List AND State, with the runtime's own words as Reason (State's reason
// upgrade runs before the record decides, so resolve must set it itself).
func TestRecordOnlyNoticeReadsFailedWithReasonInListAndState(t *testing.T) {
	ctx := context.Background()
	d, _ := recordOnlyNoticeDriver(t, sessionStart.Add(5*time.Minute+controlNoticeSettle))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if ch := alpha.State.ControlChannel; ch == nil || ch.State != fleet.ControlChannelFailed || !strings.Contains(ch.Reason, "Session creation failed") {
		t.Fatalf("List ControlChannel = %+v, want failed with the notice as Reason", ch)
	}
	st, err := d.State(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if ch := st.ControlChannel; ch == nil || ch.State != fleet.ControlChannelFailed || !strings.Contains(ch.Reason, "Session creation failed") {
		t.Fatalf("State ControlChannel = %+v, want failed with the notice as Reason", ch)
	}
}

// Inside the window the runtime may be reconnecting by itself: no claim.
func TestRecordOnlyYoungNoticeMakesNoClaim(t *testing.T) {
	ctx := context.Background()
	d, _ := recordOnlyNoticeDriver(t, sessionStart.Add(5*time.Minute+time.Minute))
	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if ch := sessionOf(t, col, "alpha💬").State.ControlChannel; ch != nil {
		t.Fatalf("ControlChannel = %+v, want none inside the settle window", ch)
	}
}
