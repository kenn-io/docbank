---
last_edited: 2026-08-28
title: Document processing configuration
description: Configure executable processing profiles while keeping credentials outside portable policy.
---

# Document processing configuration

Document-processing configuration lives in `$DOCBANK_HOME/config.toml` and is
read when the daemon starts. Provider endpoints, secret values,
environment-variable mappings, filesystem paths, and transport-only runtime
controls stay deployment-local; they do not enter portable metadata or backups.
The non-secret `credential:<name>` reference and the selected document,
response, unit, batch, and input limits are assembled into the canonical
`ProcessingProfileV1`, so they are retained with the profile and derivative
records. Restart the daemon after changing configuration.

The profile graph has four named layers:

- `[rendition_profiles.<name>]` binds a rendition adapter, its descriptor,
  bounded document/response/unit limits, requested artifact roles, disclosure
  settings, and trust boundary. The daemon's built-in adapter is
  `docbank-plaintext-rendition/v1`; it must agree with the configured descriptor
  and local-process trust boundary.
- `[embedding_profiles.<name>]` binds an embedding descriptor, input kind,
  optional rendition-chunk tokenizer and limits, and optionally a pinned hosted
  runtime. Its trust boundary, descriptor, disclosure fingerprint, and model
  input contract become part of the portable profile identity.
- `[retrieval_profiles.<name>]` supplies finite lexical and vector candidate
  limits.
- `[processing_profiles.<name>]` selects one rendition profile, zero or more
  embedding profiles, one retrieval profile, immutable normalization and
  retention fingerprints, and whether sanitized Markdown, provider Markdown,
  or typed artifacts are retained.

An empty processing profile is rejected. A profile without a rendition cannot
retain rendition Markdown. Every reference must name an existing layer, and
duplicate embedding bindings are rejected at daemon startup. Run `docbank
processing profiles` after restart to see the profiles that are actually
executable; a configured name is not usable until every selected adapter,
descriptor, and required tokenizer agrees with its portable contract.

## Credentials and hosted runtimes

`[credential_bindings.<name>]` names one environment variable. It holds only
the environment-variable name in `config.toml`; the secret value stays in that
environment and is resolved only by the selected provider adapter. Do not put
API keys in a processing profile, fingerprint, plan, provider receipt, backup,
or source-controlled configuration.

An embedding runtime may set its endpoint, model revision, deployment epoch,
capability manifest, request/retry/time limits, allowed CIDRs, SPKI pins, and
proxy policy in the embedding profile. These are deployment controls. The
daemon validates them before it creates a hosted provider and refuses redirects
and unexpected provider behavior according to the adapter contract.

The embedded Go API follows the same boundary: `ProcessingOptions` receives
provider values and their secret handling directly, while
`document.ProcessingProfileV1` remains immutable non-secret policy. A caller
must supply the matching providers and tokenizers for any profile it exposes.

## Retention is configuration, not a cleanup promise

`retain_sanitized_markdown`, `retain_provider_markdown`, and
`retain_typed_artifacts` are profile retention choices. They appear in the
reviewed plan and participate in its fingerprint. Changing them produces a
different plan; it does not retroactively erase existing derivative records or
backup snapshots. Use the previewed derivative-purge workflow for live data and
apply the backup repository's own expiry or deletion process to retained
snapshots.

See the general [Configuration](../configuration.md) reference for daemon and
vault settings, and [Document processing](document-processing.md) for the
preview-and-run workflow.
