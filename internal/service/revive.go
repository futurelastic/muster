package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
	"github.com/futurelastic/muster/internal/driver"
)

// handleRevive brings a session a restart left `unknown` ("needs re-creation") back,
// given the environment its caller created it with (POST …/revive, muster #427;
// api-http.md §3.3).
//
// # Why a verb, and why this grant
//
// A session created with caller-supplied environment is never relaunched by a
// restart: the values were never kept (muster #282), and relaunching with less would
// be a different session wearing the same id. Its conversation is intact in the
// runtime's own store, and until this verb nothing could reach it but a close. The
// caller still has the values, so the caller hands them back. Nothing is persisted
// and no secret is kept here to do it with.
//
// It needs the create grant (grantForVerb): it starts a process on this machine with
// caller-supplied environment, which is what the create grant is. A session created
// in the permission-bypass posture is started only for a caller who also holds
// `send`, the same pairing a create asking for that posture has.
//
// # What this layer decides, and what the driver does
//
// This layer refuses a body that cannot be a revive (no `env`) before any driver or
// peer is involved, resolves where the session lives, and refuses an environment
// naming a variable a delivery module reserves, as a create does. Everything about
// the SESSION — whether it is revivable, whether the names match, whether the runtime
// still has it — is the driver's, because only it knows what the restart left; its
// refusals arrive typed, 409 `not-revivable` for a session that is not the kind a
// revive can bring back.
//
// # Relay
//
// mutating() has already required the `relay` grant for a peer target, and the peer
// applies its own `create` grant, its own `send` pairing and its own checks to this
// service's credential. The environment is forwarded as a create's is. A peer built
// before the route answers with its router's bare 404, which the remote driver
// reports as unsupported, never as a missing session.
//
// # What never appears
//
// The environment's values are in this request's body and nowhere else: not in the
// audit line mutating() writes (it logs the verb, the route and the outcome), not in
// an error message, and not in a log line.
func handleRevive(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")

		// Strict, like `input` (#320): a field this verb does not define is a caller
		// error, named, never ignored. {"cwd": …} or {"name": …} would otherwise be
		// read as a revive that also changes the session, and it changes nothing.
		var body fleet.ReviveRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			msg := "malformed JSON body"
			if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
				msg = fmt.Sprintf("unknown field %s: a revive takes `env` and, optionally, `contextRef`; "+
					"it cannot change anything else about the session", field)
			}
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: msg, Machine: machine})
			return
		}
		if len(body.Env) == 0 {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: machine,
				Message: "revive needs `env`: the environment the session was created with. A session is not " +
					"relaunched with less than it was created with"})
			return
		}

		req := requestFrom(r)
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))
		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, runtimeHint, parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		reviver, ok := d.(driver.Reviver)
		if !ok {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported, Machine: machine,
				Message: "this runtime cannot bring a session back after a restart (capabilities.revives is not declared)"})
			return
		}

		spec := driver.ReviveSpec{Env: body.Env, ContextRef: body.ContextRef}
		if via != resolvedPeer {
			// muster #342: the program each session runs is not on this machine, so a
			// revive would start a server that cannot. The same refusal a create gets.
			if u, bad := svc.agentProgramDown(r.Context(), resolvedRuntime, d); bad {
				writeError(w, unavailableError(machine, resolvedRuntime, u))
				return
			}
			// muster #180, #185: an environment naming a variable this machine's delivery
			// module reserves is refused as a create's would be, naming the variable. A
			// relayed revive is the owning machine's to check against its own.
			if rr, ok := d.(driver.ReservedEnvReporter); ok {
				if err := delivery.CheckReservedEnv(body.Env, rr.ReservedEnv()); err != nil {
					writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#180)", Machine: machine})
					return
				}
			}
			if pr, ok := d.(driver.ReservedEnvPrefixReporter); ok {
				if err := delivery.CheckReservedEnvPrefixes(body.Env, pr.ReservedEnvPrefixes()); err != nil {
					writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#185)", Machine: machine})
					return
				}
			}
			// Whether this caller may start a session created in the bypass posture:
			// the `send` grant, as for the create that asked for it. Without a
			// principal table (the legacy flag) mutating() has already decided.
			p, hasPrincipal := principalOf(r)
			spec.MayBypass = !hasPrincipal || p.Allows(GrantSend)
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		sess, err := reviver.Revive(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, spec)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		if via != resolvedPeer {
			sess.Labels = svc.labels.get(resolvedRuntime, sess.ID, sess.StartedAt)
		}
		writeJSON(w, http.StatusOK, sess)
	}
}
