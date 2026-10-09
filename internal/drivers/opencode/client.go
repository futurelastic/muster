package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	fleet "github.com/futurelastic/muster"
)

// wireTime is the {created, updated} shape opencode stamps on a session,
// in epoch milliseconds.
type wireTime struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

// wireSession is the subset of opencode's Session schema this driver
// reads. Every other field (cost, tokens, share, permission, revert, ...)
// is left undecoded — encoding/json ignores fields with no matching tag,
// and this driver has no use for them.
type wireSession struct {
	ID        string   `json:"id"`
	Directory string   `json:"directory"`
	Title     string   `json:"title"`
	Agent     string   `json:"agent"`
	Time      wireTime `json:"time"`
}

// wireStatus is one entry of GET /session/status's response map — the
// three-variant union from #55's measured findings (idle | busy | retry).
// The "idle" arm is decoded defensively even though the measured behaviour
// never puts it in the map (absence means idle instead) — see state.go's
// classify.
type wireStatus struct {
	Type    string `json:"type"`
	Attempt int    `json:"attempt"`
	Message string `json:"message"`
	Next    int    `json:"next"`
}

// statusMap is GET /session/status's whole response: session id to status,
// present only for a session with something to report.
type statusMap map[string]wireStatus

// wireAssistantError is the error union opencode's AssistantMessage.error
// carries when a turn ends without a reply (muster #77). Measured
// against a real server's own OpenAPI document: eight distinct error
// "name" values, each with its own "data" shape. Seven of the eight nest a
// human-readable "message" under data; the eighth (MessageOutputLengthError)
// carries an empty data object, so Data.Message decodes to "" for it —
// honestly, since the runtime itself says nothing further, not a gap this
// type papers over. IsRetryable is only ever populated by the runtime for
// the "APIError" variant; every other Name leaves it at its zero value,
// which lastTurnFailure reads as "not claimed retryable" rather than "not
// retryable" — the runtime never said either way for those.
//
// This driver never branches on Name or the message text — the same
// discipline TurnEnd.Reason already holds itself to everywhere else in
// this model — it only carries enough to report that a turn failed and
// why, for a human.
type wireAssistantError struct {
	Name string `json:"name"`
	Data struct {
		Message     string `json:"message"`
		IsRetryable bool   `json:"isRetryable"`
	} `json:"data"`
}

// wireMessageInfo is the subset of GET /session/{id}/message's per-message
// "info" object this driver reads. Deliberately excludes "parts" — the
// field the real endpoint nests each message's actual conversation content
// under. Not decoding it here is what makes reading it impossible by
// construction, the same discipline ScreenDigest already holds the tmux
// driver to: this answers "did the last turn fail and why", never "what
// was said".
type wireMessageInfo struct {
	Role string `json:"role"`
	Time struct {
		Created int64 `json:"created"`
		// Completed is set once the runtime finished the message. An assistant
		// message without it, in a session no turn is running in, is a turn that
		// was cut off (muster #282).
		Completed int64 `json:"completed"`
	} `json:"time"`
	Error *wireAssistantError `json:"error"`
}

// wireMessage is one entry of GET /session/{id}/message's response array —
// the {info, parts} envelope the real server's OpenAPI schema documents.
type wireMessage struct {
	Info wireMessageInfo `json:"info"`
}

// do performs one request against one of this driver's own opencode servers
// (the session's, in isolated mode), authenticating as this driver (never as the caller — see Driver.password's
// doc comment; a local driver ignores fleet.Caller.Credential entirely).
//
// A transport failure and an HTTP error status are both turned into a
// *fleet.Error here, at the one place that can tell them apart from a
// genuine "the runtime looked and answered" 2xx. Every caller in ops.go
// therefore receives either a clean decode or a typed error — never a
// response that merely looks empty, which is the distinction §5.7 and
// #55's second trap both turn on.
func (d *Driver) do(ctx context.Context, srv *server, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opencode: encoding request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, srv.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("opencode: building request: %w", err)
	}
	req.SetBasicAuth(srv.username, srv.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.client.Do(req)
	if err != nil {
		// A transport failure is unreachable, never not_found: nothing
		// answered, so nothing at all is known (api-http.md §2, the same
		// rule the remote driver's `do` follows for a peer).
		return &fleet.Error{
			Kind:      fleet.ErrorUnreachable,
			Message:   fmt.Sprintf("no answer from the local opencode server: %v", err),
			Machine:   d.machine,
			Retryable: true,
		}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// This driver generated its own credential and holds it for the
		// lifetime of the server it started; a 401 here means the two
		// have drifted apart (a bug in this package, or the server having
		// been restarted independently), not a caller-permission problem.
		// It still maps to unauthorized rather than a generic failure —
		// #55's read-failure discriminator applies to this driver's own
		// reads of ITS OWN server exactly as it would to any other.
		return &fleet.Error{
			Kind:    fleet.ErrorUnauthorized,
			Message: "opencode: the local server rejected this driver's own credential",
			Machine: d.machine,
		}
	case resp.StatusCode == http.StatusNotFound:
		return &fleet.Error{
			Kind:    fleet.ErrorNotFound,
			Message: fmt.Sprintf("opencode: %s %s: no such session", method, path),
			Machine: d.machine,
		}
	case resp.StatusCode >= 400:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &fleet.Error{
			Kind:    fleet.ErrorInvalid,
			Message: fmt.Sprintf("opencode: %s %s: %d %s", method, path, resp.StatusCode, string(b)),
			Machine: d.machine,
		}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("opencode: decoding response from %s %s: %w", method, path, err)
	}
	return nil
}

// isNotFound reports whether err is the ErrorNotFound this driver's own do
// produces for a 404 — the signal ops.go uses to decide between
// fleet.ErrNoSuchSession and fleet.InferredState(fleet.StatusDead, ...)
// via seen.
func isNotFound(err error) bool {
	fe, ok := err.(*fleet.Error)
	return ok && fe.Kind == fleet.ErrorNotFound
}

// isUnreachable reports whether err is the ErrorUnreachable do produces for a
// transport failure — nothing answered.
func isUnreachable(err error) bool {
	fe, ok := err.(*fleet.Error)
	return ok && fe.Kind == fleet.ErrorUnreachable
}

// sourceStateFor maps a transport-level failure onto the closed SourceState
// set (§9), the same discrimination remote.go's sourceStateFor makes for a
// peer — unauthorized and unreachable are kept distinct because they are
// different operational problems (a stale credential vs. a dead process)
// with different fixes.
func sourceStateFor(err error) fleet.SourceState {
	if fe, ok := err.(*fleet.Error); ok {
		switch fe.Kind {
		case fleet.ErrorUnauthorized:
			return fleet.SourceUnauthorized
		case fleet.ErrorUnreachable:
			return fleet.SourceUnreachable
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fleet.SourceUnreachable
	}
	return fleet.SourceDegraded
}
