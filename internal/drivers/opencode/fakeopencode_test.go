package opencode

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
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
			writeJSON(w, session)
		case path == "/session/status":
			writeJSON(w, statusMap{})
		case path == "/session/"+sessionID && r.Method == http.MethodGet && session != nil:
			writeJSON(w, session)
		case path == "/session/"+sessionID && r.Method == http.MethodDelete && session != nil:
			session = nil
			writeJSON(w, true)
		case path == "/session/"+sessionID+"/prompt_async":
			w.WriteHeader(http.StatusNoContent)
		case path == "/session/"+sessionID+"/abort":
			writeJSON(w, true)
		case path == "/session/"+sessionID+"/message":
			writeJSON(w, []wireMessage{})
		case path == "/__test/env":
			writeJSON(w, map[string]any{"env": os.Environ(), "cwd": cwd, "pid": os.Getpid()})
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
