---
last_edited: 2026-10-05
title: Document timeline
description: How Docbank derives date claims from stored document evidence.
---

# Document timeline

Docbank derives a timeline index from each retained content version. A
**document event** records what one identified source says about a date for that
version, along with the source, original value, precision, timezone status,
parse confidence, and evidence reference.

A document event is a claim, not proof that something happened. It does not
change document identity, replace the original bytes, or become user-authored
metadata. If two sources disagree, Docbank keeps both claims. Even when two
claims state the same date, Docbank keeps them separate because their evidence
is separate.

Docbank identifies each derivation slot by its content version, source key, and
date kind. When you edit a file, you create a new version with its own
generation, which is the immutable set of events derived for that version. The
earlier version's events stay unchanged.

A superseding provenance correction updates the claim for each version linked to
the original observation without attaching that observation to a newer content
version. Earlier provenance facts remain in the audit history.

## Date kinds

The index uses sixteen closed date kinds. Each kind describes what the source
says, not something Docbank has independently established.

| Kind | Meaning |
| --- | --- |
| `sent` | The source says a message or document was sent at this date. |
| `received` | The source says it was received at this date. |
| `document_date` | A document field supplies a general document date without a narrower meaning. |
| `authored` | The source attributes an authoring date to the document. |
| `created` | The source labels the value as a creation date. |
| `modified` | The source labels the value as a modification date. |
| `captured` | Image, audio, or video evidence labels the value as a capture date. |
| `started` | Calendar or interval evidence supplies a start date. |
| `ended` | Calendar or interval evidence supplies an end date. |
| `printed` | The source labels the value as a print date. |
| `accessed` | The source labels the value as an access date. |
| `produced` | The source labels the value as a production date. |
| `exported` | The source labels the value as an export date. |
| `imported` | Retained provenance records when Docbank imported or observed the source. |
| `observed` | Retained evidence records when an external fact was observed. |
| `vault_recorded` | The content-version record supplies Docbank's own durable fallback date. |

## Precision and timezone honesty

Every event keeps one of seven precisions: year, month, date, hour, minute,
second, or fractional second. Docbank never fills in missing components. A value
such as `2024-06` stays a month, and `2024-06-12` stays a date even when the
source also names a timezone.

Docbank records each event's timezone state as UTC, a numeric offset, a named
zone, omitted, or invalid. It computes a UTC key only for values at hour
precision or finer that carry UTC or a valid numeric offset. A named zone stays
on the event as a floating value because a name alone may be ambiguous, and
omitted or invalid zones also stay floating. Unsupported leap seconds remain
diagnostics, and Docbank never uses the machine's local timezone to complete a
partial claim.

Every valid civil value gets a fixed-width calendar key. That key lets you sort
coarse and floating claims without pretending they are instants.

## Undated versions

Versions with no usable source date stay in the timeline population. The current
recipe records the content version's own `recorded_at` value as a
`vault_recorded` fallback, so a normal retained version still has one
explainable claim when its source metadata has none. Docbank never fills in a
missing source date from the machine clock and never silently removes undated
versions from coverage.

## Primary dates

A primary date is a presentation choice picked from a version's retained claims.
Docbank stores it with the rule ID and a plain-language reason, so callers can
explain the choice. Safe disclosure and full disclosure each have their own
result.

The current rule selects dates in the vault scope. Email, message, calendar,
image, audio and video, and other documents each use an order that fits their
document kind, with `imported` and `vault_recorded` as last-resort fallbacks.
Safe selection skips events marked sensitive and events containing a sensitive
actor, such as a Bcc recipient. Full selection retains those claims.

## Generations and rebuilds

The `document-events/v1` index is a derived SQLite projection. Each immutable
generation binds canonical event bytes to one content version, the deriver
fingerprint, and a digest of all consumed evidence, while a mutable head selects
the generation currently served for that version. Docbank decodes source
metadata before opening the write transaction. During publication, it checks the
input epoch, the exact-version dirty revision, and the immutable evidence
identities in the same transaction, so a stale worker cannot publish over
changed evidence. Events, actors, and primary choices stay together in the
canonical record.

A rebuild advances the input epoch and scans every retained version, including
history and trash. Using the current recipe, the worker reads active
`source-metadata/v1`, exact provenance bindings, and the content-version record,
then publishes either a fresh generation or a bounded failed or unavailable
attempt. Rebuilds do not alter originals, rerun source extraction, call a
rendition provider, or recreate a rendition that derivative purge removed.

When you remove a retained version, Docbank cascades the removal to its dirty
state, attempts, head, and generation, and advances the publication epoch.
Trashing a node keeps all of those records because the version still exists.
When consumed evidence is corrected or removed, Docbank revokes the affected
head and marks that exact version dirty in the same transaction. A rendition
purge does not do that because renditions are not an input to this index.

## Backup and restore

A logical backup ships the authority that the current recipe uses: original
content versions, source metadata, provenance facts, and exact-version
provenance bindings. It omits timeline generations, heads, attempts, dirty
markers, epochs, and rebuild receipts.

During restore, Docbank validates the shipped authority and rebuilds the
projection locally for every retained version, so the restored index reflects
the evidence in that backup rather than disposable SQLite rows copied from the
source vault. A terminal `unavailable` result does not block restore because it
means valid authority exceeded a timeline bound or could not supply a bounded
view, including retained dates outside the index's civil-year range such as year
0000. The original provenance date stays intact. Restore still fails when a
target remains `pending` or `failed`. After recovery, inspect coverage to find
unavailable versions. See [Backup and recovery](backup.md) for the wider restore
boundary.

## Coverage and rebuild APIs

To check current-file coverage, call `GET /api/v1/timeline/coverage`, or call
`Vault.DocumentEventCoverage` from embedded Go code. Both return:

- `selected`: current file versions, including files in trash
- `indexed`: versions with a fresh generation, head, and matching indexed attempt
- `pending`: selected versions that still need current work
- `failed`: current deterministic derivations that failed
- `unavailable`: current derivations blocked by bounded or missing evidence
- `missing_metadata`: versions without active `source-metadata/v1` evidence
- `invalid_dates`: safe stored date diagnostics for the current result
- `unbound_provenance`: versions without an exact provenance binding
- `operational_fallbacks`: indexed versions whose safe vault primary uses `imported` or `vault_recorded`
- `contract_version`, `deriver_fingerprint`, `input_epoch`, and `publication_epoch`: the recipe and freshness fence behind the counts

The four state counts satisfy `selected = indexed + pending + failed +
unavailable`. Diagnostic counters overlap those states and each other.

To start or replay a rebuild through the daemon, send
`POST /api/v1/timeline/rebuilds` with a canonical UUIDv4 `operation_id`, and
read its durable receipt with `GET /api/v1/timeline/rebuilds/{operation_id}`.
The receipt counts every retained version, while the coverage endpoint reports
current file heads. Embedded Go callers use `Vault.RebuildDocumentEvents`, which
drains the same provider-free worker before returning. Receipts update during
the rebuild and when the scan finishes. Synchronous rebuilds stop after three
consecutive listing failures or three failures of the same target. Explicit
rebuilds remain strict indexing requests. Daemon receipts finish `failed` when a
target is failed or unavailable, and the embedded call returns that receipt with
an error while any target is pending, failed, or unavailable.

## Document–person attribution

The daemon also rebuilds document-person links from retained actors, custodian
assignments, and operator decisions. For any version, it waits for event data
before deriving links.

To start or replay a full rebuild, send `POST /api/v1/people/rebuilds` with a
canonical UUIDv4 `operation_id`. You can read progress with
`GET /api/v1/people/rebuilds/{operation_id}` and current-file coverage with
`GET /api/v1/people/coverage`. These responses contain counts, not person
evidence. The rebuild counts every retained version, keeps running through
missing event data and retryable failures, and finishes the receipt as `failed`
on terminal unavailable results.

When you rename, retire, merge, or split a person, Docbank advances the binding
epoch to invalidate derived document links. The daemon backfill then republishes
them after the authority change.

In embedded Go code, call `Vault.RebuildDocumentEvents` before
`Vault.RebuildDocumentPeople`, and read coverage with
`Vault.DocumentPeopleCoverage`. An incomplete rebuild returns its receipt and an
error. Once the prerequisite or failure is resolved, retry with the same
operation ID.

Backups preserve person and custody records, and restore rebuilds the links
without copying old rebuild receipts. See [Backup and recovery](backup.md).
