package tmux

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/compat"
)

// # B-CONFIG: a session whose configuration is only what the create names (#392)
//
// IsolateConfiguration makes claudeCodeCommand pass --setting-sources project and
// --strict-mcp-config. The unit tests prove the flags are on the argv; this
// check proves the candidate still honours them, which is the only thing that
// makes the guarantee true.
//
// Two sessions are launched in directories that carry the same four sources of
// ambient configuration, one with the requirement and one without:
//
//   - a hook in the PROJECT settings        (loaded by both: it is the working
//     directory's own configuration, which the requirement keeps),
//   - a hook in the LOCAL settings          (loaded only by the control),
//   - a tool server in the project's .mcp.json (started only by the control),
//   - a tool server named by --mcp-config   (started by both: it is what the
//     create names).
//
// Each source touches its own sentinel file when it runs, so the evidence is a
// directory listing: no model turn is spent and nothing is parsed from a screen.
//
// # Why the local layer stands in for the user layer
//
// The case that matters is the account's own settings and servers, but this run
// shares the real account's home (it reads and writes the same store the service
// does), and a check must not plant a hook in someone's real settings file. The
// switch excludes the user layer and the local layer by the same mechanism — it
// names the layers to LOAD — so a candidate that stops honouring it fails here
// whichever layer it leaks. The user layer itself was measured by hand when the
// flag was chosen (a user-level SessionStart hook ran without the flag and did
// not run with it).
const (
	cfgProjectHook = "cfc-project-hook"
	cfgLocalHook   = "cfc-local-hook"
	cfgAmbientMcp  = "cfc-ambient-mcp"
	cfgNamedMcp    = "cfc-named-mcp"
)

var (
	// compatConfigWait bounds how long the sources are given to run once the
	// composer is up. Hooks fire at start and servers are spawned as the session
	// comes up, so this is generous rather than tight.
	compatConfigWait = 15 * time.Second
	// compatConfigSettle is the extra time a session that should show NOTHING
	// extra is watched: an absence is only evidence once a fair wait has passed.
	compatConfigSettle = 3 * time.Second
)

// compatConfigRoles are the two directories the check uses.
var compatConfigRoles = []string{"g", "gc"}

func compatConfigMcpFile(scratch, role string) string {
	return filepath.Join(scratch, "mcp-"+role+".json")
}

// plantCompatConfiguration writes the four sources into the directory of each
// role, and the file that names the tool server. It overwrites the local
// settings the world wrote for every directory, keeping what that file was for
// (remote control off) and adding the hook.
func plantCompatConfiguration(scratch string, dirs map[string]string) error {
	hook := func(path string) map[string]any {
		return map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": "touch " + shQuote(path)},
		}}}}
	}
	server := func(path string) map[string]any {
		return map[string]any{"command": "sh", "args": []string{"-c", "touch " + shQuote(path) + "; sleep 300"}}
	}
	write := func(path string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(b, '\n'), 0o600)
	}
	for _, role := range compatConfigRoles {
		dir, ok := dirs[role]
		if !ok {
			return fmt.Errorf("no working directory for %q", role)
		}
		claude := filepath.Join(dir, ".claude")
		if err := write(filepath.Join(claude, "settings.json"), map[string]any{
			// Project settings. Remote control stays off here too: the local file
			// that normally says so is exactly what the requirement leaves unloaded.
			"remoteControlAtStartup":     false,
			"enableAllProjectMcpServers": true,
			"hooks":                      hook(filepath.Join(dir, cfgProjectHook)),
		}); err != nil {
			return err
		}
		if err := write(filepath.Join(claude, "settings.local.json"), map[string]any{
			"remoteControlAtStartup": false,
			"hooks":                  hook(filepath.Join(dir, cfgLocalHook)),
		}); err != nil {
			return err
		}
		if err := write(filepath.Join(dir, ".mcp.json"), map[string]any{
			"mcpServers": map[string]any{"ambient": server(filepath.Join(dir, cfgAmbientMcp))},
		}); err != nil {
			return err
		}
		if err := write(compatConfigMcpFile(scratch, role), map[string]any{
			"mcpServers": map[string]any{"named": server(filepath.Join(dir, cfgNamedMcp))},
		}); err != nil {
			return err
		}
	}
	return nil
}

// compatConfigSeen is which sources ran in one session.
type compatConfigSeen struct {
	boot compatBoot
	// seen maps each sentinel to whether its file exists after the wait.
	seen map[string]bool
}

func (h *compatHarness) addConfiguration(s *compat.Suite) {
	up := []string{"world.up"}
	s.Probes = append(s.Probes,
		compat.Probe{ID: "boot.g", Stage: 2, Needs: up, Budget: 90 * time.Second, Run: func(ctx context.Context) error {
			return h.bootConfiguration(ctx, &h.ev.g, "g", true)
		}},
		compat.Probe{ID: "boot.gc", Stage: 2, Needs: up, Budget: 90 * time.Second, Run: func(ctx context.Context) error {
			return h.bootConfiguration(ctx, &h.ev.gc, "gc", false)
		}},
	)
	s.Checks = append(s.Checks,
		compat.Check{ID: "B-CONFIG", Probes: []string{"boot.g", "boot.gc"}, Eval: h.evalConfiguration},
	)
}

// bootConfiguration launches the session for one role and records which sources
// ran. isolate is the only difference between the two sessions.
func (h *compatHarness) bootConfiguration(ctx context.Context, into *compatConfigSeen, role string, isolate bool) error {
	w := h.world
	err := h.boot(ctx, &into.boot, role, role, func(sp *fleet.SessionSpec) {
		sp.McpConfig = []fleet.AbsolutePath{fleet.AbsolutePath(compatConfigMcpFile(w.scratch, role))}
		sp.IsolateConfiguration = isolate
	}, compatReady, compatBootWait)
	if err != nil {
		return err
	}
	dir := w.dirs[role]
	into.seen = map[string]bool{}
	if !into.boot.ready {
		// No composer: the check reports it, and there is nothing fair to wait for.
		return nil
	}
	exists := func(name string) bool {
		_, e := os.Stat(filepath.Join(dir, name))
		return e == nil
	}
	// What must show up is waited for; what must NOT is watched for a fair time
	// after that, because an absence proves nothing the instant the composer is up.
	expect := []string{cfgProjectHook, cfgNamedMcp}
	if !isolate {
		expect = append(expect, cfgLocalHook, cfgAmbientMcp)
	}
	deadline := time.Now().Add(compatConfigWait)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		all := true
		for _, n := range expect {
			all = all && exists(n)
		}
		if all {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if isolate {
		select {
		case <-ctx.Done():
		case <-time.After(compatConfigSettle):
		}
	}
	for _, n := range []string{cfgProjectHook, cfgLocalHook, cfgAmbientMcp, cfgNamedMcp} {
		into.seen[n] = exists(n)
	}
	return nil
}

func (h *compatHarness) evalConfiguration() compat.Verdict {
	g, c := h.ev.g, h.ev.gc
	describe := map[string]string{
		cfgProjectHook: "the project settings' hook",
		cfgLocalHook:   "the local settings' hook",
		cfgAmbientMcp:  "the project's own tool server",
		cfgNamedMcp:    "the tool server named with --mcp-config",
	}
	// The premise: the control shows that all four sources really run here. If it
	// does not, an absence in the other session proves nothing.
	var dead []string
	for _, n := range []string{cfgProjectHook, cfgLocalHook, cfgAmbientMcp, cfgNamedMcp} {
		if !c.seen[n] {
			dead = append(dead, describe[n])
		}
	}
	if len(dead) > 0 {
		why := "the control session (without the requirement)"
		if !c.boot.ready {
			why += fmt.Sprintf(" never showed a composer within %s, and", compatBootWait)
		}
		return compat.Errored(fmt.Sprintf("the premise is missing: %s did not run %s, so an absence in the other session would prove nothing", why, strings.Join(dead, ", ")))
	}
	if !g.boot.ready {
		return compat.Failed(fmt.Sprintf("a session started with the requirement never showed a composer within %s (status %q; the pane shows: %s)",
			compatBootWait, g.boot.shot.state.Status, screenTail(g.boot.shot.sc, 3)))
	}
	var bad []string
	for _, n := range []string{cfgProjectHook, cfgNamedMcp} {
		if !g.seen[n] {
			bad = append(bad, "did not run "+describe[n]+", which the requirement is meant to keep")
		}
	}
	for _, n := range []string{cfgLocalHook, cfgAmbientMcp} {
		if g.seen[n] {
			bad = append(bad, "ran "+describe[n]+", which the requirement is meant to leave out")
		}
	}
	if len(bad) > 0 {
		return compat.Failed("a session started with --setting-sources project and --strict-mcp-config " + strings.Join(bad, "; ") +
			" — the candidate no longer honours the switches that confine which configuration a session loads")
	}
	return compat.Passed("a session started with --setting-sources project and --strict-mcp-config ran the project hook and started the tool server it was given, and ran neither the local-settings hook nor the project's own tool server; the same session without them ran all four")
}
