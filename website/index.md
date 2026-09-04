# The system of record for documents your agents can use

Docbank is a local-first, open-source document vault. Originals stay exact and
authoritative while governed processing makes them searchable for people,
systems, and agents.

Docbank is usable through the CLI, web application, terminal interface, HTTP
API, embedded Go package, and agent tooling.

## Install

On macOS or Linux:

```sh
curl -fsSL https://docbank.ai/install.sh | sh
```

On Windows:

```powershell
irm https://docbank.ai/install.ps1 | iex
```

Then [follow the authority lifecycle](/guide/) or read the
[setup documentation](/docs/setup/).

## One record. Explicit states.

A document can be copied, indexed, transformed, and served through several
interfaces. Its authority does not move implicitly with those operations.

1. **Source** — exact retained bytes and stable identity.
2. **Authorization** — a reviewed processing boundary.
3. **Understanding** — derivatives bound to one source version.
4. **Retrieval** — results backed by evidence and source identity.
5. **Recovery** — verified restore under operator control.

## Built around the record, not the index

Docbank separates durable document authority from the systems that make
documents useful. That boundary stays visible in routine operator work.

### Source sovereign

Keep the original exact. Content-addressed bytes, stable document identity,
immutable versions, and checksums preserve what entered the system.

### Govern understanding

Approve what may leave control. Processing policy names the provider boundary
and retained outputs. Derived text and embeddings remain tied to the source
version that produced them.

### Constrain answers

Retrieve with evidence. Search and agent access operate over verified current
content while preserving the document and version behind a result.

### Restore the record

Recover without a provider. Incremental snapshots retain content, metadata, and
history so recovery does not depend on a search or processing service.

## A vault operators can inspect

[![Docbank web vault showing a synthetic technical document
collection](https://docbank.ai/assets/generated/web-vault-browser.png)](https://docbank.ai/assets/generated/web-vault-browser.png)

Browse a filesystem-shaped document tree, inspect document identity and
storage state, and move through retained authority without leaving the vault
interface. The capture shows the real web interface against a generated
synthetic vault.

## Choose the boundary per workload

Understanding is governed work, not an invisible side effect of storage.
Operators decide where it runs and which outputs become retained derivatives.

- **Local:** use local tooling when document bytes should remain on
  operator-controlled hardware.
- **Self-hosted:** point governed jobs at infrastructure your organization
  operates and audits.
- **Hosted:** authorize a provider for a defined task while keeping the original
  and resulting record in Docbank.
- **None:** a document remains a complete authoritative record even when no
  derivative is produced.

## AI understanding, on the record

OCR output, renditions, chunks, embeddings, and vector indexes are records
too: cataloged, fingerprinted, and bound to the exact source version that
produced them.

![One exact source version feeding governed rendition, chunk, embedding, and
index derivatives that can be purged and rebuilt without touching the
record](https://docbank.ai/assets/intelligence-pipeline.svg)

- **Catalog derivatives:** renditions, chunk sets, embedding generations, and
  vector indexes publish atomically into a durable catalog, each fingerprinted
  against one immutable source version.
- **Replace providers:** local, self-hosted, and hosted OCR and embedding
  providers plug into standard bridges. Consent is durable and revocable, and
  uploads bind to the exact inspected bytes.
- **Rebuild intelligence:** purge, re-derive, and rebuild renditions,
  embeddings, and indexes deterministically. Derivatives back up and restore
  without any provider, and originals are never touched.
- **Explain retrieval:** hybrid lexical and semantic search, with optional
  expansion and reranking, explains each ranked result and keeps the document
  and version behind it.

## One authority across every surface

People, applications, and agents work through interfaces suited to the task.
They do not create separate copies of record.

- **CLI:** scriptable operator workflows.
- **Web:** visual browsing and maintenance.
- **TUI:** keyboard-first vault inspection.
- **HTTP:** an authenticated filesystem-shaped API.
- **Go:** embedded vault ownership.
- **MCP:** bounded tools for agent work.

[Integrate an agent](/docs/agents/) or [inspect the HTTP
contract](/docs/architecture/http-api/).

## Not a cloud drive. Not a git repo.

Docbank is closer to a storage appliance with an evidence-aware intelligence
layer: the operator owns the vault, its policy, and its recovery path.

- **Cloud drive:** the service is the record. Identity, search, processing, and
  recovery are coupled to one provider account and its lifecycle.
- **Git repository:** the ledger expects rewriting. Rebase and force-push
  rewrite history by design, identity follows paths, and large originals or AI
  derivatives overflow into bolt-on stores with no consent or provenance
  contract.
- **Docbank:** the vault is the record. Storage locations, processors, and
  interfaces are replaceable participants around operator-controlled
  authority, including every AI-derived record.

## Follow one document through the system

The [authority guide](/guide/) makes each boundary concrete. The [operating
documentation](/docs/) carries installation, workflows, architecture, and
exact command behavior.
