# Inspect retained export plans from the CLI and MCP

Status: implemented. Revised after adversarial design review.

Source baseline: `83c7a02c9f2c92204db465497e919ad8b591f281`, after PR #766.
Parent scope: [report/export workflow, #719](https://github.com/kenn-io/docbank/issues/719).

## Outcome and roadmap fit

An operator or local agent with an export plan ID can recover its frozen header
and inspect unavailable outputs without creating another preview. The response
identifies the saved plan and fingerprint, even when current documents or
available renditions have since changed.

This takes the narrow operator outcome from closed
[#583](https://github.com/kenn-io/docbank/pull/583). It does not adopt that PR's
cumulative branch. The CLI and MCP already create explicit, originals-only
plans; the daemon also supports richer plans. These reads expose the daemon's
existing inspection contract to both clients.

The [retained-PDF qualification](2026-10-02-report-export-qualification-design.md)
merged in #766. It qualifies selected PDF reporting, date review, original
export, and two physical restore outcomes, starting with retained evidence.
That evidence does not close every acceptance boundary in #719: attachment
composition, affected maintenance operations, and platform execution must be
assessed on their own evidence. This design makes no broader completion claim.

Three possible continuations were considered:

| Continuation | Value and boundary |
| --- | --- |
| Retained-plan inspection, #583 | Completes inspection of existing export work through two existing reads. Recommended here. |
| Document catalog, coverage, and text reads, #533 | Useful CLI parity, but a separate discovery outcome spanning three response contracts. |
| Minimal PDF redaction | A new production contract requiring decisions about marks, output verification, and retained page evidence. Requires its own design. |

The intended scope is one implementation PR. It adds no export writes, source
kinds, role policies, processing runtimes, endpoints, admission slots, storage
changes, dependencies, capability registry, or redaction behavior. It does not
add a plan listing operation, enumerate all plan members, or promise that an
old plan can still start a new job. The caller must retain the plan ID.
Shared MCP export error messages will also be reworded to cover both reads
and writes, keeping their existing codes and error handling.

## Existing authority and lifetime

Use these existing generated client operations directly:

| Read | Generated client method | Result |
| --- | --- | --- |
| `GET /api/v1/exports/plans/{id}` | `GetExportPlan` | `bundle.Plan` |
| `GET /api/v1/exports/plans/{id}/problems?after=N` | `GetExportOutputProblems` | `bundle.OutputProblems` |

Both routes derive the caller's export owner before accessing the store. CLI
and MCP requests use the daemon API key and share the `master` owner. They can
inspect plans made by other API-key clients, but cannot use these commands to
inspect a browser session's plan. An unknown plan and another owner's plan
both produce `not_found`.

The plan header's `expires_at` is the deadline for admitting a new job. It is
not the current read-retention deadline. A job can keep a plan readable after
that timestamp without changing the frozen header or fingerprint. Reads must
not reject a returned plan based on the client's clock, refresh its admission,
or imply that reading it makes another start possible.

An unextended source or plan has a ten-minute admission window. Job retention
can extend the stored read lifetime. Releasing a terminal job can shorten that
lifetime again; another retained job may still keep the plan. Once retention
expires the store returns `export_expired`; after cleanup removes the record,
it returns `not_found`. No API in this design reports the separate retention
deadline, so neither client invents one or promises a minimum remaining life.

These reads reserve no source, plan, or job slot and extend no lifetime. Existing
limits remain global 32 source/plan records, global eight jobs, and two jobs per
owner. A failed inspection must not replay preview or start automatically.

## CLI contract

Add two commands under the existing `docbank export` group:

```text
docbank export show-plan <plan-id> [--json]
docbank export problems <plan-id> [--after N] [--json]
```

Both accept exactly one canonical UUIDv4 plan ID. `--after` defaults to zero
and accepts an integer from zero through `bundle.MaxOutputProblems`
(2,600,000 at the baseline). Validate the ID and flags before
`daemonconn.Ensure`, following `validateExportID`. Invalid input exits 2
without contacting or starting the daemon. There is no page-size flag.

Each command makes one generated-client read after connection setup. Reuse
the existing export group's `--json` flag and `writeCLIJSON`. Do not add a
forwarding daemon-client wrapper or open the vault from the command.

`show-plan --json` writes the returned `bundle.Plan` directly, with the same
field names, optional-field omission, and values as the existing preview's
JSON header. Human output includes the plan ID, fingerprint, source ID and
kind, source member hash, selected-member total, requested role policies,
role entry and byte totals, metadata bytes, creation time, and admission
deadline. When present, show document-row count, volume limits and count,
duplicate policy, and all output counts. Label `expires_at` as the admission
deadline, not as the last time the plan can be read. Do not print a locally
computed "ready to start" state.

`problems --json` writes the returned `bundle.OutputProblems` directly:
`plan_id`, `fingerprint`, `after`, `next`, `total`, and `items`. Every item has
`node_id`, `version_id`, `role`, and `reason`, with `part_path` when present.
Human output shows the plan identity, total problem count, and the returned
items with those identities and explanations. Quote free text and part paths
using the existing CLI's escaped-string style. When `next` is nonzero, show
the exact continuation command with that value. Do not fetch it automatically.
Distinguish "no unavailable outputs" (`total == 0`) from an empty page at the
end of a nonempty result.

Return existing daemon errors without losing their classification. Successful
reads exit 0; unknown or other-owner plans exit 3; expired retention, a cursor
beyond the total, and an inspection size limit exit 1. Local input errors exit
2. No new exit code or recovery write is introduced.

## Problem paging and bounds

The existing pager returns at most `bundle.InspectionPageSize` (50) scalar
problems per call. `after` is a count of already skipped problems within this
one immutable plan, not a document ID. `next == 0` means the page is terminal.
Use a nonzero `next` exactly as returned; clients do not add one, interpret it
as a row number, or carry it to another plan.

The same document can contribute several problems. An incomplete attachment
inventory also contributes a problem for each requested attachment role,
independently of known child outputs. `total` is a problem count, not a count
of unavailable documents, and need not equal `plan.counts.unavailable`.

If `after == total`, return an empty terminal page. If `after > total`, the
daemon returns `export_conflict`. An originals-only plan with no unavailable
outputs returns `total: 0`, `next: 0`, and `items: []` for `after: 0`.

The store walks frozen document rows to compute the page and total. It does
not re-evaluate current versions or processing coverage. Each read may scan
the complete plan; bounded output does not imply constant-time paging.

The existing canonical size cap is `bundle.MaxMemberBytes` (64 KiB) for a
stored header and for a complete problem page. The daemon returns
`export_limit` if a problem page exceeds that cap; it does not shorten that
page to make it fit. This limitation remains explicit: a caller cannot ask
for fewer items, and releasing jobs cannot shrink the frozen page. Adaptive
paging or a larger response limit would require a separate contract change.
The HTTP problems route has the same cap: switching from CLI or MCP to HTTP
does not make the same oversized page readable. There is currently no way to
request a shorter page at that offset.

## MCP contract

Register two reads in the default catalog, for both stdio and HTTP:

| Tool | Input | Structured result |
| --- | --- | --- |
| `get_export_plan` | Required `plan_id` | Private-cache fields plus `plan: bundle.Plan` |
| `get_export_problems` | Required `plan_id`; optional `after`, default zero | Private-cache fields plus `problems: bundle.OutputProblems` |

These tools require no write opt-in. Their annotations are read-only,
idempotent, non-destructive, and closed-world, following the other catalog
reads. `--allow-export-writes` still controls only the existing five writes.

Use `uuidSchema`, a bounded integer schema for `after`, and closed input
objects. Reject unknown fields, explicit nulls, wrong types, noncanonical
IDs, and offsets outside the CLI's numeric range before invoking a handler.
An omitted offset alone defaults to zero.

Use the existing `daemonRead` connection path, which can resend a read once
after a transport failure before any response begins. It does not replay a
partial response, a domain error, or a canceled call. No operation here uses
the uncertain-write path or returns `export_outcome_unknown`. Route results through
`boundedToolSuccess` with the existing private cache fields (`ttlMs: 0` and
`cacheScope: "private"`). No resource link, download ticket, or filesystem
destination is part of the response.

### Full retained-header schema

The output accepts the full current `bundle.Plan`, not just what
`preview_export` can create. Its originals-only, explicit-source, 1,000-member
schema cannot be reused unchanged. API-key clients can create other valid
plans belonging to the same owner.

The read's closed schema follows the current wire types:

- The required header fields are `format`, `id`, `vault_id`, `toolchain`,
  `source`, `roles`, `fingerprint`, `total`, `role_entries`, `role_bytes`,
  `metadata_bytes`, `created_at`, and `expires_at`.
- Optional header fields are `document_rows`, `volume_limits`, `volumes`,
  `duplicate_policy`, and `counts`. Preserve omission; do not fill absent
  values just for display or change the header before returning it.
- Source fields include all of `bundle.Source`: its nine required fields
  and optional `saved_query_id`, `saved_query_revision`, `query_fingerprint`,
  and `collection_id`. Accepted kinds are `explicit`, `nodes`, `query`,
  `saved_query`, `snapshot`, `upload`, and `mailbox_collection`. A planned
  source is sealed. Its total can reach `bundle.MaxMembers` (100,000).
- Role policies accept the six existing roles: `original`, `text`, `pages`,
  `email_pdf`, `attachment_original`, and `attachment_pdf`. Each policy
  preserves optional `allow_unavailable`, `profile_fingerprint`, and
  `recipe_sha256`. There are one through six policies.
- `volume_limits` has `role_bytes` and `roles`, using the existing volume
  limits. `counts` has all eight fields of `bundle.OutputCounts`.
  A present duplicate policy is `preserve` or `collapse_exact_content`.
- Use existing bundle limits for members, rows, role entries, role bytes,
  metadata bytes, and volumes. Do not impose the preview tool's 1,000-member
  limit. Counts are nonnegative integers; do not invent narrower per-count
  maxima. IDs and hashes use the existing canonical schemas. Other strings
  are bounded by the existing 64 KiB record envelope, without a new restrictive
  field limit that would reject a valid retained header.

The problem result is also closed. It contains the six page fields listed
above, with no more than 50 items. Each item accepts exactly the four required
fields and optional `part_path`. IDs, roles, nonnegative cursor/count values,
and positive node IDs follow the existing wire contract. Reason and part-path
strings must not be truncated or replaced. Keep the preview tool's existing
narrow result contract; share schema fragments only where their rules match.

The 64 KiB daemon objects leave room for both the JSON text and structured
copy under the existing 1 MiB complete MCP-result cap. Keep that final cap and
schema validation in place. A malformed or oversized daemon result fails the
read; there is no partial success or side-effect uncertainty to recover.

### Errors

Preserve the existing MCP domain codes: `not_found`, `export_expired`,
`export_conflict`, `export_limit`, `export_timeout`, `export_canceled`, and
`export_failed`. Invalid tool arguments use the existing protocol validation
error. Transport and invalid-response errors use existing sanitized RPC errors.

**Decision: reword the shared messages.** `domainToolError` calls
`domainErrorMessage(code)` without a tool name. Keep that path; do not add
per-tool overrides, message parameters, or a second error dispatcher. The
following replacements apply to every MCP tool returning these codes:

| Code | Shared message |
| --- | --- |
| `export_conflict` | The export request conflicts with current data, cursor position, or operation state. |
| `export_expired` | The export admission or retention period has ended, or the archive is unavailable. |
| `export_limit` | An export size or capacity limit was reached. |
| `export_timeout` | The export request timed out. |
| `export_canceled` | The export request was canceled. |
| `export_failed` | The export request failed. |

Timeout, cancellation, and failure messages also lose the existing advice to
inspect a job: a plan read may have no job. Error codes, classification,
sanitization, result shape, and retry rules stay unchanged. The CLI continues
to show the daemon's error text.

Keep operation-specific recovery advice in tool descriptions and the owning
guide. For inspection, an expired or removed plan needs a new preview with new
IDs if the operator wants a new export; that does not recreate the old snapshot.
An existing job can instead be inspected through status. A size-limit failure
does not imply that releasing jobs will make the same problem page smaller.
Existing guidance for deliberately releasing terminal jobs and recovering
writes remains in the relevant tool descriptions and guide. No shared message
directs the agent to submit a write automatically.

## Required behavior evidence

Use synthetic vaults and existing daemon/CLI/MCP test fixtures. The new
adapters need observed behavior tests, not source-text assertions or a second
implementation of export selection. Existing store tests remain the proof of
retention and admission; extend them only for a missing behavior.

1. **Recover the exact header.** Create a plan through the daemon, discard the
   local preview result except for its ID, and read it through each new client
   surface. Compare with the original saved header, including fingerprint and
   optional fields. Read again after replacing a source document. Neither
   header nor frozen problem details change.
2. **Exercise real paging.** Create 51 synthetic members with no retained text.
   Use the generated client's `CreateExportSource` and `CreateExportPlan`
   operations to seal an explicit source and a plan with roles
   `[{"role":"original"},{"role":"text","allow_unavailable":true}]`, following
   `TestExportClientFrozenPreview` in `internal/daemonconn/exports_test.go`.
   The CLI and MCP preview commands cannot create this plan; they remain
   originals-only and gain no role flag. Through the actual daemon pager, assert total
   51, page lengths 50 and 1, cursors 50 and 0, exact version identities,
   and no duplicates or omissions. Check `after == total`, `after > total`,
   and a plan with zero problems. Client calls do not fetch extra pages.
3. **Respect existing ownership and lifetime.** Exercise the actual routes
   with the owning API key and another browser owner. Preserve the existing
   fake-clock proof that a job-retained header remains readable after
   admission expires while a new start is refused. Cover expired and removed
   records in the client error mappings without real ten-minute waits.
4. **Accept the full read shape.** Exercise the MCP result validator with
   representative retained headers from existing production plan fixtures:
   non-explicit sources, more than 1,000 selected members, optional source
   fields, attachment role policies, counts, duplicate policy, and volumes.
   Reuse existing fixtures where possible; no provider calls or broad new
   attachment workflow is required. Include a problem with `part_path`.
5. **Keep inputs and output bounded.** CLI invalid IDs and offsets fail before
   daemon discovery. MCP catalog and calls work with all write flags disabled;
   invalid inputs are rejected. A near-cap problem result, including its text
   copy, fits under 1 MiB. A daemon `export_limit` is an error, never a partial
   page. Test transport/schema failures through existing read plumbing.
6. **Make output usable.** Parse JSON as the existing bundle types; assert
   admission wording, problem identities, escaped free text, the correct next
   command, and distinct empty-result/end-of-page messages. Preserve exit codes
   and MCP domain codes through errors.

These are contracts for the implementation's focused tests. They do not call
for duplicating the complete export suite in each client or qualifying new
archive production behavior. Run relevant Go tests with `-tags fts5` in both
SQLite modes, and the repository's normal lint and documentation checks.

## Documentation boundary

Update the owning native-export guide, CLI reference, and MCP guide when the
commands exist. Explain the shared API-key owner, retained ID requirement,
admission versus read lifetime, frozen problem counts, fixed-page size limit,
and read-only retry behavior. State that the HTTP problems route has the same
64 KiB cap and cannot return a shorter page at the same offset. Keep recovery
advice beside the operation it applies to, including write recovery and release;
the shared MCP messages alone do not prescribe a recovery action.
Originals-only CLI/MCP previews normally have no unavailable outputs; richer
same-owner plans may have them.

The owning guides describe the implemented commands and tools. Keep actionable
follow-up work in kata; this document records the reviewed contract.
