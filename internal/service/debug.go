package service

import (
	"net/http"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strconv"
	"time"

	fleet "github.com/futurelastic/muster"
)

// The profiling endpoint (muster #450).
//
// # Why it exists
//
// A busy service could not be sampled. The release binary has no profiling
// endpoint, and the one figure available from outside — `ps` CPU — is a decaying
// average on macOS, which misled an investigation into blaming this service for
// load that was somewhere else. A sample taken from inside answers the question
// the audit line's phase timings only narrow down.
//
// # Two independent locks, both closed by default
//
// The grant (GrantDebug) says WHO may read the process's internals. The config
// switch (Config.Profiling, `profilingEndpoint`) says whether THIS machine
// offers them at all. Holding the grant on a machine whose operator never
// switched profiling on gets a refusal that names the switch; switching it on
// gives nothing to a caller who does not hold the grant. Neither implies the
// other, which is what lets an operator hand a principal the grant in advance
// and turn the surface on only for the afternoon it is needed.
//
// # What is deliberately not offered
//
//   - cmdline: the process's argv, which is configuration an operator did not
//     agree to publish;
//   - symbol: a profile this service writes already carries its function names
//     (resolved in-process from the runtime's own tables, which a stripped
//     `-s -w` release build keeps), so there is nothing for a symbolizer to look up.
//
// The handlers are written here over runtime/pprof and runtime/trace and NOT taken
// from net/http/pprof: importing that package registers its routes on
// http.DefaultServeMux as a side effect of its init, a surface with no
// authentication that anything in the process serving the default mux would
// publish. TestDebugRoutesAreNotOnTheDefaultMux holds that line.

// maxProfileSeconds bounds ?seconds= on the two timed profiles. A holder of the
// grant can occupy the process's profiler for at most this long per request.
const maxProfileSeconds = 120

// debugProfiles is the closed set of names this endpoint serves, in the order the
// index lists them. A name outside it is not found, whatever the standard
// library would have done with it.
var debugProfiles = []struct{ Name, Description string }{
	{"profile", "CPU profile, pprof format, for ?seconds= (1-120, default 30); one at a time"},
	{"trace", "execution trace for ?seconds= (1-120, default 1); one at a time"},
	{"heap", "live heap allocations, pprof format; ?gc=1 collects first"},
	{"allocs", "every allocation since start, pprof format"},
	{"goroutine", "goroutine stacks; ?debug=1 or 2 for text"},
	{"threadcreate", "stack traces that led to a new OS thread"},
}

func debugProfileKnown(name string) bool {
	for _, p := range debugProfiles {
		if p.Name == name {
			return true
		}
	}
	return false
}

// debugGate admits a request to the debug routes. It is not mutating(), for the
// reason pushListGate is not: in the legacy single-token model mutating() lets every
// caller in when the all-verbs flag is set, and reading a process's internals must
// never be allowed without a per-principal grant. With no principal table nobody
// can hold it, so every request is refused.
//
// The grant is checked BEFORE the switch, so a caller without it learns nothing
// about how this machine is configured.
func debugGate(svc *Service, cfg Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = withTiming(r)
		p, ok := principalOf(r)
		if !ok {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnauthorized, Machine: svc.Self(),
				Message: "this machine has no principal table, so no caller can hold the debug grant (§6): " +
					"its process can be profiled only on a machine that identifies its callers"})
			return
		}
		if !p.Allows(GrantDebug) {
			logDenied(p, GrantDebug, svc.Self(), r)
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorUnauthorized, Machine: svc.Self(),
				Message: "principal " + p.Name + " does not hold the debug grant (§6). " +
					"Every grant defaults to denied, on a fresh deployment as much as an " +
					"established one — this is expected until an operator adds debug " +
					"to this principal's grants, not a bug.",
			})
			return
		}
		if !cfg.Profiling {
			logDenied(p, GrantDebug, svc.Self(), r)
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorUnsupported, Machine: svc.Self(),
				Message: "this machine does not offer the profiling endpoint: it is off unless the " +
					"operator sets \"profilingEndpoint\": true in the config file and restarts (#450). " +
					"The debug grant is held; the switch is what is missing.",
			})
			return
		}
		aw := &auditWriter{ResponseWriter: w}
		next(aw, r)
		logMutation(p, GrantDebug, svc.Self(), r, aw.status)
	}
}

// handleDebugIndex lists what the endpoint serves.
func handleDebugIndex() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		out := make([]item, 0, len(debugProfiles))
		for _, p := range debugProfiles {
			out = append(out, item{Name: p.Name, Description: p.Description})
		}
		writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
	}
}

// handleDebugProfile serves one profile by name, behind debugGate.
func handleDebugProfile(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !debugProfileKnown(name) {
			writeError(w, &fleet.Error{Kind: fleet.ErrorNotFound, Machine: svc.Self(),
				Message: "no profile named " + strconv.Quote(name) + " (GET /v1/debug/pprof/ lists them)"})
			return
		}
		bad := func(msg string) {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Machine: svc.Self(), Message: msg})
		}
		q := r.URL.Query()

		switch name {
		case "profile", "trace":
			seconds := map[string]int{"profile": 30, "trace": 1}[name]
			if raw := q.Get("seconds"); raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil || n < 1 || n > maxProfileSeconds {
					bad("seconds must be a whole number from 1 to " + strconv.Itoa(maxProfileSeconds) +
						", got " + strconv.Quote(raw))
					return
				}
				seconds = n
			}
			serveTimedProfile(w, r, svc, name, time.Duration(seconds)*time.Second)
			return
		}

		debug := 0
		if raw := q.Get("debug"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || n > 2 {
				bad("debug must be 0, 1 or 2, got " + strconv.Quote(raw))
				return
			}
			debug = n
		}
		if name == "heap" && q.Get("gc") != "" {
			runtime.GC() // what the standard handler's ?gc= does: an up-to-date live-heap view
		}
		if debug == 0 {
			w.Header().Set("Content-Type", "application/octet-stream")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_ = pprof.Lookup(name).WriteTo(w, debug)
	}
}

// serveTimedProfile samples for d and streams the result: the CPU profiler or the
// execution tracer, each a single process-wide resource.
//
// A second request while one is running — or anything else in the process using
// it — is a conflict, said in the error envelope and retryable, not the plain-text
// 500 the standard handler gives. The runtime itself is the lock: Start refuses
// when the resource is taken, so there is no second lock to forget to release.
//
// A caller that goes away ends the sampling at once; a profile nobody will read
// is not worth the seconds it occupies the profiler.
func serveTimedProfile(w http.ResponseWriter, r *http.Request, svc *Service, name string, d time.Duration) {
	start, stop := pprof.StartCPUProfile, pprof.StopCPUProfile
	if name == "trace" {
		start, stop = trace.Start, trace.Stop
	}
	// Set before Start, which may begin writing to w as soon as it returns; removed
	// again below if it refuses, so the refusal is an envelope and not a mislabelled
	// binary body.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := start(w); err != nil {
		w.Header().Del("Content-Disposition")
		writeError(w, &fleet.Error{Kind: fleet.ErrorConflict, Machine: svc.Self(), Retryable: true,
			Message: "a " + name + " cannot be started now: " + err.Error() + ". The process has one at a " +
				"time; try again when it finishes."})
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-r.Context().Done():
	}
	stop()
}
