---
last_edited: 2026-11-20
title: Embed in Go
description: Run Docbank vaults inside a Go application with SQLite or PostgreSQL metadata.
---

# Embed in Go

Import `go.kenn.io/docbank/docbank` to store, version, retrieve, and back up
documents directly inside your Go process without starting or discovering a
daemon. The embedded API shares the same tree, content checks, storage rules,
and exclusive vault lock with standalone Docbank.

Choose an embedded vault when your application owns the document lifecycle
itself. If independent processes need to share one standalone vault, use the
HTTP API instead.

## Use a PostgreSQL vault

You can back an embedded vault with PostgreSQL 16 or 18 using UTF-8 encoding,
while the standalone daemon and `New` use SQLite. For each PostgreSQL vault,
provision a dedicated schema owner and a separate runtime login, give the
process a private local directory for its vault files, and run one owning
process per vault catalog.

Docbank's numbered PostgreSQL catalog starts at version 1. See the
[schema lifecycle and runtime grants](https://github.com/kenn-io/docbank/blob/main/docs/internal/development.md#postgresql-schema-lifecycle)
for setup details.

For a fresh vault, call `InitializePostgres` with a caller-owned `*sql.DB`
connected as the schema owner, a `PostgresCatalog` with the schema and owner
names, and a new vault directory under an existing parent. It installs the
catalog if needed and publishes a private root with the matching primary
ownership marker. Call `GrantPostgresRuntime(ctx, owner, catalog, runtimeRole)`
with the embedded application's runtime login before opening. This grants the
login access to read and write catalog data and execute its routines. Setup and
restore retain control of the catalog identity and schema changes. Repeat the
call after an upgrade to apply the grants to the current schema.

The database operator provides the login and its database privileges. Docbank
checks that the login has `TEMPORARY` for paged tree walks. If the database
revokes that privilege from `PUBLIC`, grant it to the runtime login on the
vault database before calling `GrantPostgresRuntime`. The operator also
manages any additional privileges, including access inherited from other roles
and from `PUBLIC`.

Repeating initialization on the same bound and closed root returns its existing
vault ID. An existing catalog must already be at the current schema version.

If initialization reports that it installed a directory before an error, keep
the directory and retry at that path to verify the binding. If the catalog
reports `ErrPostgresPrimaryUnbound`, use a fresh directory. A retry commits its
own ownership epoch and keeps the earlier attempt fenced.

For a restore, prepare a pristine catalog with `SetupPostgres` and
`GrantPostgresRuntime`. Call `repository.RestorePostgres` with the owner pool,
the same catalog, and a `*pgx.ConnConfig` for the runtime login. Pass the saved
snapshot ID, a new target directory, and any protected roots. The backup can
come from SQLite or PostgreSQL. A catalog with a bound root is ineligible as a
restore destination, including when the vault has no documents.

If a restore returns an error, check the report. A report with
`CatalogCommitted` set means the restore committed; close any owner of that
root before retrying a failed index rebuild with `RebuildPostgresRestore`. If
the report has a target and an unconfirmed commit, keep that directory and
follow the
[ownership reconciliation procedure](architecture/backup.md#restoring-into-postgresql).

To open an initialized or restored vault, call `OpenPostgres` with
`Config{Root: vaultRoot}`, the same catalog, and the runtime login. It checks
the catalog binding and filesystem marker, then holds the root lock until you
call `Close`. Call `CreateBackup` to capture a portable snapshot.

Before upgrading, stop writes, capture and verify a portable backup, and
close the vault.
Run `SetupPostgres` and `GrantPostgresRuntime` with the new build and the
owner connection, then reopen. For data written by a fingerprint-only
development build, restore a portable backup into a fresh numbered catalog.

## Run native MCP reads

Use `Vault.CallReadTool` to run `get_vault_info`, `list_documents`, and
`search_documents` directly against a vault your application already owns. It
reuses the same handlers, schemas, authenticated cursors, search scope, result
bounds, and private-cache metadata as the standalone tools, without starting a
listener, discovering a daemon, or reading owner credentials.

Register only the tools your application admits. For example:

```go
import (
    "context"

    sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
    docbankmcp "go.kenn.io/docbank/mcp"
)

tool, err := docbankmcp.ReadTool("list_documents")
if err != nil {
    return err
}
server.AddTool(tool, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
    // Application-owned admission returns the authorized vault UUID and a
    // release function. Hold that authority fence through the operation.
    vaultID, release, err := admitVaultRead(ctx)
    if err != nil {
        return nil, err
    }
    defer release()
    return vault.CallReadTool(ctx, vaultID, req)
})
```

The admitted `vaultID` is a standard-library `uuid.UUID` that your application
resolves from current permissions on every call rather than copying an
untrusted tool argument. A zero ID or an ID for another vault returns
`docbank.ErrReadScope` before Docbank reads any document. `CallReadTool`
validates both arguments and output, including checks beyond the JSON schemas,
so this lower-level SDK registration is supported.

This initial execution contract requires access to the **whole vault**. Tool
filters narrow results within that vault; they do not enforce document
permissions. When you have separate readers, select separate vaults or grant
whole-vault access; partial-library and document-level grants require a future
scoped execution contract. Your application owns authentication, expiry, and revocation, and
must hold its admission fence through each call, including cursor
continuations. The vault's lifecycle lock keeps the vault open until admitted
operations return.

Search requires a configured processing `profile` and exactly one of
`content_version_ids` or `filters`. Explicit version lists are capped at 4,096
current versions, while filter searches cover every matching live file without
that cap. Empty scopes still run a normal search, including query and profile
validation. Vault info reports logical inventory for the admitted vault, and
exact-document retrieval sits outside this execution subset. Cursors belong to
the reader's vault and process lifetime; restart listing after reopening the
vault. Responses retain `cacheScope: private` and `ttlMs: 0`.

### Reuse other read definitions

`ReadTool(name)` returns an independent copy of the selected native read-only
definition, including nested schemas, annotations, and metadata, with schemas
represented as `map[string]any`. Unknown names and write operations return an
error. Looking up a definition opens no vault and registers no tools.
Definitions outside the three supported operations need their own execution
handler; a read-only annotation does not grant authority or enforce resource
scope.

## Inspect format coverage

Call `Vault.FormatCoverage` to read the immutable capability snapshot captured
after Docbank constructs the configured rendition providers. Call
`Vault.LookupFormat` to resolve a catalog ID or extension against that same
snapshot:

```go
coverage, err := vault.FormatCoverage(ctx)
if err != nil {
    return err
}
lookup, err := vault.LookupFormat(ctx, "wpd")
if err != nil {
    return err
}
```

Returned maps and slices are defensive copies, and changing them does not alter
later calls. Both methods follow the normal vault lifecycle and return
`ErrClosed` after `Vault.Close`. See [Format coverage](architecture/format-coverage.md)
for the seven capabilities and four states.

## Transfer explicitly exported email

To transfer an exported email into a vault:

1. Call `Vault.RegisterMailboxArchive(ctx, id, description)` once to establish
   a stable source archive identity.
2. Call `Vault.TransferEML(ctx, request, reader)` with a
   `docbank.MailboxTransferRequest` containing the archive ID, stable
   occurrence reference, SHA-256, byte size, settings identity, destination
   directory ID, and document name.

Docbank reads only from the supplied reader and never discovers source
applications, accounts, or host paths.

The returned receipt binds the ordinary message, decoded attachment documents,
and exact retained versions. Equal retries return that same receipt. Changing
the source requires `ExpectedRevision`, while a changed target conflicts and a
trashed target returns a tombstone. These calls follow the vault's existing
lifecycle and mutation ownership. See
[Explicit EML transfers](usage/importing.md#explicit-eml-transfers) for full
rules.

## Create a vault service

Each root is an independent archive, and one process can open several
non-overlapping roots at once. Docbank rejects the same root, an ancestor, or a
descendant while another daemon, embedded vault, or restore target owns it,
even when a filesystem alias spells that tree differently.

```go
package archive

import (
    "bytes"
    "context"
    "io"

    "go.kenn.io/docbank/docbank"
)

func StoreSession(ctx context.Context, root string, sessionID string, jsonl []byte) error {
    vault, err := docbank.New(ctx, docbank.Config{Root: root})
    if err != nil {
        return err
    }
    defer vault.Close()

    _, err = vault.Put(
        ctx,
        "/sessions/"+sessionID+".jsonl",
        bytes.NewReader(jsonl),
        docbank.PutOptions{MediaType: "application/x-ndjson"},
    )
    if err != nil {
        return err
    }

    content, err := vault.OpenContent(ctx, "/sessions/"+sessionID+".jsonl")
    if err != nil {
        return err
    }
    defer content.Reader.Close()
    _, err = io.Copy(io.Discard, content.Reader)
    if err != nil {
        return err
    }
    return content.Reader.Verify()
}
```

If you learn an origin after ingestion, you can append it with the same
immutable fact model:

```go
appended, err := vault.AppendProvenance(ctx, receipt.Node.ID,
    docbank.ProvenanceAppendOptions{
        IfRevision: receipt.Node.Revision,
        Source: docbank.ProvenanceSource{
            Kind: "agent", Description: "triage", Reference: "opaque://laptop/report.txt",
        },
    })
```

The result contains the updated node, live path, appended fact, and receipt.
To require the node revision you inspected, set a positive `IfRevision`; zero
makes the operation unconditional.

To correct an active caller-supplied fact on the same node, set `Supersedes`
to that fact's identity. Docbank records the source reference exactly as you
supply it and never opens it.

`Put` creates missing virtual directories automatically. Repeating the same
bytes and media type resolves to the current version, while changed bytes
append an immutable content version and keep the node ID. When you already know
the SHA-256 and byte count and want Docbank to reject a mismatched stream
before committing metadata, supply `PutOptions.Expected`. When replacing content
derived from an earlier read, supply a positive `PutOptions.IfRevision`;
Docbank rejects the write with `ErrStaleRevision` if the file changed since
that revision, even when the new bytes match the current head.

When your application owns an immutable key and must never overwrite different
content already stored there, use `Create` instead. It requires the expected
SHA-256 and size. An exact retry is idempotent, while a different byte
identity, media type, or node kind returns `ErrContentConflict` without
appending a version:

```go
receipt, err := vault.Create(ctx, "/records/immutable.jsonl", reader,
    docbank.CreateOptions{
        MediaType: "application/x-ndjson",
        Expected: docbank.ContentIdentity{SHA256: digest, Size: size},
        Provenance: &docbank.ProvenanceSource{
            Kind:        "agent-session",
            Description: "local session archive",
            Reference:   "sessions/01K123.jsonl",
        },
    },
)
```

`CreateOptions.Provenance` lets you record where an immutable document came from
without inventing application-specific Docbank tables. The kind and description
identify the source system, and the reference holds an opaque source-local URI,
archive key, path, or other stable identifier. You can also set `ModifiedAt` to
record the source's own timestamp in canonical UTC form. Docbank neither
interprets the reference nor grants it retention authority.

For a newly created document, Docbank commits the node, first content version,
ingest record, and provenance fact in one metadata transaction; if any part
fails, none of them commit. An exact `Create` retry succeeds only when its
active provenance also matches, so a caller cannot accidentally claim source
evidence that was never recorded. If you supersede that matching fact, the
original request returns `ErrContentConflict` unless another active fact still
matches, while a request matching the corrected provenance remains idempotent
when the content and media type are unchanged.

`AppendProvenance` can supersede active caller-supplied facts on the same node,
including facts supplied through `CreateOptions.Provenance`. Operational CLI
and watched-folder ingest facts cannot be superseded because they keep
re-ingest idempotent, so append newly learned origins alongside them instead.
Call `Provenance` to page through the history, newest first:

```go
page, err := vault.Provenance(ctx, receipt.Node.ID, docbank.ProvenanceOptions{
    Limit: 100,
})
if err != nil {
    return err
}
for _, fact := range page.Items {
    // fact.SourceReference remains the exact opaque identifier supplied above.
    _ = fact
}
```

The node, live path, total, and page all come from one read snapshot, and
`Path` is empty for a trashed node. Provenance only records origin; it does not
prevent trash empty, version pruning, or garbage collection, and your
application must manage any retention policy for external references itself.

`ContentIdentity` always describes decoded document bytes, whether Docbank
stores them raw, zstd-compressed, or in a pack. SHA-256 is the canonical
logical identity, not a digest of a physical storage file.

## Configure document processing

To enable document processing on an embedded vault, pass named
`ProcessingProfileConfig` values through `Config.Processing.Profiles`. Each
entry binds a canonical portable profile to its provider implementations, and
`EmbeddingProviders`, `EmbeddingDisclosures`, and `Tokenizers` are keyed by
the embedding binding names from that profile.

Every provider with an `operator_network` or `hosted_provider` trust boundary
needs a `ProcessingRuntimeDisclosure.Endpoint` when `docbank.New` opens the
vault. Set `RenditionDisclosure` for a rendition provider and an entry in
`EmbeddingDisclosures` for each network embedding binding. Opening the vault
fails if any required endpoint is missing or invalid; Docbank cannot infer an
endpoint from a provider interface.

Each endpoint must describe the provider's configured HTTP(S) destination
without URL credentials, query parameters, or a fragment. A `local_process`
provider accepts either an empty endpoint or `in-process`. Docbank fills in
omitted processor and deployment identities from the provider and profile, and
derives metadata classes and retained artifact roles. Review the resulting
`PlanProcessing` response before granting consent. These disclosures are part
of its fingerprint.

For example, given a canonical `profile` with a rendition provider and a
`text` embedding binding, the matching constructed providers, and that
binding's tokenizer:

```go
vault, err := docbank.New(ctx, docbank.Config{
    Root: root,
    Processing: docbank.ProcessingOptions{
        Profiles: map[string]docbank.ProcessingProfileConfig{
            "private": {
                Profile: profile,
                RenditionProvider: renditionProvider,
                RenditionDisclosure: docbank.ProcessingRuntimeDisclosure{
                    Endpoint: "http://127.0.0.1:5001",
                },
                EmbeddingProviders: map[string]document.EmbeddingProvider{
                    "text": embeddingProvider,
                },
                EmbeddingDisclosures: map[string]docbank.ProcessingRuntimeDisclosure{
                    "text": {Endpoint: "http://127.0.0.1:8080"},
                },
                Tokenizers: map[string]document.Tokenizer{"text": tokenizer},
            },
        },
    },
})
if err != nil {
    return err
}
defer vault.Close()
```

Use the same endpoints when you construct the providers; a disclosure names a
destination without configuring its transport. Import `document` from
`go.kenn.io/docbank/document`. See [Document understanding in Go](document-understanding.md)
for provider construction and [Document processing](usage/document-processing.md)
for planning, consent, retained results, and the private-deployment acceptance
runner.

To prefer content matches in search, set `DocumentSearchRequest.ContentFirst`,
following the
[HTTP search ordering contract](architecture/http-api.md#coverage-and-source-fenced-search).

To restrict search to the transcripts of exact recording sources, set
`DocumentSearchRequest.MediaSources`. Reports then set `MediaSourceSelection`
and list `MediaSelections`, and evidence names its `MediaSources`, following
the [HTTP media source contract](architecture/http-api.md#coverage-and-source-fenced-search).
Invalid selectors, or a mode or reranking choice they do not support, match
`docbank.ErrMediaSearchInvalid`.

## Read exact evidence windows

Call `ReadEvidenceWindow` to read a bounded excerpt from a rendition you have
already identified. Keep the file's vault, node, content-version, and content
SHA-256 alongside the attachment, build, and sanitized-Markdown SHA-256
returned by `Rendition` or document detail metadata, then pass all seven
identities in an `EvidenceWindowRequest`:

```go
window, err := vault.ReadEvidenceWindow(ctx, docbank.EvidenceWindowRequest{
    VaultUID: vault.ID(), NodeID: receipt.Node.ID,
    ContentVersionID: receipt.Version.ID, ContentSHA256: receipt.Computed.SHA256,
    RenditionAttachmentID: rendition.AttachmentID,
    BuildID: rendition.BuildID, RenditionSHA256: rendition.SHA256,
    Offset: 0, MaxChars: 8_000,
})
if err != nil {
    return err
}
// Keep window.NextOffset to continue from the exclusive end of this window.
```

Offsets count Unicode scalars. A `MaxChars` value of zero defaults to 8,000,
and any other value must be between 1 and 16,000. The response reports the
returned range in `ActualStart`, `ActualEnd`, and `NextOffset`, along with the
UTF-8 byte count in `ResponseBytes`. Reading from EOF returns empty text with
`EOF: true`, while an offset beyond EOF matches `ErrInvalidRenditionWindow` and
malformed references match `ErrInvalidEvidenceRequest`.

You can only read the current, live content version and its active rendition.
Renaming a file preserves the reference, but replacing the content or
rendition, trashing, pruning, or retiring it makes the old reference match
`ErrEvidenceUnavailable`. `ReadEvidenceWindow` never runs processing, selects a
newer rendition, or pins retention. Docbank holds the vault lifecycle lease
during the internal blob read and cleanup and releases it before the function
returns. You receive plain text rather than an open stream.

The returned digest is the one recorded in the catalog; a window read can end
before whole-artifact checksum verification reaches EOF. Search chunk and
embedding segment offsets use their own coordinates; do not pass them as
Markdown offsets. Start a document overview at zero, or continue from the
offset returned by an earlier window. See the
[HTTP evidence contract](architecture/http-api.md#exact-evidence-windows) for
the equivalent authenticated route.

## Retain and process a remote recording

You can keep a recording's permanent identity separate from the protected
reference used to fetch a local file. Submitting a remote recording stores the
pending occurrence without performing any network access. The `canonical_url`
field is optional for configured-origin requests, and acts as the identity
input for a generic URL reference when present; see the
[canonical URL rules](architecture/http-api.md#remote-recording-references) for
how Docbank derives permanent source identity. A Cap Cloud share or embed URL
maps to one [Cap source per video ID](architecture/http-api.md#remote-recording-references).
A Loom share or embed URL uses a route-qualified source because Loom does not
document equality between those routes, and submission keeps the Loom reference
unsupported without performing network access.

```go
remote, err := vault.SubmitRemoteRecording(ctx, docbank.RemoteRecordingRequest{
    OperationID:  "00000000-0000-4000-8000-000000000451",
    ReferenceURL: "https://private.invalid/share/call?token=synthetic",
    CanonicalURL: "https://recordings.invalid/share/call",
    Occurrence: docbank.MediaOccurrenceInput{
        Ref: "call-1", Revision: "1", Filename: "call.wav",
    },
})
if err != nil {
    return err
}
```

If that reply is lost, call `Vault.RemoteRecordingReceipt` to look up the saved
receipt by operation ID. `ErrNotFound` applies only to that lookup, and an
in-flight submission can still commit afterward.

Fetch the media file through your own approved path, then attach it with its
SHA-256 and byte count. WAV, MP3, and M4A files follow the existing media
rules. Remote MP4 originals must use `video/mp4`, a `.mp4` filename, and stay
within 20 MiB, 2,088,960 coded pixels, 300,000 milliseconds, and 18,000 frames.
The service's default and maximum audio byte limit is 4 GiB, with an audio
duration limit of 24 hours.

```go
original, err := vault.ImportRecordingArtifact(
    ctx, docbank.MediaArtifactRequest{
    OperationID: "00000000-0000-4000-8000-000000000452",
    SourceID:    remote.SourceID,
    OccurrenceID: remote.OccurrenceID,
    Kind:        "media",
    Origin:      "supplied",
    Filename:    "call.wav",
    MediaType:   "audio/wav",
    SHA256:      recordingHash,
    ByteLength:  recordingSize,
    Content:     recording,
})
if err != nil {
    return err
}
```

Import a caption or transcript only after you bind the original. Captions use
`application/x-subrip` with the built-in `supplied-captions` profile; they
preserve cue timing and supplied provenance and support lexical and auto
search, while semantic and hybrid search are not configured for supplied
captions. Transcripts use the built-in `supplied-transcript` profile after a
plan and consent grant.

```go
caption, err := vault.ImportRecordingArtifact(
    ctx, docbank.MediaArtifactRequest{
    OperationID: "00000000-0000-4000-8000-000000000453",
    SourceID: remote.SourceID, OccurrenceID: original.OccurrenceID,
    Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "captions.srt",
    MediaType: "application/x-subrip", SHA256: captionHash,
    ByteLength: captionSize, Content: captionReader,
})
if err != nil {
    return err
}
selector := docbank.ProcessingSelector{
    NodeID: nodeID, ContentVersionID: original.ContentVersionID,
    Profile: "supplied-captions",
}
plan, err := vault.PlanProcessing(ctx, docbank.ProcessingPlanRequest{
    Selector: selector,
})
if err != nil {
    return err
}
if _, err := vault.GrantProcessingPlanConsent(
    ctx, docbank.ProcessingConsentGrantRequest{
    PlanRequest: docbank.ProcessingPlanRequest{Selector: selector},
    PlanFingerprint: plan.Fingerprint,
}); err != nil {
    return err
}
receipt, err := vault.RetryMedia(ctx, "00000000-0000-4000-8000-000000000454",
    remote.SourceID, docbank.MediaProcessingRequest{
        Profile: "supplied-captions",
        SuppliedInputID: caption.SuppliedInputID,
    })
```

`RetryMedia` picks your newest visible occurrence for the source and returns
once the work is durably queued. It cannot select an older occurrence. Use
`PlanProcessing` and `StartProcessing` with that recording's node and current
content version instead. Call `MediaStatus` to inspect the newest
attempt and its coverage. Coverage follows the source version selected by the
visible occurrence, so a transcript for older bytes cannot cover a later
recording revision. A recognized Cap Cloud or Loom link returns `unsupported`
even when `Acquire` is set, because neither manual reference path has a
supported automatic download owner; import the file you hold with
`ImportRecordingArtifact`. Other remote acquisition remains unavailable.
Origin registration belongs to daemon configuration, and embedded vaults
register no self-hosted Cap origins, so the embedded API does not apply their
registered origin identity rules.

To read the published transcript, pass the same ID tuple you used for
processing:

```go
transcript, err := vault.MediaTranscript(ctx, docbank.MediaTranscriptRequest{
    SourceID: remote.SourceID,
    SourceVersionID: receipt.SourceVersionID,
    ContentVersionID: receipt.ContentVersionID,
})
```

`EvidenceState` is independent of `CoverageState` and `OperationState`, and
`EvidenceState == "ready"` is the only state that includes text. Units preserve
timing and speaker facts, while `Origin` distinguishes supplied from generated
evidence. A mismatched content version returns `stale` without text, so check
the complete tuple before displaying a transcript.

## Extract and read source metadata

Call `EnsureSourceMetadata` to verify and extract typed metadata from one
immutable content version using Docbank's current local extractor. The result
includes the complete `ContentVersion`, so you can bind those facts to the
exact SHA-256 and node version they describe:

```go
metadata, err := vault.EnsureSourceMetadata(ctx, receipt.Version.ID)
if err != nil {
    return err
}
for _, field := range metadata.Fields {
    // Interpret fields by namespace, key, and typed value.
    _ = field
}
```

`EnsureSourceMetadata` runs locally, processes only the requested version, and
returns once extraction finishes without starting background workers. If the
current extractor already has a recorded result, it reuses that result.

During extraction, the method holds the vault's mutation lock. It verifies the
complete original, including large media files, then applies the
[format-specific parsing limits](architecture/source-metadata.md#current-format-boundary).
Concurrent `Put`, `Create`, and maintenance calls wait until it finishes. If
Docbank cannot open or verify the source bytes, it returns
`ErrContentUnavailable`.

For a read-only lookup, call `SourceMetadata`; it returns `ErrNotFound` when no
result has been published. Both methods return all local fields, including
sensitive ones, and your application decides which fields it can disclose.

## Publish email attachment documents

Use `EnsureEmailMetadata` to retain the MIME inventory for one email version.
Then call `PublishEmailDocuments` with a
`document.EmailDocumentPublicationRequest`: an operation ID, the parent
node/version/hash/size, the returned generation and attachment IDs, and a
destination directory ID with its current revision. No host filesystem path
is accepted. This operation does not run automatically on import.
Create a new destination with `MkdirPath`.

Publication verifies retained payload bytes and creates ordinary file documents
for the full known attachment inventory in one transaction. Body
alternatives, multipart containers, and PNG, JPEG, or GIF pictures the HTML
body shows with `<img src="cid:...">` are excluded, as mail programs show those
pictures in the body instead of listing them. Other inline resources, including
a non-picture file the body references by `cid:`, and any part marked
`Content-Disposition: attachment`, are included. Docbank records which content
IDs the HTML body shows when it decodes the email; an email decoded by an
earlier Docbank release is decoded again by `EnsureEmailMetadata`. An
attached email becomes a child email, whose own attachments can be published
separately. An explicitly attached single-part root also becomes a child.
Each child takes its declared media type. A part declared as
`application/octet-stream`, or with no declared type, takes the type Docbank
detected from its bytes.
Parts inside encrypted containers stay encrypted relations without child files.
The receipt says whether the MIME inventory is `complete` or `partial`. It
records unavailable, unsupported, failed, or encrypted parts and does not
create empty files for them.

Attachments with equal bytes share storage while receiving separate document
occurrences. Docbank builds filenames from the safe MIME name plus operation
and occurrence identifiers, and keeps raw filenames in the MIME inventory. You
can alternatively pass a `reuse` selection to point at an existing child
version with matching bytes and its node revision. Renames, later content
versions, and decoder reprocessing never move an old relation to a different
version.

Keep the operation ID and request unchanged when you retry. Sending the same
request returns the original receipt, even after backup and restore, whereas a
changed request conflicts. Call `EmailDocumentPublication` to read that
receipt, and `EmailDocumentRelations` to list relations for either one parent
version or one child version along with each relation's current processing
status. Pages default to 100 rows, accept at most 250, include a total, and
return the next operation/order pair when another page exists. Publication
accepts at most 1,000 MIME parts, 128 MiB per payload, and 256 MiB of decoded
payloads, while HTTP request and response bodies are limited to 2 MiB.

Publication alone does not prove indexing or authorize provider disclosure. To
process a published attachment, call `RequestEmailDocumentProcessing` to submit
one receipt occurrence to the ordinary rendition scheduler with a processing
profile matching a configured provider, execution identity, artifact policy,
principal, scope, and consent classes.
Existing consent must authorize those exact inputs and retained outputs; grant
it with `GrantProcessingConsent` and advance the principal/scope revocation
fence with `RevokeProcessingConsent`. Neither publication nor processing
requests grant consent or inherit it from a parent email. Docbank inspects the
child bytes and rejects execution metadata that differs from the prepared
upload before enqueueing work. The embedded method runs the provider and waits
for completion, whereas the HTTP endpoint returns after enqueueing for the
daemon worker. Both paths recheck consent before provider access and before
publication.

Relation status reports `indexed` when ordinary text extraction or an active
rendition of that child version produces complete searchable output. Partial or
truncated renditions report `partial`, and empty output reports `none`.
Text-extraction failures report `failed` with reason `text_extraction_failed`,
while completed jobs without serving output report `decoded` with reason
`rendition_not_serving`. Other possible states are `pending`, `unsupported`,
`encrypted`, and `unavailable`. Ordinary search returns child matches, and
QueryV1 does not add an email-family traversal predicate.

Trash and restore preserve relations and do not cascade to children. You cannot
permanently delete or prune a referenced version. A targeted purge of a
referenced MIME inventory conflicts, while a vault-wide purge skips it and
purges other eligible derivatives; conflicts identify the blocking publication
operation. To release those references, call `RemoveEmailDocumentPublication`
with the operation ID and its request digest. That removes the receipt and its
relations, keeps the ordinary children intact, and gives up the operation's
retry guarantee while leaving other receipts' references and ordinary audit and
content-retention rules in place. After release, start a new publication with a
fresh operation ID or reuse the existing child versions. A generated filename
collision returns `email_document_conflict` and leaves the publication
uncommitted.

CLI users can [inspect and release blocking receipts](usage/trash-and-gc.md#release-email-attachment-references)
through the daemon with `docbank email documents`.

In the typed client, the principal/scope consent operations are named
`GrantScopedProcessingConsent` and `RevokeScopedProcessingConsent` to
distinguish them from consent for a configured processing plan.

The authenticated HTTP API and typed client expose the same operations:

| Operation | HTTP route |
| --- | --- |
| Publish | `POST /api/v1/email-document-publications` |
| Read receipt | `GET /api/v1/email-document-publications/{operation_id}` |
| Release receipt | `DELETE /api/v1/email-document-publications/{operation_id}` with `request_digest` |
| Read relations | `GET /api/v1/email-document-relations` with `parent_version_id` or `child_version_id` |
| Request processing | `POST /api/v1/email-document-processing` |
| Grant processing consent | `POST /api/v1/processing/consents` |
| Revoke processing consent | `POST /api/v1/processing/consents/revoke` |

These routes require the master API key and sit outside the current browser
session capability. All relation and receipt DTOs live in
`go.kenn.io/docbank/document`, and you can use the same identities for
navigation and downloads without inferring parentage from names or hashes.

## Read canonical visual previews

Call `EnsureVisualPreview` to synchronously process one immutable content
version when the current built-in recipe has no recorded result, returning the
active preview. If the recipe is already recorded, another recipe's active
result stays selected. Ready results report the preview bytes and dimensions,
while unsupported and failed results carry a stable failure code and no
content.

```go
preview, err := vault.EnsureVisualPreview(ctx, versionID)
if err != nil {
    return err
}
if preview.State == document.VisualPreviewReady {
    content, err := vault.OpenVisualPreview(ctx, versionID)
    if err != nil {
        return err
    }
    defer content.Reader.Close()
    // Reach EOF or call Verify before trusting the bytes.
}
```

`OpenVisualPreview` follows the same verified-reader contract as original
content; it returns `ErrVisualPreviewUnavailable` for a cataloged unsupported
or failed result, and `ErrNotFound` when no preview result exists.
`VisualPreview` is a read-only lookup. Opening an embedded vault does not
start a preview worker, and your application chooses when to call the
synchronous producer. The built-in producer supports JPEG, PNG, GIF, still
WebP, and supported embedded JPEG previews in ARW, DNG, CR2, NEF, and RAF
camera RAW files. See [Visual previews](architecture/visual-previews.md) for
format limits, output size, and recipe selection.

To read, produce, or stream one specific retained recipe, call
`VisualPreviewForSize`, `EnsureVisualPreviewForSize`, or
`OpenVisualPreviewForSize` with `VisualPreviewGrid`, `VisualPreviewFit`, or
`VisualPreviewLarge`. Their maximum edges are 512, 2560, and 4096 pixels.
Grid and fit leave the legacy active head alone, whereas a newly produced
large result becomes active and reusing a cached result preserves a different
head.

Call `docbank.VisualPreviewSupportsMediaType(mediaType)` to check normalized
media types without reading or decoding a source. It reports whether a decoder
path exists, though individual files can still return unsupported or failed
results.

## Inspect and repair stored content

`Put` and `Create` receipts include `Physical`, which describes the raw, zstd,
or packed representation on disk. Use `Physical` to inspect storage and
`ContentIdentity` to identify document bytes.

`Put` is an idempotent content write, not an integrity repair operation. Kit's
structural dedup can reuse an existing canonical representation without hashing
it, and the catalog can keep reading a packed copy even when a loose copy
exists. When you have trusted bytes for a known SHA-256 and size, call
`RepairContent`. It verifies the complete stream before replacing the physical
copy the catalog reads, while preserving every node and historical version
reference to that identity. Repairing packed content switches the catalog to a
verified loose copy, and a later repack reclaims the dead packed bytes.

### Choose loose compression

New content stays loose until you call `Pack`. While the standalone daemon
selects zstd when a new loose object is at least 4 KiB and compression saves
at least 10%, embedded vaults keep raw loose storage by default. You can match
the daemon policy or set your own thresholds:

```go
vault, err := docbank.New(ctx, docbank.Config{
    Root: root,
    LooseCompression: docbank.LooseCompressionOptions{
        Enabled:           true,
        MinBytes:          4 << 10,
        MinSavingsPercent: 10,
    },
})
```

Docbank keeps zstd only when the logical size meets `MinBytes` and the
completed encoding saves at least `MinSavingsPercent`; otherwise it publishes
raw loose content. Enabling compression does not migrate or rewrite existing
objects. `RepairContent` preserves an existing loose object's raw or zstd
encoding and applies this policy only when trusted bytes replace a packed or
missing physical copy. The zero value writes new loose content as raw.
[Backup restore](usage/backup.md#where-does-restored-content-go) applies its own
managed packing and compression policy, independently of these write options.
Mixed raw, zstd, and packed content all stay readable through the same verified
API. Receipts report the chosen physical encoding and stored size without
changing the logical SHA-256 or size. `PutReceipt.Created` reports a new
logical node, while `PhysicalCreated` separately reports that the operation
published new final loose authority.

During an eligible write, Docbank temporarily needs scratch space for both the
raw object and its compressed candidate before choosing which one to publish.
Call `LooseBacklog` to see how much indexed loose content is eligible for a
pack pass, including both logical and physically stored bytes and a split
between raw and compressed object counts. You can therefore schedule packing
from physical storage growth without walking loose objects. The report does
not trigger packing automatically.

## Read an existing vault without owning it

To read verified originals from an existing, checkpointed SQLite vault without
initializing, migrating, recovering, or modifying its files, call
`OpenContentReader`:

```go
reader, err := docbank.OpenContentReader(ctx, docbank.ContentReaderConfig{Root: root})
if err != nil {
    return err
}
defer reader.Close()
content, err := reader.OpenContent(ctx, "/sessions/example.jsonl")
if err != nil {
    return err
}
defer content.Reader.Close()
// Read to EOF or call content.Reader.Verify before trusting the bytes.
```

`reader.ID()` returns the vault identity. The reader holds the same exclusive
hierarchy lock as an owner until `Close` returns. Close every open stream
first, because `Close` waits for open streams.

`OpenContentReader` returns:

- `ErrVaultLocked` when a daemon, embedded vault, restore, or another reader
  holds an overlapping tree.
- `ErrVaultNeedsOwner` when an owner must initialize, upgrade, or recover the
  vault first. That covers a missing root, `vault.lock`, database, blob
  directory, or blob ownership marker, as well as an empty database, an older
  schema, SQLite `-wal`, `-shm`, or `-journal` files, and a pending restore
  handoff.
- Other errors for a newer or unreadable schema, an invalid vault identity, a
  mismatched blob ownership marker, or a database or handoff path that is not
  a regular file.

The reader changes nothing inside the vault, and writes in two places outside
it:

- The lock registry, using `DOCBANK_LOCK_DIR` when set. The reader creates the
  directory and its lock files as needed.
- The system temporary directory. Checking the schema creates a
  `docbank-current-schema-*.db` file there and deletes it before
  `OpenContentReader` returns, though a crash during the check can leave it
  behind. SQLite can also write its own temporary files there, or in
  `SQLITE_TMPDIR` when set.

The reader refuses to open if the lock registry, the system temporary
directory, or `SQLITE_TMPDIR` sits inside the vault, and the existing
`vault.lock` must be writable. Because the reader attaches only the primary
blob store, content stored only on a secondary store returns
`ErrContentUnavailable`.

## Identify a vault and verify reads

`vault.ID()` returns the archive's UUID. JSONL backup and restore preserve
that identity even when you restore a vault to a different filesystem root, so
your application can tell archives apart without treating paths as identity.

Do not trust an `OpenContent` stream until it reaches terminal `io.EOF` or
`Verify` succeeds; an early `Close` does not drain the stream. `Vault.Close`
waits for active operations and streams, closes storage, and releases the vault
lock.

`OpenContent` and `OpenVersionContent` wrap `ErrContentUnavailable` when
Docbank cannot open the catalog-authorized physical content or its physical
size disagrees with metadata. Metadata lookup failures keep their existing
`ErrNotFound`, `ErrNotFile`, or `ErrClosed` classification instead. A canceled
physical open can match both `context.Canceled` and `ErrContentUnavailable`, so
check the context error first if you distinguish cancellation.

To read a non-empty decoded logical byte range from one immutable version, call
`OpenVersionContentRange`. Offsets and lengths address the same logical bytes
described by `ContentVersion.BlobHash` and `Size`, whether those bytes are
stored raw loose, zstd-compressed loose, packed, or in a secondary store:

```go
part, err := vault.OpenVersionContentRange(ctx, versionID,
    docbank.ContentRangeOptions{Offset: 1 << 20, Length: 256 << 10},
)
if err != nil {
    return err
}
defer part.Reader.Close()
_, err = io.Copy(dst, part.Reader)
```

Range-read costs depend on how the object is stored:

- Raw loose content uses filesystem offsets.
- Compressed loose content is decoded to a temporary file first.
- Packed content is decoded in full into memory, so reading a small range can
  need RAM for the whole decoded object.

A range outside the version's bytes returns `ErrInvalidContentRange`. The
reader returns either the exact requested length or `io.ErrUnexpectedEOF` if
the stream ends early, and keeps the vault open until you call `Close`.

A successful partial read checks the selected location, decoded size, and
range bounds without verifying the whole object. To read and verify all bytes,
call `OpenVersionContent` or run maintenance verification.

## Traverse and mutate the tree

Call `Children` to read the live virtual tree one page at a time, so a large
directory is never loaded whole. Resolve a directory with `Stat`, then advance
through its direct children with `Limit` and `Offset`:

```go
manifests, err := vault.Stat(ctx, "/manifests")
if err != nil {
    return err
}
for offset := 0; ; {
    page, err := vault.Children(ctx, manifests.ID, docbank.ChildrenOptions{
        Limit:  500,
        Offset: offset,
    })
    if err != nil {
        return err
    }
    for _, child := range page.Items {
        // Inspect this bounded page.
        _ = child
    }
    offset += len(page.Items)
    if offset >= page.Total || len(page.Items) == 0 {
        break
    }
}
```

Pages list directories first and files second, sorted by name within each kind.
A zero limit uses `DefaultChildrenLimit`, and no call can exceed
`MaxChildrenLimit`. Both the total and the page come from one metadata
snapshot, though if you need a complete stable traversal you must avoid
concurrent tree mutations between page calls.

For a complete stable traversal, call `Walk`. It pins one SQLite snapshot
before returning and yields the selected root and its descendants in pages:

```go
walker, err := vault.Walk(ctx, "/sessions", docbank.WalkOptions{PageSize: 500})
if err != nil {
    return err
}
defer walker.Close()

for {
    page, err := walker.Next(ctx)
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    for _, entry := range page {
        // entry.Path is canonical within the pinned snapshot.
        _ = entry
    }
}
```

A zero page size uses `DefaultWalkPageSize`, and no page can exceed
`MaxWalkPageSize`. Paths are limited to `MaxWalkPathBytes`, and absolute tree
depth is limited to `MaxWalkDepth`. Later tree changes do not appear in the
pinned snapshot.

The walker loads the tree incrementally instead of loading the whole subtree
up front. Each node needs at most two sibling range seeks and one child seek;
the second sibling seek applies only when an include-trash walk finishes the
node IDs sharing one path and advances to the next name.

Always call `Walker.Close`, even after `io.EOF`. Repeated calls are safe, and
closing releases the read transaction, dedicated connection, and vault lease.
`Vault.Close` waits for all walkers and content readers to close.

`MkdirPath` creates one directory beneath an existing parent and returns it
with its canonical path, matching the daemon's `POST /api/v1/path/mkdir`. An
existing node at that path returns `ErrExists`.

`MovePath`, `TrashPath`, and `Restore` return the resulting node and canonical
path. Pass a positive `IfRevision` to reject stale mutations; `IfRevision == 0`
is unconditional. `Restore` accepts photo members inside trashed folders
and recovers their group, those folders, and their other contents. `EmptyTrash`
uses `MaxRoots` as a batch target, finishing complete photo groups even when
they exceed it; zero uses `DefaultTrashEmptyMaxRoots`, `More` signals that you
should schedule another batch, and `Held` counts roots the batch reached but
kept because a connected photo member is live, too new, or retained.

For an all-or-nothing reorganization of up to `MaxBatchMoves` nodes, call
`BatchMove`. Each source is either a path resolved inside the transaction or a
node ID with the revision you previously inspected. Every destination is the
node's full final path, and its parent resolves in the planned final tree; an
existing directory does not mean "move into." Docbank validates the complete
final tree before applying any change, so you can express file or directory
swaps and nested moves without temporary names or partial completion.

## Back up and restore an embedded vault

If your application owns a vault in-process, you can use the same portable,
topology-independent backup format as the daemon:

```go
repository, err := docbank.InitBackupRepository(backupRoot)
if err != nil {
    return err
}
snapshot, err := vault.CreateBackup(ctx, repository, docbank.BackupOptions{
    Tag: "before-import",
	Prepare: func(ctx context.Context) error {
		return snapshotApplicationCatalog(ctx, catalogSnapshot)
	},
	ExtraFiles: []docbank.BackupExtraFile{{
		Path: catalogSnapshot, RecordAs: "application/catalog.sqlite",
	}},
})
if err != nil {
    return err
}
proof, err := repository.Verify(ctx, docbank.BackupVerifyOptions{
    SnapshotID: snapshot.ID,
})
if err != nil {
	return err
}
if len(proof.Problems) != 0 {
	return fmt.Errorf("backup verification found %d problems", len(proof.Problems))
}
_, err = vault.RestoreBackup(ctx, repository, docbank.BackupRestoreOptions{
	SnapshotID:    snapshot.ID,
	Target:        restoreRoot,
	ProtectedRoots: applicationStorageRoots,
})
return err
```

After a process restart, call `OpenBackupRepository`, and use `Snapshots` to
list recovery points in chronological order. Save the repository's `ID()` along
with the returned snapshot's `ID` and `MinReaderVersion` in your recovery
record; `MinReaderVersion` names the required backup format reader, not a
Docbank release. Pass the saved snapshot ID to verification and restore so a
newer backup on a shared repository cannot change which recovery point you use.

`InitBackupRepository` creates an empty repository that binds to the vault
installation on its first capture. Unmarked history, or a repository owned by
another installation of the vault such as the original of a restored copy,
returns `ErrBackupRepositoryClaimRequired`. To adopt that repository:

1. Call `vault.PreviewBackupClaim(ctx, repository)` to read its count, newest
   snapshot time, `Takeover` flag, and `Fingerprint`, and show them to the
   operator.
2. Call `vault.ClaimBackupRepository(ctx, repository, preview.Fingerprint)` to
   confirm.

If the repository changed since the preview, the claim returns
`ErrBackupRepositoryClaimChanged` and leaves the repository untouched. Claims
preserve history and cannot take another vault's repository, which returns
`ErrBackupRepositoryOwnerConflict`. See
[Adopt an existing repository](usage/backup.md#adopt-an-existing-repository).

`BackupSnapshot.Coverage` records photo assets and files, distinct verified
photo bytes, albums and members, hidden and trashed photos, checkout
definitions and entries, and retained receipts. A nil pointer with an empty
`BackupSnapshot.CoverageWarning` means the historical snapshot did not record
coverage. Unsupported coverage yields a nil pointer and a version-specific
warning while preserving the snapshot ID and base statistics for listing and
cleanup. Total verified bytes stay in `BackupSnapshot.BlobBytes`, and the trash
count includes an asset when any managed file is trashed. `Coverage.Excluded`
names omitted runtime state and rebuildable projections, including
`photo_technical_metadata`. Restore recounts version-1 coverage before
publication and rejects unsupported versions with
`*UnsupportedBackupCoverageError`, whose `Version` names the recorded version.
Verification checks manifest consistency and physical content and reports
unsupported coverage versions, without recounting photo coverage.
[Older readers require an upgrade](usage/backup.md#what-each-snapshot-contains)
before restoring coverage-bearing snapshots. See
[Backup coverage](usage/backup.md#what-each-snapshot-contains) for the complete
exclusions and restore rules.

Capture prevents content reclamation for the whole snapshot. Ordinary appends
resume once Docbank pins the catalog view for the backup, while physical
maintenance waits until Docbank publishes the manifest.

To include your application's catalog in the same backup:

1. Use `Prepare` to create an immutable catalog snapshot while writes are paused.
2. Declare the file in `ExtraFiles`.
3. Keep the file unchanged until `CreateBackup` returns.

`RecordAs` sets the file's relative location beneath the restored vault root.
Coordinate your application's own writes during `Prepare`; Docbank's freeze
pauses Docbank mutations, not changes to an unrelated application database. Do
not call vault mutations from `Prepare` while that lock is held.

Extras larger than 64 MiB stream in chunks, including files over 4 GiB, and
those snapshots require backup reader version 6. Once one exists, older readers
cannot list snapshots, create backups, prune, or restore or verify the latest
snapshot, and verification of all snapshots also fails. Older readers can still
restore or verify a supported older snapshot by ID. Upgrade every reader before
writing large extras to a shared repository. Existing extras larger than 64 MiB
are stored again as chunks on their first capture, and their old whole-file
blobs remain until you remove and prune the older snapshots.

Capture fails if an extra's length changes after Docbank opens it. That check
does not detect in-place changes, so the immutable-source requirement still
applies. Restore stages all extras as complete temporary files before replacing
their destinations. Leave enough disk space for those copies alongside any
existing files.

If a file contains credentials or tokens, mark it as `Sensitive`. Docbank
rejects sensitive files in a plaintext repository unless you set
`AllowPlaintextSecrets` for that backup.

Restore always targets a separate root. It rejects overlap with the live vault
or repository and checks content, SQLite integrity, and manifest statistics
before making the result available. Pass any other application storage in
`ProtectedRoots`; Docbank checks those locations before restore cleanup too.
Embedded restore builds a fresh primary store and does not recreate secondary
store placement.

If the original vault is unavailable, open the backup repository and restore
directly without initializing or opening a source vault:

```go
repository, err := docbank.OpenBackupRepository(backupRoot)
if err != nil {
    return err
}
_, err = repository.Restore(ctx, docbank.BackupRestoreOptions{
    SnapshotID: snapshotID,
    Target: restoreRoot,
    ProtectedRoots: applicationStorageRoots,
})
return err
```

If you omit the snapshot ID, Docbank selects the latest recovery point. Set
`SQLite` in `BackupRestoreOptions` to choose your application's driver, such as
`modernc.Driver{}` from `go.kenn.io/docbank/sqlite/modernc`; omitting it uses
the build's default driver. If your application uses modernc in a CGO build,
select it explicitly for restore too, because the default CGO driver needs the
`fts5` build tag. Restore includes declared host files along with vault content
and rejects the repository, declared protected roots, and targets held by
another vault. Include any offline source or application storage you want to
preserve in `ProtectedRoots`; the repository cannot infer their current
locations. `Vault.RestoreBackup` also protects its open vault automatically and
keeps that vault's configured SQLite driver.

### Read selected files without restoring

To read content or host files directly from a backup without creating a vault,
restoring a catalog, or copying unrelated content, call
`BackupRepository.OpenSnapshot` with an exact snapshot ID. This works even when
the original vault is gone.

Call `Object` to look up a whole-file SHA-256, or `Extra` to look up the exact
`RecordAs` name supplied at capture. Both return `BackupObject` with `SHA256`
and `Bytes`. You can only read files listed in that snapshot; a missing member
or name wraps `fs.ErrNotExist`, even when another snapshot contains the file.

For example, copy an application catalog to a private temporary file and
release the reader before inspecting it:

```go
repository, err := docbank.OpenBackupRepository(backupRoot)
if err != nil {
    return err
}
reader, err := repository.OpenSnapshot(ctx, snapshotID)
if err != nil {
    return err
}
catalog, err := reader.Extra("application/catalog.sqlite")
if err != nil {
    return errors.Join(err, reader.Close())
}
fmt.Printf("Catalog: %d bytes\n", catalog.Bytes)
file, err := os.CreateTemp("", "backup-catalog-*")
if err != nil {
    return errors.Join(err, reader.Close())
}
defer os.Remove(file.Name())
_, copyErr := reader.CopyExtra(ctx, "application/catalog.sqlite", file)
if err := errors.Join(copyErr, file.Close(), reader.Close()); err != nil {
    return err
}
return inspectCatalog(file.Name())
```

`CopyObject` and `CopyExtra` stream both ordinary and chunked files, including
files over 4 GiB, and verify the complete hash and size. Discard all output if
copying or closing the destination fails. `Info`, `Object`, and `Extra` stay
usable after `Close`. Copying a valid snapshot member after closing returns
`fs.ErrClosed`, though an invalid hash or missing extra name can fail
validation first.

Use each reader sequentially and close it once you finish copying a group of
files. Opening loads the repository index, file lists, and recipes again; the
reader caches up to 64 open packs and keeps no file payload cache.

The repository must be writable for lease heartbeats. Opening a snapshot during
a backup or maintenance operation returns `ErrBackupRepositoryLocked`, which
you can retry later. A backup waits up to 60 seconds for readers before
returning the same error, and one large copy can exceed that wait, so scheduled
backups must treat `ErrBackupRepositoryLocked` as retryable. An open reader
keeps renewing its lease. Always close it, including after a failed copy, and
release it before processing the copied files. Forced maintenance or 30
minutes without a heartbeat, such as during sleep, can invalidate the lease,
after which missing or damaged bytes fail the copy. Close and reopen with the
same snapshot ID to reload placement after maintenance. A closed reader does
not prevent that snapshot from being forgotten later.

### Remove recovery points and reclaim storage

Call `BackupRepository.Forget` to remove selected snapshot records, and
`BackupRepository.Prune` to reclaim backup storage that retained snapshots no
longer need. Neither operation opens the source vault, and your application
decides retention schedules and which recovery points to keep.

Preview each operation before committing to it:

```go
selection, err := repository.Forget(ctx, docbank.BackupForgetOptions{
    SnapshotIDs: snapshotIDs,
    DryRun: true,
})
if err != nil {
    return err
}
// Present selection.Selected to the user. Call Forget again with DryRun false
// to apply the removal; selection.Forgotten is empty during a dry run.
fmt.Println("Selected recovery points:", selection.Selected)

cleanup, err := repository.Prune(ctx, docbank.BackupPruneOptions{DryRun: true})
if err != nil {
    return err
}
// After removing snapshots, preview cleanup again before calling Prune with
// DryRun false. PacksToRemove includes any old packs that will be rewritten.
fmt.Println("Packs selected for cleanup:", cleanup.PacksToRemove)
```

Forgetting alone does not reclaim packed bytes. It refuses to remove the last
recovery point unless you set `AllowEmpty`, returning `ErrBackupLastSnapshot`,
and refuses parents needed by retained incremental snapshots, returning
`ErrBackupSnapshotRequired`. Both cleanup operations take Kit's exclusive
repository lock, including dry runs, and return `ErrBackupRepositoryLocked` on
contention. Use `ForceUnlock` only for known abandoned locks.

Pruning removes wholly unused packs and rewrites packs with less than half
their encoded payload still needed. Mostly-live packs keep unused bytes, so
this is not full compaction or secure erasure. Planned byte counts cover old
pack files and copied payload rather than exact net savings or temporary-space
needs. Inspect the returned report even when an error occurs; completed removals
and writes can be partial. You can retry interrupted pruning. Standalone
operators use [daemon-backed CLI cleanup commands](usage/backup.md#remove-recovery-points-and-reclaim-storage),
which also print partial reports before returning an error. Retention schedules
remain your policy.

## Maintain physical storage

Ordinary `Put` calls publish loose content. When you are ready to move
authorized loose blobs into managed immutable packs, call `Pack`:

```go
report, err := vault.Pack(ctx, docbank.PackOptions{MaxBytes: 256 << 20})
if err != nil {
    return err
}
if report.More {
    // Run another bounded pass when scheduling allows.
}
```

`MaxBytes` sets a soft committed raw-byte budget; the pass finishes the blob
that crosses the budget, seals its pack, and stops, while zero is unlimited.
The returned report includes packing, reconciliation, missing or corrupt
content, and orphan cleanup outcomes. Your application should surface those
fields. A nil error alone is not a complete health report. Packing changes only
physical representation, and `OpenContent` keeps the same verified read
contract.

Embedded `GarbageCollect`, `Verify`, and `Repack` run as resumable bounded
passes. Setting `WorkBudget.MaxObjects == 0` uses `DefaultMaintenanceMaxObjects`,
and larger budgets cannot exceed `MaxMaintenanceObjects`. A positive `MaxBytes`
adds a soft byte bound, so one selected object can finish after crossing it. A
zero byte bound is unlimited, though the object bound still caps each pass.

When a report has `More`, pass a non-empty `NextCursor` back in the same
operation's next `WorkBudget`. Treat cursors as opaque and operation-specific;
malformed cursors and cursors from another operation return
`ErrInvalidMaintenanceCursor`. A Repack pass can validly report more work with an empty cursor when completed
mutations shrink the candidate set; in that case, repeat it with an empty
cursor. Cursors mark continuation
positions, not snapshot tokens; work inserted earlier in canonical order during
a cycle waits for a later cycle started without a cursor.

These embedded calls do less than the standalone full-run commands. Embedded
`Verify` checks a bounded page of blob bytes without running whole-catalog
metadata validation, and embedded `GarbageCollect` handles bounded unreachable
catalog authority without enumerating untracked filesystem files. The daemon's
`verify` and `gc` commands still run those full-run checks.

Your application decides when to empty trash and prune prior versions, and GC
can reclaim a blob only once no retained reference needs it. Embedded vaults
have no background maintenance scheduler, so your application also chooses when
to resume each bounded pass.

## Choose SQLite

By default, Docbank chooses the SQLite adapter to preserve performance where
CGO is available:

- CGO builds use `github.com/mattn/go-sqlite3`.
- `CGO_ENABLED=0` builds use `modernc.org/sqlite`.

You can select either adapter explicitly when opening a vault:

```go
import (
    "go.kenn.io/docbank/docbank"
    "go.kenn.io/docbank/sqlite/modernc"
)

vault, err := docbank.New(ctx, docbank.Config{
    Root: root,
    SQLite: modernc.Driver{},
})
```

In a CGO build, pass `sqlite/mattn.Driver` to select the CGO adapter
explicitly. Docbank selects the adapter when opening the vault, and query and
transaction operations run directly on that driver's `database/sql` pool.
Standalone backup and restore paths use the same adapter boundary rather than
silently switching SQLite implementations.

Docbank's adapters register their own `database/sql` drivers. Connection hooks
your application registers with `modernc.org/sqlite`, such as with its
package-level `RegisterConnectionHook`, do not run on vault connections. The
same applies to a `ConnectHook` on your own `github.com/mattn/go-sqlite3`
driver.

In a CGO build, the first vault opened through the mattn adapter, the default
there, turns off SQLite memory statistics for the whole process, removing a
global lock from every SQLite allocation. SQLite enforces
`PRAGMA soft_heap_limit` and `hard_heap_limit` only while those statistics are
on. If your application relies on a heap limit, open its own SQLite connection
before opening the first vault; once SQLite initializes, Docbank leaves the
setting alone.

SQLite allows that setting to change only while no other thread is using
SQLite. Do not use SQLite through `github.com/mattn/go-sqlite3` on another
goroutine until your first vault open returns; open your own connection either
before that first vault or after it opens.

## Ownership boundary

Do not point a daemon and an embedded application at the same root. `New` holds
the same exclusive hierarchy lock as the daemon for the entire vault lifetime
and fails if the requested root overlaps another active vault or restore target.

[`OpenContentReader`](#read-an-existing-vault-without-owning-it) takes the same
lock, so close the reader before a daemon or `New` opens that root.

The standalone CLI stays daemon-first and never opens storage directly.
Embedding is a separate application ownership mode, not a second privileged
path into a daemon-owned vault. External agents should continue using the
[authenticated HTTP contract](agents/integration.md).

## Idle embedding discovery

A quiet vault skips embedding discovery. One full pass resumes after any
committed write other than search or vector-index coverage bookkeeping, a
daemon restart, or the expiry of a consent grant selected by discovery. The
worker's own enqueue and completion writes also trigger another pass before
discovery settles. Transient input failures keep discovery retrying once a
minute until the input recovers or is permanently rejected.
