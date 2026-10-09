package tmux

import (
	"context"
	"errors"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #280: this driver inherits the service's environment, so a create that
// REQUIRES an isolated one is refused as unsupported — before anything is
// started — and the driver says so in its capabilities.
func TestCreateRefusesIsolateEnvironmentAsUnsupported(t *testing.T) {
	d := New("testbox")
	_, err := d.Create(context.Background(), fleet.Request{}, "key-1", fleet.SessionSpec{
		Cwd: "/work/x", IsolateEnvironment: true,
	})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("Create = %v, want an ErrorUnsupported", err)
	}
	if d.Capabilities().IsolatesEnvironment {
		t.Error("tmux driver reports IsolatesEnvironment: true")
	}
}
