# Qualify selected email families in reports and original exports

Status: implemented. CLI and MCP qualification covers all four scenarios below.
See the [execution evidence](2026-10-04-report-family-qualification-review.md#implementation-evidence)
for checks and platform boundaries.

Source baseline: `392dbe25905214bbd58d094134206654f730f739`, after PR #774.
Parent scope: [the local report/export workflow, #719](https://github.com/kenn-io/docbank/issues/719).

## Outcome and scope decision

Prove that a local operator gets the correct five report counts when selected
documents belong to email families. Selection must bound the counted documents
and separate exported originals, while retained relationships may connect those
documents through unselected children. Incomplete attachment inventories must
remain visible and must prevent a strict report.

This is one test-focused qualification PR, using synthetic retained evidence and
the real daemon through CLI and MCP. It adds no product feature. A reproduced
defect may justify a narrow correction to the existing contract; a change to
family semantics, selection semantics, or export policy requires a revised
design before implementation.

The maintainer chose this before additional CLI discovery conveniences from
closed #533. The CLI already exposes version identities and hashes, complete
rendition reads, and search coverage. Per-version processing coverage and short
text excerpts can be considered later; a new document listing is not required
by this scope. Broader redaction and discovery proposals remain deferred.

The [retained-PDF qualification](2026-10-02-report-export-qualification-design.md)
covered unrelated single-document families. Existing store and calculator tests
cover family behavior, including packet construction, but they do not qualify
the combined CLI/MCP family workflow described here. This work closes that
specific evidence gap, not all remaining acceptance work in #719.

The prerequisite #774 fixes are already on the source baseline. Its merged head
includes `79c0eaa3`: query fingerprints accept `sha256:<64 lowercase hex>`, and
export write descriptions retain recovery advice. The existing real-plan test
covers query, saved-query, and snapshot sources. No duplicate fix is proposed.

## Export decision: explicit ordinary documents

Accept explicit attachment selection for this qualification. CLI `export preview`
and MCP `preview_export` make originals-only plans. A published attachment is
an ordinary document with its own node, version, hash, and size. Include that
identity explicitly when a separate attachment file is wanted.

Do not add attachment expansion flags or use an HTTP-only attachment-role plan
to make a CLI/MCP test appear to cover it. Recursive attachment export remains
a separate product decision. The daemon already has richer attachment roles;
those roles are not the contract being qualified here.

An original email still contains all its original MIME parts. An attachment
omitted from the explicit selection must have no separate document row or file
in the export, but its bytes may remain embedded inside a selected parent EML.
This is original-byte export, not content removal or redaction.

## Meaning of the assertions

Use the existing [count definitions](../../usage/search-exports.md#read-the-counts-and-coverage).
All five values count documents. In particular, **Unique Families** counts
date-eligible selected documents in matching families without a competing row
hit; it does not count distinct connected components. **Incomplete families**
in coverage counts selected, date-eligible members with incomplete family
coverage, not distinct family groups.

An unselected child may appear as an endpoint in `families.jsonl`. It must not
gain a `members.jsonl` row, a `dates.jsonl` row, a text binding, a hit, or a
coverage contribution. Its own matches cannot compete with selected members'
matches. Conversely, omitting it from membership must not disconnect selected
parents joined by its retained relationships.

Use literal expected counts and exact expected identity sets. Do not call
`report.Calculate`, the family traversal, or the production CSV writer to
generate expected results. The ordinary offline verifier still runs, but its
agreement with the producer is not the independent count oracle.

## Synthetic fixture and ownership

Use small, literal MIME messages and plain-text attachment payloads. Filenames,
subjects, and other incidental metadata must not contain the query terms
`alpha` or `beta`. Give every attachment part an explicit `Content-Disposition`
filename: `chosen.txt`, `omitted.txt`, or `shared.txt` as appropriate. Do not
rely on publication-generated fallback names. Use only synthetic addresses
under `example.test`. Publication IDs must also avoid the query terms because
the publisher appends an operation/order suffix to attachment node names.

Prepare retained sources before daemon ownership, following the existing
`internal/processing/email_test.go` fixture:

1. Store the literal original bytes in a temporary vault's real blob store and
   create their ordinary file versions with the physical blob receipts.
2. Use `processing.EnsureEmailTarget` to decode and publish real MIME inventory
   and body evidence. Do not fabricate graph rows or write directly to SQLite.
3. Use `processing.PublishEmailDocuments` to publish attachment documents and
   occurrence relations. Refresh the destination directory revision for each
   publication. For a shared child, pass `EmailDocumentReuse` with its exact
   identity, current revision, and matching part path; equal bytes alone do not
   join two child documents into one family.
4. Publish deterministic retained report text for each fixture document under
   one portable profile, using the existing artifact publisher pattern in
   `cmd/docbank/report_pdf_fixture_test.go`. Parent text contains only its stated
   outer body, not its attachment payloads. Text remains independently available
   even for the deliberately incomplete inventory in scenario 3.

`EnsureEmailTarget` also publishes searchable email-body renditions under the
built-in email profile. Leave those builds intact. The reports in this fixture
use the configured `archive` profile for both term matching and text capture,
so the built-in email-body builds are not their text source.

For step 4, adapt the existing profile and rendition publication setup; do not
copy PDF-specific evidence into email fixtures. Use `mail`/message evidence for
email and `text`/section evidence for text attachments, with non-indexed
locators. Every publication needs a fresh build ID, attachment ID, and
`LexicalGenerationID`. Read back the resulting searchable text bindings for
the configured `archive` profile's fingerprint specifically, before starting
the workflow. Check the exact version, profile, build, and retained text, so a
built-in email rendition cannot hide a missing fixture publication. Do not use
`RecordExtraction` to bypass the retained rendition path.

Derive the configured processing profile once and use the same fingerprint for
all fixture publications and the daemon's `archive` profile. The portable
profile has no executable provider runtime. Preserve the existing PDF fixture's
policy settings when extracting shared test setup. Check the loaded daemon
configuration against the published fingerprint. Write configuration once per
new home rather than appending duplicate profile tables.

The fixture's retained text is deliberate test input. This does not qualify
automatic email/PDF extraction, profile selection by an operator, or the
faithfulness of a provider conversion. Real MIME decoding/publication is used
to obtain reachable attachment relationships, rather than a fake family graph.

Every retained text contains exactly one `Document dated 2024-05-06.` label.
Omit the email `Date` header from every fixture message. A usable sent date
would outrank the body label, and its timezone could change the selected UTC
day. Both report terms use
`simple` syntax, timezone `UTC`, and `2024-01-01` through `2024-12-31`.
All selected dates must resolve automatically to `2024-05-06`, with selection
reason `content`. Resolve each selected candidate ID in the packet's date
evidence and assert source class `content` and role `document_date`. Fallback
dates and unresolved review counts must be zero. This keeps date uncertainty
from masking a family-coverage refusal.

Close all setup store/blob handles before starting the daemon. Set
`DOCBANK_HOME` to the temporary vault, use `startServe` and `waitForDaemon`,
and join the daemon on cleanup. CLI operations use `runCLI`; neither client
opens the live vault directly. Run these process-environment tests serially.

## Scenario 1: one selected parent and one selected child

Create parent P with two published attachments C and U:

| Document | Retained report text, before the common date label | Selected? |
| --- | --- | --- |
| P, `parent.eml` | `alpha` | Yes |
| C, `chosen.txt` | `ordinary note` | Yes |
| U, `omitted.txt` | `beta` | No |

The publication inventory is complete. Request a strict report for P and C.
Both CLI and MCP must return a complete report with these values:

| Term | Hits | Hits Plus Family | Unique Hits | Unique Families | Unique Hits Plus Family |
| --- | ---: | ---: | ---: | ---: | ---: |
| alpha | 1 | 2 | 1 | 2 | 2 |
| beta | 0 | 0 | 0 | 0 | 0 |

Overall coverage and each row's coverage are: scoped 2, searchable 2, missing
text 0, incomplete families 0, fallback dates 0. The two selected members have
the same nonempty family ID. The relationship set is exactly P→C and P→U;
U remains a relationship endpoint only. Its `beta` text must neither create a
hit nor destroy alpha's family uniqueness. An omitted, available attachment
does not by itself make the family incomplete.

## Scenario 2: selected parents joined through an unselected child

Create distinct parents Q and R with outer bodies `alpha` and `beta`.
Each has an attachment occurrence with the same literal payload. Publish the
first child X, then explicitly reuse that exact child in the other publication.
Give X the retained text `alpha beta` plus the common date label. Select Q and
R only. Both inventories are complete.

| Term | Hits | Hits Plus Family | Unique Hits | Unique Families | Unique Hits Plus Family |
| --- | ---: | ---: | ---: | ---: | ---: |
| alpha | 1 | 2 | 1 | 0 | 2 |
| beta | 1 | 2 | 1 | 0 | 2 |

Both strict reports complete. Overall and per-row coverage are scoped 2,
searchable 2, missing text 0, incomplete families 0, fallback dates 0.
Packet membership is exactly Q and R, with one shared nonempty family ID.
The relation set is exactly Q→X and R→X. Preserve the warning identifying X
as a shared attachment with two parents; avoid asserting an entire prose
message when its identity and meaning suffice. X contributes no member, text,
date, or count. This case must fail if traversal is restricted to selected
endpoints. It does not independently qualify content-deduplication behavior.

After capturing and downloading the reports, trash only X through the daemon.
Q and R remain current and selected. The old summaries and re-downloaded ZIPs
must remain identical. A new `available_only` report must have no retained
current relation edges, incomplete-family coverage 2, and counts
`1, 1, 1, 1, 1` for each term. A new strict request must return
`incomplete_coverage`. This checks frozen evidence and propagation of an
unavailable child to both parents without adding a maintenance workflow.

## Scenario 3: incomplete inventory without missing report text

Use the truncated multipart shape already exercised by
`TestTermReportPartialFamilyWithoutPublishedAttachments`: one outer text body
without the closing MIME boundary. Set its body to `alpha` plus the common
date label. Assert that real publication returns inventory state `partial`
and no attachment relations. Retain the stated report text under the same
fixture profile and select only this parent I.

With `available_only`, both interfaces return a complete report. Alpha's counts
are `1, 1, 1, 1, 1`; beta's are all zero. Overall and per-row coverage are scoped
1, searchable 1, missing text 0, incomplete families 1, fallback dates 0.
The packet contains I, no relation edges, and I's family coverage is incomplete.
A complete report state does not erase the recorded coverage gap.

With `strict`, both interfaces return `incomplete_coverage`. There is no report
handle, no published destination, and no new history row. Compare bounded
history before and after the refusal. Verify complete searchable text and a
resolved date in the successful run first, so the strict failure is attributable
to family coverage rather than missing text or an unresolved date.

## Scenario 4: export exactly the explicitly selected originals

Using scenario 1's sources, exercise each client's own preview, start, status,
download, and release operations for two selections:

| Explicit selection | Document rows | Separate original files |
| --- | ---: | ---: |
| P, C | 2 | Parent EML and chosen attachment |
| P, C, U | 3 | Parent EML and both attachments |

For each run, verify the downloaded bundle against the admitted plan fingerprint.
Parse its document rows and compare their node/version/hash/size identities to
the literal selection. Each document has just its available `original` role;
no attachment-role rows are synthesized. Read every original ZIP entry and
compare its complete bytes and independently calculated SHA-256 to the fixture
source. The parent EML must stay byte-for-byte unchanged, including U's MIME
part in the first run.

Release each completed job after downloading it. Retry only `export_retained`
within a bounded deadline, as in the existing CLI qualification. CLI and MCP
share the master owner's two retained-job slots. Do not change admission limits
or turn an admission failure into an unbounded retry.

## Client execution and independent evidence

Keep the workflow tests in `cmd/docbank`, where the actual CLI and daemon
lifecycle helpers already live. Use separate temporary vaults for scenarios
1, 2, and 3; scenario 4 reuses scenario 1's fixture. Each client must create its
own report and export through its public operation, not inspect a report made
by the other client as a substitute for exercising admission.

For MCP, use `mcp.NewServerWithOptions` with `AllowReportWrites` and
`AllowExportWrites`, and `mcp.ServeStdio` over owned pipes. The server discovers
the same temporary daemon. Send ordinary protocol requests and check structured
results, crossing the schema and tool-handler boundary. Close pipes, cancel,
and join the server before stopping the daemon. This does not require a new
exported test hook or a second fixture framework. It does not claim MCP HTTP
listener/authentication coverage.

For every successful report:

- Compare the live summary to the literal count and coverage tables above.
- Download through the respective client. Independently parse `manifest.json`,
  `members.jsonl`, `families.jsonl`, `dates.jsonl`, and `hits.csv`; compare counts,
  identities, relationship endpoints, selected dates, and coverage to the same
  stated expectations. Treat generated family IDs as opaque; assert grouping,
  not a particular UUID's ordering.
- Run offline verification and the existing CLI CSV extraction on the saved
  packet. Compare parsed CSV values to the literal table, not calculator output.
  Internal consistency does not assert independent source authenticity.

The two clients' separate captures can have different observation times, IDs,
and ZIP bytes. Compare semantic results across clients. Byte equality applies
only to repeated downloads of the same frozen report in scenario 2. Verify
saved reports and native bundles again after the daemon stops.

The largest case admits four successful report handles across both clients,
below the existing eight-per-owner limit. Strict refusals create none. Use
bounded job polling and no sleeps to simulate the 30-minute report lifetime.
No report release operation or timeout change is part of this work.

## Validation boundary and completion evidence

Run the new family workflow tests in both SQLite modes with `-tags fts5`, along
with the existing report/counting, family-capture, MCP report/export, and PDF
qualification tests affected by any reused test setup. Run the repository's
required full Go checks in both SQLite modes, lint, hooks, and strict docs build
before handoff. Record actual native-platform results from the normal checks;
do not equate cross-compilation with Windows or macOS execution and do not poll
CI without a maintainer request.

The implementation handoff must identify the exact fixture, literal result
tables, client paths exercised, and any reproduced defect and correction.
Update this specification's status only when implemented. A passing test that
constructs the expected family graph or counts using the production calculator
does not qualify this workflow.

No storage or packet format change, dependency, endpoint, provider runtime,
product UI, recursive export feature, or compatibility adapter is planned.
Fresh-document preparation, browser family acceptance, attachment-role export,
and family recovery through backup/restore, pruning, or garbage collection are
outside this qualification. They remain separate evidence boundaries under
#719; this PR must not claim the entire umbrella complete.
