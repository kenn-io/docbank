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
- Linux, macOS, and Windows are supported. Windows CI runs the complete CLI,
  daemon, lock, backup, and restore suite on amd64 and arm64; do not replace
  real platform behavior with compile-only stubs.

## Private Data Boundary

- Never put private developer information in tracked files. Tests, fixtures,
  golden files, examples, and documentation must use synthetic data—not files,
  filenames, directory structure, document contents, credentials, session
  transcripts, hashes, or metadata copied from a developer's home directory or
  personal corpus.
- Access a real personal corpus only with explicit authorization for that
  validation. Keep the source read-only, work in an owner-private isolated
  directory outside the repository, and emit only aggregate evidence unless
  the user explicitly requests otherwise.
- Before handing off a real-corpus validation, stop its processes and remove
  every temporary vault, backup, restore, binary, report, mismatch list, log,
  and cache created for the run—even when the validation fails. Confirm the
  cleanup rather than assuming a deferred cleanup ran.

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
  `scripts/release.sh <version> [extra_instructions] [start_tag]`. Supply the
  optional start tag only when notes must span an intervening tag that did not
  publish.
- The release script creates and pushes an annotated `vX.Y.Z` tag. The release
  workflow uses the tag body as GitHub release notes and falls back to generated
  notes only for a lightweight or empty tag.

## Design Invariants

- Daemon-first: the daemon is the only process that opens the store; CLI
  commands are HTTP clients (`client.Ensure`). Do not add code paths that
  open the vault directly from a command.
- The daemon always enforces an API key (ephemeral per-run when none is
  configured, published via the runtime record). Binds are loopback-only.
- Packed storage is managed, not a user-selected format. The ordinary operator
  surface is status, pack, and repack. Do not expose Kit's unpack primitive as
  a general API or CLI command; reserve it for tests, migrations, or a
  purpose-built emergency recovery workflow with a demonstrated need.
- Documentation is not the implementation tracker. User- and agent-facing
  pages describe shipped capabilities and current limitations. Architecture
  and internal pages may preserve durable future design under explicit
  `!!! info "Planned"` admonitions, but must not carry task breakdowns,
  sequencing, ownership, or completion criteria. `docs/roadmap.md` is the one
  high-level public product-status view; kata is the sole source of truth for
  actionable work and its status.
- No storage compatibility boundary exists before the first public release.
  Keep the live schema and deterministic JSONL authority at format v1, make
  breaking pre-release changes directly, and treat vaults created by earlier
  development commits as disposable. Do not build migrations, logical
  cutovers, compatibility decoders, downgrade fences, or old-binary matrices
  for unreleased layouts. After the first public release, define compatibility
  work only when an actual released-format change requires it.

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
