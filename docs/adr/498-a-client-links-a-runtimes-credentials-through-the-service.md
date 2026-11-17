# 498 — a client links and unlinks a runtime's credentials through the service

## Context

This service is the only component that starts the runtime, so it holds the binary
path, the key files wired through `sessionEnv`, the subscription sign-in store and the
provider policy (#497 made them visible). Seeing a credential was not enough: to *add*
one a client still had to run the runtime's own program, or this service's command line
on the machine. The owner ruled the direction: the service writes the runtime's
credentials; clients store only what people choose.

Two things made that more than a route:

- `sessionEnv` is config-file-only and changing an entry needs a restart. "Wire the
  entry" over HTTP is a design that did not exist.
- The device-code sign-in lived in the command line, which blocks for minutes on a
  person. An HTTP route cannot.

## Decision

Routes under `/v1/runtimes/{runtime}/credentials/{provider}/…` (`docs/spec/api-http.md`
§3.1.3): `PUT`/`DELETE …/api-key`, `POST …/subscription/login`, `GET …/login/{id}`,
`DELETE …/subscription`, behind a new grant **`credentials`**. The points a reader of the
code would not guess:

- **A linked key is a state-managed entry beside the config, not a config rewrite.**
  Rewriting a file the operator owns would lose their comments, race their editor and has
  no reload to land in. `sessionenv.Linked` keeps owner-only files under
  `<state>/linked/<runtime>/<VARIABLE>`; the opencode driver reads them at every create
  next to the declared entries (`effectiveSessionEnv`), through the same `Provision` and
  `CheckFile` path, so a rotation is the next session's value with nothing to reload and
  nothing is cached.
- **The operator's declaration always wins.** Linking a variable they declared for this
  runtime is `409 conflict`; one they declare after a key was linked shadows it (the view
  shows theirs, an unlink still removes the file). One variable never has two sources.
- **The variable for a provider is the #497 table inverted, never guessed.**
  `keyEnvForProvider` names the one variable a link writes (google has three accepted
  names; one is canonical). A provider outside the table is `400` naming the supported
  ones. A test pins the table to its inverse so the view and the write cannot disagree.
- **A provider the policy does not permit is refused at link time.** A credential makes a
  provider usable, never allowed. Accepting the key would put it where no session reads
  and the view hides, so the refusal is the honest answer. Unlink and sign-out are never
  refused for policy: cleaning up is always allowed.
- **The value is passed once.** It is read from a body capped at 8 KiB into a buffer that
  is written and zeroed, validated as one printable token (no space or line break), and
  appears in no response, error or log line: a malformed body is refused with a fixed
  sentence because a JSON decoder's own error can quote its input, and a driver error that
  is not a typed one is replaced by a fixed sentence. The proof test captures every
  response and the service log across the whole flow and searches for the value.
- **The sign-in is the CLI's device flow as `grant.Logins`:** one attempt pending at a
  time (a second start returns the waiting one, so a reload mints no second code), polled
  in the background for the CLI's 15 minutes under the service's own lifetime. Approval
  ends in `Store.SignIn`, which the CLI now uses too, so the two save a sign-in
  identically. An attempt is held in memory: a restart forgets it and its code lapses.
- **The grant is its own and implied by nothing**, the `push-list` / `debug` shape: not
  read, send or relay; not the legacy all-verbs flags (`credentialsGate` is not
  `mutating()`, and a machine with no principal table refuses every request); not the set
  `service install` enrols; and a signed machine list that names it is refused whole,
  because who may change which account this machine bills is a local decision. Reading an
  attempt needs it as well: the code signs the machine in to whichever account approves it.
- **Local only, never relayed.** The path names no machine. A peer's credentials are
  changed on that peer by a principal that holds the grant there (the issue's scope).
- **The audit line names verb, runtime, provider, kind and action**, and never a value,
  a code or an attempt id.

## Consequences

- A client can make a provider usable on a machine, see it `usable`, and start a session
  that uses it with no engine program on its own `PATH`, and undo it.
- A missing-grant answer is `unauthorized`, like every other grant (401 by the spec's
  table, §2); the issue's wording "403" is the same condition.
- The key crosses the wire in a request body, as every bearer token already does; the
  spec says to reach the service over loopback or a TLS-terminating front.

## Not done

- Relaying to a peer (a client asks that machine).
- The terminal runtime's own credentials.
- Cancelling a pending sign-in; persisting a pending one across a restart.
- A listing of linked variables the view cannot attribute to a provider (only the short
  table is linkable, for the reason #497 gives).
