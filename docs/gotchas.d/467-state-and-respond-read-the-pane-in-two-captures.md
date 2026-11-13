# The state and `respond` read one prompt, but from two captures

**What bit:** a client drew two buttons from a state that read `waiting_input`
with a prompt of kind `external-imports`; the person chose the affirmative
option, and `respond` refused it as "not waiting on a prompt". The dialog was an
unnumbered menu whose question wrapped over three rows and whose imported file's
path was long, non-ASCII and held spaces and an ampersand. A state read taken
after the refusal still showed the prompt.

**What was measured:** the state and `respond` reach the parser through one
function now (`readPrompt`), and before this they reached the same parser
(`parsePromptShape`) over a capture built the same way, so a screen that holds
still cannot be read two ways. A screen of that shape, with the path wrapped over
two and three rows, painted in the history margin or clipped off the top at every
row, at pane heights from 12 to 40 and with scrollback copies of the dialog above
it, classifies and responds identically every time. Nothing was captured in the
field, so the report's own screen is not in hand and the cause of the one refusal
is not established. What is established is that the two reads can only differ by
the screen moving between two captures: the dialog answered or dropped, or a
capture landing on the runtime partway through repainting it.

**The rule it produced:** `respond` does not turn a screen it cannot read a
prompt on into "nothing is being asked" when a state read said otherwise. The
driver remembers the last prompt a state read reported on a session (two
minutes), `respond` reads the screen a few more times before refusing, and the
refusal names the disagreement. The refusal itself stays: with no prompt on the
screen a key would land in whatever is there. A corpus-wide test
(`respond_state_agreement_test.go`) replays every corpus case through both
readings, so a new route from a screen to a prompt that skips the shared one fails
there by name.

**If it happens again:** the refusal now carries the kind and the age of the
prompt the state had reported. Capture the pane at that moment (see
`testdata/corpus/README.md`) and add it as a corpus case; this one is a
reconstruction (`external-imports-wrapped-long-path`).
