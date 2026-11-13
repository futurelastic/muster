package tmux

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Field report muster#467: a client drew its two buttons from a state that read
// waiting_input / external-imports, the person chose the affirmative one, and
// respond refused it as "not waiting on a prompt". The state and respond are
// meant to be two views of one reading of the pane; these tests pin that they
// are, and pin what happens when the pane moved between the two captures.

var stateAgreementEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// replayDialog is a pane model built from a recorded screen: it shows the
// screen until a confirming key lands, and — when the screen's options are
// unnumbered rows it can find — moves the highlight under Up and Down the way
// the runtime's unnumbered menus do (muster#171, #205).
type replayDialog struct {
	lines []string
	rows  []int    // screen row of each option, in order
	texts []string // each option's text
	col   int      // column the option text starts at
	sel   int      // 0-based highlighted option
	done  bool
}

// newReplayDialog reads raw's options from the SAME reading the driver uses, then
// locates their rows. walk is false for a screen whose rows it cannot move a
// highlight over (numbered menus, preview panes): any key then answers it.
func newReplayDialog(t *testing.T, raw string) *replayDialog {
	t.Helper()
	g := &replayDialog{lines: strings.Split(strings.TrimRight(raw, "\n"), "\n")}
	p, shape := readPrompt(newScreen(raw))
	if p == nil || !shape.unnumbered {
		return g
	}
	g.sel = p.Selected - 1
	g.texts = p.Options
	for _, opt := range p.Options {
		row := -1
		for i := len(g.lines) - 1; i >= 0; i-- {
			body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(g.lines[i]), composerRuneMarker))
			if body == opt {
				row = i
				break
			}
		}
		if row < 0 {
			g.rows, g.texts = nil, nil
			return g
		}
		g.rows = append(g.rows, row)
	}
	// The text column is the same on every row; read it off an unmarked one.
	for i, r := range g.rows {
		if !strings.HasPrefix(strings.TrimSpace(g.lines[r]), composerRuneMarker) {
			g.col = rowIndent(g.lines[r])
			break
		}
		if i == len(g.rows)-1 {
			g.rows, g.texts = nil, nil
		}
	}
	return g
}

func (g *replayDialog) walks() bool { return len(g.rows) > 0 }

func (g *replayDialog) press(key string) {
	if g.done {
		return
	}
	switch key {
	case "Down":
		if g.walks() && g.sel < len(g.rows)-1 {
			g.sel++
		}
	case "Up":
		if g.walks() && g.sel > 0 {
			g.sel--
		}
	case "C-m", "Enter", "Escape":
		g.done = true
	default:
		if !g.walks() {
			g.done = true
		}
	}
}

func (g *replayDialog) screen() string {
	if g.done {
		return idleFixtureFor("answered")
	}
	lines := append([]string(nil), g.lines...)
	for i, r := range g.rows {
		lead := strings.Repeat(" ", max(g.col-2, 0))
		if i == g.sel {
			lines[r] = lead + composerRuneMarker + " " + g.texts[i]
		} else {
			lines[r] = lead + "  " + g.texts[i]
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// newLiveClockDriver is newTestDriver with a clock that follows real time (plus
// an optional skew a test moves forward). newTestDriver's clock is fixed in the
// past, which makes the per-call deadline Respond derives from it already
// expired; a test that waits between two reads needs a deadline that is not.
func newLiveClockDriver(run execFunc, skew *time.Duration) *Driver {
	return New("testbox",
		withExec(run),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time {
			if skew == nil {
				return time.Now()
			}
			return time.Now().Add(*skew)
		}),
	)
}

// The property the report is about, over every screen in the corpus: where the
// state reports a prompt, respond's own reading of the same screen is that same
// prompt — kind, options, nonce, the lot — and respond does not refuse it as a
// screen with nothing to answer. A corpus case a future change makes the state
// call a prompt and respond not (or the reverse) fails here by name.
func TestStateAndRespondReadTheSamePromptOnEveryCorpusScreen(t *testing.T) {
	prompts := 0
	for _, name := range corpusCaseDirs(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			c, raw := loadCorpusCase(t, name)
			// Height 0 is a capture that did not say how tall its pane was;
			// 24 is the pane a created session gets, which puts the visible-pane
			// boundary (muster#169) inside the screen for the taller cases. The
			// state and respond take their height from the same capture, so they
			// must agree at either.
			for _, height := range []int{0, 24} {
				var prior paneMemory
				for i, obs := range c.Observations {
					now := stateAgreementEpoch.Add(time.Duration(obs.AfterSeconds) * time.Second)
					capture := paneCapture{text: raw, height: height}
					st, digest := classifyCaptureRemembering(capture, true, c.Alive, obs.Young, prior, now)
					if !prior.known || prior.digest != digest {
						prior = paneMemory{known: true, digest: digest, at: now}
					}
					if st.Status != fleet.StatusWaitingInput || st.WaitingOn != fleet.WaitingPrompt || st.Prompt == nil {
						continue
					}
					prompts++
					got, _ := readPrompt(capture.screen())
					if got == nil {
						t.Errorf("height %d observation %d: the state reports a %q prompt and respond's reading of the same screen finds none\nstate prompt: %+v",
							height, i, st.Prompt.Kind, st.Prompt)
						continue
					}
					if !reflect.DeepEqual(got, st.Prompt) {
						t.Errorf("height %d observation %d: respond reads a different prompt from the one the state reports\nstate:   %+v\nrespond: %+v",
							height, i, st.Prompt, got)
					}
				}
			}
		})
	}
	if prompts == 0 {
		t.Fatal("no corpus case reports a prompt; the agreement test is checking nothing")
	}
}

// The same corpus, through Respond itself rather than its parser: the receipt
// must not be either of the two "nothing is being asked" refusals, nor the
// stale-nonce one, on a screen the state called a prompt. Cancel is the answer
// every prompt kind takes, so one request shape covers the set.
func TestRespondDoesNotRefuseAScreenTheStateCallsAPrompt(t *testing.T) {
	for _, name := range corpusCaseDirs(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			c, raw := loadCorpusCase(t, name)
			var prior paneMemory
			var prompt *fleet.SessionPrompt
			for _, obs := range c.Observations {
				now := stateAgreementEpoch.Add(time.Duration(obs.AfterSeconds) * time.Second)
				st, digest := classifyPaneRemembering(raw, true, c.Alive, obs.Young, prior, now)
				if !prior.known || prior.digest != digest {
					prior = paneMemory{known: true, digest: digest, at: now}
				}
				if st.Status == fleet.StatusWaitingInput && st.WaitingOn == fleet.WaitingPrompt && st.Prompt != nil {
					prompt = st.Prompt
				}
			}
			if prompt == nil {
				t.Skip("the state reports no prompt on this screen")
			}
			f := twoSessions()
			armDialog(f, "%1", newReplayDialog(t, raw))
			d := newTestDriver(f)
			got, err := d.Respond(context.Background(), testCaller,
				fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
				fleet.Response{Cancel: true, Nonce: prompt.Nonce})
			if err != nil {
				t.Fatal(err)
			}
			for _, nothing := range []string{"not waiting on a prompt", "no recognised prompt", "the prompt changed"} {
				if got.Outcome == fleet.OutcomeRefused && strings.Contains(got.Reason, nothing) {
					t.Errorf("respond refused a screen the state reports as a %q prompt: %s", prompt.Kind, got.Reason)
				}
			}
		})
	}
}

// fixtureExternalImportsWrapped is the external-imports question as the runtime
// paints it when its text wraps: a three-row question, and an imported file whose
// path is long, non-ASCII, holds spaces and an ampersand, and wraps over three
// rows. Synthesised, not captured (no screen was kept from the field report), from
// the measured layout of fixtureExternalImportsMenu; the path is invented and has
// no resemblance to any real one.
const fixtureExternalImportsWrapped = `
` + rule + `
  Allow external CLAUDE.md file imports?

  This project's CLAUDE.md imports files outside the current working directory. Never allow this for third-party
  repositories. The imported files can change how the agent behaves in this directory and are loaded into every
  conversation without any further review.

  External imports:
    /Users/someone/Dự án mới/Tài liệu & Ghi chú/Thư mục rất dài với tên có dấu và khoảng trắng/Bản sao & lưu trữ
    /dùng chung/日本語のフォルダ 名前 & 資料/とても長い名前のディレクトリ/quy tắc & hướng dẫn/CLAUDE chung (bản
    cuối).md

  Important: Only use Claude Code with files you trust. Accessing untrusted files may pose security risks
  https://code.claude.com/docs/en/security

  ❯ No, disable external imports
    Yes, allow external imports

  Enter to confirm · Esc to cancel
`

// The report's own screen: classified as the question it is, read the same way by
// respond, and answered — the affirmative row is walked to, the arrival read, and
// only then confirmed (the unnumbered menu's measured way, muster#171).
func TestExternalImportsOnAWrappedLongPath(t *testing.T) {
	for _, height := range []int{0, 24, 18} {
		st, _ := classifyCaptureRemembering(paneCapture{text: fixtureExternalImportsWrapped, height: height}, true, true, false,
			paneMemory{}, stateAgreementEpoch)
		if st.Status != fleet.StatusWaitingInput || st.Prompt == nil || st.Prompt.Kind != fleet.PromptExternalImports {
			t.Fatalf("height %d: state = %s, prompt = %+v; want waiting_input / external-imports", height, st.Status, st.Prompt)
		}
		got, shape := readPrompt(paneCapture{text: fixtureExternalImportsWrapped, height: height}.screen())
		if got == nil || !reflect.DeepEqual(got, st.Prompt) || !shape.unnumbered {
			t.Fatalf("height %d: respond reads %+v (unnumbered %v), the state reports %+v", height, got, shape.unnumbered, st.Prompt)
		}
	}

	f := twoSessions()
	m := newReplayDialog(t, fixtureExternalImportsWrapped)
	if !m.walks() {
		t.Fatal("the fixture's option rows were not found; the test would not move a highlight")
	}
	armDialog(f, "%1", m)
	d := newTestDriver(f)
	p, _ := readPrompt(newScreen(f.captures["%1"]))
	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.Response{Choice: 2, Nonce: p.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("respond = %q (%s), want submitted", got.Outcome, got.Reason)
	}
	if want := "[[Down] [C-m]]"; fmt.Sprint(keySends(f)) != want {
		t.Errorf("key sends = %v, want %s — no digit, an arrow to the affirmative row, then confirm", keySends(f), want)
	}
	if !strings.Contains(got.Reason, "Yes, allow external imports") {
		t.Errorf("receipt = %q; must name the option chosen", got.Reason)
	}
}

// The corpus case for the same dialog, answered through Respond with the redacted
// text the repository actually carries.
func TestRespondAnswersTheExternalImportsCorpusScreen(t *testing.T) {
	c, raw := loadCorpusCase(t, "external-imports-wrapped-long-path")
	if !c.Alive {
		t.Fatal("the case must be of a live pane")
	}
	f := twoSessions()
	m := newReplayDialog(t, raw)
	if !m.walks() {
		t.Fatal("the corpus screen's option rows were not found")
	}
	armDialog(f, "%1", m)
	d := newTestDriver(f)
	p, _ := readPrompt(newScreen(raw))
	if p == nil || p.Kind != fleet.PromptExternalImports {
		t.Fatalf("respond's reading of the corpus screen = %+v", p)
	}
	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.Response{Choice: 2, Nonce: p.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeSubmitted || m.sel != 1 {
		t.Fatalf("respond = %q (%s), highlight %d; want submitted on row 2", got.Outcome, got.Reason, m.sel+1)
	}
	if want := "[[Down] [C-m]]"; fmt.Sprint(keySends(f)) != want {
		t.Errorf("key sends = %v, want %s", keySends(f), want)
	}
}

// The state read reported a prompt; the screen has moved on by the time respond
// reads it. That is a refusal — there is nothing to answer, and a key would land
// in whatever is there now — but it must not read as "nothing is being asked":
// something was, a moment ago, and the client's buttons are stale.
func TestRespondNamesAPromptTheStateReportedThatTheScreenNoLongerHas(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = fixtureExternalImportsWrapped
	d := newLiveClockDriver(f.exec, nil)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	st, err := d.State(context.Background(), testCaller, ref)
	if err != nil {
		t.Fatal(err)
	}
	if st.Prompt == nil || st.Prompt.Kind != fleet.PromptExternalImports {
		t.Fatalf("state = %s, prompt = %+v; want the external-imports prompt", st.Status, st.Prompt)
	}

	f.captures["%1"] = idleFixtureFor("alpha") // the screen moved on
	got, err := d.Respond(context.Background(), testCaller, ref, fleet.Response{Choice: 2, Nonce: st.Prompt.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("respond = %q (%s), want refused", got.Outcome, got.Reason)
	}
	for _, want := range []string{
		"the state read of this session reported a prompt (external-imports)",
		"repeated 3 more time(s)",
		"No key was sent",
		"session is not waiting on a prompt",
	} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("refusal %q does not say %q", got.Reason, want)
		}
	}
	if len(keySends(f)) != 0 {
		t.Errorf("keys were sent to a screen with no prompt: %v", keySends(f))
	}
}

// A capture can catch the runtime partway through repainting the dialog. The
// first read finds nothing, a second one, a moment later, finds the dialog:
// respond answers it rather than refusing on the torn frame.
func TestRespondAnswersAPromptARepeatedReadFinds(t *testing.T) {
	f := twoSessions()
	m := newReplayDialog(t, fixtureExternalImportsWrapped)
	armDialog(f, "%1", m)
	rereads := 0
	torn := false
	d := newLiveClockDriver(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// captureForClassify is the only call that BEGINS with capture-pane; the
		// batched listing's captures are chained behind a list-panes.
		if len(args) > 0 && args[0] == "capture-pane" && torn {
			rereads++
			f.captures["%1"] = m.screen() // the repaint finished
			torn = false
		}
		return f.exec(ctx, name, args...)
	}, nil)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	st, err := d.State(context.Background(), testCaller, ref)
	if err != nil || st.Prompt == nil {
		t.Fatalf("state = %+v, err %v; want the prompt", st, err)
	}

	f.captures["%1"] = idleFixtureFor("alpha") // the capture that catches the repaint
	torn = true
	got, err := d.Respond(context.Background(), testCaller, ref, fleet.Response{Choice: 2, Nonce: st.Prompt.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeSubmitted || m.sel != 1 || rereads != 1 {
		t.Fatalf("respond = %q (%s), highlight row %d, re-reads %d; want submitted on row 2 after exactly one re-read",
			got.Outcome, got.Reason, m.sel+1, rereads)
	}
}

// The refusal for a screen that really holds no prompt is as it was: no re-read,
// no mention of a state read — when nothing was ever reported, and when what was
// reported is older than a client could still be acting on.
func TestRespondRefusalForAScreenWithNoPromptIsUnchanged(t *testing.T) {
	var skew time.Duration
	newDriver := func(f *fakeMux) (*Driver, *int) {
		single := 0
		d := newLiveClockDriver(func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "capture-pane" {
				single++
			}
			return f.exec(ctx, name, args...)
		}, &skew)
		return d, &single
	}
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	check := func(t *testing.T, d *Driver, single *int) {
		t.Helper()
		got, err := d.Respond(context.Background(), testCaller, ref, fleet.Response{Choice: 1})
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != fleet.OutcomeRefused || !strings.HasPrefix(got.Reason, "session is not waiting on a prompt") {
			t.Errorf("respond = %q (%s); want the plain refusal", got.Outcome, got.Reason)
		}
		if *single != 0 {
			t.Errorf("%d re-reads for a screen nobody reported a prompt on", *single)
		}
	}

	t.Run("nothing was ever reported", func(t *testing.T) {
		d, single := newDriver(twoSessions())
		check(t, d, single)
	})

	t.Run("it was reported too long ago to be what the client acts on", func(t *testing.T) {
		f := twoSessions()
		f.captures["%1"] = fixtureExternalImportsWrapped
		d, single := newDriver(f)
		if st, err := d.State(context.Background(), testCaller, ref); err != nil || st.Prompt == nil {
			t.Fatalf("state = %+v, err %v", st, err)
		}
		f.captures["%1"] = idleFixtureFor("alpha")
		skew = promptSightingTTL + time.Second
		check(t, d, single)
	})
}
