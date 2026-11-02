# Adopting the shared Go lint policy (kennlint)

`go.kenn.io/kit/lint` owns the Go lint policy for kenn-io repositories: a
canonical golangci-lint configuration, eight custom analyzers, and a
golangci-lint module plugin that runs them. This document explains how to put a
repository on the shared policy and how to keep it there.

The policy assumes the Go language version in kit's `go.mod` or newer. Some
rules point at APIs that older toolchains lack: the `errors.As` ban expects
`errors.AsType` (Go 1.26), `usetesting` expects `t.Context()` (Go 1.24), and
`sleeptest` and `deadlinetest` expect `synctest.Test` (Go 1.25). A repository
on an older language version should raise it before adopting, or disable those
rules in its overlay until it can.

## What you get

**One configuration.** golangci-lint has no configuration inheritance, so each
repository used to copy a config and drift. Instead a repository commits only
an overlay with its local additions, and `kennlint config` renders the merged
`.golangci.yml` from the canonical file plus the overlay. A `-check` mode fails
CI when the committed file is stale.

**Eight analyzers** cover checks that off-the-shelf linters miss. They run
under `kennlint`. The separate `kennnolint` linter requires kit suppressions
to name known rules and give a reason:
`//nolint:kennlint // <rule>[,<rule>...]: <reason>`. The label documents the
exception; golangci-lint still suppresses every `kennlint` analyzer on that
line, whichever rule the label names.

`deadlinetest` stays enabled through `kennlint`, including on routine plugin
upgrades. `kennnolint` has a custom definition in the canonical configuration
but remains disabled there. Kit enables it in its overlay. Consumers enable
it after adding rule labels and reasons to their suppressions.

| Analyzer | Reports |
| --- | --- |
| `sqlclosecheck`, `rowserrcheck` | SQL resource closure and iteration errors, including returned ownership and shared scanners across packages. The kit versions extend the upstream checks; do not enable the duplicate built-in checks. |
| `testifyhelper` | Imports and assertion objects must be named `assert` or `require`. Set `testifyhelper.require-helpers: true` in plugin settings to also require local helpers for repeated package calls; the default is false for both libraries. Parent scopes retain package calls when nested functions need package access. Run with `--fix` to apply object-aware renames and, when enabled, local-helper conversions. |
| `sleeptest` | `time.Sleep` in a `_test.go` file outside a `synctest.Test` bubble. Wall-clock sleeps make tests slow and timing-dependent. A bubble is the body of the function passed to `synctest.Test`, inline or by name, at any nesting depth including goroutines. A helper that sleeps and is only called from inside a bubble is still reported: give it a channel to wait on instead. Dot-imported and parenthesized `time.Sleep` calls match too. Packages named `testutil` or ending in `test` are checked too (`helper-packages`, default on). Set `eventually: true` to also report testify `Eventually`, `EventuallyWithT`, and `Never`, and their `f` variants, outside bubbles; that is off by default because some repositories endorse `Eventually` for awaiting a fake's channel. |
| `deadlinetest` | Sub-second wall-clock budgets in `_test.go` files outside a `synctest.Test` bubble: `context.WithTimeout` and `WithTimeoutCause`, `context.WithDeadline` and `WithDeadlineCause` written as `time.Now().Add(d)`, `time.After`, `time.NewTimer`, `time.AfterFunc`, and the `waitFor` of testify's `Eventually` and `EventuallyWithT` and their `f` variants. A budget under 1s fails on a loaded CI runner even when the code is right; wait on a real event or run the test in a bubble. Duration fields in keyed and positional struct literals, including implicit pointer literals, plain `=` field assignments, duration arguments to statically resolved same-module functions and methods, and standalone `time.Now().Add(d)` deadlines are also checked. For duration fields and helper arguments, field and formal parameter names must have timeout, deadline, budget, or wait as a camelCase or snake_case word, singular or plural, ignoring case. Polling intervals, ticks, and retry delays are not checked: a short cadence speeds a test up and cannot fail it. Standalone `time.Now().Add(d)` checks apply regardless of the destination name. Record ages and generic durations are outside this vocabulary. Duration elements in maps and slices, closures, and interface calls are outside the statically resolved scope. Only constant budgets are reported, and zero and negative ones only for polling assertions, which still pass or fail by timing. Testify's `Never` and `Neverf` are reported only with a zero or negative budget, which passes vacuously: a short positive window checks fewer times on a loaded runner but cannot fail. Same-module helper calls from `_test.go` files are checked, including calls into helper packages; helper package source files themselves are not scanned. Two cases are legitimate `//nolint:kennlint // deadlinetest: <reason>` exemptions: a test whose asserted result is the deadline expiring, and a `select` on `time.After` that shows an event does not happen. With sleeptest's `eventually: true`, a sub-second `Eventually` or `EventuallyWithT` call gets one diagnostic from each analyzer; `//nolint:kennlint // sleeptest,deadlinetest: <reason>` covers both. A repository with a backlog can list `deadlinetest` under the `kennlint` plugin's `disable` settings while it burns it down. |
| `errtext` | Deciding on error identity by matching `err.Error()` text: `strings.Contains(err.Error(), …)`, `err.Error() == …`, and similar. Use `errors.Is` or `errors.AsType`. Test files are skipped unless `errtext.include-tests` is set. |
| `sqlcheck` | SQL `CHECK` constraints and `CREATE TYPE ... AS ENUM` in Go string literals outside tests. A CHECK locks a validation rule into the schema, so every change to the rule (most often a new allowed value for a status-like column) needs a migration that rewrites the constraint. Validate in application code or keep allowed values in a lookup table. `kennlint sql` applies the same check to `.sql` migration files. |
| `nohttpmux` | `Handle`/`HandleFunc` on `*http.ServeMux` or the default mux outside tests, for repositories that route every operation through a typed API layer such as Huma. Disable it in repositories that serve plain `net/http`. |

`kennlint analyzers` prints this list from the binary.

## Adopting a repository

1. Add the plugin to `.custom-gcl.yml` (create the file if the repository does
   not already build a custom golangci-lint):

   ```yaml
   version: v2.13.1
   plugins:
     - module: "go.kenn.io/kit"
       import: "go.kenn.io/kit/lint/gclplugin"
       version: "<kit version from go.mod>"
   ```

   The kit version should match the one already required by `go.mod` so the
   custom build resolves the same module graph. Repositories that already use
   the nilaway plugin keep it in the same list.

2. Write the overlay, `.golangci.overlay.yml`, with the repository's own
   additions: `importas` aliases, extra `forbidigo` patterns, exclusion rules
   for known carve-outs. To adopt suppression checks, enable `kennnolint` in the overlay:

   ```yaml
   linters:
     enable:
       - kennnolint
   ```

   Add rule labels and reasons to existing exceptions, for example
   `//nolint:kennlint // deadlinetest: deadline expiry is the asserted result`.
   Existing deadline suppressions remain valid on a plugin upgrade;
   `kennnolint` requires their labels once enabled. Consumer adoption fixes
   the widened findings or gives each exception a reason before enabling
   `kennnolint`. Repositories with a backlog can keep `deadlinetest` in the
   `kennlint` plugin's `disable` settings while they work through it.

3. Render and commit the configuration:

   ```sh
   go run go.kenn.io/kit/cmd/kennlint@<kit version> config
   ```

4. Build the custom binary and run it:

   ```sh
   go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 custom \
     --destination . --name custom-gcl --version v2.13.1
   ./custom-gcl run ./...
   ```

   Add `custom-gcl` to `.gitignore`. golangci-lint caches the build and skips it
   when the plugin list is unchanged.

5. Wire the two commands into the repository's lint target and git
   hooks, and add a drift check to CI:

   ```sh
   go run go.kenn.io/kit/cmd/kennlint@<kit version> config -check
   ```

   The kit repository's `Makefile` and `prek.toml` are a working example.

## Overlay semantics

`kennlint config` merges the overlay into the canonical configuration
recursively:

- Mapping keys present in both are merged; keys only in the overlay are added.
- Lists are appended, skipping items the canonical list already contains. This
  is how `linters.enable`, `forbidigo.forbid`, `importas.alias`, and
  `exclusions.rules` grow per repository.
- Scalars in the overlay replace the canonical value, for example
  `run.timeout`.
- `linters.disable` and `formatters.disable` in the overlay remove those names
  from the rendered `enable` lists and are not emitted themselves. Use them for
  a documented backlog, not as a permanent opt-out.

Plugin settings live under `linters.settings.custom.kennlint.settings`:

```yaml
linters:
  settings:
    custom:
      kennlint:
        settings:
          disable: [nohttpmux]      # analyzers to leave out
          errtext:
            include-tests: true     # also report err.Error() matching in tests
```

## Rolling out on a repository with a backlog

Turning the full policy on a large repository usually surfaces hundreds of
existing findings. Two staged approaches work:

- **Backlog overlay.** Disable the linters with large counts in the overlay,
  file an issue listing them, and remove entries as the backlog burns down.
  This keeps CI independent of git history.
- **Only new code.** Set `issues.new-from-merge-base: origin/main` in the
  overlay so golangci-lint reports only findings introduced on the branch.
  This needs full history in CI checkouts.

Either way, suppress individual findings with `//nolint:<linter> // reason`.
For grouped kit findings, name the analyzer, for example `//nolint:kennlint // sleeptest: waits for a real process`.
For deadline findings, use `//nolint:kennlint // deadlinetest: deadline expiry is the asserted result`.
Keep `kennnolint` enabled when adopting grouped suppression checks.
Two rules that trip existing tests: `t.Context()` is already canceled inside
`t.Cleanup`, so derive `context.WithoutCancel(t.Context())` there, and helper
subprocesses started with `exec.CommandContext(t.Context(), …)` are killed at
cleanup unless they use the same uncancelled context.
The canonical `nolintlint` settings require the linter name and an explanation
and reject directives that no longer suppress anything.

## Checking SQL migration files

golangci-lint only sees Go, so migrations kept as `.sql` files need the
separate scanner:

```sh
go run go.kenn.io/kit/cmd/kennlint@<kit version> sql internal/db/migrations
```

It prints `file:line:column: message` for each `CHECK` constraint and each
`CREATE TYPE ... AS ENUM`, and exits non-zero when it finds any. Wire it into
the lint target or a pre-commit hook filtered to `*.sql`.

Every `CHECK` is reported, not only ones that spell out a value list. Range
and length checks and cross-column invariants lock a rule into the schema in
exactly the same way, and in practice most of them exist to police a set of
states. `CHECK` text inside SQL comments and string literals is ignored, so a
migration that removes a constraint can mention it in a comment. Where a set
really must live in the database, put it in a lookup table with a foreign
key.

## Running the analyzers without golangci-lint

golangci-lint cannot load an analyzer from an imported package at runtime;
its only extension points are the module plugin used here, which needs the
`golangci-lint custom` build, and Go `.so` plugins. Repositories that already
build a custom binary for nilaway pay nothing extra for the plugin.

Without a custom binary, `kennlint run ./...` runs the eight analyzers directly
through the standard `go/analysis` multichecker, with the usual `-json`,
`-fix`, and per-analyzer flags such as `-errtext.include-tests` or
`-sleeptest.eventually` or `-testifyhelper.require-helpers`; `go vet -vettool=$(command -v kennlint) ./...` works
too. Both are useful for editors and for repositories that cannot build a
custom golangci-lint, but neither honors `nolint` comments or golangci path
exclusions. Rolling `sleeptest` out to a repository with existing sleeps
works best through golangci-lint's `new-from-rev` so new sleeps are blocked
while the backlog is burned down.

## Measuring lint cost

`kennlint profile ./...` runs the analyzers once and prints the time each one,
prerequisites such as `buildssa` included, spent on your packages and on the
dependencies it ran on because some analyzer in the run declares facts. Pass
`-tags` to match the build tags in `.golangci.yml`, `-disable` to match the
plugin settings, and `-cpuprofile` to dig into one analyzer. Times are
sequential by default so they add up; `-parallel` shows real wall time instead.
golangci-lint's `-v` output prints a coarser per-analyzer total for the whole
run.

## Changing the policy

The canonical configuration is `lint/config/golangci.yml`. Analyzers live in
`lint/<name>/` with `analysistest` fixtures under `testdata/`. Changes there
land in every repository on its next kit upgrade and `kennlint config` run, so
prefer additive changes and describe the rationale in the pull request.

## Retrying workflow downloads

`kennlint workflow-retries [path ...]` scans workflow and composite-action YAML,
defaulting to `.github`. It checks Bash and sh `run:` steps. Steps without an
explicit shell are checked as Bash on every runner, even though Windows runs
them in PowerShell. Steps that don't parse as Bash are skipped.
`kit-retry` and `kit-download` only run under Bash, so Windows steps that call
them need `shell: bash`. Set `shell: pwsh` on PowerShell-only steps to skip them.
Every `curl` or `wget` call must use `kit-download`.
Package installers must run under `kit-retry`, including APT, Go, npm, pnpm,
yarn, bun, pip, Docker pulls, apk, brew, cargo, pipx, and dnf.
A bare `yarn`, with only options, counts as an install.
Findings point at the command in `run: |` blocks and single-line plain values,
and at the `run:` key otherwise.
The check does not follow other launchers (`timeout`, `xargs`, `bash -c`, or
value-taking options of `sudo`, `env`, and `exec`), and does not flag unlisted
commands that fetch, such as `docker run`, `go run module@version`, or
`go mod tidy`, or installers outside the list, such as `gem`, `uv`, or `choco`.

Add the action after checkout to put both commands on PATH. Copy the action
into your repository, or reference the Kit action at a pinned commit.

```yaml
- uses: ./.github/actions/kit-retry
- shell: bash
  run: |
    kit-download https://example.com/tool.tgz tool.tgz
    kit-retry 3 5 sudo apt-get -o APT::Update::Error-Mode=any update
    kit-retry 3 5 sudo apt-get install -y tool
```

`kit-retry ATTEMPTS BASE_DELAY COMMAND [ARG...]` waits BASE_DELAY seconds,
then twice that delay, between attempts. The example makes three attempts
with waits of five and ten seconds. APT's error mode makes a failed mirror
fetch return an error; plain `apt-get update` can exit successfully.

`kit-download URL DEST` replaces DEST only after a successful download and
removes its temporary file on failure. It retries three times with waits of
five and ten seconds, a 30-second connection timeout, and a low-speed timeout.
For `curl | sh` installers, download a file first, then run it. Downloads with
authentication headers need a custom download command with an allow comment.
Put `# workflowretry: allow <reason>` on the line where the command starts,
or alone on the line above. Local readiness probes that already poll can use it.
On Windows, `shell: bash` is the tested path for the action's PATH setup.
