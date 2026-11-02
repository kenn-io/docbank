# fsname Instructions

## Scope

`fsname/` decides whether a single file or directory name is portable across
modern operating systems and file systems, and derives a portable name from an
outside string. `Check`, `Clean`, `Join` and `CheckPath` are lexical: they
never touch the file system. Link and open safety belong to `fslink/`.

`Remote` and `RemoteFile` are the only functions that touch the file system.
They report where the containing file system lives (network or user-space
FUSE versus local kernel), not whether a path is portable.

## Invariants

- `Check` rejects; it never renames. Use it where a caller supplies a name
  kit will create or look up. `Clean` and `Join` rewrite; use them only where
  the caller asks kit to derive a name from outside data.
- `Check(name)` succeeds exactly when `Clean(name) == name` and name is a
  single element other than "." or "..". Keep the two in lockstep.
- Do not use these functions on full host paths or on names restored from a
  backup: those must keep their exact names and use `filepath.IsLocal`.
- The rules come from `github.com/spf13/pathologize`. Names it misses
  (`CONIN$`, `CONOUT$`) are added here; remove an addition once pathologize
  covers it instead of keeping both.
- `Join` output must always satisfy `filepath.IsLocal`.
- `CheckPath` rejects Windows path forms that read differently from how they
  resolve: the device namespace (`\\.\`, `\??\`), drive-relative (`C:x`) and
  driveless rooted (`\x`) paths always; network shares, `\\?\` long paths and
  8.3 short names unless the caller opts in. Defaults refuse, so a caller
  who never considered a form does not accept it silently.
- Keep the Windows rules in `checkPath(path, windows, cfg)` so they are
  tested on every platform, not only in Windows CI.
- `Remote`/`RemoteFile` are best effort: they recognize common network and
  FUSE file systems, and an unrecognized one reports false. Do not grow
  them toward completeness. A platform with no detection method at all
  returns an error wrapping `errors.ErrUnsupported`. Callers
  holding private runtime state, lock files or sockets should refuse remote
  file systems; general data writes may allow them.
- The Linux network/FUSE magic list lives only in `linuxRemoteType`.
  `safefileio` reuses it through `RemoteFile`; do not copy it elsewhere.
- BSD and macOS treat a mount as remote when it lacks `MNT_LOCAL` or its type
  name is FUSE (FUSE mounts can carry `MNT_LOCAL`). Windows reports only
  `DRIVE_REMOTE` volumes; WinFsp-style user-space file systems read as local.

## Tests

- Use testify and table tests. Cover every reserved-name addition with and
  without an extension and in mixed case, and every CheckPath form both
  refused and, where an option exists, allowed.
- Remote tests must not depend on real network mounts: test the classifier
  functions with literal inputs and `t.TempDir()` as the local case.
