---
title: Vault lifecycle
description: Operate a Docbank vault safely from first import through maintenance, upgrades, snapshots, and recovery.
---

# Vault lifecycle

To keep a vault recoverable, back it up, test restores, and check integrity
before you delete anything permanently. Data commands start the daemon when
needed, and you decide when to delete retained data and reclaim its storage.

## Know what owns the vault

In standalone CLI mode, the daemon is the only process that opens `docbank.db`
and the blob store. Embedded Go applications own separately rooted vaults
in-process and never share a root with a daemon. Ordinary CLI commands send
HTTP requests to the daemon and start a compatible background daemon
automatically. For ownership and startup rules, see
[Daemon](../architecture/daemon.md).

```bash
docbank daemon status
docbank daemon start       # reuse a compatible daemon or start until stopped
docbank daemon stop        # graceful; does not start a stopped daemon
```

To diagnose startup or configuration problems, run `docbank daemon run`. It
stays in the foreground and writes logs to your terminal, while background logs
live under `$DOCBANK_HOME/logs/`.

## A practical operating rhythm

### As documents arrive

Import new files into `/inbox`, then file them from there. You can safely
re-run an interrupted import. Docbank skips matching content already present
under a destination candidate.

```bash
docbank document add ~/Desktop/scans
docbank document tree /inbox
docbank document move /inbox/scans/tax-notice.pdf /taxes/2026/
```

Your source files stay untouched. Keep them until your normal backup process
captures the vault.

### Before removing anything permanently

Deletion and physical space reclamation use separate steps:

1. `document remove` moves a node to recoverable trash.
2. `trash empty` permanently removes old tree entries, and runs as a dry run
   unless you pass `--run`.
3. `vault gc` reclaims unreachable loose bytes, and also runs as a dry run
   unless you pass `--run`. Packed bytes become logically dead and wait for
   repacking; `vault gc` does not report them as physically reclaimed.
4. `storage repack` rewrites eligible sparse packs and retires their old files.

There is no `document remove --hard`, and none of these maintenance commands
runs on an automatic schedule today. Running `vault gc --run` right after
`document remove` cannot reclaim the document because the recoverable trash
entry still references it.

```bash
docbank document remove /inbox/duplicate.pdf
docbank trash list
docbank trash empty --older-than 30d
docbank trash empty --older-than 30d --run
docbank vault gc
docbank vault gc --run
docbank storage status
docbank storage repack
```

Read each dry-run count before you add `--run`. If you want a trashed document
back, restore it by its stable selector before emptying trash:

```bash
docbank document restore id:42
```

### On a schedule

Run `verify` after you move or restore the vault, after storage trouble, and on
a schedule that fits your archive. It reads and hashes every cataloged blob, so
large vaults take time.

```bash
docbank vault verify
```

A clean result proves that Docbank can read each cataloged hash and that its
bytes still match. Verification does not replace a backup. It can identify
missing or corrupt bytes, but it cannot recreate them.

## Upgrade the CLI and daemon together

Check for updates first, then install when you are ready:

```bash
docbank update --check
docbank update
```

Installing an update stops a running daemon, replaces the binary, and starts
the new daemon with the same lifetime. If the install fails, Docbank tries to
restart the old daemon. `daemon start`, `daemon restart`, and command
auto-start also replace a daemon whose binary or API protocol is incompatible
with the CLI you invoked.

If you installed Docbank with Homebrew, run `brew upgrade docbank` instead.
`docbank update` refuses to replace a binary owned by a package manager.

For unattended installation, run `docbank update --yes`. Do not use `--force`
for routine upgrades. It exists to bypass cached release metadata and to allow
replacing an unversioned development build.

## Take a coherent backup

Use `backup init`, `backup create`, `backup list`, `backup verify`, and
`backup restore` to capture and test recovery snapshots. Docbank reuses
unchanged content across captures. For the full procedure, see
[Backup and restore](backup.md). Backup repositories are compressed,
**not encrypted**.

### When a directory copy is enough

Copying a stopped vault captures its database and built-in primary store, not
blobs held only by secondary stores. The live reports can help you check how
much content the primary holds:

```bash
docbank vault info --json
docbank storage list --json
```

Find the store whose `role` is `primary`. You can compare its
`authoritative_objects` with `tracked_blobs` from `docbank vault info`, though
the two reports are separate live snapshots. Watches, ingests, clients, and
storage jobs can change either count between requests and before daemon
shutdown, so matching values do not prove a copy is complete.
`sole_authority_objects` is weaker still. A blob held by two secondaries and
absent from the primary is not a sole copy in either store.

For a complete recovery point, use `docbank backup create`. It reads one
verified candidate for every logical blob and fails rather than publishing a
partial snapshot. Copying `config.toml` preserves binding coordinates, not
secondary bytes. For the reporting boundary, see
[Understand primary coverage](storage.md#understand-primary-coverage).

If you need a local-state snapshot, stop every producer, watch, storage job,
and client first, then stop the daemon before copying so the SQLite database
and primary catalog cannot change during the copy. Do not use this procedure as
a complete backup when the vault has used secondary storage.

```bash
vault="${DOCBANK_HOME:-$HOME/.docbank}"
docbank daemon stop
tar -C "$(dirname "$vault")" -czf "docbank-$(date +%F).tar.gz" "$(basename "$vault")"
docbank daemon start
```

When the primary holds all content, copying the entire stopped vault directory
captures it. The stored archive contains `docbank.db` and `blobs/`. Keep
`config.toml` too if you have customized it. Logs, lock files, and stale
runtime records are not archive data.

Protect the snapshot like the vault itself. It may contain both document
contents and a configured API key. Test restoration into a separate
`DOCBANK_HOME`, then run `docbank vault verify` there.

```bash
export DOCBANK_HOME=/tmp/docbank-restore-test
mkdir -p "$DOCBANK_HOME"
# Restore the archived vault contents into this directory.
docbank vault verify
docbank document tree /
docbank daemon stop
```

Never point a restore test at the live vault, and never copy a snapshot over a
running vault. To replace a damaged vault:

1. Stop the daemon.
2. Preserve the old directory under another name.
3. Restore the snapshot.
4. Verify the restored vault before resuming normal use.

For routine recovery testing, it is better to restore the latest built-in
snapshot to a separate target and inspect it under a separate `DOCBANK_HOME`.
Restore verifies repository reads, SQLite integrity, and manifest statistics
before reporting success. Running `docbank vault verify` then exercises the
resulting live store through its normal daemon boundary.

## Move a vault

The portable way to move a vault is a verified backup restore:

1. Create and verify a snapshot repository.
2. Restore the snapshot into a separate target on the destination machine.
3. Point `DOCBANK_HOME` at that target.
4. Run `docbank vault verify`, then `docbank document tree /`.
5. Start using the destination only after both checks succeed.

A default restore places all recovered content in a fresh local primary store,
so the destination does not inherit source paths, endpoints, credentials, or
ownership epochs. Use an owner-private store mapping only when you want to
reconstruct selected placement. See
[Multi-store storage](storage.md#backup-and-restore).

If the primary holds every blob, you can also copy the stopped vault directory.
Stop the source daemon, copy the complete directory, and run the same two
checks. Linux, macOS, and Windows share the logical vault format. A copied
customized `config.toml` may contain machine-specific watched paths, secondary
bindings, or credential profiles. Review it before you start the destination
daemon.

## When something looks wrong

Stop destructive maintenance, keep the original vault directory intact, and
collect evidence before changing files by hand:

```bash
docbank daemon status --json
docbank vault verify
docbank daemon stop
```

Do not delete blob files, SQLite sidecars, lock files, or runtime records while
the daemon is running. For startup, import, integrity, and API failures,
continue with [Troubleshooting](../troubleshooting.md).
