# Development guide

Start with the component that owns the behavior, then update each client and
contract affected by the change. This page maps package responsibilities and
lists the checks contributors must preserve.

## Repository layout

| Location | Contents |
| --- | --- |
| `docbank/` | Public `go.kenn.io/docbank/docbank` embedded API and its tests |
| `cmd/docbank/` | Standalone command and daemon composition |
| `document/`, `report/`, `sqlite/` | Public processing, reporting, and SQLite packages |
| `internal/` | Vault implementation, HTTP API, generated Go client, and embedded web assets |
| `frontend/` | Web application, generated TypeScript client, and browser harnesses |
| `docs/` | Public guides and the private contributor guides in `internal/` |
| `website/` | Static product page, document guide, fonts, and site assets |
| `scripts/` | Build, test, generation, installation, and publishing tools |
| `deploy/` | Processing-service deployment and benchmark tooling |
| `.github/` | Hosted workflows and their configuration |
| `LICENSES/` | Third-party license texts |

The root also holds project entry points, legal notices, module metadata, and
configuration discovered there by development tools. Go code lives in
packages under directories, never at the root. Keep tests beside the package
they exercise.

`make build` writes the local executable to ignored `bin/` (`bin/docbank.exe`
on Windows). `make clean` removes those binaries and built frontend assets.
Documentation builds write to ignored `site/`; scratch documents and captures
belong in ignored `.superpowers/`.

### Generated API files

Run `make generate-api` from the repository root after changing HTTP contracts.
It regenerates `internal/api/openapi.yaml` from the routes, then generates both
`internal/apiclient/client.gen.go` and `frontend/src/generated/docbank.ts`.
Install the locked frontend dependencies with `npm ci --prefix frontend` first.
Review and commit the schema and clients together; do not edit generated files.

The Go generator config and templates live in `scripts/oapi-codegen/`; frontend
generation uses `frontend/orval.config.ts`. Paths in the Go generator config
are relative to the repository root. The overlay
`scripts/oapi-codegen/uuid.overlay.yaml` generates `format: uuid` values as the
standard library `uuid.UUID`. `make openapi` regenerates only the schema
and needs no running vault. The rendition and embedding bridge schemas remain
with their independent contracts in `document/bridge/` and
`document/embeddingbridge/`.

### PDF reader dependency corrections

`npm ci` runs `frontend/scripts/patch-embedpdf.mjs` against EmbedPDF 2.15.1.
It confines shortcuts to the viewer, destroys engines initialized after unmount,
and installs the readable page hooks in `embedpdf-page-hooks.mjs`. The hooks
expose actual image decoding and an inert overlay mount for shared PDF review.
They neither modify the PDF nor apply redactions. The Vite build checks that the
pinned bundle and exact payload are patched, including installs using
`--ignore-scripts`; run `npm --prefix frontend run postinstall` to apply them.

Remove each correction when upstream supplies its equivalent, and rerun the
combined reader/redaction browser qualification before changing the pin. Both
patch files and the bundled fallback font are included in the container's
frontend stage.

The PDF rendering CI lane runs `frontend/screenshots/redaction-review.run.mjs`
against both SQLite builds. It executes the document-reader and redaction-review
browser cases with synthetic vaults and the built application. Reader cases cover
activation, clipboard shortcuts, worker cleanup, Markdown form removal, link
isolation, failed module loading and delayed file details. Selecting PDFs in the
inspector must not prepare original-file downloads or start PDF workers.
Entering redaction during a smooth page turn preserves its destination rather
than an intermediate scroll offset. Returning to reading restores that page;
settled reading positions retain their exact scroll offsets.

## Share the Go build cache across worktrees

Make targets and CI compile Go with `-trimpath`, so binaries record
module-relative source paths instead of absolute ones. Without it, Go caches
every package separately for each checkout directory, and each git worktree
grows the build cache by another full copy. With it, identical source in any
checkout reuses the same cache entries.

- Run `make <target> GOFLAGS=-trimpath=false` to opt out for one command.
- Plain `go` commands outside Make do not add the flag. Run
  `go env -w GOFLAGS=-trimpath` to make it your default; Make keeps any value
  saved that way.

Debuggers need a source mapping for trimmed binaries:

- `dlv debug` and `dlv test` inherit `GOFLAGS` and cannot find source files.
  Pass `--build-flags=-trimpath=false`. Debug builds disable optimizations, so
  they never shared cache entries anyway.
- For `dlv exec` or `dlv attach` on a trimmed binary, start Delve from the
  repository root and run `config substitute-path -guess` (Delve 1.24 or
  later). It maps the module path to your checkout, the standard library to
  `GOROOT`, and dependencies to the module cache.

## Package ownership

| Area | Owns | Must not own |
|------|------|--------------|
| `internal/store` | schema, transactions, tree identity, reachability queries, typed domain errors | HTTP, CLI formatting, physical pack lifecycle |
| `internal/blob` | docbank adapter around Kit physical storage | virtual paths or node policy |
| `internal/backupapp` | frozen logical enumeration, fidelity stats, restore paths, mixed backup reads, packed restore policy | repository orchestration or CLI policy |
| `internal/ingest` | source traversal and bytes-before-reference pipeline | daemon discovery or UI behavior |
| `internal/api` | wire contract, auth, middleware, gate classification, error mapping | direct CLI output policy |
| `internal/daemonconn` | daemon convergence and receipt validation for the generated client | opening SQLite or blobs |
| `internal/home` | vault layout, privacy, and portable vault/tree locking | data operations |
| `internal/config` | strict config parsing and security validation | runtime discovery |
| `internal/telemetry` | the daemon's PostHog reporter options, event allowlist and its wiring of kit's install ID and heartbeat | vault data, HTTP auth, embedded vault paths |
| `document/mistral` | Mistral OCR, capability evidence, unit limits, and the optional office render-to-PDF route | generic format detection, office conversion, or storage authority |
| `cmd/docbank` | Cobra ergonomics and human output | store business logic |
| `docbank` | lifecycle and bounded public operations for one exclusively owned embedded vault | standalone CLI paths or a second storage implementation |

The Mistral text lane uses `UnitBoundProviderResponse`. The authenticated probe
records provider acceptance and page usage for each format; text results have
no `LocalUnits`. This method is part of the policy fingerprint and authorizes
response checks, not a pre-upload page bound. `UnitBoundLocalExact` remains the
PPTX contract and also identifies the configured office conversion route. For
the eight render profiles, `document/renderpdf` owns conversion and page
counting.
`document/mistral` owns PDF capability authority, generated byte and page
limits, retries, and the original/upload identity projection. See [Run Mistral
OCR safely](../document-understanding.md#run-mistral-ocr-safely) for the
supported formats, byte limits, response checks, render policy, and manifest
upgrade.

`document/internal/epubutil` owns bounded EPUB package reads, ordered manifest
and spine records, XML-base interpretation, and archive paths. Media inspection
uses these records to inspect every rootfile and conflicting declaration.
OOXML and ODF inspection also use its bounded ZIP reader.

`document/epub` owns single-package admission, ordered XHTML spine extraction,
and virtual usage. It consumes the shared utility and the bounded
`document.RenditionMarkdownFromXHTML` entry point. The latter uses the existing
Markdown writer with explicit XML token handling. Existing Markdown input
keeps its own token semantics. Provider libraries own no vault, consent, or
publication state. See [local EPUB extraction][epub-local] for limits and the
embedded and daemon routes. `cmd/docbank/rendition_runtime.go` selects the EPUB
provider using the existing generic profile limits and descriptor guard.
Keep virtual-unit counting in `document/epub`. Daemon coverage uses the public
planning, consent, retained-read, and lexical-search path with exact source
bytes and both filename-disclosure settings.

[epub-local]: ../document-understanding.md#extract-epub-locally

## Common change paths

### Change command help

Keep help in the owning Cobra command. Give every command with children a
workflow description, including parents whose handler only shows help. Give
runnable leaves and parents that perform work 1–4 synthetic example commands.
Add working parents to `workParents` in `cmd/docbank/help_lint_test.go`.
Prerequisite examples may invoke siblings; every line must parse and at least
one must invoke the owning command. The lint clones flag values and checks
syntax and arity without executing examples. Indented command lines in Long
text, root workflow steps, and quoted recovery commands are checked too.

Use a command group at the root, source each ID, and mark required flags.
Keep descriptions short: at most 12 Long lines for leaves and 20 for parents;
root help and the TUI key table have explicit exceptions. Root help is limited
to 6,500 bytes and leaves to 2,500, except the TUI. Only root help has a golden,
so command examples can improve without maintaining hundreds of snapshots.
After an intentional root change, review and regenerate it from the repository
root:

```sh
go test -tags fts5 ./cmd/docbank -run TestRootHelpGolden -update
```

Keep the CLI reference and guide examples in sync. Error next-step hints belong
at the CLI process boundary. Keep underlying error identity and daemon problem
text intact; strip only generated transport wrappers from CLI presentation.
Initialize Cobra's default help and completion commands before configuring
group argument checks. Diagnostic writers preserve terminal file descriptors
so automatic progress and action reporting still recognize stderr terminals.
Failed output-mode assignments preserve the last successfully parsed value.
Missing repository configuration gets a narrow internal typed cause when the
client decodes its exact problem code/detail pair. Do not classify every
`backup_repository` problem as invalid usage: that code also covers I/O failures.

The CLI property tests use Hegel alongside deterministic regressions. Keep its
Go module current with `go get hegel.dev/go/hegel@latest`. Hegel currently ships
engines for Linux and Windows amd64/arm64 and macOS arm64; the property file
excludes macOS amd64 until an engine is available. Deterministic CLI tests keep
running on every supported platform. Remove that exception when Hegel supports
the remaining architecture.

Register file/tree commands under `document`, vault identity and maintenance
under `vault`, and email operations under `email`. `report` owns search-count
evidence; leave `search` arguments available for arbitrary query words. Existing
domain groups keep their names. Use `registerGroupedCommand` after installing
the command's flags and children when moving another command. It retains a
hidden root definition for at least one release, with separate Cobra state and
shared handler flag values. Deprecation notices go to stderr at the process
boundary before argument parsing, including errors and help; Cobra's built-in
`Deprecated` field writes to stdout and breaks JSON and raw byte output.
Recovery hints use the canonical path from the compatibility annotation,
including argument and flag errors. Keep nested compatibility definitions
covered by `command_groups_test.go` and update the CLI reference mapping.
Report text chooses its next-step command from the invoked spelling so the
canonical path stays discoverable and legacy successful stdout stays compatible.

### Add a data operation

1. Add the transactional store operation and tests for its behavior.
2. Add the API route and wire types.
3. Choose the operation's side of the maintenance gate.
4. Add the internal client call, then the CLI command.
5. Generate and inspect OpenAPI.
6. Update the public reference and agent guidance.
7. Check that the CLI still reaches the vault only through HTTP.

### Change a wire contract

Update the API types, route tests, client decoding, OpenAPI assertions, public
HTTP documentation, and agent examples together.

Check whether an older running daemon could misinterpret the new request. If
it could, bump the daemon protocol revision. `Ensure` then replaces that daemon
before sending a data call.

Never rely on an optional JSON field to make a formerly destructive endpoint
safe against a daemon that ignores unknown fields. Use capability/protocol
separation or a new non-destructive route.

### Change storage or reachability

Read [Storage design](storage-design.md) first. Keep physical mechanics in Kit
and logical liveness in docbank. Exercise Kit catalog conformance plus a real
docbank lifecycle. Update GC reports so “reclaimed” means physical bytes
actually removed; packed logical death is pending repack.

### Change schema

Layouts that never shipped are disposable. v0.9.0 is the first released
compatibility boundary: incompatible SQLite changes use a tested deterministic
JSONL cutover from an exact released-schema fixture, not an in-place migration
ladder. Preserve the source database until the rebuilt current database has
imported and validated logical authority and restored its physical pack catalog.

Every release's storage schema version belongs in
`releasedStorageSchemaVersions` in `internal/store/upgrade.go`, with its exact
`schema.sql` saved as `internal/store/testdata/schema-<tag>.sql` and registered
in `releasedSchemaFixtures`. Three checks enforce this:

- `scripts/check-released-schemas.sh` runs in CI against every release tag. It
  fails when a tag's schema version is unlisted or its `schema.sql` has no
  exact fixture.
- `TestReleasedSchemaFixturesMatchVersions` fails when `schema.sql` changes
  without a schema version bump after that version shipped.
- `TestEveryReleasedSchemaFixtureUpgrades` opens each fixture with the current
  binary, so bumping the schema fails until the released version has a cutover
  adapter.

### Backend support and drift checks

`store.Store` is the database API boundary. Operations keep their validation,
transactions, result assembly and catalog maintenance together. Backend-specific
helpers handle concrete SQL and driver differences. The coverage machinery is
compiled only into tests: it adds no interface dispatch, capability lookup or
extra query to application requests.

From the repository root, inspect the current evidence without opening a database:

```sh
make backend-coverage
```

The report distinguishes native PostgreSQL operations, retained-data validation
and unqualified operations. A copied SQLite catalog can prove that PostgreSQL
reads and validates it; it cannot qualify the worker that would produce it.
Scenario counts describe the tests, not a completeness percentage or a
performance guarantee. Unlisted operations remain unqualified. The public vault
still opens SQLite.

These test files own the inventory:

- `internal/store/backend_contract_test.go` registers the executable scenarios
  and their scope. Existing cases run once per backend. Shared cases use the same
  assertions with each backend's fixture; PostgreSQL-specific cases cover its
  snapshots, locks and native mutations. SQLite also keeps its full existing suite.
- `internal/store/backend_schema_test.go` compares freshly installed canonical
  catalogs: tables, logical column types, nullability, primary-key order,
  foreign-key columns and actions, and index key columns, order, uniqueness and
  partial status. It also checks PostgreSQL's byte-order text collation.
  `backend_schema_exceptions_test.go` names each intentional or deferred
  difference. Unlisted differences fail; resolved or stale exceptions fail too.
- `internal/store/backend_maintenance_test.go` describes SQLite catalog effects
  and their PostgreSQL owners or unqualified operations for the report. Keep
  this inventory current when porting an effect. Operation tests establish
  behavioral equivalence by exercising the writes and checking their results.

The schema comparison deliberately normalizes integer widths, UUID/text storage
and SQLite's nullable primary-key metadata. It excludes FTS virtual/shadow
tables, engine allocation machinery and SQLite's released version field. It
does not compare defaults, views, trigger bodies, expression/predicate SQL or
foreign-key deferral timing. Those need operation tests. Matching index shapes
also do not prove matching query plans: keep expressions aligned with their
indexes and check plans on representative data when porting a query. Deferred
indexes remain a performance qualification gap, including on tables that native
operations already use.

Run the shared SQLite contracts in both supported builds:

```sh
go test -race -tags fts5 ./internal/store -run '^TestBackendContractsSQLite$'
CGO_ENABLED=0 go test -tags fts5 ./internal/store -run '^TestBackendContractsSQLite$'
```

With `DOCBANK_TEST_POSTGRES_DSN` set to a disposable PostgreSQL server, run
`make test-pg` for setup, permissions and the complete PostgreSQL contract suite.
CI runs that lane on PostgreSQL 16 and 18. To focus on one operation or the schema:

```sh
go test -tags fts5 ./internal/store -run '^TestBackendContractsPostgres/documents/'
go test -tags fts5 ./internal/store -run '^TestBackendContractsPostgres/schema/Schema$'
```

Add new PostgreSQL cases to the registry so the report and CI select the same
functions. Prefer one shared scenario where both backends have the same contract;
keep backend-specific failure and concurrency evidence where it adds coverage.
Do not repeat shared Go rejection cases on both databases merely to increase
counts. Port a complete operation with its access indexes, validation, catalog
effects and rollback behavior, then remove the relevant exceptions. Keep
remaining implementation work in Kata rather than adding task lists to this guide.

### PostgreSQL schema lifecycle

`internal/store/pgschema` owns canonical PostgreSQL catalogs for internal
store qualification. Initial and additional audit enrollment work through the
native preview and commit methods, including complete retained-metadata
validation. Native directory/file creation, content confirmation, replacement,
revert, and rename/move also work, including audited mutations and the required
physical, extraction and photo catalog maintenance. Document imports, source
history, watch and push observations, and collection membership and labels also
work through the native Store methods. Recoverable deletion covers document and
folder trash, whole photo groups, trash browsing and restore, including audited
mutations and photo sidecar decisions. Permanent trash emptying and content-version
pruning preserve retention holds and related catalog state. Derivative cleanup
covers purge previews, rendition and embedding collection, extracted-text and
email cleanup, and explicit rebuild authorization. Primary blob packing,
repacking, bounded GC catalog selection and exact physical cleanup receipts
also work with PostgreSQL metadata. Tags, audited bulk tag edits, and saved query
definition management use the native Store operations on both databases.
Extracted source metadata, typed photo facts, and document-event evidence,
publication, terminal attempts and rebuild receipts also use shared operations.
Original-file rendition jobs include consent, resumable claims, artifact staging
and atomic head publication through the same Store. Document embedding jobs
include reconciliation, consent-bound claims, immutable vector staging, atomic
publication and coverage reads. Email MIME inventories, body renditions,
attachment child documents, and page-render job publication also use shared
Store operations. Supplied media, remote recording references,
original recordings and transcript/caption inputs retain their source, occurrence
and replay receipts through shared native operations. Visual previews, image
embedding inputs and retained email PDFs also use shared native publication
and read paths. Plain-text extraction covers queue discovery, retry scheduling,
terminal results and publication of retained text under rendition authority.
Grouped photo imports include pairing, source replay, browser folder
continuation and import progress.
Photo metadata edits and undo, XMP conflict settlement and sidecar publication
also use shared operations for explicit asset selections.

A public PostgreSQL-backed vault is not available yet: daemon and embedded
operations still open SQLite. Photo-derived processing, email PDF
rendering and other media workers, search,
transfer, backup recovery, multi-store placement and their storage-operation jobs
remain unqualified.

`Setup` takes a caller-owned `database/sql` pool authenticated as `SchemaOwner`
and uses the explicit `Schema` setting. It creates a dedicated schema, canonical
DDL, a UUIDv4 vault identity, a fresh processing-consent incarnation, the primary
blob-store identity and ownership epoch, initial people-state epochs, and the
root directory in one transaction, under a schema-specific PostgreSQL advisory
lock. Repeated setup preserves the identity,
root, primary blob-store identity and epoch, people-state epochs, and existing
data. Existing empty or unrelated schemas, malformed identities and wrong
owners are refused rather than repaired.

The catalogs keep vault references as native UUIDs and content-version and
operation IDs as text, numeric node IDs,
sizes and revisions as `BIGINT`, and timestamps as fixed-width UTC text. This
matches the shared store queries and their lexical timestamp ordering. Both
backends use `schemaformat.TimestampLayout`. PostgreSQL text columns use
`COLLATE "C"` to match SQLite's byte order, including name ordering in directory
pages and comparisons used by cursors. Keep that order on future text sort keys
unless the owning feature explicitly defines a different comparison.
Foreign keys retain node ownership of the current version, version provenance,
and blob references.

Node IDs use a transactional `node_allocation` counter. Setup creates its single
row with high-water mark 1 alongside root node 1. Ordinary inserts increment
that row and insert the node in the caller's storage transaction, under the
existing writer lock. A rollback restores both the node and the counter; deleting
a committed node never lowers the counter. Runtime needs `SELECT` and
`UPDATE(high_water)` on this table, with no insert or delete grant.

This deliberately differs from Kata's native identity sequences. Audit replay
requires each creation to use the previous high-water mark plus one, and its
final check compares that mark with current allocation authority. Native
[PostgreSQL sequences](https://www.postgresql.org/docs/16/functions-sequence.html)
advance across rollback and can skip values during crash recovery, so they cannot satisfy the existing audit format. Keep the format
unchanged. Future import must validate that the exported high-water mark covers
all retained IDs and restore that exact counter in the import transaction;
`MAX(nodes.id)` loses committed IDs deleted before export.

Value validation and immutable-record rules belong to the Go mutation paths;
these tables alone do not provide a complete writable store.

`Validate` only reads. Its runtime role must be separate from the owner, unable
to assume the owner role, and lack schema `CREATE` and identity-table write
privileges. Validation checks the current role and every role the session login
can assume, including indirect memberships. `NOINHERIT` alone does not prevent
`SET ROLE`; PostgreSQL's [role privilege checks](https://www.postgresql.org/docs/16/functions-info.html#FUNCTIONS-INFO-ACCESS-TABLE)
resolve those grants. The operator provisions roles and grants schema `USAGE`
and identity `SELECT` explicitly. Neither function creates roles or changes
grants. `Validate` checks identity and role separation; it does not
qualify the remaining store operations. Callers own connection lifetimes and
deadlines. Schema setup does not grant ownership of blob roots or permit
multiple vault writers.

For the current catalog queries, provision the runtime role separately and
grant the following privileges after setup. This example uses schema
`documents` and an existing role `docbank_runtime`:

```sql
GRANT USAGE ON SCHEMA documents TO docbank_runtime;
GRANT SELECT ON ALL TABLES IN SCHEMA documents TO docbank_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE
    ON documents.nodes, documents.blobs, documents.content_versions
    TO docbank_runtime;
GRANT SELECT, INSERT ON documents.blob_checksums TO docbank_runtime;
GRANT SELECT ON documents.audit_records, documents.audit_authority,
    documents.audit_scopes, documents.audit_baselines, documents.audit_memberships
    TO docbank_runtime;
GRANT INSERT ON documents.audit_records, documents.audit_authority,
    documents.audit_scopes, documents.audit_baselines, documents.audit_memberships
    TO docbank_runtime;
GRANT UPDATE(operation_sequence_high_water, allocation_entry_count, allocation_head)
    ON documents.audit_authority TO docbank_runtime;
GRANT UPDATE(entry_count, chain_head) ON documents.audit_scopes TO docbank_runtime;
GRANT SELECT, UPDATE(high_water) ON documents.node_allocation TO docbank_runtime;
GRANT SELECT, INSERT, DELETE ON documents.vault_changes TO docbank_runtime;
GRANT SELECT, INSERT, DELETE ON documents.photo_xmp_unsettled TO docbank_runtime;
GRANT INSERT, UPDATE, DELETE ON documents.blob_locations TO docbank_runtime;
GRANT INSERT, UPDATE ON documents.blob_pack_entries TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(scan_hash) ON documents.blob_packs TO docbank_runtime;
GRANT INSERT, DELETE ON documents.gc_loose_retirements,
    documents.derivative_pack_purge_pending TO docbank_runtime;
GRANT DELETE ON documents.blob_pack_entries, documents.rendition_blob_staging,
    documents.derivative_blob_purge_pending TO docbank_runtime;
GRANT UPDATE(live_entries, live_stored_bytes, live_raw_bytes,
    max_live_stored_len, max_live_raw_len) ON documents.blob_packs TO docbank_runtime;
GRANT INSERT, UPDATE(next_attempt_at) ON documents.text_extraction_queue TO docbank_runtime;
GRANT INSERT ON documents.text_searchable_versions TO docbank_runtime;
GRANT INSERT, UPDATE(extractor_version, status, error, attempts, text, extracted_at)
    ON documents.extracted_text TO docbank_runtime;
GRANT INSERT, UPDATE(hidden_at, excluded_at, display_file_id,
    display_override_file_id, revision, updated_at)
    ON documents.photo_assets TO docbank_runtime;
GRANT INSERT, DELETE ON documents.photo_change_receipts TO docbank_runtime;
GRANT INSERT, UPDATE(asset_id, role, sidecar_of_file_id, source_provenance_identity,
    revision, rating, flag, label, caption, creator, copyright, rotation,
    confirmed_fields, flag_owner)
    ON documents.photo_files TO docbank_runtime;
GRANT INSERT, DELETE ON documents.photo_timeline TO docbank_runtime;
GRANT INSERT, UPDATE(original_path, original_mtime, folder_key)
    ON documents.photo_source_claims TO docbank_runtime;
GRANT INSERT, DELETE ON documents.ingests TO docbank_runtime;
GRANT INSERT ON documents.provenance, documents.provenance_version_bindings
    TO docbank_runtime;
GRANT INSERT, UPDATE, DELETE ON documents.collection_labels TO docbank_runtime;
GRANT INSERT, UPDATE ON documents.watch_sources, documents.push_sources
    TO docbank_runtime;
GRANT INSERT, DELETE ON documents.watch_source_absences TO docbank_runtime;
GRANT DELETE ON documents.document_event_heads, documents.document_event_generations,
    documents.document_people_heads, documents.document_people TO docbank_runtime;
GRANT INSERT, UPDATE(revision, reason) ON documents.document_event_dirty
    TO docbank_runtime;
GRANT UPDATE(publication_epoch, updated_at)
    ON documents.document_event_state, documents.document_people_state
    TO docbank_runtime;
GRANT DELETE ON documents.rendition_heads, documents.rendition_attachments,
    documents.rendition_job_waiters, documents.email_attachments,
    documents.embedding_jobs, documents.embedding_heads, documents.embedding_sets,
    documents.photo_files TO docbank_runtime;
GRANT DELETE, UPDATE(state, phase, claim_owner, lease_expires_at, claim_epoch,
    available_at, selected_waiter_id, authorization_grant_id,
    authorization_incarnation_id, authorization_revocation_fence,
    provider_started, provider_resume_handle, execution_snapshot_json,
    lexical_generation_id, failure_code, updated_at)
    ON documents.rendition_jobs TO docbank_runtime;
GRANT UPDATE(state, failure_code, updated_at)
    ON documents.rendition_job_waiters TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(root_kind, target_kind, target_id, fencing_token,
    recorded_at, expires_at, active, released_at)
    ON documents.current_rendition_roots TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(name, revision, assignment_count) ON documents.tags TO docbank_runtime;
GRANT INSERT, DELETE ON documents.node_tags TO docbank_runtime;
GRANT INSERT ON documents.batch_tag_receipts TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(name, description, payload, fingerprint, revision, updated_at)
    ON documents.saved_queries TO docbank_runtime;
GRANT UPDATE(binding_epoch) ON documents.document_people_state TO docbank_runtime;
GRANT INSERT ON documents.source_metadata_generations, documents.document_event_generations,
    documents.document_events, documents.document_event_actors, documents.document_event_primaries
    TO docbank_runtime;
GRANT INSERT, UPDATE(generation_id, published_at)
    ON documents.source_metadata_heads TO docbank_runtime;
GRANT INSERT, DELETE ON documents.photo_technical_metadata TO docbank_runtime;
GRANT INSERT, UPDATE(projection_recipe)
    ON documents.photo_technical_metadata_state TO docbank_runtime;
GRANT INSERT, UPDATE(deriver_fingerprint, input_epoch)
    ON documents.document_event_state TO docbank_runtime;
GRANT INSERT, UPDATE(generation_id, input_epoch, published_at)
    ON documents.document_event_heads TO docbank_runtime;
GRANT INSERT, UPDATE(input_epoch, input_revision, inputs_sha256, state, diagnostic_json, attempted_at)
    ON documents.document_event_attempts TO docbank_runtime;
GRANT INSERT, UPDATE(state, scanned, published, failed, unavailable, updated_at, finished_at)
    ON documents.document_event_builds TO docbank_runtime;
GRANT INSERT ON documents.processing_consent_grants, documents.processing_consent_revocations,
    documents.processing_profiles, documents.rendition_builds, documents.rendition_artifacts,
    documents.rendition_units, documents.rendition_lexical_segments,
    documents.rendition_attachments, documents.rendition_jobs, documents.rendition_job_waiters,
    documents.rendition_lexical_heads, documents.rendition_lexical_superseded TO docbank_runtime;
GRANT INSERT ON documents.rendition_blob_staging TO docbank_runtime;
GRANT INSERT, UPDATE(attachment_id, published_at) ON documents.rendition_heads TO docbank_runtime;
GRANT UPDATE(provider_attempts) ON documents.rendition_jobs TO docbank_runtime;
GRANT UPDATE(attachment_id) ON documents.rendition_job_waiters TO docbank_runtime;
GRANT INSERT ON documents.embedding_vector_spaces, documents.embedding_input_generations,
    documents.embedding_generation_inputs, documents.embedding_vector_sets,
    documents.embedding_vector_rows, documents.embedding_sets TO docbank_runtime;
GRANT INSERT, UPDATE(embedding_set_id, vector_space_id, profile_fingerprint,
    published_at, fencing_token) ON documents.embedding_heads TO docbank_runtime;
GRANT INSERT, UPDATE(failure_code, failed_at, fencing_token, attachment_id)
    ON documents.embedding_failures TO docbank_runtime;
GRANT INSERT, UPDATE(state, claim_owner, claim_epoch, claim_count, lease_expires_at,
    available_at, failure_code, receipt_json, updated_at)
    ON documents.embedding_jobs TO docbank_runtime;
GRANT INSERT ON documents.page_documents, documents.page_frames,
    documents.page_recipes, documents.page_images, documents.email_generations,
    documents.email_part_artifacts, documents.email_attachments,
    documents.email_body_results, documents.email_document_publications,
    documents.email_document_relations TO docbank_runtime;
GRANT INSERT, UPDATE(state, results_json, failure_code, epoch, token, updated_at)
    ON documents.page_render_jobs TO docbank_runtime;
GRANT INSERT, UPDATE(attachment_id, published_at)
    ON documents.email_heads TO docbank_runtime;
GRANT DELETE ON documents.email_document_publications TO docbank_runtime;
GRANT INSERT ON documents.media_sources, documents.media_source_versions,
    documents.media_input_artifacts, documents.media_occurrences,
    documents.media_operations TO docbank_runtime;
GRANT UPDATE(source_version_id, visible, revoked_at)
    ON documents.media_occurrences TO docbank_runtime;
GRANT UPDATE(receipt_json, updated_at) ON documents.media_operations TO docbank_runtime;
GRANT INSERT ON documents.photo_hidden_credentials TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(state, cursor, total_objects, completed_objects,
    copied_objects, copied_bytes, cancel_requested, error, receipt_json,
    updated_at, finished_at, retention_until)
    ON documents.storage_operations TO docbank_runtime;
#### Photo checkouts

The shared Store can select and freeze checkout files, record their publication,
inspect working copies, commit edited content, rebind a destination and retire a
checkout on either backend. Selection supports photo IDs, existing albums, year
ranges and the whole library. It uses the existing display, visibility and
capture-date rules without executing a general search query. Combined selectors
deduplicate photos; hidden, excluded and trashed photos remain unavailable.

Every checkout write uses writer admission. Publication commits its recovery
intent before invoking the filesystem callback, then records completion in a
separate transaction. The callback runs once. Cancellation after publication
still permits completion recording. If recording fails, the committed intent
keeps the source version retained until recovery or retirement; no database
rollback can undo the external copy.

Commit-back rechecks the frozen version, file pairing, destination identity and
operation. Content, provenance, its replay receipt, the advanced checkout base
and progress counters commit together. Retrying a completed entry preserves
the same content and counters. Admission reads its receipt and authority from
one snapshot. Observation batches and destination binding also roll back as a
unit. Retiring a checkout releases its retained versions without deleting copies.

The `photo-checkout` contracts exercise native operations, late publication and
commit failures, batched observation rollback, snapshots and retained-version
holds. Existing albums are inputs to selection; this does not qualify album
editing. Filesystem workers, portable metadata import and restore, general photo
query selection and public PostgreSQL vault opening remain separate.

