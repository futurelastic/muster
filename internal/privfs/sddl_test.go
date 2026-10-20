package privfs

import (
	"reflect"
	"testing"
)

const user = "S-1-5-21-111-222-333-1001"

func TestParseSDDL_TheOwnerOnlyProtectedList(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		wantProt bool
	}{
		{"file, as built", "D:P(A;;FA;;;" + user + ")", true},
		{"directory, as built", "D:P(A;OICI;FA;;;" + user + ")", true},
		{"as the system renders it back", "D:PAI(A;;FA;;;" + user + ")", true},
		{"hex rights", "D:P(A;;0x1f01ff;;;" + user + ")", true},
		{"with an owner and group section", "O:" + user + "G:S-1-5-21-111-222-333-513D:P(A;;FA;;;" + user + ")", true},
		{"unprotected but still only the user", "D:AI(A;ID;FA;;;" + user + ")", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := parseSDDL(tc.in)
			if err != nil {
				t.Fatalf("parseSDDL: %v", err)
			}
			if d.Protected != tc.wantProt {
				t.Errorf("Protected = %v, want %v", d.Protected, tc.wantProt)
			}
			if v, why := judge(d, user); v != OwnerOnly {
				t.Errorf("verdict = %s (%s), want owner-only", v, why)
			}
		})
	}
}

func TestJudge_AnyoneElseMakesItLoose(t *testing.T) {
	for name, in := range map[string]string{
		"the default temp-directory list": "D:PAI(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;" + user + ")",
		"users group":                     "D:P(A;;FA;;;" + user + ")(A;;FR;;;BU)",
		"everyone":                        "D:P(A;;FA;;;WD)",
		"an alias that is the user":       "D:P(A;;FA;;;" + user + ")(A;;FA;;;AU)",
		"a deny entry":                    "D:P(D;;FA;;;BU)(A;;FA;;;" + user + ")",
		"a different user":                "D:P(A;;FA;;;S-1-5-21-111-222-333-1002)",
		"no access list":                  "D:NO_ACCESS_CONTROL",
		"an empty list":                   "D:P",
	} {
		t.Run(name, func(t *testing.T) {
			d, err := parseSDDL(in)
			if err != nil {
				t.Fatalf("parseSDDL: %v", err)
			}
			v, why := judge(d, user)
			if v != Loose {
				t.Errorf("verdict = %s, want loose", v)
			}
			if why == "" {
				t.Error("a loose verdict carries no reason")
			}
		})
	}
}

func TestJudge_ASystemAccountIsRecognisedThroughItsAlias(t *testing.T) {
	d, err := parseSDDL("D:P(A;;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	if v, why := judge(d, "S-1-5-18"); v != OwnerOnly {
		t.Errorf("a service running as the system account: verdict = %s (%s), want owner-only", v, why)
	}
}

func TestParseSDDL_Fields(t *testing.T) {
	d, err := parseSDDL("D:PAI(A;OICI;FA;;;" + user + ")(D;;GW;;;BG)S:(AU;SA;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ACEs) != 2 {
		t.Fatalf("parsed %d entries, want 2 (the system ACL section is not part of the DACL)", len(d.ACEs))
	}
	got := d.ACEs[0]
	want := ace{Type: "A", Flags: map[string]bool{"OI": true, "CI": true}, Rights: 0x1F01FF, Trustee: user}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("first entry = %+v, want %+v", got, want)
	}
	if d.ACEs[1].Type != "D" || d.ACEs[1].Rights != 0x40000000 || d.ACEs[1].Trustee != "S-1-5-32-546" {
		t.Errorf("second entry = %+v", d.ACEs[1])
	}
}

func TestParseSDDL_RefusesWhatItCannotRead(t *testing.T) {
	for name, in := range map[string]string{
		"empty":                "",
		"no DACL section":      "O:SYG:SY",
		"unterminated entry":   "D:P(A;;FA;;;SY",
		"too few fields":       "D:P(A;;FA)",
		"unknown rights token": "D:P(A;;ZZ;;;SY)",
		"stray text":           "D:P(A;;FA;;;SY)junk",
		"no trustee":           "D:P(A;;FA;;;)",
		"bad hex":              "D:P(A;;0xZZ;;;SY)",
		"unrecognised flags":   "D:QQ(A;;FA;;;SY)",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSDDL(in); err == nil {
				t.Errorf("parseSDDL(%q) = nil error, want a refusal", in)
			}
		})
	}
}

func TestJudge_ADomainRelativeAliasIsTheUserOnlyWhenTheUserIsThatAccount(t *testing.T) {
	d, err := parseSDDL("D:P(A;;FA;;;LA)")
	if err != nil {
		t.Fatal(err)
	}
	if v, why := judge(d, "S-1-5-21-1-2-3-500"); v != OwnerOnly {
		t.Errorf("the built-in administrator's own entry: verdict = %s (%s), want owner-only", v, why)
	}
	if v, _ := judge(d, user); v != Loose {
		t.Errorf("an entry for the built-in administrator is not private to another user: verdict = %s, want loose", v)
	}
	if v, _ := judge(d, "S-1-5-32-500"); v != Loose {
		t.Errorf("a non-domain SID must not match the alias: verdict = %s, want loose", v)
	}
}

func TestWritableBy(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"only the user", "D:P(A;;FA;;;" + user + ")", nil},
		{"the machine's own accounts", "D:PAI(A;ID;FA;;;SY)(A;ID;FA;;;BA)(A;ID;FA;;;" + user + ")", nil},
		{"trusted installer", "D:P(A;;FA;;;S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464)(A;;FA;;;" + user + ")", nil},
		{"the built-in administrator account", "D:P(A;;FA;;;LA)(A;;FA;;;" + user + ")", nil},
		{"users may only read and run", "D:P(A;;FA;;;" + user + ")(A;;FRFX;;;BU)", nil},
		{"read-only by right mask", "D:P(A;;0x120089;;;WD)", nil},
		{"users may write", "D:P(A;;FA;;;" + user + ")(A;;FW;;;BU)", []string{"Users"}},
		{"everyone, full control", "D:P(A;;FA;;;WD)", []string{"Everyone"}},
		{"authenticated users may modify", "D:PAI(A;ID;0x1301bf;;;AU)", []string{"Authenticated Users"}},
		{"generic write", "D:P(A;;GW;;;BU)", []string{"Users"}},
		{"append only", "D:P(A;;0x100004;;;BU)", []string{"Users"}},
		{"delete only", "D:P(A;;SD;;;BU)", []string{"Users"}},
		{"change the list only", "D:P(A;;WD;;;BU)", []string{"Users"}},
		{"take ownership only", "D:P(A;;WO;;;BU)", []string{"Users"}},
		{"another account", "D:P(A;;FW;;;S-1-5-21-111-222-333-1002)", []string{"the account S-1-5-21-111-222-333-1002"}},
		{"two classes, sorted and once each", "D:P(A;;FW;;;BU)(A;;FW;;;AU)(A;;GW;;;BU)", []string{"Authenticated Users", "Users"}},
		{"inherit-only entries are not about this file", "D:P(A;OICIIO;FA;;;BU)(A;;FA;;;" + user + ")", nil},
		{"an earlier deny of the same trustee takes the write away", "D:P(D;;FW;;;BU)(A;;FW;;;BU)", nil},
		{"a deny of only some of the rights leaves the rest", "D:P(D;;WD;;;BU)(A;;FW;;;BU)", []string{"Users"}},
		{"a deny after the allow does not undo it", "D:P(A;;FW;;;BU)(D;;FW;;;BU)", []string{"Users"}},
		{"a deny for another trustee does not undo it", "D:P(D;;FW;;;AU)(A;;FW;;;BU)", []string{"Users"}},
		{"no access list", "D:NO_ACCESS_CONTROL", []string{"Everyone"}},
		{"an empty list", "D:P", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := parseSDDL(tc.in)
			if err != nil {
				t.Fatalf("parseSDDL: %v", err)
			}
			got := writableBy(d, user)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("writableBy(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWritableBy_TheCurrentUserIsNeverOthers(t *testing.T) {
	d, err := parseSDDL("D:P(A;;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	if got := writableBy(d, "S-1-5-18"); got != nil {
		t.Errorf("a service running as the system account: %q, want none", got)
	}
	d, _ = parseSDDL("D:P(A;;FA;;;LA)")
	if got := writableBy(d, "S-1-5-21-1-2-3-500"); got != nil {
		t.Errorf("the built-in administrator's own entry: %q, want none", got)
	}
}
