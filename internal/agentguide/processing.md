---
last_edited: 2026-11-05
---

# Review document processing

## MCP processing

Processing can disclose source bytes or derived text to providers. Obtain an
exact node/version pair from `list_documents`, `search_documents`, or
`list_document_versions`, and an operator-supplied configured profile name.

1. Call `get_processing_plan` with `node_id`, `content_version_id`, and `profile`
   in the same running MCP process that will start processing. Review its full
   provider disclosure and consent state before proceeding.
2. `start_processing` is available only when the operator starts MCP with
   `--allow-processing`. It accepts only `content_version_id` and the reviewed
   `plan_fingerprint`. Consent must already be active for that exact plan.
3. Follow the returned job with `get_processing_status {"job_id":"<returned-job-id>"}`.
   Read `get_processing_coverage` for the selected source fence when needed.
4. Refresh `get_document` for the current node/version, select an active
   rendition and read it through `read_rendition_text` with the exact
   vault/node/version/attachment identities. Follow `next_offset` to `eof`.

There is no MCP consent argument or consent-granting tool. Enqueue sends
`Consent: false`; `--allow-processing` enables the tool but grants no consent.
The operator can use CLI `processing build --consent` for an exact reviewed
plan, but that both records consent and starts work. It is not consent-only.
A plan from another MCP process, an evicted plan, a changed profile, or a changed
source requires a fresh plan and appropriate review. Never substitute a new
fingerprint silently.

## MCP equivalents and gaps

| CLI command | MCP tool | Difference |
| --- | --- | --- |
| `docbank processing profiles` | Operator CLI action | Profile discovery/configuration is not an MCP tool. |
| `docbank processing plan` | `get_processing_plan` | Exact node/version/profile; the MCP process remembers the disclosed plan. |
| `docbank processing build` | `start_processing` | Startup opt-in and existing consent required; no MCP `--consent`. |
| `docbank processing status` | `get_processing_status` | Uses the returned job identity. |
| `docbank rendition window` | `read_rendition_text` | Uses exact active attachment identities and bounded Unicode windows. |

CLI flags in the reference below are not tool arguments. The MCP server fixes
vault scope at startup. Reading this guide performs no processing.
