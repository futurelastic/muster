package service

import (
	"context"
	"io"
	"net/http"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/machinelist"
	"github.com/futurelastic/muster/internal/peerkey"
)

// GET and PUT /v1/machine-list (muster #352): the owner-signed machine list,
// handed from one machine to another over the service's own API.
//
// The list is signed, so the channel is untrusted by design; what this file
// decides is who may use the channel, and in what order, because the order is
// the security property:
//
//	authenticate -> the grant -> is a list used here at all -> the size ->
//	only then read the body -> only then ask the holder to verify it
//
// A caller without the grant is refused before its body is read, signed callers
// included: they are verified from their headers (withAuthDeferringBody) and
// the body is checked against the digest they signed afterwards (checkedBody).

// MachineListHolder is the running service's view of its machine list. The
// service package cannot import the binary that owns the pinned signer, the
// reload and the state directory, so the binary implements this and hands it in
// through Config.MachineList.
//
// Every method answers in the wire's terms (a *fleet.Error with its Kind and
// Reason), so the mapping from "what went wrong" to "what the caller is told"
// lives next to the code that knows what went wrong, not here.
type MachineListHolder interface {
	// Held returns the exact bytes of the list this machine has IN FORCE: not the
	// file on disk, which may be a candidate the service refused.
	Held() ([]byte, *fleet.Error)
	// CanAccept says, before any body is read, whether a pushed list could be
	// taken at all here: nil, or the refusal (the list is off; there is no state
	// directory to remember the highest sequence in).
	CanAccept() *fleet.Error
	// Accept verifies raw against the pinned signer, requires its sequence to be
	// strictly higher than the highest accepted, writes it and puts it in force.
	// by names the pusher for the log. It returns the receipt only when the list
	// is written AND in force; on any error nothing changed.
	Accept(raw []byte, by string) (fleet.MachineListReceipt, *fleet.Error)
}

func machineListOff() *fleet.Error {
	return &fleet.Error{Kind: fleet.ErrorUnsupported, Reason: fleet.ReasonMachineListOff,
		Message: "this machine does not use an owner-signed machine list (no machineList block in its config, or mode off)"}
}

func machineListTooLarge() *fleet.Error {
	return &fleet.Error{Kind: fleet.ErrorInvalid, Reason: fleet.ReasonMachineListTooLarge,
		Message: "a machine list is limited to 1 MiB"}
}

// handleGetMachineList answers GET /v1/machine-list with the list this machine
// has in force, byte for byte as it was signed. The read grant is enough
// (reading() in front of it): the list is signed, so serving it reveals nothing
// that a stronger grant would protect.
func handleGetMachineList(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.MachineList == nil {
			writeError(w, machineListOff())
			return
		}
		raw, ferr := cfg.MachineList.Held()
		if ferr != nil {
			writeError(w, ferr)
			return
		}
		w.Header().Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}
}

// pushListGate is the check a push passes before anything reads its body: the
// push-list grant, and an audit line either way. It is not mutating(), on
// purpose: in the legacy single-token model mutating() lets every caller in when
// FLEET_ALLOW_MUTATIONS is set, and a push must never be allowed without a
// per-principal grant. With no principal table nobody can hold it, so a push is
// refused.
func pushListGate(svc *Service, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalOf(r)
		if !ok {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnauthorized, Machine: svc.Self(),
				Message: "this machine has no principal table, so no caller can hold the push-list grant (§6): " +
					"a machine list can be handed over only to a machine that identifies its callers"})
			return
		}
		if !p.Allows(GrantPushList) {
			logDenied(p, GrantPushList, svc.Self(), r)
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorUnauthorized, Machine: svc.Self(),
				Message: "principal " + p.Name + " does not hold the push-list grant (§6). " +
					"Every grant defaults to denied, on a fresh deployment as much as an " +
					"established one — this is expected until an operator adds push-list " +
					"to this principal's grants, not a bug.",
			})
			return
		}
		aw := &auditWriter{ResponseWriter: w}
		next(aw, r)
		logMutation(p, GrantPushList, svc.Self(), r, aw.status)
	}
}

// handlePutMachineList answers PUT /v1/machine-list. By the time it runs the
// caller is authenticated and holds push-list; it has still read nothing.
func handlePutMachineList(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := cfg.MachineList
		if h == nil {
			writeError(w, machineListOff())
			return
		}
		if ferr := h.CanAccept(); ferr != nil {
			writeError(w, ferr)
			return
		}
		raw, ferr := checkedBody(r)
		if ferr != nil {
			writeError(w, ferr)
			return
		}
		p, _ := principalOf(r)
		receipt, ferr := h.Accept(raw, p.Name)
		if ferr != nil {
			writeError(w, ferr)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}

// checkedBody reads a pushed list's body — bounded, after every refusal that
// does not need it — and, for a caller that signed, confirms it is the body
// whose digest was signed. Only a request that got through withAuthDeferringBody
// carries the mark; a bearer caller has no digest to check.
func checkedBody(r *http.Request) ([]byte, *fleet.Error) {
	if r.ContentLength > machinelist.MaxBytes {
		return nil, machineListTooLarge()
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, machinelist.MaxBytes+1))
	if err != nil {
		return nil, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "the request body could not be read"}
	}
	if len(body) > machinelist.MaxBytes {
		return nil, machineListTooLarge()
	}
	if unchecked(r.Context()) {
		if err := peerkey.CheckBody(r, body); err != nil {
			return nil, &fleet.Error{Kind: fleet.ErrorUnauthorized,
				Message: "the body does not match the digest the sender signed"}
		}
	}
	return body, nil
}

func unchecked(ctx context.Context) bool {
	v, _ := ctx.Value(bodyUncheckedKey{}).(bool)
	return v
}
