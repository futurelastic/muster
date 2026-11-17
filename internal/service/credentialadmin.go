package service

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// The credentials routes (muster #498): link and unlink a runtime's provider
// credentials over HTTP.
//
//	PUT    /v1/runtimes/{runtime}/credentials/{provider}/api-key
//	DELETE /v1/runtimes/{runtime}/credentials/{provider}/api-key
//	POST   /v1/runtimes/{runtime}/credentials/{provider}/subscription/login
//	GET    /v1/runtimes/{runtime}/credentials/{provider}/subscription/login/{id}
//	DELETE /v1/runtimes/{runtime}/credentials/{provider}/subscription
//
// # Why they are a grant of their own, and not a rider on anything
//
// This service is the one party that starts the runtime and so the one that holds
// its credentials. Who may change which provider account this machine's sessions
// bill, or hand them a different key, is as local a decision as who may profile its
// process or replace its peer table. So `credentials` is its own grant (GrantCredentials)
// and nothing implies it: not read, send or relay, not the legacy all-verbs flags, not
// the set `muster service install` enrols the local service principal with, and a
// signed machine list that names it is refused whole.
//
// Reading a sign-in attempt needs it too. The code a pending attempt shows is the
// thing that signs this machine in to whichever account approves it, so a caller
// that can read the code can steer an approval.
//
// # The gate is not mutating()
//
// In the legacy single-token model mutating() lets every caller in when the all-verbs
// flag is set, and changing a machine's credentials must never be allowed without a
// per-principal grant. credentialsGate therefore refuses a machine with no principal
// table outright, as debugGate and pushListGate do.
//
// # What no path here ever carries
//
// A key value is read from the request body into a buffer that is written once and
// zeroed, never put in an error, a log line or a response. Every answer is a name, a
// kind and a state (the #497 row), or a sign-in attempt's address and code. The audit
// line names the verb, the runtime, the provider, the kind and the action, and never
// the value.
//
// Local only: the routes name no machine, so a request is never relayed to a peer. A
// peer's credentials are that peer's to change, with a principal that holds the grant
// there.

// maxCredentialBody bounds a request body on these routes. A key is at most a few
// thousand bytes.
const maxCredentialBody = 8 << 10

// isCredentialsPath reports whether path is under the credentials routes, so
// grantForVerb answers GrantCredentials for every method on them.
func isCredentialsPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v1/runtimes/")
	if !ok {
		return false
	}
	// {runtime}/credentials/...
	_, after, ok := strings.Cut(rest, "/")
	return ok && (after == "credentials" || strings.HasPrefix(after, "credentials/"))
}

// localDriverFor is the registered local driver for a runtime, and for a runtime
// that is enabled but has no working driver, why not.
func (s *Service) localDriverFor(rt fleet.RuntimeId) (driver.Driver, *fleet.Error) {
	s.mu.RLock()
	d, ok := s.local[rt]
	u, down := s.unavailable[rt]
	s.mu.RUnlock()
	switch {
	case ok:
		return d, nil
	case down:
		return nil, &fleet.Error{Kind: fleet.ErrorUnsupported, Machine: s.self, Reason: "runtime-unavailable",
			Message: "the " + string(rt) + " runtime is enabled on this machine but has no working driver (" + u.reason + ")"}
	}
	return nil, &fleet.Error{Kind: fleet.ErrorNotFound, Machine: s.self,
		Message: "this machine has no runtime \"" + clipID(string(rt)) + "\""}
}

// clipID renders a caller-supplied path segment in a message or an audit line: cut
// short and with anything but a plain id character dropped. It adds no quotes.
func clipID(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b = append(b, c)
		}
	}
	return string(b)
}

// credentialsGate admits a request to the credentials routes. See the file's note on
// why it is not mutating(). The grant is checked before anything about the runtime
// or the machine, so a caller without it learns nothing about either.
func credentialsGate(svc *Service, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = withTiming(r)
		p, ok := principalOf(r)
		if !ok {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnauthorized, Machine: svc.Self(),
				Message: "this machine has no principal table, so no caller can hold the credentials grant (§6): " +
					"its credentials can be changed only on a machine that identifies its callers"})
			return
		}
		if !p.Allows(GrantCredentials) {
			logCredentials(p, svc.Self(), r, 0)
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorUnauthorized, Machine: svc.Self(),
				Message: "principal " + p.Name + " does not hold the credentials grant (§6). " +
					"Every grant defaults to denied, on a fresh deployment as much as an " +
					"established one — this is expected until an operator adds credentials " +
					"to this principal's grants, not a bug.",
			})
			return
		}
		aw := &auditWriter{ResponseWriter: w}
		next(aw, r)
		logCredentials(p, svc.Self(), r, aw.status)
	}
}

// credentialAction names what a request on these routes does, for the audit line.
func credentialAction(r *http.Request) string {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/api-key") && r.Method == http.MethodPut:
		return "link"
	case strings.HasSuffix(path, "/api-key") && r.Method == http.MethodDelete:
		return "unlink"
	case strings.HasSuffix(path, "/subscription") && r.Method == http.MethodDelete:
		return "sign-out"
	case strings.HasSuffix(path, "/subscription/login") && r.Method == http.MethodPost:
		return "login-start"
	case strings.Contains(path, "/subscription/login/") && r.Method == http.MethodGet:
		return "login-status"
	}
	return "unknown"
}

// credentialKind is the kind of credential a request on these routes concerns, read
// from its path.
func credentialKind(r *http.Request) fleet.CredentialKind {
	if strings.Contains(r.URL.Path, "/subscription") {
		return fleet.CredentialSubscription
	}
	return fleet.CredentialAPIKey
}

// logCredentials writes the audit line of a credentials request: who, which verb,
// where, which provider and kind, what it did and how it ended — never a value, a
// code or a sign-in attempt's id. status 0 is a refusal for want of the grant.
func logCredentials(p Principal, target fleet.MachineId, r *http.Request, status int) {
	outcome := "DENIED"
	if status != 0 {
		outcome = outcomeOf(status)
		if status == http.StatusNotFound {
			// outcomeOf's word for a 404 is about sessions; here it is a runtime or an
			// attempt that is not held.
			outcome = "not-found"
		}
	}
	line := "audit: actor=" + strconv.Quote(actorOf(p, r)) + " verb=" + string(GrantCredentials) +
		" route=" + routeOf(r) + " target=" + string(target) + "/" + clipID(r.PathValue("runtime")) +
		" provider=" + clipID(r.PathValue("provider")) + " kind=" + string(credentialKind(r)) +
		" action=" + credentialAction(r) + " outcome=" + outcome
	if status != 0 {
		line += " status=" + strconv.Itoa(status)
	}
	log.Print(line)
}

// credentialManager resolves the runtime named in the path to a driver that can
// manage credentials, or writes the refusal.
func credentialManager(svc *Service, w http.ResponseWriter, r *http.Request) (driver.CredentialManager, string, bool) {
	d, derr := svc.localDriverFor(fleet.RuntimeId(r.PathValue("runtime")))
	if derr != nil {
		writeError(w, derr)
		return nil, "", false
	}
	m, ok := d.(driver.CredentialManager)
	if !ok {
		writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported, Machine: svc.Self(),
			Message: "the \"" + clipID(r.PathValue("runtime")) + "\" runtime does not take credentials through this service"})
		return nil, "", false
	}
	return m, r.PathValue("provider"), true
}

// writeCredentialError writes err as the *fleet.Error it should be. A driver that hands
// back anything else is refused as unreachable with a fixed sentence rather than its
// text, so no cause string a driver built can reach a caller from these routes.
func writeCredentialError(svc *Service, w http.ResponseWriter, err error) {
	if fe, ok := err.(*fleet.Error); ok {
		writeError(w, fe)
		return
	}
	writeError(w, &fleet.Error{Kind: fleet.ErrorUnreachable, Machine: svc.Self(), Retryable: true,
		Message: "the runtime could not complete the request"})
}

// handleLinkKey answers PUT …/api-key.
func handleLinkKey(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, provider, ok := credentialManager(svc, w, r)
		if !ok {
			return
		}
		value, ok := readKeyBody(svc, w, r)
		if !ok {
			return
		}
		defer zero(value)
		row, err := m.LinkKey(provider, value)
		if err != nil {
			writeCredentialError(svc, w, err)
			return
		}
		writeJSON(w, http.StatusOK, row)
	}
}

// readKeyBody reads {"value": "..."} from the body, bounded, and returns the value's
// bytes. Every refusal is a fixed sentence: a JSON decoder's own error can quote the
// input, so none of its text is passed on.
func readKeyBody(svc *Service, w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	bad := func(msg string) ([]byte, bool) {
		writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: svc.Self(), Message: msg})
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCredentialBody+1))
	defer zero(raw)
	if err != nil || len(raw) > maxCredentialBody {
		return bad("the body is unreadable or larger than " + strconv.Itoa(maxCredentialBody) + " bytes")
	}
	var body struct {
		Value *string `json:"value"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body.Value == nil || dec.More() {
		return bad(`the body must be a JSON object {"value": "<the key>"} and nothing else`)
	}
	return []byte(*body.Value), true
}

// zero overwrites b. It does not reach every copy a runtime may have made (a JSON
// decoder's string is immutable), and says nothing it cannot keep; what it ends is
// this handler's own long-lived buffers.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// handleUnlinkKey answers DELETE …/api-key.
func handleUnlinkKey(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, provider, ok := credentialManager(svc, w, r)
		if !ok {
			return
		}
		out, err := m.UnlinkKey(provider)
		if err != nil {
			writeCredentialError(svc, w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleLoginStart answers POST …/subscription/login with the attempt to show a
// person: a new one, or the one already waiting (one is pending at a time).
func handleLoginStart(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, provider, ok := credentialManager(svc, w, r)
		if !ok {
			return
		}
		out, err := m.StartLogin(provider)
		if err != nil {
			writeCredentialError(svc, w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleLoginStatus answers GET …/subscription/login/{id}.
func handleLoginStatus(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, provider, ok := credentialManager(svc, w, r)
		if !ok {
			return
		}
		out, err := m.LoginStatus(provider, r.PathValue("id"))
		if err != nil {
			writeCredentialError(svc, w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleSignOut answers DELETE …/subscription.
func handleSignOut(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, provider, ok := credentialManager(svc, w, r)
		if !ok {
			return
		}
		out, err := m.SignOut(provider)
		if err != nil {
			writeCredentialError(svc, w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}
