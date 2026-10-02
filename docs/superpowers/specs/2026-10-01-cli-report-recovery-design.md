# Inspect and recover search reports from the CLI

Status: reviewed design; not implemented. Approved for implementation planning.
The behavioral contract reviewed at `a3d4183a` is unchanged.

Source baseline: `b04894976863bfb92c9211216e50f2fad0aff3e6`, after PR #753.
Scope decision: [bounded report/export workflow, #719](https://github.com/kenn-io/docbank/issues/719).

## Purpose

An operator who already created a report should be able to inspect it and save
its evidence ZIP without capturing the vault again or spending another report
slot. This also provides a CLI destination for an MCP-created report whose
download exceeds the MCP HTTP time limit.

The existing CLI can create, revise, inspect dates, verify a saved ZIP, and
extract its CSV. It cannot show a live summary, list recorded runs, or download
by an existing ID. Create and revise also return download errors without the
new report ID, although they have already received it from the daemon.

This proposal takes only the inspection and download outcomes from closed PRs
[#456](https://github.com/kenn-io/docbank/pull/456) and
[#569](https://github.com/kenn-io/docbank/pull/569). Their older source-withdrawal,
chunked-artifact, and operation-registry designs do not apply. Frozen reports
keep the current semantics established by #726 and #753.

## Scope and alternatives

Add three commands under `docbank search-export` and retain the known report ID
in errors after a successful create or revise response. Reuse the existing
HTTP APIs, history records, packet verifier, and CLI file publication path.

Two alternatives were considered. Qualifying the entire PDF report/export
workflow through backup and restore would test a broader product boundary but
leave this concrete recovery gap. Starting native-PDF redaction would begin a
new subsystem before finishing the current operator workflow. The proposed CLI
slice is small enough for one implementation PR; broader workflow qualification
remains the next scope assessment under #719.

No new endpoint, report format, database migration, retained artifact service,
MCP tool, web UI, provider processing, or report release operation is included.
There is no automatic recreation from history, polling, partial-download
resume, or changed retention or capacity policy.

## Command contract

```text
docbank search-export show <report-id> [--json]
docbank search-export history [--offset N] [--limit N] [--json]
docbank search-export download <report-id> --output <path> [--overwrite]
```

These commands use `daemonconn.Ensure`; they never open a vault directly. They
do not create or revise reports. The two ID arguments must be exactly 48
lowercase hexadecimal characters. Reject malformed IDs, invalid page bounds,
missing output, and destination preflight errors before contacting the daemon.
These are CLI usage errors, with exit code 2.

Only `show` and `history` gain `--json`. JSON is one complete value followed by
a newline, written through the existing `writeCLIJSON`. Human output is for
operators; scripts should use JSON. Existing commands retain their flags and
success output.

### Show a live report

`show` calls `Connection.GetTermReport` and returns the validated live summary.
JSON is the existing `report.Summary` object, without a new wrapper or fields.
It includes the complete summary rather than the smaller MCP write receipt.

Human output includes the report ID, parent ID when present, state, observation
and expiry timestamps, and the ordered terms. For a complete report, display
all five count columns for each term, aggregate and per-term coverage, warnings,
and bundle size and SHA-256. Use `writeReportCoverage` for coverage disclosure;
it does not already render the count table. Quote term expressions in human
output so embedded newlines do not split a row.

For `needs_review`, show the unresolved count and explain that counts and
downloads are withheld. Point to the existing `dates` and `revise` commands.
Do not print a table of zero counts. Showing either live state succeeds.

There is no history fallback. A missing, expired, restarted, or differently
owned handle produces the daemon's existing `report_unavailable` error. An
expiry timestamp in a recorded summary is not evidence of live availability.

### List recorded runs

`history` calls the generated `ListTermReportHistory` operation once. Offset
defaults to 0 and accepts 0–100; limit defaults to 20 and accepts 1–50. Explicit
`--limit 0` is a usage error even though HTTP treats zero as its default.

JSON is the existing history page, `{ "items": [...], "total": N }`, including
each stored request and summary. Do not replace requests with counts, omit
selection arrays, apply the stricter live-summary validator to historical
receipts, or add an availability field.

Human output shows each ID, parent ID when present, recorded state, observation
and expiry timestamps, and number of terms. It states that history records past
runs and does not retain their downloadable artifacts. The footer shows the
number returned and total; when more rows remain and this page is nonempty,
it gives the next offset as `offset + len(items)`. An empty page is successful.

Preserve the daemon's order: observation descending, then ID descending. It
retains at most 100 records and may shorten a page at its 16 MiB stored-JSON
threshold. That threshold measures stored requests and summaries, not the
complete HTTP encoding. Do not assume a page contains the requested limit,
fetch subsequent pages automatically, or make one live lookup per row. Offset
pages are not a stable snapshot when other clients add runs concurrently.

History is vault-wide; live handles are owner-bound. API-key CLI and MCP calls
share an owner, while browser sessions have separate owners. Listing a browser
report does not grant the CLI access to its live artifact. History and backup
preserve requests and receipts, not packets, date pages, or reviewed choices.

### Download an existing report

`download` saves the evidence ZIP only. Reuse `downloadReportPacket`, which
stages a file privately, calls `Connection.DownloadTermReportTo`, syncs and
closes it, then publishes through the existing CLI destination rules.

The shared helper re-reads the live summary, refuses `needs_review`, binds the
stream size and digest to that summary, checks the transferred bytes, and
verifies the packet with `report.VerifyBundle` before publication. Preserve
its 512 MiB artifact limit and verification budget. Do not duplicate or weaken
this verification in the command.

Refuse an existing destination without `--overwrite`; keep the existing
no-replace publication behavior if a destination appears after preflight.
Relative output paths remain accepted. On success print the destination and
report ID, with the existing distinction: internally verified, source evidence
not checked offline. CSV remains an offline operation on that verified ZIP:

```text
docbank search-export download <report-id> --output report.zip
docbank search-export csv report.zip --output counts.csv
```

A failed transfer or verification must not publish partial bytes or replace an
existing file. A later directory-sync or staging-cleanup error can occur after
publication, so do not promise that every error leaves the destination absent.
Preserve the publication helper's message when it says the file was published.
An operator should inspect the destination before choosing a new path or
explicit overwrite. This slice does not replace the shared publication layer.

## Recover a known create or revise result

After `CreateTermReport` or `ReviseTermReport` returns a validated summary,
every subsequent error includes that summary's ID while wrapping its cause
with `%w` or an equivalent unwrapping mechanism. This covers coverage output,
review instructions, download/publication, and final status output. A revision
error names the new child, not its parent.

For a failed complete-report download, the error points to
`docbank search-export download <report-id> --output <path>` and advises checking
the destination first. For a report still needing review, point to `show` or
`dates`. If saving succeeded but writing the final status failed, say the
verified file was saved. Do not label an unconfirmed publication as successful.

Keep required `--output` and current successful create/revise behavior. Do not
replay either write automatically. If the write itself returns an error before
a validated summary arrives, this feature has no confirmed ID to report.
History can help an operator investigate; it cannot prove which record belongs
to a lost response. Do not guess an ID or promise idempotent recreation.

## Errors and unchanged limits

| Condition | Behavior |
| --- | --- |
| Invalid new-command ID, page flags, or destination preflight | Usage error; exit 2, before daemon contact. |
| Live handle unavailable | Preserve `report_unavailable`; exit 1, no history fallback or recreation. |
| Download needs date review | Explain `dates`/`revise`; exit 1, no published file. |
| Interrupted stream or local I/O failure | Preserve the cause and existing exit classification; ordinary failures exit 1. |
| Summary/stream mismatch, verified size/hash mismatch, or invalid packet | Preserve `daemonconn.ErrIntegrity`; exit 6. |
| Successful human or JSON inspection, including empty history | Exit 0. |

Keep other existing typed errors and exit mappings; do not convert every error
into a usage or integrity error. In particular, the merged `io.ErrUnexpectedEOF`
handling remains a transport failure, not an integrity failure.

Reports still share eight API-key-owner handles, with two builds and 64 handles
or pending builds globally. Each revision consumes another handle. All
descendants expire 30 minutes after the original observation, and daemon restart
loses live handles. These reads and downloads consume no new report handle and
do not extend expiry. A download retry fetches the entire ZIP again while the
handle remains available; it cannot recover a handle lost to restart or expiry.

## Reuse and implementation boundary

Keep the command additions in the existing report command area, split into a
focused file if needed for repository size limits. The generated history client
already returns `store.TermReportHistoryPage`; no hand-written HTTP layer or
response type is needed. `GetTermReport`, `downloadReportPacket`,
`writeReportCoverage`, and `writeCLIJSON` exist at the baseline.

For preflight ID checks, rename the existing private `validTermReportID` to
exported `daemonconn.IsTermReportID` and update its callers. That exported name
is a proposed addition, not an existing API. Do not add a second parser or keep
an alias. New formatting should be specific to these commands; this slice does
not require a generic report renderer or a cross-export publication framework.

Update the CLI section of `docs/usage/search-exports.md` with inspection,
pagination, and retry examples. Update `docs/usage/mcp.md` to point operators to
these commands for large summaries, known-ID downloads, and history inspection.
Retain its warning that an unknown write outcome is not safe to replay blindly.

## Acceptance evidence

Use synthetic documents and the existing real-daemon CLI test setup. Cover the
new command boundary; reuse existing engine and verifier tests for their
unchanged contracts.

1. Create a report, show both human and JSON summaries, list its recorded
   request and summary, and download its ZIP by ID. Verify the ZIP offline and
   extract matching counts. Showing and downloading add no history records.
2. Show a `needs_review` report without zero-count claims; its download fails
   without publication. A reviewed child is addressed by its own ID.
3. Exercise history limits and empty pages, plus next-offset calculation from
   an actually shortened page. Keep recorded state distinct from live state:
   a historical or differently owned run may be listed while show/download
   returns unavailable. Reuse existing cache tests for expiry and ownership.
4. Make create and revise fail after a validated response at the local delivery
   boundary. Their errors retain the correct ID and original cause; a later
   download of that ID succeeds while live, without another creation/revision.
   A final output failure after publication reports the saved file accurately.
5. Reject malformed new-command inputs before daemon acquisition. Check JSON
   output as decoded public values, not internal formatting or source text.
6. Exercise overwrite refusal and successful explicit overwrite for download.
   Keep existing shared-helper tests for stream interruption, digest mismatch,
   packet validation, and publication races; do not build another download
   fault-injection framework in the CLI.

This is a CLI recovery slice, not proof of the complete PDF processing and
backup/restore workflow. That broader acceptance boundary remains explicit in
#719 before choosing the next product area.
