# The owner-signed machine list

One signed file that says which machines belong to a fleet, how to reach them
and what each may do, so that nobody writes every machine into every other by
hand. A service reads it in place of (or over) the `peers` it was given.

This page is the reference for the format. Other programs can verify the same
file from it; the vector at the end is checked by a test, so it cannot drift
from the code.

## Trust

The signer's public key is **pinned locally, by a person**, in each machine's
config. It is never read from the list, from where the list was delivered, or
from the document inside it. The channel is untrusted: copy the file by any
means (a shared folder, scp, a message). A signature that verifies under a key
nobody pinned is refused, however well it verifies.

Keep the owner's private key **offline**. The service never reads it; only
`muster machine-list sign` does.

## Using it

```sh
muster machine-list keygen --out=owner.key        # prints the public key; keep the file offline
muster machine-list create --fleet=my-fleet --key=owner.key --out=draft.json \
  --machine='machine-a,ed25519:...,http://10.8.0.11:9000|http://192.168.1.11:9000,read|send|relay' \
  --machine='machine-b,ed25519:...,http://10.8.0.12:9000,read|create'
muster machine-list sign   --in=draft.json --key=owner.key --out=machines.json
muster machine-list verify machines.json --fleet=my-fleet --signer=ed25519:...
muster machine-list show   machines.json --fleet=my-fleet --signer=ed25519:...
```

A machine's entry is `ID,PUBLIC-KEY,ADDRESS|ADDRESS,GRANT|GRANT`. The public
key is what `muster key show` prints on that machine. Editing the draft by hand
is just as good; `sign` is the only step that needs the key. To issue the next
list, raise `seq` (`sign --seq=N`, or edit it) and sign again.

On each machine, in `config.json`:

```json
{ "machineList": { "mode": "shadow",
                   "path": "/etc/muster/machines.json",
                   "fleet": "my-fleet",
                   "signer": "ed25519:...",
                   "nextSigner": "ed25519:..." } }
```

`nextSigner` is optional (see *Recovery*). The service watches the file and
applies a newer list without a restart (a list that does not verify, or is not
newer, is refused as a whole and the last good one stays). A list can also be
handed to a running service by a peer: see *Handing a list to another machine*.

### Modes

| mode | effect |
|---|---|
| `off` | the list is ignored. The default; an absent block means this. |
| `shadow` | verify and report, change nothing. The log and `muster doctor` (row `machine-list`) say what `prefer` or `only` *would* change: machines it adds, addresses that differ, hand-written peers it would drop. Nothing is recorded in the state directory. Never stops the service. |
| `prefer` | a machine in the list wins over a hand-written peer of the same id (addresses from the list, authenticating by key). Hand-written peers the list does not name stay. A list that fails to verify is reported and the hand-written config is used unchanged. |
| `only` | the list is the sole source of peers; `FLEET_PEERS` and the config's `peers` are ignored. A list that fails to verify **stops the service from starting**: it never falls back to hand-written peers. |

`prefer` and `only` need `FLEET_STATE_DIR`: the highest `seq` ever accepted is
remembered there (`machine-list.seq`), and a list below it is refused, so an old
genuine file cannot be replayed.

Move through the modes in that order. `shadow` on every machine first, read the
report, then `prefer`, then `only`.

### What a machine in the list becomes

Each listed machine other than the one reading the list:

- is a **peer**, if it has at least one address, reached at those addresses in
  the order given, authenticating by signing each request with this machine's key
  (as `"auth": "key"` does);
- is a **principal** named by its id, identified by its public key, holding the
  grants the list gives it: what that machine may do *here*.

A listed machine's own entry is skipped on that machine. A machine with no
address can call the others but is not called.

An unknown grant name refuses the whole list rather than being dropped.

Principals that are not machines (an operator's token, say) are never touched.
`prefer` and `only` replace a hand-written principal of the same name as a listed
machine. `only` also drops a hand-written principal named after a hand-written
peer, since that peer no longer exists. **Removing a machine from the list does
not remove a principal you wrote for it by hand**; delete that from the config.

## Format

The file is an envelope:

```json
{
  "format": "muster-machine-list/1",
  "payload": "<base64url, no padding, of the document's JSON bytes>",
  "signer": "ed25519:<base64url public key>",
  "signature": "<base64url Ed25519 signature>"
}
```

The signature is over the bytes `"muster-machine-list/1\n"` followed by the
**decoded payload bytes**, exactly as carried. There is no canonical form to
reimplement: verify the signature over the bytes you were handed, and only then
parse those same bytes. Any edit to the payload, or pairing one list's payload
with another's signature, fails the check. Unknown fields and trailing content
are refused.

The document in the payload:

```json
{
  "format": "muster-machine-list/1",
  "fleet": "my-fleet",
  "seq": 3,
  "issuedAt": "2026-10-22T13:00:00Z",
  "machines": [
    { "id": "machine-a", "publicKey": "ed25519:...", "addresses": ["http://10.8.0.11:9000"], "grants": ["read", "send"] }
  ],
  "issuers": [ { "id": "sso", "publicKey": "ed25519:..." } ],
  "signers": ["ed25519:..."],
  "nextSigner": "ed25519:..."
}
```

| field | meaning |
|---|---|
| `fleet` | the fleet's id. A machine pinned to one fleet refuses another's list. |
| `seq` | starts at 1 and must increase with every list; lower than the highest accepted is refused (equal is fine: reloading the same file). A list **pushed by a peer** must be strictly higher: equal is refused (see *Handing a list to another machine*). |
| `machines[].id` | the machine's id as it names itself (`FLEET_MACHINE`); unique. |
| `machines[].publicKey` | the machine's signing key (`muster key show`). |
| `machines[].addresses` | base URLs, in the order to try. |
| `machines[].grants` | grants the machine holds on the others. |
| `issuers` | sign-in issuers the fleet trusts, for clients that verify people. Recorded and shown; the service does not verify a person itself yet. |
| `signers` | the keys allowed to sign this fleet's lists. **Exactly one is supported**; a document naming more is refused with "not supported yet". |
| `nextSigner` | the pre-pinned recovery key (below). Optional. |

A verifier checks, in this order: the envelope's shape; that `signer` is one of
the locally pinned keys; the signature; the document's shape; that it names one
signer, and that signer signed it; the fleet; `seq` against the high-water mark.
Each failure is a different, named error.

### Recovery

If the owner's key is lost or leaked, the owner signs the next list with the key
named in `nextSigner`, which each machine pinned in advance (`machineList.nextSigner`).
It is accepted once, and `muster doctor` warns that the list was signed by the
recovery key so the operator pins it as the signer and names a new next key.
A key nobody pinned cannot recover anything: the document cannot add to the trust root.

### Rotating a machine's key

A machine's key is in the list, so rotating it means issuing a new list.
`muster key rotate` tells hand-written key peers only; it does not touch a list.

### Handing a list to another machine

A machine can hand the list it holds to another over the service's own API, so
the owner signs once and a running fleet follows without anyone copying files.
The channel is still untrusted: a list is taken for what it is, never for who
delivered it.

```sh
muster machine-list push  machine-b machine-c [--file=machines.json]   # send the list this machine holds
muster machine-list fetch machine-b --out=machines.json                 # read what machine-b has in force
```

Both commands are signed with *this* machine's key (the one `muster key show`
prints), so the peer judges them by what this machine is granted there. They take
`--config`, `--state-dir` and `--self` (this machine's id) when `FLEET_CONFIG`,
`FLEET_STATE_DIR` and `FLEET_MACHINE` are not set. A peer is named as it is in the
config's `peers` (which must say `"auth": "key"`, because the request is signed)
or, failing that, as the list being sent names it, which is what makes `push`
usable while every machine is still in `shadow`. `push` first verifies the list
against this machine's own pins and the highest sequence accepted here, and sends
nothing a machine would not take itself. `fetch` verifies what it got against
what *you* pin and writes it out; it never installs anything. Neither runs on its
own: the service does not push or pull lists.

**Who may do what.** Serving needs the `read` grant. **Accepting needs `push-list`,
a grant of its own that nothing implies**: not `send`, not `relay`, not the
legacy flags, and not what `muster service install` gives the local service. An
operator gives it to one peer at a time, on the machine that will take the push:

```sh
muster principal add machine-a --public-key=ed25519:... --grants=read,push-list   # a new peer
muster principal grant machine-a push-list                                          # one that exists
```

A push from a peer without it is refused before its body is read. The grant is
**local on purpose**: a signed list that names `push-list` is refused as a whole
(like an unknown grant), because who may hand a machine its next list is decided
there, like the pinned signer. In `prefer` and `only`, where a listed machine
replaces a hand-written principal of the same name, that one grant is carried
across to the listed machine **when the key is the same**; if the list gives that
machine a different key the grant lapses and `muster doctor` (row `machine-list`)
says so. Grant it only after the machine runs a build that knows it, and revoke it
before rolling back: an older build refuses a config that names a grant it does
not know.

**What a push is checked against**, in this order, refusing at the first failure
and changing nothing:

1. the caller's credential, then its `push-list` grant (no principal table: refused);
2. that this machine uses a list at all, and in `prefer`/`only` has a state
   directory to remember the highest sequence in;
3. the size, at most 1 MiB;
4. only then is the body read, and the list verified against the signer pinned
   **here**: shape, signer, signature, fleet, grants;
5. its `seq` must be **strictly higher** than the highest this machine has
   accepted: the higher of the mark in the state directory and the list in force.
   Equal is refused. Shadow records no mark, so there the list in force is the
   floor.

Then the list replaces the current one, **whether that was copied in by hand or
not**: it is written beside the configured path and renamed over it (a crash never
leaves half a list), and put in force by the same reload a changed file gets, with
no restart. The answer is binary. `200` means the list is written **and** in force;
anything else means nothing changed, on disk or in memory, and the reason is one of:

| status | reason | meaning |
|---|---|---|
| 401 | | no credential, or the caller lacks `push-list` |
| 501 | `machine-list-off` | this machine uses no list |
| 501 | `machine-list-no-state-dir` | `prefer`/`only` need `FLEET_STATE_DIR` |
| 400 | `machine-list-too-large` | over 1 MiB |
| 400 | `machine-list-malformed` | not a list, or trailing content |
| 400 | `machine-list-signer-not-pinned` | signed by a key not pinned here |
| 400 | `machine-list-signature` | the signature does not verify |
| 400 | `machine-list-other-fleet` | another fleet's list |
| 400 | `machine-list-grant` | an unknown grant, or `push-list`, in the list |
| 501 | `machine-list-unsupported` | names several signers |
| 409 | `machine-list-not-newer` | `seq` is not higher than the highest accepted |
| 501 | `machine-list-not-applied` | genuine, but this machine cannot put it in force now (a peer in it cannot be built; the sequence cannot be recorded, in which case the previous file is restored) |
| 501 | `machine-list-not-writable` | the list's file cannot be written: the service must be able to write the directory the list lives in |

A push can bring forward any genuine newer list, including one that no longer
names the pusher or empties the peer set in `only`; that is the owner's signed
decision. It cannot change the pinned signers, the mode or the path (those stay
restart-only), and it never touches principals that are not machines. A list
signed far ahead in `seq` raises the mark for good: that is the owner's
signature, and the owner's to use sparingly.

### What is not here yet

Several signers, several fleets in one list, and a service that hands its list on
by itself (a push or a pull is a command someone runs).

## Test vector

Seed: 32 bytes of `0x01`. Document (the exact payload bytes):

```
{"format":"muster-machine-list/1","fleet":"example-fleet","seq":1,"machines":[{"id":"machine-a","publicKey":"ed25519:0EqyMnQrtKs6E2i9RhXk5tAiSrcaAWuvhSCjMsl3hzc","addresses":["http://10.8.0.11:9000"],"grants":["read"]}],"issuers":[],"signers":["ed25519:iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w"]}
```

Signer public key: `ed25519:iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w`

Signature (base64url) over `"muster-machine-list/1\n" || payload`:

```
c6xQZfPYQ_KaZCXa_IFNBg1NvdP94I-qL1AkrfaK0UFtrpN_kI4MCmK9UpPFirj4d8HVreFOU6JcPM1YXasKAQ
```

Ed25519 signatures are deterministic, so an independent implementation given the
seed and the payload must produce this signature exactly. (The machine's own key,
`ed25519:0EqyMnQr...`, is the key from seed `0x11` ×32.) A verifier pinned to fleet
`example-fleet` and this signer accepts it with `seq` 1.
