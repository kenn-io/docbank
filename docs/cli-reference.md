---
title: CLI Reference
description: Every docbank command, flag, output format, and error behavior.
---

# CLI Reference

All commands operate on the vault at `~/.docbank` (override with
`DOCBANK_HOME`; see [Configuration](configuration.md)). Errors go to
stderr and produce a non-zero exit code. Virtual paths are absolute,
`/`-separated, and case-sensitive.

Every data command below (`info`, `stat`, `add`, `ls`, `tree`, `cat`, `put`, `edit`, `versions`, `version`, `refs`, `revert`, `tag`, `audit`, `mv`, `rm`,
`restore`, `search`, `trash`, `gc`, `verify`, `storage`, `backup`, `jobs`) talks to the `docbank`
daemon over its HTTP API rather than opening the vault itself; if none
is running, the command auto-starts one in the background. `docbank
daemon status` and `docbank daemon stop` never auto-start. See
[Daemon](architecture/daemon.md) and
[Ownership & Concurrency](architecture/locking.md).

## docbank info

```
docbank info [--json]
```

Identifies the vault selected by `DOCBANK_HOME` and confirms that its daemon is
reachable. Human output shows the canonical machine-local vault path, stable
vault ID, live file and directory counts, trash size, retained version count
and logical bytes, tracked content blobs, and physical loose/packed usage.
The virtual root itself is not counted as a directory.

`--json` exposes the same values as stable fields. Agents should record
`vault_id` as identity and use `vault_path` only to confirm local placement:
restoring or moving a vault changes its path without changing its ID. Tracked
blob totals can include content awaiting garbage collection; `storage` reports
the files and packs currently occupying physical storage.

## Node selectors

Commands that inspect or mutate an existing node accept either its absolute
virtual path or a stable selector such as `id:42`. Paths are convenient live
coordinates; `id:42` continues to name the same document after a move or
rename. Human listings print this copyable `id:<positive-decimal>` form.
Machine-readable JSON continues to expose node IDs as numbers.

The destination of `mv` remains an absolute path because it describes where
the node should go. `restore` also accepts its older bare numeric form for
compatibility, although new scripts should use the unambiguous `id:42` form.
Commands that require a live tree entry reject trashed selectors. Read-only
`stat`, `cat`, `versions list`, audit status, and audit history can still inspect a
trashed node by stable ID; `restore` is the mutation that returns it to the
live tree.

## docbank stat

```
docbank stat <path-or-id> [--json]
```

Inspects one document or directory. Human output shows its stable `id:N`
selector, live or trashed state, quoted live path and name, kind, revision, and
timestamps. Files also show the current immutable version ID, SHA-256 content
identity, raw size, and recorded MIME type.

A path resolves only a live node. A stable ID can inspect the same node after
it is moved, renamed, or trashed; trashed output deliberately has no `path`
because the node has no live coordinate. `--json` returns the complete
authoritative node object used by the HTTP API.

## Process exit codes

CLI process codes are stable so shell automation can branch without parsing
stderr. Human error text remains explanatory and may change.

| Code | Meaning | Typical action |
|------|---------|----------------|
| `0` | Success, including an empty result or a dry run that found nothing | Continue |
| `1` | General operational failure, such as local I/O, transport, daemon, or an otherwise unclassified conflict | Report or inspect stderr |
| `2` | Invalid command usage, arguments, flag combinations, values, or request validation | Correct the invocation |
| `3` | The daemon returned `not_found` for the requested vault object | Refresh names or identities |
| `4` | Stale optimistic state: a revision or audit enrollment preview no longer matches | Re-read, reconsider, and retry deliberately |
| `5` | Vault maintenance is active, or a vault, backup repository, or physical pack resource is busy or locked | Wait and retry, or release the known owner; do not blindly force-unlock |
| `6` | A completed verification reported integrity findings, or a content stream failed terminal size/hash/digest proof | Do not trust or publish the affected bytes |

Integrity commands may write their complete human or JSON report before
exiting `6`; the report is evidence, not a success indication. Failures that
prevent verification from completing at all—such as an unreachable daemon—use
`1`. HTTP clients should continue to branch on the API's problem `code` rather
than translating process exits back into HTTP status.

## docbank add

```
docbank add <path>... [--dest <virtual-dir>] [--exclude <rule>]... [--progress auto|bar|plain] [--json]
docbank add <path>... --preflight [--exclude <rule>]... [--json]
```

Imports files or directory trees into the vault. Sources are copied,
never modified or deleted.

| Flag | Default | Meaning |
|------|---------|---------|
| `--dest` | `/inbox` | Virtual destination directory; created (with parents) if missing |
| `--exclude` | none | Prune a matching entry name anywhere, or a relative path within each source; repeatable |
| `--preflight` | false | Inventory source metadata without opening file content or changing the vault |
| `--json` | false | Emit only the terminal preflight or ingest report as JSON; suppress progress |
| `--progress` | `auto` | Human ingest progress: `auto`, `bar`, or durable `plain` lines |

- A directory argument imports recursively: its basename becomes a
  directory under `--dest` and relative structure is preserved.
- An explicitly named symlink to a directory is followed as the import root;
  its supplied basename and provenance spelling are retained. Symlinks inside
  that tree, symlinks to files, and other non-regular files are skipped and
  reported as failures; they do not abort the run.
- Name collisions with different content auto-suffix:
  `report.pdf` → `report (2).pdf`.
- Re-running an import converges: a file whose content already exists
  under any candidate name in the destination is skipped, so an
  interrupted bulk import can simply be re-run. See
  [Importing Documents](usage/importing.md).

Run `--preflight` before a large import. It reports regular-file and directory
counts, logical bytes, pack-eligible files, larger loose-only files, files over
the ingest ceiling, exclusions, skipped non-regular entries, filesystem errors,
and the largest extension groups. The scan reads filesystem metadata only: it
does not open cloud placeholders, create the destination, record an ingest, or
write blobs. `--json` retains a bounded set of detailed findings and file-type
groups for agents and scripts.

Exclusion rules are deliberately simple and shared by preflight and import. A
bare entry name such as `.git` or `node_modules` matches at any depth. A path
containing `/`, such as `project/cache`, matches that relative path and its
descendants within every supplied source. Rules are not shell globs and must be
relative; absolute paths and `..` escapes are rejected. Rule form is preserved:
`cache` is a name at any depth, while `cache/` and `./cache` mean only the
root-relative `cache` entry. Each `--exclude` value is literal and commas are
ordinary filename characters; repeat the flag to supply multiple rules.

An ordinary import first scans source metadata for file and byte totals, then
shows ingest progress on stderr. `auto` uses a redrawable bar on a terminal and
durable periodic lines when redirected; `--progress plain` forces durable
lines. The scan is advisory because sources may change before they are opened.
The command ends with a one-line stdout summary plus one stderr line per failed
file:

```
added: 12  skipped: 3  excluded: 2  failed: 1
failed: /src/broken.pdf: opening /src/broken.pdf: permission denied
```

Exit is non-zero if any file failed. A missing or unreadable top-level
source is reported as a failure and the command continues with remaining
source arguments, just as it does for failures inside a directory tree.
`--json` suppresses progress and returns the same terminal report shape as the
HTTP JSON endpoint, so stdout remains safe for automation.

## docbank provenance

```
docbank provenance <path-or-id> [--limit <n>] [--offset <n>] [--json]
```

Shows the immutable origin facts retained for one file, newest ingest first.
Each result includes its SHA-256 identity, whether it is the active fact, the
ingest UUID and time, source kind and description, original source path and
modification time, and the identity it supersedes when applicable. The page is
bounded to 1–1,000 facts; human output prints a continuation hint when more
remain.

Paths resolve live files. A stable `id:<node-id>` may also inspect a trashed
file; in that case the response has no live virtual path and the human output
labels it as trashed. `--json` returns the complete node, path, page authority,
and fact objects. The command is read-only and does not access or alter the
original source.

## docbank mkdir

```
docbank mkdir <absolute-virtual-path> [--json]
```

Creates one directory at the exact virtual coordinate and prints its stable
`id:N` selector plus quoted canonical path. The parent directory must already
exist; this command does not recursively invent missing parents. Existing
names, `/`, relative paths, files used as parents, and `.` or `..` path
segments are rejected without creating anything.

The daemon resolves the parent and creates the directory in one transaction,
so a concurrent ancestor move cannot redirect a path-based request. `--json`
returns the complete authoritative directory node.

## docbank ls

```
docbank ls [path-or-id] [--json]
```

Lists a virtual directory (default `/`). Columns: `SELECTOR`, `KIND`
(`dir`/`file`), `SIZE` (bytes; 0 for directories), `MODIFIED` (UTC,
RFC 3339 at second precision), `NAME`. Fails with `not a directory` when the
path names a file.

`--json` returns the resolved directory under `directory` and its complete,
ordered child list under `items`. Empty directories produce `"items": []`.
JSON preserves the authoritative full-precision timestamps.

## docbank tree

```
docbank tree [path-or-id] [-L <depth>] [--max-entries <count>] [--all] [--json]
```

Prints the subtree rooted at the path or stable node selector (default `/`),
two-space indented, each entry suffixed with its `id:N` selector in brackets.
Output is bounded by default to
four levels and 1,000 nodes, so an exploratory command cannot flood a terminal
or an agent's context. `-L`/`--depth` and `--max-entries` set narrower or wider
bounds. `--all` deliberately restores an unlimited traversal and cannot be
combined with either bound. Fails without output if `path` names a file.

When a bound hides entries, human output names every truncation boundary and
the number of direct children omitted there. Narrow the root path or increase a
bound before using `--all` on an unfamiliar archive.

`--json` returns the resolved root and a deterministic, pre-order `items`
array. Each item contains the node, its absolute virtual `path`, and its
`depth` beneath the root (direct children have depth 1). Always inspect
`truncated`; when true, `omissions` contains the affected path, the
`depth_limit` or `entry_limit` reason, and the number of direct children not
returned at that boundary.

## docbank cat

```
docbank cat <path-or-id>
```

Streams the file's stored bytes to stdout. Fails with `not a file` for
directories.

## docbank get

!!! info "Release availability"

    `docbank get` is newer than v0.10.0. Build from source to use it until the
    next release is tagged.

```text
docbank get <path-or-id> <local-file> [--overwrite] [--progress auto|bar|plain] [--json]
```

Downloads one current document version to a local file. Docbank writes into an
owner-private staging directory beside the destination, verifies the complete
size, SHA-256 identity, and terminal HTTP digest, syncs and closes the file,
then publishes it atomically. An interrupted or corrupt transfer never exposes
a partial destination.

Existing files are preserved unless `--overwrite` is explicit. Even then,
Docbank verifies the replacement before atomically replacing the existing path;
an existing symlink is replaced rather than followed. `--json` suppresses
progress and returns the node ID, immutable version ID, hash, size, and absolute
local output path. A trashed file remains retrievable by its stable `id:N`
selector while its content version is retained.

## docbank put

```text
docbank put <source-file> <vault-path-or-id> [--mime-type <type>] [--progress auto|bar|plain] [--json]
```

Replaces one existing file's current content while retaining every prior
immutable version. The source must be a regular file and is opened without
following a final symlink. It is never modified.

`put` reads the source twice: first to compute the SHA-256 and exact size the
daemon must independently verify, then to upload the bytes. Human mode shows
separate `hash` and `upload` progress; `auto` uses a terminal bar or durable
redirected lines, and `plain` always emits durable lines. `--json` suppresses
progress and returns the new node, immutable version, and server-computed hash
and size. `--mime-type` overrides extension/content detection.

The command completes its local hash before starting or contacting the daemon,
then resolves the target to a stable node ID and revision immediately before
upload. This keeps a slow local read outside the daemon's idle lifetime and
shortens the optimistic-concurrency window. The raw `PUT` carries the inspected
revision as `If-Match`; if another actor moves, trashes, or replaces the node
afterward, the operation fails with `stale_revision` rather than losing the
concurrent update. A successful put bumps the node revision, creates a
`content_replace` version, and leaves the older bytes reachable through
`docbank versions cat <id>`. Replacing with identical bytes still records
an explicit versioned operation while the blob itself deduplicates.

## docbank edit

```text
docbank edit <vault-path-or-id> [--editor <command>] [--mime-type <type>] [--progress auto|bar|plain]
```

Downloads the current immutable version into a private temporary directory,
verifies its version ID, size, SHA-256, and terminal digest, and opens the staged
file in a blocking editor. `--editor` takes precedence over `VISUAL`, then
`EDITOR`; the platform fallback is `vi` on Unix or Notepad on Windows. Editor
commands use shell-style quoting on Unix and native Windows command-line parsing
on Windows, but are executed directly without a shell. GUI editors must be
configured to wait, such as `VISUAL='code --wait'`.

After a successful editor exit, Docbank hashes the staged file. If its bytes and
media type are unchanged, it reports the existing version and does not write.
Otherwise it preserves the current media type (or applies `--mime-type`) and
uploads a verified `content_replace` using the revision inspected before the
editor opened. Concurrent mutation fails with `stale_revision`; the command
does not silently reopen or overwrite the newer state. Since editing may exceed
the idle timeout, the daemon is reacquired after hashing. Human progress covers
`download`, `hash`, and `upload`; this interactive command has no JSON mode.

Private staging is removed on every ordinary outcome. If cleanup fails after an
update already committed, Docbank keeps the command successful, prints the new
version, and emits a warning rather than encouraging a duplicate retry.

## docbank versions

!!! info "Release availability"

    The explicit `versions list|show|cat` command vocabulary is newer than
    v0.7.0. Build from source to use it until the next release is published.

```text
docbank versions <command>
```

Groups the explicit `list`, `show`, `cat`, and `prune` operations for immutable
document content versions.

### docbank versions list

```text
docbank versions list <path-or-id> [--limit <n>] [--offset <n>] [--json]
```

Lists the file's immutable content versions newest-first. The default limit is
100; `--limit` accepts 1–1000 and `--offset` continues through older records.
Human output marks the node's current version. `--json` emits
`{"items": [...], "total", "limit", "offset"}` so callers can distinguish a
complete page from a prefix.

Every newly imported file has one revision-one `content_create` version. Each
successful `put` adds a `content_replace` row and each `revert` adds a
`content_revert` row naming its immutable source.

### docbank versions show

```text
docbank versions show <version-id> [--json]
```

Inspects one immutable version by stable UUID, independent of the file's current
path. The human view prints node and node-revision identity, recording time,
transition kind, blob hash, size, media type, and any reversion source;
`--json` emits the typed record.

### docbank versions cat

```text
docbank versions cat <version-id>
```

Writes that exact version's bytes to stdout. It exits successfully only after
the response version ID, byte count, SHA-256 identity, and terminal
`Content-Digest` all agree. Output may already have reached stdout when
verification fails, so scripts publishing a file should write privately and
rename it only after a successful exit.

### docbank versions prune

```text
docbank versions prune <path-or-id> --version <version-id> [--version <version-id>...] [--run] [--json]
docbank versions prune <path-or-id> --keep-newest <n> [--run] [--json]
docbank versions prune <path-or-id> --older-than <age> [--run] [--json]
docbank versions prune <path-or-id> --all-prior [--run] [--json]
```

Selects unwanted non-current history for one file. Exactly one selector is
required. `--version` is repeatable and literal, `--keep-newest` retains at
least that many newest rows including the current head, `--older-than` accepts
Go durations plus whole days such as `90d`, and `--all-prior` selects the
complete old graph. The command is a dry run unless `--run` is present.

The current content is always retained. Ordinary selectors cannot select the
current row. If one includes a source still required by a retained reversion,
the report identifies and retains that source. `--all-prior` can replace a
current reversion with a same-byte source-free checkpoint so the complete
previous graph, including that superseded revert row, can be released safely.
Execution uses the node ID and revision inspected immediately beforehand; a
concurrent change fails with `stale_revision`.

Age previews report their evaluated cutoff. Wall-clock aging does not advance a
node revision, so a later `--older-than ... --run` can also select versions that
crossed the same age boundary after the preview. To execute the exact previewed
set, pass its candidate IDs back through repeated `--version` flags.
Explicit-ID requests accept at most 1,000 IDs; re-read the node revision between
batches when applying a larger exact set.

Human and JSON reports separate selected versions and logical bytes from
physical consequences: shared blobs remain reachable, loose unreferenced blobs
wait for `docbank gc --run`, and dead packed payload waits for GC followed by
`docbank storage repack`. Pruning itself never claims to reclaim disk space.
Deleted version IDs stop resolving. Backups made afterward preserve that
result, while snapshots made before pruning still contain their earlier state.

## docbank refs

```text
docbank refs <sha256> [--limit <n>] [--offset <n>] [--json]
```

Finds every immutable content version that retains the canonical lowercase
SHA-256 identity. Live current references sort first, followed by live prior
versions and trashed references. Each result carries the stable version and
node IDs, node revision, current/history state, size, recording time, and the
node's current path when it is live. Human output renders the node as a
copyable `id:N` selector; JSON keeps its numeric ID. Trashed nodes have no
resolvable path.

The default limit is 100; `--limit` accepts 1–1000 and `--offset` continues a
bounded result. `--json` emits the page envelope with `items`, `total`, `limit`,
and `offset`. A cataloged physical blob with no retained content version is not
a match; the command reports `no authoritative references`.

## docbank revert

```text
docbank revert <vault-path-or-id> <version-id> [--json]
```

Makes a prior version current by creating a new immutable `content_revert`
history row. It never deletes or rewinds the current or intervening versions,
and it does not read or copy the source blob. The selected source must belong to
the target file and must not already be its current version.

The command inspects the target's stable node ID and revision, then sends both
with the source version ID. A concurrent move, trash, replacement, or reversion
fails with `stale_revision`. Human output identifies the source, new version,
resulting revision, size, and hash; `--json` returns the node, new version, and
complete source-version receipt. Repeating the same historical choice later is
valid and records another explicit operation.

## docbank tag

```text
docbank tag create <name> [--json]
docbank tag list [--limit <n>] [--offset <n>] [--json]
docbank tag show <name-or-id> [--json]
docbank tag rename <name-or-id> <new-name> [--json]
docbank tag delete <name-or-id> [--json]
docbank tag assign <name-or-id> <path-or-node-id> [--json]
docbank tag unassign <name-or-id> <path-or-node-id> [--json]
docbank tag nodes <name-or-id> [--limit <n>] [--offset <n>] [--json]
```

Defines stable tags and assigns them to live nodes independently of virtual
paths. Every subcommand accepts the exact current tag name; commands operating
on an existing tag also accept its UUID. Names are Unicode NFC-normalized,
case-sensitive, mutable, and cannot contain control characters. Renaming never
changes the tag ID. Deleting a tag removes all assignments but does not delete
nodes or content; recreating the same name allocates a different ID.

A canonical UUID-shaped selector is always a stable ID, including after that
ID is deleted. If a tag's display name itself looks like a UUID, address that
tag through the different UUID returned when it was created. This prevents a
mutable or reused display name from taking over a durable identifier.

`tag list` and `tag show` expose each tag's revision. Rename and delete first
resolve the selector, then condition the mutation on that inspected revision;
a concurrent rename or assignment change returns `stale_revision` instead of
overwriting or deleting the newer state.

Assignment by path resolves that live coordinate and updates its tag inside
one daemon/store transaction, so moving an ancestor cannot redirect the
operation between separate requests. An `id:N` selector deliberately targets
the stable node identity under its inspected revision. Repeated assignment and unassignment are
idempotent and report `changed: false` without a revision bump. A real
assignment change advances both the node and tag revisions. `tag nodes`
includes live and trashed nodes, but omits a path for trash because it has no
resolvable live coordinate. List commands return at most 1000 results per page
and JSON output includes `total`, `limit`, and `offset`.

## docbank audit

```text
docbank audit enable <path-or-id> [--agent-label <label>] [--json]
docbank audit enable --node-id <id> [--agent-label <label>] [--json]
docbank audit enable --run --token <preview-token> --acknowledge-permanent-retention [--json]
docbank audit status [path-or-id] [--json]
docbank audit status --node-id <id> [--json]
docbank audit history <path-or-id> [--limit <n>] [--cursor <cursor>] [--json]
docbank audit history --node-id <id> [--limit <n>] [--cursor <cursor>] [--json]
docbank audit history --scope <scope-id> [--limit <n>] [--cursor <cursor>] [--json]
docbank audit verify [--expected <prior-json-report>] [--json]
```

`audit enable` permanently protects a directory scope and all retained content
versions beneath it. Enrollment cannot be disabled. The default invocation is
a read-only preview that reports the exact protected set, storage impact,
baseline digest, vault-wide permanent metadata, and a one-use token. The first
scope permanently retains enrollment-time names, topology, tags, assignments,
ingests, and provenance across the vault, including outside the selected scope;
unrelated content does not become a scope member. Execution is a separate
command that accepts only that token and the explicit permanent-retention
acknowledgment; target selectors are deliberately absent from the execution
command.

The token expires after ten minutes, is consumed by one execution attempt, and
does not survive daemon restart. The daemon recomputes the reviewed authority
inside the mutation boundary. If metadata or allocator state changed, it
returns `audit_preview_stale` without enabling the scope; run a new preview.

`audit status` without a selector reports vault and scope evidence. A path or
stable node ID additionally reports whether that node has sticky audit
membership.

`audit history` reads canonical events for one protected node, newest first.
Path events expose old and new coordinates with their live/trash state; content
events expose prior and resulting immutable version IDs. Tag and provenance
events expose their stable identity and typed before/after state. The default
and maximum page sizes are 50 and 500. `next_cursor` in JSON, or the `next
cursor` line in human output, continues
through older events without shifting when a newer operation is appended. A
cursor is opaque and bound to its stable node. Use `--node-id` for a moved or
trashed node. A protected enrollment-baseline member can legitimately have no
node-specific events until its first later mutation; use `audit status` for
membership authority.

`audit history --scope <scope-id>` reads the same canonical events across all
members of one permanent scope. Human output names each event's copyable node
selector; JSON includes the complete scope status alongside the page. Its
cursor is bound to the stable scope rather than one node.

`audit verify` independently replays canonical history against current
metadata, then re-hashes every unique blob retained by protected versions. Its
terminal evidence contains the stable vault and allocation-lineage identities,
allocation count/head, operation high-water mark, and every scope count/head.
Human output reports the same evidence and protected-byte totals; JSON is
suitable for external recording. Missing, corrupt, unreadable, or inconsistent
authority exits non-zero. Use the top-level `docbank verify` when the decision
requires every blob in the vault rather than only permanent audit content.

Save a successful active JSON report outside the vault, then pass it back with
`--expected`. Verification proves that its allocation head and every recorded
scope head remain exact prefixes of current authority. Equal or validly extended
chains pass; a different vault/lineage, missing scope, shorter chain, or
divergent head is reported with a stable problem code and exits non-zero.

The first scope creates the vault-wide genesis. On main after v0.10.0, later
`audit enable` commands can add disjoint directory scopes without duplicating
that genesis; overlapping or nested scopes are rejected. See
[Permanent Audited History](usage/audited-history.md) for enrollment,
supported mutations, and maintenance behavior.

## docbank mv

```
docbank mv <source-path-or-id> <dest-path> [--json]
```

Moves or renames a node. Metadata only — bytes never move. The
destination is interpreted like POSIX `mv`:

- If `dest-path` names an existing directory, the source moves **into**
  it, keeping its name.
- Otherwise `dest-path`'s parent must exist, and its basename becomes
  the new name (rename, or move-and-rename).
- If `dest-path` names an existing **file**, the move fails with
  `name already exists` — docbank never overwrites.

Directory moves carry the whole subtree. A move that would place a
directory under its own descendant fails with `move would create a
cycle`. On success human output prints `moved [id:<id>] <new-path>`; `--json`
returns the complete resulting node, including its stable ID, revision, and
new path.

### docbank mv batch

```
docbank mv batch <plan.json|-> [--json]
```

Applies up to 1,000 moves as one all-or-nothing metadata transaction. A dash
reads the plan from standard input. Each plan item has `source` (an absolute
virtual path or `id:<number>`) and an absolute `destination`:

```json
{"moves":[
  {"source":"/inbox/a.pdf","destination":"/filed/b.pdf"},
  {"source":"id:42","destination":"/inbox/a.pdf"}
]}
```

Sources are interpreted from the transaction's initial tree. A batch
destination is always the exact final coordinate, and its parent is resolved
in the planned final tree: unlike ordinary `mv`, an existing directory does
not mean “move into this directory.” Name the retained basename explicitly
when that is the intent. The complete final tree is
validated before anything moves, which supports file and directory swaps and
nested reorganizations without temporary user-visible names. Any missing
source or parent, stale ID revision, collision, or cycle rejects the entire
plan. Human output reports the stable ID and quoted old/new paths in request
order; `--json` returns the same bounded receipt set as structured data.

## docbank rm

```
docbank rm <path-or-id> [--json]
```

Soft-deletes: moves the node — and, for a directory, its entire subtree —
to the trash. Nothing is permanently removed and no bytes are reclaimed.
The freed name is immediately reusable. Prints:

```
trashed [id:15] /taxes/2024/return.pdf (restore with: docbank restore id:15)
```

`--json` returns the trashed node receipt. Its `path` is the pre-trash path
shown for recovery context; it no longer resolves to that node. Retain the
stable `id` and `revision` as authority.

There is no hard-delete flag. GC cannot collect a trashed document because the
trash entry remains a restorable reference. Permanent metadata deletion,
unreachable-content collection, and packed-space reclamation are the separate
`trash empty --run`, `gc --run`, and `storage repack` operations.

## docbank restore

```
docbank restore <id-or-selector> [--json]
```

Returns a trashed node (by `id:N` selector — see `docbank trash list`) to its original
location, re-suffixing its name if a live node now occupies it. If the
original parent directory was itself permanently deleted, the node is
restored under `/`. Human output prints `restored [id:<id>] <path>`; `--json`
returns the complete restored node with its resulting path and revision.

## docbank search

```
docbank search <query>... [--tag <name-or-id>] [--mime-type <type/subtype>] [--under <path-or-id>] [--modified-since <timestamp>] [--modified-before <timestamp>] [--limit <n>] [--json]
```

Full-text search over live node names and verified extracted text (FTS5).
Every whitespace-separated term is matched as a prefix; FTS operator syntax
in the query is escaped, not interpreted. Name matches retain their existing
BM25 order and appear before content-only matches, whose ranking is independent.
The default limit is 50 and `--limit` accepts 1–1000. When more matches exist,
the command says that the result is truncated rather than silently implying
completeness. Output columns are `SELECTOR`, `MATCH`, and `PATH`; no matches prints
`no matches`.

`--tag` requires one current tag assignment. It accepts a tag's exact name or
stable UUID using the same selector rules as `docbank tag show`; the CLI
resolves names before searching, so the request is bound to stable identity.
`--mime-type` accepts one valid parameter-free media type and matches the
current version's base type case-insensitively. Stored parameters do not affect
the match: `text/plain` includes `text/plain; charset=utf-8`. MIME filtering
excludes directories and retained non-current versions.
`--under` accepts an absolute virtual path or stable `id:N` selector for one
live directory and searches its descendants. The CLI resolves paths before the
request and the daemon uses the resulting stable directory ID. The directory
itself is excluded; a file, missing node, or trashed directory is rejected.
`--modified-since` and `--modified-before` accept absolute RFC3339 timestamps
and filter the live node's current modification time. The lower bound is
inclusive and the upper bound is exclusive. Either may be used alone; when
both are present, the lower bound must be earlier. Inputs are normalized to
canonical UTC before the request.

`--json` emits the typed search report with `hits`, the applied `limit`, and
an explicit `truncated` boolean. A filtered report also echoes the stable
`tag_id`, normalized `mime_type`, stable `under_node_id`, and canonical
`modified_since` / `modified_before` bounds when supplied. An empty result uses
`"hits": []`.

The daemon indexes current UTF-8 `text/*`, JSON, and JSONL blobs up to 16 MiB
after a terminally verified read. PDF, Office, and OCR extraction are not yet
available. See [Searching](usage/searching.md).

## docbank trash

```
docbank trash list [--json]
docbank trash empty [--older-than <age>] [--run] [--json]
```

`list` shows restorable trashed nodes: `SELECTOR`, `TRASHED AT`, `NAME`. Only
trash roots are listed — trashing a directory produces one entry, and
restoring it brings the whole subtree back. Human output renders UTC seconds;
`--json` preserves the authoritative full-precision timestamps.

`empty` reports how many trash roots are eligible but does not delete by
default. Pass `--run` to permanently delete them; their blobs then become
`gc` candidates unless referenced elsewhere. `--older-than` accepts Go
durations (`12h`, `30m`) plus a day suffix (`30d`); negative ages are
rejected. Without that filter, every trash root is eligible.

`list --json` emits `{"items": [...]}`. `empty --json` emits the same typed
dry-run or execution report as the HTTP API: `candidate_roots`, `deleted`,
and `run`. Human status lines are suppressed so stdout contains one JSON
document.

## docbank gc

```
docbank gc [--run]
```

Garbage-collects unreachable blobs — content referenced by no live node,
no trashed node, and no recorded prior version. Dry-run by default:

```
3 candidate blob(s), 0 untracked file(s), 1204882 loose byte(s) reclaimable
dry run — pass --run to delete
```

Packed candidates are reported separately as stored bytes pending repack;
removing their catalog authority does not claim that immutable pack space was
already reclaimed. With `--run`, loose blob files are deleted first, then their
metadata rows; output separately reports removed blob records, reclaimed loose
files, and reclaimed bytes. A crash mid-GC leaves
rows without files, which the next `gc --run` reconciles and `verify`
reports in the meantime. The daemon's maintenance gate rejects new
mutations with the retryable busy exit code `5` while `gc --run` runs, so it
never races a concurrent import (see
[Ownership & Concurrency](architecture/locking.md)).
GC does not invoke repack, and no automatic GC/repack scheduler exists today.

## docbank storage status

```
docbank storage status [--json]
```

Reports the daemon's physical storage inventory: logical loose blob count and
physical loose bytes (raw and zstd files), live packed blobs and their
stored/raw bytes, pack count, and immutable packed
bytes pending repack. The command is read-only. `--json` emits the same fields
as the authenticated `GET /api/v1/storage` endpoint.

## docbank storage pack

```
docbank storage pack [--max-bytes <bytes>] [--json]
```

Explicitly converts authorized loose blobs into immutable Kit pack files. The
operation runs through the authenticated daemon and holds the vault maintenance
gate; reads remain available, while imports and other mutations receive the
retryable busy exit code `5`. Packing
does not change document identity or blob read authority, and mixed loose and
packed storage remains valid after an interruption.

`--max-bytes` is a soft raw-byte work budget. The blob that crosses the budget
is committed before the operation stops, and output says the budget was
exhausted. Check `storage status` and rerun if loose blobs remain; crossing the
budget does not itself prove that more eligible work exists. Zero (the default)
is unlimited. `--json` includes packing, repair, deferral, and reconciliation
counters from the shared Kit lifecycle engine.

## docbank storage repack

```
docbank storage repack [--min-age <duration>] [--min-dead-bytes <bytes>]
                       [--max-bytes <bytes>] [--json]
```

Rewrites eligible sparse packs with only their live blobs, atomically changes
catalog authority, and retires the old immutable pack files after active
readers release them. Packs with no live mappings are retired regardless of
age. A partially live pack is eligible when at most half its entries remain and
it satisfies both selection thresholds. Defaults are `--min-age 24h` and
`--min-dead-bytes 8388608`; use explicit smaller positive values for immediate
manual compaction.

`--max-bytes` is a soft live raw-byte budget. Zero is unlimited and makes a
source-content error fail the operation immediately; a positive budget lets
Kit continue with independent eligible source packs and return their combined
errors after committed work. The report's `bytes_repacked` is live raw content
rewritten, not a claim about filesystem bytes reclaimed. Compare `storage
status` before and after when exact inventory change matters.

Pack retirement can be deferred when another process holds a source pack open,
most commonly through a Windows handle that does not permit deletion. The
repack response then uses `pack_retirement_deferred`: replacement catalog
authority has already committed, so do not restore the old mapping or assume
the rewrite rolled back. Release the external file lock and run `docbank
storage pack`; its reconciliation pass removes the orphaned source pack.

## docbank verify

```
docbank verify
```

Validates logical metadata—including independent replay of any audit history—
then re-hashes every stored blob against its recorded SHA-256. Reports metadata
failures as `metadata: <detail>` and blob failures as `missing: <hash>` (row
without file), `corrupt: <hash>` (hash mismatch), or `unreadable: <hash>` (I/O
error), followed by `<n> blob(s) ok, <n> problem(s)`. Exits non-zero if any
problem was found.

## docbank backup

```text
docbank backup init [--repo <dir>] [--json]
docbank backup create [--repo <dir>] [--tag <label>] [--jobs <n>]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
docbank backup list [--repo <dir>] [--json]
docbank backup verify [snapshot] [--repo <dir>] [--all] [--quick] [--jobs <n>]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
docbank backup restore [snapshot] --target <dir> [--repo <dir>] [--overwrite]
                       [--jobs <n>] [--force-unlock]
                       [--progress auto|bar|plain] [--json]
```

Initializes an immutable Kit repository, captures a verified JSONL-native
snapshot through the daemon, lists snapshot history, independently proves
repository integrity, and restores a proved vault. `--repo` overrides
`[backup] repo`; one of them is required. `create` briefly quiesces mutations
only while pinning its logical view, then streams loose or packed content while
normal daemon work resumes. `--jobs 1` serializes repository readers;
`--force-unlock` is only for a repository lock whose owner is known to be gone.
`create` and `verify` draw per-stage progress bars on a terminal and durable
progress lines when redirected; `--progress` can force either form. `verify`
checks the latest snapshot by default, one named snapshot positionally, or all
snapshots with `--all`; `--quick` skips content reads. `restore` targets the
latest snapshot by default and requires a separate `--target`; non-empty
targets require `--overwrite`, which merges rather than clearing unrelated
files. Compatible content is restored packed, with verified loose fallbacks
reported explicitly. Every subcommand supports typed `--json` output;
long-running operations suppress progress in that mode. See
[Backup](usage/backup.md).

## docbank daemon

```
docbank daemon run
docbank daemon start
docbank daemon status [--json]
docbank daemon restart
docbank daemon stop
```

`daemon run` runs the daemon in the foreground, logging to stderr, until
signaled or stopped; it's usually invoked by `daemon start` in the
background, and is useful directly for debugging. `daemon start` spawns
it detached in the background, logging JSON to `$DOCBANK_HOME/logs/`.
`daemon status` reports whether a daemon is running (pid, address,
version, uptime) without starting one; `--json` emits `{"running": bool,
"pid", "address", "version", "started_at"}` for agents. `daemon restart`
stops the daemon if one is running (tolerating it not already running),
then starts it again, printing `restarted: ...` or `started (was not
running): ...` accordingly. `daemon stop` gracefully stops the running
daemon (or prints `no daemon running`) without starting one. Every data
command auto-starts a daemon if none is running — `daemon start` exists
for explicit control (long-running background use, inspecting logs
before running commands). `daemon start`, `daemon restart`, and
auto-start all converge the same way: a running daemon whose version or
API protocol does not match the invoking binary is stopped and replaced
(printed as `replaced daemon <old> (pid N) with <new>: ...`), so after
any of them succeeds, the one running daemon is current. See
[Daemon](architecture/daemon.md).

## docbank jobs

```
docbank jobs [--json]
```

Shows daemon-owned background tasks in stable name order, including status,
start and finish timestamps, and the bounded error recorded for a failed task.
Running tasks have no finish timestamp; terminal task records remain visible
until the daemon restarts. `--json` emits `{"items": [...]}` for automation.
Every daemon registers `extract:plain-text`; configured watched inboxes add
`watch:<name>` tasks. See [Daemon](architecture/daemon.md).

## docbank watch

!!! info "Release availability"

    `docbank watch list` is newer than v0.10.0. Build from source to use it
    until the next release is tagged.

```
docbank watch list [--json]
```

Lists the daemon's effective watched-inbox configuration in stable name order:
the machine-local source, virtual-tree destination, complete settle window,
scan interval, exclusion count, and current runner state. Human output quotes
source and destination paths so terminal control characters cannot disguise
them. `--json` includes the complete exclusion rules and the corresponding
job record for agents and automation.

This command is inspection only. Edit `config.toml` and restart the daemon to
change a watch.

## docbank update

```
docbank update [--check] [--yes] [--force]
```

Checks GitHub for a newer release and, unless `--check`, installs it:
stops a running daemon, replaces the binary, and restarts the daemon
from the new executable (rolling back to a restart of the old daemon on
install failure). `--check` prints the current and latest versions and
stops there. `--yes` skips the install confirmation prompt (required in
non-interactive use, since there is no default without a terminal to
prompt on). `--force` bypasses the cached check (release metadata is
refetched) and allows replacing an unversioned dev build; it does not
reinstall a release that is already current. Refuses to install a
release with no published SHA256 checksum.

## docbank openapi

```
docbank openapi [--json]
```

Prints the HTTP API's OpenAPI document — YAML by default, `--json` for
JSON. Needs no running daemon and no vault: routes are registered
against an offline server instance and never invoked. For agents and
API client generation; see [HTTP API](architecture/http-api.md).

## docbank version

!!! info "Release availability"

    This command is newer than v0.7.0. Build from source to use it until the
    next release is published.

```
docbank version
```

Prints the build version and commit (`dev (unknown)` for untagged local
builds; release builds inject both via `-ldflags`).

## Environment variables

`DOCBANK_HOME` selects the vault (see [Configuration](configuration.md)).
`DOCBANK_LOG_LEVEL` sets the daemon's log level (`debug`, `info`,
`warn`, `error`; default `info`) for both `docbank daemon run` and
background-spawned daemons.
