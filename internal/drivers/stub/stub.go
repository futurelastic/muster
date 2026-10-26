// Package stub provides a driver.Driver that implements no runtime at all:
// every operation returns driver.ErrUnsupported. It exists to exercise
// internal/service's routing, deadline, error-mapping, and idempotency
// paths without a real substrate underneath.
//
// This is the only driver this repository ships in its current phase — see
// NOTES.local.md's sequencing: the local driver wrapping today's tmux
// behaviour, and the remote HTTP driver, are both future work, proven
// against this same Driver interface once it exists.
package stub

import (
	"context"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// defaultDeadlineMs is used when Driver.DeadlineMs is left at its zero
// value. Even a driver that supports nothing must still declare a positive
// deadline (§4.4) — the mandatory field is not conditional on the driver
// doing any real work.
const defaultDeadlineMs = 5000

// Driver is a no-op driver.Driver. Every method returns driver.ErrUnsupported.
type Driver struct {
	// DeadlineMs is what Capabilities reports. Zero or negative falls back
	// to defaultDeadlineMs rather than producing a DriverCapabilities that
	// would fail Validate.
	DeadlineMs int64
}

var _ driver.Driver = (*Driver)(nil)

func (d *Driver) Capabilities() fleet.DriverCapabilities {
	deadline := d.DeadlineMs
	if deadline <= 0 {
		deadline = defaultDeadlineMs
	}
	return fleet.DriverCapabilities{
		ObservesState:    false,
		DeliversRawKeys:  false,
		ConfirmsDelivery: false,
		SupportsResume:   false,
		SupportsPin:      fleet.PinSupport{},
		// muster #379: this driver supports no operation, and says so — the
		// declared "cannot" form of each person-facing capability.
		ReadsTurns:           fleet.Declared(false),
		ReportsConversation:  fleet.Declared(false),
		Respond:              &fleet.RespondSupport{},
		PermissionModeChange: &fleet.PermissionModeChangeSupport{},
		DeadlineMs:           deadline,
		Source:               fleet.CapabilitiesObserved,
	}
}

func (d *Driver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	return fleet.Session{}, driver.ErrUnsupported
}

// Neither ResumeIfStranded nor ReplaceIfStranded (muster #112) is
// applicable here: this driver supports no operation at all, so there is
// never a stranded delivery of its own to resume or replace.
func (d *Driver) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	return fleet.DeliveryReceipt{}, driver.ErrUnsupported
}

func (d *Driver) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	return fleet.DeliveryReceipt{}, driver.ErrUnsupported
}

func (d *Driver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	return fleet.SessionState{}, driver.ErrUnsupported
}

func (d *Driver) Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	return fleet.Ack{}, driver.ErrUnsupported
}

func (d *Driver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	return fleet.Ack{}, driver.ErrUnsupported
}

func (d *Driver) Discard(ctx context.Context, req fleet.Request, ref fleet.SessionRef, expectDigest string, opts driver.DiscardOptions) (fleet.Ack, error) {
	return fleet.Ack{}, driver.ErrUnsupported
}

func (d *Driver) Rename(ctx context.Context, req fleet.Request, ref fleet.SessionRef, to string) (fleet.RenameAck, error) {
	return fleet.RenameAck{}, driver.ErrUnsupported
}

func (d *Driver) List(ctx context.Context, req fleet.Request, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	return fleet.Collection[fleet.Session]{}, driver.ErrUnsupported
}

func (d *Driver) Subscribe(ctx context.Context, req fleet.Request, filter driver.SubscribeFilter) (driver.EventStream, error) {
	return nil, driver.ErrUnsupported
}
