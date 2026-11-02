# Path Resolution Instructions

## Scope

`pathresolve/` owns filesystem path canonicalization for identity and
containment checks: the one place that knows a path has to resolve to the same
string no matter how the caller spelled it, and that not every Windows reparse
point is a symbolic link.

## Invariants

- For paths without reparse points, `EvalSymlinks` delegates canonicalization
  to `filepath.EvalSymlinks`, including volume case normalization. On Windows,
  clean parent components first, matching native path opening: `junction/../file` opens the lexical parent's file. Apply the
  same normalization to resolved link targets. `EvalSymlinksAllowMissing` keeps
  conservative link-before-parent traversal for containment checks; it does not
  define the native Windows identity contract.
- A path that contains a reparse point is resolved here, and that is the whole
  point of the package: a canonical path for identity or containment has to be
  the location the OS opens, not the spelling the caller used. Do not
  "simplify" this back to `filepath.EvalSymlinks`, and do not use this package
  where the requirement is to not follow a link.
- For `EvalSymlinks`, only Windows needs the extra pass. A directory junction is a reparse point
  tagged `IO_REPARSE_TAG_MOUNT_POINT`. Since Go 1.23 (`winsymlink` default,
  go#63703) `os.Lstat` calls it `ModeIrregular` — neither a symlink
  (`IO_REPARSE_TAG_SYMLINK`) nor a directory — while `os.Readlink` still reads
  its target, so `filepath.walkSymlinks` refuses to traverse one and every path
  below a junction fails with a bare `syscall.ENOTDIR` for a directory the OS
  opens without complaint. macOS and Linux express the same relocation as a
  real symlink, which `filepath.EvalSymlinks` already follows.
- Resolve reparse points before delegating, and prefer the fully resolved
  result whenever it canonicalizes. `filepath.EvalSymlinks` leaves a trailing
  junction unresolved today and refuses to traverse one below it, so resolving
  only some elements leaves a path that cannot be compared against its own
  root, and a containment check then reports a path as outside the root it is
  in.
- Never turn a real failure into a success-shaped answer. A path that does not
  exist, or that has a non-directory element, reports the error
  `filepath.EvalSymlinks` reports (after Windows lexical normalization). On
  Windows `syscall.ENOTDIR` is `Errno(3)`, so such an error also satisfies
  `errors.Is(err, fs.ErrNotExist)`; callers must not treat that as proof the
  path is absent.
- Bound reparse-point hops. A cycle (two junctions that refer to each other)
  reaches the bound, and the caller then gets whatever
  `filepath.EvalSymlinks` reports for the path it asked about — never a partial
  rewrite, and never a hang. Go cites mount-point recursion as its reason to
  treat them as links while walking; the bound is what keeps resolving them
  safe.
- Junctions are resolved, not rejected. Callers that must refuse a symlinked
  path for safety judge the directory entry they were handed; resolving a
  junction does not weaken that, and `safefileio` keeps its own stricter rule.
- `EvalSymlinksAllowMissing` is for containment checks before creation. Follow
  existing links on every platform, including dangling links, then retain the
  missing suffix. Resolve each link before applying later `..` components,
  including those in link targets; do not clean an unresolved path first.
  Reject cycles, unreadable paths, non-directory parents, and parent traversal
  after a missing component: a future link could change which parent it means.
  Keep `EvalSymlinks`'s existing missing-path error contract unchanged.
