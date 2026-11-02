# Lint Package Instructions

## Scope

`lint/` owns the shared Go lint policy for kenn-io repositories:

- `lint/config/golangci.yml` is the canonical golangci-lint configuration and
  `lint/config` merges a repository overlay into it.
- `lint/<name>/` packages are `go/analysis` analyzers; `lint.Analyzers()` is
  the complete list.
- `lint/sqlcheck` also exposes `Scan` for raw SQL; `kennlint sql` uses it on
  `.sql` files because golangci-lint only analyzes Go. It reports every
  `CHECK` constraint and `CREATE TYPE ... AS ENUM` outside comments and string
  literals; do not add shape-based exemptions, the policy is that validation
  rules do not live in the schema.
- `lint/gclplugin` registers the analyzers as the `kennlint` golangci-lint
  module plugin.
- `cmd/kennlint` runs analyzers, renders or checks configuration, and scans
  workflow downloads with `kennlint workflow-retries`.

See `docs/adopting-kennlint.md` for the consumer workflow.

## Invariants

- Every analyzer is type-aware through `go/analysis` and proves its behavior
  with `analysistest` fixtures under `testdata/`, including negative cases.
  Do not replace an analyzer with text matching.
- Analyzers report only in the file kinds they document (test files for
  `sleeptest`, `deadlinetest`, and `testifyhelper` helper recommendations,
  all Go files for `testifyhelper` canonical names, plus `testutil` and
  `*test` helper packages for `sleeptest` unless `helper-packages` is off;
  non-test files for `nohttpmux`, `errtext` unless `include-tests` is set).
  Path policy beyond that belongs in golangci exclusions, not in analyzer
  code.
- `sleeptest` and `deadlinetest` share one bubble detector: a bubble is the
  body of the function passed to `synctest.Test`, resolved by position for
  inline literals and by type object for functions passed by name. Helpers
  called from a bubble are reported on purpose; the fixture documents that
  limitation. `sleeptest`'s testify `Eventually` check stays off by default;
  consumers opt in per repository. `deadlinetest` checks testify `waitFor`
  budgets by default, but reports `Never` and `Neverf` only for zero or
  negative budgets: a short positive window cannot fail under load.
- `deadlinetest` checks duration fields and same-module parameters whose names
  have timeout, deadline, budget, or wait as a camelCase word, plus
  standalone `time.Now().Add` deadlines. Same-module helper calls from test
  files are covered; helper package source files aren't scanned.
  Intervals, ticks, and delays are cadences: a short one speeds a test up
  and cannot fail it, so they stay out of the vocabulary.
  `kennnolint` requires grouped kit suppressions to name their rules and reason.
- An analyzer that declares facts runs, with everything it requires, on every
  dependency: the standard library and third-party modules included. Fact
  analyzers must not require `buildssa` or other heavy passes; build what
  they need lazily, only in packages that can export a fact, as
  `sqlownership`'s `sqlscanners` does. `TestAnalyzersBuildSSAOnlyForLintedPackages`
  guards this, `BenchmarkAnalyzers` tracks per-analyzer cost on kit, and
  `kennlint profile` splits a consumer's run between its packages and its
  dependencies.
- `sqlscanners` builds SSA only for packages declaring an exported function
  with a parameter that can receive rows (`mayHoldRows`): a row type, a type
  converted from one, or any interface with `Err() error` except
  `context.Context`. Do not narrow interfaces by their other methods; a
  defined row type can add any method.
- Diagnostic strings are asserted in fixture `// want` comments; change both
  together.
- The canonical configuration must stay valid for the golangci-lint version in
  the kit `Makefile`, and `kennlint config` must render it without warnings.
  Adding a linter to the canonical config affects every consuming repository;
  measure the finding count on at least two consumers before adding one.
- Kit itself lints clean with the rendered configuration.
  Keep `deadlinetest` in the `kennlint` group. Enable `kennnolint` through
  kit's overlay while consumers adopt rule labels and reasons.
- `testifyhelper` requires canonical `assert` and `require` import and helper
  names. Requiring local helpers is opt-in through `require-helpers`, off
  by default for both libraries. Parent scopes retain package calls when local helpers would shadow
  package access in nested functions. Suggested fixes preserve object identity.
