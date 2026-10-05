---
last_edited: 2026-10-05
title: Document processing configuration
description: Configure executable processing profiles while keeping credentials outside portable policy.
---

# Document processing configuration

Document-processing configuration lives in `$DOCBANK_HOME/config.toml` and is
read when the daemon starts. Provider endpoints, secret values,
environment-variable mappings, filesystem paths, and transport-only runtime
controls stay local to the deployment. They do not enter portable metadata or
backups. The non-secret `credential:<name>` reference and the selected
document, response, unit, batch, and input limits become part of the canonical
`ProcessingProfileV1`, so Docbank keeps them with the profile and derivative
records. Restart the daemon after changing configuration.

The profile graph has four named layers:

- `[rendition_profiles.<name>]` binds a rendition adapter, its descriptor,
  document/response/unit limits, requested artifact roles, disclosure settings,
  and trust boundary. The daemon's local adapters are
  `docbank-plaintext-rendition/v1` and `docbank-epub-rendition/v1`. Each must
  agree with the configured descriptor and `local_process` trust boundary.
- `[embedding_profiles.<name>]` binds an embedding descriptor, input kind,
  optional rendition-chunk tokenizer and limits, and optionally a pinned hosted
  runtime. Its trust boundary, descriptor, disclosure fingerprint, and model
  input contract become part of the portable profile identity.
- `[retrieval_profiles.<name>]` supplies finite lexical and vector candidate
  limits.
- `[processing_profiles.<name>]` selects an optional rendition profile, zero or
  more embedding profiles, one retrieval profile, normalization and retention
  fingerprints, and whether sanitized Markdown, provider Markdown, or typed
  artifacts are retained.

An empty processing profile is rejected. A profile without a rendition cannot
retain rendition Markdown. Every reference must name an existing layer, and
duplicate embedding bindings are rejected at daemon startup. Run
`docbank processing profiles` after restart to see which profiles can run. A
configured name is not usable until every selected adapter, descriptor, and
required tokenizer agrees with its portable contract.

## Local EPUB rendition

For local EPUB extraction, use `docbank-epub-rendition/v1` with descriptor ID
`epub.in-process-v1`. Set `max_document_bytes` from 1 through 524288000 and
`max_units` from 1 through 1000000. Construct the descriptor with those same
limits through `epub.New`, then use its fingerprint as
`descriptor_fingerprint`. Changing either limit requires a new descriptor.
Filename disclosure follows `disclose_filename`. See
[local EPUB extraction](../document-understanding.md#extract-epub-locally) for
supported packages and virtual-unit counting.

## Credentials and hosted runtimes

For text embedding services, prefer Kit's
`[embedding_profiles.<name>.embedder]` settings. They support typed API-key
references to an environment variable or private file. Existing flat embedding
settings and named environment credentials remain supported as legacy input.
See
[text-service configuration](../configuration.md#text-service-configuration)
for the fields, conversion rules, and adapter limits.

`[credential_bindings.<name>]` names one environment variable. `config.toml`
holds only the environment-variable name. The secret value stays in that
environment, and only the selected provider adapter resolves it. Do not put API
keys in a processing profile, fingerprint, plan, provider receipt, backup, or
source-controlled configuration.

An embedding runtime may set its endpoint, model revision, deployment epoch,
capability manifest, request size limits, timeouts, allowed CIDRs, SPKI pins, and
proxy policy in the embedding profile. These are deployment controls. The
daemon validates them before it creates a hosted provider and refuses redirects
and unexpected provider behavior according to the adapter contract.

A rendition profile can use the same deployment-local boundary for supplied
audio transcription. Set `adapter_contract` to
`docbank-docling-asr/v1`, bind `credential:<name>`, and add a `runtime` block
with the Docling origin, request and polling bounds, allowed CIDRs, proxy
mode, and transport timeouts. The qualified adapter accepts original WAV and
MP3 input and publishes generated `media-transcript/v1` evidence. Its plan
discloses the destination before consent, and provider responses do not become
portable policy. Set the rendition's `max_transcript_chars` independently of
each processing profile's `max_document_chars`. A runtime that no processing
profile selects stays staged. See the [audio configuration reference](../configuration.md#supplied-audio-transcription)
for disclosure-fingerprint calculation and the restriction on sharing a descriptor
across deployments.

The embedded Go API follows the same boundary: `ProcessingOptions` receives
provider values and their secret handling directly, while
`document.ProcessingProfileV1` remains immutable non-secret policy. A caller
must supply the matching providers and tokenizers for any profile it exposes.

## Retention is configuration, not a cleanup promise

`retain_sanitized_markdown`, `retain_provider_markdown`, and
`retain_typed_artifacts` are profile retention choices. They appear in the
reviewed plan and participate in its fingerprint. Changing them produces a
different plan. It does not erase existing derivative records or backup
snapshots. Use the previewed derivative-purge workflow for live data and apply
the backup repository's own expiry or deletion process to retained snapshots.

See the general [Configuration](../configuration.md) reference for daemon and
vault settings, and [Document processing](document-processing.md) for the
preview-and-run workflow.
