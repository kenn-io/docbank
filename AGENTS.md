# AGENTS.md

Instructions for autonomous coding agents working in this repository.

## Build and Test

- Go 1.26+ (go.mod is authoritative). Go 1.26 language features — e.g.
  value-form `new(v)` — are in use; verify against the toolchain before
  treating unfamiliar syntax as an error.
- Every build and test needs CGO and the fts5 tag: `make test` or
  `go test -tags fts5 ./...`. A bare `go build ./...` failing on sqlite
  symbols is expected, not a defect.
- Lint with `make lint` (golangci-lint, fts5 tag). Docs build strict with
  `make docs-build`; fix every warning.
- The vault is Unix-only; `internal/...` must keep compiling on Windows
  through the non-Unix stubs (CI enforces this).

## Git Rules

1. Commit every turn that changes tracked files; never amend.
2. Never push to or commit on main — feature branches and PRs only.
3. Do not merge pull requests; opening and reporting them is the agent's
   job, merging is the user's.
4. Run `prek run` before committing.
5. Keep pull request descriptions rationale-first. Do not add routine
   `Validation`, `Testing`, or checklist sections for `make test`, lint, docs,
   vet, `prek`, CI, or ordinary conformance suites; report those results in the
   handoff instead. Include validation in the PR body only when it is novel
   evidence that materially informs review, such as a migration rehearsal,
   benchmark, real-vault hardening run, or compatibility experiment.

## Releases

- Cut releases only from a clean local `main` that exactly matches
  `origin/main`.
- Preview notes with `scripts/changelog.sh <version>`; publish with
  `scripts/release.sh <version> [extra_instructions]`.
- The release script creates and pushes an annotated `vX.Y.Z` tag. The release
  workflow uses the tag body as GitHub release notes and falls back to generated
  notes only for a lightweight or empty tag.

## Design Invariants

- Daemon-first: the daemon is the only process that opens the store; CLI
  commands are HTTP clients (`client.Ensure`). Do not add code paths that
  open the vault directly from a command.
- The daemon always enforces an API key (ephemeral per-run when none is
  configured, published via the runtime record). Binds are loopback-only.
- User-facing docs describe only what exists; planned behavior sits under
  `!!! info "Planned"` admonitions.

<!-- BEGIN KATA (managed by `kata init --with-agents`) -->
## kata issue tracker

This project uses [kata](https://github.com/kenn-io/kata) as its shared issue
ledger. Run `kata quickstart` at the start of each session for the full agent
contract. The short version:

- Search before creating: `kata search "<keywords>" --agent`.
- Prefer updating existing issues over duplicates (`kata comment`, `kata label add`, `kata edit`).
- Default to `--agent` for ordinary reads and mutations; use `--json` only when a script needs structured data.
- Close only verified work: `kata close <ref> --done --message "<scope + verification>" --commit <sha>`.
- If work is incomplete, label `needs-review` and comment what remains rather than closing.
- Never `kata delete` or `kata purge` without explicit user authorization.
<!-- END KATA -->
