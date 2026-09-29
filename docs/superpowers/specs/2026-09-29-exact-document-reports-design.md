# Reports for selected document versions

> **Proposed design; not implemented.** The maintainer has confirmed current,
> whole-document selection and preservation of captured reports after source
> changes. The interface and protocol below are proposed for adversarial review.
> Source baseline: `b184ebfc4888f8f4cb6a59dbbb52573134a6bf39`.

## Purpose and scope

An operator selects documents in the vault workspace and runs the existing
search-term report against exactly those versions. An automation caller can
submit the same selection through the existing CLI or HTTP endpoint. Changing
a selected document before capture must produce an explicit conflict, never a
report against a silently substituted version.

This is one bounded outcome from [the backlog umbrella](https://github.com/kenn-io/docbank/issues/719).
[The merged export work](https://github.com/kenn-io/docbank/pull/403) already
covers sealed snapshot load-file packages. This proposal extends search
exports; it does not combine those two export systems.

The maintainer selected two rules:

- Select current, live versions of whole documents. Do not report against
  historical versions, sealed package snapshots, or selected pages. Do not
  automatically add attachments.
- Preserve captured reports as historical evidence when source documents
  change or disappear. Keep the existing 30-minute artifact lifetime and
  durable request/summary history. New runs validate their selection again.

The second rule replaces the umbrella issue's proposed live-source-withdrawal
rule for these reports. Existing session ownership and expiry still apply.
There is no new artifact archive, retention hold, revocation mechanism,
provider integration, generic selection service, or MCP/TUI reporting surface.
Other discovery, citation, redaction, and production proposals need their own
design discussions. This document is design context, not an implementation
tracker; actionable work belongs in kata.

## Operator experience

Add **Report selected documents** to the existing workspace selection actions.
It opens the existing search-export drawer with a copied selection and a
visible document count. Keep the existing sidebar entry for all-document and
collection reports.

The action requires a nonempty selection of file rows with node ID, content
version ID, and content hash. Use the selected rows on the displayed page,
including query-result pages. Do not expand to all query matches. A query
snapshot can supply row identities; its ID is not a report scope and does not
authorize reporting historical content. Folders or rows without complete
identities prevent the action rather than being silently omitted.

Copy the row identities when the operator invokes the action, before awaiting
the existing authenticated vault-ID lookup. Discard the pending action if its
browser session changes. Background
refreshes and later workspace selections must not replace them. Show
“Selected documents (N)” as a fixed scope for that draft. The operator can
start a different draft from the sidebar or a new workspace selection. A
conflict asks them to refresh the workspace and reselect; it must not offer a
retry that silently binds current versions.

Keep the drawer's existing distinction between edited input and the last
submitted result. **Use as draft** from history copies the exact selected
identities and clears date choices. It does not replace them with current
identities. History labels show the selected-document count, and the draft
notice explains that this scope is fixed. Running that draft takes a new
observation and can fail because
the documents have changed. It can also produce different results when
metadata, family relationships, or processing evidence have changed.

Automation uses `docbank search-export create --input request.json --output
report.zip` and `POST /api/v1/search-exports`. Existing verification, CSV
extraction, date review, and download commands remain the operator tools.

## Request contract

Extend `report.Request` with one optional `selected_documents` object. Reuse
`report.Identity` for each document. For example, this synthetic request uses
one exact version:

```json
{
  "version": 1,
  "all_documents": false,
  "selected_documents": {
    "vault_id": "10000000-0000-4000-8000-000000000001",
    "documents": [
      {
        "node_id": 42,
        "version_id": "20000000-0000-4000-8000-000000000002",
        "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
      }
    ]
  },
  "timezone": "UTC",
  "coverage_mode": "strict",
  "terms": [
    {
      "number": 1,
      "expression": "agreement",
      "syntax": "simple",
      "dates": {"start": "2026-01-01", "end": "2026-12-31"}
    }
  ]
}
```

Exactly one scope must be active: `all_documents: true`, a nonempty
`collection_ids` list, or a nonnull `selected_documents` object. Omission or
null means no selected-document scope; an object with an empty document list
is invalid. Never infer all documents from an empty selection.

Selected scope requires a canonical lowercase UUIDv4 vault ID, 1–50,000
document identities, positive node IDs, canonical lowercase UUIDv4 version
IDs, and 64-character lowercase hexadecimal SHA-256 hashes. Reject repeated
node IDs, including identical repetitions or multiple versions of one node.
The existing 8 MiB request limit also applies; the member limit is a ceiling,
not a guarantee that every request of that count fits.

`NormalizeRequest` validates structure without reading a vault. It must own
the nested selection object and document slice, just as it already copies
other request slices. Normalize selected identities into ascending node order;
term order and date cutoffs remain untouched. Apply the same ownership rule
to cache, frame, and history draft copies. No mutation of caller-owned input
may alter a captured report.

## Capture and counting

Admission occurs within `MaterializeTermReportFrame`'s existing lexical
generation and SQLite read snapshot. Match the request vault ID and resolve
every selected identity against a live file's current version in that same
observation. The complete requested set must equal the captured member set.

One missing, trashed, replaced, or hash-mismatched member rejects the entire
request. A version with the same bytes but a different version ID still
conflicts. A rename, move, or tag change alone does not invalidate content
identity; metadata and query dependencies are observed at capture time.
Do not validate through a separate preflight read and then capture latest
versions. Do not drop unavailable members or turn a conflict into an empty
successful report.

Keep the existing capture gate while reading retained evidence. It already
protects physical authority during preparation; it does not freeze all vault
mutations for the report's lifetime. After preparation, counts and date
revisions use the captured frame without consulting current source state.
No report retention reference may prevent a later source deletion or prune.

Use the existing search, date, coverage, and family calculations. Selected
scope changes the report population, not search syntax or date semantics.
Missing search/date evidence follows existing `strict` and `available_only`
rules. In the latter mode, the member remains in the evidence packet with its
coverage diagnostics even when it cannot contribute a count. Identity
conflicts fail in both modes. No processing provider is invoked to fill gaps.

Both `report.Calculate` and offline `verifyFrameEvidence` currently treat
every non-all-document scope as a collection scope. They must distinguish
the new scope explicitly and enforce its exact set and vault binding.
Collection witnesses remain required only for collection scope. Keep the
existing all-document and collection behavior.

### Family evidence does not expand the population

Count only selected members. A known, unselected attachment does not become
a report member and is not itself a missing-evidence error.

Keep existing family traversal through related current documents. An
unselected attachment can connect two selected parents, so deleting all
relationships outside the selection would change family counts. The packet
may therefore contain relationship references to unselected identities.
It must not contain member rows, text bindings, or date evidence for those
unselected documents. An actually incomplete family publication still uses
the existing coverage rules.

The UI and guide must say that selected scope controls counted documents;
family relationship references can identify other documents. This feature
does not introduce a delegated-access or redaction boundary.

### Failure responses

| Condition | HTTP response | Operator action |
| --- | --- | --- |
| Missing/mixed scope, empty selected object, malformed identity, duplicate node | 422 `invalid_report_request` | Correct the request or selection. |
| Wrong vault, unavailable node, or version/hash mismatch at capture | 409 `report_selection_changed` | Refresh and explicitly select the intended current versions. |
| More than 50,000 selected identities or other report resource limit | 413 `report_limit` | Narrow the selection. |

Transport JSON-size/decoding failures keep the API's existing handling.
Existing collection, coverage, date-review, expiry, and capacity errors remain unchanged.
New scope validation must not fall through to a generic server error. Failed
admission creates neither a reusable report handle nor a successful history
receipt. Do not invent a report-specific retry or partial-success protocol.

## Evidence and offline verification

Keep the `search-export-v1` ZIP entries and request version 1. Include selected
scope in the existing manifest request. Omit the new field when unused, so
existing scopes retain their request encoding. This is an additive request
extension; older binaries need not understand the new scope. Current binaries
must continue to read existing packets and history without that field.

Offline verification must establish equality of the manifest's selected
identity set and its member identity set, and equality of the request's vault
ID and the frame's vault ID. Checking only that each member is allowed would
miss a packet that omitted selected documents. Reject extra, missing,
duplicate, or substituted members even if ZIP digests and CSV counts have
been recomputed. Family references remain subject to their existing graph
checks and are not additional members.

Verification still proves internal consistency, not source authenticity or
that the supplied selection reflects an external operator's intent. Changing
both the request and all evidence coherently creates a different internally
consistent packet; there is no new signature or trusted external receipt.
The packet does not contain complete source text or rerun original searches.

## Lifetime, history, and restore

Source replacement, trash, or pruning after capture does not invalidate the
captured report. Completed downloads retain their meaning as historical
evidence. Live handles, date pages, and downloads retain the existing expiry
30 minutes after observation and are lost on daemon restart. Date revisions
use the same frame and original deadline; they do not extend it. Existing
owner/session checks and session invalidation continue to apply.

History keeps the existing maximum of 100 request/summary receipts and
existing size bounds. It retains exact selection and summary, but not full
source text, live artifacts, or date-review choices. No new table, foreign key
to selected source versions, SQLite constraint, or storage migration is needed
for the proposed optional field in the existing request JSON.

Reading history and importing metadata validate request structure, not whether
its sources still exist or remain current. A historical receipt must survive
source deletion and backup/restore. Metadata restore already preserves vault,
node, and version identity. After restore, a new report succeeds only if its
selected sources are live/current and its evidence satisfies the requested
coverage policy. History never restores old handles or guarantees identical
results from a fresh run. Keep metadata JSONL at version 1.

## Existing components to extend

| Concern | Owning code |
| --- | --- |
| Request shape, copies, and scope checks | `report/types.go`, `report/request.go`, `report/counts.go`, `report/verify.go` |
| Current-version capture and family evidence | `internal/store/term_report_frame.go`, `internal/store/term_report_families.go` |
| Frozen evidence and handle lifecycle | `internal/reporting/service.go`, `internal/reporting/cache.go` |
| Durable receipt validation and restore | `internal/store/term_report_history.go`, `internal/store/metadata.go` |
| HTTP contract and generated client shapes | `internal/api/routes_term_reports.go`, existing API generation targets |
| Web selection and report draft | `frontend/src/App.svelte`, `frontend/src/TermReportDrawer.svelte` |
| CLI JSON input and packet verification | `cmd/docbank/report.go` |

The web's mutation selection type carries node/revision pairs, which are not
content identities. Copy version/hash fields from displayed document or query
rows, following the existing export action's pattern. Do not inherit the
separate package export's larger member limit or add its job machinery.
Update the owning search-export guide and HTTP reference when behavior ships;
this proposed spec stays outside public navigation and site output.

## Behavioral examples for review

These are contract examples for the eventual implementation, not verification
claims about the current code:

- With two selected documents and an unselected matching document, both selected
  identities appear in the packet; only their eligible hits affect counts.
  A selected document with missing evidence remains visible in available-only
  coverage rather than disappearing from the population.
- Replacing or trashing either selected document before capture rejects the
  whole request. Replacing it after capture leaves the captured counts and
  download unchanged. A metadata-only edit is observed without rebinding the
  selected content version.
- Two selected parents connected through an unselected attachment retain
  existing family grouping. That attachment has no member row or date/text
  evidence, and its own search hits never contribute.
- Keeping the selected request fixed while removing, adding, or swapping a
  packet member fails verification, even after recomputing digests and CSV.
  A packet vault-ID mismatch also fails.
- Date revisions keep original identities and expiry. History drafts keep
  identities but clear date choices. Caller edits to nested request data after
  capture cannot mutate the report.
- Metadata export/import preserves a receipt after its source has been
  deleted. The history remains readable; rerunning that selection conflicts.
  A restored live selection can run with available evidence. Old report handles
  remain unavailable.
- The actual web action submits displayed version/hash identities, keeps them
  fixed during refresh, and explains conflicts. The daemon-backed CLI accepts
  the same request and produces a packet that the offline verifier accepts.

Implementation review needs focused behavior checks through the store, report
engine/verifier, HTTP/CLI, and web draft flow, with synthetic documents. Both
SQLite modes, existing scope compatibility, frontend checks, and the strict
documentation build remain repository requirements. No new testing framework
or dependency is part of this design.
