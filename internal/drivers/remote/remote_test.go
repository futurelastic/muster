package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

const peerToken = "the-token-the-peer-accepts"
const callerTok = "the-original-callers-token"

// caller is what a service derives from an inbound request and hands down.
var caller = fleet.Request{Caller: fleet.Caller{Principal: "addr:198.51.100.7", Credential: callerTok}}

// noAuthority is a caller with nothing to present. Every operation must
// refuse it — this driver holds no credential of its own to fall back to.
var noAuthority = fleet.Request{Caller: fleet.Caller{Principal: "addr:198.51.100.7"}}

// capture records what the peer actually received, which is where most of
// this driver's contract lives: the rules it must obey are about what goes
// on the wire, not about what it returns.
type capture struct {
	method string
	path   string
	query  string
	auth   string
	idem   string
	dline  string
	body   string
	// humanRelay is the #180 L3 assertion header.
	humanRelay string
	// remoteCtl is the #272 assertion header.
	remoteCtl string
}

func peerServing(t *testing.T, status int, payload any, rec *capture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// muster #67: a driver whose capabilities are unseen now
		// opportunistically probes them (noteSuccessfulContact) off the back
		// of ANY successful call, in its own goroutine, concurrently with
		// whatever the test does next. That probe hits this same server at
		// /v1/runtimes (and /v1/health, on the same round trip) — paths no
		// test using `rec` is ever exercising the operation under test
		// against. Recording into the single shared `rec` from that
		// goroutine too would be a real data race (two goroutines writing
		// the same memory with nothing ordering them) AND, race detector
		// aside, a real correctness bug: whichever request happened to land
		// last would silently overwrite what the test actually meant to
		// capture. Confining writes to the one goroutine handling the
		// operation under test — by never recording the probe's own
		// requests here — removes both, without serializing anything: the
		// probe still runs concurrently, it just never touches this slot.
		probing := r.URL.Path == "/v1/runtimes" || r.URL.Path == "/v1/health" || r.URL.Path == "/v1/whoami"
		if rec != nil && !probing {
			raw, _ := io.ReadAll(r.Body)
			*rec = capture{
				method:     r.Method,
				path:       r.URL.Path,
				query:      r.URL.RawQuery,
				auth:       r.Header.Get("Authorization"),
				humanRelay: r.Header.Get("Fleet-Human-Relay"),
				remoteCtl:  r.Header.Get("Fleet-Remote-Control"),
				idem:       r.Header.Get("Idempotency-Key"),
				dline:      r.Header.Get("Fleet-Deadline-Ms"),
				body:       string(raw),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if payload != nil {
			_ = json.NewEncoder(w).Encode(payload)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collectionJSON(sources []fleet.SourceStatus, items []fleet.Session) any {
	return map[string]any{"items": items, "sources": sources, "complete": true}
}

// §13.1: a proxied request asks for the peer's LOCAL view only. Without
// this, two mutually-configured peers query each other forever — or, worse,
// double-count and look fine.
func TestListAsksForThePeersLocalViewOnly(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), &rec)

	d := New("peerbox", srv.URL)
	if _, err := d.List(context.Background(), caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.query, "scope=local") {
		t.Errorf("query = %q, must contain scope=local (§13.1)", rec.query)
	}
	if strings.Contains(rec.query, "scope=fleet") {
		t.Error("a proxied call must never ask a peer to fan out further")
	}
}

// §13.2: adopt the peer's SourceStatus; never re-synthesize it. A peer can
// answer promptly AND report itself degraded.
func TestListAdoptsADegradedPeersOwnSourceStatus(t *testing.T) {
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{
			Machine: "peerbox", Status: fleet.SourceDegraded,
			Error: "one runtime is not answering", ObservedAt: time.Now(),
		}}, nil), nil)

	d := New("peerbox", srv.URL)
	got, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources()) != 1 {
		t.Fatalf("want exactly one adopted source, got %d", len(got.Sources()))
	}
	if got.Sources()[0].Status != fleet.SourceDegraded {
		t.Errorf("status = %q; the call succeeding must not overwrite the peer's "+
			"own self-report with ok (§13.2)", got.Sources()[0].Status)
	}
	if got.Sources()[0].Error == "" {
		t.Error("the peer's explanation was dropped in transit")
	}
	if got.Complete() {
		t.Error("a degraded source must not produce a complete envelope (§9)")
	}
}

// §5.7 across a network: a peer that cannot be reached contributes a
// SourceStatus, never an absence.
func TestUnreachablePeerIsASourceNotAnEmptyList(t *testing.T) {
	// A closed server: the connection fails at the transport layer.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	d := New("peerbox", url)
	got, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("an unreachable peer belongs in the envelope, not in err: %v", err)
	}
	if got.Complete() {
		t.Error("unreachable peer must not produce a complete envelope")
	}
	if len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceUnreachable {
		t.Fatalf("want one unreachable source, got %+v", got.Sources())
	}
	if got.Sources()[0].Machine != "peerbox" {
		t.Errorf("source machine = %q, want peerbox", got.Sources()[0].Machine)
	}
	if got.Sources()[0].Error == "" {
		t.Error("an unreachable source must say why (§9)")
	}
}

// api-http.md §2's "single most important line": not_found and unreachable
// must never be conflated. A client that treats 504 as 404 reports work as
// gone while it is running fine on an unreachable host.
func TestNotFoundAndUnreachableStaySeparate(t *testing.T) {
	cases := []struct {
		name   string
		status int
		kind   fleet.ErrorKind
		want   fleet.ErrorKind
	}{
		{"peer says no such session", 404, fleet.ErrorNotFound, fleet.ErrorNotFound},
		{"peer did not answer in time", 504, fleet.ErrorUnreachable, fleet.ErrorUnreachable},
		{"peer refused us", 401, fleet.ErrorUnauthorized, fleet.ErrorUnauthorized},
		{"driver lacks capability", 501, fleet.ErrorUnsupported, fleet.ErrorUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := peerServing(t, tc.status, fleet.ErrorEnvelope{
				Error: fleet.Error{Kind: tc.kind, Message: "from the peer", Machine: "peerbox"},
			}, nil)
			d := New("peerbox", srv.URL)
			_, err := d.State(context.Background(), caller, fleet.SessionRef{ID: "s1"})
			if err == nil {
				t.Fatal("expected an error")
			}
			var fe *fleet.Error
			if !errors.As(err, &fe) {
				t.Fatalf("error lost its kind in transit: %v", err)
			}
			if fe.Kind != tc.want {
				t.Errorf("kind = %q, want %q", fe.Kind, tc.want)
			}
		})
	}
}

// §13 / api-http.md §5: a proxy presents the ORIGINAL caller's authority.
//
// This used to be the design's most serious defect: authority travelled in a
// context value a service could forget, and a remote driver missing it fell
// back to its own token — succeeding, passing tests, and silently widening
// authorization. Authority is now a parameter, and this driver holds no
// credential at all, so the fallback has nothing to fall back to.
func TestEveryOperationPresentsTheCallersCredential(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hello", driver.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.auth != "Bearer "+callerTok {
		t.Errorf("Authorization = %q, want the caller's own credential (§13)", rec.auth)
	}
}

// Reads included. "Which sessions exist, in which directories, on which
// machine" is exactly the reconnaissance an unauthorized caller wants, and §6
// grants read permission broadly rather than universally.
func TestNoOperationProceedsWithoutCallerAuthority(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), &rec)
	d := New("peerbox", srv.URL)
	ctx := context.Background()

	// A read returns its refusal inside the envelope (§5.7), not as an error.
	col, err := d.List(ctx, noAuthority, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if col.Complete() {
		t.Error("List without authority must not report a complete envelope")
	}

	if _, err := d.State(ctx, noAuthority, fleet.SessionRef{ID: "s1"}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("State: want ErrNoCallerAuthority, got %v", err)
	}
	if _, err := d.Send(ctx, noAuthority, fleet.SessionRef{ID: "s1"}, "x", driver.SendOptions{}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("Send: want ErrNoCallerAuthority, got %v", err)
	}
	if _, err := d.Create(ctx, noAuthority, "k", fleet.SessionSpec{Cwd: "/w"}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("Create: want ErrNoCallerAuthority, got %v", err)
	}
	if _, err := d.Close(ctx, noAuthority, fleet.SessionRef{ID: "s1"}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("Close: want ErrNoCallerAuthority, got %v", err)
	}
	if _, err := d.Interrupt(ctx, noAuthority, fleet.SessionRef{ID: "s1"}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("Interrupt: want ErrNoCallerAuthority, got %v", err)
	}

	// The strongest form of the assertion: not one request was attempted.
	// A driver that reached the peer at all would have had to present
	// something, and there is nothing it could honestly present.
	if rec.auth != "" {
		t.Errorf("a request was made without caller authority, presenting %q", rec.auth)
	}
}

// §10: the caller's idempotency key must be forwarded unchanged. A proxy
// minting its own key per attempt defeats the mechanism precisely when it
// matters — a retried federated create would arrive with a different key and
// read as a different request, producing two agents in one directory.
func TestCreateForwardsTheCallersIdempotencyKeyUnchanged(t *testing.T) {
	var rec capture
	// A zero SessionState will not marshal: Status is a closed set and the
	// empty string is not a member (state.go). The peer must return a real
	// one, which is the enforcement working rather than a fixture nicety.
	srv := peerServing(t, 201, fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "new-1"},
		Runtime:    "claude-code-tmux",
		State:      fleet.InferredState(fleet.StatusStarting, "just created", nil),
	}, &rec)
	d := New("peerbox", srv.URL)

	ref, err := d.Create(context.Background(), caller, "caller-key-42", fleet.SessionSpec{Cwd: "/w", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.idem != "caller-key-42" {
		t.Errorf("Idempotency-Key = %q, want the caller's key verbatim (§10)", rec.idem)
	}
	if ref.ID != "new-1" {
		t.Errorf("ref = %+v", ref)
	}
}

// §2.4 / api-http.md §3.3: a refusal is a 200 carrying an outcome, not an
// HTTP error, and this driver must not convert it into one — that would
// train callers to retry the thing the refusal exists to prevent.
func TestRefusalSurvivesAsAValueNotAnError(t *testing.T) {
	srv := peerServing(t, 200, fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeRefused,
		Reason:  "composer holds unsent input",
	}, nil)
	d := New("peerbox", srv.URL)

	got, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hi", driver.SendOptions{})
	if err != nil {
		t.Fatalf("a refusal must not surface as an error: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused || got.Reason == "" {
		t.Errorf("receipt = %+v, want a refusal carrying its reason", got)
	}
}

// #33: Send builds its request body by hand rather than marshalling
// driver.SendOptions directly — unlike Respond, which forwards its argument
// verbatim — so a field added to that struct does not reach the wire until
// this driver is told about it separately. ResumeIfStranded was added to
// SendOptions and to the owning daemon's own body decoder, and never added
// here. The owning daemon therefore received a plain send, evaluated the
// flag as its zero value, and refused under §2.4 a delivery the caller was
// explicitly retrying to resume — on every path except the one where the
// caller happened to already be on the machine that owns the session.
func TestSendForwardsResumeIfStranded(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "the stranded text",
		driver.SendOptions{Submit: true, ResumeIfStranded: true}); err != nil {
		t.Fatal(err)
	}

	var body struct {
		ResumeIfStranded bool `json:"resumeIfStranded"`
	}
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body = %q: %v", rec.body, err)
	}
	if !body.ResumeIfStranded {
		t.Fatalf("body = %q, want resumeIfStranded:true carried through to the owning daemon (§2.4, #33)", rec.body)
	}
}

// muster #112: ReplaceIfStranded is exactly the same #33 trap one field
// over — this driver's Send body is hand-built, so a caller setting the
// flag on driver.SendOptions has no effect at all unless this test's own
// assertion holds.
func TestSendForwardsReplaceIfStranded(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "the replacement text",
		driver.SendOptions{Submit: true, ReplaceIfStranded: true}); err != nil {
		t.Fatal(err)
	}

	var body struct {
		ReplaceIfStranded bool `json:"replaceIfStranded"`
	}
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body = %q: %v", rec.body, err)
	}
	if !body.ReplaceIfStranded {
		t.Fatalf("body = %q, want replaceIfStranded:true carried through to the owning daemon (#112)", rec.body)
	}
}

// TestSendForwardsTerminalRoute is the review's regression test for the
// SAME #33 trap this file's own doc comments already name twice over, this
// time for terminal path v2 / D7's terminal route: Send's hand-built
// body silently dropped it, so a human's terminal-routed send relayed to a
// peer was evaluated for inbox-eligibility on the OWNING machine as if
// route:"terminal" had never been asked for — undoing D7's whole purpose the
// moment the inbox path is live for anything this shape can reach.
func TestSendForwardsTerminalRoute(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "a human's message",
		driver.SendOptions{Submit: true, Route: fleet.RouteTerminal}); err != nil {
		t.Fatal(err)
	}

	var body struct {
		Route string `json:"route"`
	}
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body = %q: %v", rec.body, err)
	}
	if body.Route != "terminal" {
		t.Fatalf("body = %q, want route:\"terminal\" carried through to the owning daemon (D7)", rec.body)
	}
}

// TestSendOmitsRouteWhenNotForced proves the field stays absent (not merely
// empty-string-but-present) on an ordinary send — the owning daemon's own
// decoder treats an explicit unrecognised value as a caller error, so this
// driver must never send a value a future, stricter decoder could reject for
// a send that never asked to force anything.
func TestSendOmitsRouteWhenNotForced(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "an ordinary message",
		driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.body, "route") {
		t.Fatalf("body = %q, want no \"route\" field at all when no route was asked for", rec.body)
	}
}

// #184: the wire's route vocabulary. Auto is sent as NO field (an owning peer
// built before #184 rejects any value it does not know), terminal and inbox are
// sent as themselves.
func TestSendForwardsRoute(t *testing.T) {
	for _, tc := range []struct {
		route fleet.Route
		want  string // "" means the field must be absent
	}{
		{"", ""},
		{fleet.RouteAuto, ""},
		{fleet.RouteTerminal, "terminal"},
		{fleet.RouteInbox, "inbox"},
	} {
		t.Run(string(tc.route), func(t *testing.T) {
			var rec capture
			srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
			d := New("peerbox", srv.URL)
			if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hello",
				driver.SendOptions{Submit: true, Route: tc.route}); err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
				t.Fatalf("body = %q: %v", rec.body, err)
			}
			got, present := body["route"]
			if tc.want == "" && present {
				t.Fatalf("body = %q, want no route field for %q", rec.body, tc.route)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("body = %q, want route %q", rec.body, tc.want)
			}
		})
	}
}

// An owning peer that predates the "inbox" route answers it with a 400 of its
// own. That answer is the right one: the driver returns it as an error and never
// retries the same text on a path the caller did not ask for.
func TestSendRouteInboxToAnOlderPeerIsAnErrorNotADowngrade(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/input") {
			requests++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(fleet.ErrorEnvelope{Error: fleet.Error{
				Kind: fleet.ErrorInvalid, Message: `route "inbox" is not recognised; the only accepted non-empty value is "terminal"`}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	d := New("peerbox", srv.URL)

	_, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hello",
		driver.SendOptions{Submit: true, Route: fleet.RouteInbox})
	if err == nil {
		t.Fatal("an explicit inbox request to a peer that cannot honour it succeeded")
	}
	if requests != 1 {
		t.Errorf("the peer was asked %d times, want exactly 1: the driver must not retry on another route", requests)
	}
}

// The receipt's account of the path comes back from a peer as it was, and a
// receipt from a peer built before the field has no path — never a guessed one.
func TestSendReceiptPathPassesThroughFromAPeer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload any
		want    fleet.Route
	}{
		{"a peer that names the inbox", map[string]any{"outcome": "delivered", "delivery": map[string]any{"route": "inbox"}}, fleet.RouteInbox},
		{"a peer that names the terminal", map[string]any{"outcome": "queued", "delivery": map[string]any{"route": "terminal"}}, fleet.RouteTerminal},
		{"an older peer that names none", map[string]any{"outcome": "queued"}, ""},
		{"a newer peer with a route this build does not know", map[string]any{"outcome": "queued", "delivery": map[string]any{"route": "carrier-pigeon"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := peerServing(t, 200, tc.payload, nil)
			d := New("peerbox", srv.URL)
			got, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hello", driver.SendOptions{Submit: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.RouteOf() != tc.want {
				t.Fatalf("route = %q, want %q", got.RouteOf(), tc.want)
			}
		})
	}
}

// muster #158: From is the third field Send's hand-built body has to be
// told about (#33, #112). Machine must travel with it: this hop is the only
// place it is on the wire, carrying the entering machine's own stamp.
func TestSendForwardsFrom(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	from := &fleet.MessageFrom{Agent: "agent-a", Session: "s-158", Machine: "entrybox", RelayOfHuman: true}
	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hi",
		driver.SendOptions{Submit: true, From: from}); err != nil {
		t.Fatal(err)
	}

	var body struct {
		From *fleet.MessageFrom `json:"from"`
	}
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body = %q: %v", rec.body, err)
	}
	if body.From == nil || *body.From != *from {
		t.Fatalf("body = %q, want from %+v carried through to the owning daemon (#158)", rec.body, *from)
	}
}

// An unlabelled send must stay byte-for-byte what it was before #158.
func TestSendWithoutFromSendsNone(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hi",
		driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.body, `"from"`) {
		t.Fatalf("body = %q, want no from field on an unlabelled send", rec.body)
	}
}

// #45: createBody is, like Send's body before #33, a hand-maintained mirror
// of the struct it represents rather than a marshal of it — and Marker and
// RemoteControl were the two fields that mirror never grew. A session
// created through this driver therefore came up on the peer without its
// session-type stamp and, worse, without the remote-control binding #20
// exists to give every session — reachable only from a terminal on the
// machine that made it, indistinguishable at the response from a session
// that got the binding on purpose.
func TestCreateForwardsMarkerAndRemoteControl(t *testing.T) {
	var rec capture
	srv := peerServing(t, 201, fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "new-1"},
		Runtime:    "claude-code-tmux",
		State:      fleet.InferredState(fleet.StatusStarting, "just created", nil),
	}, &rec)
	d := New("peerbox", srv.URL)

	on := true
	_, err := d.Create(context.Background(), caller, "caller-key-42", fleet.SessionSpec{
		Cwd: "/w", Marker: "supervised", RemoteControl: &on,
	})
	if err != nil {
		t.Fatal(err)
	}

	var body struct {
		Marker        string `json:"marker"`
		RemoteControl *bool  `json:"remoteControl"`
	}
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body = %q: %v", rec.body, err)
	}
	if body.Marker != "supervised" {
		t.Errorf("marker = %q, want %q carried to the owning daemon (#45)", body.Marker, "supervised")
	}
	if body.RemoteControl == nil || !*body.RemoteControl {
		t.Errorf("remoteControl = %v, want true carried to the owning daemon (#45, #20)", body.RemoteControl)
	}
}

// RemoteControl is a tri-state for a reason session.go's own doc comment
// gives: nil means "give me whatever a first-class session gets" and false
// means "deliberately unreachable" — two different requests a caller must be
// able to tell apart, which is exactly what a plain bool cannot hold. This
// asserts all three states survive the trip onto the wire body, because the
// encoding choice that forwards true and false correctly is also the one
// that can quietly collapse "absent" into "false" if got wrong (see the
// comment on createBody's RemoteControl field for which choice avoids that).
func TestCreateRemoteControlTriState(t *testing.T) {
	no := false
	yes := true
	cases := []struct {
		name    string
		rc      *bool
		wantKey bool // must the "remoteControl" key appear on the wire at all?
		want    *bool
	}{
		{name: "absent", rc: nil, wantKey: false},
		{name: "explicit true", rc: &yes, wantKey: true, want: &yes},
		{name: "explicit false", rc: &no, wantKey: true, want: &no},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec capture
			srv := peerServing(t, 201, fleet.Session{
				SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "new-1"},
				Runtime:    "claude-code-tmux",
				State:      fleet.InferredState(fleet.StatusStarting, "just created", nil),
			}, &rec)
			d := New("peerbox", srv.URL)

			if _, err := d.Create(context.Background(), caller, "caller-key-42", fleet.SessionSpec{
				Cwd: "/w", RemoteControl: tc.rc,
			}); err != nil {
				t.Fatal(err)
			}

			var wire map[string]any
			if err := json.Unmarshal([]byte(rec.body), &wire); err != nil {
				t.Fatalf("body = %q: %v", rec.body, err)
			}
			_, present := wire["remoteControl"]
			if present != tc.wantKey {
				t.Fatalf("body = %q, remoteControl present = %v, want %v", rec.body, present, tc.wantKey)
			}

			var body struct {
				RemoteControl *bool `json:"remoteControl"`
			}
			if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
				t.Fatalf("body = %q: %v", rec.body, err)
			}
			if tc.want == nil {
				if body.RemoteControl != nil {
					t.Errorf("remoteControl = %v, want absent (nil), not collapsed into false (#45)", *body.RemoteControl)
				}
			} else {
				if body.RemoteControl == nil || *body.RemoteControl != *tc.want {
					t.Errorf("remoteControl = %v, want %v", body.RemoteControl, *tc.want)
				}
			}
		})
	}
}

// §4.4: a caller may shorten a deadline, never lengthen it, and the peer is
// told the bound so it can fail fast rather than working on a request the
// caller has already abandoned.
func TestDeadlineIsAnnouncedAndCallersMayOnlyShortenIt(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), &rec)

	d := New("peerbox", srv.URL, WithDeadline(5*time.Second))

	// A shorter caller deadline wins.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := d.List(ctx, caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if rec.dline == "" {
		t.Fatal("no Fleet-Deadline-Ms header sent (§3.3)")
	}
	if ms := parseMs(t, rec.dline); ms > 1000 {
		t.Errorf("announced deadline %dms; the caller asked for 200ms and a "+
			"driver may never lengthen it (§4.4)", ms)
	}

	// A longer caller deadline does NOT win.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	if _, err := d.List(ctx2, caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if ms := parseMs(t, rec.dline); ms > 6000 {
		t.Errorf("announced deadline %dms; a caller must not be able to extend "+
			"the driver's declared 5s bound (§4.4)", ms)
	}
}

func parseMs(t *testing.T, s string) int64 {
	t.Helper()
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("bad deadline header %q: %v", s, err)
	}
	return ms
}

// A hung peer must produce unreachable in bounded time — §4.4's whole
// reason for existing, measured originally against a SIGSTOPped host that
// blocked a caller for seven seconds and would have waited forever.
func TestAHungPeerBecomesUnreachableWithinTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answer
	}))
	defer srv.Close()

	d := New("peerbox", srv.URL, WithDeadline(300*time.Millisecond))
	started := time.Now()
	got, err := d.List(context.Background(), caller, driver.ListFilter{})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("a hung peer degrades an envelope; it does not fail the call: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %s to give up on a 300ms deadline", elapsed)
	}
	if len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceUnreachable {
		t.Errorf("want one unreachable source, got %+v", got.Sources())
	}
	if got.Complete() {
		t.Error("must not report complete")
	}
}

// FINDING 1/2: capabilities of an unreached peer are indistinguishable from
// a peer that supports nothing — except through this concrete type.
func TestCapabilitiesAreConservativeUntilThePeerAnswers(t *testing.T) {
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), nil)
	_ = srv

	d := New("peerbox", "http://127.0.0.1:1", WithDeadline(200*time.Millisecond))

	caps := d.Capabilities()
	if d.CapabilitiesKnown() {
		t.Error("peer has never answered; capabilities must not read as known")
	}
	if caps.ObservesState || caps.ConfirmsDelivery || caps.SupportsResume {
		t.Error("an unreached peer must not be credited with capabilities (§5.6)")
	}
	// §4.4 still has to hold, or the driver could never be registered.
	if err := caps.Validate(); err != nil {
		t.Errorf("a remote driver must be registrable before its peer answers: %v", err)
	}
}

func TestRefreshCapabilitiesAdoptsWhatThePeerReports(t *testing.T) {
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), nil)
	_ = srv

	runtimes := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox", Runtime: "claude-code-tmux",
			Capabilities: fleet.DriverCapabilities{
				ObservesState: false, ConfirmsDelivery: false,
				SupportsResume: true, DeadlineMs: 5000,
				Source: fleet.CapabilitiesObserved,
			},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv2 := peerServing(t, 200, runtimes, nil)

	d := New("peerbox", srv2.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if !d.CapabilitiesKnown() {
		t.Error("capabilities should read as known after the peer answered")
	}
	caps := d.Capabilities()
	if !caps.SupportsResume {
		t.Error("did not adopt the peer's declared capability")
	}
	if caps.ObservesState {
		t.Error("invented a capability the peer did not declare")
	}
}

// TestRefreshCapabilitiesAdoptsPeerMaxInputBytes is muster #130's
// analog of the build-identity probe (#121): the peer's own effective
// input-length limit is learned on the same /v1/health round trip as its
// build, and MaxInputBytes() (driver.MaxInputBytesReporter) reports it
// afterward.
func TestRefreshCapabilitiesAdoptsPeerMaxInputBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/runtimes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []fleet.RuntimeInfo{{
					Machine: "peerbox", Runtime: "claude-code-tmux",
					Capabilities: fleet.DriverCapabilities{DeadlineMs: 5000, Source: fleet.CapabilitiesObserved},
				}},
				"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
				"complete": true,
			})
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"build": fleet.Build{}, "maxInputBytes": 4096})
		default:
			_ = json.NewEncoder(w).Encode(collectionJSON(
				[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}}, nil))
		}
	}))
	t.Cleanup(srv.Close)

	d := New("peerbox", srv.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if got := d.MaxInputBytes(); got != 4096 {
		t.Errorf("MaxInputBytes() = %d, want 4096 (#130 — learned on the same /v1/health probe as build)", got)
	}
}

// muster #67, ask #1 ("the safety half"): a cached `observed` describes
// the peer as it was the moment it answered. Measured directly — a peer
// upgraded seconds after being probed kept reading `observed` and wrong for
// as long as the observing process happened to run, which under a
// keep-alive supervisor is unbounded in practice. Past capabilityStaleness
// the claim must degrade to `assumed` on every read, not just eventually.
func TestObservedCapabilityDegradesToAssumedPastStaleness(t *testing.T) {
	runtimes := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox", Runtime: "claude-code-tmux",
			Capabilities: fleet.DriverCapabilities{
				DeliversRawKeys: true, DeadlineMs: 5000,
				Source: fleet.CapabilitiesObserved,
			},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv := peerServing(t, 200, runtimes, nil)

	now := time.Now()
	d := New("peerbox", srv.URL, withClock(func() time.Time { return now }))
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if caps := d.Capabilities(); caps.Source != fleet.CapabilitiesObserved {
		t.Fatalf("source = %q right after a fresh probe, want observed", caps.Source)
	}

	now = now.Add(capabilityStaleness + time.Second)
	caps := d.Capabilities()
	if caps.Source != fleet.CapabilitiesAssumed {
		t.Errorf("source = %q after %s with no fresh contact, want assumed — "+
			"a wrong observed is worse than a stale assumed (#67)", caps.Source, capabilityStaleness)
	}
	if caps.DeliversRawKeys {
		t.Error("a degraded declaration must read as a conservative floor, " +
			"not as the stale flags it can no longer stand behind")
	}
}

// muster #67, ask #4 (the sharpened one, replacing a time-based
// schedule): "probe on first successful contact, not only at startup."
// Measured directly — a peer restarted, its capabilities never populated,
// and a full round of successful relayed traffic (create, keypress, two
// refusals, close) never changed that. An ordinary List reaching the peer
// and getting a domain answer back must trigger the same opportunistic
// refresh.
func TestSuccessfulContactRefreshesNeverObservedCapabilities(t *testing.T) {
	var mu sync.Mutex
	var runtimesHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/runtimes":
			mu.Lock()
			runtimesHits++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []fleet.RuntimeInfo{{
					Machine: "peerbox", Runtime: "claude-code-tmux",
					Capabilities: fleet.DriverCapabilities{
						SupportsResume: true, DeadlineMs: 5000,
						Source: fleet.CapabilitiesObserved,
					},
				}},
				"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
				"complete": true,
			})
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"build": fleet.Build{}})
		default:
			_ = json.NewEncoder(w).Encode(collectionJSON(
				[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}}, nil))
		}
	}))
	t.Cleanup(srv.Close)

	d := New("peerbox", srv.URL)
	if d.CapabilitiesKnown() {
		t.Fatal("fixture drifted; capabilities must start unknown")
	}

	if _, err := d.List(context.Background(), caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	waitUntil := time.Now().Add(2 * time.Second)
	for !d.CapabilitiesKnown() && time.Now().Before(waitUntil) {
		time.Sleep(10 * time.Millisecond)
	}
	if !d.CapabilitiesKnown() {
		t.Fatal("an ordinary List that reached the peer should have opportunistically " +
			"refreshed capabilities instead of leaving them assumed forever (#67)")
	}
	if !d.Capabilities().SupportsResume {
		t.Error("refreshed capabilities did not adopt what the peer reported")
	}
	mu.Lock()
	hits := runtimesHits
	mu.Unlock()
	if hits == 0 {
		t.Error("expected the successful List to have triggered a probe of /v1/runtimes")
	}
}

// The expectation must survive the wire, or every cross-machine destroy
// silently downgrades to the weak check.
func TestCloseForwardsTheCallersExpectation(t *testing.T) {
	var rec capture
	srv := peerServing(t, 202, nil, &rec)
	d := New("peerbox", srv.URL)

	ts := time.Unix(1785600000, 0)
	req := fleet.Request{
		Caller: fleet.Caller{Principal: "addr:test", Credential: callerTok},
		Expect: fleet.Expectation{StartedAt: &ts},
	}
	if _, err := d.Close(context.Background(), req, fleet.SessionRef{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.query, "startedAt=") {
		t.Fatalf("query = %q, must carry the caller's expected start time (§5.4)", rec.query)
	}
	if rec.method != "DELETE" {
		t.Errorf("method = %q", rec.method)
	}
}

// muster#136: force is a real capability expansion (a stronger, more
// destructive clear mechanism) and it is easy for it to work on a local
// driver while silently vanishing at the federation boundary — the exact
// class of drift this repo elsewhere builds single-source-of-truth argv
// helpers to prevent. This pins that Discard forwards it, as its own query
// parameter, never folded into or inferred from expect.
func TestDiscardForwardsForceOnTheWire(t *testing.T) {
	var rec capture
	srv := peerServing(t, 202, fleet.Ack{Accepted: true}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Discard(context.Background(), caller, fleet.SessionRef{ID: "s1"},
		"the-digest", driver.DiscardOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.query, "force=true") {
		t.Fatalf("query = %q, must forward force=true (muster#136) — dropping it here "+
			"makes the escape hatch work locally and die across the federation boundary", rec.query)
	}
	if !strings.Contains(rec.query, "expect=the-digest") {
		t.Fatalf("query = %q, force must not replace or omit expect", rec.query)
	}
	if rec.method != "POST" {
		t.Errorf("method = %q", rec.method)
	}
}

// A caller that does not ask for force must not have it invented for it —
// the mirror of TestCloseWithoutExpectationSendsNone, for the same reason:
// a proxy must forward exactly what the caller asked for, nothing assumed.
func TestDiscardWithoutForceSendsNone(t *testing.T) {
	var rec capture
	srv := peerServing(t, 202, fleet.Ack{Accepted: true}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Discard(context.Background(), caller, fleet.SessionRef{ID: "s1"},
		"the-digest", driver.DiscardOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.query, "force=") {
		t.Errorf("query = %q; a proxy must not manufacture force the caller did not ask for", rec.query)
	}
}

// And a caller that supplies none must not have one invented for it.
func TestCloseWithoutExpectationSendsNone(t *testing.T) {
	var rec capture
	srv := peerServing(t, 202, nil, &rec)
	d := New("peerbox", srv.URL)
	if _, err := d.Close(context.Background(), caller, fleet.SessionRef{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.query, "startedAt=") {
		t.Errorf("query = %q; a proxy must not manufacture an expectation the caller did not have", rec.query)
	}
}

// §14 D7: a proxy that waits less time than its peer declared abandons calls
// the peer would have completed, and reports a healthy machine as unreachable.
func TestProxyWaitsAtLeastAsLongAsThePeerDeclared(t *testing.T) {
	runtimes := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox", Runtime: "claude-code-tmux",
			Capabilities: fleet.DriverCapabilities{
				SupportsResume: true, DeadlineMs: 5000, Source: fleet.CapabilitiesObserved},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv := peerServing(t, 200, runtimes, nil)
	d := New("peerbox", srv.URL, WithDeadline(3*time.Second), WithTransitMargin(2*time.Second))

	if got := d.Capabilities().DeadlineMs; got != 3000 {
		t.Fatalf("before the peer answers, the floor applies: got %dms", got)
	}
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	// 5s declared by the peer + 2s transit; never the 3s floor.
	if got := d.Capabilities().DeadlineMs; got != 7000 {
		t.Errorf("deadline = %dms, want 7000 (peer's 5000 + 2000 transit). A proxy "+
			"waiting less than its peer declared turns a healthy machine into an "+
			"unreachable one", got)
	}
}

// The floor still wins when it is the larger of the two — a caller that
// configured a generous proxy deadline does not get it silently reduced.
func TestPeerDeadlineNeverShortensTheConfiguredFloor(t *testing.T) {
	runtimes := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox", Capabilities: fleet.DriverCapabilities{
				DeadlineMs: 500, Source: fleet.CapabilitiesObserved},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv := peerServing(t, 200, runtimes, nil)
	d := New("peerbox", srv.URL, WithDeadline(9*time.Second))
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if got := d.Capabilities().DeadlineMs; got != 9000 {
		t.Errorf("deadline = %dms, want the 9000 floor", got)
	}
}

// D3, stated as the distinction that did not previously exist: a peer that
// genuinely supports nothing and a peer nobody has reached produce identical
// flags. Only provenance separates them.
func TestMinimalPeerIsDistinguishableFromUnreachedPeer(t *testing.T) {
	minimal := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox",
			Capabilities: fleet.DriverCapabilities{
				DeadlineMs: 1000, Source: fleet.CapabilitiesObserved,
			},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv := peerServing(t, 200, minimal, nil)

	reached := New("peerbox", srv.URL)
	if err := reached.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	unreached := New("peerbox", "http://127.0.0.1:1", WithDeadline(200*time.Millisecond))

	a, b := reached.Capabilities(), unreached.Capabilities()

	// The flags really are identical — that is the point.
	if a.ObservesState != b.ObservesState || a.SupportsResume != b.SupportsResume {
		t.Fatal("fixture drifted; both should report nothing supported")
	}
	if a.Source != fleet.CapabilitiesObserved {
		t.Errorf("a peer that answered should read observed, got %q", a.Source)
	}
	if b.Source != fleet.CapabilitiesAssumed {
		t.Errorf("a peer that never answered should read assumed, got %q", b.Source)
	}
	if a.ObservedAt == nil {
		t.Error("an observed declaration should say when")
	}
}

// A declaration with no provenance must not marshal. An absent source is not
// the same fact as "assumed", and silently defaulting would rebuild exactly
// the ambiguity this field removes.
func TestCapabilitiesWithoutProvenanceDoNotMarshal(t *testing.T) {
	if _, err := json.Marshal(fleet.DriverCapabilities{DeadlineMs: 1000}); err == nil {
		t.Error("capabilities with no source should not encode")
	}
}

// Tool-server configuration names PATHS, and the paths belong to the peer's
// filesystem, not this machine's. A proxy that resolved or checked them locally
// would refuse creates that are perfectly valid where the session will run — so
// they travel verbatim and the owning daemon decides.
//
// A field silently dropped here does not fail: it produces a session that
// starts, looks healthy, and is missing its tools — one machine away, which is
// the hardest place to notice it.
func TestCreateForwardsMcpConfigPathsVerbatim(t *testing.T) {
	var rec capture
	srv := peerServing(t, 201, fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "new-1"},
		Runtime:    "claude-code-tmux",
		State:      fleet.InferredState(fleet.StatusStarting, "just created", nil),
	}, &rec)
	d := New("peerbox", srv.URL)

	want := []fleet.AbsolutePath{"/peer/only/base.json", "/peer/only/session.json"}
	if _, err := d.Create(context.Background(), caller, "caller-key-43", fleet.SessionSpec{
		Cwd: "/w", McpConfig: want,
	}); err != nil {
		t.Fatal(err)
	}

	var body struct {
		McpConfig []fleet.AbsolutePath `json:"mcpConfig"`
	}
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body = %q: %v", rec.body, err)
	}
	if len(body.McpConfig) != len(want) {
		t.Fatalf("mcpConfig = %v, want %v", body.McpConfig, want)
	}
	for i := range want {
		if body.McpConfig[i] != want[i] {
			t.Errorf("mcpConfig[%d] = %q, want %q", i, body.McpConfig[i], want[i])
		}
	}
}

// TestSendForwardsExpect (#180): the draft rule's caller-supplied proof is
// one more field Send's hand-built body must carry, or a relayed
// replaceIfStranded with a correct expect digest is refused on the owning
// machine for want of proof the caller did supply (#33's trap, again).
func TestSendForwardsExpect(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "replacement",
		driver.SendOptions{Submit: true, ReplaceIfStranded: true, ExpectComposerDigest: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Expect            string `json:"expect"`
		ReplaceIfStranded bool   `json:"replaceIfStranded"`
	}
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body = %q: %v", rec.body, err)
	}
	if body.Expect != "0123456789abcdef" || !body.ReplaceIfStranded {
		t.Fatalf("body = %q, want expect and replaceIfStranded carried to the owning daemon", rec.body)
	}
}

// TestSendCarriesTheHumanRelayAssertion (#180 L3): a human relay established
// on the entering machine reaches the owning machine as an assertion header —
// and only when it was established.
func TestSendCarriesTheHumanRelayAssertion(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)
	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "a person's message",
		driver.SendOptions{Submit: true, Route: fleet.RouteTerminal, HumanRelay: true}); err != nil {
		t.Fatal(err)
	}
	if rec.humanRelay != "1" {
		t.Fatalf("Fleet-Human-Relay = %q, want 1", rec.humanRelay)
	}
	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "an agent's message",
		driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	if rec.humanRelay != "" {
		t.Fatalf("Fleet-Human-Relay = %q on a send that asserted nothing", rec.humanRelay)
	}
}

// TestSendCarriesTheRemoteControlAssertion (#272): the finding that the caller
// holds the remote-control grant reaches the owning machine as an assertion
// header — and only when it was established.
func TestSendCarriesTheRemoteControlAssertion(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)
	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "/rc",
		driver.SendOptions{Submit: true, RemoteControl: true}); err != nil {
		t.Fatal(err)
	}
	if rec.remoteCtl != "1" {
		t.Fatalf("Fleet-Remote-Control = %q, want 1", rec.remoteCtl)
	}
	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "/rc",
		driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	if rec.remoteCtl != "" {
		t.Fatalf("Fleet-Remote-Control = %q on a send that asserted nothing", rec.remoteCtl)
	}
}
