---
last_edited: 2026-10-08
title: Format coverage
description: How Docbank reports detection, retention, metadata, expansion, text, page, and transcript support from the running binary.
---

# Format coverage

Docbank reports format support from the running binary in a
`format-coverage/v1` record. That record combines the classified format catalog
with fixture-backed local implementations and the rendition providers
constructed for that runtime. It is a capability inventory, not a claim that a
particular document has already been processed.

To check this inventory from the terminal, run `docbank formats`. You can also
read `GET /api/v1/formats/capabilities` through the authenticated daemon API, or
call `Vault.FormatCoverage` and `Vault.LookupFormat` from an embedded
application. Each entry point reads the snapshot captured when its server or
vault is created.

## Capabilities

Every format and provider variant reports all seven keys independently.

| Key | Question it answers |
| --- | --- |
| `detect` | Can Docbank identify the bytes as this format? |
| `retain` | Can Docbank keep and return the exact original bytes? |
| `metadata` | Can the local extractor record bounded source metadata? |
| `expand` | Can Docbank open the input as a bounded container? |
| `text` | Can a rendition provider return qualified Markdown? |
| `pages` | Can a rendition provider return qualified page images? |
| `transcript` | Can an audio or video provider return a qualified transcript? |

Keeping the stored bytes proves retention only. It does not prove detection,
metadata, expansion, text, pages, or transcript support.

Each capability reports one of four states:

| State | Meaning |
| --- | --- |
| `qualified` | A named fixture exercised the exact implementation, and that implementation is available in this runtime. |
| `unqualified` | An available implementation declares support, but no accepted fixture proves the complete claim. |
| `unsupported` | No implementation is available for this capability. |
| `not_applicable` | The capability does not apply to this format. |

Because the runtime has no archive decoder, archive `expand` reports
`unsupported`. Catalog extensions and MIME hints do not turn a missing decoder
into an unqualified implementation.

## Provider qualification

A provider declaration does not qualify a result on its own. Docbank matches a
catalog row to a configured provider using the catalog media family, the exact
`MediaType`, and `original_file` input. Text requires declared Markdown output,
pages require the image artifact role, and transcripts require the transcript
role on an audio/video format. Qualification also requires the same descriptor
fingerprint to match registered fixture evidence.

No shipped rendition provider is qualified for text, pages, or transcripts.
Configured providers report their declared capabilities as `unqualified`, and
the only provider qualification exercises a synthetic PDF-to-Markdown test
provider. Local detection, retention, and metadata have separate fixture
qualifications. None of those qualify rendition output.

Provider alternatives stay as separate variants, which prevents one provider's
text qualification from becoming another provider's page or transcript claim.
Providers that are not constructed for this runtime do not appear in the
inventory.

## Lookup results

Format IDs and extensions resolve against the full constructor snapshot before
Docbank applies an optional `family` filter. Lookup keeps your exact query and
returns one of three results:

- `format`, with the matching classified format
- `pending`, with a support limitation for a recognized format that is not yet
  in the catalog
- `unknown_format`, when neither inventory contains the query

An unknown lookup still counts as a successful read. The HTTP endpoint returns
status 200, so you can tell an inventory gap apart from a transport or server
error. Pass at most one of `format` and `extension`. Sending both returns
`422 invalid_format_query`.

## Keep the record current

`document/testdata/format_coverage.json` pins the complete compiled snapshot. If
your pull request adds a row to `document/format_metadata.json`, update that
pinned record in the same change. A capability can move to `qualified` only with
fixture evidence bound to the exact implementation identity or provider
fingerprint.

Detection and retention use readable version IDs maintained alongside their
implementations. Those IDs are version labels rather than hashes of source code.
For metadata, `generated_by.extractor_id` records the implementation
fingerprint, combining the local parser descriptor and the complete shared email
recipe with its Go version normalized to `toolchain-independent`. Because of
that normalization, a Go toolchain update does not invalidate fixture
qualification, while parser, recipe, or limit changes still alter the identity
and require matching fixture evidence.

Stored source-metadata generations use a separate fingerprint that keeps the
actual Go version, and their identities and reuse rules stay unchanged. When you
run the fixture tests, they check the independently pinned qualification
manifest. That manifest never automatically adopts the runtime implementation's
identity.
