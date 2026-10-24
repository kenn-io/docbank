---
last_edited: 2026-10-16
title: Document processing
description: Preview, consent to, and run document processing. The original version is never changed.
---

# Document processing

Document processing derives searchable renditions (derived text such as
Markdown or a transcript) and, when a profile has embedding bindings, vector
sets from one exact document version. The original version remains
authoritative. A processing profile is a named, non-secret policy that fixes
the provider descriptors, trust boundaries, retention choices, and retrieval
limits that its work may use.

The daemon exposes only profiles it can execute:

```bash
docbank processing profiles
```

The default configuration has no processing profiles. Configure a processing
profile, the retrieval profile it references, and any rendition or embedding
profiles it references, then use a profile name listed by this command.

Preview the current version before sending a document or query to any provider:

```bash
docbank processing plan /inbox/notes.txt --profile <configured-profile>
```

The plan names the stable node and content-version IDs, profile fingerprint,
every provider flow and trust boundary, input classes disclosed to each flow,
retained derivative classes, estimated source bytes and provider calls, any
work beyond the selected source that consent starts, whether consent is
currently required, and the backup consequence. Each flow also names
the immediate and ultimate processor, endpoint, deployment, model and revision,
vector space where applicable, provider-visible metadata, and retained artifact
roles. These runtime disclosures are part of the plan fingerprint. Re-run the
preview when the source, profile, or provider destination changes. The consent
state shown in a plan is advisory and does not change the fingerprint.
Execution checks consent again.

Run only the reviewed plan:

```bash
docbank processing build /inbox/notes.txt \
  --profile <configured-profile> \
  --plan-fingerprint <fingerprint-from-plan> \
  --consent
```

`--plan-fingerprint` must be the lowercase SHA-256 fingerprint from the
preview. The CLI requires `--consent`. It grants ongoing permission for this
profile configuration to the vault's daemon operator across documents and
searches, with no expiry. The grant is not limited to this job. Workers check
consent again before provider egress and before publication. The daemon returns
a durable processing job ID before provider work finishes. That receipt
confirms acceptance only. The daemon's later status reports progress or
failure. Use the ID to inspect aggregate state and any failure code:

```bash
docbank processing status <job-id>
```

For supplied recordings, `media submit` stores the original bytes and caller
occurrence before processing starts. Review the ASR plan and grant its exact
disclosure, then use `media status` to track the attempt. `media retry` queues
a later attempt after a transient or credential failure. It still requires the
same consent and returns before the provider finishes. `coverage_state` keeps
the last successful transcript separate from the newest attempt.

For automation, `processing profiles`, `plan`, and `status` support `--json`.
`processing build --ndjson` writes the job event immediately, followed by one
terminal status or error event. If the stream ends early, keep the first job ID
and query its status. The authenticated HTTP API provides the same preview,
consent, run, status, rendition, and coverage contracts, plus search limited to
an authorized source set (a source fence). See the
[HTTP API](../architecture/http-api.md).

## Prepare a native PDF

Inspect the current PDF version before designing redaction decisions:

```bash
docbank redaction prepare /inbox/source.pdf
docbank redaction prepare id:42 --version <retained-version-id> --json
```

This requires the configured [Linux amd64 page runtime](../architecture/page-images.md).
The original must belong to a live file; an explicit retained historical version
is allowed. The command checks the node revision and exact source identity.
If the selection changed, inspect it again before retrying.

Human output shows source, evidence and map digests, plus each page's byte span,
accepted atom count and ordered/unordered gap counts. It does not print original
text. `--json` returns the complete `native-pdf-preparation/v1` envelope,
**including unredacted text**. Treat any saved JSON like the original document.
Agents can use the same JSON command or authenticated
`POST /api/v1/redaction/prepare`; no browser or MCP action is exposed.

Preparation returns coverage gaps for inspection. Every ordered gap must be
fully masked, and an unordered gap requires masking its whole page, before the
resolver can proceed. A zero-gap result does not approve redaction or
verify an output PDF. Keep the complete envelope with its evidence and frames;
the map alone is not an execution authorization. This command applies no masks
and creates no retained job or output PDF.

The entire PDF must fit within 64 MiB and 16 pages. Retained evidence is capped
at 8 MiB and the full result at 15 MiB. Work has a 60-second request deadline;
limits or failure never return a partial preparation. Prepare, resolve and render share one slot, with no queue. Busy or memory refusals exit 5: wait for active work to finish, or retry
when daemon memory use is lower. The renderer's 1 GiB RSS ceiling counts the
whole daemon, so even a small PDF can be refused on a busy process.

Malformed selections exit 2, unavailable documents exit 3, changed selections
exit 4, and failed integrity checks exit 6. Missing runtime, unsupported PDFs,
work limits, request cancellation and worker failures exit 1. Native text
inspection does not add OCR; the [font and coverage limitations](../architecture/page-images.md#remaining-refusals)
still apply.

## Describe photos

Configure a [vision profile](../configuration.md#photo-descriptions), review `processing plan` for a photo, and grant consent with `processing build`. Eligible photos then queue automatically, a few at a time. The provider receives the cached grid JPEG, at most 512 pixels on its longest edge and re-encoded without metadata; filenames and original bytes stay in the vault. Hidden, excluded, trashed, and superseded photos are skipped before the call and again at publication, and a hidden photo retries once it is eligible again.

The first request to a provider checks it with a synthetic image, and a failed check retries no sooner than a minute later. If it fails when you grant consent, the error says consent was saved. Caption, tags, and visible text arrive as one [suggestion](photos.md#suggested-metadata). An invalid caption or an oversized tag is dropped alone; a non-JSON reply publishes nothing. A dropped connection, timeout, or daemon restart during a description call retries it with backoff, since a description has no side effects.

Derivative purge and consent revocation leave pending suggestion text in place. Dismissing the suggestion removes it, and so does purging the photo version.

## Review and render a native PDF

Save the preparation, supply explicit masks, then inspect the review before rendering:

```bash
docbank redaction prepare /inbox/source.pdf --json > preparation.json
docbank redaction resolve preparation.json masks.json --json > review.json
docbank redaction render preparation.json masks.json review.json redacted.zip
```

The ZIP contains exactly `redacted.pdf`, `redacted.txt` and `receipt.json`.
Extract `redacted.pdf` to open it in a PDF viewer. The text file contains the
sanitized text, with a form feed after each page. The receipt binds the source,
review, recipe and file hashes. `--json` on render also reports the local
archive path, size, hash and publication state. `--overwrite` permits replacing
an existing output; otherwise an existing or concurrently created file is kept.
Output must be outside the Docbank data directory.

`masks.json` is an array. Each mask has a distinct canonical lowercase UUIDv4
`id` and a `selector`. For example, to remove the first six UTF-8 **bytes**:

```json
[
  {
    "id": "11111111-1111-4111-8111-111111111111",
    "selector": {
      "kind": "text",
      "map_sha256": "<map.sha256 from preparation.json>",
      "span": {"start": 0, "end": 6}
    }
  }
]
```

Spans must end on UTF-8 boundaries. Rectangle selectors use `boxes`, each with
`page`, `frame_sha256`, `x0`, `y0`, `x1`, `y1` in the map's integer page frame.
Page selectors use `pages`, a list of one-based page numbers. All selector kinds
carry `map_sha256`. Up to 256 masks, 1,024 boxes and 1,024 page references fit
within the 1 MiB mask limit.

Run resolve without `--json` to see effective masks, removed byte spans and
quoted retained text for each page. If a partial atom needs a larger mask,
resolve returns `selection_expansion_required` and writes the complete proposal
to stderr. Review its `expanded` selector, explicitly replace the selector in
your masks file, and resolve again. Nothing silently accepts the expansion.
Every gap anywhere in the document must be covered: ordered gaps need covering
masks; unordered gaps need a whole-page mask. `mapping_incomplete` produces no
review or output PDF.

Resolve and render inspect the selected original again. Tags, names, moves and
a newer current version do not invalidate a review of an unchanged retained
version. The selected version must still belong to the live file; trash or
pruning makes it unavailable. Revisions in the review and receipt are
informational. Keep the complete preparation envelope, masks and review;
a bare map is insufficient. These files contain unredacted evidence.

Render can hold the shared slot for up to ten minutes, including delivery.
Prepare and resolve have 60-second request deadlines. A busy message identifies
the active operation. Busy or memory refusals exit 5; wait for that operation or
lower daemon memory use. Changed evidence or review exits 4 and requires a new
review. Invalid local inputs exit 2, unavailable versions exit 3, integrity
failures exit 6, and other refusals or I/O failures exit 1. No write is retried
automatically. After a reported save followed by an output or cleanup error,
inspect the named archive before retrying.

The daemon verifies the fresh PDF before creating the ZIP. The CLI verifies the
transferred archive, its inventory, file hashes, receipt and reviewed text
before publishing one file. Client checks do not independently verify the
source pixels. No job, attachment or restart-recoverable output is retained.
A killed daemon may leave private files in its spool; see the
[staging bounds](../architecture/page-images.md#native-pdf-mask-execution).
Agents can use these commands or the authenticated `POST /api/v1/redaction/resolve`
and `/api/v1/redaction/render` routes. Browser sessions and MCP tools do not
expose these operations.

## Convert documents with Docling

Configure a [Docling document rendition profile](../configuration.md#document-conversion-with-docling)
with the `docbank-docling-document/v1` adapter, then select it from a processing
profile. The daemon can convert locally admitted PDFs, PPTX/XLSX files, plain
text/Markdown, and PNG/JPEG images through your Docling Serve deployment.
DOCX remains blocked by local inspection; this profile does not admit HTML,
TIFF, legacy Office, audio, or video. When local inspection refuses a file,
its plan shows the reason on an `ineligible:` line and `processing build`
fails with `rendition_source_ineligible`. A locked PDF instead ends with
`pdf_password_required` when no configured password opens it or no password
file is configured.

Import the original, inspect its plan, and consent to the exact destination:

```bash
docbank add /path/to/report.pdf --dest /inbox
docbank processing plan /inbox/report.pdf --profile <configured-profile>
docbank processing build /inbox/report.pdf \
  --profile <configured-profile> \
  --plan-fingerprint <fingerprint-from-plan> \
  --consent
```

The plan discloses the endpoint, deployment, trust boundary, filename policy,
and retained artifacts. Import and preview send no document to Docling. Track
the returned job with `processing status <job-id>`. When it completes, the
retained Markdown is available through the rendition read API and lexical
search. Original bytes stay unchanged. PDF output can retain exact page
locations; other supported formats report degraded provenance.

## Convert PDFs with Mistral OCR

Configure a [Mistral OCR rendition profile](../configuration.md#document-ocr-with-mistral)
with an authenticated capability manifest, then select it from a processing
profile. `docbank processing profiles` lists the executable profile. Use the
same `processing plan` and consented `processing build` flow as Docling to
retain searchable Markdown from PDFs. The plan discloses Mistral's EU endpoint
and the pinned model before upload. Local refusals end builds with
`rendition_source_ineligible`, except locked PDFs, which end with
`pdf_password_required` when no configured password opens them or no password
file is configured.

## Password-protected PDFs

Docling and Mistral processing can open encrypted PDFs using `[media].pdf_passwords_file` from [configuration](../configuration.md). Put one password per line in that owner-only file. LF and CRLF line endings work; blank lines are ignored, and spaces belong to the password. The file must be UTF-8 without a byte-order mark and may contain up to 16 KiB and 128 passwords.

Docbank decrypts a copy in memory before sending it to the provider. The archived original stays encrypted and unchanged. When no password opens the PDF, or no password file is configured, the browser drawer shows "pdf password required" as the reason and disables Run. CLI and API builds create a job that ends `failed` with `pdf_password_required`. Read it with `docbank processing status <job-id>`.

Add a working password to the file, then build again. The daemon reads the file for each locked-PDF inspection, so editing passwords needs no restart. Restart once when first configuring the file path. If the file can't be read, planning or building a locked PDF reports the error, and queued jobs retry until the file is fixed. Plain PDFs and audio keep processing.

### Design choices

The pinned pdfcpu v0.15.0 password preparation rejects spaces, non-ASCII punctuation such as curly quotes or `£`, and emoji in AES-256 passwords. It also Unicode-normalizes decomposed accents and fullwidth letters. These files report `pdf_password_required` even with the correct password. Older RC4 and AES-128 ciphers support spaces and PDFDocEncoding characters such as `£`, because Docbank also tries each password converted from UTF-8 to PDFDocEncoding. With a password file configured, Docbank tries an empty password first, even when the file is empty or contains only blank lines, so owner-only PDFs open. Without a configured file, Docbank tries no passwords, and every encrypted PDF reports `pdf_password_required`.

## Find similar documents

Choose a file's **Find similar** row action in the web app. The processing
drawer searches the file versions loaded in the current view. Select a profile
with embeddings. Results use the selected file's stored embeddings locally. An
unavailable report names the missing binding. Run processing to build it, then
retry. Views above 4,096 file versions must be narrowed first.

In the TUI, press uppercase `S` on a file to search the current listing.
Lowercase `s` still sorts. The selected file is excluded, while another file
with identical bytes can appear. Results group identical content and show the
number of other eligible copies. Coverage describes this bounded scope.
The [CLI reference](../cli-reference.md#find-similar-files) describes the
equivalent `--similar-to` command.


![Similar documents found locally from stored embeddings in the TUI](https://docbank.ai/assets/generated/tui-similar.png)

## Processing flows and boundaries

Each profile makes its disclosure visible before execution. There are three
useful shapes:

- **Private rendition.** When an operator configures a rendition profile with
  the `docbank-plaintext-rendition/v1` adapter and selects it from a processing
  profile, it runs in process, opens no network connection, accepts supported
  UTF-8 text-like media types up to 16 MiB, and produces one generic evidence
  unit. Docbank therefore reports its provenance as degraded. The
  `docbank-epub-rendition/v1` adapter also runs locally. It extracts ordered
  XHTML spine text from supported EPUB packages and keeps source locations. The
  plan reports `local_process` and the `in-process` destination before consent.
  See
  [local EPUB extraction](../document-understanding.md#extract-epub-locally)
  for limits and counting rules.
- **Hosted provider.** A configured Mistral OCR or embedding runtime, or an embedded caller's
  rendition or embedding provider, may cross a hosted-provider trust boundary.
  The reviewed plan identifies the provider and the input class it receives:
  the original file, a rendition chunk, an original-file embedding input, or
  query text. Docbank does not treat a hosted profile as private. A configured
  Docling document runtime can receive an original file and retain searchable
  Markdown. A configured Docling ASR profile can receive an original supplied
  WAV or MP3 file and publish generated transcript evidence. The daemon
  reports the provider destination before the operator grants consent.
- **Direct embedding.** A profile can embed an original file without a
  rendition provider. That flow has no readable rendition or lexical body
  index, so a relevant direct-file result comes without a text excerpt.

All flows use the same source identity, profile fingerprint, authorization,
bounded upload, and catalog publication path. An embedded Go application
supplies provider implementations through `ProcessingOptions`, but it cannot
bypass those checks. Embedded network providers need an endpoint disclosure
when the vault opens. See
[Configure document processing in Go](../embedding.md#configure-document-processing).

## Check a private processing deployment

From a repository checkout, run
[`scripts/test-private-processing.sh`](https://github.com/kenn-io/docbank/blob/main/scripts/test-private-processing.sh)
against your Docling Serve and OpenAI-compatible embedding endpoints. The
runner creates two synthetic text documents in a disposable embedded vault,
stores their renditions and vectors, and checks lexical, semantic, and hybrid
search limited to each document's source fence. It does not read an existing
vault or personal corpus.

The runner requires Bash, `setsid`, the repository's Go toolchain, and cached
Go modules. Populate the module cache with `go mod download` before running it,
because the runner builds with `GOPROXY=off`. The embedding service must accept
the Nomic document/query input format. Set the following variables to match
your services. The loopback URLs and model values shown are examples:

```bash
export DOCBANK_PRIVATE_PROCESSING_DOCLING_URL='http://127.0.0.1:5001'
export DOCBANK_PRIVATE_PROCESSING_DOCLING_ALLOWED_CIDRS='127.0.0.1/32'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_URL='http://127.0.0.1:8080'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_ALLOWED_CIDRS='127.0.0.1/32'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_MODEL='nomic-embed-text'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_REVISION='local-v1'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_DIMENSIONS='768'
read -rsp 'Embedding API key: ' DOCBANK_PRIVATE_PROCESSING_EMBEDDING_API_KEY
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_API_KEY
scripts/test-private-processing.sh
```

Set `DOCBANK_PRIVATE_PROCESSING_DOCLING_API_KEY` too if Docling requires it.
Endpoint URLs must be HTTP(S) origins with a literal private or loopback IP,
without credentials, query parameters, fragments, or a path beyond `/`. Each
endpoint must fall inside its provider's allowed CIDRs. Separate multiple
ranges with commas or whitespace. Hostnames and public address ranges are
rejected.

The output contains aggregate provider-request and connected-address counts,
plus the rendition and search results. It checks the connections made by
Docbank's provider transports. It does **not** attest to onward traffic from
those endpoints: `endpoint_onward_egress=not_attested` means the operator must
check that separately. The runner stops its processes and removes its temporary
vault, binary, logs, and build cache on exit, including after failure.

## Inspect evidence before reporting

Use `stat --json` to obtain a file's `current_version_id`, then inspect that
version without starting processing:

```bash
docbank stat /inbox/notes.txt --json
docbank processing coverage <version-uuid> --profile <name> --json
docbank rendition window /inbox/notes.txt --version <version-uuid> --profile <name>
```

Both commands require a profile listed by `docbank processing profiles --json`.
A configured profile whose runtime is unavailable is not enough, even if its
renditions are retained. Reports can still use retained evidence under such a
configured profile. A profile error here does not mean that evidence is missing.

Coverage reports rendition and embedding counters for the one requested
version. JSON contains `content_version_id` and the complete `coverage` object.
Human output names the profile, fingerprint, state, and each class's counters.
Unavailable, partial, rebuilding, and stale observations exit successfully.
Unknown, replaced, or trashed versions count as stale for a required rendition.
An unneeded class marked `not_required` does not establish source availability.
`previous_generation_serving` overlaps rebuilding. Do not add it to the total.

These processing counters do not inspect term matches, dates, or attachment
families. They do not promise that a [strict report](search-exports.md) will
succeed.

### Read and continue a text window

`rendition window` requires the selected version to remain current and live.
It discovers the active rendition for the named profile and returns at most
8,000 characters by default. Use `--max-chars` for 1–16,000 characters and
`--json` for the text, full rendition identity, offsets, and EOF flag.
The text is stored Markdown, including its metadata envelope and escaped
punctuation. It is not rendered or unescaped.

The human result prints a continuation command. Follow it to keep reading
the same attachment:

```bash
docbank rendition window id:<node-id> --version <version-uuid> --profile <name> \
  --attachment <attachment-id> --offset <next-offset> --max-chars 8000
```

Offsets count Unicode scalar values, not bytes or visible grapheme clusters.
Offsets range from 0 to 2,147,483,647. A nonzero offset requires an attachment
ID, and a pinned first window can also use `--attachment` at offset zero. The
command refuses a replaced rendition instead of continuing into its
replacement. An offset exactly at EOF succeeds with empty text. An offset past
EOF fails. No command automatically fetches the remaining windows.

Discovery uses the document catalog. It cannot find a file deeper than 256 path
components, with a path longer than 16 KiB in UTF-8 bytes, or with a filename
longer than 255 Unicode scalar values. The filename cap does not apply to
ancestor directories. The CLI names the exceeded limit. Refreshing the
selection does not solve it. If you know the active attachment, pass
`--attachment` to skip discovery. The version and profile checks still apply.

Each unpinned discovery scans the traversable live vault tree. Pinned calls
skip that scan. The text reader still reads from the beginning to reach the
requested offset, so a bounded result does not imply a constant-time seek.
A window identifies the full artifact's checksum but does not independently
verify the entire file. Use `rendition get` below for that verification.

## Read a retained rendition

When the profile retains sanitized Markdown, its active attachment is an
authenticated Docbank resource. Read it by attachment ID:

```bash
docbank rendition get <attachment-id>
```

The command verifies the complete stream before accepting it and writes the
self-describing Markdown to standard output. A rendition is bounded to 64 MiB.
It is not a replacement original, and it is not a general-purpose provider
export. The web application and TUI show the same retained sanitized Markdown
and its identity. They do not enable raw provider Markdown as ordinary document
content.

## Retention, consent, and backup

The profile controls whether Docbank retains sanitized Markdown, provider
Markdown, and typed artifacts. A rendition profile always retains normalized
evidence, and embedding work retains vector sets. The preview lists the classes
for the selected profile, so you do not have to infer retention from a provider
name.

Retained derivatives are catalog-authorized content. They follow the source
version's lifecycle, are included in catalog-authorized backups, and can keep
related source content reachable while the catalog still authorizes them.
Physical lexical and ANN indexes are rebuildable projections, not backup
authority. Restoring a backup does not call a rendition or embedding provider.
New provider work needs a new plan and consent.

Use the derivative-purge API only after its own preview. A live purge can
remove selected live derivative heads, attachments, builds, artifacts,
segments, and vector sets, but it cannot rewrite immutable backup snapshots.
Removing Markdown alone does not establish that other retained derivative
classes or backup copies are gone.

Consent is scoped to the current principal, processing scope, profile, provider
disclosure, input classes, and retained classes. It can be active, required,
expired, or revoked. API clients may grant a future expiry or revoke current
operator consent. The web app and API can revoke all processing consent for
this operator across profiles. A changed plan fingerprint, expired consent, or
revocation stops new egress and prevents publication under that authorization.

Next: configure a local profile in
[Document processing configuration](configuration.md), search an authorized
source set in [Automatic and processing search](search.md), and inspect the data
model in [Document derivatives](../architecture/document-derivatives.md).
