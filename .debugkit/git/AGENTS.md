# Git Package Instructions

## Scope

The `git/` tree provides reusable helpers for developer tools that inspect and
mutate Git repositories. Keep these packages about Git mechanics rather than a
specific application or forge workflow.

## Package Rules

- Prefer `gitcmd.New()` for Git subprocesses so callers get consistent
  environment and prompt handling.
- Do not call `exec.Command("git", ...)` directly in package code unless the
  direct call is the behavior being tested.
- Pass `context.Context` through Git operations that can block.
- Runner copies share trust caching for the first observed identity. Evaluate
  includes fresh and apply trust edits on the next command. Use native scoped
  reads whenever `GIT_CONFIG` is set, including an empty value.
  Use `WithEnv` to give a changed environment its own cache. The managed
  lifecycle sets its C diagnostic locale before populating that cache and
  retains the normalized runner across nested operations.
- Cancel the whole child process tree for non-interactive Git commands, and
  bound pipe draining after cancellation. Interactive Unix Git must stay in
  the caller's terminal process group.
- Return Git failures with captured stderr. Do not hide Git's message behind a
  generic error.
- Keep remote and clone-path parsing in `gitremote`; do not duplicate it in
  sibling packages.
- Do not assume GitHub-only identity. Keep host, owner, repository name, and
  provider-specific merge-request refs explicit.
- `git/managed` owns the shared named-worktree lifecycle. Extend it instead of
  creating application-local worktree creation, merge-request import,
  tracking, hook, or rollback implementations.
- The managed lifecycle trusts the existing repository, remotes, Git
  configuration, provider metadata, lifecycle hooks, and same-user filesystem
  state. This includes configured remote push URLs and refspecs. Ordinary
  named-worktree creation also trusts the checked-out tree.
- Merge-request import treats the fetched tree as untrusted: keep checkout-time
  hooks disabled, neutralize configured filter/fsmonitor/diff/merge programs
  the tree can select, disable implicit submodule recursion, and persist those
  settings in the imported worktree. This is a narrow Git execution boundary,
  not an OS sandbox for hostile configuration, remotes, lifecycle scripts,
  same-user replacement races, or resource exhaustion.
- Replacement merge drivers for untrusted merge-request imports must run the
  resolved Git executable without looking it up through the worktree `PATH`.
  They must clear inherited repository bindings and counted configuration,
  classify binary inputs without repository attributes or external
  diff/textconv helpers, and pin `core.bigFileThreshold=1023m` to match
  `merge-file`'s maximum text size. They must write clean text merges and diff3
  markers for text conflicts, and treat classified binary content as an
  ordinary per-file conflict. Classifier failures, text-merge I/O failures, a
  missing Git executable, or a merge-process crash must fail the whole
  operation. This contract requires Git 2.42.0+ on non-Windows and Git for
  Windows 2.53.0.windows.3+. Keep both platform behaviors explicit.
- Reject isolation-sensitive command-scope configuration during import because
  worktree configuration cannot outrank it. Explicit command-scope overrides
  on later Git commands are caller policy, not a sandbox boundary Kit can
  enforce.
- Reject config selectors and includes into an isolated checkout before creating
  it, even when their files do not exist yet. Deferred materialization must not
  turn tracked files into newly active Git configuration. Resolve dangling links
  with `pathresolve.EvalSymlinksAllowMissing` before checking containment. Keep
  parent components intact until then, including explicit selectors and raw
  HOME/XDG default paths that `git var` would otherwise clean. An explicit
  global selector replaces those defaults. Refuse parent traversal through missing
  components rather than guessing how future tree links will resolve. Compare
  resolved ancestor identities for case aliases, and check again after
  registration, before materialization, when the directory identity exists.
- The default lifecycle-hook runner is for trusted native executables. Callers
  that need process-tree supervision or cross-platform script dispatch must
  supply `RunHook`; do not grow those application policies into this package.
- Worktree base permissions follow the caller's path and host umask. Owner-only
  directory policy and platform ACL management belong to the application.
- Expected merge-request head SHAs are correctness anchors: verify them before
  creating the local branch or materializing a worktree.
- Rollback after a completed create is conservative about ordinary user work:
  preserve a dirty worktree, an initialized submodule, or an advanced branch
  and report `ErrWorktreeCleanupIncomplete`. Fresh-owned rollback may discard
  setup changes in the acquired checkout, but both policies require matching
  private directory and registration evidence. Preserve artifacts if evidence
  is incomplete. Never delete an acquired branch after it advances, or continue
  branch cleanup after failed checkout removal. Report partial removal effects.
  After a branch deletion error, inspect its effect with a bounded context
  independent of caller cancellation, retain the error, and stop further cleanup.
  Capture directory identities at acquisition time: Windows `os.Stat` can defer
  reading file IDs until `os.SameFile`, after a path has already been replaced.
  An untouched deferred checkout has no index and contains only its `.git`
  registration file; default rollback must recognize that initial state without
  treating missing tracked files as edits. Preserve actual file or index changes.
  Moving a checkout keeps that deferred state, and a move's ProjectRoot must not
  move with its source: Git runs there for move evidence and later rollback.
  A successful Git exit with an output-capture error still acquired the worktree:
  capture ownership evidence, retain the error, and apply the selected cleanup
  policy. Capture evidence after confirmed success with a bounded context
  independent of caller cancellation, then report cancellation and apply cleanup.
  A failed or unknown process outcome grants no cleanup authority.
- Isolated creation disables configured Git hooks with command-scoped settings
  before creating refs, then persists those settings in the new worktree.
  Re-read configuration after attachment to cover branch-conditional hooks.
  Merge-request fetches and temporary-ref cleanup use the same hook policy.
  Keep the original checkout's hook configuration unchanged.
- Moving a linked checkout returns acquisition evidence for the new destination.
  Explicit rollback removes that checkout and preserves its preexisting branch;
  it never silently moves it back. A requested new branch is acquired during the
  prepared checkout and may be removed only while unchanged. After a failed move,
  retain evidence at the source only when that acquired checkout remains there.
  Preserve and report an unknown move outcome.
- Configure merge-request tracking in worktree-scoped Git configuration so
  removing a worktree does not leave branch routing behind in shared config.
  Explicit upstream policy chooses configuration scope and whether to configure
  push routing. Leave must make no tracking writes; choose policy before create.
  Ordinary creation preserves Git's default tracking even with isolated checkout;
  merge-request import chooses an explicit tracking action before creation.
  Resolve remote URLs and fetch mappings from the destination worktree's effective
  configuration; a linked worktree can define its own remotes.
  Explicit conditional import tracking fetches the requested remote branch
  from the registered, isolated destination before comparing tips. Branch- and
  directory-conditional config can select its remotes; this also applies before
  deferred checkout. Honor custom fetch mappings and refresh stale cached refs.
  Fetch failures after acquisition use the caller's cleanup policy.
  Leave/Clear do not fetch a tracking target; default fork tracking remains
  best-effort when the fork is unavailable.
  Git combines branch.merge across scopes: worktree scope preserves compatible
  shared routing without duplicating it, and rejects conflicting inherited
  routing before changing config. Clearing shared routing requires repository
  scope. Do not mask inherited merge refs with empty values. Reject routing from
  other config files or command options instead of changing those sources.
  ConfigurePush always writes push.default in worktree config, even with
  repository-scoped branch routing, so other worktrees keep their push behavior.
  Reject conflicting command overrides before changing branch routing.
- Lifecycle hooks must resolve inside the project tree. Applications may
  supply Git and hook runners to retain their process limits and
  platform-specific execution policy. Prepared hooks retain the same validation
  and environment while allowing execution outside application repository locks.
  Configuring execution limits alone must not change inherited Git settings.

## Tests

- Use `git/test` fixtures or temporary local repositories instead of the
  user's repositories.
- Tests must not read or mutate global Git config. Set needed identity and
  configuration inside the fixture.
- Prefer testify assertions for new and changed checks.
