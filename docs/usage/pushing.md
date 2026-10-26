---
title: Push a folder to another daemon
description: Archive a local folder through a keyed daemon, resume by content hash, and continue an existing watch.
last_edited: 2026-10-25
---

# Push a folder to another daemon

Use `docbank push` to archive a local working folder through a keyed daemon.
The client reads your files and uploads verified bytes. It never changes or
deletes local files. The daemon does not need a filesystem copy of the folder.

## Connect and push

The daemon listens on loopback by default. To reach it from another machine,
forward its API port through SSH:

```bash
ssh -N -L 7778:127.0.0.1:7777 archive-host
```

Give the client the daemon's configured API key in `DOCBANK_API_KEY`, or put
that key in a file readable only by you. A key file takes precedence over the
environment. With an ephemeral daemon key, obtain its current value from the
owner-private runtime record on the daemon host. Keep keys out of command
arguments and source folders.

```bash
docbank push ~/documents \
  --to http://127.0.0.1:7778 \
  --api-key-file ~/.config/docbank/archive.key \
  --name laptop-documents \
  --dest /documents
```

`--to` selects an HTTP or HTTPS daemon origin. Push requires a key and refuses
redirects and proxies. It does not start a local daemon or use `DOCBANK_HOME`
to select the destination vault. The target daemon must support the push
routes; an older daemon returns an error. A daemon can also listen on a
network address, but it then sends the key and your files in cleartext unless
you put it behind an encrypted tunnel or HTTPS proxy. See
[bind validation](../configuration.md#bind-validation).

Before sending the key or any file over a new connection, push asks the
endpoint to prove that it holds the same API key. Neither side sends the key
for this check. If the SSH tunnel exits and another local process takes over
its port, that process fails the check and receives neither the key nor your
files; push stops with an error saying the endpoint did not prove it holds the
API key. The check cannot detect a process that relays the challenge to the
real daemon, which requires access to the daemon itself. Use HTTPS when the
path between the client and the daemon is not trusted.

A long-running `--watch` also needs the target daemon to stay available. A
background daemon with no configured `[[watch]]` and scheduled packing disabled
shuts down after `[server] idle_timeout` (30 minutes by default) when no API
requests arrive. Push sends no requests while the folder is unchanged, so it
cannot wake a daemon that has already stopped. Before starting push watch, set
`[server] idle_timeout = "0"` or run `docbank daemon run` in the foreground.
See [daemon idle shutdown](../architecture/daemon.md#auto-start-and-idle-shutdown).

The first push preserves the folder's relative hierarchy under `--dest`. The
daemon creates missing virtual directories together with each new document,
and only when it creates one. Each regular file is hashed locally.
The daemon independently checks the declared SHA-256 and byte count before
committing its content and provenance. A changed file during reading causes an
error or, in watch mode, a new settle observation.

## Resume and preserve versions

The push name and slash-separated relative path identify each source file.
Use the same `--name` when you resume, reinstall the client, or move the working
folder to another machine. Use different names for independent source trees.
Using an existing watch's name continues its matching relative-path identities.
Use a new name when you want an independent push source.

When it starts, the client reads the last hash the daemon accepted for every
identity of the push name, a page at a time. This also checks the key and the
push name before any file is read, so those mistakes fail immediately, even
with `--watch`. A one-shot run hashes every local file. A periodic watch scan rechecks accepted
file hashes; notification scans limit content rechecks to dirty or
metadata-changed files. Push does not trust modification times, which some
tools preserve across edits. Equal
bytes require no upload. Changed bytes append a version
to the same node, even if you moved or renamed the archived node. An unchanged
source leaves a later archive edit or revert intact. The daemon retains each
source's accepted hash separately from the node's current content and retained
version history. Pruning an accepted version can release its bytes while the
source digest remains available. Re-pushing those same bytes still skips the
upload and leaves the current archive content unchanged; it does not recreate
the pruned version.

Each new identity gets its own document, even when another file already has
the same bytes. Stored bytes are deduplicated anyway, and the
[collection quality report](web.md#browse-import-collections) counts
duplicate content. Deleting a local file leaves its archived node and history
intact. Renaming a local path gives it a new identity; it is not an archive
move. Changing `--dest` affects new identities only. A mapped node in trash is
an error. Restore that node before pushing the identity again. A new identity fails
if an unrelated entry occupies its exact destination name; push does not
overwrite it or add a collision suffix.

Each run is one [import collection](web.md#browse-import-collections) holding
the files that run added or changed. In `--watch` mode, each scan that uploads
something is one collection.

A one-shot push processes up to 32 files at a time. Outcomes print as uploads
finish, so their order can differ from the source directory's order. Source
names that normalize to the same destination are processed in traversal order.
Watch mode processes files one at a time, after settling by default or
promptly when `--capture-on-change` is selected.

The client prints each acknowledged outcome and a final count. If the daemon
rejects one file, for example because an unrelated entry occupies its
destination name or its mapped document is in trash, push prints that file as
`failed`, continues with the rest, and exits nonzero. Files over the upload
size limit fail the same way, as do files and folders push cannot read, for
example because of their permissions or a read error during upload. In
`--watch` mode an unreadable entry is reported once and pushed when it becomes
readable. If the daemon cannot be reached, answers with a
server error, or loses an acknowledgment, a one-shot push stops with an error
and reports only acknowledged files. Other uploads may already be in flight;
push cancels them and waits for its source readers to finish. Ctrl+C stops the
run and still prints the final count. Run the same command again to resume from
server state. Files already committed do not need another upload. There is no local
cursor database. [Backup and restore](backup.md) preserve push provenance and
source cursors even when a source's accepted version was pruned. A cursor does
not keep those pruned bytes in the backup; backups include bytes still required
by retained content and other authority.

## Switch from a daemon-owned watch

If a `[[watch]]` already imported a synced copy of your folder, push can continue
that archive without uploading the same files again:

1. Remove that watch from the daemon's configuration and restart the
   daemon. Keep the vault and its imported documents. The daemon refuses a push
   name that matches a configured watch.
   If you plan to use push in `--watch` mode, keep the daemon alive as described
   in [Connect and push](#connect-and-push) before removing the last configured
   watch.
2. Start `docbank push` from your working folder with exactly the old watch's
   `name`. Preserve current paths relative to the old watch's source root.
   If a running daemon watch followed a rename to `nested/final.txt`, push that
   file as `nested/final.txt` to adopt its existing node.
3. Check the summary. Files matching the watch's last accepted bytes report
   unchanged, send no upload, and create no new nodes. Changed files append a
   version to the existing node, wherever that node is now.
4. Add `--watch` to keep the push client running. Remove the intermediate synced
   copy when you no longer need it for anything else.

The last accepted watch hash is the starting cursor, even if you later edited
or reverted the archived node. After a changed source is accepted by push, its
push cursor takes precedence over the old watch cursor. `--dest` controls new
identities; it does not relocate imported documents. Choose the old watch's
destination if you want new files beside them. Backup and restore preserve the
switch-over.

## Keep watching

Add `--watch` to keep the client running:

```bash
docbank push ~/documents \
  --to http://127.0.0.1:7778 \
  --api-key-file ~/.config/docbank/archive.key \
  --name laptop-documents --dest /documents \
  --watch --settle-time 30s --scan-interval 5s \
  --minimum-age 0s --exclude cache/ --exclude .DS_Store
```

Watch mode uses the same stability and exclusion rules as a
[daemon-owned watched inbox](importing.md#continuously-ingest-a-local-inbox).
The defaults are a 30-second settle window, a 5-second scan interval, and no
minimum age. Set `--minimum-age 168h` for files that may pause for a long time
between writes. In settled mode, every restart requires a fresh full settle
window. One-shot push reads immediately, while still respecting
`--minimum-age` and exclusions.

Add `--capture-on-change` with `--watch` for prompt archival of readable
working files. The initial file and notification-observed changes bypass only
the settle window; minimum age, exclusions, stable reads, and retry behavior
still apply. For example, add `--capture-on-change --scan-interval 1h` to the
command above. The flag is rejected without `--watch`, before a credential or
daemon connection is resolved.

In settled mode, each periodic scan rechecks accepted file hashes. An
equal-length edit with a restored modification time starts a new settle window
and is then uploaded. In prompt mode, a notification hint can read that changed
file sooner, while periodic scans keep the full digest check. A matching hash
sends no daemon request and leaves an independent Docbank edit intact.
Periodic checks reread local files; increase `--scan-interval` to reduce disk
activity for large folders.

Prompt capture is best effort. Notifications can merge saves, and slow uploads
can miss bytes overwritten before the client reads them. Readable intermediate
content and temporary files can be uploaded unless excluded; the client cannot
recognize an application's logical save boundary. Each event scan still walks
the confined source tree, but only dirty or metadata-changed accepted files
need a content digest. Pending hints hold at most 1,024 paths; overflow
requests a full scan. Native subscriptions use a 32,768-entry-unit budget:
each admitted directory costs one plus its immediate entry count. A backend
or budget failure closes subscriptions, reports degraded polling, and keeps
periodic scans running. A directory that vanishes during enrollment is retried
at the next periodic tick. Polling uses existing digest checks, so a later
same-size, restored-mtime edit remains detectable while the client runs.

Repeat `--exclude` for literal rules. A basename matches anywhere; a relative
path excludes that entry and its descendants from the source root. These rules
are not glob patterns. The scanner skips symlink entries, nonregular files,
and other filesystem mounts. It pins the root and stops if that root is
replaced. A symlink used as the initial folder resolves once before pinning.

Watch mode rereads the daemon's accepted hashes at the start of each scan that
has files to push, so it sees documents moved to trash, purged, or changed by
an upload whose response was lost. If the daemon is unreachable when watch
mode starts, push keeps running and reads them on the first scan that has files
to push. A wrong key or a push name still used by a configured daemon watch
still fails at startup when the daemon answers.

Press Ctrl+C to stop a watch; push prints the final count and exits
successfully unless some files failed. A file the daemon rejects is reported
and retried when it changes or when push restarts. If the daemon is
unreachable, answers with a server error, or loses the connection partway
through an upload receipt, the file is tried again on a later scan (after a
fresh settle window in settled mode), so a restarted tunnel or daemon resumes without
restarting push. A key check cut off by a network error or a server error is
retried the same way. A wrong answer to the key check, a refused key, or a push
name still used by a configured daemon watch stops watch mode with an error;
restart the same command after correcting the problem. A file edited while it
is uploading is not a failure: settled mode waits for a new settle window;
prompt mode retries on a later observation. Both upload the new readable
bytes. If the daemon rejects an upload's hash or size, push rehashes the source to
detect edits even when file size and modification time stay unchanged.
A settle window cannot prove a producer has closed a file, so prefer a
completed-file handoff folder when one is available. Push is one-way archival,
not two-way synchronization.
