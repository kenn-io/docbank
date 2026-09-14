---
last_edited: 2026-09-13
title: Format Coverage
description: How Docbank reports detection, retention, metadata, expansion, text, page, and transcript support from the running binary.
---

# Format Coverage

Docbank reports format support from the running binary. The
`format-coverage/v1` record combines the classified format catalog with
fixture-backed local implementations and the rendition providers constructed
for that runtime. It is a capability inventory, not a claim that a particular
document has already been processed.

Use `docbank formats` for a terminal view, or read
`GET /api/v1/formats/capabilities` through the authenticated daemon API. An
embedded application can call `Vault.FormatCoverage` and `Vault.LookupFormat`.
Each surface reads the snapshot captured when its server or vault was created.

## Capabilities

Every format and provider variant reports all seven keys independently.

| Key | Question it answers |
| --- | --- |
| `detect` | Can Docbank identify the bytes as this format? |
| `retain` | Can Docbank keep and return the exact original bytes? |
| `metadata` | Can the local extractor record bounded source metadata? |
| `expand` | Can Docbank open the input as a bounded container? |
| `text` | Can a rendition provider return qualified Markdown? |
| `pages` | Can a provider and local frame path produce qualified page images? |
| `transcript` | Can an audio or video provider return a qualified transcript? |

Stored bytes prove retention only. They do not prove detection, metadata,
expansion, text, pages, or transcript support.

Each capability has one of five states:

| State | Meaning |
| --- | --- |
| `qualified` | A named fixture exercised the exact implementation, and that implementation is available in this runtime. |
| `provider_required` | The exact provider is independently qualified but is not bound in this runtime. |
| `unqualified` | An implementation is declared or compiled, but no accepted fixture proves the complete claim. |
| `unsupported` | No implementation is available for this capability. |
| `not_applicable` | The capability does not apply to this format. |

The default local snapshot currently has no archive decoder. Archive
`expand` therefore reports `unsupported`; catalog extensions and MIME hints do
not turn an absent decoder into an unqualified implementation.

## Provider qualification

A provider declaration does not qualify a result by itself. Docbank joins a
catalog row to one exact provider descriptor using the catalog media family,
the exact `MediaType`, `original_file` input, and the required artifact role.
Text requires Markdown output and the Markdown role, pages require the image
role plus available page-frame machinery, and transcripts require the
transcript role on an audio/video format. The same descriptor fingerprint must
match the registered fixture evidence.

Provider alternatives remain separate variants. This prevents one provider's
text qualification from becoming another provider's page or transcript claim.

## Lookup results

Format IDs and extensions resolve against the full constructor snapshot before
an optional `family` filter is applied. Lookup preserves the caller's exact
query and returns one of:

- `format` with the matching classified format;
- `pending` with a named owner slice for a recognized format that is not yet in
  the catalog;
- `unknown_format` when neither inventory contains the query.

An unknown lookup is a successful read. The HTTP endpoint returns status 200,
so callers can distinguish an inventory gap from a transport or server error.
Use at most one of `format` and `extension`; sending both returns
`422 invalid_format_query`.

## Keep the record current

`document/testdata/format_coverage.json` pins the complete compiled snapshot.
Any pull request that adds a row to `document/format_metadata.json` must update
that pinned record in the same change. A capability may move to `qualified`
only with fixture evidence bound to the exact implementation or provider
fingerprint.
