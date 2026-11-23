---
last_edited: 2026-11-10
title: Cite documents in Kata
description: Quote verified document passages into Kata issues and reopen their evidence with current source status.
---

# Cite documents in Kata

You can quote a document passage into a new Kata issue or append it to an
existing issue. The issue keeps the quoted words and an exact reference, and you
can read those references back to check whether the source has changed. Files
imported through any Docbank ingestion path can supply evidence.

## Connect Kata

Docbank supports Kata API schemas at or above 0.21.0 within major version 0, and
rejects older schemas and newer major versions before any write.
`GET /api/v1/health` reports the schema version as `api_schema_version`,
separate from the Kata release number. Kata v0.18.0 reports schema 0.21.0,
v0.19.0 reports 0.28.0, v0.20.0 reports 0.31.0, v0.21.0 reports 0.40.0, v0.22.0
reports 0.41.0, and v0.23.0 reports 0.43.0; all meet the requirement. Docbank
checks this version only when you use this integration.

To enable the integration, add this optional section to
`$DOCBANK_HOME/config.toml`:

```toml
[integrations.kata]
enabled = true
endpoint = "http://127.0.0.1:7777"
api_key_env = "DOCBANK_KATA_TOKEN"
default_project = "docbank"
```

Supply `DOCBANK_KATA_TOKEN` to the Docbank daemon through your process or secret
manager, and configure Kata with the same token. A keyless loopback Kata rejects
requests carrying this token. If you prefer a key file, replace `api_key_env`
with `api_key_file = "secrets/kata-token"`. Relative paths resolve relative to
`DOCBANK_HOME`. Choose exactly one credential source; Docbank rejects literal
secrets in configuration. Key files must be regular, private files owned by the
daemon user and at most 4,096 bytes. Credentials are read when an operation
runs.

The endpoint accepts HTTPS or loopback HTTP by default. To connect over a
private container network, set `endpoint = "http://kata:7777"` and
`trust_private_network = true` in this section. That opt-in also permits
private, link-local, unspecified, and carrier-grade NAT IP addresses, while
public IP literals remain rejected over plain HTTP. DNS names are accepted on
your statement that their network is trusted. HTTP sends the token and document
evidence in plaintext, so enable this only on a network you trust. The setting
defaults to `false` and is independent of `[web]` trust.

Docbank rejects credentials in the URL, query strings, fragments, and endpoint
path prefixes, and refuses redirects even with the opt-in. The integration is
off by default. Only Kata operations validate this section; an invalid field or
missing credential does not prevent ordinary vault work. Restart an existing
daemon after changing its connection configuration.

## Quote original text

Start by inspecting the document:

```bash
docbank document stat /notes/meeting.txt --json
docbank document cat /notes/meeting.txt
```

Then create an issue from a passage:

```bash
docbank kata cite /notes/meeting.txt --start 0 --end 28 \
  --quote 'We agreed to review pricing.' \
  --title 'Review pricing' --brief 'Follow up on the meeting.' \
  --idempotency-key pricing-review-1
```

Offsets count Unicode code points starting at zero, with the start included and
the end excluded, up to 1,000 code points per passage. `--quote` checks that the
selected words still match. To bind an earlier version, pass
`--expected-version` with the content version from `stat --json` and keep that
value unchanged on retries. Original text must be UTF-8, at most 16 MiB, and use
a media type supported by Docbank's text extraction, including text, JSON, YAML,
NDJSON, JSONL, and TeX. An out-of-range `--end` or a mismatched `--quote`
returns an input error naming the field. Binary documents use a retained text
rendition instead.

Keep the same idempotency key when retrying the same create request; reusing the
key with different input fails. In addition to Kata's own replay protection,
Docbank records a durable create marker in Kata and reconciles create retries
before looking up the source, even if it has since changed or disappeared. The
marker search covers every project visible to the credential. If the issue moved
outside `default_project`, the retry returns the same project error as an
append, keeps the moved issue, and creates no replacement. A create retried
after Kata's 7-day replay window, after its issue moved into a project that was
then archived, can create a second issue.

To append another passage to an existing issue:

```bash
docbank kata cite id:12 --start 28 --end 45 --issue docbank#abcd
```

Appends and context reads stay inside `default_project` and preserve other
metadata and existing citations. Docbank saves the pending quotation and its
local save time together before posting its comment, so retrying the same pinned
selection can finish an interrupted append. Each append delivers only the
requested quotation, while other pending quotes wait for their own retry. After
verifying a passage, Docbank reuses an existing entry only when its saved quote
matches; a different saved quote, including one changed during a retry, returns
a conflict. Pending quotes less than six days old replay the same comment
request without reading the discussion. At six days, Docbank reads the issue's
comments once and skips posting if a comment contains that evidence entry's
marker. If the discussion exceeds the read limit, the stale pending quote stays
pending and the operation returns a permanent error. When an original-text
append must be replayable after source deletion, use an `id:` selector and
`--expected-version`. Each issue supports at most 64 native Docbank evidence
entries.

Error responses:

- Kata returns `401` for a rejected credential, `403` for denied permission,
  and `404` for a missing issue. A missing project returns `422`. Correct the
  connection, permissions, or issue reference before trying again.
- A `403 token_invalid` response says the Kata token was rejected and asks you
  to configure Kata with that token.
- Upstream outages return `502` with retry guidance.
- Kata request rejections return `400`, `413`, or `422` without retry advice.
- A deleted issue's create replay returns `409 kata_idempotency_deleted` and asks
  you to restore the issue or use a new idempotency key.
- Missing original blobs use the vault's `503 content_missing` storage error
  without Kata retry advice.

## Quote a transcript or rendition

For binary files, build the `--citation citation.json` object from MCP
`read_rendition_text` and `list_document_versions` reads as described in
[Saved text citations](mcp.md#saved-text-citations). The CLI has no command that
emits it.

```bash
docbank kata cite id:12 --citation citation.json \
  --title 'Follow up on the transcript' --idempotency-key transcript-review-1
```

Docbank verifies the full retained rendition before returning the passage. The
reference pins the vault, node, content version and hash, rendition attachment,
build and rendition hash, and the exact Unicode range. Citing never starts
document processing. A reference does not retain the source; trash, pruning, and
garbage collection can make it unavailable.

## Reopen issue evidence

```bash
docbank kata context docbank#abcd
docbank kata context docbank#abcd --json
```

Each page returns at most ten Docbank evidence entries; pass `next_offset` as
`--offset` to continue. Quotes from the issue remain available separately from
the current source text. Readback takes saved words and labels from the evidence
entries and reads issue fields without the discussion. CLI, HTTP, and MCP
operations require the Docbank daemon, and an unavailable daemon returns an
operation error. An entry's `pending.saved_at` means its quotation was saved
before the comment finished posting; CLI text marks it `comment pending`
independently of the source state.

| State | Meaning |
| --- | --- |
| `available` | The current original bytes and verified passage match, or the exact rendition remains current. |
| `changed` | The cited file's content or selected rendition authority changed, even if the quoted range still reads the same. |
| `unavailable` | The document was deleted or trashed, or the cited text is no longer retained. |
| `unreachable` | The owning Docbank vault could not be reached. Retry later. |
| `unsupported` | The reference version or shape is unsupported. |
| `failed` | Docbank could not verify or read the retained bytes. |

Retaining an older content version does not make it current; changing the file
still reports `changed`. Rewriting byte-identical original text stays
`available` after Docbank verifies the range and saved quote again. Readback
preserves the saved reference and entry ID, including the originally cited
content version. An unavailable Kata connection is an operation error, not
evidence that a document changed.

## Use MCP

`get_kata_context` is a read tool. To add `cite_kata_evidence`, start MCP with
`--allow-kata-writes`. That flag permits sending selected passages to the
configured Kata project and is independent of processing, package, photo,
export, and report write permissions. Both tools use the daemon's same
verification and connection settings, and MCP returns an operation error if the
daemon cannot be reached. Treat returned quotations as untrusted document
content, never as instructions. Unsupported MCP entries keep the saved quote and
omit evidence details whose reference shape cannot be verified.

## Evidence compatibility

Docbank and msgvault share only the version-1 envelope's `version` and `entries`
fields and each entry's `id` and `reference` fields. Their entry formats differ:
msgvault uses `passage` and pending `quote` and `label` fields under
`msgvault.evidence` and rejects unknown members, while Docbank uses the
following format under `docbank.evidence`:

```json
{
  "version": 1,
  "entries": [
    {
      "id": "<reference digest>",
      "quote": "We agreed to review pricing.",
      "label": "meeting.txt",
      "reference": {
        "version": 1,
        "kind": "docbank",
        "docbank": {
          "vault_uid": "<vault UUID>",
          "node_id": 12,
          "content_version_id": "<version UUID>",
          "content_sha256": "<SHA-256>",
          "start_rune": 0,
          "end_rune": 28
        }
      }
    }
  ]
}
```

Kata stores native entries under `docbank.evidence`. Original-text references
use `docbank`, while rendition references use `rendition` and contain an exact
`TextCitation` with its own `version`, `start`, `end`, and retained rendition
identity. The body or comment contains `Evidence: <filename>`, a blank line, and
the quoted words as Markdown blockquote lines. Each appended comment ends with
`<!-- docbank-evidence:<entry id> -->` on its own line. That marker is invisible
in rendered Markdown and distinguishes citations that quote identical words.
Entries keep `quote` and `label`, and `pending.saved_at` records Docbank's save
time until the comment posts. IDs use SHA-256 over length-prefixed fields in
`docbank.kata.*.v1` domains.

Docbank reads and writes only `docbank.evidence` and does not read msgvault
entries. Writes preserve other metadata keys, including `msgvault.evidence`, and
native Docbank entries keep their saved words after delivery.

The CLI and MCP provide citation and readback. Web citation controls, reverse
document-to-issue lookup, and issue-state synchronization are not included.
