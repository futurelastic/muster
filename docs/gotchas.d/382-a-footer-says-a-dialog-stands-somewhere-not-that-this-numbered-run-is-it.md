# A footer says a dialog stands somewhere, not that this numbered run is it

`parseMenuShape` reads a numbered run from anywhere in the 24-row scan window.
The runtime's footers (`Enter to select`, `Enter to confirm`, `Tab to amend`)
used to license whatever run it had accumulated: with a footer on screen,
`footer && len(options) >= 1` accepted it, highlighted or not. A footer is
chrome a person cannot type, so that was a sound corroboration for a run that
is the dialog, and a wrong one for a run that only happens to be in the window.

## What broke

#380 measured one instance: a dialog whose options carry no numbers, drawn
under a transcript that holds a numbered list. The list was returned as the
dialog's options, and an answer by index went to a question nobody asked. #380
fixed it by trying the unnumbered reader on the block directly above the
footer first, which only helps a dialog that reader accepts (one marker row,
labels aligned to one column, no blank row between options).

#382 is the rest: a footer-bearing dialog of a shape that reader refuses (a
wrapped label that does not line up, options separated by blank rows, a title
row), drawn under the same kind of list. Nothing about the footer changed, so
the list was still the prompt.

## The rule

With a footer on screen, a numbered run is believed only if it stands directly
above that footer: between its last option and the footer there may be blank
rows, rules, dashed dividers and the options' own descriptions (an unnumbered
row indented to the label, including a wrapped label), and nothing else.
Any other row there belongs to something that is not the run, so the run is
transcript.

What a refused run reads as is **no prompt**, the same thing `unnumberedMenu`
names for a layout it refuses. It is not an empty-option prompt: the dialog
replaces the composer, so the screen has a footer and no composer and reads as
`unknown` (held, nothing is sent to it), never `idle` and never an answerable
list.

## What it does not cover, and what it costs

- **A genuine numbered dialog with a row of its own chrome between its options
  and its footer** now reads as no prompt too. That is visible and recoverable
  (the session shows `unknown`); a wrong answer by index is neither. Measured
  against every corpus screen and every numbered fixture before the change:
  none has such a row. If a runtime release adds one, the fix is to name that
  row as chrome in the scan loop, not to relax the rule.
- **A preview pane** is left alone, as in #358: its rows are read as a list
  column beside the pane, so the rows around it cannot be told apart this way.
- **A footer above a run** no longer licenses a run that has no highlight (the
  footer is not above anything it could belong to), but it never took a
  structurally recognised menu away, and still does not.
- `promptFooterPresent` still corroborates an accepted structural prompt on any
  footer in the window, wherever it stands. It is the second place a footer is
  taken as evidence about a run, and it is not narrowed here.

Pinned by the corpus cases `numbered-transcript-over-wrapped-label-dialog` and
`numbered-transcript-over-blank-row-dialog`, and by
`TestFooterDoesNotLicenseANumberedRunThatIsNotTheBlockAboveIt` and
`TestFooterStillLicensesTheRunDirectlyAboveItWhateverChromeBetween` in
`classify_test.go`.
