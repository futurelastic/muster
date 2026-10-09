package opencode

import (
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
			fmt.Println("fake-opencode 0.0.0")
			os.Exit(0)
		case "serve":
			runFakeOpencode(os.Args[2:])
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func runFakeOpencode(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 0, "")
	host := fs.String("hostname", "127.0.0.1", "")
	printLogs := fs.Bool("print-logs", false, "") // muster #288: the stand-in logs to stderr only when told to, like the runtime
	_ = fs.Bool("pure", false, "")                // muster #283: a bypass session starts with it
	_ = fs.Parse(args)

	if fileExists(".fake-fail-ready") {
		os.Exit(3) // dies before it ever binds: the startup-failure case
	}
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
		case path == "/session/status":
			writeJSON(w, statusMap{})
		case path == "/session/"+sessionID && r.Method == http.MethodGet && session != nil:
			writeJSON(w, session)
		case path == "/session/"+sessionID && r.Method == http.MethodDelete && session != nil:
			session = nil
			_ = os.Remove(storeFile)
			writeJSON(w, true)
		case path == "/session/"+sessionID+"/prompt_async":
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
			out, err := exec.Command("/bin/sh", "-c", body.Cmd).CombinedOutput()
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
			cmd := exec.Command("sleep", "600")
			if err := cmd.Start(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
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
