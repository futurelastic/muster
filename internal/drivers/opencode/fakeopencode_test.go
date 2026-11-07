package opencode

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file lets the isolation tests start REAL per-session processes without
// an opencode install: the test binary re-executes itself as a stand-in
// `opencode`. TestMain recognises the two invocations the driver makes —
// `<bin> --version` (Probe) and `<bin> serve --port N --hostname H` — by argv,
// not by an environment marker, because the point of the driver under test is
// that the child's environment is BUILT: no marker could travel through it.
//
// The stand-in serves just enough of opencode's HTTP API for the driver, plus
// two test-only endpoints the isolation tests use to look at the process from
// the inside. Failure modes are chosen through the session's working directory
// (a marker file the test plants there), which the driver does pass on.

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version":
			// muster #297: a stand-in that does not answer until a file
			// exists, so a test can make the version check hang past the
			// probe's deadline and later let it answer — a real hang and a
			// real answer, with no real binary and no long wait. The wait is
			// bounded so a probe killed mid-test never leaves this process
			// behind.
			if gate := os.Getenv("FAKE_OPENCODE_VERSION_GATE"); gate != "" {
				for i := 0; i < 1500 && !fileExists(gate); i++ {
					time.Sleep(10 * time.Millisecond)
				}
			}
			fmt.Println("fake-opencode 0.0.0")
			os.Exit(0)
		case "__stray":
			// muster #405: a long-lived process standing in for a browser a tool
			// started in a session of its own. The test binary rather than a system
			// program, because macOS prints no environment for a protected one.
			time.Sleep(10 * time.Minute)
			os.Exit(0)
		case "__stopfrom":
			// muster #405: runs the stray sweep from inside a process, to show what it
			// does to the one running it and to its parent.
			stopped, left, err := stopStrays(os.Args[2], "")
			fmt.Printf("stopped=%d left=%d err=%v\n", len(stopped), len(left), err)
			os.Exit(0)
		case "__relay":
			// muster #405: a process that runs __stopfrom as its child.
			exe, _ := os.Executable()
			out, err := exec.Command(exe, "__stopfrom", os.Args[2]).CombinedOutput()
			fmt.Printf("%s relay-alive err=%v\n", strings.TrimSpace(string(out)), err)
			os.Exit(0)
		case "serve":
			if serveFailLoggedStub(os.Args[2:]) {
				os.Exit(3)
			}
			runFakeOpencode(os.Args[2:])
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// failLoggedSuffix names the file beside this executable that makes it behave
// as a runtime that cannot start: it logs a provider-configuration error name
// to stderr, but only when started with --print-logs (muster #288), then exits.
// A file rather than an environment variable because the driver gives its child
// an environment of its own. It is how Windows, which has no #!/bin/sh script to
// write, stands in for a runtime that dies at start (stubShell).
const failLoggedSuffix = ".fail-logged"

// serveFailLoggedStub reports whether this process is such a stand-in, after
// doing what it does.
func serveFailLoggedStub(args []string) bool {
	exe, err := os.Executable()
	if err != nil || !fileExists(exe+failLoggedSuffix) {
		return false
	}
	for _, a := range args {
		if a == "--print-logs" {
			fmt.Fprintln(os.Stderr, "ERROR service=config error=ProviderConfigError bad provider")
		}
	}
	return true
}

// skipOnWindows skips a test that covers a Unix tool or a POSIX path, saying so
// in the form the Windows gate expects.
func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why + " (windows, #303)")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// fakeSessionAgents is the stand-in for the runtime reading the `agent` entries
// of the session's own opencode.json at start (muster #396). listed is false when
// that file defines none. Like the real runtime, it does not start on a
// definition it rejects — here a mode that is none of subagent, primary and all,
// the case measured against the real binary — and says why by NAME on stderr when
// told to log, which is all the driver ever reads of it.
func fakeSessionAgents(printLogs bool) (names []string, listed bool) {
	raw, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode", "opencode.json"))
	if err != nil {
		return nil, false
	}
	var cfg struct {
		Agent map[string]struct {
			Mode string `json:"mode"`
		} `json:"agent"`
	}
	if json.Unmarshal(raw, &cfg) != nil || len(cfg.Agent) == 0 {
		return nil, false
	}
	for name, a := range cfg.Agent {
		switch a.Mode {
		case "", "subagent", "primary", "all":
			names = append(names, name)
		default:
			if printLogs {
				fmt.Fprintln(os.Stderr, "ERROR service=config error=ConfigInvalidError agent."+name+".mode")
			}
			os.Exit(3)
		}
	}
	sort.Strings(names)
	return names, true
}

func runFakeOpencode(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 0, "")
	host := fs.String("hostname", "127.0.0.1", "")
	printLogs := fs.Bool("print-logs", false, "") // muster #288: the stand-in logs to stderr only when told to, like the runtime
	_ = fs.Bool("pure", false, "")                // muster #283: a bypass session starts with it
	_ = fs.String("log-level", "", "")            // muster #420: the log is capped, as the real runtime accepts
	_ = fs.Parse(args)

	if fileExists(".fake-fail-ready") {
		os.Exit(3) // dies before it ever binds: the startup-failure case
	}
	configuredAgents, agentsListed := fakeSessionAgents(*printLogs)
	user := os.Getenv("OPENCODE_SERVER_USERNAME")
	pass := os.Getenv("OPENCODE_SERVER_PASSWORD")
	cwd, _ := os.Getwd()

	var id [6]byte
	_, _ = rand.Read(id[:])
	sessionID := "ses_" + hex.EncodeToString(id[:])
	created := time.Now().UnixMilli()
	var session *wireSession
	// Like the real runtime, the stand-in keeps its session in a store under
	// XDG_DATA_HOME, so a second process started on the same directory finds the
	// session by id (muster #282). A test plants fake-messages.json there to give
	// the session a history.
	storeFile := os.Getenv("XDG_DATA_HOME") + "/fake-session.json"
	if raw, err := os.ReadFile(storeFile); err == nil {
		var prior wireSession
		if json.Unmarshal(raw, &prior) == nil && prior.ID != "" {
			session, sessionID = &prior, prior.ID
		}
	}

	var busMu sync.Mutex
	bus := map[int]chan string{}
	nextBus := 0

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := r.URL.Path
		switch {
		case path == "/session" && r.Method == http.MethodGet:
			writeJSON(w, []wireSession{})
		case path == "/session" && r.Method == http.MethodPost:
			if fileExists(".fake-fail-create") {
				http.Error(w, "refused", http.StatusInternalServerError)
				return
			}
			var body struct {
				Title string `json:"title"`
				Agent string `json:"agent"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			session = &wireSession{ID: sessionID, Directory: r.URL.Query().Get("directory"), Title: body.Title,
				Agent: body.Agent, Time: wireTime{Created: created, Updated: created}}
			if raw, err := json.Marshal(session); err == nil {
				_ = os.WriteFile(storeFile, raw, 0o600)
			}
			writeJSON(w, session)
		case path == "/agent" && r.Method == http.MethodGet && agentsListed:
			// Only for a session whose own configuration defines agents (muster
			// #396); every other stand-in session answers 404, which is "could not
			// tell" to the driver, as it always did.
			list := []wireAgent{}
			for _, n := range append([]string{"build", "explore", "general", "plan"}, configuredAgents...) {
				list = append(list, wireAgent{Name: n})
			}
			writeJSON(w, list)
		case path == "/session/status":
			// muster #421: a marker in the session's working directory makes the stand-in
			// report its session busy, as a runtime in the middle of a turn does.
			if session != nil && fileExists(".fake-busy") {
				writeJSON(w, statusMap{session.ID: wireStatus{Type: "busy"}})
				return
			}
			writeJSON(w, statusMap{})
		case path == "/permission" && r.Method == http.MethodGet:
			// muster #421: one tool-permission ask for the session while the marker is
			// there; none otherwise.
			asks := []wirePermissionAsk{}
			if session != nil && fileExists(".fake-ask") {
				asks = append(asks, wirePermissionAsk{ID: "per_fake", SessionID: session.ID, Permission: "bash"})
			}
			writeJSON(w, asks)
		case path == "/question" && r.Method == http.MethodGet:
			writeJSON(w, []wireQuestionAsk{})
		case path == "/session/"+sessionID && r.Method == http.MethodGet && session != nil:
			writeJSON(w, session)
		case path == "/session/"+sessionID && r.Method == http.MethodDelete && session != nil:
			session = nil
			_ = os.Remove(storeFile)
			writeJSON(w, true)
		case path == "/session/"+sessionID+"/prompt_async":
			// Every prompt body is appended to a file in the session's own data
			// directory, which outlives the process, so a test can read what a
			// session sent — which agent a message named (muster #362) and what
			// a relaunched session was sent (muster #371).
			if raw, err := io.ReadAll(r.Body); err == nil {
				if f, err := os.OpenFile(os.Getenv("XDG_DATA_HOME")+"/fake-prompts.jsonl",
					os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					_, _ = f.Write(append(bytes.TrimSpace(raw), '\n'))
					_ = f.Close()
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case path == "/session/"+sessionID+"/abort":
			writeJSON(w, true)
		case path == "/session/"+sessionID+"/message":
			msgs := []wireMessage{}
			if raw, err := os.ReadFile(os.Getenv("XDG_DATA_HOME") + "/fake-messages.json"); err == nil {
				_ = json.Unmarshal(raw, &msgs)
			}
			writeJSON(w, msgs)
		case path == "/event" && r.Method == http.MethodGet:
			// The bus (muster #284). Frames posted to /__test/emit reach every
			// open connection; a line posted to /__test/log goes to stderr, the
			// log the driver captures.
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			ch := make(chan string, 16)
			busMu.Lock()
			nextBus++
			n := nextBus
			bus[n] = ch
			busMu.Unlock()
			defer func() {
				busMu.Lock()
				delete(bus, n)
				busMu.Unlock()
			}()
			fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
			if fl != nil {
				fl.Flush()
			}
			for {
				select {
				case frame := <-ch:
					fmt.Fprint(w, frame)
					if fl != nil {
						fl.Flush()
					}
				case <-r.Context().Done():
					return
				}
			}
		case path == "/__test/emit" && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			busMu.Lock()
			for _, ch := range bus {
				ch <- "data: " + string(raw) + "\n\n"
			}
			busMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case path == "/__test/log" && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			if *printLogs {
				fmt.Fprintln(os.Stderr, string(raw))
			}
			w.WriteHeader(http.StatusNoContent)
		case path == "/__test/env":
			writeJSON(w, map[string]any{"env": os.Environ(), "cwd": cwd, "pid": os.Getpid(), "args": os.Args})
		case path == "/__test/sh" && r.Method == http.MethodPost:
			// A tool the session runs: a shell started BY the runtime's process,
			// as the agent's own commands are, so it inherits whatever profile the
			// runtime was started under (muster #281).
			var body struct {
				Cmd string `json:"cmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			out, err := shellCommand(body.Cmd).CombinedOutput()
			rc := 0
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			} else if err != nil {
				rc = -1
			}
			writeJSON(w, map[string]any{"out": string(out), "rc": rc})
		case path == "/__test/spawn" && r.Method == http.MethodPost:
			// A long-lived grandchild, as a tool the session ran would be. It is
			// found through the BUILT PATH, so the test also proves the base PATH
			// is usable.
			cmd := longSleeper()
			if err := cmd.Start(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"pid": cmd.Process.Pid})
		case path == "/__test/detach" && r.Method == http.MethodPost:
			// A long-lived process that has LEFT the session's process group, as a
			// browser a tool started with setsid does (muster #405). It inherits this
			// server's environment, so its TMPDIR is the session's.
			cmd := detachedStray()
			if err := cmd.Start(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			go func() { _ = cmd.Wait() }()
			writeJSON(w, map[string]any{"pid": cmd.Process.Pid})
		default:
			http.NotFound(w, r)
		}
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(*host, fmt.Sprint(*port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(4)
	}
	_ = http.Serve(ln, mux)
}
