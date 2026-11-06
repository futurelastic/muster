// Package delivery is the seam between a local driver and the mechanism that
// actually puts a caller's text into a session (#180).
//
// A local driver owns everything that is a decision about the REQUEST — the
// per-session lock, sanitising, the runtime-syntax guard, contradictory flags,
// the sender label. A delivery module owns the MECHANISM: how the text reaches
// the session, how it is confirmed, and what is recorded when it strands.
// Callers of POST /input never see this seam: request bodies, outcomes and
// receipts are the driver's contract, unchanged by which module ran.
//
// # What exists today, and what is deliberately absent
//
// One module ships: the tmux driver's built-in terminal path. The interface is
// shaped so a later module is additive — a new implementation of Module, not a
// change to this package or to any caller. Discovery, a configuration switch
// and automatic fallback between modules are NOT here; they belong to the
// change that adds a second module, and building them against a single
// implementation would be guessing at requirements nobody has measured.
//
// # Why Delivery is a struct
//
// A later module may need facts this one does not (the session's process
// identity, say). A struct grows a field without breaking every
// implementation; a positional signature does not.
package delivery

import (
	"context"
	"fmt"
	"sort"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// Module delivers text into one session.
type Module interface {
	// Name is stable and short: it becomes part of every counter name this
	// module's outcomes are recorded under (delivery.<name>.…).
	Name() string

	// Deliver puts in.Text into the session in.Ref and reports what
	// happened. The receipt is returned to the caller verbatim, so its
	// Outcome and Reason must obey the same honesty rules every receipt
	// does. An error means the mechanism itself failed (the multiplexer is
	// unreachable, say) — never "the delivery was refused", which is a
	// receipt.
	Deliver(ctx context.Context, in Delivery) (Result, error)

	// ReservedEnv names environment variables this module needs to be the
	// sole setter of for a session's agent process. Session create refuses a
	// caller-supplied variable with one of these names. Nil for a module
	// that reserves nothing.
	ReservedEnv() []string
}

// Delivery is one request to a module. Text is final: already sanitised,
// already checked against the runtime-syntax guard, already carrying the
// sender label if the call has one.
type Delivery struct {
	Req  fleet.Request
	Ref  fleet.SessionRef
	Text string
	Opts driver.SendOptions
}

// Class is what a delivery amounted to, finer than the receipt's Outcome.
// The receipt is the caller's contract; the class is the operator's view,
// kept so the counters can tell a refusal that protected a person's draft
// from one that merely found the session busy.
type Class string

const (
	// Delivered: the text reached the session and the submit was confirmed
	// (or, for a no-submit call, the text was placed).
	Delivered Class = "delivered"
	// Resumed: a previously stranded delivery was finished by this call.
	Resumed Class = "resumed"
	// Refused: nothing was delivered, for any reason not named below.
	Refused Class = "refused"
	// RefusedDraftKept: nothing was delivered because the composer held
	// text this module could not prove was its own, and the draft rule
	// forbids clearing or submitting it.
	RefusedDraftKept Class = "refused.draft_kept"
	// RefusedBusy: nothing was delivered because another operation held
	// this session's composer for longer than the caller would wait.
	// Transient; retrying is expected to succeed.
	RefusedBusy Class = "refused.busy"
	// Stalled: nothing was delivered because the session reads `working` and
	// has drawn nothing new for the whole stall bound (#391), so the runtime
	// would not have read it. Unlike the two refusals above, nothing a caller
	// does to the send changes the answer, and waiting does not clear it.
	Stalled Class = "stalled"
	// Ended: the session's own process exited while the delivery was being
	// confirmed (#411). No stranded or provisional record is kept, and the
	// caller is told not to retry: there is no session left to retry on.
	Ended Class = "ended"
	// Stranded: the text reached the composer but was not confirmed
	// submitted, and a record was kept so a resume can finish it.
	Stranded Class = "stranded"
	// Unknown: the outcome is unknown and no record was kept.
	Unknown Class = "unknown"
	// Discarded: a stranded or pending composer was discarded on request.
	// Recorded by Discard, never by Deliver.
	Discarded Class = "discarded"
)

// Signal names the evidence a confirmed submit rested on.
type Signal string

const (
	SignalNone       Signal = ""
	SignalTranscript Signal = "transcript"
	SignalScreen     Signal = "screen"
	// SignalModule: an external delivery module said the message was accepted
	// (#185). The module reads the runtime's own record of the turn, so this is
	// the same standard of evidence as SignalTranscript, but muster did not
	// see the record itself and does not claim to.
	SignalModule Signal = "module"
)

// Result is a module's answer to one Delivery.
type Result struct {
	Receipt     fleet.DeliveryReceipt
	Class       Class
	ConfirmedBy Signal

	// Declined is non-empty when the module wrote NOTHING and is handing the
	// send back (#185): it is not live for this session, it went unavailable
	// before the write, or it was refused the runtime build. It says why, for
	// the operator. Receipt and Class are meaningless alongside it.
	//
	// It is not a receipt because a receipt is an answer to the caller, and
	// "this lane could not take it" is a fact the DRIVER acts on — falling back
	// to the built-in module for an `auto` send, refusing for a forced route.
	// A module that DID write and cannot say whether it landed does not
	// decline: that is Receipt.Outcome unknown, and a decline after a write
	// would license a second delivery of the same text.
	Declined string
}

// CounterPrefix is the family every module counter lives under.
const CounterPrefix = "delivery."

// Observe records one result under module's counters: the receipt's
// outcome, the class, and the confirmation signal when there was one.
// A result with no class is recorded as "unclassified", which a test pins at
// zero: every path through a module is supposed to say what it amounted to.
func Observe(incr func(string), module string, r Result) {
	p := CounterPrefix + module + "."
	if r.Receipt.Outcome != "" {
		incr(p + "outcome." + string(r.Receipt.Outcome))
	}
	if r.Class == "" {
		incr(p + "unclassified")
	} else {
		incr(p + string(r.Class))
	}
	if r.ConfirmedBy != SignalNone {
		incr(p + "confirmed." + string(r.ConfirmedBy))
	}
}

// CheckReservedEnv refuses env naming any reserved variable. The error names
// every offending variable, sorted, so the message is stable.
func CheckReservedEnv(env map[string]string, reserved []string) error {
	if len(env) == 0 || len(reserved) == 0 {
		return nil
	}
	var hit []string
	for _, name := range reserved {
		if _, ok := env[name]; ok {
			hit = append(hit, name)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	sort.Strings(hit)
	return fmt.Errorf("env: %s is reserved by this machine's delivery module: the service "+
		"sets it for the agent process, so a caller may not", strings.Join(hit, ", "))
}

// CheckReservedEnvPrefixes refuses env naming any variable that begins with a
// reserved prefix (#185). The error names every offending variable, sorted.
// Prefixes are matched case-sensitively, as environment names are.
func CheckReservedEnvPrefixes(env map[string]string, prefixes []string) error {
	if len(env) == 0 || len(prefixes) == 0 {
		return nil
	}
	var hit []string
	for name := range env {
		if HasReservedPrefix(name, prefixes) {
			hit = append(hit, name)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	sort.Strings(hit)
	return fmt.Errorf("env: %s is reserved by this machine's delivery module: the service "+
		"sets it for the agent process, so a caller may not", strings.Join(hit, ", "))
}

// HasReservedPrefix reports whether name begins with any of prefixes. An empty
// prefix is ignored rather than matching everything: a malformed reservation
// must never turn into "every variable is reserved".
func HasReservedPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ValidModuleName reports whether name is a legal external delivery module
// name (#185): lowercase letters, digits and hyphens, starting with a letter,
// at most 32 bytes — the shape of a file name that is safe in a counter name,
// a URL field and a modules-directory lookup with nothing to escape. The
// names the route vocabulary already uses are refused, so a module can never
// shadow "terminal", "inbox", "auto" or the receipt's own "module".
func ValidModuleName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '-'):
		default:
			return false
		}
	}
	switch name {
	case "auto", "terminal", "inbox", "module":
		return false
	}
	return true
}
