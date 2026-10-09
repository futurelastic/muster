package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #281: this driver runs sessions in the user's own multiplexer, which is
// the very thing a sandbox keeps a session away from, so a create that asks for
// one is refused as unsupported before anything is started.
func TestCreateRefusesSandboxAsUnsupported(t *testing.T) {
	d := New("testbox")
	_, err := d.Create(context.Background(), fleet.Request{}, "key-1", fleet.SessionSpec{
		Cwd: "/work/x", Sandbox: &fleet.SandboxSpec{},
	})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported || !strings.Contains(fe.Message, "sandbox") {
		t.Fatalf("Create = %v, want an unsupported refusal naming the sandbox", err)
	}
	if d.Capabilities().Sandbox != nil {
		t.Error("the multiplexer driver declares a sandbox capability")
	}
}
