---
last_edited: 2026-10-05
title: Verify transfer packages
description: Check a Msgvault transfer package locally before importing it.
---

# Verify transfer packages

You can verify a portable Msgvault transfer package locally without opening a
vault or starting either application's daemon:

```bash
docbank transfer verify ./msgvault-transfer --json
docbank transfer verify ./msgvault-transfer.zip
```

The verifier reads only the path you name and checks the package structure,
canonical records, declared and observed counts, references, size limits,
SHA-256 inventory, and content-addressed blob names. A valid report confirms
internal package integrity, but it does not authenticate the producer or prove
that an archive ID is registered in a particular vault.

When verification produces an invalid report, the command exits with status 6.
Tool failures, such as an unavailable temporary directory or failed cleanup,
exit with status 1 and report the cause instead of declaring the package
invalid. To get the complete machine-readable report with its authority,
bounds, counts, format limitations, and bounded findings, pass `--json`.

If a package is valid and partial, the output labels it `valid partial` and
includes its continuation token. In JSON reports, Docbank sets `partial: true`
and returns that token in `next_cursor`.

Size limits apply in UTF-8 bytes, not character counts: each record can be up
to 1 MiB, date timezone names up to 200 bytes, raw date text up to 4 KiB, and
combined diagnostic text up to 4 KiB.

## Verify an older Msgvault export

The older `msgvault-message-export/1` JSONL format does not carry an archive
identity, producer sequence, snapshot, creation time, attachment bytes, raw
messages, person UIDs, or complete history. Before verifying one of these
files, bind it to the archive ID that owns it:

```bash
docbank transfer verify ./messages.jsonl \
  --archive-id mva_example_01 \
  --json
```

Local verification checks the syntax of the archive ID, but it cannot check
whether that ID is registered in a vault. Import must separately confirm that
the same ID is registered. If the archive ID is missing or malformed, the
command treats it as a usage error and exits with status 2.

While building a temporary normalized package, Docbank keeps the legacy input
bytes and their SHA-256 unchanged. Reports label the result
`package_authority: "legacy_compatibility"` rather than a native
`msgvault-transfer/1` producer export, and the adapter leaves export sequence,
snapshot, and creation authority absent. Ordering timestamps preserve the
original offset, fractional width (including comma fractions), and raw
spelling, and the adapter does not assert a sent or received date kind. In JSON
output, the `raw`, `attachment_bytes`, `people`, and `history` capabilities
appear as unavailable with the reasons `format_v1_no_raw`,
`format_v1_no_attachments`, `format_v1_no_person_uid`, and
`format_v1_no_history`. These format limitations do not claim producer coverage
or attachment counts.

Because the legacy format carries no acquisition route, legacy source routes
stay empty. For example, a SyncTech export does not establish whether its
source was local or on Drive. Native transfer packages must supply a known
route.

The verifier accepts only known combinations of the legacy message type and
conversation type. For example, SMS in a direct chat becomes a chat message, a
calendar event in a calendar conversation remains a calendar event, and a
meeting transcript in a meeting remains a transcript. Empty message types and
ambiguous combinations fail instead of converting to email.

Docbank bounds the temporary normalization files and the retained input copy,
and removes them when verification finishes, including after a failure.
