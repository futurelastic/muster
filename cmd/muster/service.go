package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/futurelastic/muster/internal/privfs"
)

// `muster service install | status | uninstall`: the single-machine setup of
// docs/install.md as one command, for the two service managers a per-user
// install can use — launchd's Background domain on macOS, `systemd --user` on
// Linux.
//
// # What it does, and what it deliberately does not
//
// It copies the running binary to a fixed per-user path (so an npm upgrade can
// never change what the unit executes), creates the state directory and a
// principal table, writes the unit, runs `doctor` under the unit's own
// environment, loads the unit and checks /v1/health. Multi-machine peers,
// inbox delivery and Windows stay manual; the command says where.
//
// # The principal table is not empty
//
// The procedure writes `{}` and enrols the supervisor next. A table with no
// principal fails config.load and the service refuses to start, so an installer
// that stopped at `{}` would hand over a unit that cannot run. A new table gets
// one principal, `supervisor`, with every grant a supervising client uses
// (relay only matters once peers exist). A table that already exists is never
// touched.
//
// # Restarting must not end the sessions
//
// The multiplexer server is started by this service, so under the managers'
// defaults it lives in the service's own process group (launchd) or cgroup
// (systemd), and stopping or restarting the service would end every session in
// it. The units therefore ask the manager to leave other processes alone when
// the service stops (AbandonProcessGroup, KillMode=process).

const (
	svcLabel       = "com.futurelastic.muster"
	svcUnitName    = "muster.service"
	svcDefaultAddr = "127.0.0.1:9470"
	svcPrincipal   = "supervisor"
)

func usageService() string {
	return strings.Join([]string{
		"usage: muster service install [--addr HOST:PORT] [--machine ID] [--tmux PATH] [--no-start]",
		"       muster service status",
		"       muster service uninstall [--purge]",
		"",
		"Sets up this binary as a per-user service for the current user: launchd (Background",
		"domain) on macOS, `systemd --user` on Linux. Idempotent: a second run changes nothing and",
		"says so; a run after an upgrade updates the installed binary and restarts the service.",
		"",
		"  install    copy the binary to ~/.local/bin, create the state directory and a principal",
		"             table (never overwriting one), write the unit, run `doctor` under the unit's",
		"             environment, load the unit and check /v1/health",
		"  --addr     loopback listen address (default " + svcDefaultAddr + "); a non-loopback address is refused",
		"  --machine  this machine's id (default: the short hostname)",
		"  --tmux     the multiplexer binary (default: the one found on PATH)",
		"  --no-start write the files and run doctor; do not load or start the unit",
		"  status     unit present, loaded, healthy, and which binary it runs; exits 1 unless all hold",
		"  uninstall  unload and remove the unit; configuration, tokens, state and the binary stay",
		"  --purge    with uninstall: also delete them (the tokens go too)",
		"",
		"Peers, inbox delivery and Windows are not covered: docs/install.md continues there.",
	}, "\n")
}

// svcEnv is everything install, status and uninstall touch outside their own
// arguments, so a test can run them against a temporary home and a fake
// service manager.
type svcEnv struct {
	GOOS     string
	Home     string
	UID      int
	Exe      string // the running binary: what gets installed
	Hostname string
	LookPath func(string) (string, error)
	// Run executes a command and returns its combined output. A non-nil env is
	// the command's whole environment; nil inherits this process's.
	Run  func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
	HTTP *http.Client
	Out  io.Writer
	Err  io.Writer
	// How long install waits for /v1/health, and how often it asks.
	HealthWait time.Duration
	HealthPoll time.Duration
}

func defaultSvcEnv(out, errw io.Writer) (svcEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return svcEnv{}, errors.New("cannot find the home directory of the current user")
	}
	exe, err := os.Executable()
	if err != nil {
		return svcEnv{}, fmt.Errorf("cannot find the running binary: %w", err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	uid := os.Getuid()
	if uid < 0 {
		if u, err := user.Current(); err == nil {
			uid, _ = strconv.Atoi(u.Uid)
		}
	}
	host, _ := os.Hostname()
	return svcEnv{
		GOOS: hostOS(), Home: home, UID: uid, Exe: exe, Hostname: host,
		LookPath: exec.LookPath,
		Run: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Env = env
			return cmd.CombinedOutput()
		},
		HTTP:       &http.Client{Timeout: 3 * time.Second},
		Out:        out,
		Err:        errw,
		HealthWait: 30 * time.Second,
		HealthPoll: 500 * time.Millisecond,
	}, nil
}

// runService handles `muster service ...` and reports whether it consumed the
// invocation, and the exit code when it did.
func runService(args []string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(args) == 0 || args[0] != "service" {
		return false, 0
	}
	env, err := defaultSvcEnv(stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "muster service: %v\n", err)
		return true, 1
	}
	return true, runServiceWith(context.Background(), env, args[1:])
}

func runServiceWith(ctx context.Context, env svcEnv, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Err, usageService())
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(env.Out, usageService())
		return 0
	case "install", "status", "uninstall":
	default:
		fmt.Fprintf(env.Err, "muster service: unknown command %q\n%s\n", args[0], usageService())
		return 2
	}
	fs := flag.NewFlagSet("service "+args[0], flag.ContinueOnError)
	fs.SetOutput(env.Err)
	fs.Usage = func() { fmt.Fprintln(env.Err, usageService()) }
	var opts svcInstallOpts
	var purge bool
	switch args[0] {
	case "install":
		fs.StringVar(&opts.Addr, "addr", "", "")
		fs.StringVar(&opts.Machine, "machine", "", "")
		fs.StringVar(&opts.Tmux, "tmux", "", "")
		fs.BoolVar(&opts.NoStart, "no-start", false, "")
	case "uninstall":
		fs.BoolVar(&purge, "purge", false, "")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(env.Err, "muster service %s: unexpected argument %q\n", args[0], fs.Arg(0))
		return 2
	}
	var err error
	switch args[0] {
	case "install":
		err = svcInstall(ctx, env, opts)
	case "status":
		err = svcStatus(ctx, env)
	case "uninstall":
		err = svcUninstall(ctx, env, purge)
	}
	if err != nil {
		fmt.Fprintf(env.Err, "muster service %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

type svcInstallOpts struct {
	Addr, Machine, Tmux string
	NoStart             bool
}

// svcPaths is where everything lives for one user.
type svcPaths struct {
	Bin       string
	ConfigDir string
	Config    string
	Token     string
	StateDir  string
	Log       string // launchd only: the unit's stdout and stderr
	Unit      string
}

func svcLayout(goos, home string) (svcPaths, error) {
	p := svcPaths{
		Bin:       filepath.Join(home, ".local", "bin", "muster"),
		ConfigDir: filepath.Join(home, ".config", "muster"),
		StateDir:  filepath.Join(home, ".local", "state", "muster"),
	}
	p.Config = filepath.Join(p.ConfigDir, "config.json")
	p.Token = filepath.Join(p.ConfigDir, svcPrincipal+".token")
	switch goos {
	case "darwin":
		p.Unit = filepath.Join(home, "Library", "LaunchAgents", svcLabel+".plist")
		p.Log = filepath.Join(p.StateDir, "muster.log")
	case "linux":
		p.Unit = filepath.Join(home, ".config", "systemd", "user", svcUnitName)
	default:
		return p, fmt.Errorf("no per-user service manager is supported on %s: launchd (macOS) and systemd --user (Linux) are; "+
			"docs/install.md, \"Windows\", has the scheduled-task procedure", goos)
	}
	return p, nil
}

// svcSettings are the three values an operator may choose; the rest of the
// unit's environment follows from the layout.
type svcSettings struct {
	Machine string
	Addr    string
	Tmux    string
}

// svcVars is the unit's whole environment, in a fixed order.
func svcVars(p svcPaths, s svcSettings) [][2]string {
	return [][2]string{
		{"FLEET_MACHINE", s.Machine},
		{"FLEET_ADDR", s.Addr},
		{"FLEET_STATE_DIR", p.StateDir},
		{"FLEET_CONFIG", p.Config},
		{"FLEET_TMUX_BIN", s.Tmux},
	}
}

func svcEnviron(p svcPaths, s svcSettings) []string {
	var out []string
	for _, kv := range svcVars(p, s) {
		out = append(out, kv[0]+"="+kv[1])
	}
	return out
}

// shortHost turns a hostname into a machine id: the first label, lowercase,
// letters digits and hyphens. A peer names this machine by it, so it should
// not be something a shell or a URL has to escape.
func shortHost(h string) string {
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case unicode.IsLower(r) && r < 128, unicode.IsDigit(r) && r < 128, r == '-':
			b.WriteRune(r)
		case r == '_' || r == ' ':
			b.WriteByte('-')
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "local"
}

// checkLoopbackAddr refuses anything but a loopback host with a fixed port:
// a service that reads paths and starts processes is not put on a network by
// a one-command install, and a peer is reached through the manual procedure.
func checkLoopbackAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr %q is not HOST:PORT: %v", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("--addr %q: a fixed port from 1 to 65535 is needed (an ephemeral port has no stable health URL)", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--addr %q is not a loopback address: this command installs a service reachable only from this machine; "+
			"a service on a network interface is the manual procedure (docs/install.md, steps 3 and 6)", addr)
	}
	return nil
}

func (e svcEnv) findTmux() (string, error) {
	cands := []string{}
	if p, err := e.LookPath("tmux"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux", "/bin/tmux")
	for _, c := range cands {
		if a, err := filepath.Abs(c); err == nil && isExecutableFile(a) {
			return a, nil
		}
	}
	return "", errors.New("no tmux found: install it (tmux 3.7 or newer is preferred) or pass --tmux PATH")
}

// ---- the units ---------------------------------------------------------

func renderSystemdUnit(p svcPaths, s svcSettings) string {
	var b strings.Builder
	b.WriteString("# Written by `muster service install`; the next run rewrites it.\n")
	b.WriteString("[Unit]\nDescription=muster: the session service\nAfter=network.target\n\n")
	b.WriteString("[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + systemdQuote(p.Bin) + " serve\n")
	for _, kv := range svcVars(p, s) {
		b.WriteString(`Environment="` + systemdEscape(kv[0]+"="+kv[1]) + `"` + "\n")
	}
	// Stopping or restarting the service must not end the sessions: the
	// multiplexer server it started lives in this cgroup.
	b.WriteString("KillMode=process\nRestart=on-failure\nRestartSec=5\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return b.String()
}

func systemdEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "%", "%%")
}

func systemdQuote(s string) string { return `"` + systemdEscape(s) + `"` }

func renderLaunchdPlist(p svcPaths, s svcSettings) string {
	x := xmlEscape
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- Written by `muster service install`; the next run rewrites it. -->\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + svcLabel + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>" + x(p.Bin) + "</string>\n\t\t<string>serve</string>\n\t</array>\n")
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	for _, kv := range svcVars(p, s) {
		b.WriteString("\t\t<key>" + kv[0] + "</key>\n\t\t<string>" + x(kv[1]) + "</string>\n")
	}
	b.WriteString("\t</dict>\n")
	// The Background domain: a service started from the desktop context can
	// move an agent runtime's login into a keychain a Background session
	// cannot read (docs/install.md, step 8).
	b.WriteString("\t<key>LimitLoadToSessionType</key>\n\t<string>Background</string>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<true/>\n")
	// Stopping the service must not end the sessions in the server it started.
	b.WriteString("\t<key>AbandonProcessGroup</key>\n\t<true/>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + x(p.Log) + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + x(p.Log) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

var (
	reSystemdEnv = regexp.MustCompile(`(?m)^Environment="((?:[^"\\]|\\.)*)"$`)
	reSystemdExe = regexp.MustCompile(`(?m)^ExecStart="((?:[^"\\]|\\.)*)"`)
	rePlistVar   = regexp.MustCompile(`<key>(FLEET_[A-Z_]+)</key>\s*<string>([^<]*)</string>`)
	rePlistExe   = regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>\s*<string>([^<]*)</string>`)
)

func systemdUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		} else if s[i] == '%' && i+1 < len(s) && s[i+1] == '%' {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseUnit reads back the settings and the binary path from a unit this
// command wrote, so a rerun without flags keeps what an earlier run chose.
func parseUnit(goos, text string) (s svcSettings, bin string) {
	vars := map[string]string{}
	switch goos {
	case "darwin":
		for _, m := range rePlistVar.FindAllStringSubmatch(text, -1) {
			vars[m[1]] = html.UnescapeString(m[2])
		}
		if m := rePlistExe.FindStringSubmatch(text); m != nil {
			bin = html.UnescapeString(m[1])
		}
	default:
		for _, m := range reSystemdEnv.FindAllStringSubmatch(text, -1) {
			if k, v, ok := strings.Cut(systemdUnescape(m[1]), "="); ok {
				vars[k] = v
			}
		}
		if m := reSystemdExe.FindStringSubmatch(text); m != nil {
			bin = systemdUnescape(m[1])
		}
	}
	return svcSettings{Machine: vars["FLEET_MACHINE"], Addr: vars["FLEET_ADDR"], Tmux: vars["FLEET_TMUX_BIN"]}, bin
}

// ---- service-manager calls ---------------------------------------------

type svcState struct {
	Loaded bool // the manager knows the unit
	Active bool // and it is running
}

func (e svcEnv) domain() string { return "user/" + strconv.Itoa(e.UID) }

func (e svcEnv) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return e.Run(ctx, nil, name, args...)
}

func (e svcEnv) state(ctx context.Context) svcState {
	switch e.GOOS {
	case "darwin":
		out, err := e.run(ctx, "launchctl", "print", e.domain()+"/"+svcLabel)
		return svcState{Loaded: err == nil, Active: err == nil && strings.Contains(string(out), "state = running")}
	default:
		_, enabledErr := e.run(ctx, "systemctl", "--user", "is-enabled", "--quiet", svcUnitName)
		_, activeErr := e.run(ctx, "systemctl", "--user", "is-active", "--quiet", svcUnitName)
		return svcState{Loaded: enabledErr == nil, Active: activeErr == nil}
	}
}

func (e svcEnv) managerErr(what string, out []byte, err error) error {
	msg := strings.TrimSpace(string(out))
	hint := ""
	if e.GOOS == "linux" && strings.Contains(msg, "Failed to connect to") {
		hint = "\n  `systemctl --user` needs a user session: log in on this machine, or run `loginctl enable-linger` and retry"
	}
	if msg != "" {
		return fmt.Errorf("%s: %v: %s%s", what, err, msg, hint)
	}
	return fmt.Errorf("%s: %v%s", what, err, hint)
}

// load registers the unit with the manager and starts it.
func (e svcEnv) load(ctx context.Context, p svcPaths) error {
	switch e.GOOS {
	case "darwin":
		if out, err := e.run(ctx, "launchctl", "bootstrap", e.domain(), p.Unit); err != nil {
			return e.managerErr("launchctl bootstrap", out, err)
		}
	default:
		if out, err := e.run(ctx, "systemctl", "--user", "enable", "--now", svcUnitName); err != nil {
			return e.managerErr("systemctl --user enable --now", out, err)
		}
	}
	return nil
}

// unload stops the unit and forgets it. Failure of an unloaded unit to unload
// is not an error: the aim is "not loaded".
func (e svcEnv) unload(ctx context.Context) {
	switch e.GOOS {
	case "darwin":
		_, _ = e.run(ctx, "launchctl", "bootout", e.domain()+"/"+svcLabel)
		// bootout returns before the job is gone, and a bootstrap straight
		// after it fails with an I/O error.
		deadline := time.Now().Add(10 * time.Second)
		for e.state(ctx).Loaded && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
	default:
		_, _ = e.run(ctx, "systemctl", "--user", "disable", "--now", svcUnitName)
	}
}

// start starts a unit the manager already knows and that is not running.
func (e svcEnv) start(ctx context.Context) error {
	switch e.GOOS {
	case "darwin":
		if out, err := e.run(ctx, "launchctl", "kickstart", "-k", e.domain()+"/"+svcLabel); err != nil {
			return e.managerErr("launchctl kickstart", out, err)
		}
	default:
		if out, err := e.run(ctx, "systemctl", "--user", "start", svcUnitName); err != nil {
			return e.managerErr("systemctl --user start", out, err)
		}
	}
	return nil
}

func (e svcEnv) restart(ctx context.Context, p svcPaths) error {
	switch e.GOOS {
	case "darwin":
		e.unload(ctx)
		return e.load(ctx, p)
	default:
		if out, err := e.run(ctx, "systemctl", "--user", "restart", svcUnitName); err != nil {
			return e.managerErr("systemctl --user restart", out, err)
		}
		return nil
	}
}

// ---- files -------------------------------------------------------------

func fileHash(path string) ([32]byte, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(b), true
}

func sameFile(a, b string) bool {
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

// installBinary puts src at dst unless it is already identical, replacing the
// file atomically so a service starting at that moment never reads half of it.
func installBinary(src, dst string) (changed bool, err error) {
	if sameFile(src, dst) {
		return false, nil
	}
	want, ok := fileHash(src)
	if !ok {
		return false, fmt.Errorf("cannot read the running binary %s", src)
	}
	if have, ok := fileHash(dst); ok && have == want {
		return false, nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(dst, data, 0o755, 0o755); err != nil {
		return false, err
	}
	return true, nil
}

func writeFileAtomic(path string, data []byte, mode, dirMode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func writeIfChanged(path, content string) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}
	return true, writeFileAtomic(path, []byte(content), 0o644, 0o755)
}

func ensureDir(path string) (created bool, err error) {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return false, nil
	}
	return true, privfs.MkdirAll(path)
}

// ---- install -----------------------------------------------------------

type svcReport struct {
	w       io.Writer
	changes int
}

func (r *svcReport) line(what string, changed bool, detail string) {
	state := "unchanged"
	if changed {
		state = "changed"
		r.changes++
	}
	fmt.Fprintf(r.w, "  %-9s %-10s %s\n", what, state, detail)
}

func svcInstall(ctx context.Context, e svcEnv, o svcInstallOpts) error {
	p, err := svcLayout(e.GOOS, e.Home)
	if err != nil {
		return err
	}
	// Settings: a flag wins, then what the unit already says, then a default.
	var cur svcSettings
	if raw, err := os.ReadFile(p.Unit); err == nil {
		cur, _ = parseUnit(e.GOOS, string(raw))
	}
	s := svcSettings{Machine: firstNonEmpty(o.Machine, cur.Machine), Addr: firstNonEmpty(o.Addr, cur.Addr, svcDefaultAddr), Tmux: firstNonEmpty(o.Tmux, cur.Tmux)}
	if s.Machine == "" {
		s.Machine = shortHost(e.Hostname)
	}
	if err := checkLoopbackAddr(s.Addr); err != nil {
		return err
	}
	if s.Tmux == "" {
		if s.Tmux, err = e.findTmux(); err != nil {
			return err
		}
	} else if !filepath.IsAbs(s.Tmux) || !isExecutableFile(s.Tmux) {
		return fmt.Errorf("--tmux %q is not an absolute path to an executable file", s.Tmux)
	}

	rep := &svcReport{w: e.Out}
	fmt.Fprintf(e.Out, "muster service install (%s, machine %q, %s)\n", e.GOOS, s.Machine, s.Addr)

	binChanged, err := installBinary(e.Exe, p.Bin)
	if err != nil {
		return fmt.Errorf("installing the binary: %w", err)
	}
	rep.line("binary", binChanged, p.Bin)

	stateNew, err := ensureDir(p.StateDir)
	if err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	rep.line("state", stateNew, p.StateDir)

	cfgDirNew, err := ensureDir(p.ConfigDir)
	if err != nil {
		return fmt.Errorf("config directory: %w", err)
	}
	cfgNew := false
	if _, err := os.Stat(p.Config); errors.Is(err, os.ErrNotExist) {
		cfgNew = true
		if err := privfs.WriteFile(p.Config, []byte("{}\n")); err != nil {
			return fmt.Errorf("writing %s: %w", p.Config, err)
		}
		grants := make([]string, 0, 8)
		for _, g := range supervisorGrants() {
			grants = append(grants, string(g))
		}
		if _, _, err := enrolPrincipal(p.Config, svcPrincipal, strings.Join(grants, ","), p.Token); err != nil {
			return fmt.Errorf("enrolling %s: %w", svcPrincipal, err)
		}
	} else if err != nil {
		return fmt.Errorf("reading %s: %w", p.Config, err)
	}
	detail := p.Config
	if cfgNew {
		detail += " (new: principal " + svcPrincipal + ", token in " + filepath.Base(p.Token) + ")"
	}
	rep.line("config", cfgNew || cfgDirNew, detail)

	var content string
	if e.GOOS == "darwin" {
		content = renderLaunchdPlist(p, s)
	} else {
		content = renderSystemdUnit(p, s)
	}
	unitChanged, err := writeIfChanged(p.Unit, content)
	if err != nil {
		return fmt.Errorf("writing the unit: %w", err)
	}
	rep.line("unit", unitChanged, p.Unit)

	// doctor, as the unit will run: its environment and nothing else.
	args := []string{"doctor", "--offline"}
	if hasPrincipal(p) {
		args = append(args, "--principal="+svcPrincipal)
	}
	out, derr := e.Run(ctx, svcEnviron(p, s), p.Bin, args...)
	fmt.Fprintf(e.Out, "\ndoctor, under the unit's environment:\n%s\n", indent(string(out), "  "))
	if derr != nil {
		return fmt.Errorf("doctor reported a failure, so the service was not started (the unit and files stay; fix the failing row and run this again)")
	}

	if o.NoStart {
		fmt.Fprintf(e.Out, "--no-start: the unit is written and not loaded.\n")
		return nil
	}

	// What is loaded is what the manager read when it last started the unit,
	// so a changed unit or binary means a restart; an unchanged one that is
	// registered but stopped means a start.
	if e.GOOS == "linux" && unitChanged {
		if out, err := e.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return e.managerErr("systemctl --user daemon-reload", out, err)
		}
	}
	switch st := e.state(ctx); {
	case !st.Loaded:
		if err := e.load(ctx, p); err != nil {
			return err
		}
		rep.line("service", true, "started")
	case unitChanged || binChanged:
		if err := e.restart(ctx, p); err != nil {
			return err
		}
		rep.line("service", true, "restarted")
	case !st.Active:
		if err := e.start(ctx); err != nil {
			return err
		}
		rep.line("service", true, "started")
	default:
		rep.line("service", false, "running")
	}

	token := readToken(p)
	h, herr := e.waitHealthy(ctx, s.Addr, token)
	if herr != nil {
		return fmt.Errorf("the service did not become healthy: %v%s", herr, e.logHint(p))
	}
	fmt.Fprintf(e.Out, "  health    ok         http://%s/v1/health%s\n", s.Addr, h)

	if rep.changes == 0 {
		fmt.Fprintf(e.Out, "\nNothing to change: muster is already installed, running and healthy.\n")
	} else {
		fmt.Fprintf(e.Out, "\nDone (%d change(s)).\n", rep.changes)
	}
	e.printNext(p, s)
	return nil
}

func (e svcEnv) printNext(p svcPaths, s svcSettings) {
	fmt.Fprintf(e.Out, "\nA client authenticates with the token in %s.\n", p.Token)
	if e.GOOS == "linux" {
		fmt.Fprintf(e.Out, "To keep the service running while nobody is logged in, and start it at boot: loginctl enable-linger\n")
	}
	fmt.Fprintf(e.Out, "Peers, inbox delivery and tmux 3.7+ are not set up here: docs/install.md steps 6, 7 and 8 continue from this point.\n")
}

func (e svcEnv) logHint(p svcPaths) string {
	if e.GOOS == "darwin" {
		return "\n  the service log: " + p.Log
	}
	return "\n  the service log: journalctl --user -u " + svcUnitName
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func indent(s, pre string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return pre + "(no output)"
	}
	return pre + strings.ReplaceAll(s, "\n", "\n"+pre)
}

// hasPrincipal reports whether the table names the supervisor and its token
// file is readable, i.e. whether doctor and the health probe can use it.
func hasPrincipal(p svcPaths) bool {
	c, err := loadConfig(p.Config)
	if err != nil {
		return false
	}
	for _, pr := range c.Principals {
		if pr.Name == svcPrincipal {
			_, err := os.Stat(p.Token)
			return err == nil
		}
	}
	return false
}

func readToken(p svcPaths) string {
	b, err := os.ReadFile(p.Token)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// probeHealth asks /v1/health once. A 200 is healthy; anything else is an
// error that says what was seen. The detail is the build version, when the
// answer carries one.
func (e svcEnv) probeHealth(ctx context.Context, addr, token string) (detail string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/health", nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if token == "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return "", fmt.Errorf("the service answers but needs a token, and no %s token file was found", svcPrincipal)
		}
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var h struct {
		Build struct {
			Version  *string `json:"version"`
			Revision string  `json:"revision"`
		} `json:"build"`
	}
	if json.Unmarshal(body, &h) == nil {
		switch {
		case h.Build.Version != nil:
			return " (build " + *h.Build.Version + ")", nil
		case h.Build.Revision != "":
			return " (build " + shortRev(h.Build.Revision) + ")", nil
		}
	}
	return "", nil
}

func shortRev(r string) string {
	if len(r) > 12 {
		return r[:12]
	}
	return r
}

func (e svcEnv) waitHealthy(ctx context.Context, addr, token string) (string, error) {
	deadline := time.Now().Add(e.HealthWait)
	var last error
	for {
		d, err := e.probeHealth(ctx, addr, token)
		if err == nil {
			return d, nil
		}
		last = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", last
		}
		time.Sleep(e.HealthPoll)
	}
}

// ---- status ------------------------------------------------------------

func svcStatus(ctx context.Context, e svcEnv) error {
	p, err := svcLayout(e.GOOS, e.Home)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(p.Unit)
	if err != nil {
		fmt.Fprintf(e.Out, "unit      missing    %s\nmuster is not installed as a service here: muster service install\n", p.Unit)
		return errors.New("not installed")
	}
	s, bin := parseUnit(e.GOOS, string(raw))
	if bin == "" {
		bin = p.Bin
	}
	ok := true
	row := func(what string, good bool, detail string) {
		state := "ok"
		if !good {
			state, ok = "NOT OK", false
		}
		fmt.Fprintf(e.Out, "%-9s %-8s %s\n", what, state, detail)
	}
	row("unit", true, p.Unit)
	ver := "cannot run it"
	if out, err := e.Run(ctx, nil, bin, "--version"); err == nil {
		ver = strings.TrimSpace(string(out))
	}
	_, statErr := os.Stat(bin)
	row("binary", statErr == nil, bin+"  ("+ver+")")
	st := e.state(ctx)
	row("loaded", st.Loaded, e.GOOS)
	row("running", st.Active, "")
	d, herr := e.probeHealth(ctx, s.Addr, readToken(p))
	if herr != nil {
		row("health", false, "http://"+s.Addr+"/v1/health: "+herr.Error())
	} else {
		row("health", true, "http://"+s.Addr+"/v1/health"+d)
	}
	if !ok {
		return errors.New("one or more checks did not pass")
	}
	return nil
}

// ---- uninstall ---------------------------------------------------------

func svcUninstall(ctx context.Context, e svcEnv, purge bool) error {
	p, err := svcLayout(e.GOOS, e.Home)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(p.Unit)
	present := statErr == nil
	if present || e.state(ctx).Loaded {
		e.unload(ctx)
	}
	if present {
		if err := os.Remove(p.Unit); err != nil {
			return err
		}
		if e.GOOS == "linux" {
			_, _ = e.run(ctx, "systemctl", "--user", "daemon-reload")
		}
		fmt.Fprintf(e.Out, "unit      removed    %s\n", p.Unit)
	} else {
		fmt.Fprintf(e.Out, "unit      absent     %s\n", p.Unit)
	}
	if !purge {
		fmt.Fprintf(e.Out, "kept      %s (configuration and tokens), %s (state), %s\n          --purge deletes them\n", p.ConfigDir, p.StateDir, p.Bin)
		return nil
	}
	for _, path := range []string{p.ConfigDir, p.StateDir, p.Bin} {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "purged    %s\n", path)
	}
	return nil
}
