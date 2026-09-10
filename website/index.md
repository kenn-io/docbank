# Your documents. Ready for you and your agents.

Keep, find, and recover documents in a vault you control. Docbank preserves saved
versions, checks file integrity, and gives people and agents the same document IDs.

Docbank is open source under Apache-2.0. Use the command line, web app, terminal
browser, HTTP API, or embedded Go package.

## Install

On macOS or Linux:

```sh
curl -fsSL https://docbank.ai/install.sh | sh
```

On Windows:

```powershell
irm https://docbank.ai/install.ps1 | iex
```

Then [see how it works](/guide/) or read the [setup guide](/docs/setup/).

## What happens to your documents?

1. **Import:** copy files into the vault; leave the sources untouched.
2. **Organize:** move and rename documents without changing their IDs.
3. **Update:** save a new version while retaining earlier content.
4. **Find:** search document names and extracted text.
5. **Recover:** restore deleted documents or verify a separate backup.

## Keep a record you can check

Docbank records which file belongs to each document and saved version. Search
indexes help you find that content; checksums let you check it.

- **Keep earlier content.** Each saved version keeps its own content and SHA-256
  checksum, a digest used to detect changed bytes. Replacing a document adds a version.
- **Know when a document changed.** Revision checks reject an edit based on stale
  information. An agent can read the new revision and decide what to do next.
- **Undo a mistaken deletion.** Move documents to trash and restore them later.
  Permanent deletion and reclaiming disk space are separate actions.
- **Test your recovery copy.** Incremental backups reuse unchanged content.
  Restore into a separate vault and verify the result before you rely on it.

## Browse your vault

[![Docbank web vault showing a synthetic technical document collection](https://docbank.ai/assets/generated/web-vault-browser.png)](https://docbank.ai/assets/generated/web-vault-browser.png)

Browse folders, search documents, inspect saved versions, and manage recoverable
trash in the local web app. The image shows the real interface with synthetic data.

## Choose where processing runs

Processing can create text or other outputs from a saved document. You configure
the provider and permitted work. The configuration guide defines which runtimes
the daemon can use.

| Choice | What you control |
| --- | --- |
| Local | Use local tools for work that must stay on hardware you control. |
| Self-hosted | Configure a supported provider endpoint on infrastructure your organization operates. |
| Hosted | Choose which processing requests may send document content to an external provider. |
| None | Keep and retrieve the original even when no processing output is available. |

See [processing configuration](/docs/configuration/) and
[document understanding in Go](/docs/document-understanding/) for requirements and limits.

## Build on the original

A derivative is an output created from a document: extracted text, a preview, or
an embedding used to compare meaning. Docbank ties retained derivatives to the
source version that produced them.

![A source version with its processing outputs: rendition, chunks, embeddings, and index](https://docbank.ai/assets/intelligence-pipeline.svg)

- **Know which version was used.** The processing catalog records source versions
  and processing identities. A generated result can be checked against its input
  and settings.
- **Make the provider explicit.** Embedding workers require a configured runtime.
  Applications using the Go provider packages supply credentials, authorization,
  consent, and scheduling.
- **Preserve the saved document.** Processing outputs have their own lifecycle.
  Creating or removing a derivative does not replace the saved source version.
- **Search names and text.** The search command finds document names and extracted
  text. The [search guide](/docs/usage/searching/) owns the supported filters;
  the [roadmap](/docs/roadmap/) describes semantic search plans.

Read the [Go processing guide](/docs/document-understanding/) for the available
packages and their contracts.

## Choose how you work

The command line, web app, terminal browser, and HTTP clients use one local daemon.
A Go application can own a separate vault in its own process.

| Interface | Use it to |
| --- | --- |
| CLI | Import, search, and maintain documents from scripts. |
| Web | Browse and manage documents visually. |
| Terminal browser (TUI) | Browse the vault with the keyboard. |
| HTTP API | Connect applications through authenticated requests. |
| Go | Own a separate vault inside your application. |
| Agents | Use the CLI or HTTP API to work with documents. |

[Integrate an agent](/docs/agents/) or [inspect the HTTP contract](/docs/architecture/http-api/).

## Is Docbank right for your records?

Use Docbank when you want to manage documents, retain their versions, and test
recovery on storage you control.

| Task | Fit |
| --- | --- |
| Sync and share files | Docbank does not synchronize folders across devices or create public share links. Keep a cloud drive if those are the tasks you need. |
| Review changes to source code | Docbank does not provide branches, merges, or code review workflows. |
| Retain and recover documents | Keep the document catalog and history under your control. Add filesystem or S3-compatible storage as your collection grows. |

## Follow one document through the system

Docbank is alpha software. Keep independent copies of irreplaceable material and
verify backups before relying on them.

Follow the [document guide](/guide/) from import to recovery. Use the
[documentation](/docs/) for setup, everyday tasks, and exact command behavior.
