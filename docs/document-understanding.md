---
last_edited: 2026-10-10
title: Document understanding in Go
description: Choose Go packages for document extraction, canonical evidence, renditions, and embeddings without opening a Docbank vault.
---

# Document understanding in Go

Use Docbank's Go packages to prepare documents for text search, OCR, or
embeddings in your own application without starting a daemon or opening a
vault. OCR extracts text from document images, and embeddings represent text or
media as numeric vectors for similarity search.

Start with the provider-neutral contracts in `document`, then choose a provider
after you decide what bytes your application may send and what results it may
retain. A rendition is a derived, readable representation of a document, and
canonical evidence is the validated text and source locations behind it.

| Your application needs to… | Package under `go.kenn.io/docbank/` |
|----------------------------|-------------------------------------|
| Validate evidence, build renditions, and prepare embedding inputs | `document` |
| Build or validate a canonical timed transcript artifact | `document/mediatranscript` |
| Identify provider destinations and define retrieval policy | `document/embedding` |
| Compare retrieval approaches | `document/embedding/eval` |
| Use a provider-neutral OCR interface or local GLM-OCR | `document/ocr`, `document/glmocr` |
| Detect media formats and enforce input limits | `document/media` |
| Stage exact bytes for an authorized provider upload | `document/upload` |
| Bind outbound connections to a declared destination | `document/providerhttp` |
| Convert CSV locally for PDF OCR | `document/csvpdf` |
| Convert office files locally to a bounded PDF | `document/renderpdf` |
| Inspect exact physical page frames and render verified PNGs locally | `document/pagerender` |

These packages provide reusable Go contracts. For vault-owned publication,
consent, backup, and search, see
[Document processing](architecture/document-processing.md). For the daemon's
executable embedding adapters, see the
[daemon configuration](configuration.md#embedding-workers-and-credentials)
reference.

## Choose a rendition provider

Each adapter implements an extraction contract with its own limits, and the
provider descriptor and profile define the supported formats and limits rather
than a filename extension. In the table below, "Operator-hosted" means you run
the service and declare its destination, while "Hosted" means the adapter calls
an external provider. Page geometry and images stay separate from text
renditions; for the optional pinned local page runtime, see
[Verified page images](architecture/page-images.md).

| Provider package | Where it runs | Contract and scope |
|------------------|---------------|--------------------|
| `document/pdftext` | In process | PDF text-layer pages and exact `no_text_layer` omissions |
| `document/epub` | In process | EPUB XHTML spine text and virtual usage |
| [`document/plaintext`](https://github.com/kenn-io/docbank/tree/main/document/plaintext) | In process | UTF-8 text, including declared source, structured-text, CSV, and mail formats; one generic unit with degraded provenance |
| [`document/suppliedtranscript`](https://github.com/kenn-io/docbank/tree/main/document/suppliedtranscript) | In process | Caller-held transcript text for bounded WAV, MP3, and M4A, plus supplied SubRip captions for WAV, MP3, M4A, and MP4 originals; captions produce timed segment units with supplied provenance |
| [`document/pymupdf`](https://github.com/kenn-io/docbank/tree/main/document/pymupdf) | Local process | PDF text through a pinned, digest-verified executable |
| [`document/trafilatura`](https://github.com/kenn-io/docbank/tree/main/document/trafilatura) | Local process | Supplied HTML through a pinned isolated runner; the native runner requires Linux namespace and Landlock support |
| [`document/docling`](https://github.com/kenn-io/docbank/tree/main/document/docling) | Operator-hosted | Uploaded files through Docling Serve; structured output and Markdown |
| [`document/marker`](https://github.com/kenn-io/docbank/tree/main/document/marker) | Operator-hosted | PDF, common images, DOCX, XLSX, PPTX, EPUB, and HTML through the fixed Marker contract |
| [`document/unstructured`](https://github.com/kenn-io/docbank/tree/main/document/unstructured) | Operator-hosted | Pinned broad-format compatibility profile for the standard rendition bridge |
| [`document/tika`](https://github.com/kenn-io/docbank/tree/main/document/tika) | Operator-hosted | Pinned Apache Tika compatibility profile for the standard rendition bridge |
| [`document/datalab`](https://github.com/kenn-io/docbank/tree/main/document/datalab) | Hosted | Uploaded files through Datalab Convert |
| [`document/mistral`](https://github.com/kenn-io/docbank/tree/main/document/mistral) | Hosted | Capability-probed PDF, PPTX, optional renderpdf-backed office files, and fourteen text-family OCR formats, including the rendition-provider adapter |
| [`document/llamaparse`](https://github.com/kenn-io/docbank/tree/main/document/llamaparse) | Hosted | Resumable PDF parsing through the fixed LlamaParse v1 API |
| [`document/reducto`](https://github.com/kenn-io/docbank/tree/main/document/reducto) | Hosted | Resumable PDF and PPTX parsing through the fixed Reducto API |
| [`document/bridge`](https://github.com/kenn-io/docbank/tree/main/document/bridge) | Declared service | `docbank-rendition/v1`: submit, poll, cancel, and validate canonical source evidence |

Unstructured and Tika supply compatibility profiles for your bridge, and they
do not install those services. Local-process providers also require you to
supply the pinned runtime. If you run Trafilatura on macOS or Windows, you must
supply an audited `IsolatedRunner`.

The rendition contract accepts an `AuthorizedUpload` bound to one inspected
source, and providers cannot substitute a source URL for those bytes. Each
receipt records the source, policy, provider identity, and usage. For the
normative types and wire format, see the
[provider contract](https://github.com/kenn-io/docbank/blob/main/document/provider.go)
and [rendition bridge schema](https://github.com/kenn-io/docbank/blob/main/document/bridge/openapi.yaml).

## Convert PDF page ranges with Docling

To convert inclusive, one-based PDF pages, call `client.RenderPageRange` with a
`docling.PageRange`, such as `{First: 51, Last: 100}`. Like `Render`, each call
consumes its authorized upload, so create a fresh upload and authorization for
each range. Every call uploads the whole PDF and sends Docling Serve's
[`page_range` option](https://github.com/docling-project/docling-serve/blob/main/docs/usage.md).
Your page-range bounds must be positive and ordered, and the input must be a
PDF. Docbank doesn't count pages. Keep `Last` at or below the page count from
inspection, or the call fails.

Range results require a supported `DoclingDocument` 1.x schema, and missing or
unsupported schemas fail as malformed evidence before Markdown fallback. When
the result includes a page registry, its keys must be exactly `First..Last`.
Every nonblank text's provenance page must also fall inside the selection, even
when the registry is missing or null, while blank text is ignored. Contradictory
selections fail as malformed evidence without retry, and page units keep their
original page numbers.

Every range result carries a field omission named by
`docling.PageRangeOmissionField` (`page_range`), so you can't mistake it for
the whole document. Structured ranges report partial coverage, and any range
after page 1 also names the preceding pages as a unit omission. If structured
page evidence in a supported schema is unusable, Docbank falls back to bounded
provider Markdown with degraded provenance. Blank selections retain page units
when Markdown is empty or whitespace-only. Whole-document `Render` also permits
Markdown fallback for missing or unsupported schemas. Your application owns
range scheduling, retries, and aggregation.

## Extract PDF text locally

Use `document/pdftext` to extract PDF text in process through embedded PDFium
as `pdftext.in-process-v1`. Construct it with
`pdftext.New(pdftext.Profile{MaxDocumentBytes: 16 << 20, MaxPages: 1000})`.
In the daemon, select this provider with `docbank-pdftext-rendition/v1`; see
[configuration](usage/configuration.md#local-pdf-text-rendition).

Present units keep one-based page locations, and any omission makes the
evidence partial. A page with zero native text-layer characters produces a
`no_text_layer` unit omission, while a page whose native characters extract to
empty text or only whitespace and control characters records a
`no_readable_text` omission. An entirely image-only PDF publishes its page
omissions with an empty Markdown body, zero lexical segments, and empty
navigation. Extraction removes PDFium's inferred line-wrap hyphen marker
(U+FFFE) to join wrapped words for search while leaving ordinary hyphens
intact, and strips other control characters except line feed, carriage return,
and tab. The retained Markdown keeps its frontmatter envelope even when its
body is empty. Older binaries refuse omission-only evidence or its empty
Markdown body. Use a version that supports local PDF text to read these
results.

Text-only admission accepts DCT, JBIG2, and JPX image streams without decoding
pixels and permits external references, though extraction opens no external
resource. Other adapters keep their existing admission rules, and encrypted
PDFs retain the `encrypted_container` refusal. Source bytes, encoded streams,
ordinary decoded streams, page counts, text, and result bytes remain bounded.

You can set page limits from 1 through 100,000. Parser, limit, and cancellation
errors fail the build, while omissions describe absent or unreadable text.
Daemon processing authorizations expire after ten minutes, so a PDF can exceed
its processing time budget within the configured page limit. For each page, the
provider opens a seekable view of the verified source in a fresh runtime with a
512 MiB WASM memory cap, a 60-second execution deadline, and the existing
process RSS watchdog. Waiting for the shared engine uses the job context and
authorization expiry, and each completed page releases the engine so a waiting
redaction job can acquire it.

## Extract EPUB locally

Use `document/epub` to extract text in process as `epub.in-process-v1` from one
reflowable EPUB package that has XHTML spine content and no
`META-INF/encryption.xml` entry. The provider rejects encrypted content and
IDPF/Adobe font obfuscation as unsupported. Construct it with
`epub.New(epub.Profile{MaxDocumentBytes: 16 << 20, MaxUnits: 1000})`. Both
limits are required: bytes range from 1 through 500 MiB, and virtual units
range from 1 through 1,000,000. The descriptor reports `local_process` and
binds both limits to its policy identity.

To use the provider with an embedded vault, pass it through
`ProcessingProfileConfig.RenditionProvider` along with its processing profile.
In the daemon, select it with the `docbank-epub-rendition/v1` adapter and set
`max_document_bytes` and `max_units` to match the limits you used to construct
the descriptor. Use the public planning and consent flow before processing.
Filename disclosure follows the profile's `disclose_filename` setting, and
extraction works with the filename disclosed or withheld.

Each declared spine occurrence produces one ordered evidence unit with its
archive path, and repeated references and `linear=no` entries count separately.
Empty entries keep their location and consume zero virtual units. Head metadata
and active XHTML content contribute no text, and images and visual layout have
limited fidelity.

Usage counts the complete normalized Markdown with LF line endings. Each line
wraps virtually at 80 Unicode code points, 48 virtual lines make one unit, and
every spine entry starts a new page. A 100-line chapter can therefore remain
one evidence unit while consuming three virtual units. Virtual wrapping leaves
retained Markdown unchanged. Configure the processing profile's retained unit
and text limits separately from the provider's virtual-unit ceiling.

The provider verifies the authorized source length and hash, applies the format
detector's ZIP limits, and rejects unsupported packages with
`unsupported_input`. Output, work, or virtual-unit overflow returns
`policy_rejected`, while a cumulative total exactly equal to `MaxUnits` is accepted.
Truncated output and partially processed books produce no successful provider
result.

## Build canonical evidence and a rendition

When an extractor can report source locations and structure, call
`NormalizeEvidenceV1` to validate `SourceEvidenceV1` and produce
`NormalizedEvidenceV1` with stable identities for ordered units, regions,
artifacts, and omissions. Locators distinguish pages, slides, sheets, records,
messages, lines, media segments, and other document units. Media segments use
half-open millisecond intervals where the start is included and the end is
excluded. Adjacent and overlapping segments are valid, and an absent time
remains absent. Completeness is reported as `complete`, `partial`, or
`degraded_provenance`. Readable text alone does not prove complete source
coverage.

Call `BuildRenditionV1` to derive sanitized Markdown, normalized units, and
lexical segments from that evidence. Lexical segments are model-independent
text spans for keyword search, while embedding chunks are built separately with
the selected model's tokenizer. Call `MarshalNormalizedEvidenceV1` to produce
the canonical retained evidence bytes and checksum.

For the owning contracts, see
[evidence](https://github.com/kenn-io/docbank/blob/main/document/evidence.go),
[evidence validation and normalization](https://github.com/kenn-io/docbank/blob/main/document/evidence_codec.go),
and [renditions](https://github.com/kenn-io/docbank/blob/main/document/rendition.go).

## Normalize provider output

If you only need text normalization for ordered source units such as pages, use
`document.NormalizeDocument`. It turns those units into consistent text and
returns heading paths, chunks, checksums, and source spans that locate each
result in the original input. The same input and policy always produce the same
result.

```go
policy, err := document.NewNormalizePolicy(2_000_000)
if err != nil {
	return err
}

normalized, err := document.NormalizeDocument(document.SourceDocument{
	Family:   "pdf",
	UnitKind: "page",
	Units: []document.SourceUnit{
		{Index: 0, Markdown: "# Quarterly report\n\nRevenue increased."},
	},
}, policy)
if err != nil {
	return err
}
```

A policy's structural values are fixed for its normalization version, and the
only input you choose is the maximum normalized document size. Any future
change to the algorithm or structural values requires a new normalization
version, so existing checksums keep their meaning. Source family and unit-kind
identifiers must be valid UTF-8 without control characters so they remain
stable in fingerprints and provider-facing locators. Version 3 binds truncation
into unit, chunk, and document checksums. A complete shorter document therefore
cannot share an identity with a longer document whose retained evidence or
remaining units were truncated by a normalization bound.

## Build evidence from a supplied transcript

If your application already has transcript text, you can pass it through the
same evidence and rendition contracts as other document families:

```go
evidencePolicy, err := document.NewEvidencePolicy(100_000)
if err != nil {
	return err
}
evidence, artifact, err := document.BuildTranscriptEvidenceV1(
	document.SuppliedTranscript{Provider: "beeper", Text: transcript}, evidencePolicy,
)
if err != nil {
	return err
}
_ = artifact // retain the provider transcript with the source record
```

By default, the daemon accepts supported audio originals up to 4 GiB, and
upload, staging, hashing, inspection, publication, and sealed provider spooling
all use bounded buffers. The built-in supplied-transcript and supplied-captions
profiles accept these originals without loading the audio into memory, while
their transcript and response limits remain 16 MiB. To set a smaller source
ceiling, configure
[`[media].max_source_bytes`](configuration.md#supplied-audio-transcription), or
set `ProcessingOptions.MediaMaxBytes` for an embedded vault. Smaller caller
inspection and profile policies still apply, hosted transcription providers
keep their own input limits, and audio duration remains limited to 24 hours.

Use a lowercase identifier for the provider. The `supplied-transcript/v1` JSON
artifact keeps the exact provider name and transcript text, while normalized
evidence and the rendition still apply their Unicode, Markdown, and character
limits.

The evidence uses the `audio` family, a generic unit, and
`degraded_provenance` completeness. Supplied text has no timing or speaker
information, and your application must associate it with the audio source and
source version. This function does not inspect audio, run speech recognition,
verify the provider's identity, or create ingestion and search records.

If you want the same transcript to travel through the rendition contract,
sealed upload, authorization, receipt, and retained artifact, construct a
`document/suppliedtranscript` provider whose `Source` uses the sealed audio
digest as its key. The provider identity includes your source binding. Use a
different binding for each transcript source to give it a distinct descriptor.
Docbank cannot verify that the binding identifies the `Source` implementation
or the transcripts it returns.

The audio capability path also accepts M4A originals with `.m4a` and
`audio/mp4`, with `audio/x-m4a` as an alias. While streaming the original, it
verifies sound-only AAC/ALAC track metadata, local sample ranges, and finite
duration, and the local transcript and caption providers can use that
capability without decoding audio. For container requirements and byte
ceilings, see [media recordings](cli-reference.md#docbank-media). Docling ASR
and Gemini audio qualification remain limited to WAV and MP3.

When the source supplies segment timing, use `document/mediatranscript`. Its
`media-transcript/v1` artifact retains ordered text, start and end
milliseconds, and an optional speaker for each segment. `ParseSubRip` accepts
the bounded `subrip/v1` grammar, keeps styling tags verbatim, and infers
nothing about language, speakers, or timing. `Build` converts that artifact
into audio or video source evidence and a retained transcript artifact. Segment
starts must not regress, while adjacent and overlapping cues remain distinct.
Speaker text participates in evidence identity when present, and omitting it
keeps the supplied form free of invented values.

### Supplied caption timing limits

If any cue ends after the recording's measured duration, even by one
millisecond, the `supplied-captions` profile rejects the entire caption file.
It preserves the supplied times without rounding, clipping, or adding a
tolerance. Check that the captions match that recording before you import them.

## Run Mistral OCR safely

Docbank refuses Mistral uploads until you supply a validated capability
manifest for the configured endpoint, model, and policy. A capability manifest
records what a live provider probe demonstrated. Docbank supplies no live
manifest, and provider documentation alone does not authorize uploads.

To prepare that evidence:

1. Call `WriteProbeFixtures` to create synthetic test documents.
2. Call `ValidateProbeFixtures` locally, without credentials or HTTP.
3. Create `NewClient(API key)` and call `RunCapabilityProbe`.
4. Review and retain the resulting `CapabilityManifest`.

Fixture generation creates the candidate formats deterministically, while five
legacy formats require operator-supplied synthetic seeds named `doc`, `ppt`,
`xls`, `numbers`, and `msg`. Fixture and staging directories must be private.
The authenticated probe authorizes PDF through a provider-request bound, and it
can authorize PPTX when its local slide count matches the provider's processed
units. After provider acceptance, it records primary-fixture evidence for TXT,
Markdown, CSV, JSON, JSONL, YAML, Go, Python, JavaScript, RST, LaTeX, XML, EML,
and MSG. Native XLSX remains unauthorized through the capability probe, even
when extraction succeeds during a probe. Configured render policies use the PDF
authority for XLSX instead.

Text formats use `provider_response` enforcement rather than counting lines,
records, or messages as pages. Docbank checks that Mistral returns at least one
page, that the number of returned pages equals `pages_processed`, and that both
stay within `MaxUnits`. A CSV with more than 5,000 rows can therefore be
submitted when it fits the input byte limits.

Before upload, `MaxDocumentBytes` limits every file, and text detection rejects
text inputs above 50 MiB. These byte limits do not establish a provider page or
spending limit, and Mistral may process and charge for a document before
Docbank rejects an over-limit response. If your application requires a
pre-upload page or spending bound, do not authorize text formats through this
route.

Capability manifests use schema v4, and existing v3 manifests are rejected for
all formats, including PDF and PPTX. Rerun the authenticated capability probe
and replace the manifest before processing documents. If validation reports
that a registered format "does not explain its unverified bound", rerun the
probe as well, and review application consent against the resulting policy
fingerprint.

A release that changes the pinned model rejects manifests recorded for the old
model, so rerun the probe and review consent. If every result returns
`invalid_model`, Mistral no longer serves the pinned model.

DOCX, DOC, ODT, RTF, PPT, XLS, ODS, and XLSX share one production route. Set
`PolicyConfig.RenderPDF` to a validated `renderpdf.Policy` and supply PDF
capability evidence in the same manifest. `Policy.Authorize` keeps the original
format identity while granting a local-exact bound for the render route. Native
office probe results remain diagnostic observations and do not authorize native
office bytes.

For each production document:

1. Call `Policy.Authorize(validated manifest, declared format)`.
2. Check your application's consent record against `PolicyFingerprint`.
3. Call `Prepare` with private staging storage, or `PrepareMemory` to avoid
   staging files for direct uploads. Formats rendered to PDF still use the
   renderer's temporary files.
4. Call `Process`. The detected format must match the authorization.
5. Call `Release` on success or failure.
6. Call `NormalizeDocument(Result.Document, Policy.NormalizePolicy())` on a
   successful result.

`Prepare` copies one input into a private, bounded, immutable staging file and
verifies your `ExpectedSize` and `ExpectedSHA256`. `PrepareMemory` keeps a copy
in memory, computes its size and SHA-256, and zeroes that copy when you call
`Release`. If you use `PrepareMemory` and expect a known hash, compare
`SHA256()` yourself. For direct formats, `Process` reopens and verifies staging
files on every attempt, or clones the private memory copy without re-hashing
it. It derives request options from the policy and authorization, bounds the
response, and converts validated provider output into `document.SourceDocument`.
For a configured render profile, it calls `renderpdf.Convert` once, counts the
generated PDF, checks `MaxUnits`, `MaxPages`, `MaxPDFBytes`, and
`MaxDocumentBytes` before HTTP, and uploads that PDF on every retry. The
returned document keeps the original family and page unit kind, and its result
carries both the original and generated PDF hashes. Call `Release` on every
success or failure path.

To display a local preparation failure, follow
[Show preparation failures](#show-preparation-failures), and keep the original
error for retry and cancellation decisions.

The rendition adapter verifies source identity before submission. For PDFs, it
compares the returned page count with that inspected count. For PPTX, it counts
the listed PresentationML slides, rejects invalid slide references or
over-limit decks before upload, and compares the provider's processed count
with that local count. Configured office formats use the shared render route.
Rendered Word keeps complete page evidence, while rendered presentations and
spreadsheets use one generic degraded evidence unit with an omission because
PDF pages do not identify the original units. The receipt records the generated
PDF hash and input bytes, and text formats use the response checks described
above. For the source and result checks, see
[Mistral rendition processing](https://github.com/kenn-io/docbank/blob/main/document/mistral/rendition.go).

The PPTX count always includes hidden slides. That count assumes Mistral
processes every listed slide. The probe fixture contains one visible slide and
does not verify hidden-slide behavior. If Mistral skips hidden slides, the
count comparison fails after upload and may incur provider charges.

Native XLSX production uploads remain blocked because its native provider
authority is unverified. With `PolicyConfig.RenderPDF`, XLSX follows the render
route and Mistral enforces the generated PDF page count before upload. The
returned processor result keeps the original family and page unit kind, and
rendition evidence for rendered presentations and spreadsheets uses generic
degraded provenance.

Your application remains responsible for credentials, human consent, spending
and scheduling limits, durable manifests, job orchestration, persistence, and
search serving. Those application decisions stay outside the reusable packages
and their policy identity.

## Prepare text for semantic retrieval

Use `document.BuildEmbeddingInputs` to convert canonical normalized evidence
into a sealed generation of provider-ready document inputs. It never reads the
retained Markdown, so YAML frontmatter, checksums, build identifiers, and
navigation metadata cannot enter embedding text.

The processing profile's `EmbeddingChunkPolicyV1` declares how to build chunks.
It records the tokenizer name and revision, content token budget, overlap,
formatter, truncation policy, and attachment-context rules. A tokenizer
determines the token units that the model counts.

`document.NewInputPolicy` combines that declaration with:

- A `document.Tokenizer` implementation reporting the declared identity.
- The binding's fixed model-input contract.
- The lexical evidence fingerprint, which identifies the text evidence.
- The provider's hard token and byte limits for the complete rendered input.

The resulting policy belongs to one rendition-chunk binding.

```go
policy, err := document.NewInputPolicy(binding, tokenizer, lexicalFingerprint, nil)
if err != nil {
	return err
}

generation, err := document.BuildEmbeddingInputs(evidence, policy, document.GenerationLimits{
	MaxInputs:              10_000,
	MaxTotalContentTokens:  4_000_000,
	MaxTotalRenderedTokens: 5_000_000,
	MaxTotalContentBytes:   64 << 20,
	MaxTotalRenderedBytes:  80 << 20,
	MaxFittingWorkTokens:   50_000_000,
	MaxFittingWorkBytes:    1 << 30,
})
if err != nil {
	return err
}
```

Chunking prefers page, heading, region, and table boundaries before splitting
by tokens. The configured token budget applies to content only. Docbank renders
the document role envelope and any declared attachment context on top of that
content, then recounts the complete rendered input against the provider limits.
Overlap is measured in emitted tokens, and every input keeps its heading path
and the source span it came from.

Generation limits can reject a build, and they cannot change its output or
enter the fingerprints. All other policy values enter the generation's policy
fingerprint. If you declare `AttachmentContextSnapshot`, it also binds the
attachment titles and context to the generation's identity.

Use `MarshalEmbeddingInputGeneration` to produce the canonical byte form.
`DecodeEmbeddingInputGeneration` accepts only that form under the limits you
supply, rejecting forged totals, policy fingerprints, and checksums.
`ToEmbeddingInputs` accepts only the model-input contract used to build the
generation.

`EgressIdentity` gives you separate endpoint-sensitive fingerprints for
document embedding and query embedding, without including credentials.
`VectorSpaceIdentity` separately pins provider, model revision, dimension, and
normalization without an endpoint, so your application can reuse compatible
vectors while still requiring fresh consent when their destination changes.

The shared retrieval helpers default omitted and `auto` search to lexical mode,
so no query goes to an embedding provider unless you select `semantic` or
`hybrid` mode. Candidate limits default to 100 and cannot exceed 1,000.
`CollectScopedCandidates` requires the backend to apply scope before its vector
cutoff and pages until authoritative exhaustion or a `limit+1` overflow probe.
Reciprocal-rank fusion preserves lexical and semantic signals and reports
overflow from either input lane or the fused union.

Use `document/embedding/eval` to run versioned public or synthetic corpora
through repeatable retrieval systems. Reports include Recall@5/10/20, nDCG@10,
MRR, hit@1, hit@10, critical misses, provider calls and input/output usage,
query latency, token evidence, and cost evidence. You can add an optional
reranker over a top-N prefix; ties keep their retrieval order and the remaining
results stay in place. Repeated trials retain query observations and report
empirical minimum, mean, maximum, and nearest-rank p95 values. The developer
comparison runs against a temporary real Docbank store and the existing
lexical, vector-index, and fusion owners, using synthetic documents only as
test data. Offline provider rows and synthetic vector rows validate wiring
only, so they do not support a quality recommendation. Missing provider runs or
usage evidence remain unavailable, skipped rerank calls add no usage or pricing
basis, and cost totals use only observations from stages that ran. Keep raw as
the default until measured results justify a different recipe.

To run the live comparison, set `COHERE_API_KEY` and `TYPESAFE_API_KEY`, then
run `RERANK_EVAL_LIVE=1 go test -tags fts5 ./scripts/rerank-eval -run
'^TestLiveRerankComparison$' -count=1 -v` from the repository root. Ordinary
tests compile the live adapter setup and check pricing inputs without making
live calls. To include costs, also set `RERANK_PRICING_DATE` (`YYYY-MM-DD`),
`TYPESAFE_MICROS_PER_TOKEN`, and `COHERE_MICROS_PER_SEARCH_UNIT` together.

### Passage reranking evaluation

The offline passage evaluation compares four reranking arms on the same frozen
hybrid candidates: no rerank, today's exact reranker excerpts, and up to one or
three selected passages per candidate (P1 and P3). It exists to decide whether
matched passages should replace document-head excerpts. It does not change
serving search, processing configuration, or defaults.

The selector draws passages from two lanes and interleaves their ranks:

- **Lexical segments.** A segment is eligible only when it matches the native
  lexical query, which requires every query term as a prefix (AND-of-prefix),
  the same rule normal lexical search uses.
- **Semantic chunks.** Eligible chunk neighbors come from the serving
  eligibility rules, with 1,536-rune chunks and 128-rune overlap.

Each passage becomes a 1,536-byte UTF-8 window. The window centers on the
first native prefix match of any query term in the passage's own text, with
the same punctuation and accent folding; text without a match starts at its
head. Overlapping passages from the same source, build, and unit are removed.
Reports disclose excluded matches, fallbacks, unknown coordinates, late
segments, and the 4,096-row lexical selection cap. Stale artifacts, selection
caps, provider failures, or missing scores make a run inconclusive, and failed
arms keep the frozen baseline order.

The hooks that reach into processing, retrieval, and the store build only with
the `rerankeval` tag and support SQLite only. Ordinary CI runs the untagged
scoring and windowing tests in `document/embedding/eval`, plus the
`scripts/rerank-eval` tests for incomplete-report serialization and Voyage
attempt accounting. It does not compile the `rerankeval` files, so the gate's
pass, fail, and threshold decisions and the bootstrap intervals are tested only
under that tag. To check the selector and gate mechanics on synthetic data
without a provider, run this manually:

```sh
go test -tags 'fts5 rerankeval' ./internal/store ./internal/retrieval \
  ./scripts/rerank-eval -run '^Test(Passage|Evaluation)' -count=1
```

These checks validate mechanics only and supply no quality evidence.

#### Outcome of the 2026-10-30 gate

The `2026-10-30v1` gate was preregistered in
[pull request #1171](https://github.com/kenn-io/docbank/pull/1171), with fixed
sample, models, seed, thresholds, and a two-hour, $25 budget. Its result is
**inconclusive**. Three attempts ran on 2026-10-30:

1. The first stopped during control preparation, before any reranker call.
2. The second was interrupted before public ranking after review found a
   window-focus defect.
3. The third, at commit `95b705931077e22479272baeeae6ecebe8ad93e4`, reached
   its fixed cutoff during corpus preparation with 2,105 of 2,800 public
   documents ready. It ranked 0 of 100 public queries and no long-positive
   proxy queries; the gate required at least 30.

Public quality, latency, and input-cost measurements are unavailable. The
short and synthetic long controls completed, but they are diagnostics and
cannot stand in for the public gate. The evaluation therefore provides no
evidence for changing passage selection. Production defaults are unchanged.
Under the preregistered rule, lexical passage selection, semantic chunk
passages, and any default change from
[issue #1100](https://github.com/kenn-io/docbank/issues/1100) do not proceed on
this result. A new experiment, including a smaller sample or longer budget,
needs a new preregistration.

The recorded result is pinned to commit
`95b705931077e22479272baeeae6ecebe8ad93e4`. Later source edits were not used
for any of the three attempts.

For reference, each of P1 and P3 had to meet every bound below against the
current-excerpt arm:

| Measure | Required result |
| --- | --- |
| Proxy nDCG@10 difference | Lower 95% confidence bound > 0 |
| Full-set nDCG@10 difference | Lower bound ≥ -0.01 and upper bound ≥ 0 |
| Existing short corpus and four deterministic long controls | Mean nDCG@10 loss ≤ 0.01 on each corpus |
| p95 selection plus provider time | ≤ 5 seconds and ≤ 1.5 times current |
| Mean input text bytes and local token estimate | Each ≤ 4 times current |

The public sample came from MLDR English test, prepared by the
[pinned conversion recipe](https://github.com/kenn-io/docbank/tree/main/scripts/rerank-eval/mldr).
MLDR labels whole articles without answer offsets, so the gate used a
**long-positive visibility proxy**: queries with a positive article of at least
8,192 UTF-8 bytes whose full text is absent from its current excerpt.
Critical-span evidence is unavailable.

## Choose an embedding provider

The adapters below implement `document.EmbeddingProvider`, and their
descriptors pin the model-input contract, input kind, vector dimensions, and
compatibility identity. A provider package is not a daemon configuration
option. Your application must supply matching profiles, authorization, and
named credential resolution.

| Provider package | Contract and scope |
|------------------|--------------------|
| [`document/openaicompat`](https://github.com/kenn-io/docbank/tree/main/document/openaicompat) | Explicit operator-hosted OpenAI-compatible text embeddings; deployment identity is required |
| [`document/openai`](https://github.com/kenn-io/docbank/tree/main/document/openai) | Fixed hosted `text-embedding-3-large` contract; separate from the operator-hosted adapter |
| [`document/mistral`](https://github.com/kenn-io/docbank/blob/main/document/mistral/embedding.go) | Hosted `mistral-embed` text embeddings |
| [`document/voyage`](https://github.com/kenn-io/docbank/blob/main/document/voyage/embedding.go) | Hosted Voyage 3.5/4 text, `voyage-context-4`, and capability-authorized direct-file embeddings |
| [`document/cohere`](https://github.com/kenn-io/docbank/tree/main/document/cohere) | Hosted `embed-v4.0` text and inspected-image inputs; distinct document and query roles |
| [`document/gemini`](https://github.com/kenn-io/docbank/tree/main/document/gemini) | Hosted `gemini-embedding-2` text and authorized image, audio, video, and PDF files |
| [`document/zeroentropy`](https://github.com/kenn-io/docbank/tree/main/document/zeroentropy) | Hosted `zembed-1` text with explicit output encoding, latency policy, and dimensions |
| [`document/embeddingbridge`](https://github.com/kenn-io/docbank/tree/main/document/embeddingbridge) | Synchronous `docbank-embedding/v1` transport for text and authorized original files, plus `v2` for image inputs |

`openaicompat.BGEM3Profile` and `openaicompat.Qwen3Profile` build reviewed
self-hosted profiles. BGE-M3 uses one dense 1,024-dimensional vector, while
Qwen3 supports its 0.6B, 4B, and 8B models at their native dimensions and
requires a query instruction. Both pin weights and tokenizer revisions,
pooling, and sequence limits, and both exclude sparse and multi-vector outputs.
For details, see the
[deployment contracts](https://github.com/kenn-io/docbank/blob/main/document/openaicompat/profiles.go).

The existing custom text contract also supports an optional
[EmbeddingGemma 2 native-768 recipe](configuration.md#optional-embeddinggemma-2-text-recipe).
Its configuration and synthetic transport checks do not qualify a live model
deployment, and you supply the server and its immutable recipe.

Voyage text profiles can embed retained rendition chunks and serve text queries
when you supply `EmbeddingProfile.DeploymentEpoch`, set the descriptor's
`ModelRevision` to that epoch, and enable `SupportsTextQuery`. The native
adapter sends document and query roles, pins dimensions, and requests float
vectors without `encoding_format`. It accepts and discards the optional string
`text` response item member, while all other unknown members still fail
decoding. Other providers keep their existing strict schemas. Finite, nonzero
vectors must match the pinned dimension. Retrieval profiles require cosine
distance and unit-length vectors whose squared norm differs from one by at most
`1e-4`, and the adapter never silently normalizes provider drift. For supported
models and your epoch responsibilities, see
[Voyage text runtime configuration](configuration.md#voyage-text-retrieval).

Voyage contextual requests contain one document's ordered chunks. Unpinned
Voyage profiles and Mistral's mutable hosted aliases remain export-only and do
not advertise a serving-time text query encoder. Do not infer query
compatibility from an equal vector dimension.

Gemini's profile selects either inline bytes or the Files API, and direct-file
requests need matching inspected capability and disclosure fingerprints. The
Files API path includes bounded polling and cleanup, and its policy identity
records the provider retention ceiling. The direct-file adapter accepts
PNG/JPEG images, WAV/MP3 audio up to three minutes, MP4/QuickTime video up to
two minutes, and PDFs up to six pages, subject to matching inspection bounds.
The
[Gemini contract](https://github.com/kenn-io/docbank/blob/main/document/gemini/profile.go)
separates that provider retention from Docbank's local derivative retention.

The standard embedding bridge sends a canonical manifest before any file parts
and validates the synchronous response against the input identities. Version 1
handles text and authorized original files, and version 2 handles image
previews. Its
[OpenAPI contract](https://github.com/kenn-io/docbank/blob/main/document/embeddingbridge/openapi.yaml)
and JSON schemas own the wire limits.

## Detect and bound visual attachments

Use `media.Detect` to identify JPEG, PNG, WebP, GIF, and MP4 from their bytes.
It does not trust the declared media type or decode pixels and samples; instead,
it reads the metadata needed to enforce input limits:

- Dimensions, including every declared axis bound for MP4 and every GIF frame
  within the logical screen.
- Verified frame counts, including animated WebP and APNG.
- MP4 duration, using the longest movie, track, media, or sample-timing value.

MP4 video tracks must contain out-of-band H.264 (`avc1`/`avcC`) or H.265
(`hvc1`/`hvcC`) dimensions, plus `mdhd` and `stts` timing. The detector rejects
in-band or missing evidence as malformed and does not substitute a smaller
container summary.

`media.Evaluate` checks byte, pixel, frame, and duration caps along with the
policy's still-image, animation, and video settings, returning stable reasons
such as `too_many_pixels` or `animated_not_allowed`. If the policy caps
duration, it refuses video with an unmeasurable duration.

```go
metadata, reason, err := media.Inspect(reader, size, "image/png", media.DefaultPolicy())
if err != nil {
	return err
}
if reason != media.ReasonEligible {
	return recordIneligible(reason)
}
```

This package has no notion of attachment ownership, roles, or hashes; handle
those as application pre-filters.

## Embed images and video with Voyage

Voyage uploads fail closed until you produce and supply a validated capability
manifest for the pinned endpoint, model, and dimension. Docbank ships no live
manifest. Each capability is authorized separately: JPEG, PNG, WebP, still GIF,
animated GIF, and MP4 documents; text queries; JPEG, PNG, WebP, and still GIF
image queries; text-then-PNG queries; text-then-media documents probed per
format; and mixed-format batches at the policy limit. Recorded probe evidence
decides whether animated GIF, video, or a given query shape may be sent.

To prepare a manifest:

1. Call `WriteProbeFixtures` to generate JPEG, PNG, and GIF fixtures and copy
   operator-supplied WebP and MP4 seeds.
2. Call `ValidateProbeFixtures` locally, without credentials or HTTP.
3. Create `NewClient(API key)` and call `RunCapabilityProbe`.
4. Review and retain the resulting `CapabilityManifest`.

Because the Go standard library cannot encode WebP or MP4, you supply synthetic
seeds named `image_webp.webp` and `video_mp4.mp4`. You also supply
`image_webp_alt.webp` and `video_mp4_alt.mp4`, which are variants of the same
formats with different content, so that pixel contribution is demonstrated
within each format. The seed directory, destination parent, and published fixture directory
must be owner-private. Fixture generation publishes a complete new directory
atomically instead of updating an existing one. The probe stores no media,
vectors, or responses, and the manifest records only sanitized pass, reject,
and fail observations.

For each production item:

1. Call `Policy.AuthorizeAll(validated manifest)`.
2. Check your application's consent record against `PolicyFingerprint`.
3. Call `media.Inspect` and require the item to be eligible under the policy.
4. Call `Client.EmbedDocuments(inputs, authorizations)`.

`EmbedDocuments` accepts only the probed input shapes `[media]` and
`[text, media]`. Before allocating a request, it enforces item and byte limits,
and it also:

- Detects every media part again and serializes from the detected metadata.
- Requires authorization for each part's format and animation state.
- Requires that format's interleaved capability for `[text, media]`.
- Requires the batch capability for more than one input.

`EmbedQuery` accepts `[text]`, `[image]`, or `[text, image]`, and each shape
needs its own probed capability. Image authorization is per format, and
combined text and image queries support PNG only.

Every response vector must have a unique index and the exact dimension. The
client does not follow redirects, and provider response bodies do not appear in
errors. Use `IsRetryable`, `RetryAfter`, and `MetricsFromError` when scheduling
retries.

Your application remains responsible for credentials, human consent, spending
and scheduling limits, durable manifests, job orchestration, vector storage,
and search serving. Importing this library does not enable a vault workflow.
For the daemon's separate retained-job requirements, see
[Configuration](configuration.md#embedding-workers-and-credentials).

## Convert CSV locally for PDF OCR

Use `document/csvpdf` to convert a declared `text/csv` source into a PDF for
the existing PDF OCR path. Conversion runs locally and never uploads content or
looks up credentials.

Before returning a result, the converter verifies the source byte count and
SHA-256, parses records, labels each cell, and counts PDF pages. It closes
`ocr.Source.Content` on every success and failure path.

```go
policy, err := csvpdf.NewPolicy(csvpdf.DefaultLimits())
if err != nil {
    return err
}
converted, err := csvpdf.Convert(ctx, originalCSV, policy)
if err != nil {
    return err
}
receipt := converted.Receipt()
pdfSource, err := converted.Source()
if err != nil {
    return err
}
// Keep the receipt with pdfSource before a later caller authorizes upload.
```

If conversion fails, follow [Show preparation failures](#show-preparation-failures)
to choose a display message.

The defaults are also hard ceilings, and `NewPolicy` accepts positive, tighter
limits.

| Limit | Maximum |
|-------|---------|
| Source bytes | 1 MiB |
| PDF bytes | 10 MiB |
| Records | 10,000 |
| Cells | 50,000 |
| Bytes per cell | 64 KiB |
| Generated pages | 100 |

The embedded Go Mono font covers a subset of Latin, Greek, Cyrillic, and common
punctuation or symbols. Conversion rejects missing glyphs, characters outside
the Unicode basic multilingual plane, combining marks, shaping scripts,
formatting controls, and control characters other than cell newlines. Use
precomposed accented characters such as `é`.

CSV quoting, blank lines, and CRLF normalization follow Go's `encoding/csv`.
The output preserves empty cells, variable-width records, and quoted newlines,
with long cells continuing onto later pages and formulas kept as literal text.

Keep the receipt with the source. It records the original and generated hashes
and byte counts, converter version, policy fingerprint, and mapping from
generated pages to original records and cells. Span identifiers start at one,
and a record may span several pages. To cite a CSV cell from PDF OCR output,
use this mapping to translate the generated page reference.

The receipt does not authorize upload. Before uploading, your application must:

1. Verify the PDF capability manifest and consent.
2. Choose PDF byte and page limits large enough for the conversion.
3. Retain the conversion policy with that consent.

This Go API does not add CSV OCR to the daemon or CLI.

## Show preparation failures

`mistral.Prepare`, `mistral.PrepareMemory`, and `csvpdf.Convert` attach
`ocr.PreparationError` to local source and conversion failures. Extract it with
`errors.AsType[*ocr.PreparationError]` and display its `Error()` value, which
contains fixed text. The original cause remains available through `Unwrap()`
for private diagnostics.

Never display the original `err.Error()` or an unwrapped cause. Even when an
error contains a `PreparationError`, the full error may include joined cleanup
errors with filesystem paths or parser details. If no `PreparationError` is
present, display a fixed fallback message.

Check cancellation and deadlines first. A canceled operation may also contain
a description for a source-close failure:

```go
func preparationMessage(err error) string {
    switch {
    case errors.Is(err, context.Canceled):
        return "Document preparation canceled"
    case errors.Is(err, context.DeadlineExceeded):
        return "Document preparation timed out"
    }
    if description, ok := errors.AsType[*ocr.PreparationError](err); ok {
        return description.Error()
    }
    return "Document preparation failed"
}
```

Use the original error for scheduling decisions rather than matching display
descriptions. For `ocr.Processor` failures, call `ocr.ErrorKindOf(err)`. For
direct `mistral.Prepare` calls, check `errors.Is` against
`mistral.ErrSpoolCapacity`, `mistral.ErrSpoolUnavailable`, and
`mistral.ErrInvalidSource`. `mistral.PrepareMemory` returns
`mistral.ErrInvalidSource` for invalid content and never returns spool errors.
In all of these paths, and for `csvpdf.Convert`, use `errors.Is` for context
errors. Display descriptions are not retry classifications. Do not match their
text to decide whether to retry.

## Convert office files locally to PDF

Use `document/renderpdf` when your application needs a local PDF derived from
DOCX, DOC, ODT, RTF, PPT, XLS, ODS, or XLSX. Using an operator-pinned
LibreOffice executable, the converter first writes FODT, FODP, or FODS inside
the Linux sandbox, checks the normalized XML for external or active content,
and then renders only admitted bytes to PDF.

### Set up the renderer

The built-in runner targets Linux on amd64 and arm64. It requires user, mount,
PID, network, and IPC namespaces, seccomp filters, and Landlock filesystem ABI 3
or newer, and your application must be allowed to create those namespaces. On
Ubuntu, AppArmor may block unprivileged user namespaces. Your AppArmor policy
must allow the launcher to create them. Container policy must also permit the
required namespaces. If the required isolation cannot be installed, conversion
fails. On macOS and Windows, you must supply an audited `renderpdf.Runner`.

On Debian or Ubuntu, install LibreOffice Writer and record the approved
executable's SHA-256 during operator setup:

```bash
sudo apt-get update
sudo apt-get install --no-install-recommends -y libreoffice-writer libreoffice-impress libreoffice-calc
sha256sum /usr/lib/libreoffice/program/soffice.bin
```

`DefaultRuntimeRoots` selects Debian/Ubuntu paths for amd64 and arm64,
including LibreOffice, its libraries, and fonts, while other layouts need an
explicit list of runtime roots. Discover and retain the manifest during
operator setup, then build a policy from its files, symlinks, and identity,
passing the approved executable digest as `executableSHA256`:

```go
package renderer

import "go.kenn.io/docbank/document/renderpdf"

func newOfficeRenderPolicy(executableSHA256 string) (renderpdf.Policy, error) {
    manifest, err := renderpdf.DiscoverRuntime(renderpdf.DefaultRuntimeRoots())
    if err != nil {
        return renderpdf.Policy{}, err
    }
    return renderpdf.NewPolicy(renderpdf.Renderer{
        Executable:       "/usr/lib/libreoffice/program/soffice.bin",
        ExecutableSHA256: executableSHA256,
        Runtime:          manifest.Files,
        RuntimeSymlinks:  manifest.Symlinks,
        RuntimeIdentity:  manifest.Identity,
    }, renderpdf.DefaultLimits())
}
```

Keep the policy for subsequent conversions. Changed runtime bytes or a changed
executable fail verification. After a LibreOffice, library, or font update,
approve the updated installation and rebuild the policy. Do not rediscover the
runtime or replace the approved digest for each input document.

### Convert a source

```go
converted, err := renderpdf.Convert(ctx, source, "docx", policy)
if err != nil {
    return err
}
receipt := converted.Receipt()
pdfSource, err := converted.Source()
if err != nil {
    return err
}
// Keep receipt with pdfSource before a later caller authorizes upload.
```

The DOCX, DOC, ODT, and RTF profiles emit FODT, PPT emits FODP, and XLS, ODS,
and XLSX emit FODS. Normalization strips unsafe external and active constructs
where LibreOffice can remove them, and the normalized scan rejects any such
constructs that survive. It rejects every `xml:base` attribute, so local
fragment links cannot inherit an external base URI. Local formulas, internal
fragment links, and embedded raster images remain valid. The scanner rejects
ordinary hyperlinks to external sites or relative files if they survive
normalization. It also rejects embedded OLE objects, plugins, applets, and
nested documents. The scanner admits two LibreOffice metadata forms: an empty Basic
script container whose library list is empty or contains only the Standard
embedded-library marker, and presentation layout placeholders for all ODF
object types. Local spreadsheet filter and sort ranges are also admitted. The
scanner still rejects script code, event handlers, external or relative links,
DDE, external formulas, database sources, charts, OLE, plugins, embedded
documents, and other active objects. The render policy limits normalized bytes,
PDF bytes, work bytes, XML depth and elements, and generated PDF pages.
`MaxPages` sets the limit for generated PDF pages.

Each XML character-data token, including base64 image data, is limited to
1 MiB. A document within the overall byte limit can still exceed this limit
when it contains a large embedded image.

The receipt records both source and normalized identities, the PDF hash, the
page count, the policy, and the runtime identities. `Result.Source` is a fresh
`application/pdf` source, and the receipt does not authorize upload. The
Mistral adapter reuses this receipt for its configured office route, which
remains Go-only. The daemon runs the PDF rendition route through a configured
[Mistral OCR profile](configuration.md#document-ocr-with-mistral), while native
office authority remains separate when the render policy is absent.

## Package boundary

Dependencies run in one direction:

```text
application storage and workers
  -> document/mistral (optional provider transport)
  -> document (provider-neutral normalization)

application storage and workers
  -> document/voyage (optional provider transport)
  -> document/media (provider-neutral detection and eligibility)

application storage and workers
  -> document/embedding (provider-neutral text planning and retrieval contracts)
  -> document (provider-neutral normalization)
```

No public package imports Docbank's vault, database, daemon, or queue. Local
extractors can use `document` without depending on Mistral or any network
transport. You can use `document/embedding` without a database or provider
client, and you can use `document/media` alone to record image dimensions at
ingest without any provider.
