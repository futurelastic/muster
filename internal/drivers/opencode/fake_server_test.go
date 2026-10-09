package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a minimal stand-in for opencode's HTTP API, scripted
// against the three endpoints #55 identified as the ones this driver
// actually needs (create, prompt_async, status) plus the handful of
// others ops.go calls (get/list/delete/abort). It exists so the bulk of
// this package's tests never require a real opencode install or a paid
// provider credential — the provider ruling on #55.
type fakeServer struct {
	t    *testing.T
	mu   sync.Mutex
	srv  *httptest.Server
	next int

	username, password string

	sessions map[string]wireSession   // id -> session
	statuses statusMap                // id -> status, present only when non-idle
	messages map[string][]wireMessage // id -> messages, newest last

	// unauthorized, when true, makes every request fail Basic auth
	// regardless of what was sent — simulates a credential mismatch.
	unauthorized bool
	// statusDown, when true, makes GET /session/status fail as a
	// transport error (connection reset) rather than answering — the
	// read-failure half of #55's discrimination.
	statusDown bool

	// catalog, when non-nil, is served by GET /config/providers as provider id
	// to model ids; nil makes the endpoint 404, like a runtime without it.
	// catalogStatus, when set, makes the endpoint answer that status instead.
	catalog       map[string][]string
	catalogStatus int

	// permissions are the asks GET /permission lists (muster #283);
	// permissionStatus, when set, makes the endpoint answer that status instead
	// (404 is a runtime without it).
	permissions      []wirePermissionAsk
	permissionStatus int

	requests []recordedRequest

	// bus is the GET /event side (muster #284): the open connections, so a
	// test can publish to all of them or cut them, and a status that makes the
	// endpoint answer something other than a stream.
	bus       map[int]*busConn
	nextBus   int
	busStatus int
}

// busConn is one open GET /event connection.
type busConn struct {
	events chan string
	cut    chan struct{}
}

type recordedRequest struct {
	method, path string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{
		t:        t,
		username: "muster",
		password: "test-credential-do-not-log",
		sessions: map[string]wireSession{},
		statuses: statusMap{},
		messages: map[string][]wireMessage{},
		bus:      map[int]*busConn{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) baseURL() string { return f.srv.URL }

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{r.Method, r.URL.Path})
	f.mu.Unlock()

	user, pass, ok := r.BasicAuth()
	f.mu.Lock()
	unauthorized := f.unauthorized
	wantUser, wantPass := f.username, f.password
	f.mu.Unlock()
	if unauthorized || !ok || user != wantUser || pass != wantPass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/session":
		f.handleCreate(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/config/providers":
		f.handleProviders(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/permission":
		f.handlePermission(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/event":
		f.handleEvent(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/session":
		f.handleList(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/session/status":
		f.handleStatus(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/message"):
		f.handleMessages(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/session/") && !strings.Contains(r.URL.Path[len("/session/"):], "/"):
		f.handleGet(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/session/"):
		f.handleDelete(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt_async"):
		f.handlePromptAsync(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/abort"):
		f.handleAbort(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.next++
	id := fmt.Sprintf("ses_fake%03d", f.next)
	title := "New session"
	if t, ok := body["title"].(string); ok && t != "" {
		title = t
	}
	agent, _ := body["agent"].(string)
	sess := wireSession{
		ID:        id,
		Directory: r.URL.Query().Get("directory"),
		Title:     title,
		Agent:     agent,
		Time:      wireTime{Created: nowMillis(), Updated: nowMillis()},
	}
	f.sessions[id] = sess
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sess)
}

func (f *fakeServer) handleGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/session/")
	f.mu.Lock()
	sess, ok := f.sessions[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sess)
}

// handleList serves GET /session for completeness — the driver itself
// never calls it (see ops.go's List, which found this endpoint unreliable
// against the real server and works from its own local cache instead).
// Kept here in case a later revision needs to test against it directly.
func (f *fakeServer) handleList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	out := make([]wireSession, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, s)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeServer) handlePermission(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status := f.permissionStatus
	asks := append([]wirePermissionAsk{}, f.permissions...)
	f.mu.Unlock()
	if status != 0 {
		http.Error(w, "no such route", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(asks)
}

func (f *fakeServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down := f.statusDown
	f.mu.Unlock()
	if down {
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	out := map[string]wireStatus{}
	for id, st := range f.statuses {
		out[id] = st
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/session/")
	f.mu.Lock()
	_, ok := f.sessions[id]
	delete(f.sessions, id)
	delete(f.statuses, id)
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(true)
}

func (f *fakeServer) handlePromptAsync(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/session/"), "/prompt_async")
	f.mu.Lock()
	_, ok := f.sessions[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeServer) handleAbort(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/session/"), "/abort")
	f.mu.Lock()
	_, ok := f.sessions[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(true)
}

// handleMessages serves GET /session/{id}/message?limit=N, honouring
// `limit` the same way the real server does (measured live for #77): the
// TAIL of the message list, newest last, never the head. Session existence
// is not checked — a session with no scripted messages simply has none,
// the same as a freshly created one that has never taken a turn.
func (f *fakeServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/session/"), "/message")
	f.mu.Lock()
	all := append([]wireMessage(nil), f.messages[id]...)
	f.mu.Unlock()

	if raw := r.URL.Query().Get("limit"); raw != "" {
		var limit int
		_, _ = fmt.Sscanf(raw, "%d", &limit)
		if limit >= 0 && limit < len(all) {
			all = all[len(all)-limit:]
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}

// setLastMessage appends one message to id's scripted history — tests use
// this to put a specific newest message (an assistant reply, with or
// without an error) in place before reading state.
func (f *fakeServer) setLastMessage(id string, msg wireMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[id] = append(f.messages[id], msg)
}

// setBusy / setRetry / setIdle script the status map directly, bypassing
// any real turn-taking — these tests are about this driver's mapping
// logic, not about opencode's own agent loop.
func (f *fakeServer) setBusy(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[id] = wireStatus{Type: "busy"}
}

func (f *fakeServer) setRetry(id string, attempt int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[id] = wireStatus{Type: "retry", Attempt: attempt, Message: message}
}

func (f *fakeServer) clearStatus(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.statuses, id)
}

func (f *fakeServer) requestsSnapshot() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// newTestDriver builds a Driver pointed at f, bypassing Probe and process
// spawning entirely — the WithBaseURL/WithHTTPClient seam.
func newTestDriver(t *testing.T, f *fakeServer) *Driver {
	t.Helper()
	d, err := newDriverWithOptions(t, f)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// newDriverWithOptions is newTestDriver plus room for extra Options, for
// tests that need to configure something (a deadline, a clock) beyond the
// base fake-server wiring.
func newDriverWithOptions(t *testing.T, f *fakeServer, extra ...Option) (*Driver, error) {
	t.Helper()
	opts := append([]Option{
		WithBaseURL(f.baseURL()),
		WithCredential(f.username, f.password),
		WithHTTPClient(f.srv.Client()),
	}, extra...)
	return New(context.Background(), "test-machine", opts...)
}

// handleEvent serves GET /event as the runtime does: a text/event-stream whose
// every frame is one JSON {type, properties} object, opened by server.connected.
func (f *fakeServer) handleEvent(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status := f.busStatus
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	c := &busConn{events: make(chan string, 64), cut: make(chan struct{})}
	f.mu.Lock()
	f.nextBus++
	n := f.nextBus
	f.bus[n] = c
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.bus, n)
		f.mu.Unlock()
	}()

	write := func(frame string) {
		_, _ = fmt.Fprint(w, frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
	write("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
	for {
		select {
		case frame := <-c.events:
			write(frame)
		case <-c.cut:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// emit publishes one bus event to every open GET /event connection.
func (f *fakeServer) emit(typ string, props any) {
	raw, err := json.Marshal(map[string]any{"type": typ, "properties": props})
	if err != nil {
		f.t.Fatalf("emit: %v", err)
	}
	f.emitRaw("data: " + string(raw) + "\n\n")
}

// emitRaw publishes a frame as written, for tests of the framing itself.
func (f *fakeServer) emitRaw(frame string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.bus {
		c.events <- frame
	}
}

// cutBus ends every open GET /event connection from the server's side, the way
// a dying or restarting runtime would.
func (f *fakeServer) cutBus() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.bus {
		close(c.cut)
	}
}

// busConns reports how many GET /event connections are open.
func (f *fakeServer) busConns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bus)
}

// handleProviders serves GET /config/providers in the runtime's shape:
// {providers: [{id, models: {<model id>: {...}}}], default: {...}}.
func (f *fakeServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	catalog, status := f.catalog, f.catalogStatus
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if catalog == nil {
		http.NotFound(w, r)
		return
	}
	type provider struct {
		ID     string              `json:"id"`
		Models map[string]struct{} `json:"models"`
	}
	out := struct {
		Providers []provider        `json:"providers"`
		Default   map[string]string `json:"default"`
	}{Default: map[string]string{}}
	for id, models := range catalog {
		p := provider{ID: id, Models: map[string]struct{}{}}
		for _, m := range models {
			p.Models[m] = struct{}{}
		}
		out.Providers = append(out.Providers, p)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
