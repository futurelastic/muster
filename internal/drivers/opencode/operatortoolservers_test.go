package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/sessionenv"
)

// muster #466: tool servers the operator defines once per machine reach the
// isolated sessions their selector matches, and never more than the ruling allows.
// The parsing, the selector, the rendering and each refusal are unit-tested on their
// own; what a session is given and what happens when the runtime does not connect a
// server is tested through the stand-in runtime (fakeopencode_test.go). The gated
// integration test pins the same against the real runtime.

// secretValue stands for a credential somebody wrote into the setting by mistake.
// No refusal may repeat it.
const secretValue = "SECRET-TOKEN-do-not-quote-me"

const localServer = `{"type":"local","command":["/usr/local/bin/cards","--stdio"],
  "environment":{"CARDS_KEY":"{env:CARDS_KEY}"},"cwd":"/var/lib/cards","timeout":10000,
  "appliesTo":{"agents":["reviewer"],"permissionModes":["default","bypass"]}}`

const remoteServer = `{"type":"remote","url":"https://tools.example/mcp",
  "headers":{"Authorization":"Bearer {env:ISSUES_TOKEN}","X-Key":"{env:ISSUES_KEY}"},"timeout":5000}`

func mustParseToolServers(t *testing.T, body string) OperatorToolServers {
	t.Helper()
	got, err := ParseOperatorToolServers(json.RawMessage(body))
	if err != nil {
		t.Fatalf("ParseOperatorToolServers: %v", err)
	}
	return got
}

func TestOperatorToolServers_TheSettingIsParsedAsWritten(t *testing.T) {
	got := mustParseToolServers(t, `{"cards":`+localServer+`,"issues":`+remoteServer+`}`)
	if got.Len() != 2 {
		t.Fatalf("servers = %d, want 2", got.Len())
	}
	cards, issues := got.servers[0], got.servers[1] // sorted by name
	if cards.name != "cards" || !cards.local || cards.command[1] != "--stdio" || cards.cwd != "/var/lib/cards" || cards.timeoutMs != 10000 {
		t.Errorf("cards = %+v", cards)
	}
	if !reflect.DeepEqual(cards.creds, []string{"CARDS_KEY"}) {
		t.Errorf("cards credentials = %v, want CARDS_KEY", cards.creds)
	}
	if !reflect.DeepEqual(cards.scope.Agents, []string{"reviewer"}) || !reflect.DeepEqual(cards.scope.PermissionModes, []string{"default", "bypass"}) {
		t.Errorf("cards scope = %+v", cards.scope)
	}
	if issues.name != "issues" || issues.local || issues.url != "https://tools.example/mcp" {
		t.Errorf("issues = %+v", issues)
	}
	if !reflect.DeepEqual(issues.creds, []string{"ISSUES_KEY", "ISSUES_TOKEN"}) {
		t.Errorf("issues credentials = %v, want the two a header refers to, sorted", issues.creds)
	}
	for _, empty := range []string{``, `null`, ` `, `{}`} {
		if none := mustParseToolServers(t, empty); none.Len() != 0 {
			t.Errorf("%q parsed to %d servers, want none", empty, none.Len())
		}
	}
}

// One case for each refusal the setting can earn at start (the doctor and a reload
// agree with the start because they call the same function). Each says what is
// wrong, and none quotes a value.
func TestOperatorToolServers_ASettingThatCannotBeUsedIsRefusedWithTheReason(t *testing.T) {
	for _, tc := range []struct {
		name, setting, reason string
	}{
		{"not an object", `["cards"]`, "must be an object"},
		{"a server twice", `{"cards":{"type":"local","command":["/bin/x"]},"cards":{"type":"local","command":["/bin/x"]}}`, "appears twice"},
		{"a server that is not an object", `{"cards":"/bin/x"}`, "JSON object"},
		{"a bad name", `{"has space":{"type":"local","command":["/bin/x"]}}`, "not a usable tool server name"},
		{"an empty name", `{"":{"type":"local","command":["/bin/x"]}}`, "not a usable tool server name"},
		{"no type", `{"cards":{"command":["/bin/x"]}}`, `no "type"`},
		{"an unknown type", `{"cards":{"type":"sse","url":"https://x.example"}}`, `"local" or "remote"`},
		{"a permission key", `{"cards":{"type":"local","command":["/bin/x"],"permission":{"bash":"allow"}}}`, `"permission" is not accepted`},
		{"a tools key", `{"cards":{"type":"local","command":["/bin/x"],"tools":{"bash":true}}}`, `"tools" is not accepted`},
		{"an agent key", `{"cards":{"type":"local","command":["/bin/x"],"agent":{}}}`, `"agent" is not accepted`},
		{"a provider key", `{"cards":{"type":"local","command":["/bin/x"],"provider":{}}}`, `"provider" is not accepted`},
		{"an oauth key", `{"cards":{"type":"remote","url":"https://x.example","oauth":{"clientSecret":"` + secretValue + `"}}}`, `"oauth" is not accepted`},
		{"an enabled key", `{"cards":{"type":"local","command":["/bin/x"],"enabled":false}}`, `"enabled" is not accepted`},
		{"a key of the other kind", `{"cards":{"type":"local","command":["/bin/x"],"url":"https://x.example"}}`, "not a key of a local server"},
		{"an unknown key", `{"cards":{"type":"local","command":["/bin/x"],"shell":true}}`, "is not a key this setting accepts"},
		{"a local server with no command", `{"cards":{"type":"local"}}`, `needs a "command"`},
		{"an empty command", `{"cards":{"type":"local","command":[]}}`, "non-empty array"},
		{"a command of non-strings", `{"cards":{"type":"local","command":["/bin/x",1]}}`, "non-empty array of strings"},
		{"a relative executable", `{"cards":{"type":"local","command":["cards"]}}`, "absolute path"},
		{"a reference in the command", `{"cards":{"type":"local","command":["/bin/x","--token","{env:T}"]}}`, "command line"},
		{"a file in the command", `{"cards":{"type":"local","command":["/bin/x","{file:/etc/x}"]}}`, "{file:"},
		{"a literal in environment", `{"cards":{"type":"local","command":["/bin/x"],"environment":{"KEY":"` + secretValue + `"}}}`, "not a reference"},
		{"a half reference in environment", `{"cards":{"type":"local","command":["/bin/x"],"environment":{"KEY":"x{env:K}"}}}`, "not a reference"},
		{"a scheme in environment", `{"cards":{"type":"local","command":["/bin/x"],"environment":{"KEY":"Bearer {env:K}"}}}`, "not a reference"},
		{"a file in environment", `{"cards":{"type":"local","command":["/bin/x"],"environment":{"KEY":"{file:/etc/x}"}}}`, "{file:"},
		{"a bad variable name in environment", `{"cards":{"type":"local","command":["/bin/x"],"environment":{"1KEY":"{env:K}"}}}`, "not a usable variable name"},
		{"a literal header", `{"cards":{"type":"remote","url":"https://x.example","headers":{"Authorization":"Bearer ` + secretValue + `"}}}`, "not a reference"},
		{"a bad header name", `{"cards":{"type":"remote","url":"https://x.example","headers":{"Bad Name":"{env:K}"}}}`, "not a header field name"},
		{"a driver-owned variable", `{"cards":{"type":"local","command":["/bin/x"],"environment":{"KEY":"{env:OPENCODE_SERVER_PASSWORD}"}}}`, "a variable this driver sets"},
		{"a driver-owned header reference", `{"cards":{"type":"remote","url":"https://x.example","headers":{"X":"{env:HOME}"}}}`, "a variable this driver sets"},
		{"no url", `{"cards":{"type":"remote"}}`, `needs a "url"`},
		{"an unusable url", `{"cards":{"type":"remote","url":"not a url"}}`, "not a usable address"},
		{"plain http to a remote host", `{"cards":{"type":"remote","url":"http://tools.example/mcp"}}`, "must be https"},
		{"another scheme", `{"cards":{"type":"remote","url":"ftp://tools.example/mcp"}}`, "must be https"},
		{"user info in the url", `{"cards":{"type":"remote","url":"https://user:` + secretValue + `@tools.example/mcp"}}`, "user name or password"},
		{"a query in the url", `{"cards":{"type":"remote","url":"https://tools.example/mcp?key=` + secretValue + `"}}`, "query string"},
		{"a reference in the url", `{"cards":{"type":"remote","url":"https://tools.example/{env:K}"}}`, "command line or in a url"},
		{"a timeout of zero", `{"cards":{"type":"local","command":["/bin/x"],"timeout":0}}`, `"timeout"`},
		{"a timeout past the bound", `{"cards":{"type":"local","command":["/bin/x"],"timeout":600001}}`, `"timeout"`},
		{"a fractional timeout", `{"cards":{"type":"local","command":["/bin/x"],"timeout":1.5}}`, `"timeout"`},
		{"a relative cwd", `{"cards":{"type":"local","command":["/bin/x"],"cwd":"cards"}}`, `"cwd" must be an absolute`},
		{"an unknown appliesTo key", `{"cards":{"type":"local","command":["/bin/x"],"appliesTo":{"hosts":["a"]}}}`, "is not a key of appliesTo"},
		{"an empty appliesTo entry", `{"cards":{"type":"local","command":["/bin/x"],"appliesTo":{"agents":[""]}}}`, "empty entry"},
		{"an appliesTo that is not a list", `{"cards":{"type":"local","command":["/bin/x"],"appliesTo":{"agents":"reviewer"}}}`, "array of strings"},
		{"runtimes without opencode", `{"cards":{"type":"local","command":["/bin/x"],"appliesTo":{"runtimes":["claude"]}}}`, "could never apply"},
		{"a bad permission mode", `{"cards":{"type":"local","command":["/bin/x"],"appliesTo":{"permissionModes":["plan"]}}}`, `not "default" or "bypass"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseOperatorToolServers(json.RawMessage(tc.setting))
			if err == nil {
				t.Fatal("ParseOperatorToolServers succeeded; want a refusal")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error = %q, want it to say %q", err, tc.reason)
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Errorf("error = %q quotes a value from the setting", err)
			}
		})
	}
}

// Plain http is accepted only to a loopback address.
func TestOperatorToolServers_PlainHTTPIsAcceptedToLoopbackOnly(t *testing.T) {
	for _, host := range []string{"localhost:9000", "127.0.0.1:9000", "[::1]:9000"} {
		mustParseToolServers(t, `{"x":{"type":"remote","url":"http://`+host+`/mcp"}}`)
	}
}

func TestOperatorToolServers_TheSelectorExcludesBypassUnlessItNamesIt(t *testing.T) {
	bypass := fleet.SessionSpec{PermissionMode: fleet.PermissionModeBypass, Agent: "reviewer", Marker: "m"}
	plain := fleet.SessionSpec{Agent: "reviewer", Marker: "m"}
	other := fleet.SessionSpec{Agent: "other"}
	otherBypass := fleet.SessionSpec{Agent: "other", PermissionMode: fleet.PermissionModeBypass}
	for _, tc := range []struct {
		name  string
		scope ToolServerScope
		spec  fleet.SessionSpec
		want  bool
	}{
		{"empty selector, a plain session", ToolServerScope{}, plain, true},
		{"empty selector, a bypass session", ToolServerScope{}, bypass, false},
		{"agents, a plain match", ToolServerScope{Scope: sessionenv.Scope{Agents: []string{"reviewer"}}}, plain, true},
		{"agents, the same agent in bypass: naming the agent is not naming bypass", ToolServerScope{Scope: sessionenv.Scope{Agents: []string{"reviewer"}}}, bypass, false},
		{"agents, no match", ToolServerScope{Scope: sessionenv.Scope{Agents: []string{"reviewer"}}}, other, false},
		{"markers", ToolServerScope{Scope: sessionenv.Scope{Markers: []string{"m"}}}, plain, true},
		{"runtimes naming opencode", ToolServerScope{Scope: sessionenv.Scope{Runtimes: []string{"opencode"}}}, plain, true},
		{"runtimes naming another", ToolServerScope{Scope: sessionenv.Scope{Runtimes: []string{"claude"}}}, plain, false},
		{"default only", ToolServerScope{PermissionModes: []string{"default"}}, plain, true},
		{"default only, bypass", ToolServerScope{PermissionModes: []string{"default"}}, bypass, false},
		{"bypass only, a bypass session", ToolServerScope{PermissionModes: []string{"bypass"}}, bypass, true},
		{"bypass only, a plain session", ToolServerScope{PermissionModes: []string{"bypass"}}, plain, false},
		{"both, bypass", ToolServerScope{PermissionModes: []string{"default", "bypass"}}, bypass, true},
		{"both, plain", ToolServerScope{PermissionModes: []string{"default", "bypass"}}, plain, true},
		{"bypass named and an agent that does not match", ToolServerScope{Scope: sessionenv.Scope{Agents: []string{"reviewer"}}, PermissionModes: []string{"bypass"}}, otherBypass, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scope.Matches(DefaultRuntime, tc.spec); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOperatorToolServers_WhatReachesTheSessionIsReSerialisedAndCarriesNothingElse(t *testing.T) {
	got := mustParseToolServers(t, `{"cards":`+localServer+`,"issues":`+remoteServer+`}`)
	raw, err := json.Marshal(map[string]any{"cards": got.servers[0].render(), "issues": got.servers[1].render()})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	cards, issues := out["cards"], out["issues"]
	if cards["type"] != "local" || cards["enabled"] != true || cards["cwd"] != "/var/lib/cards" || cards["timeout"] != float64(10000) {
		t.Errorf("cards = %v", cards)
	}
	if env, _ := cards["environment"].(map[string]any); env["CARDS_KEY"] != "{env:CARDS_KEY}" {
		t.Errorf("cards environment = %v, want the reference exactly as written", cards["environment"])
	}
	if issues["type"] != "remote" || issues["enabled"] != true || issues["oauth"] != false {
		t.Errorf("issues = %v, want enabled and oauth false written by the service", issues)
	}
	for name, def := range out {
		for _, key := range []string{"appliesTo", "permission", "tools", "agent", "provider"} {
			if _, ok := def[key]; ok {
				t.Errorf("%s carries %q", name, key)
			}
		}
	}
	if _, ok := cards["oauth"]; ok {
		t.Error("a local server carries an oauth entry")
	}
}

// What the operator's servers may not do: the session's configuration with them,
// minus `mcp`, is the configuration without them, whatever the posture.
func TestOperatorToolServers_TheyNeverChangeAnythingButTheirOwnKey(t *testing.T) {
	agents, err := parseOperatorAgents([]byte(operatorAgentsFile))
	if err != nil {
		t.Fatal(err)
	}
	servers := map[string]any{"cards": mustParseToolServers(t, `{"cards":`+localServer+`}`).servers[0].render()}
	read := func(dir string) map[string]any {
		raw, err := os.ReadFile(filepath.Join(dir, permissionConfigRel))
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	plan := &sandboxPlan{read: []string{"/work", "/opt/tools"}, write: []string{"/work"}}
	for _, tc := range []struct {
		name   string
		bypass bool
		plan   *sandboxPlan
		agents operatorAgents
		opts   []configOption
	}{
		{"a plain session", false, nil, nil, nil},
		{"a plain session with agents", false, nil, agents, nil},
		{"a plain session with read roots", false, nil, agents, []configOption{withReadRoots("/knowledge")}},
		{"a bypass session", true, plan, nil, nil},
		{"a bypass session with agents", true, plan, agents, nil},
		{"a bypass session with no sandbox the operator allowed", true, nil, agents, []configOption{withUnconfinedBypass()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			without, with := t.TempDir(), t.TempDir()
			if err := writeSessionConfig(without, tc.bypass, tc.plan, providerPolicy{}, tc.agents, []string{"/x/instructions.md"}, tc.opts...); err != nil {
				t.Fatal(err)
			}
			opts := append(append([]configOption(nil), tc.opts...), withToolServers(servers))
			if err := writeSessionConfig(with, tc.bypass, tc.plan, providerPolicy{}, tc.agents, []string{"/x/instructions.md"}, opts...); err != nil {
				t.Fatal(err)
			}
			cfgWith, cfgWithout := read(with), read(without)
			if _, ok := cfgWith["mcp"]; !ok {
				t.Fatal("the session's config does not list the tool server")
			}
			if _, ok := cfgWithout["mcp"]; ok {
				t.Fatal("a session with no tool server got an mcp entry")
			}
			delete(cfgWith, "mcp")
			if !reflect.DeepEqual(cfgWith, cfgWithout) {
				t.Errorf("the tool servers changed more than their own key:\nwith (minus mcp): %v\nwithout:          %v", cfgWith, cfgWithout)
			}
		})
	}
}

// bareDriver is a driver with nothing but what the selection and its refusals read.
func bareDriver(servers OperatorToolServers) *Driver {
	return &Driver{runtime: DefaultRuntime, machine: "test-machine", toolServers: servers}
}

func executable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func settingFor(command string, extra string) string {
	b, _ := json.Marshal(command)
	return `{"cards":{"type":"local","command":[` + string(b) + `]` + extra + `}}`
}

func TestOperatorToolServers_ASessionGetsTheServersWhoseSelectorMatches(t *testing.T) {
	exe := executable(t)
	d := bareDriver(mustParseToolServers(t, `{"everyone":{"type":"local","command":["`+exe+`"]},
		"reviewers":{"type":"local","command":["`+exe+`"],"appliesTo":{"agents":["reviewer"]}},
		"bypassers":{"type":"local","command":["`+exe+`"],"appliesTo":{"permissionModes":["bypass"]}}}`))
	names := func(spec fleet.SessionSpec) []string {
		cfg, err := d.operatorToolServersFor(spec, map[string]string{})
		if err != nil {
			t.Fatalf("operatorToolServersFor: %v", err)
		}
		return cfg.names()
	}
	if got := names(fleet.SessionSpec{}); !reflect.DeepEqual(got, []string{"everyone"}) {
		t.Errorf("a plain session got %v, want only everyone", got)
	}
	if got := names(fleet.SessionSpec{Agent: "reviewer"}); !reflect.DeepEqual(got, []string{"everyone", "reviewers"}) {
		t.Errorf("a reviewer got %v, want everyone and reviewers", got)
	}
	if got := names(fleet.SessionSpec{Agent: "reviewer", PermissionMode: fleet.PermissionModeBypass}); !reflect.DeepEqual(got, []string{"bypassers"}) {
		t.Errorf("a bypass reviewer got %v, want only bypassers: bypass is excluded unless a selector names it", got)
	}
	if got := bareDriver(OperatorToolServers{}); got != nil {
		cfg, err := got.operatorToolServersFor(fleet.SessionSpec{}, nil)
		if err != nil || len(cfg.servers) != 0 || cfg.rendered != nil {
			t.Errorf("a machine with none: %+v, %v", cfg, err)
		}
	}
}

// One test for each refusal a start can earn once it knows the session. Each is an
// unsupported refusal naming the server, and none quotes a value.
func TestOperatorToolServers_AServerThatCannotBeGivenToTheSessionRefusesTheStart(t *testing.T) {
	exe := executable(t)
	notExecutable := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(notExecutable, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withCred := `{"cards":{"type":"local","command":["` + exe + `"],"environment":{"CARDS_KEY":"{env:CARDS_KEY}"}}}`
	for _, tc := range []struct {
		name    string
		setting string
		spec    fleet.SessionSpec
		extra   map[string]string
		reason  string
		skipWin bool
	}{
		{"an executable that is missing", settingFor(filepath.Join(t.TempDir(), "gone"), ""), fleet.SessionSpec{}, map[string]string{}, "executable cannot be read", false},
		{"an executable that is a directory", settingFor(t.TempDir(), ""), fleet.SessionSpec{}, map[string]string{}, "not a regular file", false},
		{"an executable without the executable bit", settingFor(notExecutable, ""), fleet.SessionSpec{}, map[string]string{}, "not executable", true},
		{"a credential no sessionEnv entry supplies", withCred, fleet.SessionSpec{}, map[string]string{}, "none of this machine's sessionEnv entries", false},
		{"a credential the create supplies itself", withCred, fleet.SessionSpec{Env: map[string]string{"CARDS_KEY": secretValue}},
			map[string]string{"CARDS_KEY": secretValue}, "omit it from the request", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skipWin {
				skipOnWindows(t, "a file mode does not carry an executable bit on windows")
			}
			d := bareDriver(mustParseToolServers(t, tc.setting))
			_, err := d.operatorToolServersFor(tc.spec, tc.extra)
			if kindOfErr(err) != fleet.ErrorUnsupported {
				t.Fatalf("operatorToolServersFor = %v, want an unsupported refusal: the operator must repair it", err)
			}
			for _, want := range []string{`"cards"`, "operator must repair", tc.reason} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Errorf("refusal = %q quotes a value", err)
			}
		})
	}

	t.Run("a credential the machine supplies is accepted", func(t *testing.T) {
		d := bareDriver(mustParseToolServers(t, withCred))
		cfg, err := d.operatorToolServersFor(fleet.SessionSpec{}, map[string]string{"CARDS_KEY": "from-the-machine"})
		if err != nil || len(cfg.servers) != 1 {
			t.Fatalf("operatorToolServersFor = %+v, %v, want the server", cfg, err)
		}
	})
	t.Run("a server another agent's selector excludes is not judged", func(t *testing.T) {
		d := bareDriver(mustParseToolServers(t, `{"cards":{"type":"local","command":["/nonexistent/x"],"appliesTo":{"agents":["other"]}}}`))
		if _, err := d.operatorToolServersFor(fleet.SessionSpec{Agent: "reviewer"}, nil); err != nil {
			t.Errorf("a server that does not apply refused the start: %v", err)
		}
	})
}

func TestOperatorToolServers_ARemoteServerIsRefusedForALoopbackOnlySandbox(t *testing.T) {
	d := bareDriver(mustParseToolServers(t, `{"issues":`+remoteServer+`,"cards":{"type":"local","command":["/bin/x"]}}`))
	cfg := toolServerConfig{servers: d.toolServers.servers}
	closed := &sandboxPlan{network: fleet.SandboxNetworkClosed}
	err := d.checkToolServersNetwork(cfg, closed)
	if kindOfErr(err) != fleet.ErrorUnsupported || !strings.Contains(err.Error(), `"issues"`) || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("checkToolServersNetwork = %v, want an unsupported refusal naming the remote server", err)
	}
	for name, plan := range map[string]*sandboxPlan{"open": {network: fleet.SandboxNetworkOpen}, "unset": {}, "no sandbox": nil} {
		if err := d.checkToolServersNetwork(cfg, plan); err != nil {
			t.Errorf("%s: %v, want it allowed", name, err)
		}
	}
	local := toolServerConfig{servers: d.toolServers.servers[:1]}
	if d.toolServers.servers[0].name != "cards" {
		t.Fatal("servers are not sorted by name")
	}
	if err := d.checkToolServersNetwork(local, closed); err != nil {
		t.Errorf("a local server under a closed network: %v, want it allowed", err)
	}
}

func TestOperatorToolServers_ASharedDriverRefusesAServerThatApplies(t *testing.T) {
	d := bareDriver(mustParseToolServers(t, `{"cards":{"type":"local","command":["/bin/x"],"appliesTo":{"agents":["reviewer"]}}}`))
	d.shared = &server{}
	err := d.checkToolServersShared(fleet.SessionSpec{Agent: "reviewer"})
	if kindOfErr(err) != fleet.ErrorUnsupported || !strings.Contains(err.Error(), "shared mode") {
		t.Fatalf("checkToolServersShared = %v, want an unsupported refusal naming shared mode", err)
	}
	if err := d.checkToolServersShared(fleet.SessionSpec{Agent: "other"}); err != nil {
		t.Errorf("a session no server applies to was refused: %v", err)
	}
}

// ---- Through the stand-in runtime ------------------------------------------

func toolServerDriver(t *testing.T, setting string, extra ...Option) (*Driver, string) {
	t.Helper()
	opts := append([]Option{WithOperatorToolServers(mustParseToolServers(t, setting))}, extra...)
	return newIsolatedDriver(t, opts...)
}

func mcpNamesIn(cfg map[string]any) []string {
	m, _ := cfg["mcp"].(map[string]any)
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestOperatorToolServers_AnIsolatedSessionIsGivenTheServerAndTheRuntimeConnectsIt(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	exe := executable(t)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("from-the-machine"), 0o600); err != nil {
		t.Fatal(err)
	}
	setting := `{"cards":{"type":"local","command":["` + exe + `"],"environment":{"CARDS_KEY":"{env:CARDS_KEY}"}}}`
	d, _ := toolServerDriver(t, setting,
		WithSessionEnv([]sessionenv.Entry{{Name: "CARDS_KEY", FromFile: key, Required: true}}))

	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Prompt: "go"})
	cfg := sessionConfigOf(t, d, sess.ID)
	if got := mcpNamesIn(cfg); !reflect.DeepEqual(got, []string{"cards"}) {
		t.Fatalf("mcp in the session's config = %v, want cards", got)
	}
	// The setting names the variable, never its value.
	raw, _ := os.ReadFile(filepath.Join(mustSeen(t, d, sess.ID).srv.dir, permissionConfigRel))
	if strings.Contains(string(raw), "from-the-machine") {
		t.Errorf("the session's configuration carries the credential's value:\n%s", raw)
	}
	if !strings.Contains(string(raw), "{env:CARDS_KEY}") {
		t.Errorf("the session's configuration does not carry the reference:\n%s", raw)
	}
}

func mustSeen(t *testing.T, d *Driver, id string) knownSession {
	t.Helper()
	known, ok := d.wasSeen(id)
	if !ok || known.srv == nil {
		t.Fatalf("session %s is not running here", id)
	}
	return known
}

func TestOperatorToolServers_AMachineWithNoneIsUnchanged(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := newIsolatedDriver(t)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Prompt: "go"})
	if _, ok := sessionConfigOf(t, d, sess.ID)["mcp"]; ok {
		t.Error("a session on a machine with no tool servers got an mcp entry")
	}
}

func TestOperatorToolServers_ASessionThatNoSelectorMatchesGetsNoMcpEntry(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := toolServerDriver(t, `{"cards":{"type":"local","command":["`+executable(t)+`"],"appliesTo":{"agents":["reviewer"]}}}`)
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Prompt: "go"})
	if _, ok := sessionConfigOf(t, d, sess.ID)["mcp"]; ok {
		t.Error("a session no selector matches got an mcp entry")
	}
}

// The runtime's word is the status, and a server that is not connected is a refusal
// that carries that word and not the runtime's own error text, which echoes the
// command and the address.
func TestOperatorToolServers_AServerTheRuntimeDoesNotConnectRefusesTheCreateAndStartsNothing(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	for _, status := range []string{"failed", "needs_auth", "disabled"} {
		t.Run(status, func(t *testing.T) {
			d, root := toolServerDriver(t, `{"cards":{"type":"local","command":["`+executable(t)+`"]}}`)
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, ".fake-mcp-status.json"), []byte(`{"cards":"`+status+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Prompt: "go", Cwd: fleet.AbsolutePath(cwd)})
			if kindOfErr(err) != fleet.ErrorUnsupported {
				t.Fatalf("Create = %v, want an unsupported refusal: the operator must repair the server", err)
			}
			for _, want := range []string{`"cards"`, status, "not connected", "operator must repair"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "SECRET-RUNTIME-ERROR") || strings.Contains(err.Error(), "/operator/command") {
				t.Errorf("refusal = %q repeats the runtime's own error text", err)
			}
			waitForRoot(t, root)
		})
	}
}

// waitForRoot checks nothing is left under the session root once a refused create's
// own removal has finished.
func waitForRoot(t *testing.T, root string) {
	t.Helper()
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Errorf("a refused create left %d entries under the session root", len(left))
	}
}

func TestOperatorToolServers_ARuntimeThatCannotSayRefusesTheCreate(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	d, _ := newIsolatedDriver(t)
	// The stand-in answers 404 to /mcp for a session whose own configuration lists no
	// server, which is what an older runtime does. A driver that configured one and
	// got that answer must refuse rather than start on a guess.
	sess := mustCreate(t, d, "k1", fleet.SessionSpec{Prompt: "go"})
	known := mustSeen(t, d, sess.ID)
	known.srv.toolServers = []string{"cards"}
	err := d.checkToolServers(context.Background(), known.srv, known.cwd)
	if kindOfErr(err) != fleet.ErrorUnsupported || !strings.Contains(err.Error(), "could not report") {
		t.Fatalf("checkToolServers = %v, want an unsupported refusal saying the runtime could not report", err)
	}
	known.srv.toolServers = nil
}

func TestOperatorToolServers_ARelaunchGetsTheServersAsTheyAreThenAndIsRefusedWhenOneDoesNotConnect(t *testing.T) {
	skipOnWindows(t, "starts the stand-in runtime as a real child process")
	exe := executable(t)
	state := t.TempDir()
	setting := mustParseToolServers(t, `{"cards":{"type":"local","command":["`+exe+`"]}}`)
	d1 := newStateDriver(t, state, WithOperatorToolServers(setting))
	cwd := t.TempDir()
	sess := mustCreate(t, d1, "k1", fleet.SessionSpec{Prompt: "go", Cwd: fleet.AbsolutePath(cwd)})
	if err := d1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// The restart finds the server again.
	d2 := newStateDriver(t, state, WithOperatorToolServers(setting))
	got, ok := listed(t, d2)[sess.ID]
	if !ok || got.State.Status != fleet.StatusIdle {
		t.Fatalf("after the restart the session is %+v (listed %v), want it running again", got.State, ok)
	}
	if names := mcpNamesIn(sessionConfigOf(t, d2, sess.ID)); !reflect.DeepEqual(names, []string{"cards"}) {
		t.Errorf("the relaunched session lists %v, want cards", names)
	}
	if err := d2.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// Another restart, with the runtime not connecting it: the session is listed
	// unknown with the reason, not relaunched without its tool server.
	if err := os.WriteFile(filepath.Join(cwd, ".fake-mcp-status.json"), []byte(`{"cards":"failed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d3 := newStateDriver(t, state, WithOperatorToolServers(setting))
	t.Cleanup(func() { _ = d3.Shutdown() })
	got, ok = listed(t, d3)[sess.ID]
	if !ok {
		t.Fatal("the session vanished instead of being listed with a reason")
	}
	if got.State.Status != fleet.StatusUnknown || !strings.Contains(got.State.Evidence, `"cards"`) || !strings.Contains(got.State.Evidence, "failed") {
		t.Errorf("state = %q (%s), want unknown, naming the server and the status", got.State.Status, got.State.Evidence)
	}
}

func TestOperatorToolServers_ASharedDriverRefusesTheCreate(t *testing.T) {
	f := newFakeServer(t)
	d, err := newDriverWithOptions(t, f, WithOperatorToolServers(mustParseToolServers(t, `{"cards":{"type":"local","command":["/bin/x"]}}`)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = d.Create(context.Background(), testReq, "k1", fleet.SessionSpec{Prompt: "go", Cwd: fleet.AbsolutePath(t.TempDir())})
	if kindOfErr(err) != fleet.ErrorUnsupported || !strings.Contains(err.Error(), "shared mode") {
		t.Fatalf("Create = %v, want an unsupported refusal naming shared mode", err)
	}
}
