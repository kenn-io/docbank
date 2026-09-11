---
title: Importing Documents
description: Import folders, preview large sources, retry partial imports, and keep changing files up to date.
last_edited: 2026-09-10
---

# Importing Documents

Use `docbank add` to copy files or entire folders into the vault. Docbank leaves
the originals unchanged. Repeat the same command after an interruption: it
skips matching content already imported under a destination name.

## What an import does

For each regular file, Docbank performs two steps:

1. Docbank computes the SHA-256 content hash while reading the file. It stores
   the bytes durably before adding database records. Identical content already
   stored in the vault is reused.
2. Docbank creates the file entry, its revision-one `content_create` version,
   the record of its stored content, and its provenance in one database
   transaction. Provenance records the original path and modification time;
   these facts survive later renames and moves.

See [Storage](../architecture/storage.md) for the content records and
[Editing & Versions](../architecture/editing-and-versions.md) for version identity.

Directory arguments walk recursively. The directory's basename becomes a
folder under `--dest`, and everything below keeps its relative structure:

```bash
docbank add ~/old-laptop/Documents --dest /archive
# → /archive/Documents/... mirrors the source tree
```

Trailing slashes and `./`-style paths are normalized; `add docs/` and
`add ./docs` behave identically to `add docs`.

An explicitly named source may be a symlink to a directory. This supports
ordinary platform layouts such as `~/Dropbox` on macOS: docbank resolves that
one root link, retains `Dropbox` as the virtual directory name, and records
provenance using the path the user supplied. Symlinks encountered *inside* the
tree remain skipped and reported, and an explicitly named symlink to a file is
not imported. Entries filtered by an include or exclude rule are excluded
without failure; selected non-regular entries are reported as failures.

## Preflight a large tree

Inventory a source before Docbank opens any file content or changes the vault:

```bash
docbank add ~/Dropbox --preflight \
  --include '*.pdf' \
  --exclude .git \
  --exclude .Trash \
  --exclude project/cache
```

The report separates files currently eligible for packing (through 64 MiB),
larger files that will remain individual stored files, and files above the
current format-v1 ingest ceiling. It also reports logical bytes, directory
count, skipped non-regular entries, filesystem errors, and the largest groups
by lowercase filename extension. Use `--json` for a structured, bounded report.

Preflight reads metadata only. It does not open cloud placeholders: file
entries whose contents still need to be downloaded from a provider. This lets
you estimate an import without downloading the whole tree. A successful scan
does not guarantee that the later import can read every file.

On macOS, `cloud placeholders` counts regular files whose bytes are not local,
including iCloud Drive and Google Drive for Desktop placeholders. These files
also count toward the report's size classes. Check this count before starting
an import that may require substantial downloading.

A provider may decline to hydrate a placeholder for the process that opens it —
a daemon started by launchd as a background job is the usual case, while an
interactive session succeeds. Docbank reports that failure for the individual
file, names the cause, and suggests opening the file once from a user session
(or marking it available offline) before retrying the import. Re-run preflight
after changing selection, then pass the exact same `--include` and `--exclude`
flags to the real `docbank add` command.

Filesystem names and provenance paths must currently be valid UTF-8. On POSIX
filesystems that permit other byte sequences, preflight and ingest report each
such entry with an escaped, printable path; Docbank does not open or import it,
continues with the rest of the tree, and never alters the source.

### Choose files with include and exclude rules

Use include rules to select files and exclude rules to skip files or whole
subtrees. Exclusions win. Include rules leave directories open for traversal.

| Rule | Matches |
|------|---------|
| `*.pdf` | A basename at any depth |
| `reports/*.pdf` | A path relative to each source root |
| `cache` in `--exclude` | Entries named `cache`, including entire matching directory subtrees |
| `report[[]1].txt` | The literal filename `report[1].txt` |

Rules use Go's `path.Match` grammar. `*` and `?` do not cross `/`, and `**`
does not mean recursive matching. Use `/` separators on every platform;
backslashes are rejected. Use bracket expressions to match a literal `[`, `?`,
or `*`. Matching is case-sensitive, including on Windows.

Repeat flags for multiple rules; commas are literal characters. Empty rules,
absolute paths, parent traversal, and malformed patterns are rejected before
the walk. Watched-inbox exclusions remain literal and do not use this glob syntax.

When the source argument is one explicit file, a basename rule such as `*.pdf`
matches it; a path-form rule such as `reports/*.pdf` applies to a directory
source's relative paths.

## Label and browse one import run

The HTTP ingest body can attach an optional label to the logical run. The
label publishes atomically with the first committed document observation:

```bash
curl -sS -X POST -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"paths":["/srv/import/review"],"dest":"/archive","collection_label":"Review set"}' \
  http://127.0.0.1:43210/api/v1/ingest
```

For streamed progress, send the same field to the streaming route:

```bash
curl -sS -N -X POST -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"paths":["/srv/import/receipts"],"dest":"/archive","collection_label":"Receipt batch"}' \
  http://127.0.0.1:43210/api/v1/ingest/stream
```

The terminal report includes `ingest_id` when at least one file committed. Use
that ID with `GET /api/v1/collections/{id}` and
`GET /api/v1/collections/{id}/members`; list current nonempty runs with
`GET /api/v1/collections`. Collection membership follows the documents as they
move within the vault and reports current paths, sizes, and versions. Trashed
files and superseded provenance disappear from live membership. Caller-supplied
`embedded:` provenance never creates a collection.

The label has a separate ETag and edit route. Read
`GET /api/v1/collections/{id}/label`, then PUT exactly `{"label":"New name"}`
or `{"label":null}` to the same path with its quoted revision in `If-Match`.
Non-null labels stay unique even while a collection is empty. After permanent
audit is enabled, label changes fail with HTTP 409 and
`audit_mutation_unsupported`; the existing label and collection remain
readable.

If only some files succeed, the receipt names the real run and lists failures
beside it. If nothing commits, the receipt omits `ingest_id` and no collection
or label is created. Label collisions and audit restrictions on an initial
label use this same per-file failure list instead of turning the whole batch
into one transport error.

## Follow a long import

Human-mode `docbank add` performs a metadata-only scan to establish file and
byte totals, then reports content-read progress while it imports:

```bash
docbank add ~/Dropbox --dest /archive --progress plain
```

`auto` (the default) draws a progress bar on a terminal and emits durable
periodic lines when stderr is redirected. `plain` always emits durable lines;
`bar` forces the redrawable form. Progress belongs on stderr and the terminal
summary belongs on stdout. Use `--json` to suppress progress and emit only the
machine-readable terminal report.

The scan totals are an estimate rather than a filesystem lock: a source may
change before Docbank opens it. Byte progress counts content actually read,
while a file counts as done only after its individual blob and metadata
operation returns. Interrupting the command cancels the daemon request.
Docbank keeps files that completed successfully and skips them on a rerun. It
does not create a file entry for an incomplete import.

## What happens when I run the import again?

Interrupted a 200,000-file import? Run the same command again. For each
source file, docbank walks the candidate names in the destination
directory — `report.pdf`, `report (2).pdf`, `report (3).pdf`, … — and:

- if any live candidate has the **same content**, the file is counted as
  `skipped` (already imported, even if a prior run imported it under a
  suffix);
- if all existing candidates have different content, the next free
  suffix is used;
- otherwise the first free candidate name is taken.

Repeating the same source import does not create extra copies of its entries.
Two differently named source files with identical bytes still import as two
file entries. They share stored content but have distinct version UUIDs.

Each explicit filesystem re-run is still a new logical ingest run. When bytes
already match a destination node, Docbank adds that existing node to the new
run without creating a content version. Recording this new membership advances
the node revision, even without a collection label or any flags. This also
applies to `--replace` when the bytes are identical. API clients holding the
old ETag must refresh it before their next write; otherwise `If-Match` returns
`412 stale_revision`. Repeating the same observation within one run is a no-op.

Digest-checked `POST /uploads` retries keep their existing behavior: an equal retry returns the existing node with an unchanged revision
and does not return an unused ingest identity.

## Collisions

Two different files arriving at the same virtual name don't conflict —
the newcomer is suffixed (`scan.pdf` → `scan (2).pdf`). The provenance
record preserves where each one actually came from.

## Replace a changing local file

Use `--replace` when a repeated local add represents one changing source:

```bash
docbank add ~/reports/summary.pdf --dest /archive --replace
```

Docbank resolves the exact destination name and records its node revision
before reading the source. Different bytes become a new content version on the
same node. Unchanged bytes count as skipped and keep the stored MIME type and
version history, while the new run membership advances the node revision as
[described above](#what-happens-when-i-run-the-import-again). A live directory
fails that file before source content is opened. If an absent destination is
claimed while the source is read, the exact create reports a conflict and never
chooses a suffix. A stale observed revision reports a conflict and leaves the newer
content current. Omit `--replace` for ordinary collision suffixing.

## Inspect where a document came from

Docbank retains the facts about where a document came from as provenance.
Query a live file by vault path or any retained file by stable node ID:

```bash
docbank provenance /archive/Documents/report.pdf
docbank provenance id:42 --json
```

The bounded newest-first result identifies the ingest, its source kind and
description, the original source path and modification time, and the immutable
SHA-256 identity of each provenance fact. `active` means no newer fact
supersedes it; corrections retain earlier facts instead of rewriting them.
Because a source path can disclose machine-local names, provenance is available
only through the same authenticated API as the document itself.

Reading provenance does not open or change the original file. A provenance
record also does not prevent ordinary retention or deletion rules from removing
a document version.

Applications can append an origin learned later through the
[HTTP API](../architecture/http-api.md#content-identity-and-verification-evidence)
or [embedded Go API](../embedding.md). They can correct an active
caller-supplied fact by adding a new fact that supersedes it. CLI and watched
ingest facts cannot be superseded because Docbank uses them to recognize
repeated imports; append an additional origin instead. The `provenance` CLI
command reads this history.

## Failures don't abort the batch

Unreadable files, permission errors, and non-regular files (symlinks,
sockets, devices) are recorded and reported at the end; the rest of the
import continues. A directory that can't be created in the tree (for
example, its virtual path collides with an existing file) skips that
subtree and continues with the next.

```
added: 4211  skipped: 12  failed: 2
failed: /src/broken.pdf: opening /src/broken.pdf: permission denied
failed: /src/link.pdf: not a regular file or directory (symlinks are skipped)
```

The exit code is non-zero when any file failed, so scripted migrations
can detect partial imports. A missing or unreadable top-level source is
reported the same way, and the command continues with the remaining
source arguments.

## Sources are read-only

Import never deletes or modifies source files, including a followed root
directory symlink. Before deleting originals yourself, run `docbank verify`,
spot-check the imported documents, and capture a [backup](backup.md).

## Remote API imports

Authenticated integrations can send one digest-checked file at a time through
`POST /api/v1/uploads`. The server requires the writer's SHA-256 and byte length,
computes both independently while streaming, and creates no node or blob
authority when either differs. See the [HTTP API](../architecture/http-api.md#addendum-post-uploads)
and [Agent Integration Guide](../agents/integration.md#create-and-ingest-safely)
for the exact contract.

Backups include collection labels, their revision fences, run membership, and
the audit history for repeated operational observations. Older readers that do
not understand this added authority reject such a snapshot explicitly instead
of restoring it without the collection records.

## Continuously ingest a local inbox

For directories that receive files over time, configure a daemon-owned
`[[watch]]` entry instead of repeatedly running `docbank add`:

```toml
[[watch]]
name = "agent-sessions"
source = "~/agent-sessions"
destination = "/archives/agents"
settle_time = "30s"
minimum_age = "168h"
scan_interval = "5s"
exclude = ["cache/", ".DS_Store"]

[storage]
pack_interval = "1h"
pack_max_bytes = 268435456
```

The daemon observes a file's filesystem identity, size, and modification time
for a full settle window before reading it. `minimum_age = "168h"` additionally
requires seven days since the source's last modification, which is useful when
an append-heavy session may pause for minutes or hours without being closed.
The minimum-age gate survives restart; the settle observation deliberately does
not, so every file still proves a complete unchanged window in the new daemon.
Set `minimum_age = "0s"` or omit it for ordinary inboxes that need only the
settle window.

Docbank then verifies that the confined source path still names the same
unchanged object. It never follows entries that are symlinks and never changes
or deletes source data. A time window cannot prove that a producer formally
closed a file, so use a conservative age or point the watch at a completed-file
handoff directory when one is available.

The watch name and slash-separated relative source path form a stable,
portable provenance identity. The first stable observation creates the file
under `destination`; later byte changes append immutable content versions to
that same stable node. This remains true if a person or agent moves or renames
the Docbank node after ingestion. Docbank remembers the last content accepted
from the source independently of the node's current version, so an unchanged
source does not overwrite a later edit or revert after daemon restart. Removing
the source leaves the archived node alone. A renamed source-relative path is a
new identity, not an implicit move.

For an agent-session archive, use the source tree itself for the organization
you want to retain. For example,
`~/agent-sessions/codex/project-alpha/2026/07/session-01.jsonl` becomes
`/archives/agents/codex/project-alpha/2026/07/session-01.jsonl` with the
configuration above. Docbank does not reinterpret a vendor's session format:
the relative path and source facts remain exact, while each accepted byte
change becomes an immutable version of that document.

Use `docbank provenance <path-or-id>` to inspect the retained watch identity,
source-relative path, and immutable supersession history for an imported node.
JSONL session content up to the normal extraction limit is indexed by the
built-in plain-text worker, so ordinary `docbank search` can find archived
session text without a vendor-specific parser. The optional `[storage]`
schedule packs accumulated small files with a finite per-run budget. It does
not delete source files, prune versions, run GC, or rewrite existing packs.
Portable [backup and restore](backup.md) preserve the mirrored hierarchy,
source provenance, every retained version, and its verified bytes.

The watcher uses the exact destination name; it does not add a collision
suffix. It stops with an error if unrelated content already occupies that path
or the previously mapped node is in trash. `docbank jobs` reports the
named `watch:<name>` job and any terminal error; correct the problem and restart
the daemon. Successful additions, updates, and unchanged observations appear
in the daemon log. See [Configuration](../configuration.md#watched-inboxes) for
the complete field contract.
