---
last_edited: 2026-11-05
---

# Find document evidence

## MCP search

Use `search_documents` with an operator-supplied processing profile. For example,
replace the profile placeholder with a configured profile name:

```json
{"query":"invoice 2026","profile":"<operator-supplied-profile>","filters":{},"mode":"lexical"}
```

`query` and `profile` are required, including in lexical mode. Select exactly
one scope: `filters` (an empty object selects all current live files), or
`content_version_ids` containing 1–4,096 unique version IDs. Do not send both.
Use the tool schema for supported filters. The result `limit` is at most 100.
Profile discovery and configuration remain operator CLI work.

`mode` defaults to `auto`; explicit modes are `lexical`, `semantic` and `hybrid`.
Read `requested_mode`, `actual_mode`, `coverage`, `skipped_reasons` and
`reranking` before interpreting the hits. Degraded or incomplete retrieval can
omit evidence. An empty result is not proof that a document does not exist.
Opt-in reranking sends the query and bounded candidate excerpts to the
configured provider under the profile's existing consent.

Results include exact node/version identities. Call `get_document` with that
pair, select an active rendition, and use `read_rendition_text` to verify text.
For outlines or focused sections use `get_text_outline` and `read_text_section`.
Use `resolve_text_citation` to resolve a retained text reference. Consult each
tool's schema for its exact selectors; a search excerpt alone is not full evidence.

## MCP differences

| CLI command | MCP tool | Difference |
| --- | --- | --- |
| `docbank search` | `search_documents` | Uses the processing search API with a required profile and explicit scope. |
| `docbank document stat` | `get_document` | Requires the hit's node and version identities. |
| `docbank rendition window` | `read_rendition_text` | Requires vault/node/version/attachment identities; follows Unicode offsets. |

CLI `--agent` and `--json` are output flags, not MCP arguments. MCP does not
promise CLI folder hits, fuzzy flags, or plain-search fallback behavior. The
server selects the vault at startup; a call cannot change `DOCBANK_HOME`.
