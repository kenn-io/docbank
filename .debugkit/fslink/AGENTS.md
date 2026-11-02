# fslink Instructions

## Scope

`fslink/` classifies, reads, and creates symlinks and Windows junctions, and
opens paths without following them. Keep it app-neutral: no caller policy,
file formats, or ownership rules belong here (see `safefileio/` for those).

## Invariants

- On Windows, every Win32 call that takes a caller's path converts it with
  `internal/winpath.UTF16Ptr`, which adds the `\\?\` prefix to long paths as
  the os package does. Without it a path that `os.OpenFile` accepts past
  MAX_PATH fails here. A symlink target gets the prefix only when it is
  absolute, as in `os.Symlink`; a relative target is stored unchanged.
- On Windows, "is a link" means a name-surrogate reparse tag
  (`tag & 0x20000000 != 0`), read from a handle opened with
  `FILE_FLAG_OPEN_REPARSE_POINT`. Do not treat every reparse point as a link:
  cloud-file placeholders, dedup, and WOF entries hold data in place. Do not
  rely on `os.Lstat` mode bits: since Go 1.23 junctions and cloud placeholders
  both report `ModeIrregular`.
- `IO_REPARSE_TAG_MOUNT_POINT` is `Junction` unless its substitute name is a
  `\??\Volume{...}` path, which is `OtherLink`.
- `Classify`, `IsLink`, `Readlink`, `OpenFile`, `OpenRegular`, `ReadFile`, and
  `OpenRoot` judge only the final path component; earlier components may be
  links. `OpenRoot` compares the opened root with the inspected directory by
  file identity, and must not block on a FIFO at the path.
- `OpenInRoot` and `OpenRootNoFollow` guarantee that every component they
  open is the same file object that was inspected (without following it) and
  found not to be a link, and that nothing resolves outside the root. Each
  component is inspected, opened through the `*os.Root`, and compared by file
  identity with the inspected entry. A concurrent swap to a link naming the
  same object passes the check and is harmless; do not document the functions
  as detecting every link. On Windows the inspection is relative to the parent
  root's own handle; never reopen by absolute path.
- On Windows the root-confined functions refuse every reparse point, not just
  name surrogates: `os.Root` opens each component with `OBJ_DONT_REPARSE`.
  Do not reimplement `os.Root` to lift this. `lstatAt` rejects a non-link
  reparse point up front with `errReparseInRoot`, which wraps
  `errors.ErrUnsupported`, so callers can classify it instead of seeing
  `ELOOP`.
- An exclusive create (`O_CREATE|O_EXCL`) on an existing entry, a link
  included, fails with `fs.ErrExist`, not `ErrIsLink`, in both `OpenFile` and
  `OpenInRoot`. Keep that precedence; it matches `os.OpenFile`.
- A link found when a guarded component is inspected is refused with an
  `*fs.PathError` wrapping `ErrIsLink`. A link swapped in after inspection
  may be resolved transiently during the open, but the result is accepted
  only when it is the object already inspected, and nothing is created or
  truncated before that check passes.
- Never truncate or create through a link: `O_TRUNC` is applied only after the
  opened handle is verified, and root-confined creation uses `O_EXCL`.
- `OpenRegular` must not block on FIFOs (Unix opens with `O_NONBLOCK`) and must
  reject anything but a regular file.
- `CreateJunction` must not require administrator rights or Developer Mode; it
  sets the mount-point reparse data itself. It creates the directory with
  `NtCreateFile(FILE_CREATE)` relative to a parent handle and sets the reparse
  data and any rollback (delete disposition) through that same handle; never
  reopen or remove the link by pathname. An existing entry fails with
  `fs.ErrExist`. `LinkDir` falls back to a junction
  only on `ERROR_PRIVILEGE_NOT_HELD`.
- On Windows, `OpenFile`, `OpenRegular`, and `ReadFile` inspect the final
  component with `FILE_FLAG_OPEN_REPARSE_POINT`, but a non-link reparse point
  must be reopened without that flag (and matched by file identity) before it
  is read or truncated; the flag bypasses the filter driver that serves the
  data of placeholders, dedup, and WOF files.
- `Readlink` succeeds only for `Symlink` and `Junction`. `OtherLink`,
  including volume mount points that `os.Readlink` could decode, returns
  `errors.ErrUnsupported` so the result matches the `Kind`.
- On Windows `O_APPEND` handles must lack `FILE_WRITE_DATA` so the OS appends
  every write (`os.NewFile` cannot set Go's append mode). `O_APPEND|O_TRUNC`
  needs that right to truncate, so after truncation the handle is replaced by
  a `ReOpenFile` without it, matched by file identity.
- On Windows, `LinkDir` always creates a directory symlink
  (`SYMBOLIC_LINK_FLAG_DIRECTORY`), even for a missing target; `os.Symlink`
  would silently create a file symlink. Its junction fallback resolves a
  rooted target without a volume against the link's volume.
- On platforms other than Unix and Windows, `Classify`, `IsLink`, and
  `Readlink` use `os.Lstat`/`os.Readlink` (these platforms have no
  junctions); every open and link-creating function fails closed with
  `errors.ErrUnsupported`, including `OpenInRoot(root, ".")` and
  `OpenRootNoFollow(root, ".")` (`platformSupport` is checked first).
- The Unix no-follow errno set is per OS (`nofollow_*.go`): NetBSD reports
  `EFTYPE` where others report `ELOOP` or `EMLINK`.

## Tests

- Windows CI may lack the symlink privilege: symlink cases skip on
  `ERROR_PRIVILEGE_NOT_HELD`, junction cases never skip.
- Symlink fixtures use relative in-tree targets so `os.Root` would follow them;
  otherwise the any-component tests would pass on `os.Root`'s own escape check.
