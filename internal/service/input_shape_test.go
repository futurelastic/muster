package service

import (
	"net/http"
	"strings"
	"testing"
)

// #320: the request's own shape. A body whose message is missing, empty or
// under the wrong field name is a 400 naming `text`, and nothing reaches the
// driver — before this, {"prompt": "..."} delivered an empty turn and the
// sender was told "queued".
func TestSendInput_RefusesABodyWithoutAMessage(t *testing.T) {
	d, srv := routeOwner(t)
	for _, tc := range []struct {
		name string
		body map[string]any
		want []string // every one must appear in the message
	}{
		{"the create field name", map[string]any{"prompt": "x"}, []string{`"prompt"`, `"text"`}},
		{"no field at all", map[string]any{}, []string{"text"}},
		{"empty text", map[string]any{"text": ""}, []string{"text"}},
		{"empty text, submit explicit", map[string]any{"text": "", "submit": true}, []string{"text"}},
		{"null text", map[string]any{"text": nil}, []string{"text"}},
		{"unknown field next to a valid text", map[string]any{"text": "x", "sumbit": false}, []string{`"sumbit"`, `"text"`}},
		{"empty text with resumeIfStranded", map[string]any{"text": "", "resumeIfStranded": true}, []string{"text"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := d.sends()
			resp := postInput(t, srv, "agent-token", nil, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			msg := decodeError(t, resp).Error.Message
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message = %q, want it to contain %s", msg, w)
				}
			}
			if d.sends() != before {
				t.Error("a refused body reached the driver: a turn was delivered")
			}
		})
	}
}

// A well-formed send is untouched: every defined field still decodes.
func TestSendInput_AcceptsEveryDefinedField(t *testing.T) {
	d, srv := routeOwner(t)
	before := d.sends()
	resp := postInput(t, srv, "agent-token", nil, map[string]any{
		"text": "x", "submit": true, "resumeIfStranded": false, "replaceIfStranded": false,
		"expect": "", "from": map[string]any{"agent": "a"}, "route": "auto",
	})
	mustOK(t, resp)
	if d.sends() != before+1 {
		t.Errorf("sends = %d, want %d: a valid send must be delivered as before", d.sends(), before+1)
	}
}
