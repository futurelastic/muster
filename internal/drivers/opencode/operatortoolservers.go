package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// Operator tool servers (muster #466).
//
// An isolated session has its own HOME and XDG directories (env.go), so it reads
// no tool-server configuration the service user keeps, and the operator's agent
// file (operatoragents.go, #396) deliberately carries no tool server. A machine's
// operator who wants one tool server in front of every matching session, an
// interactive-card server say, had nowhere to say so. `opencode.mcp` in the file
// FLEET_CONFIG names is that place. The owner's ruling on the issue is the scope:
//
//   - ONLY THE OPERATOR adds one, there. A create still cannot: a per-create
//     `mcpConfig` stays refused on this runtime (ops.go), because starting a session
//     that also starts tool servers is a larger authority than starting a session.
//   - It NEVER WIDENS POLICY. A definition is parsed against an allowlist and
//     written back out by this driver, under the session's own `mcp` key and nowhere
//     else: no permission, provider, agent or tool-posture key can ride it (see
//     "Why it cannot widen" below). The permission posture and the provider policy
//     apply on top, unchanged.
//   - It is SCOPED. Each server carries an `appliesTo` selector shaped like a
//     sessionEnv entry's (runtimes, agents, markers) plus one axis of its own,
//     `permissionModes`. A bypass session (#283) is excluded unless that list names
//     "bypass" explicitly. See ToolServerScope.
//   - CREDENTIALS reach a server only through the machine's sessionEnv path. The
//     definition holds a REFERENCE, `{env:NAME}`, never a value: the runtime expands
//     it from the session's environment when it starts the server. A literal in
//     `environment` or `headers` is refused, so no secret sits in the setting, on a
//     command line or in a log.
//   - Like the operator's agents and instruction files, the declaration is fixed at
//     start (a change is restart-only), and what a session is given is judged again
//     on EVERY create and every relaunch; a running session keeps what it started
//     with. A definition that cannot be used refuses the create `unsupported`, with
//     the reason. It is never skipped: a session meant to have its tool server and
//     silently without it is the failure this exists to prevent.
//
// # Why it cannot widen
//
// The parser accepts a fixed set of keys per kind and refuses every other by name,
// so a definition cannot carry a `permission`, `tools`, `agent`, `provider`, `oauth`
// or `enabled`; the driver writes `enabled: true` itself and `oauth: false` for a
// remote server. What it renders is re-serialised from the parsed fields, never
// copied from the operator's text. writeSessionConfig puts it under `mcp`, a key
// nothing else there writes, so the provider keys, the agents, the instructions and
// the whole bypass `permission` block are computed exactly as before; a test pins
// that the configuration with servers, minus `mcp`, equals the one without.
//
// A tool server's calls are then judged by the posture the session already has,
// like a server a project defines today. A local server is a child of the runtime
// and runs inside the same sandbox launch. A remote server acts outside any sandbox
// by nature, which is why bypass is opt-in per server: naming "bypass" in
// `permissionModes` accepts that.
//
// # Measured against opencode 1.18.3 (the integration test pins it)
//
//   - `GET /mcp?directory=<cwd>` answers `{"<name>": {"status": ...}}` once every
//     server has settled (it waited for the connection attempts: about 0.3 s for a
//     local server, cached after). `connected` is the only status that means the
//     session has the server; a failed one says `failed` plus an `error` that echoes
//     the command, so a refusal here carries the status word and nothing else.
//   - `{env:NAME}` in `environment` is expanded from the runtime's own environment,
//     which is the session's (env.go): the credential arrives through sessionEnv.
//   - The session's opencode.json is the runtime's global level, so a project's own
//     `mcp` entry of the same name wins field by field, as for agents (#396). A
//     project can already define any server and name any variable in the session's
//     environment, so letting it override adds no authority.

// The refusal states. The reasons are written for an operator; none of them quotes
// a header value, an environment value or a runtime error.

// toolServerNameRe is what a server may be called: the runtime prefixes the tool
// names a server offers with it.
var toolServerNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// envRefRe is a whole value that is one reference to an environment variable.
var envRefRe = regexp.MustCompile(`^\{env:([A-Za-z_][A-Za-z0-9_]*)\}$`)

// headerRefRe is a header value: one reference, or `<scheme> <reference>`.
var headerRefRe = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9-]{0,31} )?\{env:([A-Za-z_][A-Za-z0-9_]*)\}$`)

// headerNameRe is an HTTP header field name (a token).
var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)

// toolServerMaxTimeoutMs bounds a server's connection timeout. Ten minutes is far
// past any real handshake and keeps a typo (a count of milliseconds written in
// seconds, the other way) from making a create wait for a day.
const toolServerMaxTimeoutMs = 600000

// toolServerConnectedStatus is the runtime's word for a server it is using.
const toolServerConnectedStatus = "connected"

// PermissionModes an appliesTo selector may name.
const (
	toolServerModeDefault = "default"
	toolServerModeBypass  = fleet.PermissionModeBypass
)

// ToolServerScope is the selector of one operator tool server: sessionenv's scope
// (runtimes AND (agents OR markers)) plus the permission modes the server applies
// to.
//
// Why a wrapper and not a field on sessionenv.Scope: that scope is shared with the
// sessionEnv entries, the credential rule and the create policy, and a field there
// would read as meaningful to all of them while only this one consults it.
//
// PermissionModes is the bypass axis. Empty means `["default"]`: a bypass session
// (#283) is excluded unless the list names "bypass". Naming an agent or a marker
// that happens to run in bypass does not name it: the mode is its own word.
type ToolServerScope struct {
	sessionenv.Scope
	PermissionModes []string
}

// Matches reports whether a session created by the driver for runtime, with spec,
// falls inside the selector.
func (s ToolServerScope) Matches(rt fleet.RuntimeId, spec fleet.SessionSpec) bool {
	if !s.Scope.Matches(rt, spec) {
		return false
	}
	mode := toolServerModeDefault
	if spec.PermissionMode == fleet.PermissionModeBypass {
		mode = toolServerModeBypass
	}
	modes := s.PermissionModes
	if len(modes) == 0 {
		modes = []string{toolServerModeDefault}
	}
	for _, m := range modes {
		if m == mode {
			return true
		}
	}
	return false
}

// toolServer is one validated definition.
type toolServer struct {
	name  string
	local bool
	// local
	command []string
	cwd     string
	env     map[string]string // variable -> reference, exactly as written
	// remote
	url     string
	headers map[string]string // header -> value with a reference, exactly as written
	// both
	timeoutMs int
	scope     ToolServerScope
	// creds are the names of the environment variables the definition refers to,
	// sorted: each must reach the session through the machine's sessionEnv.
	creds []string
}

// OperatorToolServers are the operator's definitions, parsed and checked. The zero
// value (and nil) is "none".
type OperatorToolServers struct {
	servers []toolServer // sorted by name
}

// Len is how many servers are defined.
func (o OperatorToolServers) Len() int { return len(o.servers) }

// ParseOperatorToolServers reads the `opencode.mcp` object of the config file. It is
// called at start, by the doctor and by a reload, so the three agree; the error says
// what is wrong in words an operator can act on and never quotes a value. Nil or an
// empty input means no server.
func ParseOperatorToolServers(raw json.RawMessage) (OperatorToolServers, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return OperatorToolServers{}, nil
	}
	members, err := decodeObject(raw)
	if err != nil {
		return OperatorToolServers{}, fmt.Errorf("opencode.mcp must be an object of tool servers keyed by name: %w", err)
	}
	out := OperatorToolServers{servers: make([]toolServer, 0, len(members))}
	for _, kv := range members {
		s, err := parseToolServer(kv.key, kv.value)
		if err != nil {
			return OperatorToolServers{}, err
		}
		out.servers = append(out.servers, s)
	}
	sort.Slice(out.servers, func(i, j int) bool { return out.servers[i].name < out.servers[j].name })
	return out, nil
}

// forbiddenToolServerKeys are keys a definition may not carry, with why: each is a
// way for the operator's setting to reach something the service owns.
var forbiddenToolServerKeys = map[string]string{
	"permission": "permissions are set by the service",
	"tools":      "permissions are set by the service",
	"agent":      "agents are set by the operator's agent file",
	"agents":     "agents are set by the operator's agent file",
	"provider":   "providers are set by the service",
	"oauth":      "a tool server's sign-in is not carried by this setting",
	"enabled":    "the service enables a configured server itself; remove a server or narrow its appliesTo to turn it off",
}

var (
	localToolServerKeys  = map[string]bool{"type": true, "command": true, "environment": true, "cwd": true, "timeout": true, "appliesTo": true}
	remoteToolServerKeys = map[string]bool{"type": true, "url": true, "headers": true, "timeout": true, "appliesTo": true}
)

func parseToolServer(name string, raw json.RawMessage) (toolServer, error) {
	s := toolServer{name: name}
	bad := func(format string, args ...any) (toolServer, error) {
		return toolServer{}, fmt.Errorf("opencode.mcp %q: "+format, append([]any{name}, args...)...)
	}
	if !toolServerNameRe.MatchString(name) {
		return toolServer{}, fmt.Errorf("opencode.mcp: %q is not a usable tool server name (1 to 64 letters, digits, - and _; it prefixes the tool names the server offers)", name)
	}
	members, err := decodeObject(raw)
	if err != nil {
		return bad("must be a JSON object: %v", err)
	}
	fields := make(map[string]json.RawMessage, len(members))
	for _, kv := range members {
		fields[kv.key] = kv.value
	}
	var kind string
	if v, ok := fields["type"]; !ok {
		return bad(`has no "type" ("local" or "remote")`)
	} else if err := json.Unmarshal(v, &kind); err != nil || (kind != "local" && kind != "remote") {
		return bad(`"type" must be "local" or "remote"`)
	}
	s.local = kind == "local"
	allowed := remoteToolServerKeys
	if s.local {
		allowed = localToolServerKeys
	}
	for _, kv := range members {
		if allowed[kv.key] {
			continue
		}
		if why, ok := forbiddenToolServerKeys[kv.key]; ok {
			return bad("%q is not accepted (%s)", kv.key, why)
		}
		if localToolServerKeys[kv.key] || remoteToolServerKeys[kv.key] {
			return bad("%q is not a key of a %s server", kv.key, kind)
		}
		return bad("%q is not a key this setting accepts (a %s server takes %s)", kv.key, kind, acceptedKeys(allowed))
	}

	if s.local {
		if err := s.parseLocal(fields, name); err != nil {
			return toolServer{}, err
		}
	} else if err := s.parseRemote(fields, name); err != nil {
		return toolServer{}, err
	}
	if v, ok := fields["timeout"]; ok {
		var n float64
		if err := json.Unmarshal(v, &n); err != nil || n != float64(int(n)) || n < 1 || n > toolServerMaxTimeoutMs {
			return bad(`"timeout" must be a whole number of milliseconds from 1 to %d`, toolServerMaxTimeoutMs)
		}
		s.timeoutMs = int(n)
	}
	if v, ok := fields["appliesTo"]; ok {
		scope, err := parseToolServerScope(v)
		if err != nil {
			return bad("appliesTo: %v", err)
		}
		s.scope = scope
	}
	seen := map[string]bool{}
	for _, ref := range s.references() {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if !sessionenv.ValidName(ref) {
			return bad("refers to %q, which is not a usable variable name", ref)
		}
		if ownedName(ref) {
			return bad("refers to %s, a variable this driver sets for every session", ref)
		}
		s.creds = append(s.creds, ref)
	}
	sort.Strings(s.creds)
	return s, nil
}

func acceptedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(quoteAll(keys), ", ")
}

// plainText refuses a value that carries a runtime substitution. In a field that is
// not a credential slot a substitution would put a value on a command line (command,
// url) or read a file outside the sessionEnv path.
func plainText(field, v string) error {
	switch {
	case strings.Contains(v, "{file:"):
		return fmt.Errorf("%s uses {file:...}, which reads a file outside the machine's sessionEnv; a credential reaches a tool server only as {env:NAME} in environment or headers", field)
	case strings.Contains(v, "{env:"):
		return fmt.Errorf("%s uses {env:...}, which would put the value on a command line or in a url; a credential goes in environment or headers", field)
	}
	return nil
}

func (s *toolServer) parseLocal(fields map[string]json.RawMessage, name string) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("opencode.mcp %q: "+format, append([]any{name}, args...)...)
	}
	rawCmd, ok := fields["command"]
	if !ok {
		return bad(`a local server needs a "command"`)
	}
	if err := json.Unmarshal(rawCmd, &s.command); err != nil || len(s.command) == 0 {
		return bad(`"command" must be a non-empty array of strings`)
	}
	if !filepath.IsAbs(s.command[0]) {
		return bad(`"command"[0] must be the absolute path of the executable (the service's own PATH is not the session's)`)
	}
	for i, a := range s.command {
		if err := plainText(fmt.Sprintf(`"command"[%d]`, i), a); err != nil {
			return bad("%v", err)
		}
	}
	if v, ok := fields["cwd"]; ok {
		if err := json.Unmarshal(v, &s.cwd); err != nil || !filepath.IsAbs(s.cwd) {
			return bad(`"cwd" must be an absolute path`)
		}
		if err := plainText(`"cwd"`, s.cwd); err != nil {
			return bad("%v", err)
		}
	}
	if v, ok := fields["environment"]; ok {
		m, err := stringObject(v)
		if err != nil {
			return bad(`"environment" must be an object of strings: %v`, err)
		}
		for k, val := range m {
			if !sessionenv.ValidName(k) {
				return bad(`"environment" names %q, which is not a usable variable name`, k)
			}
			if err := credentialSlot(`"environment"`, k, val, envRefRe); err != nil {
				return bad("%v", err)
			}
		}
		s.env = m
	}
	return nil
}

func (s *toolServer) parseRemote(fields map[string]json.RawMessage, name string) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("opencode.mcp %q: "+format, append([]any{name}, args...)...)
	}
	rawURL, ok := fields["url"]
	if !ok {
		return bad(`a remote server needs a "url"`)
	}
	if err := json.Unmarshal(rawURL, &s.url); err != nil || s.url == "" {
		return bad(`"url" must be a string`)
	}
	if err := plainText(`"url"`, s.url); err != nil {
		return bad("%v", err)
	}
	u, err := url.Parse(s.url)
	if err != nil || u.Host == "" {
		return bad(`"url" is not a usable address`)
	}
	switch {
	case u.User != nil:
		return bad(`"url" carries a user name or password; a credential goes in headers as {env:NAME}`)
	case u.RawQuery != "" || u.ForceQuery:
		return bad(`"url" carries a query string, which can hold a credential; a credential goes in headers as {env:NAME}`)
	case u.Fragment != "":
		return bad(`"url" carries a fragment`)
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return bad(`"url" must be https (plain http only to a loopback address)`)
	}
	if v, ok := fields["headers"]; ok {
		m, err := stringObject(v)
		if err != nil {
			return bad(`"headers" must be an object of strings: %v`, err)
		}
		for k, val := range m {
			if !headerNameRe.MatchString(k) {
				return bad(`"headers" names %q, which is not a header field name`, k)
			}
			if err := credentialSlot(`"headers"`, k, val, headerRefRe); err != nil {
				return bad("%v", err)
			}
		}
		s.headers = m
	}
	return nil
}

// credentialSlot judges one value of environment or headers: it must be a reference
// in the allowed form, never a literal. The value is not quoted: it may be exactly
// the secret the rule exists to keep out.
func credentialSlot(field, key, value string, form *regexp.Regexp) error {
	if strings.Contains(value, "{file:") {
		return fmt.Errorf("%s %q uses {file:...}, which reads a file outside the machine's sessionEnv; use {env:NAME}", field, key)
	}
	if !form.MatchString(value) {
		return fmt.Errorf("%s %q is not a reference: a value here is {env:NAME} (a header may add a scheme before it, `Bearer {env:NAME}`), "+
			"so a credential is never written into this setting; the machine's sessionEnv supplies it", field, key)
	}
	return nil
}

// references are the environment variable names the definition refers to.
func (s *toolServer) references() []string {
	var out []string
	add := func(m map[string]string, form *regexp.Regexp) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if g := form.FindStringSubmatch(m[k]); g != nil {
				out = append(out, g[1])
			}
		}
	}
	add(s.env, envRefRe)
	add(s.headers, headerRefRe)
	return out
}

// stringObject reads a JSON object whose values are all strings, refusing a key
// that appears twice.
func stringObject(raw json.RawMessage) (map[string]string, error) {
	members, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(members))
	for _, kv := range members {
		var v string
		if err := json.Unmarshal(kv.value, &v); err != nil {
			return nil, fmt.Errorf("the value of %q is not a string", kv.key)
		}
		out[kv.key] = v
	}
	return out, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseToolServerScope reads an appliesTo object: sessionEnv's three axes plus
// permissionModes.
func parseToolServerScope(raw json.RawMessage) (ToolServerScope, error) {
	var s ToolServerScope
	members, err := decodeObject(raw)
	if err != nil {
		return s, fmt.Errorf("must be a JSON object: %v", err)
	}
	for _, kv := range members {
		var list []string
		if err := json.Unmarshal(kv.value, &list); err != nil {
			return s, fmt.Errorf("%q must be an array of strings", kv.key)
		}
		for _, v := range list {
			if strings.TrimSpace(v) == "" {
				return s, fmt.Errorf("%q has an empty entry", kv.key)
			}
		}
		switch kv.key {
		case "runtimes":
			s.Runtimes = list
		case "agents":
			s.Agents = list
		case "markers":
			s.Markers = list
		case "permissionModes":
			for _, m := range list {
				if m != toolServerModeDefault && m != toolServerModeBypass {
					return s, fmt.Errorf(`"permissionModes" entry %q is not "default" or "bypass"`, m)
				}
			}
			s.PermissionModes = list
		default:
			return s, fmt.Errorf("%q is not a key of appliesTo (runtimes, agents, markers, permissionModes)", kv.key)
		}
	}
	if len(s.Runtimes) > 0 {
		reaches := false
		for _, r := range s.Runtimes {
			if r == string(DefaultRuntime) {
				reaches = true
			}
		}
		if !reaches {
			return s, fmt.Errorf(`"runtimes" does not name %q, so the server could never apply to a session here`, string(DefaultRuntime))
		}
	}
	return s, nil
}

// render is the definition as it is written into a session's opencode.json: the
// parsed fields, re-serialised, plus the keys the service sets itself. It never
// carries appliesTo and never anything the operator wrote that was not parsed.
func (s toolServer) render() map[string]any {
	out := map[string]any{"enabled": true}
	if s.local {
		out["type"] = "local"
		out["command"] = s.command
		if s.cwd != "" {
			out["cwd"] = s.cwd
		}
		if len(s.env) > 0 {
			out["environment"] = s.env
		}
	} else {
		out["type"] = "remote"
		out["url"] = s.url
		if len(s.headers) > 0 {
			out["headers"] = s.headers
		}
		// The service never lets the runtime start a sign-in of its own for a
		// remote server: a stored token is a credential this setting does not carry.
		out["oauth"] = false
	}
	if s.timeoutMs > 0 {
		out["timeout"] = s.timeoutMs
	}
	return out
}

// toolServerConfig is what one session start is given: the rendered definitions
// that apply to it, and their names for the status check after the runtime is up.
type toolServerConfig struct {
	servers  []toolServer
	rendered map[string]any
}

func (c toolServerConfig) names() []string {
	out := make([]string, 0, len(c.servers))
	for _, s := range c.servers {
		out = append(out, s.name)
	}
	return out
}

// operatorToolServersFor selects the operator's servers for one session start and
// checks what can be checked before a process exists. Called before any side effect,
// at create and again at relaunch. extra is the session's environment as the
// machine's sessionEnv and the create's own `env` merged it, names folded.
func (d *Driver) operatorToolServersFor(spec fleet.SessionSpec, extra map[string]string) (toolServerConfig, error) {
	var out toolServerConfig
	for _, s := range d.toolServers.servers {
		if !s.scope.Matches(d.runtime, spec) {
			continue
		}
		if err := d.checkToolServer(s, spec, extra); err != nil {
			return toolServerConfig{}, d.toolServerRefusal(s.name, err)
		}
		out.servers = append(out.servers, s)
	}
	if len(out.servers) == 0 {
		return toolServerConfig{}, nil
	}
	out.rendered = make(map[string]any, len(out.servers))
	for _, s := range out.servers {
		out.rendered[s.name] = s.render()
	}
	return out, nil
}

// toolServerRefusal is the refusal for a definition that cannot be used on this
// start. The operator's setting is at fault, not the request, so it is the kind the
// operator's agent file and instruction files answer with.
func (d *Driver) toolServerRefusal(name string, cause error) error {
	return &fleet.Error{
		Kind: fleet.ErrorUnsupported,
		Message: fmt.Sprintf("create: this machine's opencode tool server %q cannot be given to this session: %v; "+
			"an operator must repair that, and retrying this create will not help until they do", name, cause),
		Machine: d.machine,
	}
}

// checkToolServersShared refuses a create on a driver in shared mode when any of the
// operator's servers applies to it. One runtime for every session has no way to give
// one session a tool server another does not get, nor a credential only this session
// has. Refused, unlike the instruction files, which a shared runtime ignores: a
// silently missing tool server is the failure this exists to prevent. Called by
// Create, since a shared-mode create never starts a server of its own.
func (d *Driver) checkToolServersShared(spec fleet.SessionSpec) error {
	for _, s := range d.toolServers.servers {
		if s.scope.Matches(d.runtime, spec) {
			return d.toolServerRefusal(s.name, errors.New("this driver runs in shared mode, which cannot give one session a tool server another does not get"))
		}
	}
	return nil
}

// checkToolServer is the part of the judgement that needs the session.
func (d *Driver) checkToolServer(s toolServer, spec fleet.SessionSpec, extra map[string]string) error {
	for _, name := range s.creds {
		if callerSets(spec.Env, name) {
			// The caller's value would beat a non-required sessionEnv entry; the
			// credential for an operator's server is the machine's own, never a caller's.
			return fmt.Errorf("the create supplies %s, which the server's definition takes from the machine's sessionEnv; omit it from the request", name)
		}
		if _, ok := extra[envKey(name)]; !ok {
			return fmt.Errorf("it needs %s in the session's environment, and none of this machine's sessionEnv entries supplies it to this session", name)
		}
	}
	if s.local {
		info, err := os.Stat(s.command[0])
		switch {
		case err != nil:
			return fmt.Errorf("its executable cannot be read: %v", errors.Unwrap(err))
		case !info.Mode().IsRegular():
			return errors.New("its executable is not a regular file")
		case runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0:
			return errors.New("its executable is not executable")
		}
	}
	return nil
}

// callerSets reports whether a create's env names a variable, the way the platform's
// environment compares names.
func callerSets(env map[string]string, name string) bool {
	for k := range env {
		if envKey(k) == envKey(name) {
			return true
		}
	}
	return false
}

// checkToolServersNetwork refuses a remote server for a session whose sandbox
// allows loopback only: the runtime could never reach it, and the status check
// after the start would report that as a bare "failed".
func (d *Driver) checkToolServersNetwork(cfg toolServerConfig, plan *sandboxPlan) error {
	if plan == nil || plan.network != fleet.SandboxNetworkClosed {
		return nil
	}
	for _, s := range cfg.servers {
		if !s.local {
			return d.toolServerRefusal(s.name, errors.New("it is a remote server and this session's sandbox allows loopback only"))
		}
	}
	return nil
}

// toolServerSettle bounds how long the status check waits for a server that is
// still connecting, and how often it asks again.
const (
	toolServerSettleAttempts = 10
	toolServerSettleWait     = 300 * time.Millisecond
)

// checkToolServers asks the session's own runtime whether it has connected every
// server this session was configured with. Called once the runtime is up, at create
// and at relaunch: a server whose command cannot run inside the session's sandbox, or
// whose endpoint does not answer, would otherwise show only as a tool the model never
// had. The refusal names the server and the runtime's status word and nothing else:
// the runtime's own error text echoes the command and the address.
func (d *Driver) checkToolServers(ctx context.Context, srv *server, cwd fleet.AbsolutePath) error {
	if srv == nil || len(srv.toolServers) == 0 {
		return nil
	}
	path := "/mcp?directory=" + url.QueryEscape(string(cwd))
	var status map[string]struct {
		Status string `json:"status"`
	}
	for attempt := 0; ; attempt++ {
		status = nil
		if err := d.do(ctx, srv, "GET", path, nil, &status); err != nil {
			if isUnreachable(err) || isCtxErr(err) {
				return err
			}
			return d.toolServerRefusal(srv.toolServers[0],
				errors.New("the runtime could not report whether its tool servers connected, and a session is not started on a guess"))
		}
		pending := ""
		for _, name := range srv.toolServers {
			st, listed := status[name]
			switch {
			case listed && st.Status == toolServerConnectedStatus:
			case listed && st.Status == "pending":
				pending = name
			default:
				word := "not listed"
				if listed {
					word = st.Status
				}
				return d.toolServerRefusal(name, fmt.Errorf("the session's runtime reports it %s, not connected "+
					"(a local server's command must run inside the session's sandbox, a remote server's address must answer)", word))
			}
		}
		if pending == "" {
			return nil
		}
		if attempt+1 >= toolServerSettleAttempts {
			return d.toolServerRefusal(pending, errors.New("the session's runtime still reports it pending after waiting for it"))
		}
		select {
		case <-time.After(toolServerSettleWait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
