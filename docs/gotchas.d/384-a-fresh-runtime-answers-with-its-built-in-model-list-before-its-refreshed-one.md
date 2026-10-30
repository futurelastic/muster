# A freshly started opencode runtime can answer with its built-in model list before the refreshed one, and the two share no id

**Issue:** #384

## What happened

The create-time model check (#287) read `GET /config/providers` once, from the
session's own just-started server, and refused a model that answer did not list.
Each isolated session starts with an empty cache directory, so which list that
answer carried depended on whether the runtime had already refreshed its model
list: before, the list built into the binary; after, the downloaded one. For one
provider the two lists disagreed and no single id was in both.

Measured on runtime 1.18.3, four creates in a row asking for one provider: the
same id was refused with suggestions from the built-in list, then a different id
was refused twice with suggestions from the refreshed list, then the first id was
refused again and accepted on a retry a minute later. Whatever an operator
configured was refused some of the time, and the refusal's own suggestions were
ids the next create refused.

## Measured since, on the same runtime

Four fresh servers asked repeatedly from the moment they answered: every first
answer already carried the refreshed list, after 2–14 s of start-up. The built-in
answer is therefore not the usual case here, which is why the race could not be
reproduced on demand; the fix is written against the contract (an answer can be
the earlier list), and the test serves the two answers in sequence.

## The rule going forward

- An id the first answer does not list is not refused on that answer. The list is
  read again every half second for up to eight seconds; the id is accepted if any
  answer lists it, built-in or refreshed.
- Only an id no answer listed is refused, and its suggestions come from every
  answer read, so each suggestion is one the check would accept.
- A refusal of an unlisted id therefore takes the whole window. A listed id never
  waits: the first answer that lists it ends the check.
- The wait ends early on the caller's own deadline, which is returned as the
  deadline and never as a verdict on the model.
- The sibling refusals (#293: nothing usable, nothing listed) are deliberately not
  re-read: they were not part of the measurement, and a missing credential is not
  something a refresh changes. If a provider that exists only in the refreshed
  list is ever refused as "no model provider is usable", it is the same race and
  the same re-read applies.
