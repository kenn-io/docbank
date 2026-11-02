# Agent Instructions

## Project Overview

`go.kenn.io/kit` is a Go module of reusable building blocks for Kenn CLI and
developer tools. Keep packages small, app-neutral, and usable by more than one
caller. Do not add product-specific config, database state, UI behavior, or
provider workflows to this repo unless the package already owns that concern.

## How To Work Here

- Start from the package intent, not from a calling app's current needs. The
  reusable package should stay useful to future callers with similar mechanics.
- Prefer the repo's existing helpers and test fixtures over recreating local
  variants in a caller.
- Follow the Go version and dependency choices in `go.mod`; do not pin guidance
  here to a specific version unless the code requires that version for a reason.
- Keep Unix and Windows behavior explicit when permissions, ownership, sockets,
  paths, or process behavior differ.
- When behavior changes, update the package-level `AGENTS.md` nearest the change
  if it captures an invariant future agents need to preserve.
- Let CI, `go.mod`, and tool config files define the exact verification
  commands. Do not duplicate command recipes here unless the command carries a
  repo-specific intent that is not encoded elsewhere.

## Supported Environments

- Kit targets the latest releases of its libraries, runtimes, and external
  services (databases, search engines, embedding servers), running with their
  default settings. One to two years old is the outer limit; anything older,
  and any legacy compatibility mode or deprecated setting, is out of scope.
- Do not add code, tests, or documentation to handle them, and do not raise
  them as open questions. For example, PostgreSQL's
  `standard_conforming_strings=off` is out of scope.
- A review finding that only applies to an unsupported version, mode, or
  setting is not an issue. Record it as such without asking the user.

## Roborev

- Never invoke the `roborev review` CLI command in any form unless the user
  explicitly asks for it. Use all other `roborev` CLI commands normally when
  they are appropriate for interacting with roborev. Never invoke a roborev
  skill (including `roborev-fix` or `roborev-design-review-branch`) unless the
  user explicitly asks for that skill.

## Content Hygiene

- Never name private downstream projects or other private codebases in public
  artifacts. Describe reusable requirements generically and scrub public
  surfaces before publishing.

## Go Conventions

- Use standard library APIs first and add dependencies only when they pay for
  themselves.
- Keep public package APIs narrow and app-neutral. Callers should not need to
  import unrelated kit packages to use one package correctly.
- Surface errors with enough context for callers to act on them. Do not turn
  failures into success-shaped fallbacks.
- Use `context.Context` for subprocesses, network calls, update checks, probes,
  and other work that can block.
- Avoid broad cleanup or mutation. Operate on exact paths, runtime records, and
  repositories that the caller supplied or the test created.

## Lint

- `.golangci.yml` is generated: run `make lint-config` after editing
  `.golangci.overlay.yml` or `lint/config/golangci.yml`. The shared policy and
  the custom analyzers live in `lint/`; see `lint/AGENTS.md`.

## Tests

- Use `github.com/stretchr/testify` for new and changed tests. Prefer
  `require` for setup, preconditions, and values used later; use `assert` for
  independent checks where more failures help diagnosis.
- Do not add new `t.Fatal`, `t.Fatalf`, `t.Error`, `t.Errorf`, `t.Fail`, or
  `t.FailNow` calls. Existing tests still contain some stdlib assertions; when
  editing those checks, migrate the touched checks to testify if it keeps the
  test readable.
- Package-level testify calls and local helpers are both valid. When using
  local helpers, name them `assert := assert.New(t)` or
  `require := require.New(t)`.
- Prefer table tests when they make input and expected behavior clearer.
- Use `t.TempDir()` for files created by tests unless the test specifically
  needs a fixed OS temp path to exercise permissions or runtime-dir behavior.
- Tests must not depend on the user's git config, global credentials, real
  repositories, home directory state, or live provider availability.
- Never locate files from a path returned by `runtime.Caller` or a stack
  trace; builds use `-trimpath`, so those paths are module-relative. Tests run
  from their package directory, so use paths relative to it.
- Trimmed binaries cannot find Go's bundled time zone database, so
  `time.LoadLocation` relies on the host's. A test package that loads a named
  zone and runs on Windows, which has no host database, or uses a legacy name
  such as `US/Central` must add `import _ "time/tzdata"` in a `_test.go` file.

## CI runners

Windows tests use GitHub-hosted runners, with `git/managed` in three separate jobs.
Linux and macOS jobs use Namespace's
[Restricted access level](https://namespace.so/docs/solutions/github-actions/runner-controls/access-levels),
which disables workload access to Namespace features and APIs. GitHub fork
approvals, token permissions, and secrets are separate controls.

## Git Workflow

- Do not change branches unless the user explicitly asks.
- Do not amend commits unless the user explicitly asks.
- Pull requests must have a user-facing benefit or improve the developer
  experience, and the body must say which one.
- Write issues and pull requests in English.
- Do not poll or watch CI or pull request checks after pushing unless the user
  explicitly asks. A one-time status lookup is allowed when diagnosing a
  user-identified CI failure.
- Never revert user changes. If existing edits touch the same files, read them
  and work with them.
- Keep pull request descriptions concise and rationale-first: explain the
  problem, the chosen approach, and consequences that matter to reviewers.
- Do not add validation sections, command or check inventories, benchmark logs,
  caveats about unrelated pre-existing failures, or agent/tool attribution to
  pull request descriptions. Put verification evidence in CI, the kata issue,
  or a targeted review comment when it is useful.

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

## kata work.* conventions (agent orchestration)

When working a kata-tracked issue, keep its `work.*` metadata truthful
(see docs/operations/agent-orchestration.md for the full recipe):

- On claim/start: `kata meta set <ref> work.attention ok`; if the work has a
  dedicated branch, stamp it once with `kata meta set <ref> work.branch <branch>`.
- Signal live state: `kata meta set <ref> work.attention stuck|needs-human|ok`
  plus a one-line `work.attention_msg` saying why. Raise `stuck` when you cannot
  proceed, `needs-human` when you want review; clear back to `ok` when unblocked.
- Never stop with the signal stale: close the issue, or leave the attention
  pair reflecting the hand-off.
- Coordinators read `work.*` on issues they delegated; only the working agent
  writes them. `work.*` on closed issues is meaningless.
<!-- END KATA -->
