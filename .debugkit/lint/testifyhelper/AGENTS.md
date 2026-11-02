# Testify Helper Analyzer Instructions

## Scope

`lint/testifyhelper/` contains the custom Go analyzer that reports tests
with repeated direct package-level testify calls. `cmd/kennlint/`
only wraps this analyzer for CLI use.

## Analyzer Rules

- Keep the analyzer type-aware through `go/analysis`; do not replace it with
  text matching.
- Keep each test body independent. Nested function literals should not make the
  outer function look more repetitive than it is.
- The analyzer should report repetition only when a local testify helper would
  make the test clearer; do not warn for one-off assertions.
- A helper is recognized by its initializer (`assert.New(t)` / `require.New(t)`
  for the body's own `t`), not by its variable name, so nested subtests can
  use a non-shadowing name.
- Existing diagnostic strings are asserted in testdata comments. If a message or
  threshold changes, update the `// want` comments in the same change.

## Tests

- Use `analysistest` fixtures under `testdata/`.
- Keep fixture packages minimal so failures point at analyzer behavior.
- Add both positive and negative fixture cases for threshold or scoping changes.
