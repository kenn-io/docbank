---
title: Importing documents
description: Import folders, preview large sources, retry partial imports, and keep changing files up to date.
last_edited: 2026-10-31
---

# Importing documents

Use `docbank document add` to copy files or entire folders into the vault while
leaving your originals unchanged. If an import is interrupted, run the same
command again; Docbank skips files that were already imported from the same
source filename with matching content in that destination directory.

## Mailbox archives

To import mailbox exports, run `docbank email mailbox import` or select
**Import mailbox** in the web app. Both keep the original archive intact and
give each message occurrence its own document, even when content or a
Message-ID repeats. Docbank publishes decoded attachment documents alongside
the message and its receipt.

```bash
docbank email mailbox import ./All-mail.mbox --dest /mail --id mail-export --preview
docbank email mailbox import ./All-mail.mbox --dest /mail --id mail-export
docbank email mailbox watch mail-export
docbank email mailbox receipts mail-export --after 0 --limit 100
```

To retry an interrupted upload, pass the same `--id`, source bytes, and
settings. Docbank checks verified chunks locally and skips them. By default,
the import job uses `--id`. If you want to import the same source into another
destination without uploading it again, pass a different `--job-id`.

Docbank defaults to the `mboxrd` dialect. For `mboxo` files, pass
`--dialect mboxo`. Preview mode samples up to three messages, reads at most
16 MiB of expanded message data, and does not guess the dialect.

Before publishing any message from a Google Takeout ZIP, the background job
verifies the entire archive. It refuses unsafe paths, symlinks, bad CRCs, and
excessive expanded data, never extracts files to a caller-selected directory,
and skips empty mailbox entries. When you resume a job, Docbank reuses the
verified entry hashes and continues after the last committed message, though
compressed entries still require decompression through that position.

Mailbox jobs survive a daemon restart. Use `mailbox cancel <id>` to stop a job
and `mailbox resume <id>` to resume a canceled or failed one. After each
100,000 messages, run `mailbox continue <id>` to scan the next segment of the
same source into the same collection. Job reports separate imported, rejected,
pending, and canceled occurrences, and a partial report with an unscanned tail
is not a completed import. To fetch another receipts page, pass the last
returned ordinal as `--after`.

Source labels stay in occurrence provenance. To assign existing Docbank tags
during import, opt in with `--label-tag 'Project=existing-tag-id'`. Label
mappings are job settings and cannot be changed, and all mapped tags must exist
when a job starts or resumes. A queued or running job prevents deletion of its
mapped tags. If you delete a mapped tag while a job is paused, that job cannot
resume; start a new job with updated mappings instead, which imports the source
again.

Importing a mailbox does not grant remote-processing consent. Attachment
documents may remain pending processing until you configure a suitable profile
and grant consent. Mailbox import does not provide PDF email export.

Docbank stores each container as ordered, verified 64 MiB chunks up to 256 GiB,
with each MBOX or ZIP entry limited to 50 GiB. A ZIP is limited to 1,000
entries and 200 GiB of expanded data. Each emitted EML message is limited to
128 MiB, and Docbank records malformed or oversized messages as rejected. There
are at most two active uploads per owner and eight globally; incomplete uploads
expire 24 hours after creation, while sealed sources have no expiry. Globally,
at most two jobs run at once and at most eight are queued or running, with at
most two queued or running per owner. These limits bound work and are not
throughput claims.

### Explicit EML transfers

If you have an external exporter, you can register an archive and supply your
own stable occurrence reference without connecting Docbank to the source
application:

```bash
docbank email mailbox register synthetic-export 'Explicit exported messages'
docbank email mailbox transfer ./message.eml --archive synthetic-export --reference item-1
```

Retrying a transfer with equal bytes returns the original receipt. If the
source bytes changed, pass `--if-rev` with the receipt's target revision;
Docbank preserves the earlier content version. Editing or remapping the target
causes a conflict, and trashing it returns a tombstone without resurrecting it.

Source mappings, referenced versions, attachment relations, and sealed
container bytes survive portable backup and restore and have no silent
retention deadline. There is no release command for these retry guarantees, and
Docbank does not silently prune referenced versions or relations. When you
empty trash, Docbank skips these messages, attachments, and containing folders
while deleting unrelated eligible trash. Ordinary
`docbank document add message.eml` does not create an external transfer
identity.

## Receive a numbered redaction package

To import a recipient's numbered redaction package without allocating numbers
locally:

1. Extract the recipient ZIP into a directory.
2. Preflight its DAT and OPT files with the supplied `mapping.json`.
3. Follow the
   [numbered package import procedure](document-processing.md#package-completed-numbered-pdfs-for-a-recipient)
   to import its PDFs, supplied text, and received labels while preserving the
   sender's numbering.

## Import from a URL

```bash
docbank document add https://example.com/filing.pdf --dest /inbox
```

Docbank downloads one URL before importing it and creates any missing
destination directories. HTTPS is required by default, though `--allow-http`
permits HTTP for the initial request and redirects. Docbank rejects other
schemes and stops downloads after ten redirects. There is no total download
timeout; cancel the command to stop it.

Downloads default to a 100,000,000-byte limit, which you can adjust by setting
`--max-bytes` to a positive byte count up to the vault's ingest limit. If a
response exceeds the cap, finishes incomplete, or returns any status other than
HTTP 200, the command fails before import.

Docbank chooses the filename from a valid `Content-Disposition` filename first,
then the final URL's decoded last path component, then the requested URL's, or
`download`. Query strings stay out of filenames. If you repeat the exact
requested URL with equal current bytes in the same destination, Docbank skips
the import and preserves the original provenance and revision, even if you
renamed the document or the response filename changed. Different URLs,
including query variants and mirrors, keep distinct documents and origins,
while changed bytes receive an ordinary collision suffix.

Provenance records the requested URL and final URL exactly as supplied, the
completed fetch time in UTC, and the observed ETag and Last-Modified headers
when supplied. CLI and API JSON expose these values as `url_fetch`, with
evidence version 1 and headers encoded as base64 to preserve their bytes. CLI
and web views display readable header text with reversible byte escapes for
invalid UTF-8. Fetch time stays separate from import time, and `Last-Modified`
stays as observed rather than becoming a document modification time.

Supply one URL per command. URL mode accepts `--dest` and `--json`, and
validates `--progress` without rendering a progress bar. Filters,
`--preflight`, and `--replace` apply to local imports.

## Ordinary file imports

For each regular file, Docbank runs two steps:

1. While reading the file, Docbank computes the SHA-256 content hash and stores
   the bytes durably before adding database records, reusing identical content
   already stored in the vault.
2. In one database transaction, Docbank creates the file entry, its
   revision-one `content_create` version, the record of its stored content, and
   its provenance. Provenance records the original path and modification time,
   and those facts survive later renames and moves.

See [Storage](../architecture/storage.md) for content records and
[Editing and versions](../architecture/editing-and-versions.md) for version
identity.

### MIME type detection

For local imports, `docbank document put`, and load-file package staging,
Docbank selects MIME types by inspecting the first 512 bytes with its pinned
signature detector. The `.eml` rule runs first and always uses
`message/rfc822`. Otherwise, recognized signatures take priority over the
host's extension table, including JPEG, HEIC, and HEIF, while
`docbank document put --mime-type` still overrides automatic selection.

An extension can refine a broad detector result when it names the same MIME
node, an alias, a child format, or another member of the `text/plain` family.
Unknown bytes, including empty files, can use a valid extension mapping as
their type. An unrelated, invalid, or binary ancestor mapping leaves the
recognized detector result in place, and compatible resolver parameters remain
on the selected value.

The importer also applies fixed suffix refinements for detector results that
the pinned library cannot name as a subtype:

- TIFF bytes with `.arw`, `.dng`, `.cr2`, or `.nef` use `image/x-sony-arw`,
  `image/x-adobe-dng`, `image/x-canon-cr2`, or `image/x-nikon-nef`.
- Unknown `.raf` bytes use the fixed `image/x-fuji-raf` type.
- PNG or APNG bytes with `.png` or `.apng` use `image/png` and the existing
  preview path.
- Matroska bytes with `.mka` use `audio/x-matroska`.
- Text-family bytes with `.xmp` use `application/rdf+xml`, including XMP
  packets without an XML declaration.
- Text-family bytes with `.md` or `.markdown` use `text/markdown`.
- Text-family bytes with `.go`, `.rst`, `.yaml`/`.yml`, or `.tex` use
  `text/x-go`, `text/x-rst`, `application/yaml`, or `application/x-tex`.

The pinned detector sees those TIFF-based RAW formats as `image/tiff`, and the
suffix rules add their stored subtype without trusting an arbitrary host
mapping. Automatic local replacements and package staging use the same
selector. Each content version keeps the MIME observation selected when that
version was created; Docbank does not rewrite existing versions.

When you pass a directory argument, Docbank walks it recursively. The
directory's basename becomes a folder under `--dest`, and everything below
keeps its relative structure:

```bash
docbank document add ~/old-laptop/Documents --dest /archive
# → /archive/Documents/... mirrors the source tree
```

Docbank normalizes trailing slashes and `./`-style paths, so `add docs/` and
`add ./docs` behave identically to `add docs`.

An explicitly named source can be a symlink to a directory, which supports
ordinary platform layouts such as `~/Dropbox` on macOS. Docbank resolves that
one root link, keeps `Dropbox` as the virtual directory name, and records
provenance using the path you supplied. Symlinks encountered *inside* the tree
are still skipped and reported, and Docbank does not import an explicitly named
symlink to a file. Entries filtered out by an include or exclude rule are not
failures, while selected non-regular entries are reported as failures.

## Preflight a large tree

You can inventory a source before Docbank opens any file content or changes the
vault:

```bash
docbank document add ~/Dropbox --preflight \
  --include '*.pdf' \
  --exclude .git \
  --exclude .Trash \
  --exclude project/cache
```

The report separates files currently eligible for packing through 64 MiB,
larger files that remain individual stored files, and files above the current
format-v1 ingest ceiling. It also reports logical bytes, directory count,
skipped non-regular entries, filesystem errors, and the largest groups by
lowercase filename extension. For a structured report, pass `--json`.

Preflight reads metadata only and does not open cloud placeholders, which are
file entries whose contents still need to be downloaded from a provider. This
lets you estimate an import without downloading the whole tree, though a
successful scan does not guarantee that the later import can read every file.

On macOS, `cloud placeholders` counts regular files whose bytes are not local,
including iCloud Drive and Google Drive for Desktop placeholders. These files
also count toward the report's size classes. Check this count before starting
an import that may require substantial downloading.

A provider may decline to download a placeholder's contents for the process
that opens it. The usual case is a daemon started by launchd as a background
job, while an interactive session succeeds. Docbank reports that failure for
the individual file, names the cause, and suggests opening the file once from a
user session or marking it available offline before retrying the import. After
changing your selection, rerun preflight, then pass the same `--include` and
`--exclude` flags to the real `docbank document add` command.

Filesystem names and provenance paths must currently be valid UTF-8. On POSIX
filesystems that permit other byte sequences, preflight and ingest report each
such entry with an escaped, printable path, skip opening or importing it,
continue with the rest of the tree, and never alter the source.

### Choose files with include and exclude rules

Use include rules to select files and exclude rules to skip files or whole
subtrees. Exclusions win, and include rules leave directories open for
traversal.

| Rule | Matches |
|------|---------|
| `*.pdf` | A basename at any depth |
| `reports/*.pdf` | A path relative to each source root |
| `cache` in `--exclude` | Entries named `cache`, including entire matching directory subtrees |
| `report[[]1].txt` | The literal filename `report[1].txt` |

Rules use Go's `path.Match` grammar, where `*` and `?` do not cross `/` and
`**` does not mean recursive matching. Use `/` separators on every platform;
Docbank rejects backslashes. To match a literal `[`, `?`, or `*`, use a bracket
expression. Matching is case-sensitive, including on Windows.

Repeat a flag for each rule; commas are literal characters. Docbank rejects
empty rules, absolute paths, parent traversal, and malformed patterns before
the walk. Watched inboxes use this syntax in `exclude_patterns`, while their
`exclude` entries remain literal. See
[watched inboxes](../configuration.md#watched-inboxes).

If your source argument is a single file, a basename rule such as `*.pdf`
matches it. A path-form rule such as `reports/*.pdf` applies to a directory
source's relative paths.

## Label and browse one import run

When importing through the HTTP API, you can attach an optional label to the
logical run in the request body. Docbank publishes the label atomically with
the first committed document observation:

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

When at least one file commits, the terminal report includes `ingest_id`. Pass
that ID to `GET /api/v1/collections/{id}` and
`GET /api/v1/collections/{id}/members`, or call `GET /api/v1/collections` to
list current nonempty runs. Collection membership follows documents as they
move within the vault and reports current paths, sizes, and versions, while
trashed files and superseded provenance disappear from live membership.
Caller-supplied `embedded:` provenance never creates a collection.

Collection labels have a separate ETag and edit route. To update or clear a
label, read `GET /api/v1/collections/{id}/label`, then send a `PUT` request to
the same path with exactly `{"label":"New name"}` or `{"label":null}` and the quoted
revision in `If-Match`. Non-null labels stay unique even while a collection is
empty. After permanent audit is enabled, label changes fail with HTTP 409 and
`audit_mutation_unsupported`, while the existing label and collection remain
readable.

If only some files succeed, the receipt names the real run and lists failures
beside it. If nothing commits, the receipt omits `ingest_id`, and Docbank
creates no collection or label. Label collisions and audit restrictions on an
initial label use this same per-file failure list instead of turning the whole
batch into one transport error.

## Follow a long import

In human mode, `docbank document add` runs a metadata-only scan to establish
file and byte totals, then reports content-read progress as it imports:

```bash
docbank document add ~/Dropbox --dest /archive --progress plain
```

The default `auto` mode draws a progress bar on a terminal and prints periodic
progress lines when stderr is redirected. Pass `plain` to always print lines,
or `bar` to force the redrawn bar. Progress goes to stderr while the final
summary goes to stdout. To suppress progress and emit only the machine-readable
final report, use `--json`.

Because a source may change before Docbank opens it, the scan totals are an
estimate. Byte progress counts content actually read, and a file counts as done
only after its blob and metadata operation returns. If you interrupt the
command, Docbank cancels the daemon request, keeps files that completed
successfully, skips them on a rerun, and does not create a file entry for an
incomplete import.

## What happens when I run the import again?

If a 200,000-file import is interrupted, run the same command again. For each
source file, Docbank checks the destination directory and its candidate names
(`report.pdf`, `report (2).pdf`, `report (3).pdf`, ...) and decides what to do:

- If a live file in the destination directory has the **same content** and was
  originally imported under the same source filename (its recorded origin has
  the same source kind and original basename), Docbank counts the file as
  `skipped`. That matching file can have since been renamed, or have been
  imported under a suffix by a prior run. A file with no recorded provenance
  matches only if its name is in the incoming file's suffix family
  (`report.pdf`, `report (2).pdf`, ...). As a result, a real source file named
  `report (2).pdf` next to an identical `report.pdf` imports as a new file, not
  a skip.
- If all existing candidates have different content, Docbank uses the next free
  suffix.
- Otherwise, Docbank takes the first free candidate name.

Repeating the same source import does not create extra copies of its entries.
Two differently named source files with identical bytes still import as two
file entries that share stored content and have distinct version UUIDs.

Each explicit filesystem rerun is still a new logical ingest run. When bytes
already match a destination node, Docbank adds that existing node to the new
run without creating a content version. Recording this new membership advances
the node revision, even without a collection label or any flags, and the same
applies to `--replace` when the bytes are identical. API clients holding the
old ETag must refresh it before their next write, or `If-Match` returns
`412 stale_revision`. Repeating the same observation within one run is a no-op.

Digest-checked `POST /uploads` retries behave differently. An equal retry
returns the existing node with an unchanged revision and does not return an
unused ingest identity.

## Collisions

Two different files arriving at the same virtual name do not conflict. Docbank
gives the newcomer a suffix, such as `scan (2).pdf` for a second `scan.pdf`,
and keeps a provenance record showing where each file came from.

## Replace a changing local file

When a repeated local add represents one changing source, pass `--replace`:

```bash
docbank document add ~/reports/summary.pdf --dest /archive --replace
```

Before reading the source, Docbank resolves the exact destination name and
records its node revision. Different bytes become a new content version on the
same node. Unchanged bytes count as skipped and keep the stored MIME type and
version history, while the new run membership advances the node revision as
[described above](#what-happens-when-i-run-the-import-again). A live directory
fails that file before source content is opened. If an absent destination is
claimed while the source is read, the create reports a conflict and does not
choose a suffix. Likewise, a stale observed revision reports a conflict and
leaves the newer content current. For ordinary collision suffixing, omit
`--replace`.

## Inspect where a document came from

Docbank stores where a document came from as provenance. You can query a live
file by vault path, or any retained file by stable node ID:

```bash
docbank document provenance /archive/Documents/report.pdf
docbank document provenance id:42 --json
```

The newest-first result identifies the ingest, its source kind and description,
the original source path and modification time, and the SHA-256 identity of
each provenance fact. An `active` status means no newer fact supersedes it, and
a correction adds a fact while keeping the earlier ones. Because a source path
can disclose machine-local names, provenance is available only through the same
authenticated API as the document itself.

Reading provenance does not open or change the original file, and a provenance
record does not prevent ordinary retention or deletion rules from removing a
document version.

Applications can append an origin learned later through the
[HTTP API](../architecture/http-api.md#content-identity-and-verification-evidence)
or [embedded Go API](../embedding.md), and can correct an active
caller-supplied fact by adding a new fact that supersedes it. Because Docbank
uses CLI and watched ingest facts to recognize repeated imports, those facts
cannot be superseded; append an additional origin instead. The `provenance` CLI
command reads this history.

## Failures don't abort the batch

Docbank records unreadable files, permission errors, and non-regular files such
as symlinks, sockets, and devices, continues with the rest of the import, and
reports all failures at the end. If a directory cannot be created in the tree,
for example because its virtual path collides with an existing file, Docbank
skips that subtree and continues with the next.

```
added: 4211  skipped: 12  excluded: 0  failed: 2
failed: /src/broken.pdf: opening /src/broken.pdf: open /src/broken.pdf: permission denied
failed: /src/link.pdf: not a regular file (symlinks are skipped)
```

When any file fails, the exit code is non-zero so scripted migrations can
detect partial imports. A missing or unreadable top-level source is reported
the same way while the command continues with the remaining source arguments.

## Sources are read-only

Import never deletes or modifies source files, including a followed root
directory symlink. Before you delete originals yourself, run
`docbank vault verify`, spot-check the imported documents, and capture a
[backup](backup.md).

## Remote API imports

Authenticated integrations can send one digest-checked file at a time through
`POST /api/v1/uploads`. The server requires the writer's SHA-256 and byte
length, computes both independently while streaming, and creates no node or
blob record when either differs. See the
[HTTP API](../architecture/http-api.md#addendum-post-uploads) and
[Agent integration guide](../agents/integration.md#create-and-ingest-safely)
for the contract.

Backups include collection labels and their revisions, run membership, and the
audit history for repeated operational observations. Older readers that do not
understand these records reject such a snapshot instead of restoring it without
the collection records.

## Continuously ingest a local inbox

For a folder on another machine, use [folder push](pushing.md), which reads the
folder on the client and uploads verified bytes to the daemon.

For local directories that receive files over time, configure a daemon-owned
`[[watch]]` entry instead of repeatedly running `docbank document add`. First
run `docbank tag create agent-sessions`, then add the watch to your
configuration:

```toml
[[watch]]
name = "agent-sessions"
source = "~/agent-sessions"
destination = "/archives/agents"
tags = ["agent-sessions"]
settle_time = "30s"
minimum_age = "168h"
scan_interval = "5s"
exclude = ["cache/", ".DS_Store"]

[storage]
pack_interval = "1h"
pack_max_bytes = 268435456
```

To process new imports automatically, add `profile = "mistral-ocr"` to the
watch or use `profiles = { document = "docling", audio_video = "asr" }` for
family routing, and
[grant consent with `docbank processing build --consent`](document-processing.md)
for that configured profile. Each added or updated version queues its rendition
stage. If consent is missing, the configured rendition remains unprocessed
while baseline plain-text extraction continues, and watch status shows why.
Granting consent later does not revisit skipped imports. See
[watch processing configuration](../configuration.md#watched-inboxes) for
supported profiles and scheduling limits.

By default, the daemon observes each file's filesystem identity, size, and
modification time for a full settle window before reading it. When the
filesystem supplies a native change timestamp, Docbank observes that timestamp
too, catching edits that preserve size and restore modification time. If no
change timestamp is available, the watcher falls back to size and modification
time. Any observed metadata change restarts settling and costs one more read,
though it adds no version when the bytes match.

Setting `minimum_age = "168h"` additionally requires seven days since the source's last
modification, which helps when an append-heavy session may pause for minutes or
hours without closing. The minimum-age gate survives restart, while the settle
observation does not, so in settled mode each file again waits for a full
window after restart. For ordinary inboxes that need only the settle window,
set `minimum_age = "0s"` or omit it.

For working notes that you want to archive promptly, opt in on a separate
watch:

```toml
[[watch]]
name = "working-notes"
source = "~/notes"
destination = "/archives/notes"
capture_on_change = true
settle_time = "30s"
minimum_age = "0s"
scan_interval = "1h"
```

Prompt capture reads the initial readable file immediately and uses filesystem
notifications to wake the same confined scanner after changes. The settle
window does not delay prompt reads, including after restart, while
`minimum_age` still applies. Each content Docbank reads and commits becomes a
version. Notifications can merge writes, and a source can be overwritten before
Docbank reads it; slow reading or ingest can miss intermediate saves. Prompt
capture may also archive readable intermediate content or temporary files
unless the [default watch exclusions](../configuration.md#watched-inboxes) or
configured rules skip them; it cannot know an application's logical save
boundary. It does not guarantee one version per save.

The watcher keeps periodic scans at `scan_interval` even in prompt mode.
Notification hints never authorize a direct path read; each pass uses the
existing root, symlink, mount, exclusion, stable-read, and mutation checks. A
native notification failure or the 32,768-entry-unit subscription budget closes
subscriptions, reports degraded polling in the watch job and daemon log, and
leaves periodic scans running. Each watched directory costs one unit plus its
immediate entry count to account conservatively for macOS handles. Pending
hints hold at most 1,024 paths, and overflow asks for a full scan. If a
directory vanishes during subscription, Docbank retries it at the next periodic
scan. Without a usable native change timestamp or notification event, a
same-size, restored-mtime edit may remain unseen.

Docbank also verifies that the confined source path still names the same
unchanged object. It never follows entries that are symlinks and never changes
or deletes source data. Because a time window cannot prove that a producer
formally closed a file, use a conservative age or point the watch at a
completed-file handoff directory when one is available.

The first stable observation creates a file under `destination` and records its
original source-relative path as immutable provenance, and later byte changes
append versions to that node. The watch name and current relative path form the
operational cursor. The daemon can follow a live source rename when a complete
scan finds one unique native file identity with a valid birth time, the old
path is absent, and a final confined check confirms both facts. Docbank updates
the cursor without moving or renaming the archived node, and later source edits
still append to that node even if you independently moved or renamed it inside
Docbank.

Rename continuity uses the native identity associated with the document through
successful ingestion at the bound source path; merely observing a replacement
does not establish that ownership. If an editor atomically replaces a file and
renames the replacement before Docbank accepts it at the old path, the new path
is a new source, even if a complete scan saw it waiting to settle or meet
minimum age. Once eligible, it creates a separate document if its archive
destination is vacant, or reports a destination conflict otherwise, leaving the
earlier document and history intact. Successful acceptance at the old path can
establish the new object's identity even when unchanged bytes need no new
content version.

Docbank remembers the last content accepted from the source independently of
the node's selected version. An unchanged source does not overwrite a manual
edit or revert after restart. Deleting a source leaves the archived node and
history intact. A rename made while the daemon is stopped, a copy, a hard-link
alias, an ambiguous replacement, or a filesystem without usable native birth
evidence does not carry rename continuity. A newly seen path then follows the
ordinary exact-destination rule, reporting a collision instead of silently
joining documents.

For an agent-session archive, use the source tree itself for the organization
you want to retain. With the configuration above, for example,
`~/agent-sessions/codex/project-alpha/2026/07/session-01.jsonl` becomes
`/archives/agents/codex/project-alpha/2026/07/session-01.jsonl`. Docbank does
not interpret a vendor's session format; it keeps the relative path and source
facts unchanged, and each accepted byte change becomes a new version of that
document.

Use `docbank document provenance <path-or-id>` to inspect the watch identity,
source-relative path, and supersession history of an imported node. The
built-in plain-text worker indexes JSONL session content up to the normal
extraction limit, so ordinary `docbank search` can find archived session text
without a vendor-specific parser. The optional `[storage]` schedule packs
accumulated small files with a finite per-run budget without deleting source
files, pruning versions, running GC, or rewriting existing packs. Portable
[backup and restore](backup.md) preserve the mirrored hierarchy, source
provenance, every retained version, and its verified bytes.

The watcher uses the exact destination name and does not add a collision
suffix. If unrelated content already occupies that path or the previously
mapped node is in trash, it stops with an error. Check `docbank jobs` for the
named `watch:<name>` job and any terminal error, correct the problem, and
restart the daemon. Successful additions, updates, and unchanged observations
appear in the daemon log. See
[Configuration](../configuration.md#watched-inboxes) for the complete field
contract.
