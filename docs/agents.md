---
title: Docbank for Agents
description: Why agents use docbank, which interface to choose, and the safety model for document automation.
---

# Docbank for agents

Use Docbank to file, find, update, and verify documents through one authenticated
API. The daemon owns the vault and checks each request. Agents use the same
validation, revision checks, and maintenance rules as the CLI.

## What the contract gives an agent

- **Stable references:** a node ID keeps identifying the same document after a
  rename or move. A SHA-256 hash identifies its exact content.
- **Conflict detection:** send the revision you inspected with `If-Match`.
  If another writer changed the node, the daemon returns HTTP 412.
- **Content checks:** uploads declare their hash and size. Downloads provide
  headers and a final digest trailer that the client must verify.
- **Structured results:** use paginated listings, bounded search, problem
  codes, and progress records instead of parsing human output. NDJSON progress
  sends one JSON record per line.

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

- **The daemon owns storage.** Never open `docbank.db`, rewrite pack files, or
  infer live content from the filesystem layout.
- **IDs identify nodes; paths give their current location.** Keep the ID after
  inspecting a node. Resolve the path again when the request depends on its
  current location.
- **Versions preserve prior content.** A file has a current version UUID,
  SHA-256, and size. `put` adds a version. `revert` creates a new current version
  from a prior one and leaves stored history intact.
- **A download needs a final check.** Read through successful EOF and verify
  the digest evidence before publishing the bytes.
- **Deletion has stages.** Trash, trash empty, GC, and repack have different
  effects. See [destructive maintenance](agents/integration.md#treat-destructive-maintenance-as-a-two-step-decision).
- **Storage moves change where Docbank may read content.** Review a preview,
  execute its token, and follow the durable job ID. Inspect an uncertain result
  before retrying the move. See [Multi-store Storage](usage/storage.md).

## Common agent workflows

| Reader task | Follow this guide |
|-------------|-------------------|
| Import a local tree or upload from another machine | [Create and ingest safely](agents/integration.md#create-and-ingest-safely) |
| Find existing content or download a version | [Read a tree without unbounded responses](agents/integration.md#read-a-tree-without-unbounded-responses) |
| Replace content, adopt a prior version, or reorganize inspected nodes | [Use revisions for read-modify-write](agents/integration.md#use-revisions-for-read-modify-write) |
| Capture a backup and check its final result | [Follow backup progress](agents/integration.md#follow-backup-progress-without-scraping-a-cli) |
| Preview placement, repair storage, or evacuate a store | [Multi-store Storage](usage/storage.md) |

## Start integrating

1. Generate the current contract with `docbank openapi --json`.
2. Configure a stable loopback port and strong API key.
3. Follow the [integration guide](agents/integration.md) through health,
   authentication, bounded reads, and a revision-aware filing loop.
4. Use the running OpenAPI document for exact request and response fields.
   Use structured problem codes to decide how to handle failures.
