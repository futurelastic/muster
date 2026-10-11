# AGENTS.md — muster

A machine-local service that owns session agents across a fleet, and the peer
federation that makes two of them look like one. Go, standard library only.

Start with [`README.md`](README.md) for what it is and
[`docs/spec/`](docs/spec/) for what it promises. Those are normative; this file
is only how an agent working here finds the rules.

## This repo is PUBLIC

Everything committed here — code, docs, commit messages, Issues, PR bodies — is
world-readable, and commit messages cannot be corrected afterwards.

**Name no other system.** Not an application, a repository, a production domain,
an internal host, or a filesystem path. Describe by shape instead: "a service on
one machine", "the peer", "a repo whose CI is hand-written". The findings this
repo cites come from private infrastructure and are just as convincing
structurally — "51 of 52 read healthy throughout" carries the whole argument
without naming anything.

Stack and tooling names are not covered: Go, tmux, ssh, zsh, a process manager.
Those describe the technology, not our systems.

**Everything landing here is written in English**, whatever language the session
is being run in. An outside adopter cannot read the alternative.

Checkable before you commit:

```sh
git grep -nEi '<internal hostnames>|<internal path prefix>' HEAD -- .   # must print nothing
```

The concrete vocabulary that check needs is machine-local and deliberately not
in this file — see the working notes that never leave the machine.

## Issues are this repo's notebook

Findings get filed, not remembered. Twenty-odd issues here are measurements
somebody made once and would otherwise have to make again — a classifier that
misread a screen, a receipt that lied, a durability hole found by deploying.
When you learn something the code does not already say, that is an Issue.
