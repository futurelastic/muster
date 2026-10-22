package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/peerkey"
	"github.com/futurelastic/muster/internal/privfs"
)

// `muster key` — this machine's signing key (muster #350).
//
// A machine that authenticates to a peer by signing holds one Ed25519 key pair.
// The private half lives in the state directory, owner-only, and is never
// printed or logged; the public half is what an operator hands to each peer so
// it can list this machine as a principal.
//
//	muster key init            create the key if there is none (never replaces one)
//	muster key show            print the public key
//	muster key rotate          replace it, telling every key peer first
//
// The service installer runs `init` for you.

const (
	machineKeyFile = "machine.key"
	nextKeySuffix  = ".next"
)

func usageKey() string {
	return strings.Join([]string{
		"usage: muster key init   [--state-dir=DIR]",
		"       muster key show   [--state-dir=DIR]",
		"       muster key rotate [--state-dir=DIR] [--config=PATH] [--machine=ID]",
		"",
		"init creates this machine's signing key if none exists and prints its public half.",
		"show prints the public key: give it to a peer with `muster principal add <this-machine> --public-key=...`.",
		"rotate makes a new key, has every peer that lists this machine accept it (signed with the old one,",
		"which the peer keeps accepting for ten minutes), then switches. Safe to re-run if a peer was down.",
		"",
		"The state directory is --state-dir, else FLEET_STATE_DIR, else the service installer's default.",
	}, "\n")
}

// keyStateDir resolves where the key lives.
func keyStateDir(flag string, getenv func(string) string) (string, error) {
	switch {
	case flag != "":
		return flag, nil
	case getenv("FLEET_STATE_DIR") != "":
		return getenv("FLEET_STATE_DIR"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no state directory: set FLEET_STATE_DIR or pass --state-dir=DIR")
	}
	return filepath.Join(home, ".local", "state", "muster"), nil
}

// ensureMachineKey creates the key at path if absent. It never replaces one:
// an install run twice must not lock every peer out.
func ensureMachineKey(path string) (pub ed25519.PublicKey, created bool, err error) {
	if priv, err := peerkey.LoadPrivate(path); err == nil {
		return priv.Public().(ed25519.PublicKey), false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	pub, priv, err := peerkey.Generate()
	if err != nil {
		return nil, false, err
	}
	if err := peerkey.SavePrivate(path, priv); err != nil {
		return nil, false, err
	}
	return pub, true, nil
}

// openMachineSigner opens this machine's key for the service. A service
// configured to sign that has no key fails at start, naming the fix: refusing
// each request later would read as an unreachable peer.
func openMachineSigner(stateDir string, self fleet.MachineId) (*peerkey.Signer, error) {
	if stateDir == "" {
		return nil, errors.New("FLEET_STATE_DIR is not set, and the signing key lives there")
	}
	path := filepath.Join(stateDir, machineKeyFile)
	s, err := peerkey.NewSigner(path, string(self))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no signing key at %s: run `muster key init` (or `muster service install`) and give the public key to the peer", path)
		}
		return nil, err
	}
	return s, nil
}

// runKey handles `muster key ...` and reports whether it consumed the invocation.
func runKey(args []string, getenv func(string) string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(args) == 0 || args[0] != "key" {
		return false, 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, usageKey())
		return true, 2
	}
	var stateFlag, cfgFlag, machineFlag string
	for _, a := range args[2:] {
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(stdout, usageKey())
			return true, 0
		case strings.HasPrefix(a, "--state-dir="):
			stateFlag = strings.TrimPrefix(a, "--state-dir=")
		case strings.HasPrefix(a, "--config="):
			cfgFlag = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "--machine="):
			machineFlag = strings.TrimPrefix(a, "--machine=")
		default:
			fmt.Fprintf(stderr, "muster key: unknown argument %q\n%s\n", a, usageKey())
			return true, 2
		}
	}
	dir, err := keyStateDir(stateFlag, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "muster key:", err)
		return true, 2
	}
	path := filepath.Join(dir, machineKeyFile)

	switch args[1] {
	case "init":
		pub, created, err := ensureMachineKey(path)
		if err != nil {
			fmt.Fprintln(stderr, "muster key:", err)
			return true, 1
		}
		if created {
			fmt.Fprintf(stderr, "created %s (owner-only); fingerprint %s\n", path, peerkey.Fingerprint(pub))
		} else {
			fmt.Fprintf(stderr, "%s already exists; fingerprint %s\n", path, peerkey.Fingerprint(pub))
		}
		fmt.Fprintln(stdout, peerkey.FormatPublicKey(pub))
		return true, 0
	case "show":
		priv, err := peerkey.LoadPrivate(path)
		if err != nil {
			fmt.Fprintf(stderr, "muster key: no key at %s (%v); run `muster key init`\n", path, err)
			return true, 1
		}
		pub := priv.Public().(ed25519.PublicKey)
		fmt.Fprintf(stderr, "fingerprint %s\n", peerkey.Fingerprint(pub))
		fmt.Fprintln(stdout, peerkey.FormatPublicKey(pub))
		return true, 0
	case "rotate":
		cfgPath := cfgFlag
		if cfgPath == "" {
			cfgPath = getenv("FLEET_CONFIG")
		}
		machine := machineFlag
		if machine == "" {
			machine = getenv("FLEET_MACHINE")
		}
		if err := rotateMachineKey(context.Background(), path, cfgPath, machine, getenv, &http.Client{Timeout: 15 * time.Second}, stdout); err != nil {
			fmt.Fprintln(stderr, "muster key rotate:", err)
			return true, 1
		}
		return true, 0
	}
	fmt.Fprintln(stderr, usageKey())
	return true, 2
}

// rotateMachineKey is `muster key rotate`.
//
// The order is the whole design. The new key is written beside the old one and
// NOT yet used; every key peer is told, with a request signed by the OLD key
// (the only proof that the asker is the machine being rotated); only when every
// peer has acknowledged is the new key moved into place. A peer that has taken
// the new key keeps accepting the old one for a grace period, so the moments
// between its acknowledgement and the switch — and a running service that has
// not yet noticed the new file — are not an outage. If any peer cannot be
// reached the new key stays pending and a re-run offers the same one, so the
// peers that already have it are told again, harmlessly, rather than being left
// ahead of the rest.
func rotateMachineKey(ctx context.Context, keyPath, cfgPath, machine string, getenv func(string) string, client *http.Client, out io.Writer) error {
	if cfgPath == "" {
		return errors.New("no config: set FLEET_CONFIG or pass --config=PATH")
	}
	if machine == "" {
		return errors.New("this machine's id is needed: set FLEET_MACHINE or pass --machine=ID")
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	entries, err := resolvePeers(getenv("FLEET_PEERS"), cfg)
	if err != nil {
		return err
	}
	var targets []peerEntry
	for _, e := range entries {
		if cfg.peerAuthFor(e.machine) == peerAuthKey {
			targets = append(targets, e)
		}
	}
	if len(targets) == 0 {
		return errors.New("no peer is configured with \"auth\": \"key\", so no one holds this machine's key to be told about a new one")
	}
	signer, err := peerkey.NewSigner(keyPath, machine)
	if err != nil {
		return fmt.Errorf("the current key: %w (run `muster key init`)", err)
	}

	nextPath := keyPath + nextKeySuffix
	next, err := peerkey.LoadPrivate(nextPath)
	switch {
	case err == nil:
		fmt.Fprintf(out, "continuing a rotation already begun (new key %s)\n", peerkey.Fingerprint(next.Public().(ed25519.PublicKey)))
	case errors.Is(err, os.ErrNotExist):
		_, next, err = peerkey.Generate()
		if err == nil {
			err = peerkey.SavePrivate(nextPath, next)
		}
		if err != nil {
			return err
		}
	default:
		return err
	}
	nextPub := next.Public().(ed25519.PublicKey)
	payload, _ := json.Marshal(map[string]string{"publicKey": peerkey.FormatPublicKey(nextPub)})

	failed := 0
	for _, t := range targets {
		var last error
		ok := false
		for _, base := range t.urls {
			if last = postKeyRotation(ctx, client, signer, string(t.machine), strings.TrimRight(base, "/"), payload); last == nil {
				ok = true
				break
			}
		}
		if ok {
			fmt.Fprintf(out, "  %-20s accepted the new key\n", t.machine)
			continue
		}
		failed++
		fmt.Fprintf(out, "  %-20s NOT updated: %v\n", t.machine, last)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d peers did not take the new key; this machine still signs with the old one. "+
			"Re-run `muster key rotate` (the same new key is offered) within ten minutes of the first success, "+
			"or the peers that did take it stop accepting the old key", failed, len(targets))
	}
	if err := os.Rename(nextPath, keyPath); err != nil {
		return fmt.Errorf("every peer has the new key but moving it into place failed: %w "+
			"(the new key is at %s; move it to %s by hand)", err, nextPath, keyPath)
	}
	fmt.Fprintf(out, "rotated: now signing with key %s; peers accept the old key for ten minutes\n", peerkey.Fingerprint(nextPub))
	fmt.Fprintf(out, "a running service picks the new key up on its next request; nothing to restart\n")
	return nil
}

func postKeyRotation(ctx context.Context, client *http.Client, signer *peerkey.Signer, target, base string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/peer-key", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := signer.Sign(req, payload, target); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var env fleet.ErrorEnvelope
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&env) == nil && env.Error.Message != "" {
		return fmt.Errorf("%s: %s", resp.Status, env.Error.Message)
	}
	return errors.New(resp.Status)
}

// keyCredential is the doctor's view of "this machine signs its requests to
// that peer": whether the key is there and who can read it. It returns the
// credential a probe will present, and the row.
func keyCredential(getenv func(string) string, self, peer fleet.MachineId) (peerCred, doctorRow) {
	row := doctorRow{ID: "peer." + string(peer) + ".credential", Refs: []int{350}}
	stateDir := getenv("FLEET_STATE_DIR")
	if stateDir == "" {
		row.Status = statusFail
		row.Summary = "authenticates by key, but FLEET_STATE_DIR is unset and the signing key lives there — the service refuses to start"
		return peerCred{}, row
	}
	path := filepath.Join(stateDir, machineKeyFile)
	signer, err := peerkey.NewSigner(path, string(self))
	if err != nil {
		row.Status = statusFail
		row.Summary = "authenticates by key, but there is no usable signing key at " + machineKeyFile + " in FLEET_STATE_DIR — the service refuses to start"
		row.Detail = "`muster key init` creates it; then give the public key (`muster key show`) to " + string(peer) +
			" with `muster principal add " + string(self) + " --public-key=...`"
		return peerCred{}, row
	}
	pub, _ := signer.Public()
	row.Status = statusPass
	row.Summary = "authenticates by signing each request (key " + peerkey.Fingerprint(pub) + "); no reusable secret crosses the wire"
	if rep := privfs.Check(path); rep.Verdict == privfs.Loose {
		row.Status = statusWarn
		row.Summary += " — but the key file is readable by others"
		row.Detail = rep.Detail
	}
	return peerCred{signer: signer, peer: peer}, row
}
