---
last_edited: 2026-09-14
title: Document processing
description: Preview, consent to, and run configured document derivatives without losing source authority.
---

# Document processing

Document processing derives searchable renditions and, when a profile has
embedding bindings, vector sets from one exact immutable document version. The
original version remains the document authority. A processing profile is a
named, non-secret policy that fixes the provider descriptors, trust boundaries,
retention choices, and retrieval limits that its work may use.

The daemon exposes only profiles it can execute:

```bash
docbank processing profiles
```

The default configuration has no processing profiles. Configure a processing
profile, the retrieval profile it references, and any rendition or embedding
profiles it references, then use a profile name listed by this command.

Preview the exact current version before sending a document or query to any
provider:

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
preview when the source, profile, or provider destination changes. Consent state
is advisory and does not change the fingerprint; execution checks it again.

Run only the reviewed plan:

```bash
docbank processing build /inbox/notes.txt \
  --profile <configured-profile> \
  --plan-fingerprint <fingerprint-from-plan> \
  --consent
```

`--plan-fingerprint` must be the exact lowercase SHA-256 fingerprint from the
preview. The CLI requires `--consent`. It grants ongoing permission for this
profile configuration to the vault's daemon operator across documents and
searches, with no expiry. The grant is not limited to this job. Workers check
consent again before provider egress and before publication. The daemon
returns a durable processing job ID before provider work finishes. That receipt
confirms acceptance; the daemon's later status reports progress or failure.
Use the ID to inspect aggregate state and any failure code:

```bash
docbank processing status <job-id>
```

For supplied recordings, `media submit` retains the original bytes and caller
occurrence before processing starts. Review the ASR plan and grant its exact
disclosure, then use `media status` to track the attempt. `media retry` queues
a later attempt after a transient or credential failure; it still requires the
same consent and returns before the provider finishes. `coverage_state` keeps
the last successful transcript separate from the newest attempt.

For automation, `processing profiles`, `plan`, and `status` support `--json`.
`processing build --ndjson` writes the job event immediately, followed by one
terminal status or error event. If the stream ends early, retain the first job
ID and query its status. The authenticated HTTP API provides the same preview, consent, run,
status, rendition, coverage, and source-fenced search contracts; see the
[HTTP API](../architecture/http-api.md).

## Find similar documents

Choose a file's **Find similar** row action in the web app. The processing
drawer searches the file versions loaded in the current view. Select a profile
with embeddings; results use the selected file's stored embeddings locally.
An unavailable report names the missing binding. Run processing to build it,
then retry. Views above 4,096 file versions must be narrowed first.

In the TUI, press uppercase `S` on a file to search the current listing.
Lowercase `s` still sorts. The selected file is excluded, while another file
with identical bytes can appear. Results group identical content and show the
number of other eligible copies. Coverage describes this bounded scope.
The [CLI reference](../cli-reference.md#find-similar-files) describes the
equivalent `--similar-to` command.


## Processing flows and boundaries

Each profile makes its disclosure visible before execution. There are three
useful shapes:

- **Private rendition.** When an operator configures a rendition profile with
  the `docbank-plaintext-rendition/v1` adapter and selects it from a processing
  profile, it runs in process, opens no network connection, accepts supported
  UTF-8 text-like media types up to 16 MiB, and produces one generic evidence
  unit. Its provenance is therefore reported as degraded rather than invented.
  The `epub.in-process/v1` adapter also runs locally. It extracts ordered XHTML
  spine text from supported EPUB packages and retains source locations. Set
  `disclose_filename = true` for the verified daemon path. The plan reports
  `local_process` and the `in-process` destination before consent. See
  [local EPUB extraction](../document-understanding.md#extract-epub-locally)
  for limits and counting rules. Marker rejects EPUB before upload.
- **Hosted provider.** A configured embedding runtime, or an embedded caller's
  rendition or embedding provider, may cross a hosted-provider trust boundary.
  The reviewed plan identifies the provider and the exact input class it
  receives: the original file, a rendition chunk, an original-file embedding
  input, or query text. Docbank does not treat a hosted profile as private.
  A configured Docling ASR profile can receive an original supplied WAV or MP3
  file and publish generated transcript evidence. The daemon reports the
  provider destination before the operator grants consent.
- **Direct embedding.** A profile can embed an original file without a
  rendition provider. That flow has no readable rendition or lexical body
  index; direct-file results can be relevant without a fabricated text excerpt.

All flows use the same immutable source identity, profile fingerprint,
authorization, bounded upload, and catalog publication path. An embedded Go
application supplies provider implementations through `ProcessingOptions`; it
does not gain a path around those checks. Embedded network providers need an
explicit endpoint disclosure when the vault opens; see
[Configure document processing in Go](../embedding.md#configure-document-processing).

## Check a private processing deployment

From a repository checkout, run
[`scripts/test-private-processing.sh`](https://github.com/kenn-io/docbank/blob/main/scripts/test-private-processing.sh)
against your Docling Serve and OpenAI-compatible embedding endpoints. The runner
creates two synthetic text documents in a disposable embedded vault, retains
their renditions and vectors, and checks lexical, semantic, and hybrid search
with each document's exact source fence. It does not read an existing vault or
personal corpus.

The runner requires Bash, `setsid`, the repository's Go toolchain, and cached Go
modules. Populate the module cache with `go mod download` before running it;
the runner builds with `GOPROXY=off`. The embedding service must accept the Nomic
document/query input format. Set the following variables to match your services;
these loopback URLs and model values are examples:

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
endpoint must fall inside its provider's allowed CIDRs; multiple ranges may be
comma- or whitespace-separated. Hostnames and public address ranges are rejected.

The output contains aggregate provider-request and connected-address counts,
plus the rendition and search results. It checks the connections made by
Docbank's provider transports. It does **not** attest to onward traffic from
those endpoints: `endpoint_onward_egress=not_attested` means the operator must
check that separately. The runner stops its processes and removes its temporary
vault, binary, logs, and build cache on exit, including after failure.

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
and its identity; they do not enable raw provider Markdown as ordinary document
content.

## Retention, consent, and backup

The profile controls whether Docbank retains sanitized Markdown, provider
Markdown, and typed artifacts. A rendition profile always retains normalized
evidence; embedding work retains vector sets. The preview lists the classes
for the selected profile, so retention is not inferred from a provider name.

Retained derivatives are catalog-authorized content. They follow the source
version's lifecycle, are included in catalog-authorized backups, and can keep
related source content reachable while the catalog still authorizes them.
Physical lexical and ANN indexes are rebuildable projections, not backup
authority. Restoring a backup does not call a rendition or embedding provider;
new provider work needs a new plan and consent.

Use the derivative-purge API only after its own preview. A live purge can
remove selected live derivative heads, attachments, builds, artifacts,
segments, and vector sets, but it cannot rewrite immutable backup snapshots.
Removing Markdown alone does not establish that other retained derivative
classes or backup copies are gone.

Consent is scoped to the current principal, processing scope, profile, provider
disclosure, input classes, and retained classes. It can be active, required,
expired, or revoked. API clients may grant a future expiry or revoke current
operator consent. The web app and API can revoke all processing consent for
this operator across profiles. A changed plan fingerprint, expired consent, or revocation
stops new egress and prevents publication under that authorization.

Next: configure a local profile in [Document processing
configuration](configuration.md), search an authorized source set in
[Document processing search](search.md), and inspect the durable data model in
[Document derivatives](../architecture/document-derivatives.md).
