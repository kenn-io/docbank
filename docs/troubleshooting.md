---
title: Troubleshooting
description: Diagnose Docbank startup, daemon, import, integrity, update, and HTTP API failures without risking the vault.
---

# Troubleshooting

Check daemon status and verify the vault before you change any files. Keep the
vault intact while you find out whether the failure comes from configuration,
the daemon, source-file access, or stored content.

```bash
docbank daemon status --json
docbank vault verify
```

If a command prints a specific error, save it. CLI errors go to stderr and
return a non-zero status, while HTTP errors include a machine-readable `code`.

## The daemon does not start

Run the daemon in the foreground to see the startup error directly:

```bash
DOCBANK_LOG_LEVEL=debug docbank daemon run
```

Common causes:

- `config.toml` has an unknown key, an invalid duration, or a non-loopback bind
  without an explicit key. The daemon rejects these settings.
- Another process owns the same vault. Check `docbank daemon status`, and do
  not remove `vault.lock` while a daemon may still be alive.
- The configured port is already in use. Set `api_port = 0` to let the OS pick
  a port, or choose a different fixed port.

Background daemon logs are JSON files under `$DOCBANK_HOME/logs/`. A data
command normally repairs stale runtime records and replaces an incompatible
daemon automatically, while `daemon status` and `daemon stop` do not start a
daemon.

## A command cannot connect

Restart the daemon, then check whether it is running:

```bash
docbank daemon restart
docbank daemon status
```

If restart fails, run the daemon in foreground mode. If restart succeeds but a
raw HTTP client still fails, check that the client uses the daemon's current
address and effective API key. With `api_port = 0` or an empty configured key,
both change whenever the daemon restarts. The `docbank` CLI discovers them
automatically, whereas an independent client does not.

For a stable integration, configure a fixed loopback port and API key as shown
in the [Agent integration guide](agents/integration.md).

## Import completed with failures

`docbank document add` continues past unreadable files inside a directory and
reports each failure. Its exit status is non-zero if any file failed, even when
other files were added successfully.

Fix the reported source permissions or availability problem, then run the
command again. Docbank skips matching content already imported under a
destination name, so the completed files do not need another entry.

```bash
docbank document add ~/Documents/archive --dest /imports
```

If a top-level argument is missing or unreadable, Docbank reports it as a
failed source and continues with any remaining arguments. Docbank does not
import non-regular files or file symlinks. If a regular target file belongs in
the vault, name that target file itself. A source argument can be a symlink to
a directory, though links inside that directory are still skipped and reported.
See [Importing documents](usage/importing.md).

## Search cannot find document text

An empty CLI search prints hints on stderr explaining why: files still being
indexed, files with no searchable text, or PDF pages that need OCR
(`needs_ocr`). Read those hints first, then check the background jobs:

```bash
docbank search "lease renewal"
docbank jobs
```

Docbank indexes UTF-8 text, Markdown, JSON, and JSONL up to 16 MiB, and the
text layer of PDFs up to 500 MiB and 5,000 pages. Indexing runs in the
background and may take a moment after you add a file. Scanned PDF pages,
office documents, and images need a processing profile, while filename search
still works for every file. See
[Why did a text search find nothing?](usage/searching.md#why-did-a-text-search-find-nothing)
and [how words match](usage/searching.md#how-do-words-match-in-lexical-search).

The running extractor keeps transient blob open, read, and verification
failures queued and retries them. Repair or restore unavailable content, then
retry search after the worker finishes a verified read.

## A move or restore conflicts

The CLI never overwrites a live file. Moving onto an existing file returns
`name already exists`. Choose another destination, or move into an existing
directory to keep the source name.

Restore handles name reuse automatically by adding a numeric suffix. If the
original parent was permanently removed, Docbank restores the item at `/`. Use
the path printed by `docbank document restore` rather than assuming the old
path returned.

HTTP clients must also distinguish these responses:

- `409 exists` or `cycle`: the requested tree state is invalid. Choose a new
  action.
- `412 stale_revision`: another mutation won. Re-read the node and reconsider
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

Do not run `vault gc --run` as a repair tool. GC removes unreachable data and
does not repair referenced content. Keep the affected vault intact, save the
full `verify` output, and restore from a snapshot only after you identify which
copy is known-good.

## GC reclaimed fewer bytes than expected

A pack stores several blobs in one file that Docbank does not edit in place.
GC can remove the database references to an unused blob, while its bytes remain
inside the pack. `docbank storage repack` rewrites eligible packs with only the
content still needed and retires the old files.

Check `pending_packed_bytes` in the GC report. It counts packed bytes waiting
for repack, separately from individual files that GC can remove right away.
See [Trash, garbage collection, repack, and verify](usage/trash-and-gc.md).

## Update fails

`docbank update` requires a published release with a matching SHA-256 checksum
and refuses unverifiable assets. If the install fails, it tries to restart the
old daemon. Check `docbank daemon status` afterward, and run `daemon run` if
the daemon is not healthy.

`--force` does not bypass checksum verification. It refreshes release metadata
and allows replacing an unversioned development build.

## HTTP returns 401

Every `/api/v1` request requires an effective API key, even when
`api_key = ""` in config. The empty setting means "generate a per-run key."
Use one of these headers, not both:

```text
X-Api-Key: <key>
Authorization: Bearer <key>
```

The health check, ping, interactive API docs, and OpenAPI documents do not
require authentication. A successful `/health` response shows that the server
is reachable, not that your key is valid. For the request contract, see
[HTTP API authentication](architecture/http-api.md).

## Before asking for help

Capture the version, daemon status, failing command or request, and relevant
log lines. Remove API keys, shutdown tokens, document contents, and private
paths before you share them.

```bash
docbank version
docbank daemon status --json
```
