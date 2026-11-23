---
title: Push a folder to another daemon
description: Archive a local folder through a keyed daemon, resume by content hash, and continue an existing watch.
last_edited: 2026-10-25
---

# Push a folder to another daemon

Use `docbank push` to archive a local working folder through a keyed daemon.
The client reads your files and uploads verified bytes without changing or
deleting anything locally, and the daemon does not need its own filesystem copy
of the folder.

## Connect and push

By default, the daemon listens on loopback. To reach it from another machine,
forward its API port over SSH:

```bash
ssh -N -L 7778:127.0.0.1:7777 archive-host
```

Next, pass the daemon's configured API key in `DOCBANK_API_KEY`, or store it in
a file that only you can read. A key file takes precedence over the environment
variable. If the daemon uses an ephemeral key, get its current value from the
owner-private runtime record on the daemon host. Keep keys out of command
arguments and source folders, then run `docbank push`:

```bash
docbank push ~/documents \
  --to http://127.0.0.1:7778 \
  --api-key-file ~/.config/docbank/archive.key \
  --name laptop-documents \
  --dest /documents
```

The `--to` flag selects an HTTP or HTTPS daemon origin. Push requires an API
key, refuses redirects and proxies, and neither starts a local daemon nor uses
`DOCBANK_HOME` to pick the destination vault. The target daemon must support
the push routes; an older daemon returns an error. You can also configure a
daemon to listen on a network address, though it sends the key and your files in
cleartext unless you put it behind an encrypted tunnel or HTTPS proxy. See
[bind validation](../configuration.md#bind-validation).

Before sending the key or any file over a new connection, push asks the endpoint
to prove it holds the same API key, without either side sending the key itself.
If the SSH tunnel exits and another local process takes over that port, the
process fails this check and receives neither your key nor your files. Push then
stops with an error saying the endpoint did not prove it holds the API key. This
check cannot catch a process that relays the challenge to the real daemon,
though doing that requires access to the daemon itself. If the path between the
client and the daemon is not trusted, use HTTPS.

A long-running `--watch` also requires the target daemon to stay available. An
automatically started daemon with no configured `[[watch]]`, scheduled packing,
or scheduled backups shuts down after `[server] idle_timeout`, which defaults to
30 minutes, when no API requests arrive. Because push sends no requests while
your folder is unchanged, it cannot wake a daemon that has already stopped.
Before you start a push watch, run `docbank daemon restart`, set
`[server] idle_timeout = "0"`, or run `docbank daemon run` in the foreground.
See [daemon idle shutdown](../architecture/daemon.md#auto-start-and-idle-shutdown).

On the first push, Docbank preserves your folder's relative directory structure
under `--dest`, creating missing virtual directories alongside each new document
and only when a document is created. The client hashes each regular file
locally, and the daemon independently checks the declared SHA-256 and byte count
before committing the content and provenance. If a file changes while the client
reads it, push reports an error or, in watch mode, starts a new settle
observation.

## Resume and preserve versions

Docbank identifies each source file by its push name and slash-separated
relative path. Reuse the same `--name` when you resume a push, reinstall the
client, or move the working folder to another machine, and pick different names
for independent source trees. If you reuse an existing watch's name, push
continues that watch's matching relative-path identities. If you want an
independent push source, choose a new name.

At startup, the client fetches the last hash the daemon accepted for every
identity under that push name, one page at a time. This step also checks your
key and push name before reading any files, so mistakes there fail right away
even with `--watch`.

A one-shot run hashes every local file. In watch mode, periodic scans compare
file identity, size, modification time, and native change time. They reread
changed files, or every file if the filesystem reports no change time, while
notification scans also recheck files named by notifications. When the bytes
match, push skips the upload. When the bytes change, push appends a new version
to the same node even if you moved or renamed that node in the archive. If you
later edited or reverted the archived node, an unchanged source leaves that
change intact.

The daemon stores each source's accepted hash separately from the node's current
content and retained version history. Even if pruning an accepted version
releases its bytes, the daemon keeps the source digest. Re-pushing those same
bytes still skips the upload, leaves the current archive content unchanged, and
does not recreate the pruned version.

Each new identity gets its own document even if another file already has the
same bytes. The vault deduplicates stored bytes anyway, and the
[collection quality report](web.md#browse-import-history) counts duplicate
content. Deleting a local file leaves its archived node and history untouched,
while renaming a local path creates a new identity rather than moving the
archived document. Changing `--dest` affects new identities only. If a mapped
node is in trash, push reports an error; restore that node before pushing the
identity again. A new identity also fails if an unrelated entry already occupies
its exact destination name. Push does not overwrite that entry or add a
collision suffix.

Each run creates one [import collection](web.md#browse-import-history)
containing the files added or changed during that run. In `--watch` mode, each
scan that uploads something becomes one collection.

A one-shot push processes up to 32 files at a time and prints outcomes as
uploads finish, so output order can differ from your source directory's order.
When source names normalize to the same destination, push processes them in
traversal order. Watch mode processes files one at a time, either after settling
by default or promptly if you pass `--capture-on-change`.

Push prints each acknowledged outcome and a final count. If the daemon rejects a
file, for example because an unrelated entry occupies its destination name or
its mapped document is in trash, push prints that file as `failed`, continues
with the rest, and exits nonzero. Files over the upload size limit fail the same
way, as do files and folders push cannot read, for example because of
permissions or a read error during upload. In `--watch` mode, push reports an
unreadable entry once and uploads it when it becomes readable.

If a one-shot push cannot reach the daemon, receives a server error, or loses an
acknowledgment, it stops with an error and reports only the files already
acknowledged. Push cancels any other uploads in flight and waits for its source
readers to finish. Because the daemon acknowledges a file only after committing
it and push keeps up to 32 uploads in flight, a busy or slow daemon can take a
while to answer. Push waits up to five minutes for each acknowledgment before
treating the answer as lost.

Pressing Ctrl+C stops the run and still prints the final count. To resume from
server state, run the same command again. Push keeps no local cursor database,
and files that already committed do not upload again.
[Backup and restore](backup.md) preserve push provenance and source cursors even
when a source's accepted version was pruned. A cursor does not keep those pruned
bytes in the backup; backups include the bytes still required by retained
content and other authority.

## Switch from a daemon-owned watch

If a daemon `[[watch]]` already imported a synced copy of your folder, you can
continue that archive with `docbank push` without uploading unchanged files
again:

1. Remove that watch from the daemon's configuration and restart the daemon,
   keeping the vault and its imported documents. The daemon refuses a push name
   that matches a configured watch. If you plan to run push with `--watch`, keep
   the daemon alive as described in [Connect and push](#connect-and-push) before
   removing the last configured watch.
2. Start `docbank push` from your working folder with the exact `name` of the
   old watch, preserving current paths relative to the old watch's source root.
   If a running daemon watch followed a rename to `nested/final.txt`, push that
   file as `nested/final.txt` to adopt its existing node.
3. Check the summary. Files matching the watch's last accepted bytes report
   unchanged, send no upload, and create no new nodes. Changed files append a
   version to the existing node, wherever that node is now.
4. Add `--watch` to keep the push client running, and remove the intermediate
   synced copy when you no longer need it for anything else.

Push starts from the watch's last accepted hash even if you later edited or
reverted the archived node. Once push accepts a changed source, its push cursor
takes precedence over the old watch cursor. `--dest` controls new identities and
does not relocate imported documents. If you want new files beside them, choose
the old watch's destination. Backup and restore preserve this switch-over.

## Keep watching

To keep the client running and watch for changes, add `--watch`:

```bash
docbank push ~/documents \
  --to http://127.0.0.1:7778 \
  --api-key-file ~/.config/docbank/archive.key \
  --name laptop-documents --dest /documents \
  --watch --settle-time 30s --scan-interval 5s \
  --minimum-age 0s --exclude cache/ --exclude .DS_Store
```

Watch mode follows the same stability rules as a
[daemon-owned watched inbox](importing.md#continuously-ingest-a-local-inbox),
except that push does not apply the watch default exclusions. Use repeated
`--exclude` flags to skip specific literal basenames or source-relative paths;
push does not accept the glob patterns that watch defaults use. By default,
watch mode uses a 30-second settle window, a 5-second scan interval, and no
minimum age. For files that may pause for a long time between writes, set
`--minimum-age 168h`. In settled mode, every restart requires a fresh full
settle window. One-shot push reads files immediately while still honoring
`--minimum-age` and exclusions.

To archive readable working files promptly, combine `--capture-on-change` with
`--watch`, for example by adding `--capture-on-change --scan-interval 1h` to the
command above. The initial file and notification-observed changes bypass only
the settle window; minimum age, exclusions, stable reads, and retry behavior
still apply. Push rejects `--capture-on-change` without `--watch` before
resolving credentials or connecting to the daemon.

Periodic scans compare each file's identity, size, modification time, and native
change time, keeping accepted hashes for unchanged files without rereading their
bytes. When an edit keeps the same byte length and restores the modification
time, push starts a new settle window and uploads the file if the native change
time reveals it. On filesystems where change time follows modification time,
including FAT/exFAT on Linux and rclone mounts, that kind of edit can go unseen
while `push --watch` runs in settled mode. In prompt mode, a notification hint
can read that changed file sooner.

On filesystems that report no change time, periodic scans reread every file to
catch those edits. For large folders on those filesystems, increase
`--scan-interval` to reduce disk activity. Metadata-only changes, such as
permissions, extended attributes, or a hard link, also update the change time.
The file settles again, gets reread, and is checked against the daemon. If the
hash still matches, push skips the upload and leaves any independent Docbank
edit intact.

Prompt capture is best effort. Notifications can merge saves, and slow uploads
can miss bytes overwritten before the client reads them. Push can also upload
readable intermediate content and temporary files unless you exclude them; the
client cannot recognize an application's logical save boundary. Each event scan
still walks the confined source tree, hashing only dirty or metadata-changed
accepted files. Pending hints hold at most 1,024 paths, and overflow requests a
full scan. Native subscriptions use a 32,768-entry-unit budget, where each
admitted directory costs one unit plus its immediate entry count. If a backend
or budget failure occurs, push closes subscriptions, reports degraded polling,
and keeps periodic scans running. If a directory vanishes during enrollment,
push retries it at the next periodic tick. Polling uses the file metadata and
digest checks described above, with the same change-time limits.

Repeat `--exclude` to add literal rules, which are not glob patterns. A basename
matches anywhere, while a relative path excludes that entry and its descendants
from the source root. The scanner skips symlink entries, nonregular files, and
other filesystem mounts. It pins the root and stops if that root is replaced. If
the initial folder itself is a symlink, push resolves it once before pinning.

At the start of each scan that has files to push, watch mode rereads the
daemon's accepted hashes so it sees documents moved to trash, purged, or changed
by an upload whose response was lost. If the daemon is unreachable when watch
mode starts, push keeps running and reads those hashes on the first scan that
has files to push. When the daemon does answer at startup, a wrong key or a push
name still used by a configured daemon watch still fails immediately.

To stop a watch, press Ctrl+C. Push prints the final count and exits
successfully unless some files failed. When the daemon rejects a file, push
reports the failure and retries the file when it changes or when push restarts.
If the daemon is unreachable, returns a server error, or loses the connection
partway through an upload receipt, push tries the file again on a later scan,
after a fresh settle window in settled mode, so a restarted tunnel or daemon
resumes without restarting push. Push retries a key check cut off by a network
or server error the same way. By contrast, a wrong answer to the key check, a
refused key, or a push name still used by a configured daemon watch stops watch
mode with an error. Fix the problem and restart the same command.

Editing a file while it uploads is not a failure. Settled mode waits for a new
settle window, prompt mode retries on a later observation, and both upload the
new readable bytes. If the daemon rejects an upload's hash or size, push
rehashes the source to detect edits even when file size and modification time
stay unchanged.

A settle window cannot prove that a producer has closed a file, so prefer a
completed-file handoff folder when one is available. Push is one-way archival,
not two-way synchronization.
