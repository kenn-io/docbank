---
last_edited: 2026-11-13
title: Docbank for agents
description: Why agents use Docbank, which interface to choose, and how it keeps document automation safe.
---

# Docbank for agents

Use Docbank to file, find, update, and verify documents through one authenticated
API. The daemon owns the vault and checks every request, and agents follow the
same validation, revision checks, and maintenance rules as the CLI.

## What the contract gives an agent

- **Stable references.** A node ID still identifies the same document after a
  rename or move, while a SHA-256 hash identifies its content.
- **Conflict detection.** Send the revision you inspected in `If-Match`. If
  another writer changed the node, the daemon returns HTTP 412.
- **Content checks.** Uploads declare their hash and size, and downloads return
  headers plus a final digest trailer that your client must verify.
- **Structured results.** Use paginated listings, bounded search, problem
  codes, and progress records instead of parsing human output. NDJSON progress
  streams send one JSON record per line.

## Choose the right surface

| Surface | Use it for | Contract |
|---------|------------|----------|
| CLI | Human-directed work, shell scripts, and inspecting behavior | Readable output; machine modes where documented. Discover commands with `docbank cli search "<task>"`, then inspect the suggested help |
| HTTP API | Independent applications and long-running agent workflows | Authenticated JSON, revisions, pagination, structured errors |
| MCP | Claude Code, Codex, and other MCP clients | `docbank agent setup` or `docbank mcp config` for manual setup |
| OpenAPI | Client generation and capability discovery | `docbank openapi`, `/openapi.yaml` |
| Markdown docs | Context retrieval without HTML scraping | Every public `/foo/` page is also published at `/foo.md` |

You can use [command discovery](cli-reference.md#find-a-command-by-task) without a vault or daemon.

For endpoint setup, authentication, upload and download proof, revision-aware
mutations, backup progress, error handling, and a complete safe filing loop,
see the [Agent integration guide](agents/integration.md). To read the design
contract and non-goals, see the [HTTP API](architecture/http-api.md) page.

## The mental model

- **The daemon owns storage.** Never open `docbank.db`, rewrite pack files, or
  infer live content from the filesystem layout.
- **IDs identify nodes, and paths show their current location.** Keep the ID
  after you inspect a node, and resolve the path again whenever a request
  depends on its current location.
- **Versions preserve prior content.** Each file has a current version UUID,
  SHA-256, and size. `put` adds a version, and `revert` creates a new current
  version from a prior one while leaving stored history intact.
- **Downloads need a final check.** Read through successful EOF and verify the
  digest evidence before you publish the bytes.
- **Deletion happens in stages.** Trash, trash empty, GC, and repack each have
  different effects. See [destructive maintenance](agents/integration.md#treat-destructive-maintenance-as-a-two-step-decision).
- **Storage moves change where Docbank may read content.** Review a preview,
  execute its token, and follow the job ID. If a result is uncertain, inspect it
  before retrying the move. See [Multi-store storage](usage/storage.md).

## Common agent workflows

| Reader task | Follow this guide |
|-------------|-------------------|
| Import a local tree or upload from another machine | [Create and ingest safely](agents/integration.md#create-and-ingest-safely) |
| Find existing content or download a version | [Read a tree without unbounded responses](agents/integration.md#read-a-tree-without-unbounded-responses) |
| Replace content, adopt a prior version, or reorganize inspected nodes | [Use revisions for read-modify-write](agents/integration.md#use-revisions-for-read-modify-write) |
| Capture a backup and check its final result | [Follow backup progress](agents/integration.md#follow-backup-progress-without-scraping-a-cli) |
| Preview placement, repair storage, or evacuate a store | [Multi-store storage](usage/storage.md) |

## Start integrating

1. Connect Claude Code or Codex with `docbank agent setup`, or generate the current HTTP contract with `docbank openapi`.
2. Configure a fixed loopback port and a strong API key.
3. Walk through the [integration guide](agents/integration.md) for health checks,
   authentication, bounded reads, and a revision-aware filing loop.
4. Check the running OpenAPI document for request and response fields, and
   branch on structured problem codes to handle failures.
