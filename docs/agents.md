---
title: Docbank for Agents
description: Why agents use docbank, which interface to choose, and the safety model for document automation.
---

# Docbank for agents

Docbank is a document system of record with an agent-ready interface, not a
database that an automation should open directly. One daemon owns the vault.
The CLI, agents, scripts, and external applications all use its authenticated
HTTP contract and therefore share the same validation, concurrency, integrity,
and maintenance rules.

This makes Docbank useful when an agent needs to file, retrieve, reorganize,
or verify documents without taking ownership of their physical storage format.

## What the contract gives an agent

<div class="feature-grid">
  <section>
    <h3>Stable identity</h3>
    <p>Node IDs survive renames and moves; immutable SHA-256 identities describe file content.</p>
  </section>
  <section>
    <h3>Conflict evidence</h3>
    <p>Revisions and <code>If-Match</code> turn stale read-modify-write decisions into explicit HTTP 412 responses.</p>
  </section>
  <section>
    <h3>Byte evidence</h3>
    <p>Uploads declare hash and size; downloads can be checked against headers, a verified digest trailer, and the node record.</p>
  </section>
  <section>
    <h3>Bounded automation</h3>
    <p>Pagination, search limits, structured problem codes, dry runs, and NDJSON progress avoid scraping human output.</p>
  </section>
</div>

## Choose the right surface

| Surface | Use it for | Contract |
|---------|------------|----------|
| CLI | Human-directed work, shell scripts, and inspecting behavior | Readable output; machine modes where documented |
| HTTP API | Independent applications and long-running agent workflows | Authenticated JSON, revisions, pagination, structured errors |
| OpenAPI | Client generation and capability discovery | `docbank openapi --json`, `/openapi.json`, `/openapi.yaml` |
| Markdown docs | Context retrieval without HTML scraping | Every public `/foo/` page is also published at `/foo.md` |

The detailed [Agent Integration Guide](agents/integration.md) covers endpoint
setup, authentication, upload and download proof, revision-aware mutations,
backup progress, error handling, and a complete safe filing loop. The
[HTTP API](architecture/http-api.md) page explains the design contract and
non-goals.

## The mental model

1. **The daemon is the authority.** Never open `docbank.db`, rewrite pack
   files, or infer live content from filesystem layout.
2. **IDs identify nodes; paths describe current placement.** Retain a node ID
   after inspecting it. Re-resolve paths when the intent is path-specific.
3. **Content identity is immutable.** A file node names SHA-256 and size.
   Future edits create versions rather than modifying stored bytes in place.
4. **A stream is not verified until it finishes.** Read through successful EOF
   and require the digest evidence before publishing downloaded bytes.
5. **Destruction has stages.** Trash is recoverable; trash empty removes tree
   history; GC removes unreachable loose bytes or marks packed payload dead;
   repack reclaims physical packed space.
6. **Dry run before policy-changing maintenance.** Preview destructive work,
   evaluate the result, then make the separate explicit run request.

!!! info "Planned — audited scopes"
    A full-audit scope will make membership sticky and retain every successful
    authoritative mutation and content version. Ordinary trash remains
    reversible; trash empty will loudly exclude protected roots while removing
    eligible ordinary trash, and version pruning and GC cannot remove protected
    history. Refused protected mutations return every blocking scope. Agents
    must inspect the preview's structured `vault_metadata_retention` inventory
    as well as its scope baseline. The irreversible enable operation requires
    both the server-issued preview token and
    `acknowledge_vault_metadata_retention: true`; an agent must not supply that
    acknowledgment unless its caller explicitly approved permanent metadata
    retention outside the selected scope. Agents can inspect and verify history,
    but no normal agent surface will destroy it. See
    [Audited History](architecture/audited-history.md).

## Common agent workflows

- **File local material:** preflight large server-side trees with the intended
  exclusions, require no errors or over-limit files, then ingest with the same
  selection and inspect every added, skipped, excluded, and failed result.
- **Write from another machine:** upload one file with declared SHA-256, size,
  name, and destination directory ID; accept success only when the returned
  server-computed identity matches.
- **Reorganize after inspection:** read by ID, retain the revision, and mutate
  with `If-Match`. On `stale_revision`, re-read and reconsider rather than
  blindly replaying the action.
- **Retrieve for another system:** stage the response privately, hash while
  reading, require successful EOF and the digest trailer, then publish.
- **Protect a workflow boundary:** create a tagged incremental snapshot, follow
  structured progress to a terminal result, and verify the repository on the
  schedule appropriate for its storage medium.

## Start integrating

1. Generate the current contract with `docbank openapi --json`.
2. Configure a stable loopback port and strong API key.
3. Follow the [integration guide](agents/integration.md) through health,
   authentication, bounded reads, and a revision-aware filing loop.
4. Treat the running OpenAPI document and structured problem codes as the wire
   authority; treat these pages as the maintained operating model.
