---
title: Push a folder to another daemon
description: Archive a local folder through a keyed daemon, resume by content hash, and continue an existing watch.
last_edited: 2026-10-10
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

Before uploading, the client reads the last hash accepted for that identity
from the daemon. Equal bytes require no upload. Changed bytes append a version
to the same node, even if you moved or renamed the archived node. An unchanged
source leaves a later archive edit or revert intact. The daemon retains each
source's accepted hash separately from the node's current content and retained
version history. Pruning an accepted version can release its bytes while the
source digest remains available. Re-pushing those same bytes still skips the
upload and leaves the current archive content unchanged; it does not recreate
the pruned version.

Deleting a local file leaves its archived node and history intact. Renaming a
local path gives it a new identity; it is not an archive move. Changing
`--dest` affects new identities only. A mapped node in trash is an error.
Restore that node before pushing the identity again.

The client prints each acknowledged outcome and a final count. If the daemon
rejects one file, for example because an unrelated entry occupies its
destination name or its mapped document is in trash, push prints that file as
`failed`, continues with the rest, and exits nonzero. Files over the upload
size limit fail the same way. If the daemon cannot be reached, answers with a
server error, or loses an acknowledgment, a one-shot push stops with an error
and reports only acknowledged files. Ctrl+C stops the run and still prints the
final count. Run the same command again to resume from server
state. Files already committed do not need another upload. There is no local
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
   `name`. Preserve paths relative to the old watch's source root. For example,
   `nested/notes.txt` must still be `nested/notes.txt` in the pushed folder.
3. Check the summary. Files matching the watch's last accepted bytes report
   unchanged, send no upload, and create no new nodes. Changed files append a
   version to the existing node, wherever that node is now.
4. Add `--watch` to keep the push client running. Remove the intermediate synced
   copy when you no longer need it for anything else.

The last accepted watch hash is the starting cursor, even if you later edited
or reverted the archived node. After a changed source is accepted by push, its
push cursor takes precedence over the old watch cursor. `--dest` controls new
identities; it does not relocate imported documents. Choose the old watch's
destination if you want new files beside them. Do not keep both producers
running against the same identities: either producer can version a changed
source into their shared node. Backup and restore preserve the switch-over.

## Choose what identical files mean

`--duplicates` applies when a **new identity** has the same bytes as the current
version of a live document that this push name already owns. That includes
documents imported by the daemon watch the push took over. Documents from other
imports, uploads, or push names are never duplicate targets, so a push cannot
change them. Historical versions and trashed nodes are not duplicate targets
either. When several owned documents match, Docbank selects the lowest node ID.

| Policy | Result |
| --- | --- |
| `link` (default) | Record the new source provenance against the owned document. Create no extra document. |
| `skip` | Leave the new source unrecorded. Report `duplicate_skipped`. |
| `create` | Create a separate node at the requested destination. |

A linked source may have no entry under its requested destination because the
existing node remains where it is. Linked identities share that node and its
future versions. Each identity still keeps its own accepted hash, so an
unchanged linked source does not undo another source's change. Choose `create`
when files with identical bytes must remain independently editable documents.
Physical content remains deduplicated under every policy.

New identities upload their bytes before the daemon applies the policy. A
skipped duplicate has no resume cursor and is checked again on the next run.
Existing identities keep their node regardless of a later policy change.
`create`, or a new nonduplicate source, fails if an unrelated entry occupies
its exact destination name. Push does not silently overwrite it or add a
collision suffix.

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
between writes. Every restart requires a fresh full settle window. One-shot
push reads immediately, while still respecting `--minimum-age` and exclusions.

Repeat `--exclude` for literal rules. A basename matches anywhere; a relative
path excludes that entry and its descendants from the source root. These rules
are not glob patterns. The scanner skips symlink entries, nonregular files,
and other filesystem mounts. It pins the root and stops if that root is
replaced. A symlink used as the initial folder resolves once before pinning.

Press Ctrl+C to stop a watch; push prints the final count and exits
successfully unless some files failed. A file the daemon rejects is reported
and retried when it changes or when push restarts. If the daemon is
unreachable or answers with a server error, the file waits for a fresh settle
window and is tried again, so a restarted tunnel or daemon resumes without
restarting push. A key check cut off by a network error or a server error is
retried the same way. A wrong answer to the key check, a refused key, or a push
name still used by a configured daemon watch stops watch mode with an error;
restart the same command after correcting the problem. A file edited while it
is uploading is not a failure: push waits for it to settle and uploads the new
bytes. A settle window cannot
prove a producer has closed a file, so prefer a completed-file handoff folder
when one is available. Push is one-way archival, not two-way synchronization.
