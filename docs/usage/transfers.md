---
last_edited: 2026-09-14
title: Verify Transfer Packages
description: Check a Msgvault transfer package locally before importing it.
---

# Verify Transfer Packages

Docbank can verify a portable Msgvault transfer package without opening a
vault or starting either application's daemon:

```bash
docbank transfer verify ./msgvault-transfer --json
docbank transfer verify ./msgvault-transfer.zip
```

The verifier reads only the path you name. It checks the package structure,
canonical records, declared and observed counts, references, size limits,
SHA-256 inventory, and content-addressed blob names. A valid report establishes
internal package integrity. It does not authenticate the producer or prove
that an archive ID is registered in a particular vault.

The command exits with status 6 when verification produces an invalid report.
Tool failures, such as an unavailable temporary directory or failed cleanup,
exit with status 1 and report the cause instead of declaring the package invalid.
Use `--json` for the complete machine-readable report, including its authority,
bounds, counts, format limitations, and bounded findings.

A valid partial package is labeled `valid partial` and includes its continuation
token. JSON reports set `partial: true` and return that token in `next_cursor`.

Each record is limited to 1 MiB. Date timezone names are limited to 200 bytes,
raw date text to 4 KiB, and combined diagnostic text to 4 KiB. These are UTF-8
byte limits, not character counts.

## Verify an older Msgvault export

The older `msgvault-message-export/1` JSONL format has no archive identity,
producer sequence, snapshot, creation time, attachment bytes, raw messages,
person UIDs, or complete history. Bind one of these files to the archive ID
that will own it before verification:

```bash
docbank transfer verify ./messages.jsonl \
  --archive-id mva_example_01 \
  --json
```

Local verification checks the archive ID's syntax but cannot check vault
registration. Import must separately confirm that the same ID is registered.
Missing or malformed archive IDs are usage errors and exit with status 2.

Docbank retains the exact legacy input bytes and SHA-256 while it builds a
temporary normalized package. Reports label this as
`package_authority: "legacy_compatibility"`; it is not a native
`msgvault-transfer/1` producer export. The adapter leaves export sequence,
snapshot, and creation authority absent. Its ordering timestamps keep the
original offset, fractional width (including comma fractions), and raw spelling,
with no sent or received date kind asserted. The JSON report exposes unavailable `raw`,
`attachment_bytes`, `people`, and `history` capabilities with the reasons
`format_v1_no_raw`, `format_v1_no_attachments`,
`format_v1_no_person_uid`, and `format_v1_no_history`. These format limitations
do not claim producer coverage or attachment counts.

Legacy source routes remain empty because the format carries no acquisition
route. For example, a SyncTech export does not establish whether its source was
local or on Drive. Native transfer packages must supply a known route.

Only known combinations of the legacy message type and conversation type are
accepted. For example, SMS in a direct chat becomes a chat message, a calendar
event in a calendar conversation remains a calendar event, and a meeting
transcript in a meeting remains a transcript. Empty message types and ambiguous
combinations fail instead of being converted to email.

Temporary normalization files and the retained input copy are bounded and
removed when verification finishes, including after a failure.
