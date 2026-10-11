# CLAUDE.md — muster

@AGENTS.md

## Conventions

<!-- colab-handbook @ v1.11.0 -->

This repo follows the [colab-handbook](https://github.com/godx-jp/colab-handbook/blob/main/CONVENTIONS.md) conventions.

- **Tier:** `B` — no production target; a human runs the documented
  build-install-restart procedure to put a commit in front of anything. See
  [`docs/deploy.md`](docs/deploy.md).
- **Trunk:** `main` (feature branches `feat|fix|docs|chore|refactor|test|perf/<slug>`)
- **Descriptor:** see `.github/project.yml`. CI workflows are copy-and-own from
  the handbook's `templates/` — this repo owns its copies, they are not called
  remotely. The workflow here is hand-written rather than copied, and predates
  adoption: treat it as this repo's own file, not as a template copy to refresh.
- **Writes:** `serial-direct` — one unit of work in flight, landing on trunk.
  Claim before you start (`colab claim <issue>`); the claim is what stops two
  sessions taking the same issue.
- **Secret guard:** `.githooks/pre-commit` runs only in a clone that ran
  `.githooks/install.sh` — the setting is per clone, and a commit says nothing
  when it is off (#201). `go run ./cmd/muster doctor --offline` from a
  clone prints a `hooks.pre-commit` row; `warn` means commits there are not
  scanned. Check it before your first commit in a clone you have not used.
- **Gate:** `scripts/gate.sh` — `gofmt -l .`, `go build ./...`, `go vet ./...`,
  `go test -race ./...`. Run it before you wrap; `ci.yml`'s `go` job calls this
  same file, so a local pass and CI answer the same question by construction
  (#233 — a gate remembered separately in two places had already drifted when
  #230 shipped gofmt-dirty with build/vet/test all green).
