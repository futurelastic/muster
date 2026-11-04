package tmux

import (
	"reflect"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #400: the Claude Code runtime has no catalogue to ask, so its models
// are declared — by the operator, else the aliases the CLI documents.

func TestModels_AliasesWhenNothingIsDeclared(t *testing.T) {
	d := New("m")
	got := d.Models()
	if got.Source != fleet.ModelsDeclared || !reflect.DeepEqual(got.IDs, []string{"opus", "sonnet", "haiku"}) {
		t.Errorf("Models() = %+v, want the documented aliases, declared", got)
	}
	if got.ReadAt != nil || got.Default != nil {
		t.Errorf("Models() = %+v: nothing was read and the default is account-specific, so neither is reported", got)
	}
}

func TestModels_TheOperatorsListWins(t *testing.T) {
	d := New("m", WithModels([]string{"claude-opus-4-1", "sonnet"}))
	got := d.Models()
	if got.Source != fleet.ModelsDeclared || !reflect.DeepEqual(got.IDs, []string{"claude-opus-4-1", "sonnet"}) {
		t.Errorf("Models() = %+v, want the operator's list, declared", got)
	}
	// The caller's slice is not the driver's.
	got.IDs[0] = "tampered"
	if again := d.Models(); again.IDs[0] != "claude-opus-4-1" {
		t.Errorf("a caller reached the driver's list: %v", again.IDs)
	}
}

// A driver whose command was replaced runs some other agent program, and the
// aliases of this one say nothing about it.
func TestModels_NoneForAReplacedCommand(t *testing.T) {
	d := New("m", WithCommandBuilder(func(fleet.SessionSpec, string) []string { return []string{"true"} }))
	if got := d.Models(); got.Source != fleet.ModelsNone || len(got.IDs) != 0 {
		t.Errorf("Models() = %+v, want none", got)
	}
}
