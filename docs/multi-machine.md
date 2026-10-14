# Two machines, over a LAN or a VPN

Federating two instances: one machine owns sessions, the other calls it through
its own local service. This is the end-to-end walk-through; once both machines
read `ok`, [Driving a session on another machine](driving-a-machine.md) shows what
it buys you.

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
