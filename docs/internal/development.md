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
to 4,500 bytes and leaves to 2,500, except the TUI. Only root help has a golden,
so command examples can improve without maintaining hundreds of snapshots.
After an intentional root change, review and regenerate it from the repository
root:

```sh
go test -tags fts5 ./cmd/docbank -run TestRootHelpGolden -update
```

Keep the CLI reference and guide examples in sync. Error next-step hints belong
at the CLI process boundary; preserve error prefixes and daemon problem text.

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

### PostgreSQL schema lifecycle

`internal/store/pgschema` provides the internal schema installation and
validation foundation. A PostgreSQL-backed vault is not available yet: ordinary
daemon and embedded operations still use SQLite. This package owns the native
vault identity, the core `nodes`, `blobs`,
`blob_checksums`, `content_versions` and `node_allocation` catalogs, and the five
`audit_*` catalogs for canonical records, authority, scopes, baselines and memberships.
It also owns the photo input catalogs and `photo_xmp_inputs` view used for audit
re-export decisions: photo assets/files/sets/receipts, source metadata generations,
heads and technical projections, tags, ingests, provenance version bindings,
derivative-purge suppressions and photo library settings.
Complete mutations, the remaining catalogs, search, transfer, and recovery are
not available yet.

`Setup` takes a caller-owned `database/sql` pool authenticated as `SchemaOwner`
and uses the explicit `Schema` setting. It creates a dedicated schema, canonical
DDL, a UUIDv4 vault identity, and the root directory in one transaction, under a
schema-specific PostgreSQL advisory lock. Repeated setup preserves the identity,
root, and existing data. Existing empty or unrelated schemas, malformed
identities and wrong owners are refused rather than repaired.

The catalogs keep content-version and operation IDs as text, numeric node IDs,
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
GRANT SELECT ON documents.vault_metadata TO docbank_runtime;
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
GRANT SELECT ON documents.ingests, documents.provenance,
    documents.provenance_version_bindings, documents.derivative_purge_suppressions,
    documents.source_metadata_generations, documents.source_metadata_heads,
    documents.photo_technical_metadata, documents.tags, documents.node_tags,
    documents.photo_assets, documents.photo_files, documents.photo_sets,
    documents.photo_change_receipts, documents.photo_library_settings,
    documents.photo_xmp_inputs TO docbank_runtime;
GRANT SELECT, INSERT ON documents.photo_xmp_unsettled TO docbank_runtime;
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
Go still owns which record kinds are valid.

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
readback, not the backfill target query, complete blob publication, or document
mutations, which still depend on catalogs that have not been ported.

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

!!! info "Planned"

    Use shared Go mutation code for logical catalog maintenance on both
    backends. Perform related changes in the same SQL transaction as the node
    or content-version change, and publish subscriber notifications only after
    commit. This keeps one implementation of the operation's meaning; PostgreSQL
    will not gain a second set of these maintenance triggers.

    SQLite currently owns these effects in triggers. They are separate from
    value validation and must not disappear when a write is ported:

    | Change | Required catalog effect |
    | --- | --- |
    | Insert a content version | Remove its blob from `rendition_blob_staging` and `derivative_blob_purge_pending` |
    | Delete a version with person assertions | Advance `document_people_state.binding_epoch` and its update timestamp before cascades remove those assertions |
    | Delete a photo's version | Queue its node in `photo_timeline_pending` |
    | Change a photo node's current version | Queue its node when the version changes |
    | Add or remove an audit membership | Queue eligible photo assets associated with that node in `photo_xmp_unsettled` |
    | Add or remove audit authority | Queue every eligible photo asset in `photo_xmp_unsettled` |
    | Record a checksum | Keep the existing value immutable; reject a conflicting value in Go and omit runtime `UPDATE` privileges |

    Initial and additional enrollment persistence already queue affected assets
    on PostgreSQL through the shared audit writers, as described under
    [shared query qualification](#shared-query-qualification). The remaining
    mutation, cascading deletion and restore paths must preserve these effects
    too; the presence of photo tables does not qualify those operations.

    Shared Go validation must also enforce the audit invariants currently
    covered by SQLite checks: one active authority row at `singleton=1`, known
    record kinds, and positive stored operation sequences and entry counts
    wherever present. Validate records before insertion and the complete
    authority within the transaction, including imports and restores. The
    PostgreSQL write paths must not rely on SQLite constraints for these rules.

    This table identifies the immediate dependencies of the core catalogs,
    not the complete trigger inventory. Bulk and cascading deletions must
    capture affected identities before deleting the rows that identify them.
    Each port must cover normal and audited writes, imports, restores and
    maintenance paths, and verify both commit and rollback effects on both
    backends. Engine-specific index maintenance, such as SQLite FTS5, remains
    part of that engine's search implementation.

    Preserve released SQLite schemas and their triggers. Replace each trigger
    through the supported SQLite layout cutover only when its shared Go owner
    is present. Do not run both owners for an effect such as advancing a binding
    epoch. Complete PostgreSQL document mutations remain unavailable until
    their related catalog effects are implemented; successful SQL alone does
    not qualify a write.

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
existing mutation transactions and error handling; complete mutations and the
remaining catalogs still require SQLite.

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
copying DDL into fixtures. These tests do not qualify the full directory or file mutation
on PostgreSQL.

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
committed between the view's reads. This qualifies the read queries only;
PostgreSQL content mutations and a public vault remain unavailable.

Node and scope audit history also use shared SQL and read-only `REPEATABLE READ`
transactions. Membership, node revision, path, scope head, event count and page
come from one snapshot, even when another operation commits between their
reads. Canonical event projection and cursor ordering remain in the existing
Go implementation. The runtime role needs only `SELECT` on the audit catalogs
for these reads; complete enrollment, audit mutations and full metadata
validation still require SQLite.

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
history against PostgreSQL's high-water mark after a failed insert. It does not
yet run complete metadata validation on PostgreSQL or enable complete audit
writes or import.

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

Initial and additional enrollment previews run against PostgreSQL by node ID or
path. The existing writer transaction keeps their target, retained versions,
trash origins and attached metadata consistent. Additional preview validates
the existing audit history and rejects overlapping scopes. The same builders
produce baseline records on both backends, including provenance bindings,
derivative-purge suppressions and photo settings.

Enrollment persistence uses the shared native writers. Records, authority,
scopes, baselines, memberships and photo re-export queue entries commit or roll
back together. On PostgreSQL, initial enrollment
queues every eligible photo asset once, after inserting all memberships. Later
membership inserts queue eligible originals in the member's asset, including
when the member is a sidecar. Released SQLite layouts retain their triggers as
the sole queue owner; PostgreSQL performs these writes in Go in the same storage
transaction. Neither backend performs filesystem publication during enrollment.

`photo_xmp_inputs` preserves SQLite eligibility and exact change-detection tokens.
The PostgreSQL view uses JSON extraction and ordered string aggregation, with
explicit numeric boolean encoding in tokens. `PhotoXMPTargetsAfter` shares its
filtering, bounds and cursor order; only the candidate join spelling differs.
The parity test creates source data through SQLite APIs, including an ingested
and trashed document, a derivative purge, photo settings, settled receipts,
tags, missing extraction and video. PostgreSQL then previews enrollment and
builds and validates its own audit records. The test compares records, tokens,
queues and target pages with SQLite after commit and rollback, and rejects a
baseline projection that disagrees with canonical history.

This qualifies internal preview, audit-history validation and persistence, not
the complete enrollment or photo publication API. Full metadata validation,
import/restore, removal and other photo mutations remain unavailable on
PostgreSQL. `EnableInitialAudit` still requires complete metadata validation;
do not bypass it. Added input catalogs grant runtime read access only. Their
write paths still require validation and related catalog maintenance.

For PostgreSQL tests, start a disposable server, never an operator database:

```sh
docker run --detach --rm --name docbank-pg-test \
  --publish 127.0.0.1:15432:5432 \
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

Always remove the disposable server after the run, including after a failure.
Use `postgres:18` in the first command to check the other CI version. The tests
create and drop a database per test and use separate real owner/runtime logins;
the test administrator needs database, role and event-trigger creation for the
rollback fixtures. These are test privileges, not runtime requirements.
`internal/pgtest` owns database creation and cleanup for both the schema and
node and content-history query tests. `make test-pg` requires the DSN, bypasses
cached test results, and runs these groups under the race detector. Without
that variable, ordinary `make test` skips PostgreSQL tests.
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

Documentation browser checks use Microsoft's public Playwright image matching the installed `@playwright/test` version.
Render PDF caches LibreOffice and Poppler installers by runner image, architecture, and workflow recipe.
A cache miss downloads the packages and their missing dependencies.
Only `main` pushes save automatically; pull requests only restore.
Explicit manual runs can save: dispatch Render PDF twice on the same branch and runner image to qualify cold and warm installs before merge.

Go jobs restore build and module caches through `.github/actions/go-cache-restore`
by group: Linux CGO, Linux pure-Go, lint (with the golangci-lint analysis
cache), and each Windows architecture. Only `main` pushes save, once per group
per day, from the job that builds the most of that group; pull requests restore
without saving so they cannot evict `main`'s caches from the repository quota.

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
