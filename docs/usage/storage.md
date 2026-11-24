---
title: Multi-store storage
description: Keep document bytes local first, add filesystem or S3-compatible stores, and move verified content between them.
---

# Multi-store storage

Add a secondary store when you need another location for document bytes or
want to free space in the vault. Every new write goes first to the built-in
filesystem store, called the primary. From there, you can copy or move
verified content to a secondary filesystem or S3-compatible store.

Docbank records which stores hold a verified copy of each retained content
hash. This catalog record, called authority on this page, authorizes a store
to serve those bytes. Moving content between stores manages capacity. It does
not synchronize arbitrary filesystem changes, manage bucket lifecycle rules,
or replace a complete [backup](backup.md).

![The Docbank web application showing the primary and a secondary physical store for a synthetic vault.](https://docbank.ai/assets/generated/web-multi-store-storage.png)

The web application and TUI show this inventory as read-only. Registration,
placement, repair, takeover, evacuation, and removal are CLI or master-key API
operations.

Photo checkouts hold source versions while queued, running, or completed.
Failed and cancelled checkouts release unpublished source holds and their
destination reservation, while published and uncertain copies retain their
source bases until retirement. Backup preserves those holds through restore.
For filesystem stores, Docbank checks checkout separation at daemon start and
at registration. An overlapping store stays unattached at startup until a
background check confirms separation, and registration rejects overlapping
roots. Released checkout folders, including retired ones, stay protected until
they are empty or missing. See
[Create writable copies](photos.md#create-writable-copies).

## Configure a binding

A binding tells this machine how to reach a store, including its path or S3
connection settings and credential profile. Docbank keeps paths, endpoints,
buckets, prefixes, and credentials out of portable metadata, audit history,
browser sessions, and backup placement manifests. See
[Configuration](../configuration.md) for the binding fields.

```toml
# $DOCBANK_HOME/config.toml
[store_bindings.archive]
kind = "filesystem"
path = "/Volumes/docbank-archive"
priority = 20

[store_bindings.cold]
kind = "s3"
endpoint = "https://objects.example.net"
region = "us-east-1"
bucket = "documents"
prefix = "docbank/cold"
credential_profile = "docbank"
priority = 40
force_path_style = true
```

S3 endpoints must use HTTPS, including services reached through loopback.
Docbank has no production switch for plaintext S3 because a TCP port does not
prove which local process received the ownership marker or document bytes.
Use the service's authenticated TLS endpoint or an owner-confined,
authenticated proxy.

!!! warning "Secondary stores contain readable document bytes"
    Docbank verifies secondary objects but does not encrypt them. Anyone with
    raw read access to a filesystem store or S3 prefix can decode the documents
    without the daemon API key. Restrict that access to the vault owner, and
    use independently controlled filesystem, bucket, or KMS encryption when the
    storage operator is outside the intended trust boundary.

Restart the daemon after editing `config.toml`. Binding configuration is read
once at startup. When a storage command names a binding that the running
daemon has not loaded, or when the daemon has no binding registry, the daemon
fails with `storage_configuration_stale` and asks you to restart it after
updating `config.toml`. It does not detect edits to a binding it already
loaded, and it keeps using the values it read at startup.

## Attach and inspect a store

Registration is preview-first because it writes an ownership marker into the
target namespace:

```bash
docbank storage add cold --binding cold
docbank storage add --run --token <preview-token>

docbank storage list
docbank storage status cold --refresh
```

Select a store by its stable UUID when you need a selector that still points
to the same store after a rename. Docbank always treats a canonical UUID
selector as an ID rather than a display name. Passing `--refresh` checks the
ownership marker again, whereas ordinary status uses the daemon's recorded
observations without making a network request on every read.

To transfer a store namespace from another vault instance, pass `--takeover`.
A namespace is the filesystem directory or S3 prefix reserved for that store.
Takeover writes a new ownership epoch, which identifies the current owner, and
blocks the former owner from using the store normally. To the former owner,
the store is now fenced. Two live vaults cannot share one prefix this way.

Each active secondary must own a disjoint namespace. Filesystem paths may not
overlap the vault, a watched inbox, or another active filesystem store, and S3
stores may not use equal or nested prefixes under the same canonical endpoint
and bucket. The ownership marker is the authoritative check, while path and
prefix comparisons reject obvious aliases before Docbank contacts the backend.

Watches and filesystem stores that overlap checkout folders wait and log a
warning. Released checkout folders stay protected while they contain files or
Docbank cannot inspect them. See
[Create writable copies](photos.md#create-writable-copies).

`storage list` and `storage status` separate catalog authority from observed
availability. `authoritative_objects` counts the verified locations recorded
for a store, and `unreadable_objects` counts objects for which every
authorized location is currently offline, so two unavailable replicas do not
look readable. Because their recovery actions differ, Docbank reports missing,
corrupt, fenced, unavailable, and unbound states separately.

### Understand primary coverage

You can use two reports to check primary coverage:

```bash
docbank vault info --json
docbank storage list --json
```

Compare `authoritative_objects` for the store whose `role` is `primary` with
`tracked_blobs` from `docbank vault info`. A lower primary count warns you
that content is held elsewhere, but matching counts are not proof. The two
endpoints read separate live snapshots, and watches, ingests, clients, or
storage jobs can change placement before shutdown. `sole_authority_objects`
cannot prove coverage either, because two secondary replicas can make neither
one a sole copy while the primary still has no location. For a complete,
topology-independent recovery point, run `docbank backup create`. It verifies
one authorized location for every logical blob or fails without publishing a
partial backup.

## Place retained content

To copy content while keeping it on the primary, preview the placement and
then run it:

```bash
docbank storage place /archive/closed-projects --to cold
docbank storage place --run --token <preview-token>
```

To request source retirement after the destination is published, read back,
and SHA-256 verified, add `--move`:

```bash
docbank storage place /archive/closed-projects --to cold --move
```

The preview reports logical bytes, bytes requiring transfer, verification
read-back, remote egress, local scratch, shared-reference constraints,
audit-pinned bytes, and immutable-pack bytes that need a later repack. Before
committing catalog changes, the daemon rechecks every object. If documents
changed during the transfer, it may reclaim less source space while keeping
the verified destination copy valid.

Audited content stays on the primary by default. To allow remote-only audited
retention, pass `--allow-audited-remote-only` in the preview. That
acknowledgement means Docbank never authorizes deletion, though it cannot
prevent deletion by bucket lifecycle rules, storage administrators, or lost
credentials.

## Recover offline, damaged, or taken-over stores

Docbank tries the locations recorded for each blob in stable priority order,
and an unavailable redundant store does not block a healthy copy. Missing and
corrupt locations are reported separately and demoted immediately for the
current daemon run. The authority recorded in the catalog changes only when
you run a repair.

```bash
docbank storage repair <sha256> --store cold
docbank storage repair --run --token <preview-token>
```

Repair republishes verified bytes from another readable location. If another
vault has taken over a store's ownership marker, use salvage to recover bytes
into the primary; salvage only reads from that store:

```bash
docbank storage salvage <sha256> --store cold
docbank storage salvage --run --token <preview-token>
```

Salvage never restores ordinary authority to the fenced store. Every operation
has a durable ID, and you can inspect interrupted or uncertain work with
`docbank jobs` and `docbank jobs show <operation-id>`.

Physical deletion happens after catalog authority moves. If that cleanup fails
temporarily, the operation returns to the durable queue with the failure
visible, and the daemon retries it. The operation is never left looking as if
it is running after its worker has stopped, and the verified destination
remains authoritative during the retry.

## Evacuate and remove

```bash
docbank storage evacuate cold
docbank storage evacuate --run --token <preview-token>
docbank storage detach cold
docbank storage unregister cold
```

Evacuation copies every object the secondary holds into the primary, not only
the objects that no other store holds. It verifies the destination, then
removes the secondary locations from the catalog. Packed bytes on the
secondary are retired only after the complete store handoff. After its cleanup
finishes, a successful evacuation detaches the store itself. Detach removes an
empty store from runtime use and preserves its identity, and running it on a
store that is already detached changes nothing. Unregister is the separate
final action and is accepted only for an empty detached store.

## Backup and restore

Every successful backup is complete, even when the live vault is remote-only.
Docbank reads and verifies one candidate for every logical blob. If a blob's
only location is unavailable, the snapshot fails instead of publishing a
partial recovery point.

The snapshot carries a deterministic `docbank-placement-v1` artifact
containing source store IDs, display names, backend kinds, roles, per-hash
source store IDs, and aggregate counts. It excludes deployment paths,
endpoints, buckets, prefixes, credential profiles, ownership epochs,
encodings, and pack coordinates.

By default, restore puts all recovered content into a fresh local primary
store. To restore selected content to other stores, provide a mapping file
that only the owner can access:

```toml
version = 1

[[stores]]
source_id = "4d9c1a61-f8c4-4c17-99f4-b30dd2f7d8a2"
name = "restored archive"
binding = "cold"
takeover = false
remote_only = false
allow_audited_remote_only = false
```

```bash
docbank backup restore --target ~/Restores/docbank \
  --store-map ~/.config/docbank/restore-stores.toml
```

On Unix, the mapping file must be owned by the current user and mode `0600` or
stricter. Windows requires an owner-restricted DACL. Docbank refuses a final
symlink/reparse point and validates the complete map before contacting a
backend. Target stores receive fresh IDs and epochs, and existing identical
objects may be adopted only after full read-back verification.

Mapped filesystem namespaces must also be disjoint from the running source
vault and the backup repository. Before publishing the restored vault, Docbank
writes a minimal owner-private `config.toml` containing the selected target
bindings. If the target already has a configuration file, Docbank requires
those bindings to match and never rewrites it. A `remote_only` restore
therefore cannot retire the restored primary and then reopen with its only
authoritative stores unbound.
