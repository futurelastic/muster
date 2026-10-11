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
