---
last_edited: 2026-09-13
title: Document Timeline
description: How Docbank derives date claims from retained document evidence.
---

# Document timeline

Docbank derives a timeline index from each retained content version. A
**document event** means that one identified source says a date about that
version. It is a claim with its source, original value, precision, timezone
status, parse confidence, and evidence reference attached.

An event is not proof that something happened. It does not change document
identity, replace the original bytes, or become user-authored metadata. Two
sources can disagree, and Docbank keeps both claims. Even when two claims have
the same date, they stay separate because their evidence is separate.

Each derivation slot is identified by the exact content version, source key,
and date kind. Editing a file creates a new version with its own generation;
it does not rewrite the earlier version's events.

A superseding provenance correction updates the claim for each version linked
to the original observation. It does not attach that observation to a newer
content version. The earlier provenance facts remain in the audit history.

## Date kinds

The index uses sixteen closed date kinds. Each describes what the source says,
not what Docbank has independently established.

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

An event preserves one of seven precisions: year, month, date, hour, minute,
second, or fractional second. Missing components are never filled in. A date
such as `2024-06` stays a month; `2024-06-12` stays a date even when the source
also names a timezone.

Timezone state is recorded as UTC, a numeric offset, a named zone, omitted, or
invalid. Docbank computes a UTC key only for hour-or-finer values with UTC or a
valid numeric offset. A named zone is preserved but remains floating because a
name alone may be ambiguous. Omitted and invalid zones also remain floating.
Unsupported leap seconds remain diagnostics. The machine's local timezone is
never used to complete a partial claim.

Every valid civil value gets a fixed-width calendar key. That key keeps coarse
and floating claims sortable without pretending they are instants.

## Undated versions

The model keeps versions with no usable source date as part of the population.
The current recipe records the content version's own `recorded_at` value as an
explicit `vault_recorded` fallback, so a normal retained version still has one
explainable claim when its source metadata has none. Missing source dates are
never filled from the machine clock or silently removed from coverage.

## Primary dates

A primary date is a presentation choice over the retained claims. It is stored
with the rule ID and a plain-language reason, so a caller can explain the
choice. Safe and full disclosure each have their own result.

The current rule selects dates in the vault scope. Email, message, calendar,
image, audio/video, and other documents each use an order that fits the
document kind. `imported` and `vault_recorded` are last-resort fallbacks.
Safe selection excludes events marked sensitive and events containing a
sensitive actor, such as a Bcc recipient. Full selection retains those claims.

## Generations and rebuilds

The `document-events/v1` index is a derived SQLite projection. Each immutable
generation binds canonical event bytes to one exact content version, the
deriver fingerprint, and a digest of all consumed evidence. A mutable head
selects the generation currently served for that version. Publication checks
the input epoch, exact-version dirty revision, and immutable evidence
identities in the same transaction, so a stale worker cannot publish over
changed evidence. Source metadata is decoded before the write transaction.
Events, actors, and primary choices stay together in the canonical record.

A rebuild advances the input epoch and scans every retained version, including
history and trash. The current recipe reads active `source-metadata/v1`, exact
provenance bindings, and the content-version record. It then publishes a fresh
generation or a bounded failed or unavailable attempt. A rebuild does not
alter originals, rerun source extraction, call a rendition provider, or
recreate a rendition that derivative purge removed.

Removing a retained version cascades its dirty state, attempts, head, and
generation, and advances the publication epoch. Trashing a node
keeps them because the version still exists. When consumed evidence is corrected or removed,
Docbank revokes the affected head and marks the exact version dirty in the same
transaction. Rendition purge does not do that because renditions are not an
input to this index.

## Backup and restore

A logical backup ships the authority used by the current recipe: original
content versions, source metadata, provenance facts, and exact-version
provenance bindings. It deliberately omits timeline generations, heads,
attempts, dirty markers, epochs, and rebuild receipts.

Restore validates the shipped authority and rebuilds the projection locally
for every retained version. The restored index therefore reflects the evidence
in that backup rather than copying disposable SQLite rows from the source
vault. A terminal `unavailable` result does not block restore because it means
valid authority exceeded a timeline bound or could not supply a bounded view.
This includes retained dates outside the index's civil-year range, such as
year 0000. The original provenance date remains intact.
Restore still fails when a target remains `pending` or `failed`. Inspect
coverage after recovery to find unavailable versions. See
[Backup and recovery](backup.md) for the wider restore boundary.

## Coverage and rebuild APIs

`GET /api/v1/timeline/coverage` reports current-file coverage. Embedded Go
callers use `Vault.DocumentEventCoverage`. Both return:

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

Daemon callers start or replay a rebuild with `POST
/api/v1/timeline/rebuilds`, supplying a canonical UUIDv4 `operation_id`, and
read its durable receipt with `GET
/api/v1/timeline/rebuilds/{operation_id}`. The receipt counts every retained
version, while the coverage endpoint reports current file heads. Embedded Go
callers use `Vault.RebuildDocumentEvents`; it drains the same provider-free
worker before returning. Receipts update during the rebuild and when the scan
finishes. Synchronous rebuilds stop after three consecutive listing failures
or three failures of the same target. Explicit rebuilds remain strict indexing requests:
daemon receipts finish `failed` when a target is failed or unavailable, and
the embedded call returns that truthful receipt with an error while any target
is pending, failed, or unavailable.
