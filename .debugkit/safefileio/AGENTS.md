# Safe File I/O Instructions

## Scope

`safefileio/` provides hardened file and directory helpers for local runtime
state that must belong to the current user. It is intentionally small. Keep
callers responsible for their own file formats and higher-level policy.

## Invariants

- On Windows, every Win32 call that takes a caller's path converts it with
  `internal/winpath.UTF16Ptr`, which adds the `\\?\` prefix to long paths as
  the os package does. Without it a path that `os.OpenFile` accepts past
  MAX_PATH fails here.
- Treat ambiguous paths as unsafe. Empty paths, symlinks, and non-regular files
  should fail before callers can write runtime state through them.
- Runtime directory validation should judge the directory entry the caller
  supplied, not a symlink target reached after traversal.
- `EnsurePrivateDir` may repair a real directory's permissions when that is safe.
  On Windows, it always rewrites the DACL and repairs inherited child
  permissions.
  `EnsurePrivateDirShallow` skips writes only on an exact protected DACL match;
  use it for directories whose children hold no data.
  `ValidatePrivateDir` must report problems without repairing them.
- Keep file ownership checks tied to current-user-only runtime state. On
  Windows, private directories may also be owned by LocalSystem or built-in
  Administrators only when their protected DACL restricts access to the same
  trusted user, token-owner, system, and administrator principals. Use SID
  semantics rather than username string comparisons.
- If ownership or file type cannot be established, return an error.
- Private-file validation must inspect the same open handle used by the caller
  and must never mutate permissions. Existing broad access may already have
  produced handles that no in-place repair can revoke.
- On macOS, capture ACL errno before returning to Go; locking an OS thread
  does not prevent the runtime from overwriting errno.
- On supported Unix platforms, require exact mode 0600 and no access ACL.
  Reject Linux network and user-space filesystems whose effective access policy
  cannot be verified through local mode and access-ACL operations, using
  `fsname.RemoteFile` (the list lives only in `fsname`), plus AppArmor's
  securityfs (`AAFS_MAGIC`), which is local but has its own access policy.
- On Windows, require a protected DACL that grants access only to the current
  user and trusted administrative principals. Callers recovering a broad or
  inheritable file must create a private replacement rather than repair it in
  place.
- `CreatePrivateFile` (and `CreatePrivateTemp`, which retries it under
  random names) is the sanctioned way to make a private file. It creates the
  file exclusively without following links, private from the moment it exists
  (Unix: `O_EXCL|O_NOFOLLOW` with mode 0600; Windows: `CREATE_NEW` with a
  protected, non-inheritable DACL in the creation security attributes), then
  validates the new handle with the same private-file checks. It must never
  open or repair an existing file: anything already at the path, including a
  dangling symlink or junction, fails with an error wrapping `fs.ErrExist`.
- When post-creation validation fails, `CreatePrivateFile` fails closed and
  deletes only the file it created: Windows marks the created handle for
  deletion. Unix cannot unlink by handle, so it pins the parent directory as
  an `os.Root` and does the identity check and removal through that handle.
  It removes only when the pinned parent is owned by the current user or
  root, denies group/other write (or is sticky while the created file is
  owned by the current user or root), is not on a network or FUSE file system
  (`fsname.RemoteFile`), and on macOS has no extended ACL. Otherwise it
  leaves the empty private file in place and says so in the error, because
  another user could swap the entry between the check and the removal.
- Platforms that cannot validate private files refuse in `CreatePrivateFile`
  before creating anything, since every create would fail validation.

## Tests

- Use testify for new or changed assertions.
- Keep Unix and Windows coverage in build-tagged test files.
- Permission tests may use fixed paths under the OS temp directory when
  `t.TempDir()` starts from permissions that hide the behavior under test.
- Clean up every path created outside `t.TempDir()`.
- Windows link cases: symlink tests skip on `ERROR_PRIVILEGE_NOT_HELD`, so
  every link guarantee also needs a junction case (`fslink.CreateJunction`,
  from an external `safefileio_test` file) that never skips.
