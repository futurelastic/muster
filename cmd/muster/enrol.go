package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/privfs"
	"github.com/futurelastic/muster/internal/service"
)

// Enrolment: giving a client a principal without hand-editing JSON.
//
// # Why this exists
//
// The service is emphatic that there is no unauthenticated mode — not on
// loopback, not in development — and §6's per-verb grants only mean anything
// when a caller has an identity of its own. That is right, and it left the
// enrolment path as "ask the operator to add you to the principal table",
// which is not a path at all.
//
// What that produced in practice, on a two-machine fleet in one evening:
//
//   - every principal created by hand-editing JSON, four times;
//   - a token minted with an ad-hoc one-liner each time;
//   - a grant typed that the config loader did not know, which made the
//     service refuse to start — fail-closed, correct, and two minutes of
//     downtime for a config an operator had every reason to think was valid;
//   - and an asymmetry nobody noticed until it was looked for: one machine had
//     a read-only principal for a client, the other did not, so the nearest
//     token on that host carried create, send, close and rename.
//
// That last one is the whole argument. **When enrolment is manual, the path of
// least resistance is reusing a credential that already exists** — and the
// credential lying around is rarely the one with the right grants. A client
// with no route to a principal does not stay unauthorized; it routes around
// the service, and the fleet quietly returns to one shared secret.
//
// # What this deliberately is not
//
// Not a registration API. A client cannot enrol itself: in a statically
// configured fleet (§7.2) the operator is the authority, and an endpoint that
// mints credentials on request would need an approval flow that nobody wants
// to build or operate. This is the operator's tool, made safe and quick.
//
// It validates grants BEFORE writing, using the same list the service loads
// with, so the failure that took the service down cannot be typed here at all.

func usageEnrol() string {
	return strings.Join([]string{
		"usage: muster principal add <name> --grants=read[,send,...] [--config PATH] [--token-file PATH]",
		"       muster principal add <name> --grants=... --public-key=ed25519:... [--config PATH]   (a peer that signs; no token)",
		"       muster principal list [--config PATH]",
		"       muster principal grant|revoke <name> <grant>... [--config PATH]   (muster principal grant -h)",
		"",
		"grants: " + strings.Join(grantNames(), " · "),
		"",
		"A running service picks the new principal up within seconds, with no restart and no dropped connections (SIGHUP applies it now).",
	}, "\n")
}

func grantNames() []string {
	all := service.Grants()
	out := make([]string, 0, len(all))
	for _, g := range all {
		out = append(out, string(g))
	}
	sort.Strings(out)
	return out
}

// runPrincipal handles `muster principal ...` and reports whether it
// consumed the invocation.
func runPrincipal(args []string) (handled bool, err error) {
	if len(args) == 0 || args[0] != "principal" {
		return false, nil
	}
	if len(args) < 2 {
		return true, errors.New(usageEnrol())
	}

	switch args[1] {
	case "grant", "revoke":
		for _, a := range args[2:] {
			if a == "-h" || a == "--help" {
				fmt.Println(usageGrants())
				return true, nil
			}
		}
		return true, runGrantEdit(args[1] == "revoke", args[2:], os.Stdout)
	}

	cfgPath := os.Getenv("FLEET_CONFIG")
	tokenFile := ""
	publicKey := ""
	var positional []string
	grants := ""
	for _, a := range args[2:] {
		switch {
		case strings.HasPrefix(a, "--public-key="):
			publicKey = strings.TrimPrefix(a, "--public-key=")
		case strings.HasPrefix(a, "--grants="):
			grants = strings.TrimPrefix(a, "--grants=")
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "--token-file="):
			tokenFile = strings.TrimPrefix(a, "--token-file=")
		default:
			positional = append(positional, a)
		}
	}
	if cfgPath == "" {
		return true, errors.New("no config: set FLEET_CONFIG or pass --config=PATH")
	}

	switch args[1] {
	case "list":
		return true, listPrincipals(cfgPath)
	case "add":
		if len(positional) != 1 {
			return true, errors.New(usageEnrol())
		}
		if publicKey != "" {
			if tokenFile != "" {
				return true, errors.New("--public-key and --token-file are exclusive: a principal that signs has no token")
			}
			return true, addKeyPrincipal(cfgPath, positional[0], grants, publicKey)
		}
		return true, addPrincipal(cfgPath, positional[0], grants, tokenFile)
	default:
		return true, errors.New(usageEnrol())
	}
}

func listPrincipals(cfgPath string) error {
	c, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	for _, p := range c.Principals {
		// Tokens are never printed. An operator asking "who can do what" is
		// not asking to have every secret on the machine echoed into a
		// terminal, a screen share, or a scrollback buffer.
		kind := "token"
		if p.PublicKey != "" {
			if k, err := peerkey.ParsePublicKey(p.PublicKey); err == nil {
				kind = "key " + peerkey.Fingerprint(k)
			} else {
				kind = "key"
			}
		}
		fmt.Printf("%-20s %-24s %s\n", p.Name, kind, strings.Join(p.Grants, ","))
	}
	return nil
}

func addPrincipal(cfgPath, name, grantList, tokenFile string) error {
	grants, tokenFile, err := enrolPrincipal(cfgPath, name, grantList, tokenFile)
	if err != nil {
		return err
	}
	fmt.Printf("added principal %q with grants %s\n", name, strings.Join(grants, ","))
	fmt.Printf("token written to %s (%s)\n", tokenFile, privfs.Label)
	fmt.Printf("a running service applies this within seconds, without a restart; `muster doctor` (row config.reload) says whether it did\n")
	return nil
}

// enrolPrincipal is addPrincipal without the report: it validates, mints,
// writes the token file and then the table, and returns what it wrote. The
// service installer enrols its supervisor through it and says its own thing.
func enrolPrincipal(cfgPath, name, grantList, tokenFile string) (grants []string, tokenPath string, err error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", errors.New("a principal needs a name")
	}
	if strings.TrimSpace(grantList) == "" {
		// Silence is not "read-only". §6's default is denied, and a principal
		// created with no grants can do nothing — which is more likely a
		// forgotten flag than an intent, so say so rather than create it.
		return nil, "", errors.New("--grants is required (a principal with none can do nothing); " +
			"available: " + strings.Join(grantNames(), ", "))
	}

	for _, g := range strings.Split(grantList, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		// Validated HERE, against the same list the service loads with. The
		// alternative is what happened once already: an unknown grant written
		// to disk, discovered only when the service refused to start.
		if !service.ValidGrant(service.Grant(g)) {
			return nil, "", fmt.Errorf("unknown grant %q; available: %s", g, strings.Join(grantNames(), ", "))
		}
		grants = append(grants, g)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, "", fmt.Errorf("reading %s: %w", cfgPath, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("%s is not valid JSON, refusing to rewrite it: %w", cfgPath, err)
	}
	list, _ := doc["principals"].([]any)
	for _, entry := range list {
		if m, ok := entry.(map[string]any); ok && m["name"] == name {
			return nil, "", fmt.Errorf("principal %q already exists; use `muster principal grant|revoke` to change its grants", name)
		}
	}

	token, err := mintToken()
	if err != nil {
		return nil, "", err
	}
	doc["principals"] = append(list, map[string]any{
		"name": name, "token": token, "grants": grants,
	})

	if tokenFile == "" {
		tokenFile = filepath.Join(filepath.Dir(cfgPath), name+".token")
	}
	// The token file first: a config naming a principal whose token nobody has
	// is worse than a token file with no principal, because the first looks
	// finished.
	if err := privfs.WriteFile(tokenFile, []byte(token+"\n")); err != nil {
		return nil, "", fmt.Errorf("writing %s: %w", tokenFile, err)
	}
	if err := writeJSONAtomic(cfgPath, doc); err != nil {
		return nil, "", fmt.Errorf("writing %s: %w", cfgPath, err)
	}

	return grants, tokenFile, nil
}

// addKeyPrincipal enrols a principal that authenticates by signing (muster
// #350): the operator is handed the other machine's PUBLIC key, there is no
// secret to mint or file to write, and nothing to protect on this side.
func addKeyPrincipal(cfgPath, name, grantList, publicKey string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("a principal needs a name")
	}
	if strings.TrimSpace(grantList) == "" {
		return errors.New("--grants is required (a principal with none can do nothing); " +
			"available: " + strings.Join(grantNames(), ", "))
	}
	key, err := peerkey.ParsePublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("--public-key: %w", err)
	}
	var grants []string
	for _, g := range strings.Split(grantList, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if !service.ValidGrant(service.Grant(g)) {
			return fmt.Errorf("unknown grant %q; available: %s", g, strings.Join(grantNames(), ", "))
		}
		grants = append(grants, g)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", cfgPath, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s is not valid JSON, refusing to rewrite it: %w", cfgPath, err)
	}
	list, _ := doc["principals"].([]any)
	for _, entry := range list {
		if m, ok := entry.(map[string]any); ok && m["name"] == name {
			return fmt.Errorf("principal %q already exists; use `muster principal grant|revoke` to change its grants", name)
		}
	}
	doc["principals"] = append(list, map[string]any{
		"name": name, "publicKey": peerkey.FormatPublicKey(key), "grants": grants,
	})
	if err := writeJSONAtomic(cfgPath, doc); err != nil {
		return fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	fmt.Printf("added principal %q (key %s) with grants %s\n", name, peerkey.Fingerprint(key), strings.Join(grants, ","))
	fmt.Printf("a running service applies this within seconds, without a restart; `muster doctor` (row config.reload) says whether it did\n")
	return nil
}

// mintToken produces a bearer credential.
//
// crypto/rand, because a token minted from a predictable source is not a
// secret, and "it is only a local fleet" is exactly the reasoning that puts a
// guessable credential on a service that can start processes.
func mintToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("minting a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeJSONAtomic replaces a config without a window in which it is truncated.
//
// Same discipline as the state store: a half-written principal table parses as
// authoritative for whatever it happens to contain, and the service reads it at
// startup — which is the least convenient moment to discover it.
func writeJSONAtomic(path string, doc any) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp, err := privfs.CreateTemp(filepath.Dir(path), ".config.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
