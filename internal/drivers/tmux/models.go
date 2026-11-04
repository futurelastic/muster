package tmux

import (
	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// The models a create on this runtime may name (muster #400).
//
// The CLI this driver runs has no catalogue endpoint a service can ask, so the
// list is never "read from the runtime". It is either what the operator
// declared for this machine (WithModels) or the aliases the CLI documents, and
// either way the source is "declared": a value outside the list may still work,
// and a client keeps accepting one.

// claudeModelAliases are the model aliases the CLI documents for its model
// flag. They are names the CLI resolves itself, so they stay valid as the
// models behind them change.
var claudeModelAliases = []string{"opus", "sonnet", "haiku"}

// WithModels sets the model ids this machine's operator declares for a create
// on this runtime. Empty keeps the documented aliases. The composition root
// validates the ids (cmd/muster) before they get here.
func WithModels(ids []string) Option {
	return func(d *Driver) { d.declaredModels = append([]string(nil), ids...) }
}

// Models implements driver.ModelsReporter: the operator's declared list, else
// the CLI's documented aliases, else "none" for a driver whose command was
// replaced — it then runs some other agent program, and the aliases of this
// one say nothing about it. The default is always null: the CLI's own default
// depends on the account it is signed in with.
func (d *Driver) Models() fleet.RuntimeModels {
	ids := d.declaredModels
	if len(ids) == 0 {
		if !d.buildsClaudeArgv {
			return fleet.NoModels()
		}
		ids = claudeModelAliases
	}
	return fleet.RuntimeModels{
		Source: fleet.ModelsDeclared,
		IDs:    append([]string(nil), ids...),
	}.Normalized()
}

var _ driver.ModelsReporter = (*Driver)(nil)
