---
last_edited: 2026-10-09
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
retained derivative classes, estimated source bytes and provider calls, whether
consent is currently required, and the backup consequence. Each flow also names
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

## Convert documents with Docling

Configure a [Docling document rendition profile](../configuration.md#document-conversion-with-docling)
with the `docbank-docling-document/v1` adapter, then select it from a processing
profile. The daemon can convert locally admitted PDFs, PPTX/XLSX files, plain
text/Markdown, and PNG/JPEG images through your Docling Serve deployment.
DOCX remains blocked by local inspection; this profile does not admit HTML,
TIFF, legacy Office, audio, or video.

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
- **Hosted provider.** A configured embedding runtime, or an embedded caller's
  rendition or embedding provider, may cross a hosted-provider trust boundary.
  The reviewed plan identifies the provider and the input class it receives:
  the original file, a rendition chunk, an original-file embedding input, or
  query text. Docbank does not treat a hosted profile as private. A configured Docling document runtime can receive an original file and retain
  searchable Markdown. A configured
  Docling ASR profile can receive an original supplied WAV or MP3 file and
  publish generated transcript evidence. The daemon reports the provider
  destination before the operator grants consent.
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
source set in [Document processing search](search.md), and inspect the data
model in [Document derivatives](../architecture/document-derivatives.md).
