---
title: Vault Lifecycle
description: Operate a docbank vault safely from first import through maintenance, upgrades, snapshots, and recovery.
---

# Vault lifecycle

A docbank vault is deliberately low-maintenance: data commands start the
daemon when needed, imports never alter their sources, deletion is staged, and
integrity checks are explicit. This page connects those pieces into an
operating routine.

## Know what owns the vault

The daemon is the only process that opens `docbank.db` and the blob store.
Ordinary commands are HTTP clients of it and start a compatible background
daemon automatically.

```bash
docbank daemon status
docbank daemon start       # optional: data commands do this automatically
docbank daemon stop        # graceful; does not start a stopped daemon
```

Use `docbank daemon run` when diagnosing startup or configuration problems: it
stays in the foreground and writes logs to the terminal. Background logs live
under `$DOCBANK_HOME/logs/`.

## A practical operating rhythm

### As documents arrive

Import into `/inbox`, then file from there. Re-running an interrupted import is
safe: matching content already present under a destination candidate is
skipped.

```bash
docbank add ~/Desktop/scans
docbank tree /inbox
docbank mv /inbox/scans/tax-notice.pdf /taxes/2026/
```

The source files remain untouched. Keep them until your normal backup process
has captured the vault.

### Before removing anything permanently

Deletion and physical reclamation have separate gates:

1. `rm` moves a node to recoverable trash.
2. `trash empty` permanently removes old tree entries, but is a dry run unless
   `--run` is present.
3. `gc` reclaims unreachable loose bytes, but is also a dry run unless `--run`
   is present. Packed bytes become logically dead and await repacking; they are
   not reported as physically reclaimed.
4. `storage repack` rewrites eligible sparse packs and retires their old files.

There is no `rm --hard`, and none of these maintenance commands is scheduled
automatically today. Running `gc --run` immediately after `rm` cannot reclaim
the document because the recoverable trash entry still references it.

```bash
docbank rm /inbox/duplicate.pdf
docbank trash list
docbank trash empty --older-than 30d
docbank trash empty --older-than 30d --run
docbank gc
docbank gc --run
docbank storage status
docbank storage repack
```

Read each dry-run count before adding `--run`. If a document should return,
restore its numeric trash ID before emptying:

```bash
docbank restore 42
```

### On a schedule

Run `verify` after moving or restoring the vault, after storage trouble, and on
a schedule appropriate for the archive. It reads and hashes every cataloged
blob, so large vaults take time.

```bash
docbank verify
```

A clean result proves that each cataloged hash can be read and still matches
its content. It does not replace a backup: verification can identify missing
or corrupt bytes, but it cannot recreate them.

## Upgrade without daemon drift

Check first, then install when ready:

```bash
docbank update --check
docbank update
```

An install stops a running daemon, replaces the binary, and starts the new
daemon. If installation fails, docbank attempts to restart the old daemon.
`daemon start`, `daemon restart`, and command auto-start also replace a daemon
whose binary or API protocol is incompatible with the invoking CLI.

For unattended installation, use `docbank update --yes`; do not use `--force`
as a routine upgrade flag. It exists to bypass cached release metadata and to
allow replacing an unversioned development build.

## Take a coherent manual snapshot

Docbank provides `backup init`, `backup create`, `backup list`, and `backup
verify` for incremental capture and independent integrity proof in an
immutable Kit repository; see [Backup](backup.md). Those repositories are
compressed but **not encrypted**, and user-facing `backup restore` has not
landed yet.

A stopped-vault copy remains the complete manual recovery path while restore is
planned. Stop the daemon before copying so the SQLite database and blob catalog
cannot change during the copy.

```bash
vault="${DOCBANK_HOME:-$HOME/.docbank}"
docbank daemon stop
tar -C "$(dirname "$vault")" -czf "docbank-$(date +%F).tar.gz" "$(basename "$vault")"
docbank daemon start
```

The whole directory is the simplest snapshot. The essential archive state is
`docbank.db` plus `blobs/`; `config.toml` is worth retaining when customized.
Logs, lock files, and stale runtime records are not archive data.

Protect the snapshot like the vault itself: document contents and a configured
API key may both be present. Test restoration into a separate
`DOCBANK_HOME`, then run `docbank verify` there.

```bash
export DOCBANK_HOME=/tmp/docbank-restore-test
mkdir -p "$DOCBANK_HOME"
# Restore the archived vault contents into this directory.
docbank verify
docbank tree /
docbank daemon stop
```

Never point the restore test at the live vault, and never copy a snapshot over
a running vault. To replace a damaged vault, stop the daemon, preserve the old
directory under another name, restore the snapshot, and verify before resuming
normal use.

!!! info "Planned — backup restore"
    `docbank backup restore` will complete the built-in recovery workflow.
    Initialization, incremental capture, listing, and repository verification
    already exist for unencrypted repositories. See [Backup](backup.md).

## Move a vault

1. Stop the source daemon.
2. Copy the complete vault directory while it is stopped.
3. Point `DOCBANK_HOME` at the copy on the destination machine.
4. Run `docbank verify`, then `docbank tree /`.
5. Start using the destination only after both checks succeed.

The database refers to content by hash rather than absolute filesystem path,
so no path rewriting is required. docbank itself still requires macOS or
Linux; the Windows build contains lifecycle stubs but cannot open a vault.

## When something looks wrong

Stop destructive maintenance, keep the original vault directory intact, and
collect evidence before changing files by hand:

```bash
docbank daemon status --json
docbank verify
docbank daemon stop
```

Do not delete blob files, SQLite sidecars, lock files, or runtime records while
the daemon is running. Continue with [Troubleshooting](../troubleshooting.md)
for startup, import, integrity, and API failures.
