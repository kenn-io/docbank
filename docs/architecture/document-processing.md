---
last_edited: 2026-10-12
title: Document processing
description: How Docbank derives readable text and search data while keeping original versions and requiring consent for processing.
---

# Document processing

Docbank keeps original document versions and the results derived from them
separately. A new extractor or embedding model can produce a new result without
rewriting the original. Search uses only results that the vault has published
for the relevant document version.

This page describes the implemented Go contracts and internal processing
architecture. For an application choosing extractors or embedding providers,
start with [Document understanding in Go](../document-understanding.md).

## Which interfaces expose this work?

| Interface | Current scope |
|-----------|---------------|
| Public `document/*` Go packages | Evidence, rendition, OCR, upload, embedding, and provider contracts; the importing application owns orchestration and persistence |
| Public embedded vault API | Plan and run processing, read retained renditions and coverage, and search exact source versions in lexical, semantic, or hybrid mode. Also process local metadata and read visual previews. See [Embed in Go](../embedding.md) |
| Daemon background jobs | Local metadata, checksum, and visual-preview work; configured rendition and embedding jobs; vector-index rebuilding |
| Internal processing and storage packages | Profiles, consent, publication, retention, and derivative purge |
| Internal retrieval packages | Lexical, semantic, and hybrid retrieval; optional expansion and reranking; QMD export and query adapters |
| CLI and HTTP | Preview and run profiles, inspect jobs, read retained renditions, and search an explicit source-version fence |
| Web app and TUI | Preview processing, record consent, inspect jobs, and read retained sanitized renditions |

The default configuration has no processing profiles. The daemon can execute
its built-in plaintext and EPUB rendition adapters and the OpenAI-compatible
and Voyage embedding runtimes listed in [Configuration][embedding-runtime].
Other public provider packages are available to embedded Go callers. Adding a
package does not register a daemon runtime. Configuration alone does not grant
consent or enqueue work.

[embedding-runtime]: ../configuration.md#embedding-workers-and-credentials

`configureRenditionProviders` selects `document/epub` for `docbank-epub-rendition/v1`
and passes the existing byte and unit limits to its constructor. The provider
owns the `epub.in-process-v1` descriptor and virtual-unit policy. Startup
rejects descriptor or trust-boundary drift. Unknown adapters remain
unavailable.

[Document processing](../usage/document-processing.md) follows the operator
workflow from profile selection through preview, consent, execution, and reads.
[Processing search](../usage/search.md) explains lexical, semantic, hybrid, and
auto modes with an explicit profile and authorized source-version fence.
Ordinary name and text search remains separate.

## From original bytes to searchable evidence

```mermaid
flowchart TD
    Original[Exact original version] --> Local[Local source metadata and visual preview]
    Original --> Upload[Inspect and authorize exact bytes]
    Upload --> Provider[Rendition provider]
    Provider --> Evidence[Canonical evidence and source locations]
    Evidence --> Rendition[Sanitized Markdown and lexical segments]
    Rendition --> Publish[Publish for an exact document version]
    Evidence --> Inputs[Tokenizer-specific embedding inputs]
    Original --> Direct[Authorize original-file embedding]
    Inputs --> Vectors[Retained vector sets]
    Direct --> Vectors
    Vectors --> Index[Rebuildable vector index]
```

Canonical evidence records what the extractor found and where it came from.
It includes ordered units, source locators, regions, headings, artifacts, and
explicit omissions. A partial or degraded result retains that status instead
of claiming complete document coverage. Timed transcript units retain optional
speaker identity and half-open millisecond spans. Untimed supplied transcripts
follow the same contract without invented timestamps or speaker identities.

A rendition contains sanitized Markdown and model-independent text segments
for keyword search. Embedding input generation reads canonical evidence, not
the retained Markdown wrapper. It applies a named tokenizer, chunk policy,
and document-role formatter. This keeps navigation metadata and frontmatter
out of embedding text. Direct-file embeddings use separately authorized
original bytes instead of a rendition.

Local [source metadata](source-metadata.md) and
[visual previews](visual-previews.md) have their own processors and version
identities. They do not require a hosted rendition provider.

## Keep policy and result identities separate

A processing profile is an immutable description of extraction, text handling,
embedding inputs, retention, disclosure, and retrieval limits. Its fingerprint
is a checksum of that declared policy. Credentials are named bindings. The
profile does not retain their secret values.

Docbank also computes separate fingerprints for the rendition request,
evidence and lexical policy, each embedding-input policy, each vector space,
and retention and disclosure. This permits reuse at the layer whose inputs
still match. Changing embedding chunking does not redefine the original or
make a different extractor result equivalent.

The built-in plain-text policy has no vault-specific inputs. Docbank computes
its canonical JSON and fingerprints once per process so text imports and
restore queue rebuilds can reuse them. Each caller receives its own JSON bytes.
Extraction eligibility and purge suppressions still read the vault's current
transaction state.

A vector space defines which vectors can be compared: provider descriptor,
model, dimensions, metric, normalization, scalar encoding, and input
formatters all matter. Destination-sensitive disclosure identities are checked
separately. Matching dimensions alone do not establish compatibility.

The [profile contract](https://github.com/kenn-io/docbank/blob/main/document/profile.go)
owns these identities. The [embedding contract](https://github.com/kenn-io/docbank/blob/main/document/embedding.go)
owns provider requests and compatibility checks.

## Authorize disclosure before provider work

Inspection binds the operation to the exact source hash, byte count, detected
format, and locally measured limits. The upload layer stages those bytes and
issues a one-shot authorized reader. Provider adapters receive that reader,
not arbitrary access to the vault. The outbound transport checks the declared
origin and allowed network ranges when establishing the actual connection.

PDF format detection requires a versioned `%PDF-` header at byte zero and a
`startxref` within the final 64 KiB. Its offset must point backward to a
recognized cross-reference table or stream. Each cross-reference read is capped
at 4 KiB. Linearized (Fast Web View) streams may need a second read to validate
the forward link to the final cross-reference section. A final `%%EOF` marker
may be followed by whitespace, NULs, or short text or binary residue within
the tail window. Appended ZIP signatures, `<` or `>` bytes, repeated end
markers, and PDF revision syntax are rejected. The last `startxref` remains
authoritative, so an incomplete appended revision cannot fall back to an older
valid revision. Missing-final-marker recovery still requires only PDF
whitespace after the offset. These checks recognize the format; subsequent
inspection validates the page tree, encryption, external references, and
policy limits before authorizing provider work.

The final cross-reference stream must close with `endstream` and `endobj`
before the last `startxref`. Complete objects without streams, such as an
indirect `/Length`, may sit between them. When the stream starts inside the
tail window, detection finds `endstream` by keyword. A stream that starts
earlier needs a direct `/Length` that ends inside the window. Detection
rejects an earlier-starting stream with an indirect `/Length` because it
cannot find that stream's end within its bounded reads.

Before granting consent, the caller reviews each flow's runtime disclosure:
immediate and ultimate processor, endpoint, deployment, model and revision,
vector space, provider-visible metadata, and retained artifact roles. The plan
fingerprint seals those values alongside the exact source and portable profile.
Embedded callers supply endpoint disclosures for network providers when opening
the vault. [Embed in Go](../embedding.md#configure-document-processing) describes
the configuration contract.

A durable consent grant records the principal, scope, processing-profile and
disclosure fingerprints, permitted input classes, retained artifact classes,
and optional expiry. A worker checks the grant before provider egress and
holds a fence against revocation while the call runs. Publication checks its
authority again. A replacement grant does not revive work whose original
grant was revoked.

Restoring a vault starts a new processing incarnation: a new local identity
for provider authority. The restored history still explains previous consent,
but those old grants do not authorize new outbound requests.

These rules are implemented in the
[consent catalog](https://github.com/kenn-io/docbank/blob/main/internal/store/consent.go),
[upload package](https://github.com/kenn-io/docbank/tree/main/document/upload),
and [provider transport](https://github.com/kenn-io/docbank/tree/main/document/providerhttp).
Provider capability manifests and a vault's human-consent records serve
different purposes. Neither substitutes for the other.

## Publish complete results for exact versions

The rendition catalog separates an immutable build from the attachment that
allows one content version to use it. Several attachments can reuse a build
inside the vault. An active head selects the attachment visible for one
content-version and processing-profile pair.

Workers retain attempts, leases, and provider-operation state separately from
completed builds. A resumable provider can continue polling a recorded
operation without reopening source bytes. If a submission outcome is ambiguous,
the worker resumes a known operation or requires operator action instead of
automatically uploading the source again. A late or stale worker cannot
publish over newer authority. Rendition attachment and lexical serving
generation changes publish together.

Embedding inputs, vector sets, and version-specific embedding sets are also
immutable records. Each embedding binding has its own active head, so an
optional failed binding does not replace another binding's successful result.
Vector values live in validated blob artifacts. SQLite records their identity
and membership. The vector-index worker builds disposable generations from
those retained sets and publishes only against the source membership it read.
Reader leases keep a selected generation available during a query.

Coverage reads source visibility, serving heads, and replacement jobs in one
catalog snapshot. A replacement counts as `rebuilding`. The separate
`previous_generation_serving` count records whether a complete prior result
remains available. Those documents are not also counted as `complete`. Embedding
coverage uses the same current attachment and evidence checks as search. A
missing required result keeps aggregate coverage `partial` even if other work
is rebuilding. See the [HTTP coverage contract](http-api.md#coverage-and-source-fenced-search).

Within that snapshot, rendition coverage fully validates each distinct build
and attachment-profile pair once. Documents sharing a build retain their own
visibility and rebuilding counts without reloading the same extracted text.
Validation results are discarded when the request ends; later reads evaluate
the current catalog again.

The owning implementations are the
[rendition worker](https://github.com/kenn-io/docbank/blob/main/internal/processing/rendition_worker.go),
[rendition catalog](https://github.com/kenn-io/docbank/blob/main/internal/store/processing_catalog.go),
[embedding catalog](https://github.com/kenn-io/docbank/blob/main/internal/store/embedding_catalog.go),
and [vector-index catalog](https://github.com/kenn-io/docbank/blob/main/internal/store/vector_index.go).

## Explain retrieval without granting authority to a provider

The internal searcher treats omitted and `auto` modes as lexical. Explicit
semantic and hybrid modes require compatible active vector authority and a
query encoder. The source-version fence selects eligible vectors from current,
live attachments before scoring. Missing vector-row authority stops the search.
Results retain source references, lexical and semantic rank contributions,
coverage, truncation, and degradation information. Candidates are checked
against current vault authority before they are passed to optional reranking.

For a timed rendition segment, lexical evidence resolves the exact lexical
segment to its build unit. Rendition-chunk semantic evidence resolves the exact
input generation and source unit from retained canonical artifacts. Both paths
return the same optional `time_span` shape and the exact build identity.
Direct-file embeddings and untimed evidence omit `time_span`. Zero is a valid
segment start and remains present in JSON. Hybrid results retain each lane's
separate evidence reference instead of combining disjoint spans. The daemon HTTP
API preserves these spans for CLI and web clients.

The processing service caches verified timing mappings for up to 100,000 inputs.
A cold lookup still reads the retained generation and evidence artifacts. If an
artifact cannot be read or verified, search omits the affected semantic hit and
marks the report `truncated`. Other documents remain available.

Query expansion produces bounded alternate queries. Reranking reorders an
already authorized candidate list from bounded excerpts and evidence
references. Each stage requires its own authorization, deadline, and explicit
choice to fail the query or return degraded results. Its receipt records the
outcome and counts without retaining query or excerpt text.

QMD is an operator-hosted document-search service. Docbank supplies an export
and query adapter for it alongside the internal reranking adapters below.

| Internal adapter | Purpose |
|------------------|---------|
| [`internal/retrieval/cohere`](https://github.com/kenn-io/docbank/tree/main/internal/retrieval/cohere) | Hosted `rerank-v4.0-pro` or `rerank-v4.0-fast` |
| [`internal/retrieval/zeroentropy`](https://github.com/kenn-io/docbank/tree/main/internal/retrieval/zeroentropy) | Hosted `zerank-2` reranking |
| [`internal/qmdexport`](https://github.com/kenn-io/docbank/tree/main/internal/qmdexport) | Export active sanitized Markdown as disposable QMD collections |
| [`internal/retrieval/qmdbridge`](https://github.com/kenn-io/docbank/tree/main/internal/retrieval/qmdbridge) | Query an operator-hosted QMD endpoint and resolve results through Docbank authority |

A QMD export contains verified Markdown and a manifest mapping QMD URIs back
to exact Docbank versions and rendition artifacts. Publication selects a whole
immutable collection generation. The query adapter holds consent through the
request, checks the selected generation, and resolves returned candidates
against the current vault and requested scope. QMD's index does not become the
source of document ownership or current-version truth.

Similar-document retrieval scores every stored source row against the fenced
candidate rows. Cosine and dot product keep the greatest score. L2 keeps the
smallest distance and returns its negative. The store preserves every document
membership when several documents share a vector-set row. It deduplicates only
the scoring inputs, then expands matches before excluding the source node and
grouping by content hash. For example, two eligible copies of a result appear
as one row with `duplicate_count: 1`.

Current/live eligibility, binding authority, and vector-index leases apply to
both source and candidates. The final transaction rechecks source identity and
the source manifest. Missing source coverage returns an unavailable report.
It never invokes an encoder or provider. Ordinary processing search keeps its
existing document results. See the [HTTP contract](http-api.md#similar-documents).

The [searcher](https://github.com/kenn-io/docbank/blob/main/internal/retrieval/search.go)
and [optional provider stages](https://github.com/kenn-io/docbank/blob/main/internal/retrieval/providers.go)
own the detailed retrieval contract. These packages are internal integration
primitives, not public Go imports or CLI recipes.

## Reopen a saved quotation

A retained text citation binds a vault, file node, immutable source version and
hash, published rendition attachment and build, Markdown hash, and Unicode
range. The [HTTP operation](http-api.md#retained-text-citations) and default
[MCP tool](../usage/mcp.md#saved-text-citations) reopen that exact text. A profile
need not remain configured or executable. The read never calls a provider or
selects newer evidence.

The store checks the complete reference in one metadata snapshot. The file must
be live and untrashed, and still own the historical version. The attachment must
remain published and unsuppressed, with a matching build, profile, source hash,
and sanitized-Markdown artifact. A retained build without that attachment is
insufficient. Purge suppression denies the read even when another dependency
keeps the attachment or bytes alive.

Replacing source content or the active rendition does not invalidate retained
attachments. Ordinary derivative GC keeps their builds. Trash makes the citation
unavailable until restore; pruning its version or purging its rendition can
remove it permanently. A backup restore preserves the authority present in that
snapshot. Saving a citation adds no retention root, job, lease beyond the read,
or promise that its evidence will remain available.

The service holds `CaptureContext` from metadata lookup through full blob
verification and close. The database transaction ends before blob I/O. This
shared capture gate excludes physical maintenance, including purge, while
allowing backup's short metadata freeze. Logical mutations may finish after the
snapshot: an already admitted read can finish, while later reads see the changed
authority. Embedded vaults keep their existing serialized gate behavior.

Each resolution streams the complete rendition, up to 64 MiB, and retains only
the requested quote. It verifies the full digest, size, and UTF-8 stream before
returning text. A short quote still costs a full read. Cancellation and I/O
causes remain distinct from corrupt evidence. This verification concerns the
stored sanitized Markdown; it does not prove provider correctness or quote
verbatim bytes from the original PDF, email, or Office file.

## Read the section around a quotation

The [outline and section reads](http-api.md#retained-text-sections) use the same
retained citation authority. They accept historical attachments without a
configured processing profile. A saved reference is an anchor, not a retention
lease. Each page checks authority again; a successful first page does not promise
that a later page will remain available.

Navigation reads are stateless. A service admits two at a time, shared between
outline and section requests. Waiting is cancelable and consumes no report,
export or processing-job slot. Within `CaptureContext`, the service resolves
metadata, copies the entire verified artifact and closes the stream. It releases
capture protection before parsing. Purge and repack can then proceed while the
request works from its owned bytes. Logical trash after admission does not
invalidate that read; later requests see the changed authority.

A navigation request accepts at most 4 MiB of Markdown, 65,536 LF characters and
4,096 document-root headings. Goldmark's block parsers and paragraph transformers,
including GFM tables, derive headings from stored Markdown. Inline parsing is
omitted: section titles use their source lines, and parsing emphasis, links and
code spans can take quadratic time without contributing section information.
The text is not rendered or normalized again. A synthetic root covers all
text; each heading includes its subsections. Section bounds count Unicode code
points and preserve source whitespace, escapes and line endings. Labels are
bounded navigation metadata, not quotations. The [HTTP reference](http-api.md#retained-text-sections)
owns boundary, label and pagination rules.

The admission slot covers parsing and response construction as well as I/O.
Cancellation is checked around parsing and during scans, but Goldmark's
synchronous parser cannot be interrupted mid-call. These input and concurrency
caps limit input size and concurrent reads, not a fixed heap or CPU budget.
Returned strings do not retain the whole source buffer or syntax tree. Nothing
is cached or stored after the operation.

Every page rereads and reparses the whole artifact, even a final empty page.
Choose the largest useful page size. Existing short citations still stream
artifacts up to 64 MiB, independently of navigation slots. `rendition get` can
retrieve an attachment still active for its version; it cannot retrieve a
superseded attachment. Navigation does not add historical bulk download.

## Retain, restore, or remove derived results

A processing profile declares whether to retain provider Markdown, sanitized
Markdown, and typed artifacts. Retained evidence, renditions, embedding-input
generations, and vector sets are cataloged against verified blobs. Original
content versions remain separate authority.

Backups include the retained derivative catalog and the blobs it authorizes.
Restore validates them and rebuilds lexical and vector indexes from retained
local data. It does not rerun extraction or call embedding providers. See
[Backup architecture](backup.md) for the snapshot and restore boundary.

The internal derivative-purge operation removes selected serving attachments
and collects complete builds or generations only when no remaining root needs
them. Shared attachments, active workers and readers, and backup pins can
retain a result. A durable suppression prevents background discovery from
silently recreating a purged result. Rebuilding requires an explicit
authorization that supersedes that suppression.

Derivative purge preserves original versions and does not erase copies in
existing backup repositories. Removing a source version first revokes its
derived serving authority, then leaves shared-result collection to the same
retention rules. The [garbage collector](https://github.com/kenn-io/docbank/blob/main/internal/store/gc.go)
and [purge suppressions](https://github.com/kenn-io/docbank/blob/main/internal/store/derivative_suppression.go)
own these rules.

The [Document derivatives](document-derivatives.md) reference describes the
sanitized Markdown envelope, body-relative navigation, and consumer source
identity contract.

## How are large audio sources bounded?

`document/media.MaxMediaSourceBytes` is the default and maximum streaming
media source ceiling: 4 GiB, represented as `int64`. The daemon's
`[media].max_source_bytes` and embedded `ProcessingOptions.MediaMaxBytes` may
lower it. HTTP envelopes add only 128 KiB for multipart metadata and framing.
Source staging, aggregate staging reservations, publication and rendition
inspection use the same configured ceiling. Sequential rendition inspection
and sealed upload preparation use verified blob streams, including terminal
verification and a catalog size check, instead of materializing a seekable copy.
Audio admission and worker runtime preparation intersect it with the selected
profile's source limit. The built-in supplied-transcript and supplied-captions
profiles allow 4 GiB original sources;
their 16 MiB transcript, caption and response limits remain separate.

Audio retains its 24-hour duration and codec qualification bounds.
WAV and MP3 inspection hash every byte while retaining parser headers and a
fixed copy buffer. WAV chunk skipping uses `int64` counts, including on 32-bit
systems. Source lengths never select a sample allocation size. Document
inspectors retain their existing 1 GiB ceiling because they materialize the
source. Visual detection and remote MP4 originals remain bounded at 20 MiB.
M4A identities (`.m4a`, `audio/mp4`, `audio/x-m4a`) select the audio ceiling;
accepting them still requires M4A format support. Its movie metadata bound
remains independent of source length. Hosted-provider limits
continue to apply before uploads to those providers.

Staging reserves the declared length before creating its private file. The
reservation is an aggregate service quota, not a claim about filesystem free
space. Disk or filesystem quota exhaustion during creation, writing or syncing
returns `ErrMediaStagingUnavailable`; HTTP exposes this as retryable 503
`media_staging_unavailable`. Failed staging closes and removes its file and
releases the reservation. Operators can free space and retry the same operation.
HTTP processing plans for cataloged audio sources above 1 GiB and within the
configured ceiling lift the server's 60-second handler deadline after bounded
JSON decoding. They clear the remaining socket read deadline and preserve the
client's cancellation and its cause, original deadline and authentication context.
Browser-session revocation still cancels the inspection, and cancellation hooks
detach when the plan finishes. An expired handler cannot lift its deadline.
Smaller audio plans, documents, video, over-ceiling sources and request-body
limits keep their existing deadline behavior.

A source may need both its stage and durable loose copy on disk during
publication. Provider execution may also hold a sealed spool copy.
