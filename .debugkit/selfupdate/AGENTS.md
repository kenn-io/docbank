# Self-Update Instructions

## Scope

`selfupdate/` checks GitHub releases, caches update metadata, verifies
downloaded assets, extracts archives, and installs a replacement binary. Treat
this package as security-sensitive because it moves bytes from the network to an
executable path.

## Invariants

- Never install an asset without a checksum.
- When signature verification is enabled, verify the checksum payload before
  downloading or installing the archive.
- Keep checksum, signature, release metadata, and asset-name validation tied to
  the update request. Do not let metadata from one asset authorize another.
- Archive extraction must not write outside the chosen destination or preserve
  privilege-changing file modes.
- `SanitizeArchivePath` decides locality with `filepath.IsLocal` on the
  platform path, not hand-rolled ".." string checks: names like `..foo` are
  valid, while empty names, Windows drive-relative, UNC, and reserved device
  names are refused. Keep the absolute-path containment check after it.
- Install through a staging path and atomic replacement where the platform
  allows it. Do not leave a half-written destination on ordinary failures.
- On Windows, backup, install, and rollback renames retry sharing violations
  for up to two seconds each through `internal/renamebusy`. Do not replace
  them with bare `atomicfile.Replace`.
- Keep cache-only update metadata from being installable; force a fresh check
  before install.
- Do not add live GitHub calls to tests. Use fake HTTP handlers and explicit
  responses.

## Tests

- Cover both success and refusal paths when changing verification, asset naming,
  caching, archive extraction, or install behavior.
- Use small in-memory or temp-file fixtures. Do not download real releases.
- Preserve tests that prove the archive extractor cannot write outside the
  destination directory.
