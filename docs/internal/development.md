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

Make targets, CI, and the container image compile Go with `-trimpath`, so
binaries record module-relative source paths instead of absolute ones. Without
it, Go caches every package separately for each checkout directory, and each
git worktree grows the build cache by another full copy. With it,
identical source in any checkout reuses the same cache entries.

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
- `TestReleasedSchemaFixturesMatchVersions` requires a bump when a released table,
  trigger, view, or index changes, or when a unique index is added.
  New non-unique indexes need no bump because startup creates them in place.
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
also use shared operations for explicit asset selections. Quality scores, hidden-photo
sessions and photo settings have native Store contracts as well.

A public PostgreSQL-backed vault is not available yet: daemon and embedded
operations still open SQLite. Photo-derived processing, email PDF rendering,
other media workers, search and physical backup recovery remain unqualified.
Native multi-store catalog operations are qualified below; deployment adapters
and filesystem workers remain separate from public PostgreSQL vault opening.

`Setup` takes a caller-owned `database/sql` pool authenticated as `SchemaOwner`
and uses the explicit `Schema` setting. The database must use `UTF8` encoding;
both setup and runtime validation check `server_encoding` and reject any other
value before accessing the vault schema. This keeps Unicode name matching
consistent with SQLite. A different client encoding does not satisfy this
requirement: provision a UTF-8 database before installing the schema.

Setup creates a dedicated schema, canonical
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
GRANT INSERT, DELETE, UPDATE(lifecycle) ON documents.blob_stores TO docbank_runtime;
GRANT INSERT ON documents.storage_operation_stores TO docbank_runtime;
GRANT INSERT, DELETE ON documents.storage_operation_cleanup TO docbank_runtime;
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
GRANT INSERT ON documents.collection_snapshots,
    documents.collection_snapshot_members, documents.collection_snapshot_representations,
    documents.packages, documents.package_volumes, documents.bates_namespaces,
    documents.bates_namespace_cursors, documents.bates_allocations,
    documents.bates_page_labels, documents.bates_artifacts, documents.bates_artifact_pages
    TO docbank_runtime;
GRANT UPDATE(next_sequence) ON documents.bates_namespace_cursors TO docbank_runtime;
GRANT UPDATE(state, committed_at) ON documents.bates_allocations TO docbank_runtime;
GRANT INSERT, DELETE ON documents.package_preflights TO docbank_runtime;
GRANT INSERT ON documents.package_records, documents.package_labels,
    documents.package_import_receipts, documents.package_import_jobs TO docbank_runtime;
GRANT UPDATE(state, snapshot_id, completed_at) ON documents.packages TO docbank_runtime;
GRANT UPDATE(state, epoch, token, claim_owner, lease_expires_at, updated_at)
    ON documents.package_import_jobs TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(canonical_json, state, expires_at)
    ON documents.export_sources TO docbank_runtime;
GRANT INSERT ON documents.export_chunks, documents.export_members,
    documents.export_documents, documents.export_role_roots TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(canonical_json, expires_at)
    ON documents.export_plans TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(canonical_json, state, expires_at, epoch, token, archive_name)
    ON documents.export_jobs TO docbank_runtime;
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
    display_override_file_id, representative_rank, revision, updated_at)
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
GRANT INSERT, DELETE, UPDATE(display_name, display_name_folded, state, revision,
    updated_at, key_face_id) ON documents.persons TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(person_id) ON documents.person_identities TO docbank_runtime;
GRANT INSERT, UPDATE(person_id, uid_kind, uid_state, last_seen_revision,
    display_name_snapshot, updated_at) ON documents.person_external_identities TO docbank_runtime;
GRANT INSERT, UPDATE(surviving_uid, observed_at)
    ON documents.person_external_uid_aliases TO docbank_runtime;
GRANT INSERT, UPDATE(surviving_person_id, reason, retired_at)
    ON documents.person_aliases TO docbank_runtime;
GRANT INSERT ON documents.person_merges, documents.person_splits TO docbank_runtime;
GRANT INSERT, UPDATE(person_id, raw_label, raw_label_folded, basis, source_ref,
    revision, recorded_at, retired_at) ON documents.custodian_assignments TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(person_id, action, note, revision, photo_mark)
    ON documents.person_document_assertions TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(suggested_person_id, state, decided_person_id, decided_at, revision)
    ON documents.person_match_candidates TO docbank_runtime;
GRANT INSERT, DELETE ON documents.person_candidate_faces TO docbank_runtime;
GRANT UPDATE(assertion_id) ON documents.photo_faces TO docbank_runtime;
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
GRANT INSERT ON documents.document_people_generations, documents.document_people TO docbank_runtime;
GRANT INSERT, UPDATE(event_generation_id, inputs_sha256, generation_id, unresolved_actors,
    suppressed_actors, candidate_overflow, resolver_fingerprint, binding_epoch, node_revision,
    edge_count, state, failure_reason, published_at)
    ON documents.document_people_heads TO docbank_runtime;
GRANT UPDATE(resolver_fingerprint) ON documents.document_people_state TO docbank_runtime;
GRANT INSERT, UPDATE(state, scanned, published, failed, updated_at, finished_at)
    ON documents.document_people_builds TO docbank_runtime;
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
GRANT INSERT, DELETE, UPDATE(passcode_hash)
    ON documents.photo_hidden_credentials TO docbank_runtime;
GRANT INSERT, DELETE ON documents.photo_hidden_failures TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(locked_until)
    ON documents.photo_hidden_lockout TO docbank_runtime;
GRANT INSERT, UPDATE(preference, revision, updated_at, burst_gap_seconds,
    backup_interval_seconds, backup_keep_last, hidden_unlock_lifetime_seconds,
    representative_sequence)
    ON documents.photo_library_settings TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(state, focus, blur, brightness, color_red,
    color_green, color_blue, framing, aesthetics, face_reports_json, face_inputs)
    ON documents.photo_quality_signals TO docbank_runtime;
GRANT INSERT, UPDATE(name, starred, revision, cover_asset_id, updated_at, deleted_at)
    ON documents.photo_sets TO docbank_runtime;
GRANT INSERT, DELETE ON documents.photo_set_members TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(state, cursor, total_objects, completed_objects,
    copied_objects, copied_bytes, cancel_requested, error, receipt_json,
    updated_at, finished_at, retention_until)
    ON documents.storage_operations TO docbank_runtime;
GRANT INSERT ON documents.photo_description_explicit_waiters,
  documents.photo_face_scans TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(producer_fingerprint, caption, tags_json, visible_text,
  rotation, dismissed, awaiting, caption_resolved, resolved_tags_json)
  ON documents.photo_suggestions TO docbank_runtime;
GRANT INSERT, DELETE, UPDATE(x, y, width, height, roll, facing, eyes_open, smile,
  detector_fingerprint, revision) ON documents.photo_faces TO docbank_runtime;
GRANT INSERT, UPDATE(last_operation_id, destination, destination_identity, state,
    destination_reserved, error, retention_released, retired_at, retirement_requested)
    ON documents.photo_checkouts TO docbank_runtime;
GRANT INSERT, UPDATE(published, base_held, observed_identity, observed_hash,
    observed_size, observed_mtime, observed_since, working_state, version_id,
    blob_hash, size) ON documents.photo_checkout_entries TO docbank_runtime;
GRANT INSERT ON documents.visual_preview_generations TO docbank_runtime;
GRANT INSERT, UPDATE(generation_id, published_at)
    ON documents.visual_preview_heads TO docbank_runtime;
GRANT DELETE ON documents.embedding_input_generations, documents.embedding_vector_sets,
    documents.embedding_vector_spaces, documents.embedding_failures,
    documents.email_heads, documents.email_part_artifacts, documents.email_generations,
    documents.email_body_results, documents.rendition_builds, documents.rendition_artifacts,
    documents.rendition_units, documents.rendition_lexical_segments,
    documents.rendition_lexical_index, documents.rendition_lexical_heads,
    documents.rendition_lexical_generations, documents.rendition_lexical_generation_manifests,
    documents.rendition_lexical_stream_builds,
    documents.text_searchable_versions, documents.extracted_text,
    documents.text_extraction_queue TO docbank_runtime;
GRANT INSERT ON documents.derivative_blob_purge_pending,
    documents.rendition_lexical_generations, documents.rendition_lexical_stream_builds,
    documents.rendition_lexical_generation_manifests, documents.rendition_lexical_index
    TO docbank_runtime;
GRANT UPDATE(generation_id) ON documents.rendition_lexical_heads TO docbank_runtime;
GRANT INSERT, UPDATE(purged_at, active, superseded_at, superseding_build_id)
    ON documents.derivative_purge_suppressions TO docbank_runtime;
```

Checksum rows are inserted or compared with the existing value, never updated;
deleting the owning blob cascades to its checksum. Pin every runtime connection's
`search_path` to `documents,pg_catalog`, substituting the configured schema. The
pgx connection config uses `RuntimeParams["search_path"]` for this setting;
setting it on one borrowed connection does not configure the pool. Keep schema
ownership, `CREATE`, and identity-table writes with the setup role. These grants
qualify the current queries; they do not enable a public PostgreSQL vault or
replace validation of the complete runtime catalog contract.

Neither the session login nor the selected role may belong to the schema owner
role, directly or indirectly. This ownership check is separate from object
privileges: an owner can revoke its own write privileges and still retain the
authority to alter objects or grant those privileges again.

PostgreSQL schemas are disposable until the first deployment whose data must
survive upgrades. An ordinary Docbank release does not establish that boundary:
the public vault lifecycle still opens only SQLite. Edit
`internal/store/pgschema/schema.sql` directly; no manual version bump or migration
is needed during this period. The build hashes the exact embedded SQL template
with SHA-256 once at package initialization, before substituting the vault schema
name. `Setup` records this fingerprint in `vault_metadata` in the installation
transaction. Both `Setup` and runtime `Validate` reject a missing or different
fingerprint with a "recreate this schema" error; neither repairs it. Any SQL-file
change, including comments or formatting, changes the fingerprint. Existing
development schemas with the older integer version are also refused. Recreate
only disposable development schemas with the current build, never retained data.
Go-written seed rows in `Setup` are outside the fingerprint; an incompatible seed
change must also change the SQL template so existing development schemas are refused.

Before the first retained deployment, establish the current schema as migration
1 (`000001_initial_schema.up.sql`) and add a CI check that installs the numbered
chain and compares it with the canonical schema. This is the PostgreSQL upgrade
floor, independent of SQLite layout versions and JSONL format versions. From
that cutoff onward, every change to the schema needs a numbered
`NNNNNN_description.up.sql` transition, including indexes or defaults added while
porting an operation. A port that changes only Go or queries needs none. Keep at
most one new migration per PR; revise it while it remains branch-only and has
been applied only to disposable databases.

Keep one complete forward chain, and commit each transition and its version
together under the setup lock. Runtime startup never migrates. Once a migration
reaches main or a deployed database it is immutable; fix forward. Recovery uses
a pre-upgrade backup and compatible binary, not down migrations. This discipline
does not change SQLite's JSONL cutover policy. The migration chain and its CI
comparison must land before retained PostgreSQL deployments are supported.

Do not add SQL `CHECK` constraints. Value validation belongs in Go and must be
shared by writes, imports, restores and audits. Primary keys, foreign keys,
nullability and uniqueness describe structural relationships. Existing released
SQLite constraints are compatibility commitments; this rule does not authorize
removing them from an immutable schema or migration.

The `audit_record_single_genesis` partial unique index is the intentional
exception to the closed-list rule: its three-kind predicate preserves one
record per genesis kind, matching SQLite; it does not reject other kinds.
Go still owns which record kinds are valid. Conditional unique indexes also
preserve alias, current external identity and active custodian relationships.
These predicates enforce uniqueness; they do not reject unknown kind or state
values. Go validators own those values. Multi-store operations have native
contracts below; their active-evacuation uniqueness rule is enforced by the shared Go admission
check under writer serialization, without a closed state list in PostgreSQL.

Secondary access indexes on the added read-only catalogs are intentionally
deferred. Port them with the operations that need them, including indexes on
foreign-key columns used by cascading deletes; PostgreSQL does not add those
automatically. Keep structural uniqueness in the canonical schema. Enrollment
qualification establishes correctness, not validation or cascade performance
on large vaults; those paths need representative query-plan and workload checks.

#### Sealed delivery catalogs

The native Store seals inline and streaming collection snapshots, records package
identity and volumes, retains preflight results, reserves Bates page numbers,
and publishes verified numbered PDF artifacts on both backends. These operations
start with explicit, already-selected versions. Received-package intake is
qualified below. Query-driven selection, filesystem delivery workers,
transfer/recovery and public PostgreSQL vault opening remain separate.

A sealed snapshot keeps its exact versions, source collections, page selections,
representations and canonical bytes. Replaying the same request returns the
original header; a different request cannot overwrite it. Streaming writes bounded
member batches and writes the immutable header last. SQLite uses its existing
foreign-key deferral pragma. PostgreSQL makes the member-to-header reference
`DEFERRABLE INITIALLY IMMEDIATE` and defers it for the streaming transaction.
Failure leaves neither a partial header nor member rows. The canonical JSON stays
byte storage so hashing and readback preserve the exact bytes; PostgreSQL decodes
UTF-8 only where a query reads frozen source-collection evidence.

Bates previews allocate nothing. Reservations serialize under the existing writer
lock, preserve namespace cursors and globally unique labels, and commit the sealed
snapshot and range together. Semantic replay can reuse the original reservation
after its live source changes. Publication commits the output blob, immutable
artifact and page receipts, and allocation transition together. The first verified
publisher wins an exact retry, including when a later render has different PDF
container bytes. Runtime roles can insert identities, advance `next_sequence`,
and change only `state` and `committed_at` on allocations; they cannot rewrite
snapshot, namespace, label, artifact or volume identities. Store methods enforce
cursor monotonicity and one-way allocation publication under writer admission.
The PostgreSQL column grants do not enforce those transitions against direct SQL.

Package and artifact pages, member representations, publication plans and custodian
evidence use repeatable-read transactions. A concurrent custodian change affects
the next request, including its binding epoch. Custodian evidence follows the
snapshot's frozen source membership after the source leaves the live library.
Preflight reads enforce their deadline even before the expiry sweep removes rows.

The `sealed-delivery` contracts run existing snapshot, streaming, package and Bates
scenarios on both backends. PostgreSQL cases exercise late-write rollback during
reservation and publication, concurrent connections sharing one namespace, and
custodian changes committed between statements in a read. Access indexes cover
artifact history and labels, namespace ranges, package predecessors, preflight
expiry and retained representation versions.

#### Received-package intake

The native Store admits received packages and their source collections, stages
files under worker leases, publishes immutable source rows and scoped labels,
and completes or cancels imports on both backends. Filesystem parsing, import
workers, portable metadata recovery and public PostgreSQL opening
remain separate qualification boundaries.

Admission commits the source collection, package identity, volume mappings and
queued job together. An exact owner/operation retry returns the original job,
including when the caller allocated new internal IDs. A worker claim, renewal,
release or reclaim uses the existing writer transaction. Replacement advances
the epoch; an expired, cancelled or superseded worker cannot stage files,
publish receipts or finalize the package.

Record publication binds the retained version to that package's source collection.
The original row bytes, sensitivity flag, received labels and receipt commit
together. Exact replay preserves the original receipt; changed rows or received
labels conflict. Assigned labels have their own immutable identity and do not
alter received evidence. Label lookup preserves ambiguity across packages;
label, record and timeline pages retain their existing ordering and cursors.
Timeline pages and progress counts with their bounded gap list share one read
snapshot.

Completion seals the member stream and advances the package and job together.
Only members matching committed record receipts may enter the final snapshot.
Cancellation, terminal failure and owner revocation remove unreceipted staging,
while preserving primary versions, supplied representations retained by receipts,
and checkout-held versions. Cleanup updates tag counts, photo membership and
export work, version-dependent epochs and the vault change stream in the same
transaction. Released SQLite triggers keep their existing effects; PostgreSQL
uses the shared maintenance helpers.

The `received-packages` contracts exercise native intake, lease replacement,
record replay, gap reporting, completion, label browsing and cleanup on both
engines. PostgreSQL cases fail the final admission, receipt or job write to
verify rollback, and commit new receipts during reads to verify snapshot
isolation. The PostgreSQL runtime grants are insert-only for records, labels
and receipts. Updates are limited to job lifecycle columns and package state,
snapshot and completion columns. Store methods enforce one-time snapshot and
completion values under writer admission; the PostgreSQL grants do not prevent
direct SQL from rewriting those columns. SQLite's released receipt trigger
prevents updates but does not prevent deletion. These database-level differences
do not change the qualified Store operations.

#### Document export plans and jobs

The native Store supports explicit members, current node IDs and chunked uploads
as export sources on both backends. It seals exact retained versions, plans
originals and available retained text, page images, email PDFs and attachment
outputs, and provides frozen previews and output-problem pages. Explicit photo
members can capture authored metadata and publish an already-prepared render.
Query-driven and mailbox source discovery, rendering and filesystem workers,
portable recovery and public PostgreSQL opening remain separate boundaries.

Source admission reserves the owner and operation before invoking a resolver.
A failed or cancelled resolution keeps that reservation, so retry cannot silently
select newer documents. Chunk retries must match the original bytes; sealing
requires every chunk and the promised member digest. Plans pin exact output
receipts, paths, optional-role failures, volume choices and retention roots.
A failed plan commits no quota reservation, document rows or roots.

Job claims and progress use the existing writer admission. Restart and cancellation
advance the claim epoch and clear its token; old attempts cannot publish. A
successful finish stores the terminal receipt and archive name together and
extends source and plan retention. Cancellation and failure reduce the extension.
Expired authority keeps its versions and blobs until cleanup removes its rows.
Archive release checks the owner and terminal state before calling the existing
removal callback; a callback failure keeps the job. Archive removal must tolerate
retries because SQL rollback cannot restore a removed file.

Plans, frozen previews, problem pages, photo inputs and live output/attachment
discovery use read snapshots where multiple rows must agree. Archive document
walks release each page before callbacks so blob reads and claim checks can
re-enter the Store. Cleanup during a read affects the next request. PostgreSQL decodes stored
JSON bytes only for concrete JSON predicates and uses `GREATEST` for scalar
retention comparisons; SQLite keeps its existing expressions. Both retain the
same canonical bytes and fingerprints.

The `exports` contracts cover native source sealing, chunk replay, pinned roles,
plan rollback, job completion and release, restart/revocation, cleanup and output
inspection. PostgreSQL-specific cases revoke a final write to verify rollback,
remove expired plans between read statements, and recheck explicit photo inputs
before publication. Runtime grants permit only inserts into members, chunks,
planned documents and roots; source, plan and job updates are limited to their
existing mutable columns. These grants do not replace Store lifecycle checks.

#### Logical metadata transfer and snapshots

The internal Store exports deterministic logical metadata on either backend
through `ExportMetadata`. `BeginMetadataSnapshot` pins the database view before
returning; its `Export`, `ExportBackup` and blob-authority queries keep that same
view while writers continue. PostgreSQL uses a read-only `REPEATABLE READ`
transaction from the runtime pool. Closing the snapshot releases its connection
and leaves the pool open. SQLite keeps its dedicated deferred connection so a
backup does not take its writer lock.

Both streams use the existing metadata JSONL format, validators and dependency
order. The header carries the vault UUID and transactional node allocation
high-water mark, including IDs whose nodes were deleted. `ExportBackup` includes
only the retained document and artifact blob closure; uncommitted provider
staging is excluded. Full export keeps the complete logical blob catalog.
Neither stream contains physical pack placements or rebuildable search indexes.
An export validates the retained metadata before writing its header. Treat a
destination error as a failed export and discard the partial output.

Logical metadata can move between PostgreSQL and SQLite without changing the
format. Use `ImportMetadata` for a pristine SQLite Store. For PostgreSQL, use
`store.ImportPostgresMetadata(ctx, ownerPool, cfg, reader)` with
`pgschema.Config{Schema: "documents", SchemaOwner: "docbank_owner"}` and a pool
connected as that schema owner. The internal import entry point runs `Setup`,
refuses a nonpristine target, and uses the existing storage writer transaction.
It selects the schema through `SET LOCAL search_path`; the caller's pool stays
open with its original session settings. Runtime credentials cannot perform
setup or change the vault identity, and need no additional grants.

Import restores the source vault UUID, node/version identities, allocation
high-water mark, retained processing records and canonical audit history. It
keeps the fresh target's processing incarnation, so restored consent records
cannot authorize a provider call until the operator grants consent again.
`VerifyAudit` independently replays the restored history and can compare
previously recorded evidence on either backend.

Provenance strings, person names, extracted text and caller-supplied occurrence
attributes retain U+0000 across logical transfer and native operations.
PostgreSQL stores unrestricted provenance strings, person names and retained
name snapshots, extracted text/error strings and retained rendition text, and
caller-supplied media occurrence attributes as `BYTEA`. The metadata codec marks these fields with
`db:"column,bytes"`; its existing import plan binds raw bytes on PostgreSQL.
Native writes use explicit bindings. Shared readers scan the columns back into
strings, and SQLite retains its released `TEXT` columns. Wire strings,
provenance identities, metadata exports and audit records keep their original
bytes. JSON documents remain JSON text with escaped control characters.

Source classification inspects only the nine-byte `embedded:` prefix,
case-insensitively, without decoding the full kind into PostgreSQL text. Exact
source-path lookups bind bytes directly against the existing index. JSON receipt
paths use separate text parameters from binary path lookups.

Person-name ordering and cursors compare the original folded-name bytes.
NUL-free names retain the PostgreSQL Unicode word-prefix regexp. For a name or
prefix containing NUL, the query finds exact byte occurrences and checks the
preceding Unicode rune; it never decodes a NUL-bearing value as PostgreSQL text
or filters a bounded page after reading it. Recent-name exclusions use a native
`bytea[]` parameter. These differences add no database round trip or storage
escaping layer.

PostgreSQL foreign keys that normally check immediately allow explicit deferral.
Logical import and collection snapshot sealing, including received packages,
defer those keys until validation or commit. Other writes retain their existing
check timing. The complete import stream is validated before commit, including
foreign keys and audit replay. Interrupted streams or failed SQL roll back the
logical rows, identity and node allocator.
A failed first import can leave the pristine schema created by `Setup`; retry
against that same target. Never overwrite or recreate a retained destination.

SQLite rebuilds its extracted-text FTS projection after all records are loaded.
PostgreSQL restores its retained lexical rows and rebuilds photo timelines, XMP
export work and candidate-to-face membership in the import transaction. Photo
rebuilding reads bounded pages. Hidden-photo failure records keep their IDs and
their next insertion position without relaxing the runtime identity column.

This is logical recovery. Before publishing a restored vault, restore and verify
all blob bytes and physical pack authority separately. A metadata stream alone
cannot recover those bytes. Filesystem backup/restore and public PostgreSQL
vault opening remain separate boundaries.

The `metadata-export` and `metadata-import` contracts cover pinned snapshots,
native audited transfer, retained-catalog round trips, backup blob selection,
failed streams and destinations, late database failures, runtime refusal and
fresh processing consent. PostgreSQL imports are read through the runtime role
and exported back through the existing SQLite importer. Retained fixtures
qualify the transfer, not their producers. Runtime SELECT grants cover export;
import uses the separate schema-owner credentials. The schema fingerprint
changes with deferrable references; recreate stale disposable test schemas.

#### PostgreSQL reference implementations

Start with AgentsView's [backend parity rules][av-storage], shared
[`readbase.Catalog`][av-catalog], and [`QueryDialect`][av-dialect]. They show how
to share operation semantics while keeping SQL syntax and typed loaders with
the backend. Then compare msgvault's native archive and Kata's lifecycle code.
The references below identify concrete implementations to reuse as patterns;
their `internal` packages are not importable shared libraries.

| Reference | Shared contract | Deliberate difference |
| --- | --- | --- |
| AgentsView [catalog adapter][av-pg-catalog] and [query dialect][av-dialect] | Shared filtering, pagination and result assembly; explicit backend SQL and typed scanning | Docbank is an authoritative vault, not a disposable mirror. Keep its existing `Store` and mutation helpers; no replica registry or second implementation of audit and publication |
| AgentsView [pool opening][av-connect] and [hosted validation][av-hosted] | Configure every pooled connection, validate effective schema at opening, and account for connection reuse | Its tenant/RLS schema is a separate design, not Docbank's permission model. Do not copy its CHECK constraints, tenant tables or triggers |
| [msgvault embedded archive][mv-archive] | pgx through `database/sql`; separate setup and validation-only opening; `current_schema()` check | Docbank serializes setup itself and requires a dedicated schema; it does not copy ambient-schema defaults or msgvault's migration ledger |
| [Kata PostgreSQL operations][kata-operations] | `Schema`/`SchemaOwner` naming, owner/runtime role separation, transaction advisory lock | This internal API accepts a pool and has separate setup/validate calls; schema fingerprints reject stale disposable schemas; numbered migrations start before retained deployments, and a public vault is not exposed yet |
| [Kata numbered migrations][kata-migrations] and [sequence export][kata-sequences] | Canonical fresh DDL, immutable forward SQL, atomic version stamps, and preservation of allocated IDs through transfer | Version history belongs to Docbank; its audit format requires a transactional node counter rather than a native identity sequence |
| AgentsView [behavior parity][av-parity] and [pool tests][av-pool-tests]; msgvault and Kata PostgreSQL lanes | Exercise real backends and their public operations, including errors, filtering and reused connections | Keep Docbank's canonical setup fixture, separate runtime role, PostgreSQL 16/18 lanes and both SQLite builds; no new fixture schema or per-test container |

Keep validation, result bounds, ordering and domain error meanings in the
existing operation owner. Use shared SQL where it already works. Add a narrow
backend-specific operation only for a demonstrated difference, such as a
sequence query or typed SQLSTATE classification; msgvault's
[`Dialect`][mv-dialect] separates these concerns. Do not rewrite SQL with a new
general-purpose placeholder parser or copy the entire dialect interface.

AgentsView's [curation retries][av-retries] and Kata's
[transaction helper][kata-transactions] retry complete SQL transactions whose
effects roll back together. This is not permission to retry Docbank mutation
callbacks: filesystem publication and external work do not roll back with SQL.
Keep the transaction and publication contract below. Multi-statement reads
that need one view of data require an explicit snapshot, not the server's
default isolation level.

Use the existing Kit `search/sqlquery`, `search/postgres`, `search/lexical`,
`search/hybrid` and `vector` packages before adding search machinery. Their
presence does not establish Docbank search parity: retained-version eligibility,
rendition selection, counts and cursors still belong to Docbank. Keep relevance
differences explicit; do not promise identical FTS5 and PostgreSQL scores.

[av-storage]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/docs/agents/storage.md#backend-parity
[av-catalog]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/internal/readbase/catalog.go
[av-dialect]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/internal/db/query_dialect.go
[av-pg-catalog]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/internal/postgres/catalog.go
[av-connect]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/internal/postgres/connect.go
[av-hosted]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/internal/postgres/hosted_tenant.go
[av-parity]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/internal/postgres/search_content_parity_pg_test.go
[av-pool-tests]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/internal/postgres/hosted_tenant_pgtest_test.go
[av-retries]: https://github.com/kenn-io/agentsview/blob/666b82857ec5458590396cca83401ad96fdf063b/internal/postgres/curation_retry.go
[mv-archive]: https://github.com/kenn-io/msgvault/blob/0700dc36231fa7e02e56269141eaed0665e69742/pkg/archive/archive.go
[mv-dialect]: https://github.com/kenn-io/msgvault/blob/0700dc36231fa7e02e56269141eaed0665e69742/internal/store/dialect.go
[kata-operations]: https://github.com/kenn-io/kata/blob/0d49cbca0aaec5e09f5ff8edeac374966181f630/docs/operations/postgres.md
[kata-migrations]: https://github.com/kenn-io/kata/blob/0d49cbca0aaec5e09f5ff8edeac374966181f630/docs/development/postgres-migrations.md
[kata-sequences]: https://github.com/kenn-io/kata/blob/0d49cbca0aaec5e09f5ff8edeac374966181f630/internal/db/pgstore/export.go
[kata-transactions]: https://github.com/kenn-io/kata/blob/0d49cbca0aaec5e09f5ff8edeac374966181f630/internal/db/pgstore/transaction.go

#### Storage transactions and checksum recording

The internal PostgreSQL store serializes storage mutations with a
transaction-scoped advisory lock, following Kata's lock pattern. Its key is
derived from `docbank:storage:` and the configured schema; PostgreSQL keeps
these locks within one database. Different vault schemas can write independently.
The pool's pinned search path and the store's schema binding must agree.

`withStorageTx` explicitly uses `READ COMMITTED` on PostgreSQL and acquires the
writer lock before calling the mutation. Protected rows are read in later
statements, so a writer admitted after another commits sees that commit even
when the server defaults to repeatable read. SQLite keeps its immediate
transaction on the single-connection writer pool. This serializes metadata
changes; it does not replace the exclusive vault and blob-root ownership.

Cancellation reaches connection acquisition, `BEGIN`, and the PostgreSQL lock
wait. After the cancellation handoff from `BEGIN`, the store owns rollback and
its errors. If cancellation races a successful `BEGIN`, `database/sql` can
start its own rollback first. The store then receives `sql.ErrTxDone` without
that rollback's error. Closing the dedicated connection still waits for the
rollback to finish before the store returns cancellation. Callback and commit
failures propagate to the caller; the store never retries a mutation callback.

Subscriber notifications follow successful commit only. PostgreSQL
conservatively signals every storage commit; SQLite's temporary capture
triggers narrow that signal to changes in tables Photos reads. Unread signals
still coalesce.

`RecordVerifiedBlobChecksum` and `BlobChecksums` use shared SQL on both backends.
Recording requires an existing blob, accepts an identical checksum again, and
rejects a conflicting checksum without changing the retained value. The runtime
role needs only `SELECT` and `INSERT` on `blob_checksums`. SHA-256 remains the
blob identity; MD5 is auxiliary metadata. This qualifies checksum recording and
readback. Native document writes also record physical receipts as described
below. Checksum backfill remains unqualified.

!!! info "Planned"

    Runtime opening must reject a connection whose `current_schema()` differs
    from the configured vault schema. Each pool must pin that same search path,
    so writers to one schema use the same advisory lock key.

    Photos checkout writes currently rely on SQLite's single-connection writer
    pool. Their PostgreSQL implementation must use `withStorageTx` for every
    metadata write. Publication intent must commit before the filesystem copy;
    recording its result or clearing an unused intent needs a separate
    transaction that survives request cancellation. A database transaction
    must not span the filesystem publication callback.

#### Catalog maintenance during writes

Shared Go operations own document mutations on both backends. PostgreSQL's
`document_maintenance.go` performs their related catalog changes inside the
same SQL transaction. Released SQLite triggers remain the only owner of those
effects on SQLite. Subscriber notifications occur only after commit.

For qualified document writes, insertion of a version clears rendition staging
and pending derivative collection for its blob. Photo enrollment and content
changes recompute source claims and the timeline immediately; merely leaving
rows in a pending queue would not match SQLite. Revision, name and parent
changes refresh photo export work, including both ends of an unowned sibling
sidecar move. Reintroducing a blob updates summaries of surviving pack mappings
and removes its old primary mapping when a new loose receipt replaces it.

Permanent version deletion advances people-binding and event-publication epochs
before their owning rows disappear through cascades. Pruning refreshes the
surviving photo's source claims, timeline and export work after deletion. Trash
emptying collects affected photo identities before removing memberships and
receipts, repairs the surviving assets, and refreshes paired sidecar work after
node deletion. These effects share the deletion transaction; a failure retains
the original versions, jobs, receipts, epochs and node revisions.

`make backend-coverage` reports the recorded SQLite catalog effects,
their PostgreSQL owners and their remaining boundaries. Audit enrollment already
queues affected photos through `queueAuditPhotosTx`. Logical import requires a
pristine target, so it never deletes retained authority or memberships. Candidate
insertion writes its face projection in the same transaction; import rebuilds
that projection after loading the evidence. No supported writer replaces
candidate evidence or source heads during recovery.
Search still has unported effects; retained tables do not establish search support.

For each remaining port, capture affected identities before cascades remove the
rows that identify them. Validate values and immutable-record rules in the Go
write path, including imports and restores, and verify commit and rollback
through the native operation. PostgreSQL does not use maintenance triggers.

Preserve released SQLite schemas and their triggers. Replace each trigger
through the supported SQLite layout cutover only when its Go owner is present.
Do not run both owners for an effect such as advancing a binding epoch.
Engine-specific index maintenance, such as SQLite FTS5, remains part of that
engine's search implementation.

#### Native document writes

Directory creation (including exact paths and recursive creation), file creation,
content confirmation, replacement, revert, and rename/move use the existing
Store operations on both engines. They preserve revisions, immutable version
history, exact physical receipts and audit records. PostgreSQL unique violations
map to the same domain errors as SQLite. A rejected creation rolls back the node
counter, blob publication and every related catalog change together.

Creating a supported text version records extraction eligibility and work without
bypassing derivative-purge suppression. This qualifies scheduling, not extraction
workers or PostgreSQL search. Creating a photo enrolls its singleton asset under
the existing policy; active audit authority still disables automatic enrollment.
Changing current bytes refreshes retained provenance and technical date
projections, while reverting selects the retained version's bytes only if the
physical catalog still authorizes them.

The operation tests use canonical setup and the runtime grants above. They
create and change ordinary and audited documents, reject conflicting writes,
and validate the complete resulting metadata and audit chain. Photo maintenance
is compared with the released SQLite triggers using retained provenance,
technical metadata and sidecar receipts. Physical cases cover compressed
receipts, missing authority and dead pack mappings surviving logical collection.
These are internal Store capabilities; they do not expose a public PostgreSQL
vault or qualify the remaining storage and recovery lifecycle.

#### Source metadata and document events

Publishing source metadata atomically records immutable evidence, its photo
technical projection, and the selected generation. Replaying an extractor's
identical evidence selects that recorded generation; conflicting bytes fail.
A changed head invalidates event and people projections for every retained
version of the source bytes. Republishing the unchanged head does not dirty
those versions again.

PostgreSQL refreshes the affected photo timelines and export queue in that same
transaction. It visits each photo and asset once even when retained versions
share the source bytes. Rebuilding technical projections from retained evidence
refreshes affected photos after the full replacement. SQLite retains its
existing triggers, including FTS maintenance; PostgreSQL search remains
unqualified.

Document-event publication captures source and provenance evidence, then checks
its digest, recipe, epoch and dirty revision under the writer lock. A stale
result cannot become a head or a terminal attempt. Generation bytes, events,
actors and primary selections are inserted together and cannot be updated by
the runtime role. Head selection and terminal-attempt recording share the same
transaction. Replaying the current publication preserves its epoch; failed
writes retain the previous evidence and progress.

Coverage, missing-work pages, metadata views and event evidence use repeatable-read
snapshots when several queries assemble one result. Coverage uses the indexed
event and primary-selection rows instead of expanding canonical JSON. Canonical
source payloads remain bytes on both engines, including when checking size and
corruption. Rebuild requests atomically advance the input epoch and record an
idempotent receipt; refresh counts only current terminal attempts, and completed
receipts remain immutable.

#### Rendition jobs and publication

Original-file rendition jobs run through the same Store operations on both
backends. Consent grants bind the exact profile, disclosure, input classes and
retained artifacts to the current processing incarnation. Revocations append a
new fence. A new grant cannot replace the authorization used by an earlier
provider call; publication rechecks that authorization inside its writer
transaction. Runtime can insert grants and revocations but cannot update or
delete their history. Logical import retains a fresh processing incarnation,
so restored grants remain historical.

Jobs share a build only when source bytes, execution identity and captured output
policy agree. Each version/profile waiter keeps its own consent. Claims,
renewals, scheduled retries and resumable provider handles keep the existing
lease and fencing rules. An expired provider call without a durable handle
requires operator resolution. Worker error classification recognizes PostgreSQL
serialization failures, deadlocks and lock unavailability. It also retries
pgx-confirmed safe connection failures before commit, so a disconnected claim
can resume on a fresh connection. A lost commit response remains fatal unless
PostgreSQL explicitly reports an aborted transaction. Reusing a staged
build or restoring a superseded head does not repeat provider work.

Builds, artifacts, evidence units and text segments are immutable records. The
runtime role may insert them; cleanup may delete them, but no update grant
permits rewriting evidence. Job staging creates retention roots in the same
transaction. Publication validates the source, retained policy, consent and
complete lexical manifest, then moves rendition and lexical heads together.
The final lexical generation is staged in that publication transaction, so a
concurrent text extraction or rendition cannot invalidate its manifest before
the head flip. Retained staged builds resume publication against current heads.
A failed replacement leaves the old heads and waiting job intact. Successful
replacement removes stale chunk-embedding heads and failures, and collects
superseded lexical generations only when no root or reader still needs them.

Lexical generations share an append-only stream of build membership. Each
generation names the ordinals up to its fixed cutoff. Collection trims only the
stream tail above every remaining generation's cutoff. The stored chain digest
is vault-local; metadata export computes portable digests from the generation's
members.

Build reads, active rendition views and exact text resolution use repeatable-read
snapshots. A purge committed during a read affects the next request. PostgreSQL
stores the relational lexical projection and manifests; SQLite additionally
maintains FTS through its existing triggers. PostgreSQL ranked search is not
qualified by publication tests.

The `renditions` backend suite covers this native lifecycle, including provider
resume, revocation, lease theft, staged-build reuse, failed publication and
concurrent purge. It does not qualify photo-description or face-detection jobs,
media producers, extraction workers, or public PostgreSQL vault opening.
The email and page suites below qualify their own native producers.

#### Plain-text extraction and publication

The shared Store discovers selected text versions, deduplicates queued blobs,
returns bounded ready batches, and defers retryable work behind ready items.
Terminal failures retire their queue entry. Startup seeding queues missing or
older results without reviving completed failures or purge-suppressed versions.

`RecordExtraction` keeps the extractor version, exact UTF-8 text, attempt count
and outcome together. It rejects version downgrades and same-version failures
that would overwrite a success. The existing `MigrateLegacyPlainText` operation
publishes retained plain-text results as immutable rendition builds and version
attachments, with a complete lexical generation in the same transaction. Once a
lexical head exists, `RecordExtraction` publishes only the affected blob's results
inside its writer transaction. The result cache and publication commit together;
a failure rolls both back and leaves the existing work queued for retry.
Identical results reuse existing immutable records. Changed same-version text
replaces the serving head and retires the queue entry. Processing changes identify
newly served live versions and changed results.

Purge suppression applies both to late worker results and subsequent startup
seeding. A shared blob can still serve an unsuppressed version. PostgreSQL uses
an identity column for the extraction cache's local row ID; it is not a document
or audit identity. Document IDs continue to use the transactional counter.

The `text-extraction` backend suite exercises these operations through native
writes, including terminal outcomes, scheduling, exact Unicode and empty text,
replay, same-version replacements, purge scopes and atomic rollback. PostgreSQL
indexes lexical generation membership by build ID for publication cleanup,
matching SQLite. SQLite alone
maintains its legacy FTS5 cache. PostgreSQL retains the text and rendition lexical
catalogs; ranked search, complete metadata transfer and public vault opening remain
unqualified.

#### Document embedding jobs and publication

Original-file and rendition-chunk embeddings use the same Store operations on
both backends. Jobs retain their exact consent grant, processing incarnation and
revocation fence. Replacement consent creates a successor job; it cannot adopt
an earlier job or its provider calls. Claims exclude live leases, renew under
the same epoch, and preserve retry counts when another worker resumes them.
Reconciliation checks current source and rendition authority, purge suppression,
executable providers and current consent before scheduling retained inputs.

Vector spaces, input generations, vector sets and their ordered row manifests
are immutable. Vector values and rendered inputs stay in blob artifacts; the
database retains their checksums, references and bounded projections. Runtime
credentials may insert those records but cannot update them. Exact duplicates
reuse the existing authority; a conflicting record fails the transaction.
Publication advances the serving head and completes the job in one writer
transaction. Failed staging retains no partial set, and failed completion
leaves the previous serving state and live claim unchanged. Purge and source
replacement fence older attempts, including failures arriving after publication.

Processing coverage uses one repeatable-read snapshot for source visibility,
rendition and embedding heads, and pending replacements. It shares current-head
eligibility with semantic retrieval. A replacement can be rebuilding while its
previous complete generation remains available. These metadata counts do not
establish that a vector search index is built or that its payloads are readable.

The `embeddings` backend suite covers native document jobs, artifact publication,
consent, leases, reconciliation, cleanup and coverage. The `visual-evidence`
suite also qualifies native image-input preparation and its publication chain.
Vector index construction, ranked retrieval and public PostgreSQL vault opening
remain unqualified.

#### Email and rendered-page publication

MIME inventory publication retains decoded artifacts, shares a generation across
identical source bytes, and selects an exact version's inventory in one writer
transaction. Body publication uses the ordinary rendition publisher: the selected
MIME body, rendition head and complete lexical generation become visible together.
A failed publication preserves staging and the prior serving state. Re-observing a
body build preserves its first completion time; purge suppression prevents a late
attempt from restoring deleted authority.

Attachment publication creates ordinary child documents and their exact parent
relations together. It preserves audit history, rejects stale destinations, and
replays an operation's original receipt. Retained publication receipts protect
versions from pruning and inventories from purging. Releasing a receipt checks
mailbox and export references before deleting it. Relation pages report processing
status for the retained child version, even after the current child changes.
They use the same rendition-coverage rules as collection queries.

Page jobs keep their operation identity, source revision and claim epoch through
queueing, restart, cancellation and completion. Frame, recipe and image records
are immutable. Publishing an image records its physical blob and job result in
the same transaction; failed result publication leaves neither behind. Completion
requires every requested page receipt. Canceling a job preserves already published
pages but fences further writes by its old claim.

Email metadata, target selection, relation pages, page inventories, image reads
and job views use repeatable-read snapshots. A publication or receipt release
committed during a read affects the next request. Email target selection retains
SQLite's exact MIME partial-index expression; PostgreSQL uses its equivalent
`strpos` predicate and index. No new per-row queries or runtime backend interface
are introduced.

The `email` and `pages` backend suites exercise these native operations, including
late-write rollback and concurrent publication. Plain-text extraction, email PDF
rendering, rendering subprocesses and metadata recovery remain outside these
suites. They do not enable a public PostgreSQL-backed vault or qualify ranked
search.

#### Visual previews and retained PDFs

Visual preview publication records an exact source version, immutable recipe
result and physical output together. Recording a new recipe can advance the
active head; replaying an older recipe cannot move that head backwards. A failed
head write leaves neither a partial generation nor its output blob. Generation
rows are insert-only for the runtime role; cleanup may delete them.

Preview target discovery pages through live photo display versions. It skips
versions with any recorded result for the recipe, including failed or unsupported
results. Eligibility is rechecked before work begins. Batch recipe reads keep a
single query. Image reconciliation prepares each missing input once per page and
can resume pending work in a profile that mixes text and image bindings.

Exact-version preview reads and retained email-PDF receipt reads use one
repeatable-read snapshot. Concurrent pruning or purge affects the next request.
PDF discovery validates every retained attachment against the same source and
email inventory, even if its renderer is no longer installed. These PDFs do not
replace the email body's serving or search authority.

The daemon's preview backfill uses target discovery and the eligibility recheck.
HTTP routes use the PDF list and receipt reads to inspect and download retained
PDFs. These production callers currently open SQLite; PostgreSQL qualification
covers their Store operations, not a complete daemon workflow.

The `visual-evidence` backend suite covers these native operations, rollback,
and concurrent removal. Rendering executables, general photo/person workflows,
vector indexes and public PostgreSQL vault opening remain unqualified.

#### Media retention and input artifacts

Supplied-media retention, remote-reference retention, recording acquisition and
transcript/caption import use the same Store operations on both backends. Core
content, source revisions, occurrence bindings, input artifacts and the operation
receipt commit together. Replaying an operation returns its stored receipt;
changed request identities conflict. Equal remote bytes reuse an existing source
revision without merging separate source identities or caller occurrences.

Reads select only the caller's visible occurrences. Revocation hides that
occurrence and its inputs, removes its input-bound rendition heads, and preserves
other callers' occurrences. Source pages, occurrence pages and exact-revision
views use one repeatable-read snapshot for their totals, selection and receipts.
A concurrent revocation or receipt update becomes visible on the next request.
SQLite retains its page-first join order; PostgreSQL uses ordinary joins. Both
schemas index occurrence visibility and principal/source operation history.

Media acquisition consent uses the existing consent grants and revocation
fences. Its operation receipt commits with the grant or revocation, so an exact
replay does not issue another grant or advance the fence again. Processing
receipts retain their exact source and consent selection while recording queued,
failed and completed outcomes. Continuation pages expose only the requesting
principal's queued operations. Revoking acquisition consent does not remove
already retained content.

The runtime role has insert-only access to media sources, source versions and
input artifacts. Occurrence updates are limited to first-version binding and
revocation; operation updates are limited to the receipt and update timestamp.
The `media` backend suite exercises native retention, immutable-identity
conflicts, replay, visibility, consent, continuations, late-write rollback and
concurrent reads. It does not qualify remote network acquisition, media provider
execution, metadata recovery or public PostgreSQL vault opening.

#### Grouped photo import

Native photo import groups RAW files, JPEGs and sidecars in one writer
transaction. Repeated imports preserve node and member identities; changed
source bytes create a new version unless create-only import was requested.
Separate source owners keep their own bytes. Ambiguous groups remain unpaired
and return the same explanation on both backends. Operator pairing, hidden and
excluded state survive later imports. Audited vaults continue to refuse grouped
import through the existing mutation boundary.

A retained XMP sync receipt lets import recognize accepted outside bytes without
replacing the newer published sidecar. SQLite keeps its indexed source-path
lookup and exact receipt-index expression. PostgreSQL uses its JSON expressions
and receipt indexes. Size matching keeps SQLite's released expression index;
PostgreSQL indexes the integer size directly. Browser folder continuation uses
current eligible members; hidden, excluded, trashed and sidecar files cannot
supply another folder's identity.

PostgreSQL refreshes source claims, timeline rows and export work when membership
changes, inside the same transaction. SQLite retains its released triggers. A
failed receipt write rolls back the documents, memberships, derived rows and node
allocation together. Photo import's progress, cancellation and finalization use
the shared operation records under writer admission. Expired receipts stay until
pending physical cleanup is gone. Multi-store operations have their own contracts
below.

The `photo-import` suite covers these native Store workflows. Retained XMP
receipts used by import are produced by SQLite's publisher. Native XMP publication
and authored editing are covered by `photo-edit`; `photo-review` and
`people-curation` cover their own operations below. Public PostgreSQL vault
opening remains unavailable.

#### Photo metadata edits and XMP synchronization

The internal PostgreSQL Store supports metadata preparation by explicit asset
IDs, batch edits, keyword changes and undo. Ratings, flags, labels, captions,
creator, copyright and rotation keep the same file and node revision fences as
SQLite. Audited edits append their history in the same writer transaction.
Preparation reads the assets, file values, keywords, XMP status and retained
suggestions from one repeatable-read snapshot.

XMP classification, incoming metadata and keyword imports, conflict settlement,
and sidecar creation or replacement use the shared Store operations. A plan must
still match both its input token and last receipt when it publishes. A settled
plan cannot be replayed by a second worker. Detaching a generated sidecar retains
its history; a later edit can reuse it. Source pairing excludes caller-supplied
embedded evidence case-insensitively on both backends.

PostgreSQL refreshes photo export work inside authored edits and XMP settlement.
Receipts, imported values, sidecar versions and queue changes commit together;
a failed receipt write leaves the prior state and queued retry intact. SQLite
retains its released triggers and receives no additional maintenance queries.
The existing receipt lookup expressions keep their engine-specific indexes.

The `photo-edit` contracts exercise native writes, stale plans, both conflict
choices, generated sidecars, undo, audit history, queue invalidation and rollback.
Face and description records also participate in metadata preparation; their
native processing lifecycle is covered by the `photo-analysis` suite below. Query-based selections,
`SkipUnavailable`, best-shot/representative choices, hidden-session management,
photo review, and public PostgreSQL vault opening remain unqualified.
The processing worker's XML encoding and filesystem publication still run through
the SQLite vault; this qualification covers their Store calls.

#### Photo descriptions and face detection

Description and face-detection jobs use the shared rendition queue on both
backends. Discovery pages return ready grid previews in version order and keep
active work separate by task: describing a photo does not block face detection.
Consent, preview identity, claim expiry and the current display photo are checked
again before publication. An explicit request can replace a dismissed producer;
automatic work preserves the dismissal.

Publication commits suggestions or face observations, completion, waiter state
and change events together. A failed write leaves the claim available to retry
without partial results. Face reconciliation keeps matching IDs and preserves
manual faces, person assignments, candidates and references retained for undo.
Unknown roll or expression measurements remain distinct from observed zero or
false. A new detector with identical observations updates provenance without
announcing a content change.

Accepting caption, tag or rotation suggestions uses the existing authored-edit
transaction, including revisions, undo receipts and export work. Rejecting a
stale response or failing its final suggestion update leaves authored values and
pending suggestions unchanged. PostgreSQL keeps the relational suggestions;
SQLite still owns its released full-text triggers. PostgreSQL ranked search
remains separate.

The `photo-analysis` contracts run native description jobs, face reconciliation,
suggestion responses and target discovery. The SQLite plan check proves an
ordered index seek. The PostgreSQL check disables sequential scans to prove the
ready-preview index is usable; planner choice on a populated library remains
unverified. Retained SQLite fixtures cover
candidate/undo protection and regeneration after restore; they do not qualify
their producers or restore. Native people curation has its own contract below.
The two worker tables (explicit waiters
and completed scans) are disposable. Suggestion IDs use a PostgreSQL identity
sequence; a future metadata importer must advance it past imported IDs.
Provider network execution and public PostgreSQL vault opening remain unqualified. Quality scoring and hidden sessions use the contracts below.

#### Photo quality and hidden sessions

The shared Store discovers photos with ready grid previews, records quality
scores or terminal unavailable results, and reads those results on both
backends. Publication rechecks the current display version, source bytes and
face inputs inside writer admission. A changed face box or observation queues
new work; a late result cannot overwrite the current face report. Readers keep
image scores while omitting stale face reports. Repeating the same input does
not overwrite a terminal result. Replacing an evaluator removes its old cache
in the same transaction as publication; failed publication preserves that cache.

Face lists and their input signature come from one read snapshot. The signature
uses each engine's JSON aggregation; it is an opaque, backend-local equality
key, not a portable digest. Quality caches are disposable and excluded from
metadata transfer. The face-to-candidate index supports dismissal checks without
scanning all candidates. Retained dismissal tests qualify reading existing
people decisions, not producing them on PostgreSQL.

Hidden-photo operations configure and change passcodes, unlock and lock local
sessions, record failed attempts and lockouts, and reset or disable the feature.
Tokens remain in the owning Store's memory and disappear on restart. Use one
process per PostgreSQL vault schema: passcode changes and locks revoke sessions
in that process only. Expiry,
explicit lock, passcode change and reset end access. A wrong passcode commits
its failure record even though the operation returns an error. Disable unhides
every asset, writes its receipt, refreshes XMP export work and removes the
credential atomically. A failed disable retains both the credential and the
live sessions. Hidden-state reads keep the visibility change ID, configuration
and lockout in one snapshot.

Visibility change IDs follow receipt insertion order, not timestamps or random
UUID order. SQLite keeps its existing `rowid` query and operation index.
PostgreSQL uses a private `receipt_order` identity column with a matching
operation/order index. This ordinal is not part of logical metadata. Failure
IDs also use a PostgreSQL identity column; a future importer of retained failure
rows must use `OVERRIDING SYSTEM VALUE` and advance its sequence after inserting
explicit IDs. Runtime roles need
no sequence grants for these identity inserts.

Photo settings, including hidden-session lifetime and inherited display
preference, share the existing revision checks and receipts. Preference changes
refresh affected displays and export work inside the write.

The `photo-quality-hidden` contracts cover native publication, terminal and stale
results, retained dismissal, lockout, expiry, revocation, settings, rollback and
read snapshots. General photo browsing and quality filters,
portable restore and public PostgreSQL vault opening remain separate
qualification boundaries. Explicit-selection review uses the contract below.

#### Photo review

The shared Store groups an explicit selection by capture time, folder and camera
identity, chooses keepers or representatives, accepts or dismisses best-shot
suggestions, and undoes those choices on both backends. The selection defines
membership; a new neighboring import is not added to an existing decision.
Decisions recheck source metadata, every original file and the current settings
inside writer admission. Independent companion-file flags stay intact.
Hidden groups and their undo require a live session in the owning Store.

Representative choices advance a transactional counter without changing file
flags. Undo restores the prior choice only at the receipt's asset revision and
never reuses an issued rank. Asset revisions refresh export work in the same
transaction. Failed receipt writes roll back flags, ranks, the counter, export
work and receipts, and send no change notification.

Grouping and best-shot reads keep one snapshot. SQLite retains its forced join
order; PostgreSQL uses ordinary joins. Review can read retained image index
generations and hydrate only the indexed, still-eligible inputs. Damaged
disposable index bytes fall back to capture-time grouping. The two index tables
have read-only runtime access; this does not qualify PostgreSQL index building,
publication, reader leases or collection.

The `photo-review` suite covers native explicit selections and retained-index
reads. Search-driven browsing and query/frozen-run selection,
portable restore and public PostgreSQL vault opening remain separate.

#### People and face curation

The shared Store supports person creation, revision-fenced edits and retirement,
contact identities, external UID aliases, custodians, candidate decisions, face
naming and rejection, key faces, merges, explicit splits, and face-decision undo
on both backends. Exact-person details, candidate pages and face previews use
these same operations. Browser writes retain the existing hidden-photo checks;
unlocking remains local to the owning Store.

All mutations use writer admission. Identity moves, aliases, face links,
candidate state, binding epochs and replay receipts commit together. PostgreSQL
writes candidate-to-face membership when it inserts a face candidate; SQLite
keeps its released trigger. Candidate evidence is immutable through these
operations. The runtime role can change only the listed mutable columns and can
insert, but not rewrite, merge and split receipts. Conflict handling does not
continue a PostgreSQL transaction after a uniqueness error.

Person details, candidate and custodian pages, external UID resolution and actor
keys use repeatable-read transactions. Counts, identities and the returned rows
therefore describe one snapshot. Queries close their result sets before issuing
related lookups. SQLite keeps its forced preview join order and exact receipt
index expressions; PostgreSQL uses ordinary joins and matching catalog indexes.
Queries do not rewrite placeholders or add another database facade.

The `people-curation` contracts reuse the existing lifecycle scenarios on both
engines. PostgreSQL-specific cases interrupt writes at the final receipt or
candidate projection, and commit edits between statements in a read. Package
custody writes run against a validated retained package fixture; this does not
qualify package creation or import.

People discovery is covered below. Query-selected photo browsing, portable
recovery and public PostgreSQL vault opening remain unqualified. Their retained
catalogs and the curation operations above do not establish support for those
producers or user entry points.

#### Document-person resolution

The shared Store prepares document-person inputs, publishes attribution, records
failures and tracks rebuilds on both backends. Exact email and international phone
identities can provision a person once; name-only claims remain review candidates.
Retired identities and explicit external-identity unlinking retain their existing
suppression rules. Inputs include current custodians and operator assertions.
Preparing inputs in an audited vault does not create unaudited person authority.

Preparation and publication use writer admission. Publication checks the person
binding epoch, event generation and node revision before writing. Retained
generations are insert-only; edges, the current head, publication epoch and change
events commit together. An identical replay keeps the existing publication, and
input-only changes do not emit a processed event when the result is unchanged.
Provisioning an unbound identity does not invalidate unrelated attribution.

Version readback and coverage use repeatable-read transactions, so one response
cannot mix a retained head with later authority or counts. Stale attribution is
withheld until republished. Rebuild creation advances the binding epoch and
records its replay receipt atomically. Retryable failures keep a rebuild running;
terminal unavailable results let it finish with failed coverage. Progress is
recounted under writer admission before it commits.

The `document-people` contracts run the existing input, publication, freshness and
rebuild scenarios on both engines. PostgreSQL cases also commit changes between
read statements and interrupt provisioning, publication and rebuild creation
through the restricted runtime role. The Store port adds no schema or runtime SQL
translation. People discovery is covered below; query execution, recovery and
public PostgreSQL vault opening remain separate qualification boundaries.

#### People browsing

`PeopleByDisplayName` supports autocomplete, name-order and recent-first pages,
visible identity labels and photo cards on both backends. Namesakes share their
newest eligible evidence rank and remain adjacent across cursor pages. Retired,
sensitive, stale, excluded, trashed and locked Hidden evidence keeps its existing
visibility rules. Photo cards count distinct assets and choose a readable key
face or the existing fallback portrait; Hidden counts appear only while unlocked.

One repeatable-read transaction covers eligibility, the page, labels, counts and
portraits. SQLite keeps its forced join order, name-index hint and word-prefix
function. PostgreSQL uses ordinary joins, JSON array expansion and a literal
regular expression with the same Go Unicode letter, number and combining-mark
ranges. The ranges are rendered once; typed text is escaped and bound as data.
Neither backend filters a limited page after reading it or translates SQL at
runtime. PostgreSQL has the same folded-name/page-cursor index as SQLite;
the existing runtime grants suffice.

The `people-browsing` contracts share the existing browse and summary scenarios,
including one-item cursor pages, hidden-photo access and shared/paired photos.
Unicode cases cover literal punctuation, non-Latin names, combining marks and
numbers at word boundaries. A PostgreSQL scenario commits a retirement during
a page read and verifies that the next request alone sees the change.

#### Photo albums

The shared Store creates, renames, stars, duplicates and deletes albums, changes
membership by explicit asset IDs, and reads album summaries on both backends.
Mutations check the expected revision inside writer admission. Member changes,
cover changes, revision advances and receipt chunks commit together; a failed
receipt write leaves the prior album and membership intact. Repeated adds and
removes preserve member timestamps and produce no new receipt when nothing
changes. Duplication preserves member timestamps, star and cover choices.

Summary reads use one repeatable-read snapshot for properties, counts and cover
selection. They distinguish retained membership from currently visible photos.
A cover needs a live display version and a ready preview for the requested
recipe. A hidden, excluded or unavailable cover falls back to the newest visible
member with a matching preview. A locked hidden cover is omitted from responses,
including duplication, without erasing the stored choice. Explicit membership or
cover changes involving hidden photos require a live hidden session. Existing
audit restrictions on album mutations remain unchanged.

SQLite retains its ordered membership index and forced cover join order. The
query-plan check requires an indexed album seek without a temporary ordering
sort. PostgreSQL uses ordinary joins and the same membership access indexes;
its runtime role can insert albums, update their mutable properties, and insert
or delete members. It cannot update member identities or timestamps in place.

This qualifies explicit-ID Store operations. Search-selected populations,
`SkipUnavailable` selection, browse pages and albums created from frozen query
snapshots remain within the search and delivery qualification boundaries. The
public PostgreSQL vault lifecycle is still unavailable.

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
unit. On SQLite, observations drive the join from each bounded JSON batch and
seek entries by checkout and file ID; a query-plan test covers this without
statistics. Retiring a checkout releases its retained versions without deleting
copies.

The `photo-checkout` contracts exercise native operations, late publication and
commit failures, batched observation rollback, snapshots and retained-version
holds. Existing albums are inputs to selection; this does not qualify album
editing. Filesystem workers, physical backup and restore, general photo
query selection and public PostgreSQL vault opening remain separate.

#### Recoverable deletion

Native document and folder trash, photo-group trash, trash browsing and restore
use the shared Store operations on both engines. Trashing records the original
parent and name, stamps the live subtree together and preserves bytes and content
history. Restore preserves node IDs, leaves independently trashed descendants
alone, and uses the existing parent fallback and smallest free name suffix.
Audited operations retain their scope restrictions and append the same canonical
history as other document mutations.

Photo recovery follows connected members across folders, restores original
parents before children, and keeps sidecar sync history and recorded decisions.
PostgreSQL refreshes export work inside the mutation transaction after node,
asset and XMP receipt changes. SQLite keeps its released triggers. A failed final
receipt write rolls back the nodes, audit advancement and export work together.
The runtime role can insert photo receipts and delete the superseded XMP sync
receipt; it has no receipt `UPDATE` grant.

Trash pages count and select grouped photos and ordinary roots from one
repeatable-read snapshot. Photo reads also request that isolation when assembling
assets from multiple statements. Backend qualification compares populated photo
state, receipts and export inputs against SQLite and validates the resulting
ordinary and audited metadata.

Photo review, portable backup/restore and public PostgreSQL vault
opening remain unqualified.

#### Permanent cleanup

`TrashEmpty`, `TrashEmptyBounded` and `PruneContentVersions` use the same native
operation on both backends. A trash batch completes the last connected photo
group even when that exceeds its root limit. Live, too-new or retained peers
hold the group. Email publications, mailbox receipts, snapshots, packages,
media authority and active photo checkouts preserve their existing retention
rules; an unrelated trash root can still be deleted. A retained export refuses
an execution until its authority is released.

Version pruning preserves revert dependencies and photo sidecar history. The
all-prior selector can install a source-free checkpoint for a current revert.
A pruning operation that removes history advances the node revision once. Its inventory
separates shared bytes from loose and packed bytes eligible for later cleanup;
it does not report physical space reclaimed.

Before deleting a version, cleanup removes its serving attachments, embedding
sets and worker waiters. Shared rendition jobs retain or reselect remaining
consumers under current consent. In-flight calls keep their claim; staged work
can be requeued with a new fencing token. An orphaned ambiguous provider call
keeps its durable record so cleanup cannot silently authorize a repeat call.
Immutable derivative generations remain available for later collection.

Receipt lookups must match each backend's expression indexes exactly: retain
SQLite's text cast and PostgreSQL's JSON operator and `C` collation. Equivalent
JSON values alone do not preserve indexed lookup plans.

Both destructive operations retain the existing refusal while audit authority
is active. Dry runs can inspect candidates but do not authorize deletion.
Physical reclamation uses the separate catalog operations described below;
other processing producers remain unqualified.

#### Derivative cleanup

`PurgeDerivatives` removes selected derived data through one shared storage
transaction. Version, attachment and build selectors keep their existing scope;
`All` selects all derivative authority, while an empty request collects only
unrooted garbage. Original versions and their bytes remain intact. Unreachable
derivative blobs enter the pending collection catalog; this operation does not
delete physical bytes or claim reclaimed space.

Active worker and reader leases, durable roots and retained exports keep their
existing protection. A purge can revoke a serving head while a reader still
retains its immutable data. Shared extracted text and email generations survive
until no remaining version needs them. Email document publication receipts block
an exact purge and cause a bulk purge to skip the retained inventory.

Purge suppressions and their audit records commit with the removals. Explicit
rendition and embedding rebuild authorization supersedes only the requested
suppression. A failure rolls back the entire operation, including head changes,
replacement lexical generations, audit advancement and pending collection rows.

`DerivativePurgeFingerprint` reads its selected identities in one statement and
preserves the same compact identity bytes on both backends. `DerivativeGCPlan`
uses one repeatable-read snapshot for builds, generations and roots, so a lease
change between queries cannot produce a mixed plan. Execution checks current
retention again under the writer lock.

Lexical replacement preserves surviving build membership and text. Shared index
writes allocate their rebuildable row IDs under the store writer lock. SQLite
continues to maintain its FTS shadow tables through its released triggers;
PostgreSQL has only the retained lexical catalogs at this stage. Cleanup does
not qualify PostgreSQL search or processing publication.

#### Physical packing and collection

The native `PackCatalog` supports Kit's primary-store pack and repack lifecycle
with PostgreSQL metadata. Kit still writes and verifies the physical files;
Docbank supplies logical membership, read locations, mappings and pack summaries.
Packing replaces loose authority with verified packed authority. Repacking
checks the complete live mapping set before publishing replacements. A failed
transaction retains the original authority and mappings.

PostgreSQL mutation helpers refresh affected pack summaries once per pack inside
the writer transaction. This includes adoption, replacement, index removal,
loose repair, placement retirement, storage repair, logical collection and
re-ingest. SQLite keeps its released
summary triggers. Counts, totals and maxima describe only live members; dead
mappings remain until physical retirement. A pack's scan key stays stable after
publication, even when its lowest member disappears. Bounded GC and repack scans
count examined rows, including ineligible entries, and retain byte-order cursors
and SQLite's existing index hints.

Logical blob deletion and exact cleanup receipts commit together. Receipts name
the store and loose encoding or immutable pack, survive a failed physical cleanup,
and can be retried. Before retiring a loose object, the caller checks its receipt
again: renewed location authority consumes stale work without authorizing byte
deletion. Completion removes only that exact receipt. Pack retirement removes
its pending receipt through the catalog's foreign key. The caller must still
hold exclusive vault/blob-root ownership across physical maintenance; the SQL
writer lock alone cannot serialize filesystem publication.

PostgreSQL tests run Kit's existing catalog contract against setup-owned DDL and
a separate runtime role. A real-file lifecycle packs documents, revokes collected
members, repacks the survivor and reads its original bytes. Additional cases
cover bounded scans, summary changes, rollback and cleanup replay after content
is reauthorized. This qualifies the primary catalog and cleanup protocol, not
public PostgreSQL vault opening, deployment workers or metadata recovery.

#### Secondary stores, placement and evacuation

The native Store registers secondary filesystem or S3 identities, reports their
inventory and availability impact, plans placements and evacuations, and commits
verified destination receipts on both backends. Store plans contain stable
identities and physical receipts; deployment paths and credentials stay with the
caller. Public PostgreSQL opening and deployment-specific workers are separate.

Placement previews and repair plans use one read snapshot. Previews account for retained
versions and all references, report shared or audit-pinned content,
and account for container reads, verification and scratch space. PostgreSQL uses
plain joins and scalar `GREATEST`; SQLite keeps its forced inventory join order.

The caller copies and verifies bytes before entering the short writer transaction.
Commit rechecks the destination receipt and reference closure, authorizes the
verified destination and retires only the exact reviewed source generation.
Reference drift, audit pins or changed source authority keep the source. Packed
source retirement and repair refresh pack summaries inside the same transaction.
Failed catalog publication leaves the prior authority, mappings and summaries.

Evacuation admits one active operation per source under writer serialization.
Active source or destination references prevent detachment and unregistering.
Finalization requires complete destination coverage, records exact physical
cleanup, and leaves the source draining until that cleanup finishes. Cancellation
cannot interrupt already-finalizing publication. Recovery records retain the
reviewed source candidates and prior destination generation; repair or salvage
cannot overwrite a destination changed after review.

Cleanup receipts survive process interruption and block expired-operation pruning.
Before removing an object, the caller rechecks the receipt: a reauthorized object
consumes its stale receipt without authorizing deletion. Successful physical
removal consumes only that exact receipt under writer admission. The SQL writer
lock does not replace exclusive ownership of physical storage during publication.

The `multi-store` contracts run the existing registry, placement, cleanup and
operation-lifecycle scenarios on both backends. PostgreSQL cases move real bytes
from a primary pack to a secondary and back, test late-write rollback during
placement and repair, and commit location changes between preview statements.
The runtime role may insert store identities, change their lifecycle and remove
detached identities; it may insert operation-store references and insert/delete
cleanup receipts. Other lifecycle rules are enforced by Store methods, rather
than by PostgreSQL grants or a closed state index.

#### Imports and source history

Native file imports publish their ingest run, provenance, exact-version binding,
physical receipt and optional collection label in one storage transaction.
Planned filesystem imports create their destination directories in that same
transaction. A rejected label or receipt leaves none of those changes behind.
Finalizing an empty labeled import creates its requested directories without
retaining an empty ingest run or label. Repeated observations preserve content
versions and record membership under the existing source and basename rules.

Watch cursors continue to follow moved nodes. Unchanged source bytes preserve
independent document edits; changed source bytes update the tracked node. Push
can adopt a watch cursor, records subsequent observations atomically, and pages
cursors in byte order. Complete watch inventories record and clear absence
observations; rebinding a source clears its mark, including on an exact retry.
Removing a watch clears its observations. These marks do not change archived
documents or their exported metadata.

Collection listing, member pages, label changes and provenance history use the
shared queries. Multi-statement collection, provenance and directory-plan reads
use repeatable-read snapshots. The `embedded:` source-kind prefix is
case-insensitive on both backends. Caller-supplied evidence stays out of
operational collections, collection labels and filesystem photo-source
selection; its stored spelling and provenance remain unchanged.

Provenance corrections append facts rather than replacing them. Ingest runs,
provenance and version bindings have no runtime `UPDATE` grant. The runtime can
delete a run only for empty-import finalization; references from retained facts
prevent deleting a document-bearing run. Existing audit restrictions on labels
and supported source operations remain unchanged.

On PostgreSQL, observation and correction operations refresh photo source
claims, dates and export work inside the writer transaction. Binding or
correcting evidence invalidates derived events and people, schedules event
recomputation, and advances the publication epochs for removed projections.
SQLite retains its released trigger ownership, including the event epoch's
per-head increment. Canonical event tables support this invalidation and its
cascades; [event publication](#source-metadata-and-document-events) uses the
shared Store, as does [document-person publication](#document-person-resolution).

Operation tests use canonical setup and the runtime role, validate complete
metadata and audit replay, and compare derived effects with real SQLite writes.
They force a late invalidation failure to verify rollback, and commit concurrent
writes during history reads to verify snapshot consistency. These capabilities
do not expose a public PostgreSQL vault or qualify portable metadata import,
collection snapshots, reporting,
backup or restore.

#### Tags and saved definitions

Tag creation, assignment, rename and deletion use the same Store methods on
both databases, including audited operations and path-based assignment. Tag
pages retain byte-order sorting, current assignment counts and trash filtering.
Definitions store exact assignment counts. Membership changes, purges, and
metadata restores update counts in the same transaction.
Bulk edits preserve revision fences, atomic changes and immutable replay
receipts, including replay after a tag is deleted. A bulk preview reads the tag
and selected nodes from one repeatable-read snapshot.

On PostgreSQL, tag writes refresh affected photo export work in their write
transaction. Bulk edits and definition changes collect affected assets as a set,
including sidecars belonging to those photos; SQLite retains its released
triggers. Tag deletion still refuses definitions referenced by queued or running
mailbox jobs.
The job-state index supports that check. Runtime credentials can insert batch
receipts but cannot update or delete them.

Saved query and highlight definitions retain canonical payloads, optimistic
revisions, idempotent edits, filtered pages and photo-only selectors. These
mutations remain unavailable after audit enrollment, as on SQLite. Deleting a
definition cascades retained run receipts through the definition index; this
does not qualify saved-query execution or run publication on PostgreSQL.

The `organization` backend scenarios cover native mutations, bulk replay and
rollback, concurrent saved-definition edits, snapshot consistency and photo
export effects. Mailbox jobs and settled photo receipts are retained fixtures;
their producers remain outside this qualification.

#### Shared query qualification

Node allocation, direct lookups, single-node canonical paths and directory
pages use the same SQL on both engines. Numbered `$1` parameters,
`INSERT ... RETURNING id` and a recursive ancestry query work with both SQLite
drivers and pgx. Multi-segment path resolution and batched display paths also
share traversal and result assembly. Only JSON array expansion differs:
SQLite uses `json_each`; PostgreSQL uses `json_array_elements_text` with
ordinality. Both bind one JSON array, preserving exact integer IDs and input
positions without one parameter per item. Recursive path assembly avoids a
backend-specific ordered string aggregate.

Path lookup preserves name normalization, case-sensitive matching and live
ancestor traversal. An invalid segment reports an invalid name only after its
valid prefix resolves; a missing prefix still reports not found. Batched paths
retain input order and repeated IDs, include display paths for retained trashed
nodes, and fail if any requested ID is missing. Both operations use their
caller's transaction when supplied. Directory and file creation retain their
existing mutation transactions and error handling, as described under
[native document writes](#native-document-writes).

Node views and directory pages request read-only `REPEATABLE READ`
transactions. Their metadata, canonical path, child count and selected page
must come from one snapshot. PostgreSQL's default `READ COMMITTED` gives each
statement a new snapshot and can combine an old node name with a renamed path.
SQLite retains its existing transaction snapshot. This follows the explicit
snapshot pattern in msgvault and Kata; apply it to each multi-statement
snapshot read as that operation is ported.

The shared node-query tests exercise allocation, current-version projection,
live-child lookup, normalized path resolution, ordered batched paths,
directory paging, rollback and name reuse
after trashing. Concurrent-write cases commit a rename or new child between
reads and verify that a response stays on its original snapshot while a later
request sees the change. They use the real SQLite schema and PostgreSQL
catalogs installed by `pgschema.Setup`. PostgreSQL
queries run through a separate runtime login with explicit schema, table and
allocation-counter grants. Reuse this setup for new query tests instead of
copying DDL into fixtures. The separate native document-write tests qualify
complete mutations.

Retained content-version lookups and history pages also use shared SQL. They
preserve node ownership, revision ordering, source-version provenance and
auxiliary checksums, including retained history for trashed files. A node/version
view requests a read-only `REPEATABLE READ` transaction, following msgvault and
Kata, so a prune committed between its reads cannot remove a version from that
response. A later request sees the prune. History counts and pages remain one
statement. Shared queries introduce numbered parameters in ascending order:
SQLite assigns their bindings by first appearance, while PostgreSQL uses the
number itself. Exercise shared queries on both backends to catch binding and
result differences.

The content-history tests reuse the same setup and the real SQLite schema.
They cover version projection, ownership, pagination and a prune
committed between the view's reads. Native mutation tests separately qualify
content writes; a public PostgreSQL vault remains unavailable.

Node and scope audit history also use shared SQL and read-only `REPEATABLE READ`
transactions. Membership, node revision, path, scope head, event count and page
come from one snapshot, even when another operation commits between their
reads. SQL orders and pages events through the scope and node indexes; Go
projects the returned canonical events. The runtime role needs only `SELECT`
on the audit catalogs for these reads. Enrollment additionally needs the narrow
write grants above.

Audit-history qualification creates directories, enables audit and renames nodes
through normal SQLite operations, then validates that history before copying
its catalogs into a disposable PostgreSQL schema. Setup credentials load the
fixture; the separate runtime login reads it. The fixture preserves the vault
identity and node allocation high-water mark. It is limited to directory
history and is not a production import implementation. Tests cover node and
scope pagination and commit later canonical history between a reader's
statements: the current response retains its snapshot and the next request
sees the new events.

Audit status and optional node membership use shared queries and the same
read-only `REPEATABLE READ` boundary. Path resolution, allocation authority,
scope counts, heads and sticky membership describe one snapshot. An audited
node retains its membership after trashing; its live path is no longer reported.
The PostgreSQL tests read native SQLite-generated history, including a rename
committed between status reads, through the existing canonical fixture.

Audit mutation preparation also shares authority and scope loading. The store
owns node allocation queries: SQLite reads `sqlite_sequence`; PostgreSQL reads
`node_allocation` in the same transaction as the protected rows. Both retain
committed allocations after deletion and discard allocations on rollback.
The transaction tests cover callback failure, cancellation and commit-time
constraint failure. Audit qualification also replays native SQLite-generated
history against PostgreSQL's high-water mark after a failed insert. Complete
enrollment also runs the full metadata validator described below.
Native operation tests also replay history after creation, content changes,
imports, moves, trash and restore. Qualification remains limited to the native
operations described above; it does not enable every audited mutation or
portable metadata import.

The native canonical-record insertion helper also uses shared SQL. Its index
builder rejects zero node IDs, operation sequences and entry counts in Go
before insertion. The same validation is used by metadata import and record
loading. Indexed integers must fit the storage integer range; event ordinal zero
and zero-valued genesis counters remain valid. Registered nested record kinds
cannot be stored as top-level authority records.

Record-insertion qualification uses canonical records produced by normal audit
enrollment. Existing SQLite enrollment tests validate stored relational indexes;
the PostgreSQL insertion test checks the same projections. It uses a caller-owned
transaction and rolls it back: inserting records alone does not publish complete
authority or membership projections. Canonical records remain append-only:
PostgreSQL runtime has `SELECT` and `INSERT` on `audit_records`, with no `UPDATE`
or `DELETE` on that table. Event rows are inserted with their canonical mutation
and carry its operation sequence.

Existing audit authority and scope heads also advance through shared Go writers.
They reject nonpositive counters and require the allocation entry count to equal
the operation sequence before updating authority. Canonical records and their
head updates stay in the caller's storage transaction; a failed operation
publishes neither. Runtime needs only the column-specific `UPDATE` grants above
for these projections, alongside record `INSERT` and catalog `SELECT`.

The PostgreSQL advancement test starts from validated SQLite history, applies a
native rename's resulting node projections and records, and compares committed
status and every canonical record with SQLite. It also verifies that discarding
the transaction preserves the earlier status and records.

Initial and additional enrollment use `PreviewInitialAudit` or
`PreviewInitialAuditPath`, followed by `EnableInitialAudit`. The existing writer
transaction protects the target, retained versions, trash origins and attached
metadata. Commit rebuilds the reviewed baseline and checks allocation authority;
a changed preview is rejected. Additional preview validates existing audit
history and rejects overlapping scopes.

Enrollment validates all retained metadata before committing, including catalogs
outside the selected scope. The same Go validators cover processing and consent,
embeddings, email and media, people, collections, packages, page numbering and
photo state on both backends. Shared SQL uses numbered parameters and explicit
boolean comparisons. JSON receipt queries share their validation logic and select
SQLite or PostgreSQL expressions at construction. Readers close result sets
before issuing another query on the same transaction.

SQLite inspects foreign keys with `PRAGMA foreign_key_check`. PostgreSQL checks
pending deferred references with `SET CONSTRAINTS ALL IMMEDIATE` after logical
validation and keeps them immediate through commit. Call the complete validator
after the final mutation, not before writes that rely on deferred references.
The PostgreSQL dialect flag
in the metadata source description selects SQL only; its logical record version
is not a PostgreSQL schema version or migration gate.

Records, authority, scopes, baselines, memberships and photo re-export queue
entries commit or roll back together. PostgreSQL initial enrollment queues every
eligible photo asset once after inserting memberships. Later membership inserts
queue eligible originals in the member's asset, including sidecar enrollment.
Released SQLite layouts retain their triggers as the sole queue owner; PostgreSQL
performs these writes in Go. Neither backend publishes files during enrollment.
The `photo_xmp_inputs` view preserves eligibility and exact change-detection
tokens, and target paging preserves filtering and cursor order.

Qualification uses populated catalogs created through SQLite operations, then
calls PostgreSQL's native preview and commit methods with a separate runtime
login. It compares status, canonical records, photo tokens, queues and pages.
Invalid metadata outside the scope and invalid photo receipt references reject
enrollment without leaving audit authority or queue changes. Changed previews
are rejected for both initial and additional enrollment. A fresh PostgreSQL
schema can enroll its root without a copied fixture.

Copying supporting catalogs qualifies their validation, not their producers.
The [operation inventory](#backend-support-and-drift-checks) identifies native
writes and their catalog-maintenance coverage. Public PostgreSQL opening,
physical backup/restore and search remain unqualified. Logical metadata transfer
uses the internal import/export entry points described above.

For PostgreSQL tests, start a disposable server, never an operator database:

```sh
docker run --detach --rm --name docbank-pg-test \
  --publish 127.0.0.1:15432:5432 \
  --tmpfs /pgdata --env PGDATA=/pgdata/data \
  --env POSTGRES_USER=docbank_test \
  --env POSTGRES_PASSWORD=docbank_test \
  --env POSTGRES_DB=docbank_test postgres:16
docker exec docbank-pg-test pg_isready -h 127.0.0.1 -U docbank_test -d docbank_test
```

Once `pg_isready` reports `accepting connections`, run from the repository root:

```sh
DOCBANK_TEST_POSTGRES_DSN='postgres://docbank_test:docbank_test@127.0.0.1:15432/docbank_test?sslmode=disable' make test-pg
docker rm --force docbank-pg-test
```

The temporary-memory data directory matches CI and avoids filesystem-sync
delays when tests repeatedly create and drop databases. Always remove the
disposable server after the run, including after a failure.
Use `postgres:18` in the first command to check the other CI version. The tests
create and drop a database per test and use separate real owner/runtime logins;
the test administrator needs database, role and event-trigger creation for the
rollback fixtures. These are test privileges, not runtime requirements.
`internal/pgtest` owns database creation and cleanup for both the schema and
node and content-history query tests. `make test-pg` requires the DSN, bypasses
cached test results, and runs these groups under the race detector. It limits
parallel tests to four: each schema installation locks hundreds of catalog
objects, so CPU-count concurrency can exhaust the default server lock table.
The Store suite has an eight-minute timeout for the full contract inventory
at that concurrency; the schema lifecycle suite has a separate three-minute limit.
Without that variable, ordinary `make test` skips PostgreSQL tests.
A supplied but invalid connection fails rather than skipping.

Name PostgreSQL tests in `internal/store` with the `Postgres` suffix. The target
selects them with `-run 'Postgres$'`, so adding a test needs no Makefile edit.
It runs the entire `internal/store/pgschema` package separately.

CI supplies the server through its service-container configuration, uses the
existing Linux cache group, and runs only the PostgreSQL tests in this lane.
Ordinary jobs still cover both SQLite builds. The main-pinned PR dispatcher
uses the workflow from `main` and the checked-out branch's `make test-pg`.
Changes to the target run in that PR; changes to the workflow take effect
after merge. Run both PostgreSQL versions locally and validate workflow edits
with `actionlint`. These checks qualify the schema foundation and shared node
queries, not complete PostgreSQL document-store support.

### Change daemon lifecycle

Exercise foreground, detached, auto-start, restart, mismatch replacement,
concurrent starters, PID reuse, graceful stop, idle timeout, and both Windows
architectures. Keep status/stop discovery permissive and all starter paths
convergent.

### Add a configured provider adapter

Keep portable profile identity and deployment-local runtime inputs separate.
Validate every runtime field in `internal/config`, construct the provider in
`cmd/docbank`, and register it by descriptor fingerprint with the existing
processing worker. Reuse one provider only when all effective endpoint,
transport, credential, and adapter inputs match. Add a typed plan disclosure,
an authenticated daemon test with synthetic provider responses, and focused
tests for missing credentials, consent, retries, and restored work.

The Docling document and ASR runtimes share transport construction, lazy
credential resolution, descriptor conflict checks, and the configured rendition
request guard. `document/docling.DocumentDescriptor` owns the fixed document
capabilities and policy identity; ASR keeps its separately qualified transcript
policy. Each adapter has its own endpoint/deployment disclosure fingerprint.
Both runtimes restrict `operator_network` allowlists to loopback, private, or
shared prefixes. Neither adapter owns vault state or grants consent.
See [Docling document configuration](../configuration.md#document-conversion-with-docling)
for the admission limits and provenance behavior.

### Add configured search reranking

Keep reranking deployment settings in the processing profile's `reranking`
section. Do not add them to `document.ProcessingProfileV1`, because provider
credentials, endpoints, deadlines, and excerpt limits are daemon policy rather
than derivative identity.

The config loader validates the complete section without reading a secret. The
daemon command constructs the existing ZeroEntropy or Cohere adapter, records
its policy fingerprint in the runtime disclosure, and passes the adapter to
`processing.Service`. The service adds a separate
`query_text_and_excerpt` consent request. Retrieval receives only the
source-fenced, revalidated candidate prefix and holds the provider egress lease
until the call returns.

The HTTP route and CLI carry the same opt-in bit. The route projects the
retrieval receipt without requiring `explain`; daemon clients reject an
unrequested, missing, or malformed receipt. Keep the base search path free of
provider construction and calls.

### Change the container image

`Dockerfile` is the only image recipe. `docker-bake.hcl` wraps it in the
`oci-amd64` and `oci-arm64` targets that the central release pipeline builds,
each on a native Linux runner. The pipeline sets `VERSION`, `OCI_VERSION`,
`COMMIT`, `REVISION`, `BUILD_DATE`, and `OCI_OUTPUT_DIR`, then expects
`<arch>.oci.tar` in that directory. Keep those target names, variables, and
file names stable, and keep the binary's version string the same as in the
native archives.

To export both archives locally, use a builder that can write OCI archives for
both architectures, such as a `docker-container` builder:

```bash
docker buildx bake oci
```

The archives go to ignored `dist/` unless `OCI_OUTPUT_DIR` names another
directory.

## Design and documentation updates

For every material design change:

1. Distill local Superpowers specs, plans, and reviews into the relevant
   internal living design page with current mechanics and rationale before
   opening or updating the pull request.
2. Update public architecture when the user-visible model or boundary changes.
3. Update CLI/API references and examples when a contract changes.
4. Keep planned public behavior inside explicit planned callouts.

Keep the working documents in ignored `.superpowers/`; they are not PR
deliverables. The [documentation workflow](../README.md#what-happens-to-specs-and-plans)
describes how to preserve useful decisions and retire the working material.

Do not add a historical decision ledger. Git records prior versions; the
working tree should let a new contributor understand the current system without
replaying them. Keep work, ordering, blockers, and acceptance state outside
the design documentation; these pages carry resulting capability and durable
rationale.

### Timing checks

Tests never wait on the real clock. A wait on the wall clock passes on a quiet
machine and fails on a busy CI runner.

`make lint` runs kit's `sleeptest` and `deadlinetest` analyzers through the
`kennlint` plugin. Outside a synctest bubble, `sleeptest` reports every
`time.Sleep` and every testify `Eventually`, `EventuallyWithT`, or `Never`.
`deadlinetest` reports `context.WithTimeout`, `time.After`, and timers with a
budget below one second. Never suppress these findings. `make lint` and CI
reject a `//nolint:kennlint` directive that names either analyzer.

Run every wait inside `testing/synctest`, where time is fake and advances only
when the bubble is blocked. When the clock lives outside the bubble, drive the
event directly. Examples are a subprocess, a real socket, SQLite, and the
operating system. Stop the daemon instead of waiting for it to idle out. Cancel
the context instead of letting a deadline expire. Make a fake block until the
test releases it. To show that something does not happen, check the condition
that prevents it, or advance fake time past the deadline inside a bubble.

The analyzers cannot read durations in configuration text, such as an
`idle_timeout` written to `config.toml`. The same rule applies to them. A
budget of one second or more may bound a hung test, but the test must never
wait for it to expire.

## Verification contract

Repository workflows use GitHub-hosted runners. The main CI workflow uses
Ubuntu 24.04 for Linux jobs and Windows Server 2025 for Windows amd64. Windows
arm64 uses `windows-11-vs2026-arm`. Each Windows architecture runs four package
shards. Pull requests call `ci.yml` from `main`, so changes to that workflow take
effect for pull requests after merge. Workflow validation runs against the
proposed workflow changes.

The exception is the `CI gate` job in `ci-pr.yml`, which only checks the CI
run's result. It runs on the Namespace profile `namespace-profile-linux-1cpu` so
it does not wait in the GitHub-hosted queue after CI finishes.

Go test jobs run `go test` through gotestsum, which the module pins as a Go
tool. gotestsum reruns each failed test up to two more times. A test that passes
on a rerun lets the job pass, and gotestsum lists it in
`gotestsum/<suite>-reruns.txt`. Linux, pure-Go, and macOS jobs upload the
`gotestsum/*.json` reports on every run and list the ten slowest tests per report
in the job summary. Other Go jobs upload when a test fails or is rerun, or the
job itself is a rerun attempt. Reports use the
`gotestsum-<suite>-attempt-<n>` artifact name and stay for 14 days. A flaky test
therefore stays visible after the job passes, and a passing rerun attempt can
be matched with the earlier failure.

In CI, Vitest and Playwright also retry a failed test up to two more times and
write JSON reports under `frontend/test-results/`. Each test step uploads its
report as the `vitest-<suite>-attempt-<n>` or `playwright-<suite>-attempt-<n>`
artifact for 14 days, whatever the result. A test that passed only on a retry
has Vitest status `passed` with failure messages, or Playwright status `flaky`.
The report-export browser check still requires one unretried pass.

Go test workflows set `GOTRACEBACK=all` for crashes. When diagnosing a hang, call Kit's `stacktest.Dump(t)` before failing an expired wait to log every goroutine's stack.

CLI redaction verification tests run in separate test processes because the PDF worker's 1 GiB RSS ceiling counts all memory in its process. Each child keeps the real PDF checks and the qualified memory limit.

The native PDF engine reclaims retired runtime memory after each page while holding its exclusive backing lease. This keeps decoded module metadata from accumulating between pages under the same RSS ceiling.

Documentation browser checks use Microsoft's public Playwright image matching the installed `@playwright/test` version.
The LibreOffice PDF proof runs in a GHCR image built from `.github/docker/render-pdf/Dockerfile` and tagged by that file's hash.
The Render PDF image workflow (`render-pdf-image.yml`) publishes missing images on `main` pushes or a manual run from a trusted branch, then runs the proofs.
Only its publishing job requests `packages: write`. Update the Ubuntu digest to refresh the installed packages.
Pull requests call `render-pdf.yml` through `render-pdf-pr.yml` with `contents: read` and `cache-mode: read`.
Image lookup is anonymous. If a recipe's image is missing, publish it with the Render PDF image workflow from a trusted branch containing that Dockerfile, then rerun the proofs.
The browser proofs still install Poppler, util-linux, and Chromium on their runners.
The GHCR package must be public so fork pull requests can reuse the image.

Go jobs restore build and module caches through `.github/actions/go-cache-restore`
by group: Linux CGO, Linux pure-Go, lint (with the golangci-lint analysis
cache), and each Windows architecture. Only `main` pushes save, once per group
per day, from the job that builds the most of that group; pull requests restore
without saving so they cannot evict `main`'s caches from the repository quota.
Before each daily save, the owning job retains the build and analysis action
records it used and their referenced outputs, including executable directories.
Trimming reads Go's v1 action records and rejects unknown record formats.
The job rebuilds the module cache from restored downloads through a local file
proxy, dropping stale versions while retaining the current `go.mod` graph,
modules and tools requested by the job, checksum-database records, and the configured proxy fallback.
An exact daily cache hit skips this preparation and trimming. Trimming first appears at the next new daily key.

Owner saves keep the `GOMODCACHE` archive path and remove expanded module
sources with `go clean -modcache`, preserving the `cache` subtree.
Every restore ends with `go mod download` to expand module sources before
the first build or test step. Existing archives keep restoring under the same
cache version. Older workflows reading new archives expand modules in their
first Go step.

Windows jobs put `GOCACHE` and `GOMODCACHE` under `D:\a\_temp`. If D: is
missing on ARM64, setup initializes only a raw `Microsoft NVMe Direct Disk v2`
disk and mounts it as D:. A setup failure logs a warning and keeps both Go cache
paths unchanged. Changing cache paths creates a new cache version, so the first
restore at the new location misses until a `main` push saves it.

Windows jobs also set `TEMP` and `TMP` to `D:\a\_temp\tmp`, so `t.TempDir`
and every test vault use the local disk. The default `TEMP` is on C:, the
runner's network-attached OS disk. While other test packages write, its flushes
average tens of milliseconds and sometimes stall for several seconds. Durable
vault writes flush on every upload and commit, so those stalls add up and fail
tests on timeouts. Setup grants that directory only the user, SYSTEM and
Administrators, as the default `TEMP` does, because private-file checks reject
files that inherit D:'s broader access. Without D:, the job logs a warning and
keeps the default.

Repository commands are defined in `AGENTS.md` and the Makefile. The important
design-specific checks are:

- every Go build, test, and lint uses the `fts5` tag; the complete suite must
  pass with both CGO SQLite and `CGO_ENABLED=0` pure-Go SQLite, as required by
  `AGENTS.md`;
- Linux, macOS, and Windows exercise the real daemon and vault lifecycle, with
  Windows CI covering amd64 and arm64;
- docs build strictly, publish Markdown counterparts, and exclude this internal
  directory;
- API examples are checked against generated OpenAPI when routes change;
- store tests cover transaction rollback and schema invariants, not only happy
  path handlers; and
- cross-filesystem storage behavior is exercised through Kit rather than
  mocked into a second docbank implementation.

## Review posture

Review against Docbank's [trust boundary](../architecture/integrity.md): local
operation, loopback by default with opt-in authenticated network listeners,
one user, one owner per vault, and personal
archive scale. Focus on authentication gaps, non-loopback exposure, data loss,
incompatible daemons, crash ordering, and incorrect authority decisions.
Multi-tenant controls require a separate product decision.
