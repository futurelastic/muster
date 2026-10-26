package tmux

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// The remote-control switch-off against SCREENS (muster#380).
//
// remotecontrol_test.go drives the toggle against a model of the dialog, which
// cannot say what the classifier reads when the session's own transcript is on
// screen too. Here the port is a screen: the driver's real classifier reads it,
// so what the toggle sees is what the service would see.

// rcDialogFrame is the dialog the runtime opens when /remote-control is typed
// on a session whose remote control is on, measured live on runtime 2.1.310
// and shortened to what the classifier reads. It is drawn under the transcript,
// in place of the composer. Note what it is not: its options carry no numbers,
// and it has no rule line above it (its top edge is a row of ▔), so nothing
// but its footer says where it starts.
//
// The address in the second line is invented.
const rcDialogFrame = `
 a tip row · scroll with PgUp/PgDn ▔
   Remote Control

   This session is available in the Claude mobile app and at
   https://example.test/code/session_0000000000.

     Disconnect this session
     Show QR code   Scan with your phone to open this session
   ❯ Continue

   Enter to select · Esc to continue
`

// rcIdleBottom is the bottom of an idle session: an empty composer between
// rules, and the status rows under it.
const rcIdleBottom = "────────────────────────────────────────────────────────────────────────────────\n" +
	"❯ \n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"  ▸ model · name\n" +
	"  ⏵⏵ auto mode on (shift+tab to cycle)\n"

// rcTranscripts is what a session may have printed above its prompt. The
// numbered ones are the measured failure: a numbered row in the window was read
// as the dialog's options. The rest are shapes that did NOT misread and must
// keep reading the dialog.
var rcTranscripts = map[string]string{
	"numbered list": "● Một bản tóm tắt:\n\n" +
		"  1. Sửa lỗi phân tích\n" +
		"  2. Thêm kiểm thử\n" +
		"  3. Chạy lại cổng\n\n",
	"numbered list, Japanese": "● 次の三つです:\n\n" +
		"  1. 解析の修正\n" +
		"  2. テストの追加\n" +
		"  3. ゲートの再実行\n\n",
	"one numbered row": "● Bước một:\n\n" +
		"  1. Một việc\n\n" +
		"● Xong.\n\n",
	"numbered list then more text": "● Danh sách:\n\n" +
		"  1. Một\n" +
		"  2. Hai\n" +
		"  3. Ba\n\n" +
		"● Còn lại không có gì.\n\n",
	"numbered echo with the highlight marker": "❯ 1. Một\n" +
		"  2. Hai\n" +
		"  3. Ba\n\n" +
		"● Đã nhận.\n\n",
	"short lines, no numbers": "● Xong rồi\n\n" +
		"  Một\n" +
		"  Hai\n" +
		"  Ba\n" +
		"  日本語\n" +
		"  テスト\n\n",
	"multi-line message echo": "❯ Làm giúp tôi\n" +
		"  việc này\n" +
		"  và việc kia\n\n" +
		"● Đã làm\n\n",
	"dash bullets": "● Kết quả:\n" +
		"  - Một\n" +
		"  - Hai\n" +
		"  - Ba\n\n",
	"nothing": "",
}

// rcScreenPort is rcPort over a screen. It models only what the toggle depends
// on: the command opens the dialog when remote control is on, and the dialog is
// answered by what the CLASSIFIER reads from the screen — the nonce and the
// option the caller chose are checked against that read, as Respond does.
type rcScreenPort struct {
	t          *testing.T
	mu         sync.Mutex
	transcript string
	channel    fleet.ControlChannelState
	dialog     bool
	noDialog   bool // the runtime ignores the command
	sends      []string
	responds   []fleet.Response
}

func (p *rcScreenPort) screen() string {
	if p.dialog {
		return p.transcript + rcDialogFrame
	}
	return p.transcript + rcIdleBottom
}

func (p *rcScreenPort) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := classify(p.screen(), true)
	if !p.dialog {
		if st.Prompt != nil {
			p.t.Errorf("a prompt was read off an idle screen: %+v", st.Prompt)
		}
		// One capture of an empty composer cannot say idle (the driver settles
		// it with a second look); this port stands in for that second look.
		st.Status = fleet.StatusIdle
	}
	st.ControlChannel = &fleet.ControlChannel{State: p.channel}
	return st, nil
}

func (p *rcScreenPort) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sends = append(p.sends, text)
	if p.noDialog {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, nil
	}
	if p.channel == fleet.ControlChannelOff {
		p.channel = fleet.ControlChannelActive
	} else {
		p.dialog = true
	}
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, nil
}

func (p *rcScreenPort) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responds = append(p.responds, resp)
	cur := classify(p.screen(), true).Prompt
	if cur == nil || cur.Nonce != resp.Nonce {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "not waiting on that prompt"}, nil
	}
	chosen := ""
	if !resp.Cancel {
		chosen = strings.ToLower(strings.TrimSpace(cur.Options[resp.Choice-1]))
	}
	p.dialog = false
	if strings.HasPrefix(chosen, disconnectOptionText) {
		p.channel = fleet.ControlChannelOff
	}
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted}, nil
}

func (p *rcScreenPort) set(enabled bool) (fleet.Ack, error) {
	return rcToggler{p: p, window: 300 * time.Millisecond, interval: 5 * time.Millisecond}.
		set(context.Background(), testCaller, rcRef, enabled)
}

// The dialog is found by its own frame, so nothing the session printed above it
// can be read as its options — numbered or not — and the switch-off goes
// through.
func TestToggleOff_FindsTheDialogWhateverTheTranscriptAboveIt(t *testing.T) {
	for name, transcript := range rcTranscripts {
		t.Run(name, func(t *testing.T) {
			p := &rcScreenPort{t: t, transcript: transcript, channel: fleet.ControlChannelActive}

			// What the classifier reads from the dialog's own screen.
			prompt := classify(transcript+rcDialogFrame, true).Prompt
			if prompt == nil {
				t.Fatal("the dialog was not read as a prompt")
			}
			want := []string{"Disconnect this session", "Show QR code   Scan with your phone to open this session", "Continue"}
			if strings.Join(prompt.Options, "|") != strings.Join(want, "|") || prompt.Selected != 3 {
				t.Fatalf("options %q selected %d, want the dialog's own %q with Continue highlighted", prompt.Options, prompt.Selected, want)
			}
			if got := disconnectOption(prompt); got != 1 {
				t.Fatalf("disconnectOption = %d, want 1", got)
			}

			ack, err := p.set(false)
			if err != nil || !ack.Accepted {
				t.Fatalf("ack %+v err %v, want the switch-off to succeed", ack, err)
			}
			if p.channel != fleet.ControlChannelOff || p.dialog {
				t.Fatalf("channel %s dialog open %v, want off with nothing left open", p.channel, p.dialog)
			}
			if len(p.sends) != 1 || p.sends[0] != "/remote-control" {
				t.Fatalf("sends %v, want the one command", p.sends)
			}
			if len(p.responds) != 1 || p.responds[0].Cancel || p.responds[0].Choice != 1 {
				t.Fatalf("responds %+v, want one answer choosing option 1 (Disconnect)", p.responds)
			}
		})
	}
}

// With no dialog on screen the transcript is only transcript: nothing is read as
// a prompt, and nothing is sent to a session that showed none. In particular no
// Escape, which would be a keypress into a session nobody asked to disturb.
func TestToggleOff_NoDialogLeavesTheScreenAsFound(t *testing.T) {
	for name, transcript := range rcTranscripts {
		t.Run(name, func(t *testing.T) {
			p := &rcScreenPort{t: t, transcript: transcript, channel: fleet.ControlChannelActive, noDialog: true}
			_, err := p.set(false)
			wantConflict(t, err)
			if !strings.Contains(err.Error(), "no disconnect dialog appeared") {
				t.Fatalf("err = %v, want the no-dialog conflict", err)
			}
			if len(p.sends) != 1 || len(p.responds) != 0 {
				t.Fatalf("sends %v responds %+v, want the command and no keypress", p.sends, p.responds)
			}
		})
	}
}

// strayModel is rcModel with the two things the timing tests need: what each
// read of the screen shows, and when the toggle sent and answered.
type strayModel struct {
	*rcModel
	// shows, if set, picks the prompt a read reports: n is the 1-based count of
	// reads taken since the command was sent, real is the model's own prompt.
	shows               func(n int, real *fleet.SessionPrompt) *fleet.SessionPrompt
	reads               int
	sentAt, respondedAt time.Time
}

func (s *strayModel) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	st, _ := s.rcModel.State(ctx, req, ref)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sends) == 0 || s.shows == nil {
		return st, nil
	}
	s.reads++
	st.Prompt = s.shows(s.reads, st.Prompt)
	if st.Prompt != nil {
		st.Status = fleet.StatusWaitingInput
	}
	return st, nil
}

func (s *strayModel) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	s.mu.Lock()
	s.sentAt = time.Now()
	s.mu.Unlock()
	return s.rcModel.Send(ctx, req, ref, text, opts)
}

func (s *strayModel) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	s.mu.Lock()
	s.respondedAt = time.Now()
	s.mu.Unlock()
	return s.rcModel.Respond(ctx, req, ref, resp)
}

func (s *strayModel) set(t *testing.T, enabled bool) (fleet.Ack, error) {
	t.Helper()
	return rcToggler{p: s, window: 300 * time.Millisecond, interval: 5 * time.Millisecond}.
		set(context.Background(), testCaller, rcRef, enabled)
}

func rcStray() *fleet.SessionPrompt {
	return &fleet.SessionPrompt{Options: []string{"first line", "second line"}, Nonce: "stray"}
}

// A prompt that lacks the option is not the dialog, and one read of it proves
// nothing: the next read may be the dialog. It is answered when it turns up.
func TestToggleOff_AStrayReadBeforeTheDialogIsNotAnswerOrDismissed(t *testing.T) {
	s := &strayModel{rcModel: newRCModel(fleet.ControlChannelActive)}
	s.shows = func(n int, real *fleet.SessionPrompt) *fleet.SessionPrompt {
		if n <= 3 {
			return rcStray()
		}
		return real
	}
	ack, err := s.set(t, false)
	if err != nil || !ack.Accepted {
		t.Fatalf("ack %+v err %v, want the switch-off to ride out three bad reads", ack, err)
	}
	if len(s.responds) != 1 || s.responds[0].Cancel || s.channel.State != fleet.ControlChannelOff {
		t.Fatalf("responds %+v channel %+v, want one answer of the real dialog and nothing cancelled", s.responds, s.channel)
	}
}

// What was read and is gone again is not dismissed: nothing is open, so a
// keypress would land on a session that showed nothing.
func TestToggleOff_AStrayReadThatGoesAwayIsNeverCancelled(t *testing.T) {
	s := &strayModel{rcModel: newRCModel(fleet.ControlChannelActive)}
	s.noDialog = true
	s.shows = func(n int, real *fleet.SessionPrompt) *fleet.SessionPrompt {
		if n <= 2 {
			return rcStray()
		}
		return nil
	}
	_, err := s.set(t, false)
	wantConflict(t, err)
	if !strings.Contains(err.Error(), "no disconnect dialog appeared") || len(s.responds) != 0 {
		t.Fatalf("err %v responds %+v, want the no-dialog conflict and no keypress", err, s.responds)
	}
}

// A prompt that stands without the option for the whole window did open after
// the command (there was none before it): it is dismissed, once, only when the
// window is over, and the conflict names what was seen.
func TestToggleOff_AStrayPromptThatStandsIsDismissedAtTheEndOfTheWindow(t *testing.T) {
	s := &strayModel{rcModel: newRCModel(fleet.ControlChannelActive)}
	s.shows = func(n int, real *fleet.SessionPrompt) *fleet.SessionPrompt { return real }
	s.options = []string{"Show QR code", "Continue"}
	_, err := s.set(t, false)
	wantConflict(t, err)
	if len(s.responds) != 1 || !s.responds[0].Cancel || s.prompt != nil || s.channel.State != fleet.ControlChannelActive {
		t.Fatalf("responds %+v prompt %+v channel %+v, want one cancel and the channel untouched", s.responds, s.prompt, s.channel)
	}
	if waited := s.respondedAt.Sub(s.sentAt); waited < 300*time.Millisecond {
		t.Fatalf("dismissed %s after the command, before the 300ms window was over", waited)
	}
	for _, want := range []string{"Show QR code", "Continue"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to name %q", err, want)
		}
	}
}
