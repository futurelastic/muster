package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAPIErrorFixture writes a minimal JSONL record file for latestAPIError tests.
// Each line is a raw JSON object the test controls directly, rather than
// going through apiErrorRecordEntry, so a test can also exercise shapes the
// struct does not decode (admin lines with no "type", for instance).
func writeAPIErrorFixture(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "record.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing fixture record: %v", err)
	}
	return path
}

const rateLimitLine = `{"type":"assistant","timestamp":"2026-08-19T13:40:00.123Z","message":{"content":[{"type":"text","text":"You've hit your session limit · resets 9:50pm (Asia/Saigon)"}]},"error":"rate_limit","isApiErrorMessage":true,"apiErrorStatus":429}`

const serverErrorRetryableLine = `{"type":"assistant","timestamp":"2026-08-19T13:41:00.000Z","message":{"content":[{"type":"text","text":"API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment. If it persists, check https://status.claude.com."}]},"error":"server_error","isApiErrorMessage":true,"apiErrorStatus":529}`

const authFailedLine = `{"type":"assistant","timestamp":"2026-08-19T13:42:00.000Z","message":{"content":[{"type":"text","text":"Not logged in · Please run /login"}]},"error":"authentication_failed","isApiErrorMessage":true}`

const cleanAssistantLine = `{"type":"assistant","timestamp":"2026-08-19T13:43:00.000Z","message":{"content":[{"type":"text","text":"Done."}]}}`

const adminLine = `{"type":"system","subtype":"turn_duration","timestamp":"2026-08-19T13:40:00.130Z"}`

func TestLatestAPIError_RateLimitIsAccountBlock(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{rateLimitLine, adminLine})
	fact, verdict := latestAPIError(path)
	if verdict != recordAPIError {
		t.Fatalf("verdict = %v, want recordAPIError", verdict)
	}
	if fact.category != "rate_limit" {
		t.Fatalf("category = %q, want rate_limit", fact.category)
	}
	wantAt := time.Date(2026, 8, 19, 13, 40, 0, 123_000_000, time.UTC)
	if !fact.at.Equal(wantAt) {
		t.Fatalf("at = %v, want %v", fact.at, wantAt)
	}
	if fact.resetHintText() != "9:50pm (asia/saigon)" {
		t.Fatalf("resetHintText() = %q", fact.resetHintText())
	}
}

func TestLatestAPIError_ServerErrorIsRetryableFromTheSameWords(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{serverErrorRetryableLine})
	fact, verdict := latestAPIError(path)
	if verdict != recordAPIError {
		t.Fatalf("verdict = %v, want recordAPIError", verdict)
	}
	if fact.category != "server_error" {
		t.Fatalf("category = %q", fact.category)
	}
	if !fact.retryable() {
		t.Fatalf("expected fact.retryable() true for %q", fact.text)
	}
	if got := fact.reasonSentence(); got != "API Error: 529 Overloaded" {
		t.Fatalf("reasonSentence() = %q", got)
	}
}

func TestLatestAPIError_AuthFailureIsNotRetryable(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{authFailedLine})
	fact, verdict := latestAPIError(path)
	if verdict != recordAPIError {
		t.Fatalf("verdict = %v, want recordAPIError", verdict)
	}
	if fact.retryable() {
		t.Fatalf("expected fact.retryable() false for %q", fact.text)
	}
}

// A newer clean turn after an old refusal means the account is not blocked
// NOW — the durable "coming out" signal #56 asks for, independent of
// whatever the screen still shows.
func TestLatestAPIError_LaterCleanTurnSupersedesEarlierRefusal(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{rateLimitLine, adminLine, cleanAssistantLine})
	_, verdict := latestAPIError(path)
	if verdict != recordCleanTurn {
		t.Fatalf("verdict = %v, want recordCleanTurn", verdict)
	}
}

func TestLatestAPIError_NoAssistantEntryIsUnavailable(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{adminLine})
	_, verdict := latestAPIError(path)
	if verdict != recordUnavailable {
		t.Fatalf("verdict = %v, want recordUnavailable", verdict)
	}
}

func TestLatestAPIError_MissingFileIsUnavailable(t *testing.T) {
	_, verdict := latestAPIError(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if verdict != recordUnavailable {
		t.Fatalf("verdict = %v, want recordUnavailable", verdict)
	}
}

// A torn line at the very tail (the runtime caught mid-write) must not stop
// the scan from finding the real, complete entry just before it.
func TestLatestAPIError_TornTrailingLineIsSkipped(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{rateLimitLine, `{"type":"assistant","isApiErrorM`})
	fact, verdict := latestAPIError(path)
	if verdict != recordAPIError || fact.category != "rate_limit" {
		t.Fatalf("verdict = %v, fact = %+v, want recordAPIError/rate_limit", verdict, fact)
	}
}

// The tail window bounds how far back this driver will look. A file whose
// final assistant entry falls entirely before that window is honestly
// unresolvable, never silently wrong.
func TestLatestAPIError_BeyondTailWindowIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.WriteString(rateLimitLine + "\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Pad well past recordTailBytes with admin lines carrying no assistant
	// entry, so the real fact falls out of the tail window.
	padLine := adminLine + "\n"
	for i := 0; i < (recordTailBytes/len(padLine))+10; i++ {
		if _, err := f.WriteString(padLine); err != nil {
			t.Fatalf("write pad: %v", err)
		}
	}
	f.Close()

	_, verdict := latestAPIError(path)
	if verdict != recordUnavailable {
		t.Fatalf("verdict = %v, want recordUnavailable", verdict)
	}
}

// turnLine builds a turn_duration marker line for turnsSince tests — the
// same shape adminLine already uses above, parameterised on timestamp.
func turnLine(ts string) string {
	return `{"type":"system","subtype":"turn_duration","timestamp":"` + ts + `"}`
}

// muster #111: turnsSince counts turn_duration markers strictly after
// `since`, ignoring anything at or before it.
func TestTurnsSince_CountsOnlyMarkersAfterTheDelivery(t *testing.T) {
	since := time.Date(2026, 8, 19, 13, 40, 0, 0, time.UTC)
	path := writeAPIErrorFixture(t, []string{
		turnLine("2026-08-19T13:39:00.000Z"), // before `since`: not counted
		turnLine("2026-08-19T13:40:00.000Z"), // AT `since`: not counted (After, not AtOrAfter)
		turnLine("2026-08-19T13:41:00.000Z"), // after: counted
		turnLine("2026-08-19T13:42:00.000Z"), // after: counted
		rateLimitLine,                        // not a turn_duration entry: ignored either way
	})
	n, ok := turnsSince(path, since)
	if !ok {
		t.Fatal("expected ok=true: the window reaches a line at `since`")
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
}

// Zero completed turns since delivery is a POSITIVE finding (§5.7) — the
// exact "prompt never landed" smoking gun #111 exists to make visible — and
// must come back ok=true, not be confused with "could not tell".
func TestTurnsSince_ZeroIsAPositiveFinding(t *testing.T) {
	since := time.Date(2026, 8, 19, 13, 40, 0, 0, time.UTC)
	path := writeAPIErrorFixture(t, []string{
		turnLine("2026-08-19T13:39:00.000Z"), // only evidence is BEFORE since
	})
	n, ok := turnsSince(path, since)
	if !ok {
		t.Fatal("expected ok=true: a line at/before `since` proves the window reaches back far enough")
	}
	if n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
}

// The honest-absence rule this field's whole safety argument rests on: a
// window that cannot be SHOWN to reach back past `since` must answer
// ok=false, never a guessed 0 — reporting 0 here would manufacture "the
// prompt never landed" out of a read failure instead of an honest unknown.
func TestTurnsSince_TornWindowNotReachingSinceIsHonestlyUnresolvable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A delivery long before anything in the padded tail.
	since := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	// Pad well past recordTailBytes with turn_duration markers dated AFTER
	// `since` — so if this bug regressed to counting them anyway, the
	// count would come back nonzero instead of ok=false, making the
	// regression loud rather than silent.
	padLine := turnLine("2026-08-19T23:00:00.000Z") + "\n"
	for i := 0; i < (recordTailBytes/len(padLine))+10; i++ {
		if _, err := f.WriteString(padLine); err != nil {
			t.Fatalf("write pad: %v", err)
		}
	}
	f.Close()

	n, ok := turnsSince(path, since)
	if ok {
		t.Fatalf("expected ok=false: the tail window cannot be shown to reach back to %v, got count=%d", since, n)
	}
}

func TestTurnsSince_MissingFileIsUnresolvable(t *testing.T) {
	_, ok := turnsSince(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), time.Now())
	if ok {
		t.Fatal("expected ok=false for a record that does not exist")
	}
}

// muster #142: turnsSinceOffset is the incremental counterpart to
// turnsSince, used once a checkpoint (deliveryMark.Size) has already been
// established by a prior successful count. Unlike turnsSince it needs no
// honesty-window proof against a timestamp — `from` IS the boundary already
// vouched for by that earlier count, so reading forward from it to EOF is
// provably complete however large the file has grown since. This is what
// lets turnsFor keep advancing once a session's record has grown past
// recordTailBytes since its delivery mark, which is exactly the case
// turnsSince's own window-honesty rule (by design, see
// TestTurnsSince_TornWindowNotReachingSinceIsHonestlyUnresolvable) cannot
// resolve on its own.
func TestTurnsSinceOffset_CountsMarkersAfterTheOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.jsonl")
	if err := os.WriteFile(path, []byte(turnLine("2026-08-19T13:39:00.000Z")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := info.Size()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(turnLine("2026-08-19T13:41:00.000Z") + "\n" +
		turnLine("2026-08-19T13:42:00.000Z") + "\n" +
		rateLimitLine + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	n, ok := turnsSinceOffset(path, checkpoint)
	if !ok {
		t.Fatal("expected ok=true: reading forward from an already-vouched-for offset needs no window proof")
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2 (the pre-checkpoint marker must not be recounted)", n)
	}
}

// Nothing appended since the checkpoint is a positive zero, the same §5.7
// discipline turnsSince's own zero test pins.
func TestTurnsSinceOffset_UnchangedSizeIsAPositiveZero(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{turnLine("2026-08-19T13:39:00.000Z")})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := turnsSinceOffset(path, info.Size())
	if !ok {
		t.Fatal("expected ok=true: the file is exactly as large as the checkpoint, nothing to prove")
	}
	if n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
}

// A file SMALLER than the checkpoint means the checkpoint no longer refers
// to anything real (truncated or rotated underneath this driver) — honestly
// unresolvable, never a guessed count, exactly like every other absence in
// this file.
func TestTurnsSinceOffset_TruncatedFileIsUnresolvable(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{turnLine("2026-08-19T13:39:00.000Z")})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := turnsSinceOffset(path, info.Size()+1000)
	if ok {
		t.Fatal("expected ok=false: the checkpoint offset is past the current file size")
	}
}

func TestTurnsSinceOffset_MissingFileIsUnresolvable(t *testing.T) {
	_, ok := turnsSinceOffset(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), 0)
	if ok {
		t.Fatal("expected ok=false for a record that does not exist")
	}
}

// #489: latestServedEntry is latestAPIError for "did the account serve a turn,
// and when". The one difference is an entry the runtime composed itself that is
// not a refusal.
const syntheticAssistantLine = `{"type":"assistant","timestamp":"2026-08-19T13:44:00.000Z","message":{"model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`

func TestLatestServedEntry_CleanTurnCarriesItsOwnTime(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{rateLimitLine, cleanAssistantLine})
	fact, verdict := latestServedEntry(path)
	if verdict != recordCleanTurn {
		t.Fatalf("verdict = %v, want recordCleanTurn", verdict)
	}
	if want := time.Date(2026, 8, 19, 13, 43, 0, 0, time.UTC); !fact.at.Equal(want) {
		t.Fatalf("at = %v, want %v", fact.at, want)
	}
}

func TestLatestServedEntry_AnEntryTheRuntimeComposedDoesNotSupersedeARefusal(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{rateLimitLine, syntheticAssistantLine})
	if _, verdict := latestServedEntry(path); verdict != recordAPIError {
		t.Fatalf("verdict = %v, want recordAPIError: a placeholder after a refusal is not the account serving", verdict)
	}
	// latestAPIError keeps reading it as the newest assistant entry, as it
	// always has: the LastTurn upgrade is not what #489 changes.
	if _, verdict := latestAPIError(path); verdict != recordCleanTurn {
		t.Fatalf("latestAPIError verdict = %v, want recordCleanTurn (unchanged)", verdict)
	}
}

func TestLatestServedEntry_OnlyPlaceholdersIsUnavailable(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{syntheticAssistantLine})
	if _, verdict := latestServedEntry(path); verdict != recordUnavailable {
		t.Fatalf("verdict = %v, want recordUnavailable", verdict)
	}
}

func TestLatestServedEntry_UndatedCleanTurnHasNoTime(t *testing.T) {
	path := writeAPIErrorFixture(t, []string{`{"type":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}`})
	fact, verdict := latestServedEntry(path)
	if verdict != recordCleanTurn {
		t.Fatalf("verdict = %v, want recordCleanTurn", verdict)
	}
	if !fact.at.IsZero() {
		t.Fatalf("at = %v, want the zero time: an undated turn proves nothing about when", fact.at)
	}
}
