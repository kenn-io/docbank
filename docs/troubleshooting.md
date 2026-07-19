---
title: Troubleshooting
description: Diagnose docbank startup, daemon, import, integrity, update, and HTTP API failures without risking the vault.
---

# Troubleshooting

Start with observation, not cleanup. Keep the vault intact until you know
whether the problem is configuration, daemon lifecycle, source-file access, or
stored data.

```bash
docbank daemon status --json
docbank verify
```

If a command prints a specific error, preserve it. CLI errors go to stderr and
return a non-zero status; HTTP errors include a machine-readable `code`.

## The daemon will not start

Run it in the foreground to see the startup error directly:

```bash
DOCBANK_LOG_LEVEL=debug docbank daemon run
```

Common causes:

- `config.toml` contains an unknown key, invalid duration, or non-loopback
  bind address. The daemon rejects these rather than guessing.
- Another process owns the same vault. Check `docbank daemon status`; do not
  remove `vault.lock` while a daemon may still be alive.
- The configured port is already in use. Set `api_port = 0` to let the OS
  choose one, or choose another fixed port.

Background daemon logs are JSON files under `$DOCBANK_HOME/logs/`. A data
command normally repairs stale runtime records and replaces an incompatible
daemon automatically; `daemon status` and `daemon stop` intentionally do not
start anything.

## A command cannot connect

First ask the CLI to converge the daemon explicitly:

```bash
docbank daemon restart
docbank daemon status
```

If restart fails, use foreground mode. If it succeeds but a raw HTTP client
still fails, confirm that the client uses the daemon's actual address and
effective API key. With `api_port = 0` or an empty configured key, both change
when the daemon restarts; the docbank CLI discovers them automatically, but an
independent client does not.

For a stable integration, configure a fixed loopback port and API key as shown
in the [Agent Integration Guide](agents/integration.md).

## Import completed with failures

`docbank add` continues past unreadable files inside a directory and reports
each failure. Its exit status is non-zero if any file failed, even when other
files were added successfully.

Fix source permissions or availability, then rerun the same command. Already
imported content is skipped, so a rerun converges instead of duplicating the
successful portion.

```bash
docbank add ~/Documents/archive --dest /imports
```

A missing or unreadable top-level argument is reported as a failed source, and
the command continues with any remaining arguments. Symlinks and non-regular
files are intentionally skipped; import the regular target file explicitly if
it belongs in the vault.

## Search cannot find document text

Search currently indexes live node names, not document contents. Try terms
from the filename and remember that each term is a prefix match.

```bash
docbank search insur stat
```

PDF, office-document, and plain-text extraction are not available. See
[Searching](usage/searching.md) for the current name-search contract.

## A move or restore conflicts

The CLI never overwrites a live file. Moving onto an existing file returns
`name already exists`; choose another destination or move into an existing
directory to retain the source name.

Restore handles name reuse automatically by adding a numeric suffix. If its
original parent was permanently removed, it restores at `/`. Use the path
printed by `docbank restore` rather than assuming the old path returned.

HTTP clients must also distinguish:

- `409 exists` or `cycle`: the requested tree state is invalid; choose a new
  action.
- `412 stale_revision`: another mutation won; re-read the node and reconsider
  the action before retrying.
- `428 precondition_required`: read the target node or tag again and send its
  current revision in `If-Match`.

## Verify reports a problem

`verify` classifies each affected hash:

| Result | Meaning | First response |
|--------|---------|----------------|
| `missing` | The catalog references bytes that cannot be found. | Stop maintenance and locate a known-good snapshot. |
| `corrupt` | Bytes are readable but no longer match their SHA-256 identity. | Preserve the vault and restore that content from a known-good copy. |
| `unreadable` | The storage layer returned an I/O or permission error. | Check mounts, permissions, and system logs before retrying. |

Do not run `gc --run` as a repair tool. GC removes unreachable data; it does
not repair referenced content. Preserve the affected vault, record the full
`verify` output, and restore from a snapshot only after identifying which copy
is known-good.

## GC reclaimed fewer bytes than expected

Packed blobs are immutable members of pack files. GC can remove their catalog
authority, but that only makes their stored ranges logically dead; physical
pack space remains until `docbank storage repack` selects and retires the
sparse source pack. The report separates
`pending_packed_bytes` from loose bytes actually reclaimable so logical
deletion is not presented as disk reclamation.

## Update fails

`docbank update` requires a published release with a matching SHA-256 checksum.
It refuses unverifiable assets. An install failure attempts to restart the old
daemon; inspect `docbank daemon status` afterward and use `daemon run` if it is
not healthy.

`--force` does not bypass checksum verification. It refreshes release metadata
and permits replacing an unversioned development build.

## HTTP returns 401

Every `/api/v1` request requires an effective API key, even when
`api_key = ""` in config—the empty setting means “generate a per-run key.” Use
exactly one of:

```text
X-Api-Key: <key>
Authorization: Bearer <key>
```

The health check, ping, interactive API docs, and OpenAPI documents are
auth-exempt. A successful `/health` response therefore proves reachability,
not authenticated access.

## Before asking for help

Capture the version, daemon status, failing command or request, and relevant
log lines. Remove API keys, shutdown tokens, document contents, and private
paths before sharing them.

`docbank version` is newer than v0.7.0; use a source build until the next
release is published.

```bash
docbank version
docbank daemon status --json
```
