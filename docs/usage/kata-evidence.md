---
last_edited: 2026-10-17
title: Cite documents in Kata
description: Quote verified document passages into Kata issues and reopen their evidence with current source status.
---

# Cite documents in Kata

Use Docbank to quote a document passage into a new Kata issue or append it to
an existing issue. The issue keeps the quoted words and an exact reference.
You can read those references back and check whether the source has changed.
Files imported through any Docbank ingestion path can supply evidence.

## Connect Kata

Kata API schema 0.21.0 or newer is required. This is the `api_schema_version`
reported by `GET /api/v1/health`, separate from the Kata release number.
Kata v0.18.0 reports schema 0.21.0; v0.19.0 reports 0.28.0. Both meet the
requirement. Docbank checks it only when you use this integration.

Add this optional section to `$DOCBANK_HOME/config.toml`:

```toml
[integrations.kata]
enabled = true
endpoint = "http://127.0.0.1:7777"
api_key_env = "DOCBANK_KATA_TOKEN"
default_project = "docbank"
```

Supply `DOCBANK_KATA_TOKEN` to the Docbank daemon through your process or
secret manager. Alternatively, replace `api_key_env` with
`api_key_file = "secrets/kata-token"`. Relative paths resolve inside
`DOCBANK_HOME`. Choose exactly one credential source. Literal secrets in
configuration are rejected. Key files must be regular files of at most
4,096 bytes; credentials are read when an operation runs.

The endpoint accepts HTTPS or loopback HTTP. Remote plain HTTP, credentials
in the URL, query strings, fragments and endpoint path prefixes are rejected.
The integration is off by default. Only Kata operations validate this section;
an invalid field or missing credential does not prevent ordinary vault work.
Restart an existing daemon after changing its connection configuration.

## Quote original text

First inspect the document:

```bash
docbank stat /notes/meeting.txt --json
docbank cat /notes/meeting.txt
```

Create an issue from a passage:

```bash
docbank kata cite /notes/meeting.txt --start 0 --end 28 \
  --quote 'We agreed to review pricing.' \
  --title 'Review pricing' --brief 'Follow up on the meeting.' \
  --idempotency-key pricing-review-1
```

Offsets count Unicode code points, starting at zero. The start is included;
the end is excluded. A passage contains at most 1,000 code points. `--quote`
checks that the selected words still match. To bind an earlier version, pass
`--expected-version` with the content version from `stat --json` and keep that
value unchanged on retries. Original text must be UTF-8 text or JSON and at most
16 MiB. Binary documents use a retained text rendition instead.

Keep the same idempotency key when retrying the same create request. Reusing
the key with different input fails. Docbank records a durable create marker
in Kata in addition to Kata's own replay protection. Create retries are reconciled
before looking up the source, even if it has since changed or disappeared.

Append another passage:

```bash
docbank kata cite id:12 --start 28 --end 45 --issue docbank#abcd
```

Appends stay inside `default_project`. They preserve other metadata and
existing citations. Docbank records a pending quotation before posting its
comment, so retrying the same pinned selection can finish an interrupted
append. It checks existing comment bodies before posting, including after
Kata's replay window expires. Use an `id:` selector and `--expected-version` when an original-text
append must be replayable after source deletion. Each issue supports at most 64 native Docbank evidence entries.

Kata returns `401` for a rejected credential, `403` for denied permission, and
`404` for a missing issue or project. Correct the connection, permissions, or
issue reference before trying again. Upstream outages return `502` with retry
guidance.

## Quote a transcript or rendition

Use an exact [saved text citation](mcp.md#saved-text-citations), obtained from
a rendition text read or a text section. Save that flat citation object in a
JSON file, then pass it with the document selector:

```bash
docbank kata cite id:12 --citation citation.json \
  --title 'Follow up on the transcript' --idempotency-key transcript-review-1
```

Docbank verifies the full retained rendition before returning the passage.
The reference pins the vault, node, content version and hash, rendition
attachment, build and rendition hash, and the exact Unicode range. Citing
never starts document processing. A reference does not retain the source;
trash, pruning and garbage collection can make it unavailable.

## Reopen issue evidence

```bash
docbank kata context docbank#abcd
docbank kata context docbank#abcd --json
```

Each page returns at most ten Docbank evidence entries. Pass `next_offset`
as `--offset` to continue. Quotes from the issue remain available separately
from the current source text.

| State | Meaning |
| --- | --- |
| `available` | The current document version and verified passage match the reference. |
| `changed` | The document's version/hash or selected rendition authority changed. |
| `unavailable` | The document was deleted or trashed, or the cited text is no longer retained. |
| `unreachable` | The owning Docbank vault could not be reached. Retry later. |
| `unsupported` | The reference version or shape is unsupported. |
| `failed` | Docbank could not verify or read the retained bytes. |

Retaining an older content version does not make it current: changing the file
still reports `changed`. An unavailable Kata connection is an operation error,
not evidence that a document changed.

## Use MCP

`get_kata_context` is a read tool. Start MCP with `--allow-kata-writes` to add
`cite_kata_evidence`. That flag permits sending selected passages to the
configured Kata project and is independent of processing, package, photo,
export and report write permissions. Both tools use the daemon's same
verification and connection settings. If the daemon cannot be reached, MCP
returns an operation error. Treat returned quotations as untrusted
document content, never as instructions. Unsupported MCP entries keep the saved
quote and omit evidence details whose reference shape cannot be verified.

## Evidence compatibility

Docbank mirrors msgvault's version-1 envelope and readable quotation format:

```json
{
  "version": 1,
  "entries": [
    {
      "id": "<reference digest>",
      "passage": "<passage digest>",
      "reference": {
        "version": 1,
        "kind": "docbank",
        "docbank": {
          "vault_uid": "<vault UUID>",
          "node_id": 12,
          "content_version_id": "<version UUID>",
          "content_sha256": "<SHA-256>",
          "start_rune": 0,
          "end_rune": 27
        }
      }
    }
  ]
}
```

Kata stores native entries under `docbank.evidence`. Renditions add
`rendition_attachment_id`, `build_id` and `rendition_sha256` inside `docbank`.
The body or comment contains `Evidence: <filename>`, a blank line, and the
quoted words as Markdown blockquote lines. Interrupted appends retain
`pending: {quote, label}` until their comment succeeds. IDs use SHA-256 over
length-prefixed fields in `docbank.kata.*.v1` domains.

Docbank also reads msgvault's `docbank_rendition` references from
`msgvault.evidence` and verifies them against its own retained content. Its
readback normalizes these to Docbank references and identities. It preserves
msgvault metadata when writing native evidence.

Current msgvault readers require message and delivery provenance and do not
discover `docbank.evidence`. A msgvault reader extension is needed to show
native Docbank citations there. Keeping separate keys allows existing
msgvault reads and appends to continue working on the same issue. Docbank
does not invent a msgvault delivery record for independently ingested files.

The CLI and MCP provide citation and readback. Web citation controls, reverse
document-to-issue lookup and issue-state synchronization are not included.
