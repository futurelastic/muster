package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/inboxclient"
)

// Config wires the pieces an HTTP server needs beyond the Service itself.
type Config struct {
	// Token is the single bearer token this instance accepts. There is no
	// unauthenticated mode (api-http.md §5) — every request must present
	// Authorization: Bearer <Token>, loopback or not, dev or not.
	Token string

	// Principals is the per-identity authorization table (§6, auth.go).
	// When non-empty it is authoritative and Token/AllowLocalMutations/
	// AllowPeerRelay are ignored.
	//
	// The older fields remain for a single-machine or single-token
	// deployment, where a principal table is ceremony without benefit. They
	// are not a fallback that silently engages: if a table is configured,
	// it decides everything.
	Principals []Principal

	// AllowLocalMutations permits create/input/interrupt/close against
	// sessions ON THIS MACHINE. Defaults FALSE — §6 requirement 3 taken
	// literally: reads may be granted broadly, mutations are opt-in.
	//
	// The gate exists because authentication alone cannot express it. A
	// single shared token cannot distinguish a peer from a local
	// supervisor, so without a verb gate anything holding the token can
	// start processes here. §6: "a service that can start processes and
	// read files is a remote-execution surface regardless of intent."
	AllowLocalMutations bool

	// AllowPeerRelay permits forwarding a mutation to a PEER. Also
	// defaults FALSE, but it is a different question, and conflating the
	// two was defect D6.
	//
	// "May something mutate sessions on my machine" is about what this host
	// exposes. "May I relay a mutation to a peer" is about what this
	// instance may do as a client, and exposes nothing here — the peer is
	// the one taking the risk, and the peer already has its own gate.
	//
	// With one flag governing both, a hardened host could not act as a
	// full-featured client: relaying required opening this machine's own
	// sessions to mutation. That is backwards, and it was found by
	// deploying rather than by reasoning.
	AllowPeerRelay bool
}

// NewMux builds the routing skeleton of api-http.md §3 over svc, using
// Go's method+wildcard ServeMux patterns (no router dependency needed).
// Every handler does real request parsing, deadline/idempotency/auth
// enforcement, and error-envelope construction; none of them implement a
// working driver behind it — that boundary is the task's, and it is drawn
// here, not faked by half-real handlers.
func NewMux(svc *Service, cfg Config) *http.ServeMux {
	mux := http.NewServeMux()
	// The self item of GET /v1/machines reports what this machine's own
	// model grants the credential it presents to peers (#154); the model is
	// cfg, which only the mux holds.
	svc.setSelfGrantResolver(func(tok string) []string { return grantsForToken(cfg, tok) })

	mux.HandleFunc("GET /v1/health", withAuth(cfg, reading(handleHealth(svc))))
	// GET /v1/whoami deliberately skips reading()'s GrantRead gate — see
	// handleWhoAmI's doc comment (whoami.go) for why a caller reading its
	// own grants is not the same authorization question as a caller
	// reading someone else's data.
	mux.HandleFunc("GET /v1/whoami", withAuth(cfg, handleWhoAmI(svc, cfg)))
	mux.HandleFunc("GET /v1/machines", withAuth(cfg, reading(handleMachines(svc))))
	mux.HandleFunc("GET /v1/runtimes", withAuth(cfg, reading(handleRuntimes(svc))))
	mux.HandleFunc("GET /v1/sessions", withAuth(cfg, reading(handleListSessions(svc))))
	mux.HandleFunc("GET /v1/sessions/closed", withAuth(cfg, reading(handleListClosedSessions(svc))))
	mux.HandleFunc("GET /v1/sessions/watch", withAuth(cfg, reading(handleWatchSessions(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions", withAuth(cfg, mutating(svc, cfg, handleCreateSession(svc))))
	mux.HandleFunc("GET /v1/machines/{machine}/sessions/{id}", withAuth(cfg, reading(handleGetSession(svc))))
	mux.HandleFunc("GET /v1/machines/{machine}/sessions/{id}/environment", withAuth(cfg, reading(handleSessionEnvironment(svc))))
	mux.HandleFunc("GET /v1/machines/{machine}/sessions/{id}/turns", withAuth(cfg, revealing(handleTurns(svc))))
	mux.HandleFunc("GET /v1/machines/{machine}/sessions/{id}/composer", withAuth(cfg, reading(handleComposer(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/input", withAuth(cfg, mutating(svc, cfg, handleSendInput(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/respond", withAuth(cfg, mutating(svc, cfg, handleRespond(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/interrupt", withAuth(cfg, mutating(svc, cfg, handleInterrupt(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/discard", withAuth(cfg, mutating(svc, cfg, handleDiscard(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/rename", withAuth(cfg, mutating(svc, cfg, handleRename(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/keys", withAuth(cfg, mutating(svc, cfg, handleKeys(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/labels", withAuth(cfg, mutating(svc, cfg, handleLabels(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/remote-control", withAuth(cfg, mutating(svc, cfg, handleRemoteControl(svc))))
	mux.HandleFunc("DELETE /v1/machines/{machine}/sessions/{id}", withAuth(cfg, mutating(svc, cfg, handleClose(svc))))
	mux.HandleFunc("GET /v1/events", withAuth(cfg, reading(handleEvents(svc))))

	return mux
}

// withAuth enforces api-http.md §5: no request is served without a bearer
// token, ever — not for loopback, not for development. cfg.Token is a
// single shared secret appropriate to a small, statically-configured fleet
// (§7.2); it is not a multi-tenant credential store, and this middleware
// does not pretend otherwise. Per-peer, per-verb authorization (§6.3) is
// what Config.Principals implements below, once a deployment configures
// more than one peer identity to distinguish — this function only
// authenticates and resolves; mutating and reading are what consult a
// resolved principal's grants (muster #80 found this comment had
// drifted from the code it sits above: the "future work" it once was had
// already landed for the mutating verbs by the time this was written, and
// only the read half was still true).
func withAuth(cfg Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.Principals) > 0 {
			p, ok := cfg.principalFor(r)
			if !ok {
				writeError(w, &fleet.Error{
					Kind:    fleet.ErrorUnauthorized,
					Message: "unrecognised credential",
				})
				return
			}
			next(w, r.WithContext(withPrincipal(r.Context(), p)))
			return
		}
		want := "Bearer " + cfg.Token
		if cfg.Token == "" || r.Header.Get("Authorization") != want {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnauthorized,
				Message: "missing or invalid Authorization bearer token",
			})
			return
		}
		next(w, r)
	}
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// principalOf returns the resolved principal, and whether a table was in use.
func principalOf(r *http.Request) (Principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(Principal)
	return p, ok
}

// mutating gates the verbs that change something behind Config.AllowMutations
// (§6 requirement 3). A refusal here is unauthorized, not unsupported: the
// driver is perfectly capable, and this instance is configured not to permit
// it. Reporting "unsupported" would tell a req something false about the
// runtime and invite it to give up permanently on a capability that exists.
func mutating(svc *Service, cfg Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := fleet.MachineId(r.PathValue("machine"))

		// With a principal table, §6 requirement 3 is expressible as
		// written: per verb, per identity. D6's host/client split survives
		// as the difference between a verb grant and GrantRelay.
		if p, ok := principalOf(r); ok {
			need := grantForVerb(r)
			if target != "" && target != svc.Self() {
				need = GrantRelay
			}
			if !p.Allows(need) {
				logDenied(p, need, target, r)
				writeError(w, &fleet.Error{
					Kind: fleet.ErrorUnauthorized,
					Message: "principal " + p.Name + " does not hold the " +
						string(need) + " grant (§6). Every grant defaults to " +
						"denied, on a fresh deployment as much as an established " +
						"one — this is expected until an operator adds " +
						string(need) + " to this principal's grants, not a bug.",
					Machine: target,
				})
				return
			}
			// Run first, then log what actually happened. Logging the
			// authorization decision as though it were the outcome made a
			// destroy and a refusal indistinguishable in the trail.
			aw := &auditWriter{ResponseWriter: w}
			next(aw, r)
			logMutation(p, need, target, r, aw.status)
			return
		}

		allowed, why := cfg.AllowLocalMutations,
			"this host does not permit mutation of its own sessions (§6; FLEET_ALLOW_MUTATIONS)"
		if target != "" && target != svc.Self() {
			allowed, why = cfg.AllowPeerRelay,
				"this instance does not relay mutations to peers (§6; FLEET_ALLOW_RELAY). "+
					"Note this is a separate grant from mutating local sessions: a hardened "+
					"host may still be a full-featured client"
		}
		if !allowed {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnauthorized, Message: why, Machine: target})
			return
		}
		next(w, r)
	}
}

// reading gates every read route behind Config.Principals' read grant (§6)
// — the read-side counterpart mutating already provides for every mutating
// verb (muster #80). Before this existed, grantForVerb's own GrantRead
// answer for a GET was computed by nothing and consulted by nothing:
// withAuth resolved a principal and asked no further question, so any
// authenticated principal — a janitor scoped to one narrow mutating grant,
// or one holding no grants at all — could still list every session on
// every configured peer, read every working directory, and subscribe to
// the full event stream. Not exploitable (every route still needs a valid
// bearer token; there is no unauthenticated mode), but the least-privilege
// model the principal table exists to provide was not enforced on its read
// half, and an operator reading the spec would reasonably believe it was.
//
// Deliberately NOT mutating()'s relay-target upgrade: a fleet-scoped read,
// or one explicitly naming a peer machine, needs only GrantRead here,
// regardless of target. Ruled, not defaulted into (muster #81): a
// relayed mutation changes state on a machine the caller is not talking to,
// while a relayed read does not, so requiring GrantRelay for both would
// treat reaching and changing as one act. The symmetric rule — folding
// relay into this check too — was considered and rejected on a measured
// cost, not a hypothetical one: at least one principal this fleet is
// observed through holds read without relay today, and that option would
// have refused its calls silently the moment it landed. Full reasoning,
// including both alternatives considered and rejected: docs/spec/api-http.md
// §5 (Authorization, the normative grants section) and
// docs/adr/81-relay-grant-not-required-for-reads.md.
//
// A no-op when no principal table is configured. The legacy single-token
// model's read side has no equivalent gate BY DESIGN — AllowLocalMutations'
// own doc comment states it plainly: "reads may be granted broadly,
// mutations are opt-in" — and this wrapper leaves that model exactly as it
// was; it only ever has something to check once principalOf resolves.
func reading(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalOf(r)
		if !ok {
			next(w, r)
			return
		}
		if !p.Allows(GrantRead) {
			target := fleet.MachineId(r.PathValue("machine"))
			logDenied(p, GrantRead, target, r)
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorUnauthorized,
				Message: "principal " + p.Name + " does not hold the read grant (§6). " +
					"Every grant defaults to denied, on a fresh deployment as much as an " +
					"established one — this is expected until an operator adds read to " +
					"this principal's grants, not a bug.",
				Machine: target,
			})
			return
		}
		next(w, r)
	}
}

// callerFrom derives the authority a request is made on behalf of (§6, §13).
//
// Credential is the bearer token the req actually presented — not this
// service's own. That is the whole of §13's "proxying does not launder
// authorization": when this service forwards to a peer, the peer sees the
// token of whoever started the request, and authorizes them rather than the
// machine that relayed it.
//
// Principal is provenance, not identity, and the difference is worth being
// honest about. While the fleet authenticates with one shared token (§7.2),
// nothing distinguishes one bearer from another, so the most specific true
// statement available is where the request came from. It is recorded for the
// audit trail §6 requirement 4 asks for — "actor, verb, target, outcome" —
// and it should be read as an address, because that is all it is. Real
// per-peer identity is §6's outstanding work; when it arrives, it lands
// here and nothing above this function changes.
func callerFrom(r *http.Request) fleet.Caller {
	// A resolved principal is an identity; the address below is only what
	// is available when no table is configured.
	if p, ok := principalOf(r); ok {
		return callerFor(p, r)
	}
	origin := r.RemoteAddr
	if host, _, err := net.SplitHostPort(origin); err == nil {
		origin = host
	}
	return fleet.Caller{
		Principal:  "addr:" + origin,
		Credential: bearerOf(r),
	}
}

// requestFrom builds the caller-side context of an operation (§2.6): who is
// asking, and what they believe about the target.
//
// Expect.StartedAt is read from ?startedAt=, and its absence is meaningful
// rather than incidental: a caller that supplies it gets §5.4's real
// guarantee — "destroy the session I looked at" — while a caller that omits it
// gets the weaker one a driver can offer from its own sightings, and is told
// which it got when the operation refuses.
func requestFrom(r *http.Request) fleet.Request {
	req := fleet.Request{Caller: callerFrom(r)}
	if raw := r.URL.Query().Get("startedAt"); raw != "" {
		if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			req.Expect.StartedAt = &ts
		} else if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			req.Expect.StartedAt = &ts
		}
	}
	return req
}

// bearerOf extracts the presented token. It deliberately does not validate —
// withAuth has already done that, and a second opinion here could only
// disagree.
func bearerOf(r *http.Request) string {
	const p = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
		return v[len(p):]
	}
	return ""
}

func writeError(w http.ResponseWriter, e *fleet.Error) {
	w.Header().Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Kind.DefaultHTTPStatus())
	_ = json.NewEncoder(w).Encode(fleet.ErrorEnvelope{Error: *e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// setResolutionHeaders makes resolveSessionDriver's choice visible on the
// wire (muster issue #60 guardrail 2) — set once, right after a
// resolution succeeds, so it is present whether the operation that follows
// eventually succeeds or fails. §5.7 forbids a caller who named its runtime
// and a caller who got the machine's configured default rendering alike;
// a log line is not "the answer", so this is a header rather than an entry
// only an operator can read afterwards.
//
// Fleet-Runtime names the runtime that actually served the call — useful on
// its own for the bare-id single-driver and existence-matched cases, which
// otherwise have no way to tell a caller which runtime answered short of
// the runtime field already carried by a session read. Fleet-Runtime-
// Resolution is set to "default" ONLY when the configured default runtime
// was the tiebreak; its absence is itself informative — every other
// resolution is as trustworthy as a caller naming its own runtime, because
// each is either the caller's own word or a fact this machine just
// confirmed by asking.
//
// rt == "" (the peer and error paths) sets nothing: a peer resolution
// carries no local runtime id to report, and an unresolved call has no
// driver to attribute a header to at all.
func setResolutionHeaders(w http.ResponseWriter, rt fleet.RuntimeId, via runtimeResolution) {
	if rt != "" {
		w.Header().Set("Fleet-Runtime", string(rt))
	}
	if via == resolvedDefault {
		w.Header().Set("Fleet-Runtime-Resolution", "default")
	}
}

// writeDriverError maps a Go-level driver error to the wire error kind
// api-http.md §2 defines. Only reached for a driver-level failure — a
// refusal (DeliveryReceipt.Outcome == "refused") is not an error at all
// and is written as an ordinary 200 by its own handler.
func writeDriverError(w http.ResponseWriter, machine fleet.MachineId, deadline time.Duration, err error) {
	// A peer that already classified this failure has said something this
	// service cannot improve on: adopt its kind rather than re-deriving one.
	//
	// This is §13.2's rule — "adopt a peer's SourceStatus; never
	// re-synthesize it" — applied to errors, and it was found the same way,
	// by watching a correct answer get worse in transit. The peer returned
	// conflict (§5.4: the caller's belief is stale); re-classification here
	// turned it into invalid, telling the caller to fix its syntax when what
	// it should do is re-read and decide.
	var relayed *fleet.Error
	if errors.As(err, &relayed) && relayed.Kind != "" {
		writeError(w, relayed)
		return
	}

	switch {
	case isConversationIdError(err):
		// #268: the session exists; the caller addressed it by its
		// conversation id. Still not_found (no session HAS this id), but the
		// message says what the id is and the ids to use travel structured.
		var ce *fleet.ConversationIdError
		errors.As(err, &ce)
		writeError(w, &fleet.Error{Kind: fleet.ErrorNotFound, Message: ce.Error(), Machine: machine,
			Reason: fleet.ReasonConversationId, SessionIds: ce.SessionIds})
	case errors.Is(err, fleet.ErrNoSuchSession):
		// The machine answered, and there is no such session. Distinct from
		// unreachable, which is the machine not answering at all — see
		// fleet.ErrorUnreachable's comment for why conflating them is the
		// worst mistake a client of this API can make.
		writeError(w, &fleet.Error{Kind: fleet.ErrorNotFound, Message: err.Error(), Machine: machine})
	case errors.Is(err, fleet.ErrNoTurnRecord):
		// The session exists; the source of its turns does not (yet). Retryable,
		// because the usual cause is a record the runtime has not written.
		writeError(w, &fleet.Error{Kind: fleet.ErrorNotFound, Message: err.Error(), Machine: machine, Retryable: true})
	case errors.Is(err, fleet.ErrAmbiguousTarget):
		// §5.4: the caller's belief and the world disagree. Well-formed
		// request, conflicting state — 409, not 400.
		writeError(w, &fleet.Error{Kind: fleet.ErrorConflict, Message: err.Error(), Machine: machine})
	case isUnsupported(err):
		writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported, Message: err.Error(), Machine: machine})
	case isDeadlineExceeded(err):
		writeError(w, &fleet.Error{
			Kind:      fleet.ErrorUnreachable,
			Message:   "no answer within " + deadline.String(),
			Machine:   machine,
			Retryable: true,
		})
	default:
		writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error(), Machine: machine})
	}
}

// parseDeadline reads Fleet-Deadline-Ms (api-http.md §3.3). Absent or
// malformed, it returns 0, meaning "no caller-supplied bound" — the
// driver's own declared deadline applies unmodified (effectiveDeadline).
func parseDeadline(r *http.Request) time.Duration {
	raw := r.Header.Get("Fleet-Deadline-Ms")
	if raw == "" {
		return 0
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func handleHealth(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"epoch": svc.epoch,
			// The event plane is real now, so this reports the actual
			// high-water mark. It used to be a hardcoded 0 with a comment
			// explaining that no event bus existed — true when written, and
			// left behind when one did. A health endpoint that reports a
			// constant is worse than one that omits the field: a subscriber
			// comparing its cursor against this would conclude it had run
			// ahead of the service.
			"cursor":    svc.events.currentCursor(),
			"startedAt": svc.startedAt.Format(time.RFC3339Nano),
			// Which code this is. See fleet.Build for the incident that
			// added it; the short version is that two services disagreeing
			// is a different problem from two services being different
			// vintages, and nothing could previously tell them apart.
			"build": svc.build,
			// This machine's effective limit on `prompt` (create) and
			// `text` (input) — muster #130, the ask-do-not-infer
			// move #121 already made for build above. It is what
			// GET /v1/machines' per-entry maxInputBytes for self reports,
			// and what internal/drivers/remote's peerHealth reads to learn
			// a PEER's own value the same way it already learns build.
			"maxInputBytes": svc.MaxInputBytes(),
			// The bounds this machine enforces on session labels
			// (muster #153). Its presence is also how a relaying peer
			// learns this service carries labels at all, so a labelled
			// create is refused there rather than silently stripped here.
			"labels": fleet.SelfLabelLimits(),
			// Whether this service's create endpoint understands
			// conversationId (muster #224) — the same reason `labels`
			// is reported one line up: this is a wire-protocol fact, learned
			// by internal/drivers/remote's peerHealth the same way it learns
			// build and labels, so a create carrying the field is refused
			// against an older peer rather than silently forwarded and
			// dropped there.
			"supportsConversationId": true,
			// Whether the create endpoint understands `isolateEnvironment`
			// (muster #280) — the same wire-protocol fact, for the same reason:
			// a relaying peer asks before forwarding, so a create that REQUIRES an
			// isolated environment is refused against an older build rather than
			// forwarded and silently started with the service's own environment.
			"supportsIsolateEnvironment": true,
			// Whether the create endpoint understands `settings` (muster
			// #247) — the same wire-protocol fact, for the same reason: a relaying
			// peer asks before forwarding, so a create carrying launch settings
			// is refused against an older build rather than forwarded and
			// silently dropped there.
			"supportsLaunchSettings": true,
			// The keys a session NOT in bypass mode may carry in `settings`
			// (muster #254). Absent on a peer that carries only #247, whose
			// `settings` is bypass-only, so a relaying peer reads absence as
			// "refuse a non-bypass create" rather than forwarding it into a
			// 400 — or, worse, into an older build that drops the field.
			"launchSettingsOutsideBypass": fleet.LaunchSettingsOutsideBypass(),
			"drivers":                     svc.driverSummaries(),
			// counters is the read path #9 asked for onto the registry #44
			// built (internal/drivers/tmux/counters.go): an integer per
			// named fact, keyed by runtime. It is deliberately read here
			// rather than logged periodically — the F57 flap this issue
			// opens with was found by curling a read surface five times and
			// comparing counts, and a pull endpoint composes with the
			// poller that already exists on this response (the dashboard
			// already hits /v1/health); a log line only pays off with
			// someone tailing it at the moment it happens.
			//
			// startedAt above is the divisor this needs and already had:
			// every count here resets to zero on every restart, so read
			// alone it cannot distinguish a quiet machine from a young one.
			// A reader who divides by time-since-startedAt gets a rate;
			// one who does not is the reader #9's regression scenario
			// describes — looking at a healthy-looking number that is
			// actually just recent.
			"counters": svc.counterSnapshot(),
		})
	}
}

func handleMachines(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ?verify=1 re-probes every peer first, instead of answering from
		// the cached cycle (muster #154). Still only read.
		if r.URL.Query().Get("verify") == "1" {
			svc.RefreshPeers(r.Context(), parseDeadline(r))
		}
		col, err := svc.ListMachines(r.Context(), requestFrom(r), parseDeadline(r))
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, col)
	}
}

func handleRuntimes(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		col, err := svc.ListRuntimes(r.Context())
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, col)
	}
}

func handleListSessions(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		scope := Scope(q.Get("scope"))
		if scope == "" {
			scope = ScopeFleet // client default (api-http.md §3.2)
		}
		if scope != ScopeLocal && scope != ScopeFleet {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "scope must be 'local' or 'fleet'"})
			return
		}

		labels, ferr := parseLabelFilter(q["label"])
		if ferr != nil {
			writeError(w, ferr)
			return
		}
		filter := driver.ListFilter{
			Status:    fleet.Status(q.Get("status")),
			Agent:     fleet.AgentId(q.Get("agent")),
			CwdPrefix: q.Get("cwdPrefix"),
			Labels:    labels,
		}

		// Read the sequence position BEFORE enumerating, so a client that
		// watches from it re-applies a few changes rather than missing any.
		// See fleet.FeedPosition for why the overlap is the safe direction and
		// why an unwatched service withholds the number entirely.
		cursor, epoch, resumable := svc.FeedPosition()

		col, err := svc.ListSessions(r.Context(), requestFrom(r), scope, filter, parseDeadline(r))
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		if resumable {
			col = col.WithFeed(cursor, epoch)
		}
		writeJSON(w, http.StatusOK, col)
	}
}

// handleListClosedSessions answers GET /v1/sessions/closed (muster
// #179): the records this service keeps of sessions that ended, for a bounded
// retention period. `since` (RFC 3339) keeps records that closed at or after
// it; `scope` has the live list's meaning.
func handleListClosedSessions(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		scope := Scope(q.Get("scope"))
		if scope == "" {
			scope = ScopeFleet
		}
		if scope != ScopeLocal && scope != ScopeFleet {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "scope must be 'local' or 'fleet'"})
			return
		}
		var since time.Time
		if raw := q.Get("since"); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "since must be an RFC 3339 timestamp"})
				return
			}
			since = t
		}
		col, err := svc.ListClosedSessions(r.Context(), requestFrom(r), scope, since, parseDeadline(r))
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, col)
	}
}

// parseLabelFilter reads repeated `label=key:value` parameters into the AND
// set GET /v1/sessions filters on (muster #153). The split is on the
// first separator, which keys may not contain. Naming one key twice is
// refused rather than answered with a list that can never match anything.
func parseLabelFilter(raw []string) (map[string]string, *fleet.Error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for _, pair := range raw {
		k, v, ok := strings.Cut(pair, fleet.LabelSeparator)
		if !ok {
			return nil, &fleet.Error{Kind: fleet.ErrorInvalid,
				Message: fmt.Sprintf("label filter %q must be key%svalue", pair, fleet.LabelSeparator)}
		}
		if err := fleet.ValidateLabelKey(k); err != nil {
			return nil, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "label filter: " + err.Error()}
		}
		if prev, dup := out[k]; dup && prev != v {
			return nil, &fleet.Error{Kind: fleet.ErrorInvalid,
				Message: fmt.Sprintf("label filter names key %q twice with different values; every pair must match, so nothing could", k)}
		}
		out[k] = v
	}
	return out, nil
}

// Long-poll bounds. The default is long enough that an idle fleet costs one
// request every half minute, short enough to sit inside the timeouts every
// proxy and HTTP client has whether or not their operator remembers them.
const (
	watchDefaultWait = 25 * time.Second
	watchMaxWait     = 60 * time.Second
)

// watchResponse is the long poll's body.
//
// Cursor is what to send as the next `since` — not "the service's current
// cursor", which would silently skip anything stamped between the last event
// in this batch and the read of that field.
type watchResponse struct {
	Cursor int64           `json:"cursor"`
	Epoch  string          `json:"epoch"`
	Events []eventEnvelope `json:"events"`
}

// handleWatchSessions is the change-feed as ordinary request/response
// (api-http.md §4.1).
//
// # Why a second transport rather than a second feed
//
// Everything here is the same hub, the same cursor sequence, the same event
// vocabulary and the same envelope as GET /v1/events. What differs is only how
// the bytes reach the caller: a consumer maintaining a materialized mirror
// wants a request it can retry, log and reason about, and one that survives
// its own restart without a stream-reconnect state machine. Nothing is
// expressible in one transport and not the other, deliberately — the moment
// they diverge, two answers to the same question exist.
//
// # A stale cursor is an answer, not a fault
//
// control.resync arrives IN BAND, as the first (and only) event of the batch,
// with an ordinary 200. Same rule as a refused input: the request was well
// formed, and what the service has to say about it is domain information a
// caller must act on, not an exception to retry. A 4xx here would train a
// client to retry the one thing it must instead re-list after.
func handleWatchSessions(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		scope := ScopeFleet
		switch q.Get("scope") {
		case "", string(ScopeFleet):
		case string(ScopeLocal):
			// §13.1 in the event plane: a peer asking us for scope=local must
			// not make us open streams back to our own peers.
			scope = ScopeLocal
		default:
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "scope must be 'local' or 'fleet'"})
			return
		}

		filter := driver.SubscribeFilter{Sessions: q["session"], CwdPrefix: q.Get("cwdPrefix")}

		var since int64
		if raw := q.Get("since"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid,
					Message: "since must be a non-negative cursor from a previous response"})
				return
			}
			since = n
		}
		// An omitted epoch means "the instance I was already talking to",
		// matching how the SSE handler reads a browser's Last-Event-ID. A
		// caller that has one should send it; one that has a cursor and no
		// epoch is asserting continuity, and if that assertion is wrong the
		// epoch check below turns it into a resync rather than a bad resume.
		fromEpoch := q.Get("epoch")
		if since > 0 && fromEpoch == "" {
			fromEpoch = svc.Epoch()
		}

		wait := watchWait(r)

		ch, backlog, needResync, cancel := svc.Events(r.Context(), scope, filter, since, fromEpoch)
		defer cancel()

		epoch := svc.Epoch()

		if needResync {
			reason := fleet.ResyncCursorExpired
			if fromEpoch != epoch {
				reason = fleet.ResyncEpochChanged
			}
			// Cursor stays at what the caller sent. The resync is not a
			// position to resume from — the caller re-lists, and the listing
			// carries the position it should watch from next.
			writeJSON(w, http.StatusOK, watchResponse{
				Cursor: since, Epoch: epoch,
				Events: []eventEnvelope{{
					Epoch: epoch, Machine: svc.Self(), Kind: fleet.EventControlResync,
					Payload: fleet.ControlResyncPayload{Reason: reason},
				}},
			})
			return
		}

		events := make([]eventEnvelope, 0, len(backlog))
		for _, ev := range backlog {
			events = append(events, envelopeOf(ev))
		}

		if len(events) == 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			case ev, open := <-ch:
				if open {
					events = append(events, envelopeOf(ev))
					// Take whatever else is already waiting, so a busy fleet
					// answers one poll with a batch instead of one poll per
					// event. Nothing BLOCKS here: this drains what has already
					// arrived and stops.
					events = drainReady(ch, events)
				}
			}
		}

		writeJSON(w, http.StatusOK, watchResponse{
			Cursor: resumeCursor(since, events),
			Epoch:  epoch,
			Events: events,
		})
	}
}

// watchWait resolves how long this poll may block: the caller's `wait`, capped,
// and shortened further by Fleet-Deadline-Ms if that is smaller — §3.3's rule
// that a caller may always shorten and never extend.
func watchWait(r *http.Request) time.Duration {
	wait := watchDefaultWait
	if raw := r.URL.Query().Get("wait"); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms >= 0 {
			wait = time.Duration(ms) * time.Millisecond
		}
	}
	if wait > watchMaxWait {
		wait = watchMaxWait
	}
	if d := parseDeadline(r); d > 0 && d < wait {
		wait = d
	}
	return wait
}

// drainReady appends everything already queued for this subscriber without
// waiting for more.
func drainReady(ch <-chan fleet.Event, into []eventEnvelope) []eventEnvelope {
	for {
		select {
		case ev, open := <-ch:
			if !open {
				return into
			}
			into = append(into, envelopeOf(ev))
		default:
			return into
		}
	}
}

// resumeCursor is what the caller sends as its next `since`.
//
// The last event in the batch, or — for an empty batch — exactly what the
// caller already had. Not the service's current cursor: an empty batch means
// nothing SELECTED by this filter arrived, while the sequence may well have
// advanced for somebody else, and advancing this caller's cursor past events
// it never saw is the silent gap the whole design refuses.
func resumeCursor(since int64, events []eventEnvelope) int64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Cursor > 0 {
			return events[i].Cursor
		}
	}
	return since
}

func envelopeOf(ev fleet.Event) eventEnvelope {
	return eventEnvelope{
		Cursor: ev.Cursor, Epoch: ev.Epoch, Machine: ev.Machine,
		Kind: ev.Kind, Payload: ev.Payload, Origin: ev.Origin,
	}
}

type createSessionBody struct {
	Runtime    fleet.RuntimeId    `json:"runtime"`
	Cwd        fleet.AbsolutePath `json:"cwd"`
	Agent      fleet.AgentId      `json:"agent"`
	Model      string             `json:"model"`
	Effort     string             `json:"effort"`
	Name       string             `json:"name"`
	Prompt     string             `json:"prompt"`
	ContextRef fleet.AbsolutePath `json:"contextRef"`

	// Marker and RemoteControl close the gap that made an API-created
	// session a different KIND of session from a launcher-created one. See
	// fleet.SessionSpec for both. RemoteControl is a pointer because
	// "absent" and "false" must not be the same request.
	Marker        string `json:"marker"`
	RemoteControl *bool  `json:"remoteControl"`

	// TrustCwd is the caller's consent to the runtime's folder-trust question
	// about Cwd. See fleet.SessionSpec.TrustCwd for what it means, and
	// handleCreateSession for why it needs a second grant.
	TrustCwd bool `json:"trustCwd"`

	// Env, Resume, PermissionMode and Consents close the gap that kept a
	// supervisor driving the substrate directly instead of this API: a session
	// created here could not carry its identity, continue a conversation, or be
	// started in a non-default permission posture. See fleet.SessionSpec for
	// each, and handleCreateSession for which of them need the send grant.
	Env            map[string]string  `json:"env"`
	Resume         string             `json:"resume"`
	PermissionMode string             `json:"permissionMode"`
	Consents       []fleet.PromptKind `json:"consents"`

	// IsolateEnvironment requires the session's process to carry only a built
	// environment (muster #280). It only NARROWS what the session gets, so it
	// needs no grant beyond create. See fleet.SessionSpec.IsolateEnvironment.
	IsolateEnvironment bool `json:"isolateEnvironment"`

	// ConversationId asks the runtime to start a NEW conversation under a
	// caller-chosen UUID (muster #224) — mutually exclusive with Resume,
	// which continues one. See fleet.SessionSpec.ConversationId.
	ConversationId string `json:"conversationId"`

	// McpConfig names tool-server configuration files by path. See
	// fleet.SessionSpec for why paths and not content, and createNeedsSend for
	// why it is one of the fields that asks for more than a create.
	McpConfig []fleet.AbsolutePath `json:"mcpConfig"`

	// Settings is launch-time runtime configuration for the agent CLI
	// (muster #247). Any key in bypass mode; only the allow-listed keys
	// outside it (muster #254). See fleet.SessionSpec.Settings.
	Settings json.RawMessage `json:"settings"`

	// Labels are stored by the service against the created session
	// (muster #153). Only create is needed to send them, as for name
	// and marker: they describe the session, they grant it nothing.
	Labels map[string]string `json:"labels"`
}

// createNeedsSend names the part of a create body that requires the send grant,
// or "" when the body asks for nothing beyond starting a session.
//
// It returns the NAME rather than a boolean so the refusal can say which field
// caused it. A caller told only "you need send" re-reads the whole request
// guessing; one told "consents requires it" fixes it in a line.
func createNeedsSend(body createSessionBody) string {
	switch {
	case body.TrustCwd:
		return "trustCwd"
	case len(body.Consents) > 0:
		return "consents"
	case body.PermissionMode != "" && body.PermissionMode != permissionModeDefaultRequest:
		// The explicit ordinary mode (#256) asks for nothing beyond a create.
		return "permissionMode"
	case len(body.McpConfig) > 0:
		// A tool-server configuration names processes the session will LAUNCH.
		// Same gap as permissionMode's, one step out: "may start a session" and
		// "may start a session that also starts these" are different
		// authorities, and only the second needs saying out loud.
		return "mcpConfig"
	case len(bytes.TrimSpace(body.Settings)) > 0 && !bytes.Equal(bytes.TrimSpace(body.Settings), []byte("null")):
		// Launch-time runtime settings (#247): a step up from plain create like
		// permissionMode. Still needs `send` outside bypass (#254) — the
		// allow-listed key widens nothing, but the field's grant is by field, not
		// by key, so a client's authority does not depend on what it put in it.
		return "settings"
	}
	return ""
}

// defaultMaxInputBytes bounds `prompt` on create and `text` on input
// (muster #114) when no machine-local override is configured
// (Service.MaxInputBytes, cmd/muster/config.go's `maxInputBytes`,
// muster #130). A caller that pastes something long into either field
// reaches the composer and then never submits — the session sits at zero
// turns holding unsent text with no way forward but resumeIfStranded
// (identical text only) or destroying the session (#110, #112). Rejecting
// the request outright, before any driver is even resolved, turns that
// silent failure into an immediate, actionable error.
//
// 1024 is a conservative default, not the bisected failure boundary — #114
// says a real bisect is still open work, and #130 records that the
// mechanism behind that boundary may be startup timing rather than size at
// all, which is exactly why raising this default is deliberately NOT part
// of making it configurable: #130 keeps this number unchanged and leaves
// raising it to follow the bisect (#129), on evidence, per machine. The
// cleanest data available (#110's one-line, no-newline sweep against the
// input path) held reliable through 1200 bytes and broke at 1600; #112
// separately measured a ~900-byte CREATE prompt stranding twice the same
// day 3833- and 5139-byte prompts landed fine, so raw size alone is not the
// whole story — multi-line shape and host load are named confounds there
// too. 1024 sits inside the one controlled bisect with headroom below it,
// and is far below the "detailed briefing material" case #114 itself says
// belongs in a reference the agent reads deliberately, never a pasted
// composer.
const defaultMaxInputBytes = 1024

// rejectOverLength returns a fleet.Error naming field, limit and the
// caller's actual size when text exceeds limit, or nil when it does not.
// Shared by handleCreateSession's prompt and handleSendInput's text — see
// #114. limit is the caller's own effective value (Service.MaxInputBytes,
// #130) rather than a package constant, so the message always names the
// bound this machine is actually enforcing, configured or defaulted.
//
// Both call sites run this check unconditionally, before resolving whether
// `machine` is this instance or a peer — the same "before any driver is
// resolved" property #114 already required, kept unchanged by #130. A
// relayed create or input is therefore pre-checked against the RELAYING
// machine's own limit, not the target's; the peer's own handleCreateSession
// / handleSendInput enforces its own limit again once the call actually
// reaches it, which is the authoritative check. Divergence between the two
// is real only once an operator configures different limits on different
// machines — #130 keeps the shipped default identical everywhere, so this
// is a known, currently-inert edge rather than an oversight.
func rejectOverLength(field, text string, machine fleet.MachineId, limit int) *fleet.Error {
	if len(text) <= limit {
		return nil
	}
	return &fleet.Error{
		Kind: fleet.ErrorInvalid,
		Message: fmt.Sprintf(
			"%s is %d bytes, over the %d-byte limit (#114): send shorter "+
				"text, or put the detail somewhere the agent can read it "+
				"deliberately and send only a pointer",
			field, len(text), limit),
		Machine: machine,
	}
}

func handleCreateSession(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))

		// Idempotency-Key is required, not optional (§10, api-http.md
		// §3.3) — a create without one is rejected before any driver is
		// even consulted.
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "Idempotency-Key header is required (§10)", Machine: machine})
			return
		}

		var body createSessionBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body", Machine: machine})
			return
		}

		if ferr := rejectOverLength("prompt", body.Prompt, machine, svc.MaxInputBytes()); ferr != nil {
			writeError(w, ferr)
			return
		}
		if err := fleet.ValidateLabels(body.Labels); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#153)", Machine: machine})
			return
		}
		// muster #247/#254: shape and the mode/key pairing, refused here with a
		// reason before a driver — or a peer — is involved. A driver checks
		// again (Create is a public method), so this is the cheap early answer.
		if _, err := fleet.ValidateLaunchSettings(body.PermissionMode, body.Settings); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#247)", Machine: machine})
			return
		}
		// muster #224: checked here, before any driver is resolved —
		// same placement as rejectOverLength and ValidateLabels above, and
		// for the same reason stated on rejectOverLength: a relayed create is
		// pre-checked against these two rules before the call ever crosses to
		// a peer, so a malformed or self-contradictory request never pays for
		// a round trip it was always going to lose.
		if body.ConversationId != "" {
			if body.Resume != "" {
				writeError(w, &fleet.Error{
					Kind: fleet.ErrorInvalid,
					Message: "conversationId and resume are mutually exclusive: one starts a new " +
						"conversation, the other continues one (#224)",
					Machine: machine,
				})
				return
			}
			if err := fleet.ValidateConversationId(body.ConversationId); err != nil {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#224)", Machine: machine})
				return
			}
		}

		// Two things in this body ask for more than a create.
		//
		// A CONSENT (`trustCwd`, `consents`) asks the driver to produce a
		// KEYPRESS on the caller's behalf, which is what `respond` does and what
		// `send` grants (see grantForVerb: answering a prompt shares that grant
		// because it has the same blast radius).
		//
		// A non-default `permissionMode` asks for a session that ACTS WITHOUT
		// ASKING — and raises an acceptance screen that then has to be answered.
		// A principal permitted only to start sessions must not be able to start
		// that one; between "may start a session" and "may start a session that
		// needs no permission for anything", the second is plainly the larger
		// authority.
		//
		// Folding either into `create` would be the sort of quiet privilege
		// widening that is invisible until it is someone's incident: nobody
		// reviewing a grants table would see it.
		//
		// Only checked when this machine is the one serving the create. A
		// relayed create is authorized by the PEER, against the same
		// credential, under its own table — §13's "proxying does not launder
		// authorization" — and a second opinion here could only disagree with
		// the host that actually holds the session.
		if elevated := createNeedsSend(body); elevated != "" && (machine == "" || machine == svc.Self()) {
			if p, ok := principalOf(r); ok && !p.Allows(GrantSend) {
				writeError(w, &fleet.Error{
					Kind: fleet.ErrorUnauthorized,
					Message: "principal " + p.Name + " does not hold the " +
						string(GrantSend) + " grant, which " + elevated + " requires (§6)",
					Machine: machine,
				})
				return
			}
		}

		// create has no id yet to resolve existence-first against — that is
		// exactly the "genuine miss" shape resolveSessionDriver falls to the
		// configured default for when body.Runtime names none (§60).
		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), requestFrom(r), machine, "", body.Runtime, parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		// #180: refuse env naming a variable this machine's delivery module
		// reserves, as a 400 naming it. A relaying driver does not report
		// reserved names; the owning machine's handler checks its own.
		if rr, ok := d.(driver.ReservedEnvReporter); ok {
			if err := delivery.CheckReservedEnv(body.Env, rr.ReservedEnv()); err != nil {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#180)", Machine: machine})
				return
			}
		}
		// #185: the same guard by prefix. A delivery module declares the
		// prefixes it reserves in its own handshake, so they cannot be listed
		// ahead of time; the driver answers from what it last recorded, which
		// is why this holds whether or not the module is running right now.
		if pr, ok := d.(driver.ReservedEnvPrefixReporter); ok {
			if err := delivery.CheckReservedEnvPrefixes(body.Env, pr.ReservedEnvPrefixes()); err != nil {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#185)", Machine: machine})
				return
			}
		}

		// muster #256: a resume that names no permissionMode (or no settings)
		// launches with what the conversation's previous session ran in. Only for
		// a create this machine serves — a relayed one is carried by the peer that
		// holds the conversation's record, and the answer comes back in its response.
		var launch resumeLaunch
		if via != resolvedPeer {
			var cerr *fleet.Error
			launch, cerr = svc.carryResumeLaunch(r, machine, resolvedRuntime, &body)
			if cerr != nil {
				writeError(w, cerr)
				return
			}
			// The explicit ordinary mode is spelled "default" on the wire and
			// empty everywhere below the handler: no driver learns a new value.
			if body.PermissionMode == permissionModeDefaultRequest {
				body.PermissionMode = ""
			}
		}

		spec := fleet.SessionSpec{
			// Machine is filled from the URL path, not the request body
			// (session.go's SessionSpec doc comment) — the wire body
			// deliberately does not repeat a value the path already
			// carries.
			Machine: machine, Runtime: body.Runtime, Cwd: body.Cwd,
			Agent: body.Agent, Model: body.Model, Effort: body.Effort,
			Name: body.Name, Prompt: body.Prompt, ContextRef: body.ContextRef,
			Marker: body.Marker, RemoteControl: body.RemoteControl,
			TrustCwd: body.TrustCwd, Env: body.Env, Resume: body.Resume,
			ConversationId: body.ConversationId, IsolateEnvironment: body.IsolateEnvironment,
			PermissionMode: body.PermissionMode, Consents: body.Consents,
			McpConfig: body.McpConfig, Labels: body.Labels,
			Settings: body.Settings,
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		sess, err := d.Create(ctx, requestFrom(r), key, spec)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}

		// muster #84/#85/#86: the driver builds the response now — it is
		// the only party that knows what a create actually did — so this
		// handler no longer synthesizes a Session from the caller's own
		// request. Two fields still need a fallback here, both for reasons
		// that predate this change and still apply unchanged:
		//
		// Runtime on the response names the driver that ACTUALLY served this
		// on THIS machine — resolvedRuntime, not spec.Runtime/body.Runtime,
		// which is empty on precisely the request this resolution exists
		// for: a caller that supplied no hint and got the default. Echoing
		// the (possibly empty) caller hint back here would silently discard
		// the one piece of information guardrail 2 asks this response to
		// carry. resolvedRuntime is itself empty only for a relayed create
		// (resolvedPeer) — this machine never learns which runtime the PEER
		// used, so the caller's own hint is the best available answer there,
		// same as before this resolution existed. A driver that already
		// filled Runtime itself (a relayed create adopting the peer's own
		// answer, §13.2) is left alone.
		if sess.Runtime == "" {
			sess.Runtime = resolvedRuntime
		}
		if sess.Runtime == "" {
			sess.Runtime = spec.Runtime
		}
		// State: most drivers do not read it back inside Create (a fresh
		// session's state is knowable the moment it is asked for, not
		// necessarily cheaper to compute inline), so this is the ordinary
		// path, not a fallback for a driver that forgot. A driver that DID
		// resolve its own State inside Create (a relayed create adopting the
		// peer's) is left alone.
		if sess.State.Status == "" {
			sess.State, _ = d.State(ctx, requestFrom(r), sess.SessionRef)
		}
		// Labels (muster #153). A local session's are this service's to
		// store; a relayed create's are the peer's, and arrive on its answer.
		if via != resolvedPeer {
			svc.history.created(resolvedRuntime, sess, launch.facts)
			if sess.Carried == nil {
				sess.Carried = launch.carried
			}
			stored, added := svc.labels.setIfAbsent(resolvedRuntime, sess.ID, sess.StartedAt, body.Labels)
			sess.Labels = stored
			if added {
				svc.publishLabels(svc.Self(), sess)
			}
		}
		writeJSON(w, http.StatusCreated, sess)
	}
}

// handleSessionEnvironment reports what a session's process actually received
// (fleet.SessionEnvironment).
//
// It answers "did this session get what a launcher-created one gets" from a
// read rather than from an ssh session and two process-manager unit files, and
// it is the reason the login-shell wrapping is an accepted dependency rather
// than an invisible one: the service still does not own the shell startup file,
// but it can now say what came out of it.
//
// A driver that cannot answer is reported as such, not as an empty
// environment — §5.7, and doubly so here, where "no variables" and "we did not
// look" would otherwise be the same response.
func handleSessionEnvironment(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		req := requestFrom(r)

		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, fleet.RuntimeId(r.URL.Query().Get("runtime")), parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)
		reporter, ok := d.(driver.EnvironmentReporter)
		if !ok {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: "this runtime cannot report a session's environment",
				Machine: machine,
			})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		env, err := reporter.Environment(ctx, req, fleet.SessionRef{Machine: machine, ID: id})
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusOK, env)
	}
}

func handleGetSession(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))
		req := requestFrom(r)

		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, runtimeHint, parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ref := fleet.SessionRef{Machine: machine, ID: id}

		// Answered from a listing rather than from State(), because State()
		// returns only a SessionState and this endpoint must return a
		// Session (api-http.md §3.3: cwd, agent, model, startedAt).
		//
		// The omission was not cosmetic. `startedAt` is what a caller quotes
		// back to make a destroy corroborable (§5.4), so a response without
		// it left the strong guarantee unreachable through the very endpoint
		// a caller would read before destroying something — a guarantee is
		// only as reachable as the data needed to invoke it.
		//
		// The cost is one enumeration, which is a constant number of
		// subprocess spawns on the driver that motivated that design; a
		// per-session query would not be cheaper.
		if col, err := d.List(ctx, req, driver.ListFilter{}); err == nil {
			for _, s := range col.Items() {
				if s.ID == id {
					if via != resolvedPeer {
						s.Labels = svc.labels.get(resolvedRuntime, s.ID, s.StartedAt)
					}
					writeJSON(w, http.StatusOK, s)
					return
				}
			}
		}

		// Not in the listing: fall through to the driver's own answer, which
		// distinguishes "looked and it is gone" from "could not look" (§5.7).
		state, err := d.State(ctx, req, ref)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusOK, fleet.Session{SessionRef: ref, Runtime: resolvedRuntime, State: state})
	}
}

func handleSendInput(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))

		var body struct {
			Text string `json:"text"`
			// Submit (#257): absent or null means true. An explicit false
			// keeps its meaning (stage the text, do not submit it).
			Submit *bool `json:"submit"`
			// ResumeIfStranded completes a delivery this service already made
			// and could not confirm. It never submits text the service did not
			// place there — see driver.SendOptions.
			ResumeIfStranded bool `json:"resumeIfStranded,omitempty"`
			// ReplaceIfStranded (muster #112) clears a composer this
			// service's own record shows it stranded, and delivers THIS
			// call's DIFFERENT text in its place — see driver.SendOptions.
			// Easy to forget forwarding, the same way #33 already warned
			// ResumeIfStranded is: it has no effect unless explicitly set,
			// so a caller that never sets it sees no symptom at all.
			ReplaceIfStranded bool `json:"replaceIfStranded,omitempty"`
			// Expect (#180) is the composer digest the caller read and is
			// prepared to have cleared or submitted — the draft rule's
			// caller-supplied proof. See driver.SendOptions.
			Expect string `json:"expect,omitempty"`
			// From (muster #158) labels the message with who it says
			// it comes from. Its Machine is replaced by stampSender below,
			// never passed through.
			From *fleet.MessageFrom `json:"from,omitempty"`
			// Route (terminal path v2, item g / D7; #184) chooses the delivery
			// path: "" or "auto" (the service picks by who is sending),
			// "terminal" (the session's composer), or "inbox" (the runtime's own
			// messaging socket, arriving as a peer message). Anything else is a
			// caller error, rejected below rather than silently ignored — the
			// same discipline this service already applies to a malformed body.
			Route string `json:"route,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body", Machine: machine})
			return
		}
		from := svc.stampSender(r, body.From)
		humanRelay := svc.humanRelay(r)
		submit := body.Submit == nil || *body.Submit

		if ferr := rejectOverLength("text", body.Text, machine, svc.MaxInputBytes()); ferr != nil {
			writeError(w, ferr)
			return
		}

		// "route" is a closed set (#184). Rejected here, before a driver ever
		// sees it, for the same reason #112's contradictory-flags check runs
		// ahead of everything in Send: a caller error about the REQUEST'S OWN
		// SHAPE should never depend on which session exists or whether a
		// driver can be resolved for it.
		route := fleet.RouteAuto
		switch fleet.Route(body.Route) {
		case "", fleet.RouteAuto:
		case fleet.RouteTerminal:
			// #257: an explicit terminal route from a caller with no printable
			// `from` and no human-relay grant is labelled with the
			// authenticated principal below (labelledSender), not refused: the
			// #180 M8 guarantee that an agent's send is never recorded as
			// human-typed input holds because the label prints.
			route = fleet.RouteTerminal
		case fleet.RouteInbox:
			// The inbox has no composer: a text that is not submitted, or a
			// resume/replace of a delivery stranded in one, names a composer
			// concept it cannot honour. Refused as the request's own shape, not
			// discovered per session — an explicit route is never quietly
			// downgraded, and this is the same fact stated up front.
			if !submit || body.ResumeIfStranded || body.ReplaceIfStranded {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid,
					Message: "route \"inbox\" delivers a message as a turn: it cannot be combined with " +
						"submit:false, resumeIfStranded or replaceIfStranded, which name a composer " +
						"the inbox does not have",
					Machine: machine})
				return
			}
			route = fleet.RouteInbox
		default:
			// #185: an enabled delivery module's own name forces that module.
			// The set is this machine's configuration, so it is checked here,
			// in the request-shape switch, before any driver is resolved — an
			// unknown name is a 400 whatever session or runtime it targets.
			if svc.isDeliveryModuleRoute(body.Route) {
				// A module carries the user's own turn exactly as the
				// terminal path does; a caller with no printable `from` and no
				// human-relay grant is labelled with its principal below
				// (#257), not refused.
				// A module has no composer: submit:false, or a resume/replace
				// of a delivery stranded in one, names a concept it cannot
				// honour. Refused up front, never discovered per session.
				if !submit || body.ResumeIfStranded || body.ReplaceIfStranded {
					writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid,
						Message: fmt.Sprintf("route %q delivers a message as a turn: it cannot be combined with "+
							"submit:false, resumeIfStranded or replaceIfStranded, which name a composer a "+
							"delivery module does not have", body.Route),
						Machine: machine})
					return
				}
				route = fleet.Route(body.Route)
				break
			}
			accepted := `"auto" (or omitted), "terminal" and "inbox"`
			if names := svc.deliveryModuleRouteNames(); len(names) > 0 {
				quoted := make([]string, len(names))
				for i, n := range names {
					quoted[i] = strconv.Quote(n)
				}
				accepted = `"auto" (or omitted), "terminal", "inbox" and ` + strings.Join(quoted, ", ")
			}
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid,
				Message: fmt.Sprintf("route %q is not recognised; the accepted values are %s", body.Route, accepted),
				Machine: machine})
			return
		}

		// #184, #257: the service decides who is sending and labels; the driver
		// decides lane, inbox and terminal. A human relay's message is the
		// user's own words: it stays unlabelled and keeps its route (an auto
		// stays auto and crosses a peer as auto, the machine that owns the
		// session decides). Anyone else's is labelled, with what the service
		// actually knows when the caller said nothing, on every route. The
		// decision reads the principal's own grant (or a trusted relay's
		// assertion of it), never a header or a `from` a caller set.
		if !humanRelay {
			from = svc.labelledSender(r, from)
		}

		req := requestFrom(r)
		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, runtimeHint, parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		receipt, err := d.Send(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, body.Text, driver.SendOptions{Submit: submit, ResumeIfStranded: body.ResumeIfStranded, ReplaceIfStranded: body.ReplaceIfStranded, ExpectComposerDigest: body.Expect, From: from, Route: route, HumanRelay: humanRelay, RemoteControl: svc.remoteControl(r), LiveLaneOnly: true})
		if err != nil {
			// A refusal from the driver is not this branch — Send returns
			// it as a DeliveryReceipt value, not an error. Only a
			// driver-level failure (unsupported, deadline exceeded, ...)
			// reaches here.
			writeDriverError(w, machine, deadline, err)
			return
		}
		// api-http.md §3.3: a refusal is 200, not an HTTP error — this is
		// the same 200 whether Outcome is submitted, queued, refused, or
		// unknown.
		writeJSON(w, http.StatusOK, receipt)
	}
}

// labelledSender makes a non-human sender's label real (#184). The terminal
// path puts the label on the message as its first line and the inbox carries it
// in the envelope, and either way it is what lets a receiver tell an agent's
// text from a person's. A caller that names itself keeps exactly what it said;
// a caller that names nothing gets the one fact this service actually holds —
// the authenticated principal's name — instead of arriving as an anonymous
// message that reads like a human typed it.
//
// `from` is the value stampSender already produced, so Machine is settled. Only
// a missing self-description is filled: when the caller gave neither an agent
// nor a session, Agent becomes the principal (on a trusted relay, the principal
// the request is made on behalf of). With no principal table the request has no
// authenticated identity at all, and the label is the machine alone; if even
// that is empty the agent reads "unidentified" rather than sending nothing.
//
// The result is a pure function of the request, so a resumeIfStranded retry
// carrying the same body is labelled identically and still matches the record
// it is resuming.
func (svc *Service) labelledSender(r *http.Request, from *fleet.MessageFrom) *fleet.MessageFrom {
	out := fleet.MessageFrom{}
	if from != nil {
		out = *from
	}
	behalf := r.Header.Get(onBehalfOfHeader)
	relayed := behalf != "" && svc.relayTrusted(r)
	if from == nil && !relayed {
		// stampSender is nil-in, nil-out, so a request that named nobody was
		// never stamped. A direct request entered HERE; a relayed one entered
		// somewhere this service was not told, and naming the owning machine as
		// its origin would be a guess.
		out.Machine = svc.Self()
	}
	if strings.TrimSpace(out.Agent) == "" && strings.TrimSpace(out.Session) == "" {
		if relayed {
			out.Agent = behalf
		} else if p, ok := principalOf(r); ok {
			out.Agent = p.Name
		}
	}
	if inboxclient.SenderName(driver.SenderLabel(&out)) == "" {
		out.Agent = "unidentified"
	}
	return &out
}

// stampSender applies muster #158's one rule about a sender's machine:
// the service stamps it, the caller never supplies it.
//
// The machine is where the request ENTERED the fleet. For a direct request
// that is this machine. For a relayed one it is the machine that relayed it,
// which forwarded its own stamp in from.machine — accepted here only because
// the request arrives as a relay (it carries the on-behalf-of assertion) and
// only when it names one of this machine's configured peers. That is the same
// bound the on-behalf-of assertion itself has: trusted exactly as far as the
// relay is. Anything else, and the machine is omitted rather than guessed —
// including a peer whose configured name here differs from its own name for
// itself, which FLEET_PEERS permits.
//
// Nil in, nil out: no `from` means unlabelled, exactly as before #158.
func (svc *Service) stampSender(r *http.Request, from *fleet.MessageFrom) *fleet.MessageFrom {
	if from == nil {
		return nil
	}
	out := *from
	out.Machine = ""
	// #180 M8: an on-behalf-of header from a caller this service does not
	// trust to relay is ignored — the request is treated as direct, and
	// stamped with this machine, rather than letting any caller blank its
	// own label's machine by claiming to be a relay.
	if r.Header.Get(onBehalfOfHeader) == "" || !svc.relayTrusted(r) {
		out.Machine = svc.Self()
	} else if claimed := from.Machine; claimed != "" && claimed != svc.Self() {
		if _, isPeer := svc.peerDrivers()[claimed]; isPeer {
			out.Machine = claimed
		}
	}
	return &out
}

// handleRespond answers a prompt a session is blocked on (§3).
//
// A refusal is a 200 carrying an outcome, exactly as for input (api-http.md
// §3.3): "this session is not at a prompt" is a fact about the session, not a
// fault in the request, and mapping it to 4xx would train callers to retry it.
func handleRespond(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		req := requestFrom(r)
		d, resolvedRuntime, via, ferr := svc.resolveSessionDriver(r.Context(), req, machine, id, fleet.RuntimeId(r.URL.Query().Get("runtime")), parseDeadline(r))
		if ferr != nil {
			writeError(w, ferr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)
		var body fleet.Response
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed body: " + err.Error()})
			return
		}
		// Before any driver sees it, local or peer: a contradictory body is
		// the caller's fault, and an empty choices set would be marshalled
		// away on the way to a peer and arrive there as "accept the
		// highlighted option" (see fleet.Response.Validate).
		if err := body.Validate(); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		// muster#206: a free-text answer is typed into a field of the same
		// runtime input reads, so it is bounded by the same limit and for the
		// same reason (#114) — at this boundary, for a local and a relayed
		// request alike, before a driver is asked to type it.
		if body.Text != nil {
			if ferr := rejectOverLength("text", *body.Text, machine, svc.MaxInputBytes()); ferr != nil {
				writeError(w, ferr)
				return
			}
		}
		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		receipt, err := d.Respond(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, body)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}

func handleInterrupt(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))
		req := requestFrom(r)

		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, runtimeHint, parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ack, err := d.Interrupt(ctx, req, fleet.SessionRef{Machine: machine, ID: id})
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

func handleClose(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))
		req := requestFrom(r)

		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, runtimeHint, parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ack, err := d.Close(ctx, req, fleet.SessionRef{Machine: machine, ID: id})
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		if via != resolvedPeer {
			svc.labels.remove(resolvedRuntime, id)
			svc.history.closedByRequest(resolvedRuntime, id)
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

func handleDiscard(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		req := requestFrom(r)
		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, fleet.RuntimeId(r.URL.Query().Get("runtime")), parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)
		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		// muster#136: force is the escape hatch past a proven-futile
		// ordinary pass. It is its own query parameter, deliberately not
		// folded into expect — expect keeps meaning exactly what it always
		// has (the digest the caller last saw), and force is a SEPARATE
		// opt-in a caller adds on top, never inferred from the presence or
		// shape of expect.
		opts := driver.DiscardOptions{Force: r.URL.Query().Get("force") == "true"}
		ack, err := d.Discard(ctx, req, fleet.SessionRef{Machine: machine, ID: id},
			r.URL.Query().Get("expect"), opts)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

func handleRename(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))

		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body", Machine: machine})
			return
		}
		// Trimmed ONCE, here, and used everywhere below (muster #222 review
		// fix): before this, publishRename/labels.rekey/history.renamed read the
		// UNTRIMMED body.Name while tmux's own Rename renamed to the trimmed one
		// (tmux.go strips it independently), so a name with incidental leading or
		// trailing whitespace announced and labelled a DIFFERENT string than the
		// one the multiplexer actually carried.
		name := strings.TrimSpace(body.Name)
		if name == "" {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "rename needs a non-empty name", Machine: machine})
			return
		}

		req := requestFrom(r)
		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, runtimeHint, parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ack, err := d.Rename(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, name)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}

		// Announce it. A subscriber filtering by id would otherwise see the old
		// id go quiet and a stranger appear — indistinguishable from a session
		// dying and another being created, which is exactly the wrong reading.
		//
		// This is the accept-time half only (RenameAccepted). muster #103:
		// a rename that reverts still needs a later, honest word on the stream
		// about it, which publishRename's own corroboration watch supplies —
		// see rename_corroboration.go.
		//
		// Deliberately BEFORE the title-sync step below (muster #222): the
		// id-change announcement must stay exactly as timely as it was before
		// that step existed — nothing about bringing the runtime's title along
		// is allowed to delay a subscriber learning the id itself already moved.
		svc.publishRename(machine, id, name, req.Expect.StartedAt)
		// Labels follow the session to its new id (muster #153), and the
		// title half is synced on the runtime, on a LOCAL driver only — via ==
		// resolvedPeer means d is a remote driver, and the peer machine's own
		// service already ran both of these against its own state; running them
		// again here would rekey by a runtime id this machine never resolved,
		// and ack.Title is already exactly what the peer answered (see
		// remote.Driver.Rename). A rename that later reverts is caught by the
		// label store's retain either way.
		if via != resolvedPeer {
			svc.labels.rekey(resolvedRuntime, id, name)
			svc.history.renamed(resolvedRuntime, id, name)
			ack.Title = syncTitle(ctx, d, req, fleet.SessionRef{Machine: machine, ID: name})
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

// handleLabels changes a session's labels after it exists (POST …/labels,
// muster #153): merge semantics, and a null value deletes its key.
//
// It corroborates against startedAt the way rename does, for the same
// reason: labelling the wrong session succeeds silently and binds a stranger
// to somebody's work. With ?startedAt= a disagreement — or a live session
// whose startedAt nobody knows — is a 409. Without it the write gets the
// weaker id-only guarantee.
//
// The answer is the whole session, labels included, so a caller never has to
// read back what it just wrote.
func handleLabels(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))

		var body struct {
			Labels map[string]*string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body", Machine: machine})
			return
		}
		if body.Labels == nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid,
				Message: `labels needs a "labels" object; a null value deletes that key`, Machine: machine})
			return
		}
		for k := range body.Labels {
			if err := fleet.ValidateLabelKey(k); err != nil {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#153)", Machine: machine})
				return
			}
		}

		req := requestFrom(r)
		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, runtimeHint, parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()
		ref := fleet.SessionRef{Machine: machine, ID: id}

		if via == resolvedPeer {
			relayer, ok := d.(driver.LabelRelayer)
			if !ok {
				writeDriverError(w, machine, deadline, driver.ErrUnsupported)
				return
			}
			sess, err := relayer.Labels(ctx, req, ref, body.Labels)
			if err != nil {
				writeDriverError(w, machine, deadline, err)
				return
			}
			writeJSON(w, http.StatusOK, sess)
			return
		}

		// The live session, read the way handleGetSession reads it: from a
		// listing, because that is where startedAt is.
		var live *fleet.Session
		if col, err := d.List(ctx, req, driver.ListFilter{}); err == nil {
			for _, s := range col.Items() {
				if s.ID == id {
					s := s
					live = &s
					break
				}
			}
		}
		if live == nil {
			state, err := d.State(ctx, req, ref)
			if err != nil {
				writeDriverError(w, machine, deadline, err)
				return
			}
			live = &fleet.Session{SessionRef: ref, State: state}
		}
		if want := req.Expect.StartedAt; want != nil && !startedAtMatches(want, live.StartedAt) {
			msg := "startedAt disagrees with the live session: it has been replaced since you looked (§5.4)"
			if live.StartedAt == nil {
				msg = "startedAt was supplied and this session's start time is unknown, so the write cannot be corroborated (§5.4)"
			}
			writeError(w, &fleet.Error{Kind: fleet.ErrorConflict, Message: msg, Machine: machine})
			return
		}

		merged, err := svc.labels.merge(resolvedRuntime, id, live.StartedAt, body.Labels)
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#153)", Machine: machine})
			return
		}
		live.Labels = merged
		if live.Runtime == "" {
			live.Runtime = resolvedRuntime
		}
		if live.Machine == "" {
			live.Machine = machine
		}
		svc.publishLabels(machine, *live)
		writeJSON(w, http.StatusOK, *live)
	}
}

// handleRemoteControl turns a running session's remote control on or off
// (api-http.md §3.3, muster #269).
//
// It returns 202, intent only, like interrupt: the confirmation is the session's
// controlChannel changing, delivered on the event stream. The body is
// {"enabled": true|false}; a body without it is invalid, because a default here
// would be a default for "publish this session off the machine".
//
// # Declared, not assumed
//
// A driver answers this only when it declares DriverCapabilities.RemoteControl.
// A driver without the optional interface is `unsupported`, and so is
// enabled:false against a driver that declared it can turn remote control on
// but not off — refused rather than emulated (§5.6).
//
// # Relay
//
// mutating() has already required the `relay` grant for a peer target, and the
// peer applies its own `remote-control` grant to this service's credential. A
// peer built before the route answers with its router's bare 404; the remote
// driver reports that as unsupported, never as a missing session.
func handleRemoteControl(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")

		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body", Machine: machine})
			return
		}
		if body.Enabled == nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid,
				Message: `remote-control needs {"enabled": true|false}`, Machine: machine})
			return
		}

		req := requestFrom(r)
		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, fleet.RuntimeId(r.URL.Query().Get("runtime")), parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)

		setter, ok := d.(driver.RemoteControlSetter)
		if !ok {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported,
				Message: "this runtime cannot turn remote control on or off on a running session", Machine: machine})
			return
		}
		// A local driver's declaration is known here; a peer's is checked by the
		// peer itself (the call is relayed), so only refuse on what is certain.
		if rc := d.Capabilities().RemoteControl; via != resolvedPeer && !*body.Enabled && (rc == nil || !rc.Off) {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported,
				Message: "this runtime can turn remote control on but declares no way to turn it off (capabilities.remoteControl.off is false)", Machine: machine})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ack, err := setter.SetRemoteControl(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, *body.Enabled)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

// handleKeys delivers one raw key event to a session's screen (api-http.md
// §3.3).
//
// # Why this is not a flag on respond
//
// `respond` refuses whenever the driver sees no prompt, and that refusal is the
// whole of its safety: a keypress delivered to a session that is not asking
// anything is consumed invisibly by whatever it is doing. The screens this
// endpoint exists for are exactly the ones the classifier does not recognise,
// so folding it into `respond` would mean relaxing that check for precisely the
// case it was written to exclude. A separate route pays for its own safety —
// `expect`, and the driver's refusals — instead of spending `respond`'s.
//
// A refusal is a 200 carrying an outcome, as for input and respond. A stale or
// missing `expect` is a 409: the request is well formed and the caller's belief
// is out of date (§5.4), which is the same answer `discard` gives for the same
// reason.
func handleKeys(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")

		var body struct {
			Key fleet.KeyName `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			// The decoder rejects an unknown key name itself (fleet.KeyName's
			// UnmarshalJSON), which is why this message names the vocabulary:
			// a caller told only "malformed body" would go looking at its JSON.
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorInvalid,
				Message: "malformed body: " + err.Error() + " (keys this API delivers: " +
					keyVocabulary() + ")",
				Machine: machine,
			})
			return
		}
		if !body.Key.Valid() {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorInvalid,
				Message: "key is required; one of " + keyVocabulary(),
				Machine: machine,
			})
			return
		}

		req := requestFrom(r)
		d, resolvedRuntime, via, resErr := svc.resolveSessionDriver(r.Context(), req, machine, id, fleet.RuntimeId(r.URL.Query().Get("runtime")), parseDeadline(r))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		setResolutionHeaders(w, resolvedRuntime, via)
		sender, ok := d.(driver.KeySender)
		if !ok {
			// §5.6: a driver that cannot press a key says so, and nothing here
			// approximates one out of `input` — which would break input's own
			// guarantee that a message never becomes a keystroke.
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: "this runtime cannot deliver a raw key event",
				Machine: machine,
			})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		receipt, err := sender.Keys(ctx, req, fleet.SessionRef{Machine: machine, ID: id},
			body.Key, r.URL.Query().Get("expect"))
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}

func keyVocabulary() string {
	names := fleet.KeyNames()
	out := make([]string, 0, len(names))
	for _, k := range names {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

// handleEvents streams events as Server-Sent Events (api-http.md §4).
//
// # The framing question, resolved
//
// fleet.Event's doc comment recorded an open question: does Kind travel as the
// SSE "event:" line, as a JSON property inside "data:", or both? Both.
//
//   - "event:" so a browser EventSource can addEventListener by kind, which
//     is the entire reason SSE has the field;
//   - "kind" inside the payload so a client reading the stream as newline
//     framed JSON — every non-browser consumer — does not have to parse SSE
//     framing to learn what it received;
//   - "id:" carries the cursor, so a reconnecting EventSource sends
//     Last-Event-ID automatically and resumption works without the client
//     writing any resumption code.
//
// Duplicating kind is redundancy chosen deliberately: the alternative is a
// stream that is only usable from one kind of client, and the cost is a short
// string per event.
func handleEvents(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported,
				Message: "this server cannot stream"})
			return
		}

		q := r.URL.Query()
		filter := driver.SubscribeFilter{
			Sessions:  q["session"],
			CwdPrefix: q.Get("cwdPrefix"),
		}
		fromCursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
		fromEpoch := q.Get("epoch")
		// A browser reconnecting supplies Last-Event-ID rather than a query
		// parameter; honour it so resumption needs no client code.
		if lastID := r.Header.Get("Last-Event-ID"); lastID != "" && fromCursor == 0 {
			fromCursor, _ = strconv.ParseInt(lastID, 10, 64)
			if fromEpoch == "" {
				fromEpoch = svc.Epoch()
			}
		}

		// §13.1: a proxied subscription asks for the peer's LOCAL view, and
		// a peer receiving it answers for itself without forwarding. The
		// default is fleet, matching plural reads (api-http.md §3.2).
		scope := ScopeFleet
		if q.Get("scope") == string(ScopeLocal) {
			scope = ScopeLocal
		}
		ch, backlog, needResync, cancel := svc.Events(r.Context(), scope, filter, fromCursor, fromEpoch)
		defer cancel()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		if needResync {
			// §7.3: announce the gap. A subscriber told this refetches
			// state; one silently resumed from an arbitrary point believes
			// it has a complete history and does not.
			reason := fleet.ResyncCursorExpired
			if fromEpoch != svc.Epoch() {
				reason = fleet.ResyncEpochChanged
			}
			writeSSE(w, flusher, fleet.Event{
				Epoch: svc.Epoch(), Machine: svc.Self(), Kind: fleet.EventControlResync,
				Payload: fleet.ControlResyncPayload{Reason: reason},
			})
		}
		for _, ev := range backlog {
			writeSSE(w, flusher, ev)
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case ev, open := <-ch:
				if !open {
					return
				}
				writeSSE(w, flusher, ev)
			}
		}
	}
}

// writeSSE encodes one event. Encoding failure ends the stream rather than
// emitting a partial frame: a truncated event is worse than a closed
// connection, because the client cannot tell it happened.
func writeSSE(w http.ResponseWriter, f http.Flusher, ev fleet.Event) {
	body, err := json.Marshal(envelopeOf(ev))
	if err != nil {
		return
	}
	if ev.Cursor > 0 {
		fmt.Fprintf(w, "id: %d\n", ev.Cursor)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, body)
	f.Flush()
}

// eventEnvelope is fleet.Event's wire form, shared by BOTH transports: it is
// what an SSE frame's "data:" line carries and what an entry of the long
// poll's "events" array is.
//
// One shape, deliberately. The long poll is a transport for the event
// vocabulary, not a second event model, and the moment it grew an envelope of
// its own the two would begin answering the same question differently. It
// exists rather than marshalling fleet.Event directly so the JSON shape is
// stated in one place and cannot drift with the in-memory type.
type eventEnvelope struct {
	Cursor  int64           `json:"cursor"`
	Epoch   string          `json:"epoch"`
	Machine fleet.MachineId `json:"machine"`
	Kind    fleet.EventKind `json:"kind"`
	Payload any             `json:"payload,omitempty"`
	// Origin is the relayed event's coordinates in its originating
	// service's sequence. Omitting it here once cost a live test its
	// provenance while every unit test still passed — a separate wire type
	// keeps the stream's shape in one place, but only if it is kept in step
	// with what it encodes.
	Origin *fleet.EventOrigin `json:"origin,omitempty"`
}

func isConversationIdError(err error) bool {
	var ce *fleet.ConversationIdError
	return errors.As(err, &ce)
}
