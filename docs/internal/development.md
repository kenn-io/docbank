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
It confines keyboard shortcuts to the viewer element and destroys an
engine that finishes initializing after the viewer unmounts. Without these
corrections, a hidden preview can replace copied text with an empty string,
and switching documents during initialization can leave three workers running.
The patch rejects an unexpected package version or bundle. Remove it when an
upstream release supplies both fixes, and rerun the document-reader browser
proof before changing the pin. The container frontend stage includes the patch
script and the bundled fallback font.

The PDF rendering CI lane runs `frontend/screenshots/redaction-review.run.mjs`
against both SQLite builds. It executes the document-reader and redaction-review
browser cases with synthetic vaults and the built application. Reader cases cover
activation, clipboard shortcuts, worker cleanup, Markdown form removal, link
isolation, failed module loading and delayed file details. Selecting PDFs in the
inspector must not prepare original-file downloads or start PDF workers.

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
syntax and arity without executing examples.

Use a command group at the root, source each ID, and mark required flags.
Keep descriptions short: at most 12 Long lines for leaves and 20 for parents;
root help and the TUI key table have explicit exceptions. Root help is limited
to 3,600 bytes and leaves to 2,500, except the TUI. Only root help has a golden,
so command examples can improve without maintaining hundreds of snapshots.
After an intentional root change, review and regenerate it from the repository
root:

```sh
go test -tags fts5 ./cmd/docbank -run TestRootHelpGolden -update
```

Keep the CLI reference and guide examples in sync. Error next-step hints belong
at the CLI process boundary; preserve error prefixes and daemon problem text.

Register file/tree commands under `document`, vault identity and maintenance
under `vault`, and email operations under `email`. `report` owns search-count
evidence; leave `search` arguments available for arbitrary query words. Existing
domain groups keep their names. Use `registerGroupedCommand` after installing
the command's flags and children when moving another command. It retains a
hidden root definition for at least one release, with separate Cobra state and
shared handler flag values. Deprecation notices go to stderr at the process
boundary before argument parsing, including errors and help; Cobra's built-in
`Deprecated` field writes to stdout and breaks JSON and raw byte output.
Keep nested compatibility definitions covered by
`command_groups_test.go` and update the CLI reference mapping.
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

A public PostgreSQL-backed vault is not available yet: daemon and embedded
operations still open SQLite. Grouped photo imports, XMP sidecar writing,
provider/rendition, embedding, page and media workers, search, transfer, backup recovery, multi-store placement
and storage-operation jobs remain unqualified.

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
GRANT INSERT, UPDATE(hidden_at, excluded_at, display_file_id,
    display_override_file_id, revision, updated_at)
    ON documents.photo_assets TO docbank_runtime;
GRANT INSERT, DELETE ON documents.photo_change_receipts TO docbank_runtime;
GRANT INSERT, UPDATE(asset_id, role, sidecar_of_file_id, source_provenance_identity)
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
GRANT INSERT, DELETE, UPDATE(name, revision) ON documents.tags TO docbank_runtime;
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
GRANT DELETE ON documents.embedding_input_generations, documents.embedding_vector_sets,
    documents.embedding_vector_spaces, documents.embedding_failures,
    documents.email_heads, documents.email_part_artifacts, documents.email_generations,
    documents.email_body_results, documents.rendition_builds, documents.rendition_artifacts,
    documents.rendition_units, documents.rendition_lexical_segments,
    documents.rendition_lexical_index, documents.rendition_lexical_heads,
    documents.rendition_lexical_generations, documents.rendition_lexical_generation_manifests,
    documents.text_searchable_versions, documents.extracted_text,
    documents.text_extraction_queue TO docbank_runtime;
GRANT INSERT ON documents.derivative_blob_purge_pending,
    documents.rendition_lexical_generations, documents.rendition_lexical_generation_builds,
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

PostgreSQL support is unreleased. Edit the canonical DDL directly; do not assign
layout versions, add compatibility gates, or write migrations for development
schemas. Recreate disposable test databases when the schema changes.

The first release with PostgreSQL support establishes its schema version and
upgrade floor, independently of SQLite layout versions and JSONL format versions.
Subsequent changes require numbered
`NNNNNN_description.up.sql` transitions, with the target version in the filename.
Keep one complete forward chain, and commit each transition and its version
together under the setup lock. Runtime startup never migrates. Once a migration
reaches main or a deployed database it is immutable; fix forward. Recovery uses
a pre-upgrade backup and compatible binary, not down migrations. This discipline
does not change SQLite's JSONL cutover policy.

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
values. Go validators own those values. Physical storage operations are read-only
inputs to validation here; their active-evacuation uniqueness rule remains part
of the future write port rather than a closed state list in the new schema.

Secondary access indexes on the added read-only catalogs are intentionally
deferred. Port them with the operations that need them, including indexes on
foreign-key columns used by cascading deletes; PostgreSQL does not add those
automatically. Keep structural uniqueness in the canonical schema. Enrollment
qualification establishes correctness, not validation or cascade performance
on large vaults; those paths need representative query-plan and workload checks.

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
| [Kata PostgreSQL operations][kata-operations] | `Schema`/`SchemaOwner` naming, owner/runtime role separation, transaction advisory lock | This internal API accepts a pool and has separate setup/validate calls; versioning starts with the first PostgreSQL release, and a public vault is not exposed yet |
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
queues affected photos through `queueAuditPhotosTx`. Deleting authority or
memberships during import/restore still needs that effect. XMP publication,
people candidates and search also have unported effects; their
retained tables do not establish write support.

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

General photo editing, portable backup/restore and public PostgreSQL vault
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
processing workers remain unqualified.

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
loose repair, logical collection and re-ingest. SQLite keeps its released
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
public PostgreSQL vault opening, multi-store placement, storage jobs or recovery.

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
cascades; event publication and derivation workers remain unqualified.

Operation tests use canonical setup and the runtime role, validate complete
metadata and audit replay, and compare derived effects with real SQLite writes.
They force a late invalidation failure to verify rollback, and commit concurrent
writes during history reads to verify snapshot consistency. These capabilities
do not expose a public PostgreSQL vault or qualify grouped photo imports, XMP
sidecar writing, portable metadata import, collection snapshots, reporting,
backup or restore.

#### Tags and saved definitions

Tag creation, assignment, rename and deletion use the same Store methods on
both databases, including audited operations and path-based assignment. Tag
pages retain byte-order sorting, current assignment counts and trash filtering.
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
reads. Canonical event projection and cursor ordering remain in the existing
Go implementation. The runtime role needs only `SELECT` on the audit catalogs
for these reads. Enrollment additionally needs the narrow write grants above.

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
or `DELETE` on that table.

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

The supporting catalogs grant runtime read access only. Their presence does not
qualify their write paths: validation, immutability and same-transaction catalog
maintenance still belong to each operation's port. Public PostgreSQL opening,
`ValidateMetadata` snapshot export, backup/restore, document and photo mutations,
and search remain unavailable.

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

`make lint` runs kit's `sleeptest` and `deadlinetest` analyzers through the
`kennlint` plugin. They report `time.Sleep`, and `context.WithTimeout`,
`time.After`, timers, or `Eventually` calls with a budget below one second, in
tests outside a synctest bubble. A short positive `Never` window is allowed,
since it can only check less.

A deadline finding may carry `//nolint:kennlint // deadlinetest: reason` in three cases: the
asserted result is the deadline expiring; a `select` on a timer shows that an
event does not happen; or the call sits in a helper that runs only inside a
bubble the analyzer can't see across the call. The reason names the case and
what holds the wait. A sleep finding's reason names the real-clock dependency
that keeps it out of a bubble.

Use `testing/synctest` for channels, callbacks, timers, goroutines, and readers
owned by a test. Keep waits for provider HTTP, managed commands, daemons,
sockets, files, OS locks, SQLite, blob stores, process-global state, and held
mutexes on the real clock.

## Verification contract

All repository workflows use GitHub-hosted runners. The main CI workflow uses
Ubuntu 24.04 for Linux jobs and Windows Server 2025 for Windows amd64. Windows
arm64 uses `windows-11-vs2026-arm`. Each Windows architecture runs four package
shards. Pull requests call `ci.yml` from `main`, so changes to that workflow take
effect for pull requests after merge. Workflow validation runs against the
proposed workflow changes.

Go test workflows set `GOTRACEBACK=all` for crashes. When diagnosing a hang, call Kit's `stacktest.Dump(t)` before failing an expired wait to log every goroutine's stack.

CLI redaction verification tests run in separate test processes because the PDF worker's 1 GiB RSS ceiling counts all memory in its process. Each child keeps the real PDF checks and the qualified memory limit.

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

Windows jobs put `GOCACHE` and `GOMODCACHE` under `D:\a\_temp`. If D: is
missing on ARM64, setup initializes only a raw `Microsoft NVMe Direct Disk v2`
disk and mounts it as D:. A setup failure logs a warning and keeps both Go cache
paths unchanged. Changing cache paths creates a new cache version, so the first
restore at the new location misses until a `main` push saves it.

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
