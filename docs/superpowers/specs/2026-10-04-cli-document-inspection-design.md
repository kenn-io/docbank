# Inspect processing coverage and retained text from the CLI

Status: proposed; not implemented. Ready for adversarial specification review.

Source baseline: `eba64ff4b039226b545cd25b9d722cc7bb87385c`, after #788.
Parent scope: [local document review and export, #719](https://github.com/kenn-io/docbank/issues/719).

## Purpose and scope

Before making a report, an operator needs to inspect the processing evidence
available for a selected version and read a short part of its retained text.
These reads must not start processing or silently select a newer version.

Add two CLI commands using existing daemon operations. Take only the coverage
and bounded-text outcomes from closed [#533](https://github.com/kenn-io/docbank/pull/533).
Existing `ls --json`, `stat --json`, and `versions list --json` already provide
document/version identities and source hashes. A new document catalog is not
part of this proposal.

The scope decision follows the merged retained-PDF qualification (#766),
retained-plan inspection (#774 and its correction), and selected email-family
qualification (#788). The status paragraph in #719 predates these results.
They establish useful report/export behavior but do not complete that umbrella:
combined browser acceptance and family recovery through maintenance remain
separate evidence gaps. Broader redaction, automatic extraction, and discovery
remain separate product decisions.

No HTTP endpoint, generated client, database layout, report or bundle format,
MCP tool, provider runtime, or web/TUI feature changes are proposed. Coverage
calculation and the text reader stay unchanged. No polling, automatic retries,
background processing, batch coverage output, or new retained artifact is added.

## Commands

```text
docbank processing coverage <version-id> --profile <name> [--json]
docbank rendition window <path-or-id> --version <version-id> --profile <name>
  [--attachment <attachment-id>] [--offset N] [--max-chars N] [--json]
```

`<path-or-id>` keeps the existing absolute virtual path or `id:<positive-id>`
syntax. Both version arguments are required canonical lowercase UUIDv4 values.
There is no implicit current-version default. Operators can obtain the version
from `stat --json`; a later replacement must not change which version is read.

Both commands require a profile name matching `[a-z][a-z0-9_-]*`, with 1–128
bytes. Window offsets default to 0 and accept 0–2,147,483,647. `--max-chars`
defaults to 8,000 and accepts 1–16,000; explicit zero is invalid. An explicitly
supplied attachment must be exactly 64 lowercase hexadecimal characters.
Reject an empty explicit attachment. An offset greater than zero requires
`--attachment`, so continuation cannot silently move to a replacement rendition.

Validate arguments and these flag bounds before `daemonconn.Ensure`. Reuse
`parseNodeSelector`, `daemonconn.IsCanonicalUUIDv4`, and `canonicalSHA256`.
The commands then use one daemon connection and close it on return. They never
open a vault directly. Do not rediscover or retry against another daemon after
an error. Ordinary `Ensure` startup behavior remains unchanged.

No success output is written until the complete response has been received and
the checks below pass. Output errors propagate. This is not a promise of atomic
stdout writes when an output pipe itself fails.

## Coverage for one exact version

Use `VaultInfo` on the connection, then the generated
`GetDocumentProcessingCoverage` operation. Pass the selected profile, that
vault UUID, and a one-element `ContentVersionID` query list. Do not resolve a
current version, run search, or use `ResolveDocumentSourceFence` as a preflight.

The existing coverage route aggregates a version set. Restricting the CLI to
one version makes every reported class refer to that document version, without
inventing a per-member response or making one request per member of a batch.
The HTTP/MCP multi-version contracts remain unchanged.

Before printing, require a non-nil response whose vault identity matches
`VaultInfo`. The daemon owns coverage policy and profile lookup; the CLI does
not recalculate states or require every embedding binding to be complete.

JSON is one object followed by a newline through `writeCLIJSON`. It has two
fields: `content_version_id`, the requested UUID string, and `coverage`, the
complete existing `api.CoverageReport` object. Preserve all fields, including
rendition and embedding classes; do not flatten or drop counters.
The version field binds the output to the CLI input;
the HTTP response itself does not echo the requested version list.

Human output names the version, vault, requested profile, returned profile
fingerprint, and aggregate state. It then shows the rendition row and each
embedding row with name, required, state, total, complete, unavailable, stale,
ineligible, rebuilding, and previous-generation-serving counts. Quote free-text
names. Show the returned aggregate state, not a locally inferred readiness flag.

These are processing counters, not report coverage: they do not inspect term
matches, dates, family completeness, or whether a strict report would succeed.
`previous_generation_serving` qualifies rebuilding work and is not an extra
mutually exclusive population to add to the total.

### Absence is useful output

Successful inspection exits 0 even for partial, unavailable, rebuilding, or
stale evidence. At this baseline, a syntactically valid unknown, superseded, or
trashed version is a successful coverage observation, not a stale-selection
error. A profile requiring a rendition reports one stale rendition for that ID.
Preserve the returned classes and do not substitute the current head. A
`not_required` class does not prove source availability. A missing retained
rendition on a live version is unavailable when the profile requires it.
An unconfigured profile remains the daemon's error.

## A bounded window from one retained rendition

The initial call resolves a rendition from existing metadata, so the operator
does not need an HTTP client to discover an attachment ID:

1. Resolve the path or stable node ID with `nodeSelector.resolve`. Reject a
   directory, and require the live node's current version to equal `--version`.
2. Read `ListDocumentProcessingProfiles` and select the requested name. Keep
   its fingerprint; do not call the processing-plan or build operation.
3. Call `Connection.ResolveDocumentSummaries` with one `api.DocumentIdentity`
   containing the resolved node ID, explicit version, and resolved path. This
   existing helper validates the returned identity. Do not enumerate a catalog
   page or match documents by a path prefix.
4. Select the active rendition whose profile fingerprint equals the profile
   from step 2. Require exactly one match. No match is unavailable; multiple
   matches are an invalid response. If `--attachment` was supplied, require it
   to equal that active attachment; never select a replacement instead.
5. Obtain `VaultInfo` on the same connection and call
   `Connection.RenditionTextWindow` with its vault ID, the node ID, explicit
   version, selected attachment, offset, and maximum character count.
6. The helper already bounds and validates the window response. Additionally
   require its build ID and profile fingerprint to equal the selected summary.
   Only then render the result.

Each read is current/live at the existing daemon catalog check. These sequential
operations do not freeze the document. A move between node resolution and
summary resolution may fail as stale; retrying the command is an explicit new
inspection. A replacement, trash operation, or rendition replacement before the
window's catalog check refuses the pinned tuple. A change after that check need
not retract an already admitted read. No lock or new transaction spans CLI output.

### Why use the existing rendition-window helper?

[#781](https://github.com/kenn-io/docbank/pull/781) adds `/api/v1/evidence/windows`
for callers already holding the complete source/build/rendition-hash reference.
Both window routes now use the same internal reader and current-rendition check.
The older `Connection.RenditionTextWindow` already supplies bounded decoding and
response-identity checks and is used by MCP. Reuse it for this command.

The CLI pins an immutable attachment and checks the resolved build and profile;
it does not expose a saved-citation import/export format. Requiring seven
evidence-reference fields would add a discovery step that current `stat` and
version output cannot supply. Adding another window adapter or manually parsing
a complete rendition to discover those fields is unnecessary for this scope.
The exact-evidence API remains available unchanged for its existing callers.

### Output and continuation

JSON is the complete existing `api.RenditionTextWindow`, written through
`writeCLIJSON`. It includes vault, node, version, attachment, build, profile,
checksum, media type, text, requested offset, actual start/end, next offset,
EOF, and response byte count. There is no new wrapper.

Human output identifies the version, attachment, build, profile, checksum, and
half-open character interval, followed by the returned text. Do not strip the
Markdown envelope, unescape punctuation, normalize whitespace, or render HTML.
Offsets count Unicode scalar values in the stored Markdown, including its
envelope; they are not byte offsets, grapheme positions, or PDF coordinates.

When EOF is false, print a continuation command using `id:<node-id>`, the same
version and profile, the returned attachment, `--offset <next_offset>`, and the
same maximum character count. No next-page command is printed at EOF. Scripts
carry the same identity fields and consume `next_offset` from JSON. An explicit
attachment is also accepted at offset zero to replay a pinned first window.

An empty rendition or an offset exactly at EOF succeeds with empty text and EOF
true. An offset beyond EOF is an error, not a clamped offset. There is no loop
that downloads all remaining windows automatically.

### Integrity and work limits

Reuse the existing 16,000-character and 1 MiB response-decoding bounds. A window
contains at most 64,000 UTF-8 text bytes. The existing helper also checks offset
arithmetic, UTF-8, identity fields, media type, next offset, and byte count.

The checksum identifies the retained full artifact. A partial window does not
hash or independently verify that whole artifact. Do not label an excerpt as
fully verified. `rendition get` remains the full-stream verification command.

The reader scans from the beginning to reach a Unicode offset and may read
ahead. Bounded output does not imply constant-time seeks or exactly that many
source bytes read. This proposal adds no text index or performance guarantee.
Neither command calls processing writes, OCR, embedding providers, or consent
operations. Independent daemon background work is outside these read operations.

## Failure contract

Keep existing CLI exit conventions. Add command-local context with wrapped
errors where useful; do not change the global daemon problem-code mappings.

| Condition | Result |
| --- | --- |
| Malformed version/selector/profile/attachment, missing required argument, invalid bounds, unpinned continuation | Usage error, exit 2, before connection setup |
| Coverage inspected successfully, including stale/unavailable classes | Exit 0, with the actual counters |
| Profile is not configured/executable | Descriptive failure, exit 1 |
| Window target is a directory | Usage error, exit 2; discovered after node resolution |
| Missing/trashed node, no matching retained rendition, supplied attachment is no longer active | Not found, exit 3; local absence wraps `store.ErrNotFound` |
| Window version differs from the live node, or summary resolution returns `stale_version` | Refresh-and-reselect advice, exit 1; no automatic substitution |
| Daemon rejects an offset beyond EOF | Existing `invalid_rendition_window`, exit 1 |
| Invalid UTF-8, mismatched response identity, transport or output failure | Existing error/context, exit 1 unless an existing typed error already selects another code |

Coverage and window operations intentionally handle unavailable versions
differently: coverage diagnoses an exact ID; a window cannot return unavailable
text. Domain failures print no successful JSON or text window.

## Acceptance examples

These examples define adapter behavior; the underlying engine is already tested.

1. A retained rendition under the selected profile gives one complete rendition
   in coverage. A second live version without a rendition gives one unavailable
   rendition. JSON preserves every class counter, including embedding rows.
   Inspecting the old ID after replacement or trash reports one stale member,
   while querying the replacement ID remains a separate operation.
2. The coverage request uses exactly the supplied version, named profile, and
   connected vault. Invalid inputs fail before connection setup. A mismatched
   vault response yields no success output.
3. Starting with only a CLI path/ID, version, and profile returns that profile's
   active retained text. With two profile renditions, the other text must not
   appear. Compare result identity with independently retained fixture receipts.
4. For stored text containing `aé界🙂z`, a window starting at that sequence's
   scalar offset plus one and length three returns exactly `é界🙂`. The next
   offset advances by three characters, not UTF-8 bytes. Follow the printed
   pinned continuation; EOF and beyond-EOF have the behavior specified above.
5. Replace a rendition between window calls. Continuation with the old attachment
   refuses instead of reading the new text. A new first-window call may select
   the new attachment. Replacing or trashing the source also refuses the old
   window selection. Existing frozen report packets remain outside this read.
6. Prove the adapter's selected-build/profile checks with a narrow daemon-client
   boundary fixture: a mismatch produces no success output. Keep the existing
   helper tests for wrong node/version/attachment and malformed windows; do not
   duplicate that rejection matrix in the CLI.
7. Exercise the new commands against a real temporary daemon with synthetic
   retained renditions. Use `reportRenditionFixture` publication where suitable;
   give each publication distinct build, attachment, and lexical-generation
   identities. Its current helper derives those IDs only from the version;
   it cannot be called unchanged for two profiles or replacements on that same
   version. Adapt the fixture's publication identity for those cases. Setup
   store handles close before daemon ownership. Observe no
   new processing jobs caused by inspection and verify the existing full
   `rendition get` behavior remains intact.

Run focused CLI, daemon-connection, processing-window, and coverage tests with
`-tags fts5` in both SQLite modes. Implementation also requires the repository's
normal Go/lint/docs checks. No new dependency or public test-only hook is needed.
Documentation belongs in `docs/usage/document-processing.md`, with an exact
selection example and the distinction between processing coverage, report
coverage, and full-artifact verification. Keep the proposed commands out of
current-behavior guides until their implementation lands.
