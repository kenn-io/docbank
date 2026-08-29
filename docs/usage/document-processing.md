---
last_edited: 2026-08-28
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
consent is currently required, and the backup consequence. Re-run the preview
when the document, profile, or consent state changes.

Run only the reviewed plan:

```bash
docbank processing build /inbox/notes.txt \
  --profile <configured-profile> \
  --plan-fingerprint <fingerprint-from-plan> \
  --consent
```

`--plan-fingerprint` must be the exact lowercase SHA-256 fingerprint from the
preview. `--consent` records consent for the reviewed provider operations and
is required by the CLI; the worker checks that consent again before provider
egress and before publication. The daemon returns a durable processing job ID.
Use it to inspect aggregate state and any failure code:

```bash
docbank processing status <job-id>
```

For automation, `processing profiles`, `plan`, and `status` support `--json`.
`processing build --ndjson` writes one job event followed by one terminal status
event. The authenticated HTTP API provides the same preview, consent, run,
status, rendition, coverage, and source-fenced search contracts; see the
[HTTP API](../architecture/http-api.md).

## Processing flows and boundaries

Each profile makes its disclosure visible before execution. There are three
useful shapes:

- **Private rendition.** When an operator configures a rendition profile with
  the `docbank-plaintext-rendition/v1` adapter and selects it from a processing
  profile, it runs in process, opens no network connection, accepts supported
  UTF-8 text-like media types up to 16 MiB, and produces one generic evidence
  unit. Its provenance is therefore reported as degraded rather than invented.
- **Hosted provider.** A configured embedding runtime, or an embedded caller's
  rendition or embedding provider, may cross a hosted-provider trust boundary.
  The reviewed plan identifies the provider and the exact input class it
  receives: the original file, a rendition chunk, an original-file embedding
  input, or query text. Docbank does not treat a hosted profile as private.
- **Direct embedding.** A profile can embed an original file without a
  rendition provider. That flow has no readable rendition or lexical body
  index; direct-file results can be relevant without a fabricated text excerpt.

All flows use the same immutable source identity, profile fingerprint,
authorization, bounded upload, and catalog publication path. An embedded Go
application supplies provider implementations through `ProcessingOptions`; it
does not gain a path around those checks.

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
operator consent. A changed plan fingerprint, expired consent, or revocation
stops new egress and prevents publication under that authorization.

Next: configure a local profile in [Document processing
configuration](configuration.md), search an authorized source set in
[Document processing search](search.md), and inspect the durable data model in
[Document derivatives](../architecture/document-derivatives.md).
