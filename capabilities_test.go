package fleet

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestDriverCapabilities_Validate_RejectsZeroDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: 0}
	if err := c.Validate(); !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Validate() = %v, want ErrNoDeadline", err)
	}
}

func TestDriverCapabilities_Validate_RejectsNegativeDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: -1}
	if err := c.Validate(); !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Validate() = %v, want ErrNoDeadline", err)
	}
}

func TestDriverCapabilities_Validate_AcceptsPositiveDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: 3000}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// A driver that cannot toggle remote control declares nothing: the key is
// absent from the wire, so "is there a control to offer" is the field's
// presence and never a pair of falses a client has to interpret (muster #269).
func TestDriverCapabilities_RemoteControlIsOmittedWhenUndeclared(t *testing.T) {
	b, err := json.Marshal(DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "remoteControl") {
		t.Fatalf("an undeclared capability must not be serialised: %s", b)
	}
	// And the floor an unreached peer reports is the same value, so it omits it too.
	b, _ = json.Marshal(DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved}.Assumed())
	if strings.Contains(string(b), "remoteControl") {
		t.Fatalf("an assumed floor must not declare the capability: %s", b)
	}
}

func TestDriverCapabilities_RemoteControlRoundTrips(t *testing.T) {
	in := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved,
		RemoteControl: &RemoteControlSupport{Toggle: true, Off: false}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"remoteControl":{"toggle":true,"off":false}`) {
		t.Fatalf("wire shape: %s", b)
	}
	var out DriverCapabilities
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.RemoteControl == nil || !out.RemoteControl.Toggle || out.RemoteControl.Off {
		t.Fatalf("round trip lost it: %+v", out.RemoteControl)
	}
}

// The four capabilities a person-facing client reads (muster #379) are
// absent unless a driver declares them: an undeclared one is not on the wire,
// and neither is the floor an unreached peer reports. Absent means "not
// declared", which a client must never read as "cannot".
func TestDriverCapabilities_PersonFacingFieldsAbsentWhenUndeclared(t *testing.T) {
	declared := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved}
	for name, c := range map[string]DriverCapabilities{"observed": declared, "assumed floor": declared.Assumed()} {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"readsTurns", "reportsConversation", "respond", "permissionModeChange"} {
			if strings.Contains(string(b), key) {
				t.Errorf("%s: an undeclared %q must not be serialised: %s", name, key, b)
			}
		}
	}
}

// A declared "cannot" is on the wire, spelled out, and a declared "can" names
// what it can. Both decode back unchanged — the false is a statement, not an
// absence, so it must survive a round trip as present.
func TestDriverCapabilities_PersonFacingFieldsRoundTrip(t *testing.T) {
	cannot := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved,
		ReadsTurns: Declared(false), ReportsConversation: Declared(false),
		Respond:              &RespondSupport{},
		PermissionModeChange: &PermissionModeChangeSupport{},
	}
	can := DriverCapabilities{DeadlineMs: 1000, Source: CapabilitiesObserved,
		ReadsTurns: Declared(true), ReportsConversation: Declared(true),
		Respond:              &RespondSupport{Kinds: []PromptKind{PromptToolPermission}},
		PermissionModeChange: &PermissionModeChangeSupport{Live: true, Via: PermissionModeChangeViaKeys},
	}
	for name, tc := range map[string]struct {
		in   DriverCapabilities
		want []string
	}{
		"cannot": {cannot, []string{
			`"readsTurns":false`, `"reportsConversation":false`,
			`"respond":{"kinds":[],"unclassified":false}`, `"permissionModeChange":{"live":false}`,
		}},
		"can": {can, []string{
			`"readsTurns":true`, `"reportsConversation":true`,
			`"respond":{"kinds":["tool-permission"],"unclassified":false}`,
			`"permissionModeChange":{"live":true,"via":"keys"}`,
		}},
	} {
		b, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range tc.want {
			if !strings.Contains(string(b), w) {
				t.Errorf("%s: wire shape lacks %s: %s", name, w, b)
			}
		}
		var out DriverCapabilities
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.ReadsTurns == nil || *out.ReadsTurns != *tc.in.ReadsTurns ||
			out.ReportsConversation == nil || *out.ReportsConversation != *tc.in.ReportsConversation {
			t.Errorf("%s: a declared boolean did not survive: %+v", name, out)
		}
		if out.Respond == nil || out.Respond.Unclassified != tc.in.Respond.Unclassified ||
			len(out.Respond.Kinds) != len(tc.in.Respond.Kinds) {
			t.Errorf("%s: respond did not survive: %+v", name, out.Respond)
		}
		if out.PermissionModeChange == nil || *out.PermissionModeChange != *tc.in.PermissionModeChange {
			t.Errorf("%s: permissionModeChange did not survive: %+v", name, out.PermissionModeChange)
		}
	}
}

// A peer built before these fields sends none of them. It must read as not
// declared — all four nil, under the peer's own `observed` — and never as the
// false a plain boolean would have decoded to.
func TestDriverCapabilities_OlderPeerJSONReadsNotDeclared(t *testing.T) {
	var c DriverCapabilities
	older := `{"observesState":true,"deliversRawKeys":false,"supportsPin":{"model":true,"effort":false,"agent":true},` +
		`"deadlineMs":3000,"source":"observed"}`
	if err := json.Unmarshal([]byte(older), &c); err != nil {
		t.Fatal(err)
	}
	if c.Source != CapabilitiesObserved {
		t.Fatalf("source = %q", c.Source)
	}
	if c.ReadsTurns != nil || c.ReportsConversation != nil || c.Respond != nil || c.PermissionModeChange != nil {
		t.Fatalf("an older peer must read as not declared, got %+v", c)
	}
}

// A nil Kinds is still a declared "answers none": it is written as [], never
// null, so a client does not have to read a third state.
func TestRespondSupport_NilKindsMarshalAsEmptyList(t *testing.T) {
	b, err := json.Marshal(RespondSupport{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"kinds":[],"unclassified":false}` {
		t.Fatalf("got %s", b)
	}
	// A kind a newer peer lists that this build does not define passes through.
	var r RespondSupport
	if err := json.Unmarshal([]byte(`{"kinds":["something-newer"],"unclassified":true}`), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Kinds) != 1 || r.Kinds[0] != "something-newer" || !r.Unclassified {
		t.Fatalf("an unknown kind must decode unchanged: %+v", r)
	}
}

// PromptKinds lists every kind this package defines, in declaration order, and
// hands out a copy: a driver that answers every prompt it reports declares
// exactly this.
func TestPromptKindsListsEveryKind(t *testing.T) {
	want := []PromptKind{
		PromptResumeChooser, PromptFolderTrust, PromptExternalImports, PromptSettingsTrust,
		PromptToolPermission, PromptBypassAcceptance, PromptFeedbackReview,
	}
	got := PromptKinds()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kind %d = %q, want %q", i, got[i], want[i])
		}
	}
	got[0] = "tampered"
	if PromptKinds()[0] != PromptResumeChooser {
		t.Fatal("PromptKinds must return a fresh slice")
	}
}

// PromptKinds is the one list a driver that answers every prompt declares, so
// it must not lag the constants: a kind added to prompt.go and forgotten here
// would leave such a driver silently declaring less than it answers. The
// constants are read from the source, not repeated, or this would only compare
// the list with a second copy of itself.
func TestPromptKindsHoldsEveryDeclaredKindConstant(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "prompt.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "PromptKind" {
				continue
			}
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok {
					declared = append(declared, strings.Trim(lit.Value, `"`))
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no PromptKind constants in prompt.go: the check is reading the wrong thing")
	}
	listed := map[string]bool{}
	for _, k := range PromptKinds() {
		listed[string(k)] = true
	}
	for _, k := range declared {
		if !listed[k] {
			t.Errorf("prompt kind %q is declared in prompt.go but missing from PromptKinds()", k)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("PromptKinds() holds %d kinds, prompt.go declares %d", len(listed), len(declared))
	}
}
