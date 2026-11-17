package tmux

import (
	"strings"
	"testing"
)

// The listing a multiplexer answers carries the credential as its eleventh
// field; one from before #490 carries nine or ten, and a driver that reuses
// captures gets six more after them.
func TestParseRowsReadsTheCredentialAndAcceptsListingsWithoutIt(t *testing.T) {
	sep := "§"
	nine := strings.Join([]string{"s", "%1", "/w", "1", "1785600000", "0", "0", "t", "1"}, sep)
	ten := nine + sep + "1"
	eleven := ten + sep + "c-abc"
	reuse := sep + strings.Join([]string{"1785600001", "80", "24", "0", "0", "3"}, sep)
	for _, c := range []struct {
		name, in, want string
		reuse          bool
	}{
		{"nine fields", nine, "", false},
		{"ten fields", ten, "", false},
		{"eleven fields", eleven, "c-abc", false},
		{"ten fields and the fingerprint", ten + reuse, "", true},
		{"eleven fields and the fingerprint", eleven + reuse, "c-abc", true},
		{"eleven fields, option unset", ten + sep + "" + reuse, "", true},
	} {
		rows, err := parseRows(c.in+"\n", sep)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: %+v err=%v", c.name, rows, err)
		}
		if rows[0].credential != c.want {
			t.Errorf("%s: credential = %q, want %q", c.name, rows[0].credential, c.want)
		}
		if got := rowCredential(rows[0]); (c.want == "" && got != credentialDefault) || (c.want != "" && got != c.want) {
			t.Errorf("%s: rowCredential = %q", c.name, got)
		}
		if c.reuse && rows[0].activity != 1785600001 {
			t.Errorf("%s: the fingerprint was not read: %+v", c.name, rows[0])
		}
		if !rows[0].managed || rows[0].launchRC != map[bool]string{false: "", true: "1"}[len(strings.Split(c.in, sep)) >= 10] {
			t.Errorf("%s: an earlier field moved: %+v", c.name, rows[0])
		}
	}
	if _, err := parseRows(nine+sep+"1"+sep+"c"+sep+"extra\n", sep); err == nil {
		t.Error("a listing with an unexpected number of fields was accepted")
	}
}

// muster #490: a credential's name tells sessions on the same one from sessions
// on another, and is never the credential.

func TestCredentialIdentityNamesTheCredentialAndNeverContainsIt(t *testing.T) {
	token := "sk-ant-" + "oat01-THIS-IS-A-SECRET-VALUE-0123456789" // built at run time: no secret scanner has a token to find
	id := credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token})
	if id == credentialDefault {
		t.Fatalf("a session handed a token was named %q, the ambient credential", id)
	}
	if strings.Contains(id, token) || strings.Contains(id, "SECRET") {
		t.Fatalf("the name %q contains the credential", id)
	}
	if len(id) > 20 {
		t.Errorf("the name %q is long enough to be mistaken for a value", id)
	}
	if again := credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token}); again != id {
		t.Errorf("the same token was named %q and then %q", id, again)
	}
}

func TestCredentialIdentityTellsCredentialsApart(t *testing.T) {
	a := credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token-a"})
	b := credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token-b"})
	key := credentialIdentity(map[string]string{"ANTHROPIC_API_KEY": "token-a"})
	cfg := credentialIdentity(map[string]string{"CLAUDE_CONFIG_DIR": "/somewhere/else"})
	both := credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token-a", "ANTHROPIC_API_KEY": "token-a"})

	seen := map[string]string{credentialDefault: "default"}
	for name, id := range map[string]string{"token a": a, "token b": b, "same value as an api key": key, "a config dir": cfg, "two variables": both} {
		if other, dup := seen[id]; dup {
			t.Errorf("%s and %s are both named %q", name, other, id)
		}
		seen[id] = name
	}
}

func TestCredentialIdentityIgnoresWhatIsNotACredential(t *testing.T) {
	ambient := credentialIdentity(nil)
	if ambient != credentialDefault {
		t.Fatalf("no env named %q, want %q", ambient, credentialDefault)
	}
	// An unrelated variable, and a credential variable that is empty, hand the
	// session nothing of its own.
	for name, env := range map[string]map[string]string{
		"an unrelated variable": {"PATH": "/usr/bin", "SESSION_NAME": "x"},
		"an empty token":        {"CLAUDE_CODE_OAUTH_TOKEN": ""},
	} {
		if got := credentialIdentity(env); got != credentialDefault {
			t.Errorf("%s: named %q, want %q", name, got, credentialDefault)
		}
	}
	// What else is in the environment does not move the name.
	a := credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "t"})
	b := credentialIdentity(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "t", "PATH": "/usr/bin", "FOO": "bar"})
	if a != b {
		t.Errorf("an unrelated variable changed the name: %q vs %q", a, b)
	}
}
