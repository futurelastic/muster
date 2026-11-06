package tmux

import (
	"context"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
	"github.com/futurelastic/muster/internal/driver"
)

// builtinModuleName is the built-in module's name, and so the middle of every
// counter it records (delivery.tmux.…).
const builtinModuleName = "tmux"

// tmuxModule is the built-in delivery module: terminal path v2
// (deliverViaPane). It reserves no environment names.
type tmuxModule struct{ d *Driver }

func (tmuxModule) Name() string          { return builtinModuleName }
func (tmuxModule) ReservedEnv() []string { return nil }

func (m tmuxModule) Deliver(ctx context.Context, in delivery.Delivery) (delivery.Result, error) {
	var tr sendTrace
	receipt, err := m.d.deliverViaPane(ctx, in.Ref, in.Text, in.Opts, &tr)
	if err != nil {
		return delivery.Result{}, err
	}
	if receipt.Outcome == fleet.OutcomeUnknown {
		// Whether a record was kept is a fact about state, not about which
		// return statement produced the receipt — reading it here means no
		// strand site can forget to say so.
		_, tr.stranded = m.d.strandedRecordByID(in.Ref.ID)
	}
	return delivery.Result{Receipt: receipt, Class: tr.class(receipt), ConfirmedBy: tr.confirmedBy}, nil
}

// sendTrace is what one pass through deliverViaPane amounted to, beyond the
// receipt's outcome. Set only at the sites that know: the draft-rule
// refusals, a completed resume, and the submit confirmation.
type sendTrace struct {
	stranded    bool
	resumed     bool
	draftKept   bool
	busy        bool
	confirmedBy delivery.Signal
}

func (t sendTrace) class(r fleet.DeliveryReceipt) delivery.Class {
	switch r.Outcome {
	case fleet.OutcomeQueued, fleet.OutcomeSubmitted:
		if t.resumed {
			return delivery.Resumed
		}
		return delivery.Delivered
	case fleet.OutcomeRefused:
		switch {
		case t.draftKept:
			return delivery.RefusedDraftKept
		case t.busy:
			return delivery.RefusedBusy
		}
		return delivery.Refused
	case fleet.OutcomeEnded:
		return delivery.Ended
	case fleet.OutcomeUnknown:
		if t.stranded {
			return delivery.Stranded
		}
		return delivery.Unknown
	}
	return ""
}

// WithDeliveryModule replaces the built-in module. For tests: nothing in
// this repository ships a second module yet (#180 keeps discovery and a
// configuration switch out of scope).
func WithDeliveryModule(m delivery.Module) Option {
	return func(d *Driver) { d.module = m }
}

func (d *Driver) deliveryModule() delivery.Module {
	if d.module != nil {
		return d.module
	}
	return tmuxModule{d: d}
}

// ReservedEnv implements driver.ReservedEnvReporter: the names this driver's
// delivery module needs to be the sole setter of.
func (d *Driver) ReservedEnv() []string { return d.deliveryModule().ReservedEnv() }

var _ driver.ReservedEnvReporter = (*Driver)(nil)

// observeEarly records a refusal Send made before any module ran (the lock,
// the runtime-syntax guard, contradictory flags) under the module's counters,
// so every Send that reaches the pane path is counted exactly once.
func (d *Driver) observeEarly(class delivery.Class, r fleet.DeliveryReceipt) fleet.DeliveryReceipt {
	delivery.Observe(d.counters.incr, d.deliveryModule().Name(), delivery.Result{Receipt: r, Class: class})
	return r
}

// strandedRecordByID reports whether a stranded record exists for id,
// whatever its working directory.
func (d *Driver) strandedRecordByID(id string) (strandedRecord, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.stranded[id]
	return rec, ok
}
