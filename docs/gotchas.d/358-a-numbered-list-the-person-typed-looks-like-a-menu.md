# A numbered-list message the person typed is echoed with the menu's own marker, and reads as a menu

**Issue:** #358

## The trap

The terminal interface echoes a submitted message in the scrollback with the
`❯` leader, and marks a selection menu's highlighted option with the same
glyph. A message written as a numbered list is therefore, row for row, a
numbered run with one row highlighted:

```
❯ 1. two years
  2. not yet
  3. wildcard
```

`parseMenuShape` accepts exactly that as a menu (contiguous numbers, one
highlighted), and recognises menus by structure rather than by wording on
purpose, so nothing in the text can tell the two apart. The result was a session
mid-turn reported as `waiting_input` / `prompt`. A client that trusted it showed
an answer card, and the keys that "answered" it went to a session that was
working: one of them interrupted the turn.

It lasted as long as the echo was the last numbered list on screen. A running
turn is the first case; an interrupted turn is the second, and nothing animates
there, so the unrecognised-prompt hold (a screen seen unchanged for
`spinnerPaintGrace` is promoted to a confident prompt) promoted it for good.

## The rule

A selection menu **replaces** the composer. So numbered rows with a live fenced
composer opening *below* the last of them are transcript, not a menu. Both the
running turn (status line, then the composer) and the interrupted one (notice,
then the composer) have one.

Two things override it, deliberately:

- **A dialog footer in the window** (`Enter to select`, `Enter to confirm`,
  `Tab to amend`). It is chrome a person cannot type, so a screen that shows one
  is believed whatever sits below it.
- **A preview pane**, because the pane's bottom border reads as a rule and the
  composer search is not trustworthy there (see `maskComposer`).

## What this does not cover

- **A composer the driver could not read in full** (`composerClipped`, including
  the pane's bottom edge cutting it off). The check compares the composer's
  opening row to the last option row, and a clipped composer has no position to
  compare. An echoed list above one still reads as a menu.
- **A cue that does not need the composer.** The report also named the question
  candidate ending in the previous turn's `✻ … done <time>` footer: a real menu
  has its question directly above its options, an echo has the footer there.
  Not built, because the corpus did not need it and the composer rule is the
  stronger signal; it is the next thing to reach for if a clipped shape turns up.

Pinned by the corpus cases `numbered-list-echo-over-running-turn` and
`numbered-list-echo-after-interrupt`, and by `echoed_numbered_list_test.go`.
