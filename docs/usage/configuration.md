---
last_edited: 2026-10-05
title: Document processing configuration
description: Configure processing profiles the daemon can run, with credentials kept outside the portable policy.
---

# Document processing configuration

You configure document processing in `$DOCBANK_HOME/config.toml`, and the daemon
reads the file when it starts. Restart the daemon after you change the
configuration.

Provider endpoints, secret values, environment-variable mappings, filesystem
paths, and transport-only runtime controls stay local to your deployment. They
do not enter portable metadata or backups. The non-secret `credential:<name>`
reference and the selected document, response, unit, batch, and input limits
become part of the canonical `ProcessingProfileV1`, so Docbank keeps them with
the profile and derivative records.

A processing setup connects four named layers:

- `[rendition_profiles.<name>]` binds a rendition adapter, its descriptor,
  document, response, and unit limits, requested artifact roles, disclosure
  settings, and a trust boundary. The daemon's local adapters are
  `docbank-plaintext-rendition/v1`, `docbank-epub-rendition/v1`, and
  `docbank-pdftext-rendition/v1`. Each must agree with the configured descriptor
  and `local_process` trust boundary.
- `[embedding_profiles.<name>]` binds an embedding descriptor, input kind,
  optional rendition-chunk tokenizer and limits, and an optional pinned hosted
  runtime. Its trust boundary, descriptor, disclosure fingerprint, and model
  input contract become part of the portable profile identity.
- `[retrieval_profiles.<name>]` sets finite lexical and vector candidate
  limits.
- `[processing_profiles.<name>]` selects an optional rendition profile, zero or
  more embedding profiles, one retrieval profile, normalization and retention
  fingerprints, and whether Docbank retains sanitized Markdown, provider
  Markdown, or typed artifacts.

At startup, the daemon rejects empty processing profiles, duplicate embedding
bindings, and references to layers that do not exist. A profile without a
rendition cannot retain rendition Markdown. After you restart the daemon, run
`docbank processing profiles` to see which profiles can run. A configured name
is not usable until every selected adapter, descriptor, and required tokenizer
agrees with its portable contract.

## Local EPUB rendition

To extract EPUB files locally, use `docbank-epub-rendition/v1` with descriptor
ID `epub.in-process-v1`. Set `max_document_bytes` from 1 through 524288000 and
`max_units` from 1 through 1000000. Construct the descriptor with those same
limits using `epub.New`, then copy its fingerprint into
`descriptor_fingerprint`. If you change either limit, you need a new descriptor.
Filename disclosure follows `disclose_filename`. For supported packages and
virtual-unit counting, see
[local EPUB extraction](../document-understanding.md#extract-epub-locally).

## Local PDF text rendition

To read PDF text locally, use `docbank-pdftext-rendition/v1` with descriptor ID
`pdftext.in-process-v1` and trust boundary `local_process`. Set
`max_document_bytes` from 1 through 524288000 and `max_units`, the page limit,
from 1 through 100000. Construct
`pdftext.New(pdftext.Profile{MaxDocumentBytes: 16 << 20, MaxPages: 1000})`
from `go.kenn.io/docbank/document/pdftext` with the same limits, then copy
`Descriptor().Fingerprint` into `descriptor_fingerprint`. Changing either limit
changes the descriptor. Request the `structured_evidence` artifact role and
select this rendition from a processing profile, as you do for EPUB.

The Linux qualification processes a 500-page, 31 MiB scan in about a minute.
Daemon authorizations expire after ten minutes, so a job can run out of time
below the configured page limit.

This provider reads the existing text layer. Each page with zero native
characters records a `no_text_layer` omission, and pages whose extracted text is
empty or contains only whitespace and control characters record a
`no_readable_text` omission. An entirely image-only PDF completes with an empty
Markdown body and retained page omissions. The provider refuses encrypted PDFs.
For admission and execution limits, see
[local PDF text extraction](../document-understanding.md#extract-pdf-text-locally).

## Credentials and hosted runtimes

For text embedding services, prefer Kit's
`[embedding_profiles.<name>.embedder]` settings. They support typed API-key
references to an environment variable or a private file. Existing flat embedding
settings and named environment credentials remain supported as legacy input. For
the fields, conversion rules, and adapter limits, see
[text-service configuration](../configuration.md#text-service-configuration).

Each `[credential_bindings.<name>]` entry names one environment variable.
`config.toml` holds only that environment-variable name. The secret value stays
in your environment, and only the selected provider adapter resolves it. Do not
put API keys in a processing profile, fingerprint, plan, provider receipt,
backup, or source-controlled configuration.

An embedding profile can set deployment controls for its runtime, including its
endpoint, model revision, deployment epoch, capability manifest, request size
limits, timeouts, allowed CIDRs, SPKI pins, and proxy policy. The daemon
validates these controls before creating a hosted provider, and it refuses
redirects and unexpected provider behavior according to the adapter contract.

You can use the same deployment-local boundary in a rendition profile for
supplied audio transcription. Set `adapter_contract` to
`docbank-docling-asr/v1`, bind `credential:<name>`, and add a `runtime` block
with the Docling origin, request and polling bounds, allowed CIDRs, proxy
mode, and transport timeouts. The qualified adapter accepts original WAV and
MP3 input and publishes generated `media-transcript/v1` evidence. Its plan
discloses the destination before consent, and provider responses do not become
portable policy. Set the rendition's `max_transcript_chars` independently of
each processing profile's `max_document_chars`. If no processing profile selects
a runtime, that runtime stays staged. For disclosure-fingerprint calculation and
the restriction on sharing a descriptor across deployments, see the
[audio configuration reference](../configuration.md#supplied-audio-transcription).

To convert documents with an external service, configure Docling with
`adapter_contract = "docbank-docling-document/v1"` or Mistral OCR with
`adapter_contract = "docbank-mistral-ocr/v1"`. Mistral accepts PDFs and requires
a capability manifest, the EU endpoint, and a request timeout of at most ten
minutes. After a model re-pin, re-probe and recompute the descriptor and
disclosure fingerprints. See the
[Docling](../configuration.md#document-conversion-with-docling) and
[Mistral](../configuration.md#document-ocr-with-mistral) configuration
references for details.

The embedded Go API follows the same boundary. You pass provider values and
their secret handling directly through `ProcessingOptions`, while
`document.ProcessingProfileV1` remains immutable non-secret policy. Any caller
must supply the matching providers and tokenizers for the profiles it exposes.

## Retention is configuration, not a cleanup promise

`retain_sanitized_markdown`, `retain_provider_markdown`, and
`retain_typed_artifacts` control what a profile retains. They appear in the
reviewed plan and participate in its fingerprint. Changing them produces a
different plan, and it does not erase existing derivative records or backup
snapshots. To remove live derivative data, use the previewed derivative-purge
workflow. To remove retained snapshots, apply your backup repository's own
expiry or deletion process.

For daemon and vault settings, see the general
[Configuration](../configuration.md) reference. For the preview-and-run
workflow, see [Document processing](document-processing.md).
