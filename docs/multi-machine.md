# Two machines, over a LAN or a VPN

Federating two instances: one machine owns sessions, the other calls it through
its own local service. This is the end-to-end walk-through; once both machines
read `ok`, [Driving a session on another machine](driving-a-machine.md) shows what
it buys you.

## Recommended: signed requests, no shared secret

A bearer token is sent on every request, usually over plain http. Anything that
sees it once can use it forever, and nothing makes it expire. A peer can
instead hold a **key pair**: each request to it is *signed*, valid for about a
minute, accepted once, and good only for that method, path, body and target
machine. Nothing reusable crosses the wire, so a captured request is useless to
replay, to redirect, or to alter.

Be exact about what this gives. A signature authenticates the **caller** to the
peer and makes capture useless. It does **not** encrypt: traffic on plain http is
still readable by anyone on the path, and it does not prove to the caller that
the answer came from the peer. Use a private network, as before.

Do **machine-b first**. Both machines need a state directory and a config file
with a principal table (`muster service install` makes all three; see
[`install.md`](install.md)).

**1. Each machine makes its key.** `muster service install` already did; otherwise:

```sh
muster key init       # prints the PUBLIC key; the private half stays in the state directory, owner-only
muster key show       # print it again later
```

**2. Each machine lists the other as a principal, by public key.** On machine-b,
with machine-a's public key; on machine-a, the reverse:

```sh
muster principal add machine-a --grants=read,create,send --public-key=ed25519:...
```

The principal's name is the other machine's id, exactly as it names itself
(`FLEET_MACHINE`). There is no token to mint, file to protect or copy to carry.

**3. On machine-a, say the peer is a key peer** (`config.json`):

```json
{ "peers": [ { "machine": "machine-b", "url": "http://10.8.0.12:9000", "auth": "key" } ] }
```

Restart both services. A peer entry with `"auth": "key"` has no `token`.

**4. Verify.** `muster doctor` has a `peer.machine-b.credential` row that says it
authenticates by key and shows the key's fingerprint; `GET /v1/machines` carries
`peer.auth: "key"` for it. Both machines must agree on the time to within a
minute — a signature outside that is refused, and the refusal names both clocks.

A machine may mix kinds while it migrates: some peers `"auth": "key"`, others
with a `token`, as the sections below describe. Moving a pair over is adding the
two principals and the `auth` line, then removing the old token.

### Rotating a key

```sh
muster key rotate
```

It makes a new key, tells every key peer (signing the request with the *old* key,
which is the proof it is you), and only when all of them have accepted switches to
the new one. A running service picks it up without a restart. The peer keeps
accepting the old key for ten minutes, so nothing is dropped in the gap, and
refuses to let that retired key rotate anything. If a peer is down the command
says so, leaves the old key in place and keeps the new one pending; run it again
and the same new key is offered. Do that within ten minutes of the first success.
A peer must have a state directory to accept a rotation: one without would forget
it at the next restart.

If a private key may have leaked, do not wait for the next rotation: rotate now,
and the retired key is useless ten minutes later.

## Token peers

The setup below is the older one and still works unchanged.

Four steps. Start from a service you can already run — see the
[Quickstart](../README.md#quickstart) for the single-machine form. Do **machine-b first** — machine-a needs an address to point at.

**1. Give each machine its own credential.** Not one shared token: the whole
point of the next three steps is that the two machines have distinct identities.

```sh
mkdir -p ~/.config/muster
openssl rand -hex 32 > ~/.config/muster/machine-a.token
chmod 600 ~/.config/muster/machine-a.token
```

**2. On machine-b — the one that owns the sessions.** Bind the interface the
peer will actually reach: **a specific address, never `0.0.0.0`**. On a VPN that
is the VPN address; on a trusted LAN it is the LAN address. Loopback is added
alongside it automatically, so the service never becomes undiagnosable from its
own machine.

```sh
export FLEET_MACHINE=machine-b
export FLEET_TOKEN="$(cat ~/.config/muster/machine-b.token)"
export FLEET_ADDR=10.8.0.12:9000        # this machine's VPN or LAN address
export FLEET_ALLOW_MUTATIONS=1          # permit writes to its own sessions
muster serve
```

**3. On machine-a — the one that will call it.** A peer is statically
configured; there is no discovery. The address is one **you** have confirmed
reachable from this machine, never the peer's own idea of its name — that is how
a fleet ends up pointing at a hostname which resolves on only one side.

```sh
export FLEET_MACHINE=machine-a
export FLEET_TOKEN="$(cat ~/.config/muster/machine-a.token)"
export FLEET_PEERS="machine-b=http://10.8.0.12:9000"
export FLEET_ALLOW_RELAY=1              # permit forwarding writes to a peer
muster serve
```

**4. Verify, from machine-a.**

```sh
curl -s -H "Authorization: Bearer $(cat ~/.config/muster/machine-a.token)" \
  http://127.0.0.1:9000/v1/machines
```

```json
{ "items": [ { "machine": "machine-a", "self": true,  "status": "ok" },
             { "machine": "machine-b", "self": false, "status": "ok" } ],
  "complete": true }
```

Both reading `ok` is the entire handshake. If machine-b reads `unreachable`,
the bind address or the network path is wrong — **not** the token: a bad
credential is a `401`, not a silence.

### A machine that is known by more than one address

Some machines have a different fixed address depending on how they are
connected — a laptop that joins the private network through one of two
connections gets one address on each, and only one of them exists at a time.
Both sides handle that without anyone editing configuration when the laptop
moves.

**On the machine itself,** list every address in `FLEET_ADDR`, comma-separated:

```sh
export FLEET_ADDR=10.8.0.12:9000,10.9.0.12:9000
```

Each entry is *wanted*, not *required*. The service binds the ones that exist
now and keeps retrying the others (every few seconds); it logs a line when an
entry is waiting and another when it binds. If an address later goes away, that
listener is closed and the entry goes back to waiting, so nothing is left
holding an address no one can reach. The service starts as long as loopback
binds — which is always mandatory and never waits.

Only "that address is not on this machine" is waited out. An entry that is not
`host:port`, a port another process already holds, a permission refusal and
loopback itself still stop the service at startup, exactly as before. The
rule against `0.0.0.0` is unchanged: the answer to an address that changes is
several *specific* addresses, never a wildcard. `muster doctor` (row
`bind.addr`) says how many entries will bind now and how many are waiting.

**On the machines that call it,** give the peer several URLs, in the order to
try them:

```json
{ "machine": "laptop", "urls": ["http://10.8.0.12:9000", "http://10.9.0.12:9000"], "token": "…" }
```

(`url` still works and means one address; setting both `url` and `urls` on one
entry is refused. In `FLEET_PEERS`, name the machine more than once:
`laptop=http://10.8.0.12:9000,laptop=http://10.9.0.12:9000`.)

A call goes to the address that answered last, and falls over to the next one
**only when no connection could be made** — refused, no route, or no answer
within a few seconds. Once a connection exists the call is never retried on
another address: the peer may already have acted on it, and running a create
twice is worse than reporting it unreachable. A peer on neither address is
`unreachable` exactly as before. `GET /v1/machines` shows the address in use
(`address`) for a peer configured with more than one, and `muster doctor` probes
each in order and names, by position, the one that answered.

> **The two credentials are not symmetric.** The token machine-a presents for a
> peer is *machine-a's identity on machine-b*, not machine-b's identity here.
> They are different secrets, and conflating them is how a fleet quietly ends up
> back on one shared token.

You now have one API over both machines. What to do with it is in
[Driving a session on another machine](driving-a-machine.md).

> **This is a demo, not an install.** Every process above ends with its shell.
> A service that survives a reboot — a principal table, a state directory, a
> service unit, and a `muster doctor` run that checks all of it — is
> [`install.md`](install.md).
