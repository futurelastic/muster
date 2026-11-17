package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// muster #490: which credential a session runs on.
//
// A usage limit belongs to the credential a request is made with, not to the
// machine. One machine can run sessions on several at once: the sign-in the
// service itself runs under, and any token a create hands a session through
// its env or the machine's declared sessionEnv. One of them hitting its limit
// says nothing about the others, so the block this driver remembers has to be
// keyed by the credential, and this file is where a session's credential gets
// its name.
//
// # What the name is
//
// An opaque string, good for one thing: telling whether two sessions run on the
// same credential. It is derived at create time, from the environment the
// session is actually started with, and never from anything this service reads
// back later, because after the start nothing reliable says what a running
// process was handed.
//
// It is a digest of the credential-bearing variables, never the values, and it
// is the only form in which a credential reaches state, an event or a log. A
// 48-bit prefix of SHA-256 over a token is no use to anyone holding it and is
// plenty to tell the handful of credentials one machine runs from one another.
// The digest is not salted on purpose: the same token on two machines of a fleet
// names the same credential, which is what a supervisor reading two machines'
// blocks wants to be able to see.
//
// # What it cannot tell apart
//
// A session handed none of those variables runs on whatever the runtime finds
// ambient: the sign-in on disk, or a variable in the service's own environment.
// Every such session is one credential here, named credentialDefault. Two
// different ambient credentials on one machine therefore read as one; that is
// the old behaviour, narrowed to the sessions that cannot be told apart, and not
// something this service has a way to look through.

// credentialDefault is the credential of a session that was handed none of its
// own: the machine's ambient one.
const credentialDefault = "default"

// credentialVars are the variables through which the runtime is handed the
// credential a session runs on, in the order they are digested. CONFIG_DIR is
// in the list although it holds a path, not a secret: pointing the runtime at
// another configuration directory is pointing it at another sign-in.
//
// A session given several of them is one credential per distinct set, whatever
// the runtime prefers when more than one is present. That is deliberate: which
// of them wins is the runtime's business and has changed before, and a name that
// told two sessions apart wrongly would only ever under-share a block, where one
// that merged them wrongly would block a session whose credential has quota.
var credentialVars = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CONFIG_DIR",
}

// credentialDomain separates this digest from any other use of SHA-256 over the
// same bytes, and versions it: a change to the derivation is a new domain, so
// two builds can never silently disagree about what one name means.
const credentialDomain = "muster/credential/v1\n"

// credentialIdentity names the credential a session started with env runs on:
// credentialDefault when env carries none of credentialVars, otherwise a digest
// of the ones it does.
func credentialIdentity(env map[string]string) string {
	var b strings.Builder
	for _, name := range credentialVars {
		v := env[name]
		if v == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(credentialDomain)
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return credentialDefault
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "c-" + hex.EncodeToString(sum[:6])
}

// credentialOption is the tmux user option Create sets, on a session started with
// a credential of its own, to the name credentialIdentity gave it. It is a
// session option for the reasons managedSessionOption is one: it is the
// multiplexer's own record of the session, so it lives exactly as long as the
// session, survives a restart of this service, follows a rename, and is read in
// the same listing as everything else. A record this service kept beside it would
// have to be written and found again, and a listing that rewrites the record file
// while a create is writing to it can lose the entry for good.
//
// Never set for credentialDefault: an option nobody set reads back empty, and
// empty is the ambient credential.
const credentialOption = "@colab-credential"

// rowCredential is the credential the live run r was started with: the option
// Create recorded, or credentialDefault when there is none. Never a guess about
// another credential.
func rowCredential(r paneRow) string {
	if r.credential == "" {
		return credentialDefault
	}
	return r.credential
}
