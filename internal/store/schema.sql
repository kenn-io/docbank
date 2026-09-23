-- docbank core schema. Idempotent: applied on every Open.

CREATE TABLE IF NOT EXISTS export_sources (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    request_json BLOB NOT NULL,
    canonical_json BLOB NOT NULL,
    state TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS export_chunks (
    source_id TEXT NOT NULL REFERENCES export_sources(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(source_id,chunk_index)
);
CREATE TABLE IF NOT EXISTS export_members (
    source_id TEXT NOT NULL REFERENCES export_sources(id) ON DELETE CASCADE,
    node_id INTEGER NOT NULL,
    version_id TEXT NOT NULL REFERENCES content_versions(version_id),
    blob_hash TEXT NOT NULL REFERENCES blobs(hash),
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(source_id,node_id,version_id)
);
CREATE INDEX IF NOT EXISTS export_members_version ON export_members(version_id);
CREATE TABLE IF NOT EXISTS export_plans (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    source_id TEXT NOT NULL REFERENCES export_sources(id),
    request_sha256 TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS export_documents (
    plan_id TEXT NOT NULL REFERENCES export_plans(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(plan_id,ordinal)
);
CREATE TABLE IF NOT EXISTS export_role_roots (
    plan_id TEXT NOT NULL REFERENCES export_plans(id) ON DELETE CASCADE,
    blob_hash TEXT NOT NULL REFERENCES blobs(hash),
    PRIMARY KEY(plan_id,blob_hash)
);
CREATE TABLE IF NOT EXISTS export_jobs (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    plan_id TEXT NOT NULL REFERENCES export_plans(id) ON DELETE CASCADE,
    request_sha256 TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    state TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    token TEXT NOT NULL,
    archive_name TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS export_jobs_pending ON export_jobs(state,id);

-- Package snapshots retain exact source versions independently of the
-- short-lived query snapshots and export jobs.
CREATE TABLE IF NOT EXISTS collection_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    vault_uid TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    predecessor_id TEXT REFERENCES collection_snapshots(snapshot_id),
    source_collection_ids_json BLOB NOT NULL DEFAULT '[]',
    member_count INTEGER NOT NULL CHECK (member_count >= 0),
    page_count INTEGER NOT NULL CHECK (page_count >= 0),
    member_hash TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    checksum TEXT NOT NULL,
    sealed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS collection_snapshot_members (
    snapshot_id TEXT NOT NULL REFERENCES collection_snapshots(snapshot_id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    occurrence_id TEXT NOT NULL,
    node_id INTEGER NOT NULL,
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id),
    blob_sha256 TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    family_id TEXT NOT NULL,
    parent_occurrence_id TEXT,
    family_order INTEGER NOT NULL,
    display_name TEXT NOT NULL,
    frozen_fields_json BLOB NOT NULL,
    document_kind TEXT NOT NULL,
    selected_source_pages_json BLOB,
    selected_pdf_sha256 TEXT,
    source_page_count INTEGER NOT NULL DEFAULT 0,
    canonical_json BLOB NOT NULL,
    checksum TEXT NOT NULL,
    PRIMARY KEY (snapshot_id, ordinal),
    UNIQUE (snapshot_id, occurrence_id)
);
CREATE INDEX IF NOT EXISTS collection_snapshot_members_version
    ON collection_snapshot_members(content_version_id);
CREATE TABLE IF NOT EXISTS collection_snapshot_representations (
    snapshot_id TEXT NOT NULL,
    occurrence_id TEXT NOT NULL,
    role TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    status TEXT NOT NULL,
    text_authority TEXT NOT NULL DEFAULT 'none',
    content_version_id TEXT,
    blob_sha256 TEXT,
    size INTEGER,
    media_type TEXT NOT NULL,
    page_number INTEGER,
    verified_page_count INTEGER,
    rendition_build_id TEXT,
    lexical_generation_id TEXT,
    recipe_sha256 TEXT,
    canonical_json BLOB NOT NULL,
    checksum TEXT NOT NULL,
    PRIMARY KEY (snapshot_id, occurrence_id, role, ordinal),
    FOREIGN KEY (snapshot_id, occurrence_id)
        REFERENCES collection_snapshot_members(snapshot_id, occurrence_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS collection_snapshot_representations_version
    ON collection_snapshot_representations(content_version_id);
CREATE TABLE IF NOT EXISTS packages (
    package_id TEXT PRIMARY KEY,
    snapshot_id TEXT REFERENCES collection_snapshots(snapshot_id),
    direction TEXT NOT NULL,
    package_name TEXT NOT NULL,
    party_label TEXT NOT NULL,
    profile_sha256 TEXT NOT NULL,
    profile_json BLOB NOT NULL,
    mapping_sha256 TEXT NOT NULL,
    mapping_json BLOB NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    manifest_blob_sha256 TEXT NOT NULL,
    predecessor_package_id TEXT REFERENCES packages(package_id),
    relation TEXT NOT NULL,
    ingest_id TEXT REFERENCES ingests(id),
    export_plan_id TEXT,
    state TEXT NOT NULL,
    produced_on TEXT,
    created_at TEXT NOT NULL,
    completed_at TEXT
);
CREATE INDEX IF NOT EXISTS packages_history
    ON packages(predecessor_package_id, created_at, package_id);
CREATE TABLE IF NOT EXISTS package_volumes (
    package_id TEXT NOT NULL REFERENCES packages(package_id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    volume_name TEXT NOT NULL,
    declared_root TEXT NOT NULL,
    mapped_root TEXT NOT NULL,
    resolved_root_sha256 TEXT NOT NULL,
    PRIMARY KEY (package_id, volume_name)
);
CREATE TRIGGER IF NOT EXISTS collection_snapshots_immutable_update
BEFORE UPDATE ON collection_snapshots BEGIN
    SELECT RAISE(ABORT, 'collection snapshots are immutable');
END;
CREATE TRIGGER IF NOT EXISTS collection_snapshot_members_immutable_update
BEFORE UPDATE ON collection_snapshot_members BEGIN
    SELECT RAISE(ABORT, 'collection snapshot members are immutable');
END;
CREATE TRIGGER IF NOT EXISTS collection_snapshot_representations_immutable_update
BEFORE UPDATE ON collection_snapshot_representations BEGIN
    SELECT RAISE(ABORT, 'collection snapshot representations are immutable');
END;
-- Bates labels are globally unique even when namespace profiles differ.
CREATE TABLE IF NOT EXISTS bates_namespaces (
    namespace_id TEXT PRIMARY KEY,
    prefix TEXT NOT NULL,
    suffix TEXT NOT NULL,
    padding INTEGER NOT NULL CHECK (padding BETWEEN 1 AND 10),
    created_at TEXT NOT NULL,
    UNIQUE(prefix,suffix)
);
CREATE TABLE IF NOT EXISTS bates_namespace_cursors (
    namespace_id TEXT PRIMARY KEY REFERENCES bates_namespaces(namespace_id),
    next_sequence INTEGER NOT NULL CHECK (next_sequence >= 1)
);
CREATE TABLE IF NOT EXISTS bates_allocations (
    allocation_id TEXT PRIMARY KEY,
    operation_id TEXT NOT NULL UNIQUE,
    namespace_id TEXT NOT NULL REFERENCES bates_namespaces(namespace_id),
    snapshot_id TEXT NOT NULL REFERENCES collection_snapshots(snapshot_id),
    request_sha256 TEXT NOT NULL,
    recipe_sha256 TEXT NOT NULL,
    start_sequence INTEGER NOT NULL CHECK (start_sequence >= 1),
    end_sequence INTEGER NOT NULL CHECK (end_sequence >= start_sequence),
    state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    committed_at TEXT
);
CREATE INDEX IF NOT EXISTS bates_allocations_namespace ON bates_allocations(namespace_id,start_sequence);
CREATE TABLE IF NOT EXISTS bates_page_labels (
    allocation_id TEXT NOT NULL REFERENCES bates_allocations(allocation_id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    namespace_id TEXT NOT NULL REFERENCES bates_namespaces(namespace_id),
    sequence INTEGER NOT NULL CHECK (sequence >= 1),
    occurrence_id TEXT NOT NULL,
    source_page INTEGER NOT NULL CHECK (source_page >= 1),
    output_page INTEGER NOT NULL CHECK (output_page >= 1),
    label TEXT NOT NULL UNIQUE,
    PRIMARY KEY(allocation_id,ordinal),
    UNIQUE(namespace_id,sequence)
);
-- A verified Bates export is permanent evidence. It is intentionally rooted
-- independently of short-lived export jobs and browser download tickets.
CREATE TABLE IF NOT EXISTS bates_artifacts (
    artifact_id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL UNIQUE REFERENCES bates_allocations(allocation_id),
    blob_hash TEXT NOT NULL REFERENCES blobs(hash),
    size INTEGER NOT NULL CHECK (size > 0),
    media_type TEXT NOT NULL,
    page_count INTEGER NOT NULL CHECK (page_count > 0),
    recipe_json BLOB NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    state TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS bates_artifacts_history ON bates_artifacts(created_at,artifact_id);
CREATE TABLE IF NOT EXISTS bates_artifact_pages (
    artifact_id TEXT NOT NULL REFERENCES bates_artifacts(artifact_id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    occurrence_id TEXT NOT NULL,
    source_blob_sha256 TEXT NOT NULL,
    source_page INTEGER NOT NULL CHECK (source_page >= 1),
    output_page INTEGER NOT NULL CHECK (output_page >= 1),
    label TEXT NOT NULL,
    PRIMARY KEY(artifact_id,ordinal)
);
CREATE TRIGGER IF NOT EXISTS bates_artifacts_immutable_update
BEFORE UPDATE ON bates_artifacts BEGIN
    SELECT RAISE(ABORT, 'Bates artifacts are immutable');
END;
CREATE TRIGGER IF NOT EXISTS bates_artifact_pages_immutable_update
BEFORE UPDATE ON bates_artifact_pages BEGIN
    SELECT RAISE(ABORT, 'Bates artifact pages are immutable');
END;
CREATE TRIGGER IF NOT EXISTS bates_namespaces_immutable_update
BEFORE UPDATE ON bates_namespaces BEGIN
    SELECT RAISE(ABORT, 'Bates namespace identity is immutable');
END;
CREATE TRIGGER IF NOT EXISTS bates_namespace_cursors_monotone
BEFORE UPDATE ON bates_namespace_cursors
WHEN NEW.namespace_id<>OLD.namespace_id OR NEW.next_sequence<OLD.next_sequence
BEGIN SELECT RAISE(ABORT, 'Bates cursor cannot rewind'); END;
CREATE TRIGGER IF NOT EXISTS bates_allocations_transition_guard
BEFORE UPDATE ON bates_allocations
WHEN NEW.allocation_id<>OLD.allocation_id OR NEW.operation_id<>OLD.operation_id
 OR NEW.namespace_id<>OLD.namespace_id OR NEW.snapshot_id<>OLD.snapshot_id
 OR NEW.request_sha256<>OLD.request_sha256 OR NEW.recipe_sha256<>OLD.recipe_sha256
 OR NEW.start_sequence<>OLD.start_sequence OR NEW.end_sequence<>OLD.end_sequence
 OR NEW.created_at<>OLD.created_at OR OLD.state<>'reserved'
 OR NEW.state NOT IN ('committed','abandoned')
 OR (NEW.state='committed' AND (OLD.committed_at IS NOT NULL OR NEW.committed_at IS NULL))
 OR (NEW.state='abandoned' AND NEW.committed_at IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'Bates allocation identity or terminal state is immutable'); END;
CREATE TRIGGER IF NOT EXISTS bates_page_labels_immutable_update
BEFORE UPDATE ON bates_page_labels BEGIN
    SELECT RAISE(ABORT, 'Bates page labels are immutable');
END;
CREATE TRIGGER IF NOT EXISTS package_volumes_immutable_update
BEFORE UPDATE ON package_volumes BEGIN
    SELECT RAISE(ABORT, 'package volumes are immutable');
END;
CREATE TRIGGER IF NOT EXISTS packages_transition_guard
BEFORE UPDATE ON packages
WHEN NEW.package_id<>OLD.package_id OR NEW.direction<>OLD.direction
    OR NEW.package_name<>OLD.package_name OR NEW.party_label<>OLD.party_label
    OR NEW.profile_sha256<>OLD.profile_sha256 OR NEW.profile_json<>OLD.profile_json
    OR NEW.mapping_sha256<>OLD.mapping_sha256 OR NEW.mapping_json<>OLD.mapping_json
    OR NEW.manifest_sha256<>OLD.manifest_sha256
    OR NEW.manifest_blob_sha256<>OLD.manifest_blob_sha256
    OR NEW.relation<>OLD.relation OR NEW.created_at<>OLD.created_at
    OR NEW.snapshot_id IS NOT OLD.snapshot_id AND OLD.snapshot_id IS NOT NULL
    OR NEW.completed_at IS NOT OLD.completed_at AND OLD.completed_at IS NOT NULL
    OR NEW.export_plan_id IS NOT OLD.export_plan_id AND OLD.export_plan_id IS NOT NULL
    OR NEW.predecessor_package_id IS NOT OLD.predecessor_package_id
    OR NEW.ingest_id IS NOT OLD.ingest_id OR NEW.produced_on IS NOT OLD.produced_on
BEGIN
    SELECT RAISE(ABORT, 'package identity is immutable');
END;

-- Received load-file rows and their scoped sender labels are immutable. The
-- head records which publication won an idempotent record key.
CREATE TABLE IF NOT EXISTS package_records (
    package_id TEXT NOT NULL REFERENCES packages(package_id) ON DELETE CASCADE,
    row_id TEXT NOT NULL,
    load_file TEXT NOT NULL,
    row_ordinal INTEGER NOT NULL CHECK (row_ordinal >= 1),
    occurrence_id TEXT NOT NULL,
    raw_json BLOB NOT NULL,
    raw_sha256 TEXT NOT NULL,
    sensitive INTEGER NOT NULL CHECK (sensitive IN (0,1)),
    PRIMARY KEY (package_id, row_id),
    UNIQUE (package_id, occurrence_id)
);
CREATE TRIGGER IF NOT EXISTS package_records_immutable_update
BEFORE UPDATE ON package_records BEGIN
    SELECT RAISE(ABORT, 'package records are immutable');
END;
CREATE TABLE IF NOT EXISTS package_labels (
    package_id TEXT NOT NULL REFERENCES packages(package_id) ON DELETE CASCADE,
    provenance TEXT NOT NULL,
    label_set TEXT NOT NULL,
    label TEXT NOT NULL,
    label_sort_key TEXT NOT NULL,
    occurrence_id TEXT NOT NULL,
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id),
    artifact_id TEXT,
    page_number INTEGER,
    page_state TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    PRIMARY KEY (package_id, provenance, label_set, label, endpoint, occurrence_id)
);
CREATE INDEX IF NOT EXISTS package_labels_lookup ON package_labels(label, provenance, package_id);
CREATE TRIGGER IF NOT EXISTS package_labels_immutable_update
BEFORE UPDATE ON package_labels BEGIN
    SELECT RAISE(ABORT, 'package labels are immutable');
END;
CREATE TABLE IF NOT EXISTS package_import_jobs (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    preflight_id TEXT NOT NULL,
    package_id TEXT NOT NULL REFERENCES packages(package_id),
    claim_owner TEXT,
    lease_expires_at TEXT,
    state TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    token TEXT NOT NULL,
    job_json BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(owner, operation_id),
    UNIQUE(package_id)
);
CREATE INDEX IF NOT EXISTS package_import_jobs_pending ON package_import_jobs(state,id);
CREATE TABLE IF NOT EXISTS package_import_receipts (
    receipt_id TEXT PRIMARY KEY,
    package_id TEXT NOT NULL REFERENCES packages(package_id),
    record_key TEXT NOT NULL,
    occurrence_id TEXT NOT NULL,
    content_version_id TEXT REFERENCES content_versions(version_id),
    state TEXT NOT NULL,
    receipt_json BLOB NOT NULL,
    recorded_at TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS package_import_receipts_immutable_update
BEFORE UPDATE ON package_import_receipts BEGIN
    SELECT RAISE(ABORT, 'package import receipts are immutable');
END;
CREATE TABLE IF NOT EXISTS package_import_heads (
    package_id TEXT NOT NULL REFERENCES packages(package_id),
    record_key TEXT NOT NULL,
    receipt_id TEXT NOT NULL REFERENCES package_import_receipts(receipt_id),
    PRIMARY KEY (package_id, record_key)
);

-- Physical page frames are independent of optional renderer recipes.
CREATE TABLE IF NOT EXISTS page_documents (
    version_id TEXT PRIMARY KEY REFERENCES content_versions(version_id) ON DELETE CASCADE,
    canonical_json BLOB NOT NULL,
    checksum TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS page_frames (
    version_id TEXT NOT NULL REFERENCES page_documents(version_id) ON DELETE CASCADE,
    page INTEGER NOT NULL,
    canonical_json BLOB NOT NULL,
    checksum TEXT NOT NULL,
    PRIMARY KEY(version_id,page)
);
CREATE TABLE IF NOT EXISTS page_recipes (
    checksum TEXT PRIMARY KEY,
    canonical_json BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS page_images (
    version_id TEXT NOT NULL,
    page INTEGER NOT NULL,
    recipe_sha256 TEXT NOT NULL REFERENCES page_recipes(checksum),
    canonical_json BLOB NOT NULL,
    checksum TEXT NOT NULL,
    blob_hash TEXT NOT NULL REFERENCES blobs(hash),
    PRIMARY KEY(version_id,recipe_sha256,page),
    FOREIGN KEY(version_id,page) REFERENCES page_frames(version_id,page) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS page_render_jobs (
    id TEXT PRIMARY KEY,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    request_sha256 TEXT NOT NULL,
    request_json BLOB NOT NULL,
    state TEXT NOT NULL,
    results_json BLOB NOT NULL,
    failure_code TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    token TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS page_render_jobs_pending ON page_render_jobs(state,created_at,id);
CREATE INDEX IF NOT EXISTS page_render_jobs_request ON page_render_jobs(request_sha256);

-- One stable logical identity follows the vault through JSONL backup and
-- restore. Filesystem location is deliberately not identity.
CREATE TABLE IF NOT EXISTS vault_metadata (
    singleton      INTEGER PRIMARY KEY CHECK (singleton = 1),
    vault_uid      TEXT NOT NULL UNIQUE,
    schema_version INTEGER NOT NULL CHECK (schema_version >= 1)
);

-- AUTOINCREMENT: node ids are stored as origins (trash_parent) and will be
-- handed to agents over the HTTP API; a reused rowid would silently retarget
-- those references at an unrelated node.
CREATE TABLE IF NOT EXISTS nodes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id     INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('dir', 'file')),
    current_version_id TEXT,
    revision      INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL,
    modified_at   TEXT NOT NULL,
    trashed_at    TEXT,
    trash_parent  INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
    trash_name    TEXT,
    CHECK ((kind = 'file') = (current_version_id IS NOT NULL)),
    FOREIGN KEY (id, current_version_id)
        REFERENCES content_versions(node_id, version_id)
        DEFERRABLE INITIALLY DEFERRED
);

-- Exactly one root. SQLite treats NULLs as distinct in unique indexes, so
-- uniqueness of the NULL parent needs a constant-expression partial index.
CREATE UNIQUE INDEX IF NOT EXISTS one_root ON nodes((1)) WHERE parent_id IS NULL;

-- Sibling names are unique among LIVE nodes only; trashed nodes never block
-- reuse of a name.
CREATE UNIQUE INDEX IF NOT EXISTS live_sibling_names
    ON nodes(parent_id, name) WHERE trashed_at IS NULL;

CREATE INDEX IF NOT EXISTS nodes_parent ON nodes(parent_id);
CREATE INDEX IF NOT EXISTS nodes_parent_name_id ON nodes(parent_id, name, id);
CREATE INDEX IF NOT EXISTS nodes_live_modified ON nodes(modified_at DESC, name, id)
    WHERE trashed_at IS NULL;
CREATE INDEX IF NOT EXISTS nodes_trashed ON nodes(trashed_at) WHERE trashed_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS blobs (
    hash       TEXT PRIMARY KEY,
    size       INTEGER NOT NULL CHECK (size >= 0),
    created_at TEXT NOT NULL
);

-- Package previews are expiring control state. Full normalized manifests and
-- diagnostics live in staged blobs held only for the lifetime of this row.
CREATE TABLE IF NOT EXISTS package_preflights (
    preflight_id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_locator TEXT NOT NULL DEFAULT '',
    profile_sha256 TEXT NOT NULL,
    profile_json TEXT NOT NULL DEFAULT '',
    mapping_sha256 TEXT NOT NULL,
    mapping_json TEXT NOT NULL DEFAULT '',
    manifest_sha256 TEXT NOT NULL,
    manifest_blob_sha256 TEXT NOT NULL,
    diagnostics_blob_sha256 TEXT,
    canonical_json BLOB NOT NULL,
    diagnostics_json BLOB NOT NULL,
    blocking INTEGER NOT NULL CHECK (blocking IN (0,1)),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS package_preflights_expiry ON package_preflights(expires_at);

-- MD5 is auxiliary interoperability metadata over the same exact logical
-- bytes. SHA-256 remains the sole content identity and authority.
CREATE TABLE IF NOT EXISTS blob_checksums (
    blob_sha256 TEXT PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
    md5         TEXT NOT NULL
        CHECK (length(md5) = 32 AND md5 NOT GLOB '*[^0-9a-f]*')
);

CREATE TRIGGER IF NOT EXISTS blob_checksums_immutable_update
BEFORE UPDATE ON blob_checksums BEGIN
    SELECT RAISE(ABORT, 'blob checksum records are immutable');
END;

-- Source metadata is immutable evidence derived only from verified original
-- bytes. New extractor behavior creates a new generation; it never rewrites
-- an older observation. The small head table selects the active generation.
CREATE TABLE IF NOT EXISTS source_metadata_generations (
    generation_id        TEXT PRIMARY KEY,
    source_sha256        TEXT NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
    contract_version     TEXT NOT NULL,
    extractor_fingerprint TEXT NOT NULL,
    canonical_json       BLOB NOT NULL,
    checksum             TEXT NOT NULL,
    created_at           TEXT NOT NULL,
    UNIQUE (source_sha256, contract_version, extractor_fingerprint),
    UNIQUE (source_sha256, generation_id)
);

CREATE TABLE IF NOT EXISTS source_metadata_heads (
    source_sha256 TEXT PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
    generation_id TEXT NOT NULL UNIQUE
        REFERENCES source_metadata_generations(generation_id) ON DELETE CASCADE,
    published_at TEXT NOT NULL,
    FOREIGN KEY (source_sha256, generation_id)
        REFERENCES source_metadata_generations(source_sha256, generation_id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS source_metadata_generations_immutable_update
BEFORE UPDATE ON source_metadata_generations BEGIN
    SELECT RAISE(ABORT, 'source metadata generations are immutable');
END;

CREATE TABLE IF NOT EXISTS document_event_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1), contract_version TEXT NOT NULL,
    deriver_fingerprint TEXT NOT NULL, input_epoch INTEGER NOT NULL CHECK (input_epoch > 0),
    publication_epoch INTEGER NOT NULL CHECK (publication_epoch > 0), updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS document_event_generations (
    generation_id TEXT PRIMARY KEY,
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    contract_version TEXT NOT NULL, deriver_fingerprint TEXT NOT NULL, inputs_sha256 TEXT NOT NULL,
    document_kind TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    checksum TEXT NOT NULL, event_count INTEGER NOT NULL CHECK (event_count >= 0), created_at TEXT NOT NULL,
    UNIQUE (content_version_id, generation_id)
);

CREATE TABLE IF NOT EXISTS document_event_heads (
    content_version_id TEXT PRIMARY KEY REFERENCES content_versions(version_id) ON DELETE CASCADE,
    generation_id TEXT NOT NULL UNIQUE REFERENCES document_event_generations(generation_id) ON DELETE CASCADE,
    input_epoch INTEGER NOT NULL, published_at TEXT NOT NULL,
    FOREIGN KEY (content_version_id, generation_id)
        REFERENCES document_event_generations(content_version_id, generation_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS document_events (
    generation_id TEXT NOT NULL REFERENCES document_event_generations(generation_id) ON DELETE CASCADE,
    event_id TEXT NOT NULL, source_key TEXT NOT NULL, date_kind TEXT NOT NULL,
    source_kind_raw TEXT NOT NULL, date_value TEXT NOT NULL, raw_value TEXT NOT NULL,
    precision TEXT NOT NULL, fraction_digits INTEGER NOT NULL CHECK (fraction_digits BETWEEN 0 AND 9),
    timezone_kind TEXT NOT NULL, zone_text TEXT NOT NULL, offset_seconds INTEGER,
    axis_key TEXT NOT NULL, utc_key TEXT, claim_basis TEXT NOT NULL, parse_confidence TEXT NOT NULL,
    evidence_kind TEXT NOT NULL, evidence_id TEXT NOT NULL, evidence_sha256 TEXT NOT NULL,
    evidence_locator BLOB NOT NULL, sensitive INTEGER NOT NULL CHECK (sensitive IN (0, 1)),
    PRIMARY KEY (generation_id, event_id),
    UNIQUE (generation_id, source_key, date_kind)
);

CREATE TABLE IF NOT EXISTS document_event_actors (
    generation_id TEXT NOT NULL, event_id TEXT NOT NULL, role TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0), actor_key TEXT NOT NULL,
    display_name TEXT NOT NULL, address TEXT NOT NULL, claim_json BLOB NOT NULL,
    evidence_kind TEXT NOT NULL, evidence_id TEXT NOT NULL,
    sensitive INTEGER NOT NULL CHECK (sensitive IN (0, 1)),
    PRIMARY KEY (generation_id, event_id, role, ordinal),
    FOREIGN KEY (generation_id, event_id)
        REFERENCES document_events(generation_id, event_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS document_event_primaries (
    generation_id TEXT NOT NULL, scope_class TEXT NOT NULL, disclosure TEXT NOT NULL,
    event_id TEXT NOT NULL, rule_id TEXT NOT NULL, reason TEXT NOT NULL,
    PRIMARY KEY (generation_id, scope_class, disclosure),
    FOREIGN KEY (generation_id, event_id)
        REFERENCES document_events(generation_id, event_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS document_event_builds (
    operation_id TEXT PRIMARY KEY, request_sha256 TEXT NOT NULL, deriver_fingerprint TEXT NOT NULL,
    state TEXT NOT NULL, target_epoch INTEGER NOT NULL,
    scanned INTEGER NOT NULL, published INTEGER NOT NULL, unavailable INTEGER NOT NULL,
    failed INTEGER NOT NULL, started_at TEXT NOT NULL, updated_at TEXT NOT NULL, finished_at TEXT
);

CREATE TABLE IF NOT EXISTS provenance_version_bindings (
    provenance_identity TEXT NOT NULL REFERENCES provenance(identity) ON DELETE CASCADE,
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    observed_at TEXT NOT NULL, basis_ref TEXT NOT NULL,
    PRIMARY KEY (provenance_identity, content_version_id)
);

CREATE TABLE IF NOT EXISTS document_event_dirty (
    content_version_id TEXT PRIMARY KEY REFERENCES content_versions(version_id) ON DELETE CASCADE,
    revision INTEGER NOT NULL, reason TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS document_event_attempts (
    content_version_id TEXT PRIMARY KEY REFERENCES content_versions(version_id) ON DELETE CASCADE,
    input_epoch INTEGER NOT NULL, input_revision INTEGER NOT NULL, inputs_sha256 TEXT NOT NULL,
    state TEXT NOT NULL, diagnostic_json BLOB NOT NULL, attempted_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS provenance_version_bindings_version
    ON provenance_version_bindings(content_version_id);

CREATE TRIGGER IF NOT EXISTS document_event_generations_immutable_update
BEFORE UPDATE ON document_event_generations BEGIN
    SELECT RAISE(ABORT, 'document event generations are immutable');
END;

CREATE TRIGGER IF NOT EXISTS document_events_immutable_update
BEFORE UPDATE ON document_events BEGIN
    SELECT RAISE(ABORT, 'document events are immutable');
END;

CREATE TRIGGER IF NOT EXISTS document_event_actors_immutable_update
BEFORE UPDATE ON document_event_actors BEGIN
    SELECT RAISE(ABORT, 'document event actors are immutable');
END;

CREATE TRIGGER IF NOT EXISTS document_event_primaries_immutable_update
BEFORE UPDATE ON document_event_primaries BEGIN
    SELECT RAISE(ABORT, 'document event primaries are immutable');
END;

CREATE TRIGGER IF NOT EXISTS document_event_heads_deleted
AFTER DELETE ON document_event_heads BEGIN
    UPDATE document_event_state SET publication_epoch=publication_epoch+1,
        updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE singleton=1;
END;

CREATE TRIGGER IF NOT EXISTS provenance_version_bindings_immutable_update
BEFORE UPDATE ON provenance_version_bindings BEGIN
    SELECT RAISE(ABORT, 'provenance version bindings are immutable');
END;

-- Physical placement authority is store-scoped. The logical blobs table says
-- which content Docbank retains; these rows say where verified bytes live.
-- Lifecycle and placement policy stay in Go.
CREATE TABLE IF NOT EXISTS blob_stores (
    store_id        TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    kind            TEXT NOT NULL,
    role            TEXT NOT NULL,
    lifecycle       TEXT NOT NULL,
    binding         TEXT NOT NULL,
    ownership_epoch TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS one_primary_blob_store
    ON blob_stores((1)) WHERE role = 'primary';

CREATE TABLE IF NOT EXISTS blob_locations (
    blob_hash    TEXT NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
    store_id     TEXT NOT NULL REFERENCES blob_stores(store_id),
    generation   TEXT NOT NULL,
    kind         TEXT NOT NULL,
    encoding     TEXT,
    stored_size  INTEGER NOT NULL CHECK (stored_size >= 0),
    pack_eligible INTEGER NOT NULL CHECK (pack_eligible IN (0, 1)),
    PRIMARY KEY (blob_hash, store_id)
);

CREATE INDEX IF NOT EXISTS blob_locations_store
    ON blob_locations(store_id, blob_hash);

CREATE TABLE IF NOT EXISTS blob_packs (
    store_id     TEXT NOT NULL REFERENCES blob_stores(store_id),
    pack_id      TEXT NOT NULL,
    entry_count  INTEGER NOT NULL CHECK (entry_count >= 0),
    stored_bytes INTEGER NOT NULL CHECK (stored_bytes >= 0),
    created_at   TEXT NOT NULL,
    scan_hash             TEXT NOT NULL DEFAULT '',
    live_entries          INTEGER NOT NULL DEFAULT 0 CHECK (live_entries >= 0),
    live_stored_bytes     INTEGER NOT NULL DEFAULT 0 CHECK (live_stored_bytes >= 0),
    live_raw_bytes        INTEGER NOT NULL DEFAULT 0 CHECK (live_raw_bytes >= 0),
    max_live_stored_len   INTEGER NOT NULL DEFAULT 0 CHECK (max_live_stored_len >= 0),
    max_live_raw_len      INTEGER NOT NULL DEFAULT 0 CHECK (max_live_raw_len >= 0),
    PRIMARY KEY (store_id, pack_id)
);

CREATE TABLE IF NOT EXISTS blob_pack_entries (
    blob_hash   TEXT NOT NULL,
    store_id    TEXT NOT NULL,
    pack_id     TEXT NOT NULL,
    pack_offset INTEGER NOT NULL CHECK (pack_offset >= 0),
    stored_len  INTEGER NOT NULL CHECK (stored_len >= 0),
    raw_len     INTEGER NOT NULL CHECK (raw_len >= 0),
    flags       INTEGER NOT NULL CHECK (flags BETWEEN 0 AND 255),
    crc32c      INTEGER NOT NULL CHECK (crc32c BETWEEN 0 AND 4294967295),
    PRIMARY KEY (blob_hash, store_id),
    FOREIGN KEY (store_id, pack_id)
        REFERENCES blob_packs(store_id, pack_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS blob_pack_entries_pack
    ON blob_pack_entries(store_id, pack_id, blob_hash);
CREATE INDEX IF NOT EXISTS blob_pack_entries_store_hash
    ON blob_pack_entries(store_id, blob_hash);

-- Durable storage work is resumable deployment state. Request and receipt
-- shapes are versioned and validated in Go; SQLite owns only atomic progress
-- and bounded lifecycle bookkeeping.
CREATE TABLE IF NOT EXISTS storage_operations (
    operation_id      TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,
    source_store_id   TEXT REFERENCES blob_stores(store_id) ON DELETE SET NULL,
    request_version   INTEGER NOT NULL CHECK (request_version > 0),
    request_digest    TEXT NOT NULL,
    request_json      TEXT NOT NULL,
    plan_json         TEXT NOT NULL,
    state             TEXT NOT NULL,
    cursor            TEXT NOT NULL DEFAULT '',
    total_objects     INTEGER NOT NULL DEFAULT 0 CHECK (total_objects >= 0),
    completed_objects INTEGER NOT NULL DEFAULT 0 CHECK (completed_objects >= 0),
    copied_objects    INTEGER NOT NULL DEFAULT 0 CHECK (copied_objects >= 0),
    copied_bytes      INTEGER NOT NULL DEFAULT 0 CHECK (copied_bytes >= 0),
    cancel_requested  INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
    error             TEXT NOT NULL DEFAULT '',
    receipt_json      TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    finished_at       TEXT,
    retention_until   TEXT
);

CREATE INDEX IF NOT EXISTS storage_operations_state
    ON storage_operations(state, created_at, operation_id);
CREATE INDEX IF NOT EXISTS storage_operations_retention
    ON storage_operations(retention_until)
    WHERE retention_until IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS one_active_evacuation_per_store
    ON storage_operations(source_store_id)
    WHERE kind = 'evacuate' AND state IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS storage_operation_stores (
    operation_id TEXT NOT NULL REFERENCES storage_operations(operation_id) ON DELETE CASCADE,
    store_id     TEXT NOT NULL REFERENCES blob_stores(store_id) ON DELETE CASCADE,
    role         TEXT NOT NULL CHECK (role IN ('source', 'destination')),
    PRIMARY KEY (operation_id, store_id)
);

CREATE INDEX IF NOT EXISTS storage_operation_stores_store
    ON storage_operation_stores(store_id, operation_id);

CREATE TABLE IF NOT EXISTS storage_operation_cleanup (
    operation_id  TEXT NOT NULL REFERENCES storage_operations(operation_id) ON DELETE CASCADE,
    store_id      TEXT NOT NULL REFERENCES blob_stores(store_id),
    loose_hash    TEXT NOT NULL DEFAULT '',
    loose_encoding INTEGER NOT NULL DEFAULT 0,
    pack_id       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (operation_id, store_id, loose_hash, loose_encoding, pack_id)
);

-- GC removes logical membership and records exact loose objects atomically.
-- Physical retirement consumes these rows only after the backend confirms
-- removal, so a transient failure remains retryable after process restart.
CREATE TABLE IF NOT EXISTS gc_loose_retirements (
    store_id       TEXT NOT NULL REFERENCES blob_stores(store_id),
    blob_hash      TEXT NOT NULL,
    loose_encoding INTEGER NOT NULL,
    PRIMARY KEY (store_id, blob_hash, loose_encoding)
);

-- Bounded maintenance reads pack summaries instead of rescanning every mapping.
-- These triggers maintain physical catalog projections only; document liveness
-- remains Go-owned and is expressed by inserting or deleting blobs rows.
CREATE TRIGGER IF NOT EXISTS blob_pack_summary_mapping_insert
AFTER INSERT ON blob_pack_entries
WHEN EXISTS (SELECT 1 FROM blobs WHERE hash=NEW.blob_hash)
BEGIN
    UPDATE blob_packs SET
        scan_hash=CASE WHEN scan_hash='' THEN NEW.blob_hash ELSE scan_hash END,
        live_entries=live_entries+1,
        live_stored_bytes=live_stored_bytes+NEW.stored_len,
        live_raw_bytes=live_raw_bytes+NEW.raw_len,
        max_live_stored_len=MAX(max_live_stored_len, NEW.stored_len),
        max_live_raw_len=MAX(max_live_raw_len, NEW.raw_len)
    WHERE store_id=NEW.store_id AND pack_id=NEW.pack_id;
END;

CREATE TRIGGER IF NOT EXISTS blob_pack_summary_mapping_delete
AFTER DELETE ON blob_pack_entries
WHEN EXISTS (SELECT 1 FROM blobs WHERE hash=OLD.blob_hash)
BEGIN
    UPDATE blob_packs SET
        live_entries=live_entries-1,
        live_stored_bytes=live_stored_bytes-OLD.stored_len,
        live_raw_bytes=live_raw_bytes-OLD.raw_len,
        max_live_stored_len=CASE WHEN max_live_stored_len=OLD.stored_len
            THEN COALESCE((SELECT MAX(i.stored_len) FROM blob_pack_entries i
                JOIN blobs b ON b.hash=i.blob_hash
                WHERE i.store_id=OLD.store_id AND i.pack_id=OLD.pack_id),0)
            ELSE max_live_stored_len END,
        max_live_raw_len=CASE WHEN max_live_raw_len=OLD.raw_len
            THEN COALESCE((SELECT MAX(i.raw_len) FROM blob_pack_entries i
                JOIN blobs b ON b.hash=i.blob_hash
                WHERE i.store_id=OLD.store_id AND i.pack_id=OLD.pack_id),0)
            ELSE max_live_raw_len END
    WHERE store_id=OLD.store_id AND pack_id=OLD.pack_id;
END;

CREATE TRIGGER IF NOT EXISTS blob_pack_summary_mapping_update
AFTER UPDATE ON blob_pack_entries
WHEN EXISTS (SELECT 1 FROM blobs WHERE hash=OLD.blob_hash)
BEGIN
    UPDATE blob_packs SET
        live_entries=live_entries-1,
        live_stored_bytes=live_stored_bytes-OLD.stored_len,
        live_raw_bytes=live_raw_bytes-OLD.raw_len,
        max_live_stored_len=COALESCE((SELECT MAX(i.stored_len) FROM blob_pack_entries i
            JOIN blobs b ON b.hash=i.blob_hash
            WHERE i.store_id=OLD.store_id AND i.pack_id=OLD.pack_id),0),
        max_live_raw_len=COALESCE((SELECT MAX(i.raw_len) FROM blob_pack_entries i
            JOIN blobs b ON b.hash=i.blob_hash
            WHERE i.store_id=OLD.store_id AND i.pack_id=OLD.pack_id),0)
    WHERE store_id=OLD.store_id AND pack_id=OLD.pack_id;
    UPDATE blob_packs SET
        scan_hash=CASE WHEN scan_hash='' THEN NEW.blob_hash ELSE scan_hash END,
        live_entries=live_entries+1,
        live_stored_bytes=live_stored_bytes+NEW.stored_len,
        live_raw_bytes=live_raw_bytes+NEW.raw_len,
        max_live_stored_len=MAX(max_live_stored_len, NEW.stored_len),
        max_live_raw_len=MAX(max_live_raw_len, NEW.raw_len)
    WHERE store_id=NEW.store_id AND pack_id=NEW.pack_id;
END;

CREATE TRIGGER IF NOT EXISTS blob_pack_summary_blob_delete
AFTER DELETE ON blobs
WHEN EXISTS (SELECT 1 FROM blob_pack_entries WHERE blob_hash=OLD.hash)
BEGIN
    UPDATE blob_packs SET
        live_entries=live_entries-1,
        live_stored_bytes=live_stored_bytes-(
            SELECT stored_len FROM blob_pack_entries
            WHERE blob_hash=OLD.hash AND store_id=blob_packs.store_id
        ),
        live_raw_bytes=live_raw_bytes-(
            SELECT raw_len FROM blob_pack_entries
            WHERE blob_hash=OLD.hash AND store_id=blob_packs.store_id
        ),
        max_live_stored_len=COALESCE((SELECT MAX(i.stored_len) FROM blob_pack_entries i
            JOIN blobs b ON b.hash=i.blob_hash
            WHERE i.store_id=blob_packs.store_id AND i.pack_id=blob_packs.pack_id),0),
        max_live_raw_len=COALESCE((SELECT MAX(i.raw_len) FROM blob_pack_entries i
            JOIN blobs b ON b.hash=i.blob_hash
            WHERE i.store_id=blob_packs.store_id AND i.pack_id=blob_packs.pack_id),0)
    WHERE EXISTS (
        SELECT 1 FROM blob_pack_entries i
        WHERE i.blob_hash=OLD.hash
          AND i.store_id=blob_packs.store_id
          AND i.pack_id=blob_packs.pack_id
    );
END;

CREATE TRIGGER IF NOT EXISTS blob_pack_summary_blob_insert
AFTER INSERT ON blobs
WHEN EXISTS (SELECT 1 FROM blob_pack_entries WHERE blob_hash=NEW.hash)
BEGIN
    UPDATE blob_packs SET
        live_entries=live_entries+1,
        live_stored_bytes=live_stored_bytes+(
            SELECT stored_len FROM blob_pack_entries
            WHERE blob_hash=NEW.hash AND store_id=blob_packs.store_id
        ),
        live_raw_bytes=live_raw_bytes+(
            SELECT raw_len FROM blob_pack_entries
            WHERE blob_hash=NEW.hash AND store_id=blob_packs.store_id
        ),
        max_live_stored_len=MAX(max_live_stored_len,
            (SELECT stored_len FROM blob_pack_entries
             WHERE blob_hash=NEW.hash AND store_id=blob_packs.store_id)),
        max_live_raw_len=MAX(max_live_raw_len,
            (SELECT raw_len FROM blob_pack_entries
             WHERE blob_hash=NEW.hash AND store_id=blob_packs.store_id))
    WHERE EXISTS (
        SELECT 1 FROM blob_pack_entries i
        WHERE i.blob_hash=NEW.hash
          AND i.store_id=blob_packs.store_id
          AND i.pack_id=blob_packs.pack_id
    );
END;

CREATE INDEX IF NOT EXISTS blob_packs_dead_scan
ON blob_packs(store_id, scan_hash, pack_id) WHERE live_entries=0;
CREATE INDEX IF NOT EXISTS blob_packs_live_scan
ON blob_packs(store_id, scan_hash, pack_id) WHERE live_entries>0;

-- A file node is stable document identity; immutable content-version rows are
-- its byte history. Random UUIDv4 identities remain safe across JSONL
-- round-trips and pruning because they are never allocator-derived or reused.
CREATE TABLE IF NOT EXISTS content_versions (
    version_id              TEXT PRIMARY KEY,
    node_id                 INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    blob_hash               TEXT NOT NULL REFERENCES blobs(hash),
    size                    INTEGER NOT NULL CHECK (size >= 0),
    mime_type               TEXT,
    recorded_at             TEXT NOT NULL,
    node_revision           INTEGER NOT NULL CHECK (node_revision > 0),
    introduced_operation_id TEXT NOT NULL,
    transition_kind         TEXT NOT NULL
        CHECK (transition_kind IN ('content_create', 'content_replace', 'content_revert')),
    source_version_id       TEXT REFERENCES content_versions(version_id)
        DEFERRABLE INITIALLY DEFERRED,
    UNIQUE (node_id, node_revision),
    UNIQUE (node_id, introduced_operation_id),
    UNIQUE (node_id, version_id),
    CHECK ((transition_kind = 'content_create') = (node_revision = 1)),
    CHECK ((transition_kind = 'content_revert') = (source_version_id IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS content_versions_node
    ON content_versions(node_id, node_revision DESC);
CREATE INDEX IF NOT EXISTS content_versions_blob ON content_versions(blob_hash);

-- Media source authority separates an immutable recording identity and its
-- exact revisions from each caller's independently revocable occurrence.
-- Private acquisition references remain runtime-only and are never exported.
CREATE TABLE IF NOT EXISTS media_sources (
    source_id       TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,
    provider        TEXT NOT NULL,
    origin_scope    TEXT NOT NULL,
    identity_sha256 TEXT NOT NULL UNIQUE,
    created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS media_source_versions (
    source_version_id  TEXT PRIMARY KEY,
    source_id          TEXT NOT NULL REFERENCES media_sources(source_id),
    revision           INTEGER NOT NULL,
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id),
    source_sha256      TEXT NOT NULL,
    source_bytes       INTEGER NOT NULL,
    capture_json       TEXT NOT NULL,
    claim_sha256       TEXT NOT NULL,
    created_at         TEXT NOT NULL,
    UNIQUE (source_id, revision)
);

CREATE TABLE IF NOT EXISTS media_source_heads (
    source_id         TEXT PRIMARY KEY REFERENCES media_sources(source_id),
    source_version_id TEXT REFERENCES media_source_versions(source_version_id),
    revision          INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS media_occurrences (
    occurrence_id         TEXT PRIMARY KEY,
    source_id             TEXT NOT NULL REFERENCES media_sources(source_id),
    source_version_id     TEXT REFERENCES media_source_versions(source_version_id),
    caller_principal      TEXT NOT NULL,
    caller_occurrence_ref TEXT NOT NULL,
    caller_revision       TEXT NOT NULL,
    caller_filename       TEXT NOT NULL,
    caller_person_ref     TEXT NOT NULL,
    speaker_label         TEXT NOT NULL,
    message_json          TEXT NOT NULL,
    visible               INTEGER NOT NULL,
    first_seen_at         TEXT NOT NULL,
    revoked_at            TEXT,
    UNIQUE (caller_principal, caller_occurrence_ref, caller_revision)
);

CREATE TABLE IF NOT EXISTS media_visibility_fences (
    caller_principal TEXT PRIMARY KEY,
    fence            INTEGER NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS media_input_artifacts (
    input_id           TEXT PRIMARY KEY,
    occurrence_id      TEXT NOT NULL REFERENCES media_occurrences(occurrence_id),
    source_id          TEXT NOT NULL REFERENCES media_sources(source_id),
    source_version_id  TEXT REFERENCES media_source_versions(source_version_id),
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id),
    kind               TEXT NOT NULL,
    origin             TEXT NOT NULL,
    provider           TEXT NOT NULL,
    language           TEXT NOT NULL,
    input_sha256       TEXT NOT NULL,
    created_at         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS media_operations (
    operation_id  TEXT PRIMARY KEY,
    principal     TEXT NOT NULL,
    verb          TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    state         TEXT NOT NULL,
    source_id     TEXT REFERENCES media_sources(source_id),
    receipt_json  TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS media_acquisitions (
    acquisition_id      TEXT PRIMARY KEY,
    operation_id        TEXT NOT NULL REFERENCES media_operations(operation_id),
    occurrence_id       TEXT NOT NULL REFERENCES media_occurrences(occurrence_id),
    source_id           TEXT NOT NULL REFERENCES media_sources(source_id),
    origin_id           TEXT NOT NULL,
    resolver_fingerprint TEXT NOT NULL,
    request_sha256      TEXT NOT NULL,
    authorization_json  TEXT NOT NULL,
    state               TEXT NOT NULL,
    stage               TEXT NOT NULL,
    outcome             TEXT NOT NULL,
    failure_code        TEXT NOT NULL,
    attempt             INTEGER NOT NULL,
    claim_owner         TEXT,
    claim_epoch         INTEGER NOT NULL,
    lease_expires_at    TEXT,
    available_at        TEXT NOT NULL,
    received_bytes      INTEGER NOT NULL,
    started_at          TEXT,
    finished_at         TEXT
);

CREATE TABLE IF NOT EXISTS media_protected_refs (
    acquisition_id    TEXT PRIMARY KEY REFERENCES media_acquisitions(acquisition_id),
    occurrence_id     TEXT NOT NULL REFERENCES media_occurrences(occurrence_id),
    request_url       TEXT NOT NULL,
    credential_binding TEXT NOT NULL,
    expires_at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS media_versions_content
    ON media_source_versions(content_version_id, source_id);
CREATE INDEX IF NOT EXISTS media_occurrences_principal
    ON media_occurrences(caller_principal, visible, source_id, occurrence_id);
CREATE INDEX IF NOT EXISTS media_occurrences_source
    ON media_occurrences(source_id, visible, caller_principal, occurrence_id);
CREATE INDEX IF NOT EXISTS media_acquisitions_claimable
    ON media_acquisitions(state, available_at, lease_expires_at, acquisition_id);

CREATE TRIGGER IF NOT EXISTS media_sources_immutable_update
BEFORE UPDATE ON media_sources BEGIN
    SELECT RAISE(ABORT, 'media source records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS media_source_versions_immutable_update
BEFORE UPDATE ON media_source_versions BEGIN
    SELECT RAISE(ABORT, 'media source version records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS media_input_artifacts_immutable_update
BEFORE UPDATE ON media_input_artifacts BEGIN
    SELECT RAISE(ABORT, 'media input artifact records are immutable');
END;

-- Visual previews are immutable exact-version derivatives selected by a small
-- mutable head. The content version owns their lifecycle; deleting the version
-- removes its preview catalog, after which ordinary blob GC may reclaim bytes
-- no other version or derivative retains.
CREATE TABLE IF NOT EXISTS visual_preview_generations (
    generation_id         TEXT PRIMARY KEY,
    vault_uid             TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    content_version_id    TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    source_sha256         TEXT NOT NULL REFERENCES blobs(hash),
    contract_version      TEXT NOT NULL,
    recipe_fingerprint    TEXT NOT NULL,
    canonical_result      BLOB NOT NULL
        CHECK (length(canonical_result) BETWEEN 2 AND 65536),
    checksum              TEXT NOT NULL,
    state                 TEXT NOT NULL,
    output_blob_hash      TEXT REFERENCES blobs(hash),
    output_size           INTEGER,
    output_media_type     TEXT,
    output_width          INTEGER,
    output_height         INTEGER,
    failure_code          TEXT,
    failure_detail        TEXT,
    created_at            TEXT NOT NULL,
    UNIQUE (content_version_id, recipe_fingerprint),
    UNIQUE (content_version_id, generation_id)
);

CREATE INDEX IF NOT EXISTS visual_preview_generations_output
    ON visual_preview_generations(output_blob_hash, generation_id)
    WHERE output_blob_hash IS NOT NULL;

CREATE TABLE IF NOT EXISTS visual_preview_heads (
    content_version_id TEXT PRIMARY KEY REFERENCES content_versions(version_id) ON DELETE CASCADE,
    generation_id      TEXT NOT NULL UNIQUE,
    published_at       TEXT NOT NULL,
    FOREIGN KEY (content_version_id, generation_id)
        REFERENCES visual_preview_generations(content_version_id, generation_id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS visual_preview_generations_immutable_update
BEFORE UPDATE ON visual_preview_generations BEGIN
    SELECT RAISE(ABORT, 'visual preview generation records are immutable');
END;

CREATE TABLE IF NOT EXISTS ingests (
    id          TEXT PRIMARY KEY NOT NULL,
    started_at  TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_desc TEXT NOT NULL
);

-- Mutable collection labels are separate from immutable ingest identity.
-- A cleared label retains its row so optimistic-concurrency fences never reset.
CREATE TABLE IF NOT EXISTS collection_labels (
    ingest_id  TEXT PRIMARY KEY REFERENCES ingests(id),
    label      TEXT UNIQUE,
    revision   INTEGER NOT NULL CHECK (revision >= 1),
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS provenance (
    identity       TEXT PRIMARY KEY NOT NULL,
    node_id        INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ingest_id      TEXT NOT NULL REFERENCES ingests(id),
    original_path  TEXT NOT NULL,
    original_mtime TEXT,
    supersedes     TEXT REFERENCES provenance(identity)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX IF NOT EXISTS provenance_node ON provenance(node_id);
CREATE UNIQUE INDEX IF NOT EXISTS provenance_direct_successor
    ON provenance(supersedes) WHERE supersedes IS NOT NULL;

-- A watched source has two independent identities: the stable document node
-- and the last source bytes the watcher accepted. Keeping this small cursor
-- separate from provenance prevents an unchanged source from overwriting a
-- later manual edit after daemon restart. The primary key is the hot restart
-- lookup; policy decisions remain in Go.
CREATE TABLE IF NOT EXISTS watch_sources (
    watch_name TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    node_id    INTEGER NOT NULL UNIQUE REFERENCES nodes(id) ON DELETE CASCADE,
    blob_hash  TEXT NOT NULL,
    size       INTEGER NOT NULL CHECK (size >= 0),
    PRIMARY KEY (watch_name, source_ref)
) WITHOUT ROWID;

-- Ingest and provenance facts are append-only authority. Corrections add a
-- new provenance fact linked through supersedes; they never rewrite history.
CREATE TRIGGER IF NOT EXISTS ingests_immutable_update
BEFORE UPDATE ON ingests BEGIN
    SELECT RAISE(ABORT, 'ingest records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS provenance_immutable_update
BEFORE UPDATE ON provenance BEGIN
    SELECT RAISE(ABORT, 'provenance records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS provenance_same_node_insert
BEFORE INSERT ON provenance
WHEN NEW.supersedes IS NOT NULL AND EXISTS (
    SELECT 1 FROM provenance prior
    WHERE prior.identity = NEW.supersedes AND prior.node_id != NEW.node_id
) BEGIN
    SELECT RAISE(ABORT, 'provenance supersession must stay on one node');
END;

-- Processing consent is scoped to a random local incarnation. The pointer is
-- deliberately not backup authority: a restored database keeps the imported
-- grant history while the fresh restore target retains its own incarnation.
CREATE TABLE IF NOT EXISTS processing_incarnations (
    incarnation_id TEXT PRIMARY KEY,
    created_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS current_processing_incarnation (
    singleton      INTEGER PRIMARY KEY CHECK (singleton = 1),
    incarnation_id TEXT NOT NULL UNIQUE REFERENCES processing_incarnations(incarnation_id)
);

CREATE TRIGGER IF NOT EXISTS current_processing_incarnation_immutable_update
BEFORE UPDATE ON current_processing_incarnation BEGIN
    SELECT RAISE(ABORT, 'current processing incarnation is immutable');
END;

CREATE TRIGGER IF NOT EXISTS current_processing_incarnation_immutable_delete
BEFORE DELETE ON current_processing_incarnation BEGIN
    SELECT RAISE(ABORT, 'current processing incarnation is immutable');
END;

CREATE TABLE IF NOT EXISTS processing_consent_grants (
    grant_id                  TEXT PRIMARY KEY,
    consent_set_id            TEXT NOT NULL,
    vault_uid                 TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    incarnation_id            TEXT NOT NULL REFERENCES processing_incarnations(incarnation_id),
    principal                 TEXT NOT NULL,
    scope                     TEXT NOT NULL,
    profile_fingerprint       TEXT NOT NULL,
    disclosure_fingerprint    TEXT NOT NULL,
    input_classes_json        TEXT NOT NULL,
    retained_classes_json     TEXT NOT NULL,
    revocation_fence          INTEGER NOT NULL CHECK (revocation_fence >= 0),
    issued_at                 TEXT NOT NULL,
    expires_at                TEXT
);

CREATE INDEX IF NOT EXISTS processing_consent_grants_authority
    ON processing_consent_grants(
        vault_uid, incarnation_id, principal, scope, profile_fingerprint,
        disclosure_fingerprint, input_classes_json, retained_classes_json,
        issued_at, grant_id
    );

CREATE INDEX IF NOT EXISTS processing_consent_grants_set
    ON processing_consent_grants(consent_set_id, grant_id);

CREATE TABLE IF NOT EXISTS processing_consent_revocations (
    revocation_id  TEXT PRIMARY KEY,
    vault_uid      TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    incarnation_id TEXT NOT NULL REFERENCES processing_incarnations(incarnation_id),
    principal      TEXT NOT NULL,
    scope          TEXT NOT NULL,
    fence          INTEGER NOT NULL CHECK (fence > 0),
    revoked_at     TEXT NOT NULL,
    UNIQUE (vault_uid, incarnation_id, principal, scope, fence)
);

CREATE INDEX IF NOT EXISTS processing_consent_revocations_scope
    ON processing_consent_revocations(
        vault_uid, incarnation_id, principal, scope, fence
    );

CREATE TRIGGER IF NOT EXISTS processing_incarnations_immutable_update
BEFORE UPDATE ON processing_incarnations BEGIN
    SELECT RAISE(ABORT, 'processing incarnation records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS processing_incarnations_immutable_delete
BEFORE DELETE ON processing_incarnations BEGIN
    SELECT RAISE(ABORT, 'processing incarnation records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS processing_consent_grants_immutable_update
BEFORE UPDATE ON processing_consent_grants BEGIN
    SELECT RAISE(ABORT, 'processing consent grant records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS processing_consent_grants_immutable_delete
BEFORE DELETE ON processing_consent_grants BEGIN
    SELECT RAISE(ABORT, 'processing consent grant records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS processing_consent_revocations_immutable_update
BEFORE UPDATE ON processing_consent_revocations BEGIN
    SELECT RAISE(ABORT, 'processing consent revocation records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS processing_consent_revocations_immutable_delete
BEFORE DELETE ON processing_consent_revocations BEGIN
    SELECT RAISE(ABORT, 'processing consent revocation records are immutable');
END;

-- Processing profiles are immutable canonical policy snapshots. Rendition
-- builds deliberately reference only their rendition/evidence components;
-- embedding-only profile fields never enter build identity.
CREATE TABLE IF NOT EXISTS processing_profiles (
    profile_fingerprint               TEXT PRIMARY KEY,
    canonical_profile                 TEXT NOT NULL,
    rendition_request_fingerprint     TEXT NOT NULL,
    evidence_lexical_fingerprint      TEXT NOT NULL,
    retention_disclosure_fingerprint  TEXT NOT NULL,
    attachment_policy_fingerprint     TEXT NOT NULL,
    consent_fingerprint               TEXT NOT NULL,
    rendition_disclosure_fingerprint  TEXT NOT NULL,
    trust_boundary                    TEXT NOT NULL
);

-- A completed rendition build is vault-local immutable authority. Shared build
-- IDs cover source bytes, policies, and the exact rendition execution identity;
-- attachments carry the full profile.
CREATE TABLE IF NOT EXISTS rendition_builds (
    build_id                             TEXT PRIMARY KEY,
    vault_uid                            TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    source_sha256                        TEXT NOT NULL REFERENCES blobs(hash),
    rendition_request_fingerprint        TEXT NOT NULL,
    evidence_lexical_fingerprint         TEXT NOT NULL,
    captured_artifact_policy_fingerprint TEXT NOT NULL,
    captured_artifact_policy_json        TEXT NOT NULL,
    authorization_checksum               TEXT NOT NULL,
    provider_operation_id                TEXT NOT NULL,
    provider_receipt_json                 TEXT NOT NULL,
    evidence_checksum                    TEXT NOT NULL,
    rendition_checksum                   TEXT NOT NULL,
    markdown_checksum                    TEXT NOT NULL,
    completeness                         TEXT NOT NULL,
    partial_success                      INTEGER NOT NULL CHECK (partial_success IN (0, 1)),
    truncated                            INTEGER NOT NULL CHECK (truncated IN (0, 1)),
    warnings_json                        TEXT NOT NULL,
    completed_at                         TEXT NOT NULL,
    declared_artifact_count              INTEGER NOT NULL
        CHECK (declared_artifact_count >= 0),
    unit_count                           INTEGER NOT NULL
        CHECK (unit_count >= 0),
    lexical_segment_count                INTEGER NOT NULL
        CHECK (lexical_segment_count >= 0),
    UNIQUE (vault_uid, build_id)
);

CREATE INDEX IF NOT EXISTS rendition_builds_source
    ON rendition_builds(source_sha256, build_id);

CREATE TABLE IF NOT EXISTS rendition_artifacts (
    build_id    TEXT NOT NULL REFERENCES rendition_builds(build_id),
    artifact_id TEXT NOT NULL,
    role        TEXT NOT NULL,
    blob_hash   TEXT NOT NULL REFERENCES blobs(hash),
    size        INTEGER NOT NULL CHECK (size >= 0),
    checksum    TEXT NOT NULL,
    PRIMARY KEY (build_id, artifact_id)
);

CREATE INDEX IF NOT EXISTS rendition_artifacts_blob
    ON rendition_artifacts(blob_hash, build_id);

CREATE TABLE IF NOT EXISTS rendition_units (
    build_id          TEXT NOT NULL REFERENCES rendition_builds(build_id),
    unit_id           TEXT NOT NULL,
    evidence_unit_id  TEXT NOT NULL,
    unit_order        INTEGER NOT NULL CHECK (unit_order >= 0),
    checksum          TEXT NOT NULL,
    heading_path_json TEXT NOT NULL,
    locator_json      TEXT NOT NULL,
    PRIMARY KEY (build_id, unit_id),
    UNIQUE (build_id, unit_order)
);

CREATE TABLE IF NOT EXISTS rendition_lexical_segments (
    build_id     TEXT NOT NULL,
    segment_id   TEXT NOT NULL,
    unit_id      TEXT NOT NULL,
    segment_order INTEGER NOT NULL CHECK (segment_order >= 0),
    char_start   INTEGER NOT NULL CHECK (char_start >= 0),
    char_end     INTEGER NOT NULL CHECK (char_end >= char_start),
    checksum     TEXT NOT NULL,
    text         TEXT NOT NULL,
    PRIMARY KEY (build_id, segment_id),
    UNIQUE (build_id, segment_order),
    FOREIGN KEY (build_id, unit_id)
        REFERENCES rendition_units(build_id, unit_id)
);

-- Attachments own version-specific visibility, profile, retention,
-- disclosure, and consent identity. Reusing a build creates a new attachment;
-- it never grants authority inherited from another content version.
CREATE TABLE IF NOT EXISTS rendition_attachments (
    attachment_id                    TEXT PRIMARY KEY,
    vault_uid                        TEXT NOT NULL,
    content_version_id               TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    build_id                          TEXT NOT NULL,
    profile_fingerprint               TEXT NOT NULL REFERENCES processing_profiles(profile_fingerprint),
    retention_disclosure_fingerprint  TEXT NOT NULL,
    attachment_policy_fingerprint     TEXT NOT NULL,
    consent_fingerprint               TEXT NOT NULL,
    rendition_disclosure_fingerprint  TEXT NOT NULL,
    trust_boundary                    TEXT NOT NULL,
    attached_at                       TEXT NOT NULL,
    FOREIGN KEY (vault_uid, build_id)
        REFERENCES rendition_builds(vault_uid, build_id),
    UNIQUE (content_version_id, profile_fingerprint, attachment_id),
    UNIQUE (content_version_id, profile_fingerprint, build_id)
);

-- A head can resolve only through the exact attachment at its version/profile
-- key. Updating this small pointer is the sole rendition activation mutation.
CREATE TABLE IF NOT EXISTS rendition_heads (
    content_version_id          TEXT NOT NULL,
    profile_fingerprint         TEXT NOT NULL,
    attachment_id               TEXT NOT NULL,
    published_at                TEXT NOT NULL,
    PRIMARY KEY (content_version_id, profile_fingerprint),
    FOREIGN KEY (content_version_id, profile_fingerprint, attachment_id)
        REFERENCES rendition_attachments(
            content_version_id, profile_fingerprint, attachment_id
        ) ON DELETE CASCADE
);

-- Embedding authority is normalized into immutable identities. Rendered text
-- and vector bytes remain in bounded derivative payloads; SQLite retains only
-- checksums, membership, activation pointers, and provider-neutral failures.
CREATE TABLE IF NOT EXISTS embedding_vector_spaces (
    vector_space_id          TEXT PRIMARY KEY,
    contract_version         TEXT NOT NULL,
    descriptor_json          BLOB NOT NULL CHECK (length(descriptor_json) > 0),
    provider_descriptor      TEXT NOT NULL CHECK (length(CAST(provider_descriptor AS BLOB)) > 0),
    provider_revision        TEXT NOT NULL CHECK (length(CAST(provider_revision AS BLOB)) > 0),
    descriptor_fingerprint   TEXT NOT NULL,
    compatibility_id         TEXT NOT NULL CHECK (length(CAST(compatibility_id AS BLOB)) > 0),
    dimensions               INTEGER NOT NULL CHECK (dimensions > 0),
    metric                   TEXT NOT NULL CHECK (length(CAST(metric AS BLOB)) > 0),
    normalization            TEXT NOT NULL CHECK (length(CAST(normalization AS BLOB)) > 0),
    scalar_encoding          TEXT NOT NULL CHECK (length(CAST(scalar_encoding AS BLOB)) > 0),
    document_formatter       TEXT NOT NULL CHECK (length(CAST(document_formatter AS BLOB)) > 0),
    query_formatter          TEXT NOT NULL CHECK (length(CAST(query_formatter AS BLOB)) > 0),
    model_input_fingerprint  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS embedding_input_generations (
    generation_id                  TEXT PRIMARY KEY,
    generation_blob_hash           TEXT REFERENCES blobs(hash),
    generation_encoded_size        INTEGER NOT NULL,
    generation_checksum            TEXT NOT NULL,
    source_version_id              TEXT NOT NULL,
    profile_fingerprint            TEXT NOT NULL REFERENCES processing_profiles(profile_fingerprint),
    evidence_fingerprint           TEXT NOT NULL,
    tokenizer_fingerprint          TEXT NOT NULL,
    chunk_policy_fingerprint       TEXT NOT NULL,
    formatter_fingerprint          TEXT NOT NULL,
    attachment_context_fingerprint TEXT NOT NULL,
    attachment_id                  TEXT,
    input_count                    INTEGER NOT NULL CHECK (input_count > 0),
    created_at                     TEXT NOT NULL,
    CHECK (attachment_context_fingerprint = '' OR attachment_id IS NOT NULL),
    CHECK ((generation_blob_hash IS NULL AND generation_encoded_size = 0)
        OR (generation_blob_hash IS NOT NULL AND generation_encoded_size > 0))
);

-- Embedding jobs are rebuildable operational state. Immutable input
-- generations, vector spaces, consent grants, failures, and published heads
-- remain the portable authority; this table only resumes bounded execution.
CREATE TABLE IF NOT EXISTS embedding_jobs (
    job_id              TEXT PRIMARY KEY,
    vault_uid           TEXT NOT NULL,
    content_version_id  TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    profile_fingerprint TEXT NOT NULL REFERENCES processing_profiles(profile_fingerprint),
    binding_id          TEXT NOT NULL,
    input_kind          TEXT NOT NULL,
    generation_id       TEXT NOT NULL REFERENCES embedding_input_generations(generation_id) ON DELETE CASCADE,
    vector_space_id     TEXT NOT NULL REFERENCES embedding_vector_spaces(vector_space_id) ON DELETE CASCADE,
    principal           TEXT NOT NULL,
    scope               TEXT NOT NULL,
    authorization_grant_id TEXT NOT NULL,
    authorization_incarnation_id TEXT NOT NULL,
    authorization_revocation_fence INTEGER NOT NULL,
    state               TEXT NOT NULL,
    claim_owner         TEXT,
    claim_epoch         INTEGER NOT NULL DEFAULT 0 CHECK (claim_epoch >= 0),
    claim_count         INTEGER NOT NULL DEFAULT 0,
    lease_expires_at    TEXT,
    available_at        TEXT NOT NULL,
    failure_code        TEXT,
    receipt_json        TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    UNIQUE (content_version_id,profile_fingerprint,binding_id,input_kind,generation_id,
            authorization_grant_id,authorization_incarnation_id,authorization_revocation_fence)
);

CREATE INDEX IF NOT EXISTS embedding_jobs_ready
    ON embedding_jobs(state,available_at,job_id);

CREATE INDEX IF NOT EXISTS embedding_jobs_generation
    ON embedding_jobs(generation_id);

CREATE INDEX IF NOT EXISTS embedding_jobs_vector_space
    ON embedding_jobs(vector_space_id);

CREATE TABLE IF NOT EXISTS embedding_generation_inputs (
    generation_id     TEXT NOT NULL REFERENCES embedding_input_generations(generation_id) ON DELETE CASCADE,
    input_id          TEXT NOT NULL CHECK (length(CAST(input_id AS BLOB)) > 0),
    input_order       INTEGER NOT NULL CHECK (input_order >= 0),
    rendered_checksum TEXT NOT NULL,
    PRIMARY KEY (generation_id, input_id),
    UNIQUE (generation_id, input_order)
);

CREATE TABLE IF NOT EXISTS embedding_vector_sets (
    vector_set_id      TEXT PRIMARY KEY,
    contract_version   TEXT NOT NULL,
    vector_space_id    TEXT NOT NULL REFERENCES embedding_vector_spaces(vector_space_id),
    payload_blob_hash  TEXT NOT NULL REFERENCES blobs(hash),
    payload_size       INTEGER NOT NULL CHECK (payload_size > 0),
    payload_checksum   TEXT NOT NULL,
    manifest_checksum  TEXT NOT NULL,
    row_count          INTEGER NOT NULL CHECK (row_count > 0),
    dimensions         INTEGER NOT NULL CHECK (dimensions > 0)
);

CREATE TABLE IF NOT EXISTS embedding_vector_rows (
    vector_set_id  TEXT NOT NULL REFERENCES embedding_vector_sets(vector_set_id) ON DELETE CASCADE,
    row_id         TEXT NOT NULL CHECK (length(CAST(row_id AS BLOB)) > 0),
    row_order      INTEGER NOT NULL CHECK (row_order >= 0),
    input_id       TEXT NOT NULL CHECK (length(CAST(input_id AS BLOB)) > 0),
    dimensions     INTEGER NOT NULL CHECK (dimensions > 0),
    checksum       TEXT NOT NULL,
    PRIMARY KEY (vector_set_id, row_id),
    UNIQUE (vector_set_id, row_order),
    UNIQUE (vector_set_id, input_id)
);

CREATE INDEX IF NOT EXISTS embedding_vector_sets_payload
    ON embedding_vector_sets(payload_blob_hash, vector_set_id);

CREATE TABLE IF NOT EXISTS embedding_sets (
    embedding_set_id             TEXT PRIMARY KEY,
    vault_uid                    TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    binding_id                   TEXT NOT NULL CHECK (length(CAST(binding_id AS BLOB)) > 0),
    input_kind                   TEXT NOT NULL,
    content_version_id           TEXT NOT NULL REFERENCES content_versions(version_id),
    profile_fingerprint          TEXT NOT NULL REFERENCES processing_profiles(profile_fingerprint),
    embedding_input_fingerprint  TEXT NOT NULL,
    vector_space_id              TEXT NOT NULL REFERENCES embedding_vector_spaces(vector_space_id),
    input_generation_id          TEXT NOT NULL REFERENCES embedding_input_generations(generation_id),
    vector_set_id                TEXT NOT NULL REFERENCES embedding_vector_sets(vector_set_id),
    created_at                   TEXT NOT NULL,
    UNIQUE (vault_uid, content_version_id, binding_id, input_kind,
            vector_space_id, input_generation_id, vector_set_id)
);

CREATE INDEX IF NOT EXISTS embedding_sets_content_version
    ON embedding_sets(content_version_id, embedding_set_id);
CREATE INDEX IF NOT EXISTS embedding_sets_input_generation
    ON embedding_sets(input_generation_id, embedding_set_id);
CREATE INDEX IF NOT EXISTS embedding_sets_vector_set
    ON embedding_sets(vector_set_id, embedding_set_id);
CREATE INDEX IF NOT EXISTS embedding_sets_vector_space
    ON embedding_sets(vector_space_id, embedding_set_id);

CREATE TABLE IF NOT EXISTS embedding_heads (
    content_version_id   TEXT NOT NULL,
    binding_id           TEXT NOT NULL,
    input_kind           TEXT NOT NULL,
    embedding_set_id     TEXT NOT NULL REFERENCES embedding_sets(embedding_set_id),
    vector_space_id      TEXT NOT NULL REFERENCES embedding_vector_spaces(vector_space_id),
    profile_fingerprint  TEXT NOT NULL REFERENCES processing_profiles(profile_fingerprint),
    published_at         TEXT NOT NULL,
    fencing_token        INTEGER NOT NULL,
    PRIMARY KEY (content_version_id, profile_fingerprint, binding_id, input_kind)
);

CREATE INDEX IF NOT EXISTS embedding_heads_set
    ON embedding_heads(embedding_set_id);

CREATE TABLE IF NOT EXISTS embedding_failures (
    content_version_id   TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    profile_fingerprint  TEXT NOT NULL REFERENCES processing_profiles(profile_fingerprint),
    binding_id           TEXT NOT NULL CHECK (length(CAST(binding_id AS BLOB)) > 0),
    input_kind           TEXT NOT NULL,
    failure_code         TEXT NOT NULL,
    failed_at            TEXT NOT NULL,
    fencing_token        INTEGER NOT NULL,
    attachment_id        TEXT NOT NULL,
    PRIMARY KEY (content_version_id, profile_fingerprint, binding_id, input_kind)
);

CREATE TRIGGER IF NOT EXISTS embedding_vector_spaces_immutable_update
BEFORE UPDATE ON embedding_vector_spaces BEGIN
    SELECT RAISE(ABORT, 'embedding vector space records are immutable');
END;
CREATE TRIGGER IF NOT EXISTS embedding_input_generations_immutable_update
BEFORE UPDATE ON embedding_input_generations BEGIN
    SELECT RAISE(ABORT, 'embedding input generation records are immutable');
END;
CREATE TRIGGER IF NOT EXISTS embedding_generation_inputs_immutable_update
BEFORE UPDATE ON embedding_generation_inputs BEGIN
    SELECT RAISE(ABORT, 'embedding input records are immutable');
END;
CREATE TRIGGER IF NOT EXISTS embedding_vector_sets_immutable_update
BEFORE UPDATE ON embedding_vector_sets BEGIN
    SELECT RAISE(ABORT, 'embedding vector set records are immutable');
END;
CREATE TRIGGER IF NOT EXISTS embedding_vector_rows_immutable_update
BEFORE UPDATE ON embedding_vector_rows BEGIN
    SELECT RAISE(ABORT, 'embedding vector row records are immutable');
END;
CREATE TRIGGER IF NOT EXISTS embedding_sets_immutable_update
BEFORE UPDATE ON embedding_sets BEGIN
    SELECT RAISE(ABORT, 'embedding set records are immutable');
END;
-- The lexical projection is rebuildable search state over immutable rendition
-- builds. Segment text is indexed once per build in rendition_lexical_index;
-- the FTS table is an external-content index over it. A generation names one
-- exact set of builds, and the head selects the generation that search reads,
-- so publishing a new generation never copies text an earlier one indexed.
CREATE TABLE IF NOT EXISTS rendition_lexical_index (
    row_id     INTEGER PRIMARY KEY,
    build_id   TEXT NOT NULL REFERENCES rendition_builds(build_id),
    segment_id TEXT NOT NULL,
    text       TEXT NOT NULL,
    UNIQUE (build_id, segment_id)
);

CREATE VIRTUAL TABLE IF NOT EXISTS rendition_lexical_fts USING fts5(
    build_id      UNINDEXED,
    segment_id    UNINDEXED,
    text,
    content='rendition_lexical_index',
    content_rowid='row_id'
);

CREATE TRIGGER IF NOT EXISTS rendition_lexical_index_insert
AFTER INSERT ON rendition_lexical_index BEGIN
    INSERT INTO rendition_lexical_fts(rowid, build_id, segment_id, text)
    VALUES (NEW.row_id, NEW.build_id, NEW.segment_id, NEW.text);
END;

CREATE TRIGGER IF NOT EXISTS rendition_lexical_index_delete
AFTER DELETE ON rendition_lexical_index BEGIN
    INSERT INTO rendition_lexical_fts(rendition_lexical_fts, rowid, build_id, segment_id, text)
    VALUES ('delete', OLD.row_id, OLD.build_id, OLD.segment_id, OLD.text);
END;

CREATE TABLE IF NOT EXISTS rendition_lexical_generations (
    generation_id TEXT PRIMARY KEY,
    segment_count INTEGER NOT NULL CHECK (segment_count >= 0),
    build_count   INTEGER NOT NULL CHECK (build_count >= 0),
    built_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS rendition_lexical_generation_manifests (
    generation_id  TEXT PRIMARY KEY REFERENCES rendition_lexical_generations(generation_id),
    manifest_digest TEXT NOT NULL,
    build_digest    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS rendition_lexical_generation_builds (
    generation_id TEXT NOT NULL REFERENCES rendition_lexical_generations(generation_id)
        ON DELETE CASCADE,
    build_id      TEXT NOT NULL REFERENCES rendition_builds(build_id),
    PRIMARY KEY (generation_id, build_id)
);

CREATE INDEX IF NOT EXISTS rendition_lexical_generation_builds_build
    ON rendition_lexical_generation_builds(build_id);

CREATE TABLE IF NOT EXISTS rendition_lexical_heads (
    singleton     INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation_id TEXT NOT NULL REFERENCES rendition_lexical_generations(generation_id)
);

-- Generations the head has moved away from. A superseded generation is
-- collected by the next head flip once no root, reader lease, or job still
-- names it; generations staged for a future publication are never touched.
CREATE TABLE IF NOT EXISTS rendition_lexical_superseded (
    generation_id TEXT PRIMARY KEY
        REFERENCES rendition_lexical_generations(generation_id) ON DELETE CASCADE
);

-- Vector indexes are disposable, vault-local projection state. These rows are
-- deliberately absent from metadata-v1 export/import: restore reconstructs
-- them from current logical embedding authority and retained vector-set blobs.
CREATE TABLE IF NOT EXISTS vector_index_generations (
    generation_id             TEXT PRIMARY KEY,
    vector_space_id           TEXT NOT NULL,
    source_manifest_checksum  TEXT NOT NULL,
    index_manifest_checksum   TEXT NOT NULL,
    generation_bytes          BLOB NOT NULL,
    byte_size                 INTEGER NOT NULL
        CHECK (byte_size = length(generation_bytes)),
    row_count                 INTEGER NOT NULL CHECK (row_count > 0),
    built_at                  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS vector_index_generations_space
    ON vector_index_generations(vector_space_id, source_manifest_checksum, generation_id);

CREATE TABLE IF NOT EXISTS vector_index_heads (
    vector_space_id          TEXT PRIMARY KEY,
    generation_id           TEXT NOT NULL REFERENCES vector_index_generations(generation_id),
    source_manifest_checksum TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS vector_index_build_jobs (
    vector_space_id          TEXT PRIMARY KEY,
    source_manifest_checksum TEXT NOT NULL,
    owner                    TEXT NOT NULL,
    fencing_token            INTEGER NOT NULL CHECK (fencing_token > 0),
    lease_expires_at         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS vector_index_reader_leases (
    lease_id          TEXT PRIMARY KEY,
    generation_id     TEXT NOT NULL REFERENCES vector_index_generations(generation_id) ON DELETE CASCADE,
    owner             TEXT NOT NULL,
    fencing_token     INTEGER NOT NULL CHECK (fencing_token > 0),
    lease_expires_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS vector_index_reader_leases_generation
    ON vector_index_reader_leases(generation_id, lease_expires_at);

CREATE TABLE IF NOT EXISTS vector_index_unavailable_coverage (
    vector_space_id           TEXT NOT NULL,
    source_manifest_checksum  TEXT NOT NULL,
    embedding_set_id          TEXT NOT NULL,
    vector_set_id             TEXT NOT NULL,
    payload_blob_hash         TEXT NOT NULL,
    external_reembedding_required INTEGER NOT NULL,
    PRIMARY KEY (vector_space_id, source_manifest_checksum, embedding_set_id)
);

-- Rendition jobs are restart authority, not immutable artifact authority.
-- Provider-specific payloads never enter these tables. Continuation authority
-- is only the provider-neutral sealed execution snapshot plus an optional
-- opaque provider-issued handle.
CREATE TABLE IF NOT EXISTS rendition_jobs (
    job_id                               TEXT PRIMARY KEY,
    vault_uid                            TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    source_sha256                        TEXT NOT NULL REFERENCES blobs(hash),
    rendition_request_fingerprint        TEXT NOT NULL,
    evidence_lexical_fingerprint         TEXT NOT NULL,
    captured_artifact_policy_fingerprint TEXT NOT NULL,
    execution_identity_fingerprint        TEXT NOT NULL,
    execution_identity_json               TEXT NOT NULL,
    execution_snapshot_json               TEXT,
    captured_artifact_policy_json        TEXT NOT NULL,
    state                                TEXT NOT NULL,
    phase                                TEXT NOT NULL,
    claim_owner                          TEXT,
    claim_epoch                          INTEGER NOT NULL DEFAULT 0 CHECK (claim_epoch >= 0),
    lease_expires_at                     TEXT,
    available_at                         TEXT NOT NULL,
    provider_started                     INTEGER NOT NULL DEFAULT 0
        CHECK (provider_started IN (0, 1)),
    provider_attempts                    INTEGER NOT NULL DEFAULT 0
        CHECK (provider_attempts >= 0),
    provider_resume_handle               TEXT,
    selected_waiter_id                   TEXT,
    authorization_grant_id               TEXT,
    authorization_incarnation_id         TEXT,
    authorization_revocation_fence       INTEGER,
    lexical_generation_id                TEXT,
    failure_code                         TEXT,
    created_at                           TEXT NOT NULL,
    updated_at                           TEXT NOT NULL,
    CHECK ((authorization_grant_id IS NULL) =
           (authorization_incarnation_id IS NULL AND authorization_revocation_fence IS NULL))
);

CREATE INDEX IF NOT EXISTS rendition_jobs_claimable
    ON rendition_jobs(state, available_at, lease_expires_at, job_id);

CREATE TABLE IF NOT EXISTS rendition_job_waiters (
    waiter_id                TEXT PRIMARY KEY,
    job_id                   TEXT NOT NULL REFERENCES rendition_jobs(job_id) ON DELETE CASCADE,
    content_version_id       TEXT NOT NULL REFERENCES content_versions(version_id),
    profile_fingerprint      TEXT NOT NULL REFERENCES processing_profiles(profile_fingerprint),
    principal                TEXT NOT NULL,
    scope                    TEXT NOT NULL,
    disclosure_fingerprint   TEXT NOT NULL,
    input_classes_json       TEXT NOT NULL,
    retained_classes_json    TEXT NOT NULL,
    authorization_grant_id   TEXT NOT NULL,
    authorization_incarnation_id TEXT NOT NULL,
    authorization_revocation_fence INTEGER NOT NULL,
    state                    TEXT NOT NULL,
    failure_code             TEXT,
    attachment_id            TEXT NOT NULL,
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS rendition_job_waiters_job
    ON rendition_job_waiters(job_id, state, waiter_id);

-- Root producers outside the immutable rendition catalog retain one exact
-- build or lexical generation. Lease expiry uses canonical fixed-width UTC
-- timestamps; monotonic fencing prevents a stale worker from releasing a
-- renewed root. Attachments and heads remain normalized in their own tables.
CREATE TABLE IF NOT EXISTS current_rendition_roots (
    root_id       TEXT PRIMARY KEY,
    root_kind     TEXT NOT NULL,
    target_kind   TEXT NOT NULL,
    target_id     TEXT NOT NULL,
    fencing_token INTEGER NOT NULL CHECK (fencing_token > 0),
    recorded_at   TEXT NOT NULL,
    expires_at    TEXT,
    active        INTEGER NOT NULL CHECK (active IN (0, 1)),
    released_at   TEXT,
    CHECK ((active = 1) = (released_at IS NULL))
);

CREATE INDEX IF NOT EXISTS current_rendition_roots_target
    ON current_rendition_roots(target_kind, target_id, root_kind);
CREATE INDEX IF NOT EXISTS current_rendition_roots_expiry
    ON current_rendition_roots(expires_at)
    WHERE expires_at IS NOT NULL;

-- Purge suppression is durable logical authority. It prevents startup legacy
-- convergence from silently recreating an exact purged derivative. A later
-- explicit authorization supersedes, rather than deletes, that history.
CREATE TABLE IF NOT EXISTS derivative_purge_suppressions (
    source_sha256        TEXT NOT NULL,
    profile_fingerprint  TEXT NOT NULL,
    build_id             TEXT NOT NULL,
    purged_at            TEXT NOT NULL,
    active               INTEGER NOT NULL CHECK (active IN (0, 1)),
    superseded_at        TEXT,
    superseding_build_id TEXT,
    PRIMARY KEY (source_sha256, profile_fingerprint, build_id),
    CHECK ((active = 1) = (superseded_at IS NULL AND superseding_build_id IS NULL))
);

CREATE TABLE IF NOT EXISTS rendition_blob_staging (
    blob_hash TEXT PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE
);

-- Logical purge and physical erasure are separate crash boundaries. Pending
-- hashes remain durable until location-aware collection deletes the blob row.
CREATE TABLE IF NOT EXISTS derivative_blob_purge_pending (
    blob_hash TEXT PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE
);

-- Packed erasure targets outlive the blob rows whose dead bytes they contain.
-- Repack deletes each receipt through the source pack's cascading retirement
-- only after the old immutable pack file is absent.
CREATE TABLE IF NOT EXISTS derivative_pack_purge_pending (
    store_id TEXT NOT NULL,
    pack_id  TEXT NOT NULL,
    PRIMARY KEY (store_id, pack_id),
    FOREIGN KEY (store_id, pack_id) REFERENCES blob_packs(store_id, pack_id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS content_versions_promote_rendition_staging
AFTER INSERT ON content_versions BEGIN
    DELETE FROM rendition_blob_staging WHERE blob_hash=NEW.blob_hash;
END;

CREATE TRIGGER IF NOT EXISTS content_versions_revoke_derivative_purge
AFTER INSERT ON content_versions BEGIN
    DELETE FROM derivative_blob_purge_pending WHERE blob_hash=NEW.blob_hash;
END;

CREATE INDEX IF NOT EXISTS derivative_purge_suppressions_active_source
    ON derivative_purge_suppressions(source_sha256, profile_fingerprint)
    WHERE active = 1;

CREATE TRIGGER IF NOT EXISTS processing_profiles_immutable_update
BEFORE UPDATE ON processing_profiles BEGIN
    SELECT RAISE(ABORT, 'processing profile records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS rendition_builds_immutable_update
BEFORE UPDATE ON rendition_builds BEGIN
    SELECT RAISE(ABORT, 'rendition build records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS rendition_artifacts_immutable_update
BEFORE UPDATE ON rendition_artifacts BEGIN
    SELECT RAISE(ABORT, 'rendition artifact records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS rendition_units_immutable_update
BEFORE UPDATE ON rendition_units BEGIN
    SELECT RAISE(ABORT, 'rendition unit records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS rendition_lexical_segments_immutable_update
BEFORE UPDATE ON rendition_lexical_segments BEGIN
    SELECT RAISE(ABORT, 'rendition lexical segment records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS rendition_attachments_immutable_update
BEFORE UPDATE ON rendition_attachments BEGIN
    SELECT RAISE(ABORT, 'rendition attachment records are immutable');
END;

CREATE TABLE IF NOT EXISTS tags (
    id       TEXT PRIMARY KEY NOT NULL,
    name     TEXT NOT NULL UNIQUE,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1)
);

CREATE TABLE IF NOT EXISTS node_tags (
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    tag_id  TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, tag_id)
);

CREATE INDEX IF NOT EXISTS node_tags_tag ON node_tags(tag_id);

-- Immutable protocol replay authority deliberately has no tag or node foreign
-- keys: a committed operation identity survives later deletion and purge.
CREATE TABLE IF NOT EXISTS batch_tag_receipts (
    operation_id  TEXT PRIMARY KEY,
    request_digest TEXT NOT NULL,
    receipt_json  BLOB NOT NULL
);

CREATE TRIGGER IF NOT EXISTS batch_tag_receipts_immutable_update
BEFORE UPDATE ON batch_tag_receipts BEGIN
    SELECT RAISE(ABORT, 'batch tag receipts are immutable');
END;

CREATE TRIGGER IF NOT EXISTS batch_tag_receipts_immutable_delete
BEFORE DELETE ON batch_tag_receipts BEGIN
    SELECT RAISE(ABORT, 'batch tag receipts are immutable');
END;

-- Saved definitions are mutable intent, fenced by revision. Their canonical
-- payload and fingerprint are validated in Go before they enter authority.
CREATE TABLE IF NOT EXISTS saved_queries (
    id          TEXT PRIMARY KEY NOT NULL,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    kind        TEXT NOT NULL,
    payload     BLOB NOT NULL,
    fingerprint TEXT NOT NULL,
    revision    INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- Saved run receipts retain comparison evidence while snapshot rows remain
-- daemon-local. Evolving receipt policy is validated in Go, not SQL checks.
CREATE TABLE IF NOT EXISTS saved_query_runs (
    run_id                     TEXT PRIMARY KEY NOT NULL,
    saved_query_id             TEXT NOT NULL REFERENCES saved_queries(id) ON DELETE CASCADE,
    saved_query_revision       INTEGER NOT NULL,
    query_fingerprint          TEXT NOT NULL,
    snapshot_id                TEXT NOT NULL,
    member_hash                TEXT NOT NULL,
    total                      INTEGER NOT NULL,
    total_bytes                INTEGER NOT NULL,
    ran_at                     TEXT NOT NULL,
    expires_at                 TEXT NOT NULL,
    previous_run_id            TEXT,
    previous_member_hash       TEXT,
    previous_total             INTEGER,
    previous_query_fingerprint TEXT
);

CREATE INDEX IF NOT EXISTS saved_query_runs_definition
    ON saved_query_runs(saved_query_id, ran_at DESC);

-- Canonical full-audit records are immutable content-addressed authority. The
-- digest is over Docbank's typed canonical audit encoding, never the JSON
-- spelling retained here for deterministic metadata-v1 transport.
CREATE TABLE IF NOT EXISTS audit_records (
    digest             TEXT PRIMARY KEY NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN (
        'enrollment_baseline', 'topology_genesis',
        'attached_metadata_genesis', 'event', 'canonical_mutation',
        'scope_chain_entry', 'allocation_genesis', 'allocation_entry',
        'topology_delta', 'path_effect_list', 'attached_metadata_delta'
    )),
    operation_id       TEXT,
    operation_sequence INTEGER CHECK (operation_sequence IS NULL OR operation_sequence > 0),
    scope_id           TEXT,
    entry_count        INTEGER CHECK (entry_count IS NULL OR entry_count > 0),
    event_id           TEXT,
    event_ordinal      INTEGER CHECK (event_ordinal IS NULL OR event_ordinal >= 0),
    node_id            INTEGER CHECK (node_id IS NULL OR node_id > 0),
    record_json        TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS audit_record_event
    ON audit_records(event_id) WHERE event_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS audit_record_event_node
    ON audit_records(node_id) WHERE kind = 'event';
CREATE INDEX IF NOT EXISTS audit_record_event_scope
    ON audit_records(scope_id) WHERE kind = 'event';
CREATE UNIQUE INDEX IF NOT EXISTS audit_record_mutation_operation
    ON audit_records(operation_id) WHERE kind = 'canonical_mutation';
CREATE UNIQUE INDEX IF NOT EXISTS audit_record_mutation_sequence
    ON audit_records(operation_sequence) WHERE kind = 'canonical_mutation';
CREATE UNIQUE INDEX IF NOT EXISTS audit_record_scope_entry
    ON audit_records(scope_id, entry_count) WHERE kind = 'scope_chain_entry';
CREATE UNIQUE INDEX IF NOT EXISTS audit_record_allocation_operation
    ON audit_records(operation_id) WHERE kind = 'allocation_entry';
CREATE UNIQUE INDEX IF NOT EXISTS audit_record_allocation_sequence
    ON audit_records(operation_sequence) WHERE kind = 'allocation_entry';
CREATE UNIQUE INDEX IF NOT EXISTS audit_record_single_genesis
    ON audit_records(kind) WHERE kind IN (
        'topology_genesis', 'attached_metadata_genesis', 'allocation_genesis'
    );

CREATE TABLE IF NOT EXISTS audit_authority (
    singleton                       INTEGER PRIMARY KEY CHECK (singleton = 1),
    lineage_id                      TEXT NOT NULL UNIQUE,
    operation_sequence_high_water   INTEGER NOT NULL CHECK (operation_sequence_high_water > 0),
    allocation_genesis_digest       TEXT NOT NULL UNIQUE REFERENCES audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED,
    allocation_entry_count          INTEGER NOT NULL CHECK (allocation_entry_count > 0),
    allocation_head                 TEXT NOT NULL REFERENCES audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS audit_scopes (
    scope_id             TEXT PRIMARY KEY NOT NULL,
    target_node_id       INTEGER NOT NULL REFERENCES nodes(id),
    enable_operation_id  TEXT NOT NULL UNIQUE,
    entry_count          INTEGER NOT NULL CHECK (entry_count > 0),
    chain_head           TEXT NOT NULL REFERENCES audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS audit_baselines (
    digest          TEXT PRIMARY KEY NOT NULL REFERENCES audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED,
    scope_id        TEXT NOT NULL REFERENCES audit_scopes(scope_id)
        DEFERRABLE INITIALLY DEFERRED,
    target_node_id  INTEGER NOT NULL REFERENCES nodes(id),
    operation_id    TEXT NOT NULL,
    UNIQUE (scope_id, target_node_id, operation_id)
);

CREATE TABLE IF NOT EXISTS audit_memberships (
    scope_id         TEXT NOT NULL REFERENCES audit_scopes(scope_id)
        DEFERRABLE INITIALLY DEFERRED,
    node_id          INTEGER NOT NULL REFERENCES nodes(id),
    baseline_digest  TEXT NOT NULL REFERENCES audit_baselines(digest)
        DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (scope_id, node_id)
);

CREATE INDEX IF NOT EXISTS audit_membership_node ON audit_memberships(node_id);

CREATE TABLE IF NOT EXISTS extracted_text (
    id                INTEGER PRIMARY KEY,
    blob_hash         TEXT NOT NULL,
    extractor         TEXT NOT NULL,
    extractor_version INTEGER NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('ok', 'failed')),
    error             TEXT,
    attempts          INTEGER NOT NULL DEFAULT 0,
    text              TEXT,
    extracted_at      TEXT NOT NULL,
    UNIQUE (blob_hash, extractor)
);

-- Derived work queue. Logical writes enqueue supported text in Go; the daemon
-- drains it after terminally verified reads. It is not portable authority.
CREATE TABLE IF NOT EXISTS text_extraction_queue (
    blob_hash       TEXT PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
    next_attempt_at TEXT NOT NULL
);

-- Per-version search eligibility is derived from MIME policy in Go. Keeping
-- this projection separate lets existing vaults adopt it without altering the
-- authoritative content-version table.
CREATE TABLE IF NOT EXISTS text_searchable_versions (
    version_id TEXT PRIMARY KEY REFERENCES content_versions(version_id) ON DELETE CASCADE
);

-- Derived full-text cache. This table is rebuilt from extracted_text during
-- portable import and is never part of document or audit authority.
CREATE VIRTUAL TABLE IF NOT EXISTS content_fts USING fts5(
    blob_hash UNINDEXED,
    extractor UNINDEXED,
    text
);

-- FTS over live node names. External-content table kept in sync by triggers;
-- trashed nodes are filtered at query time (the row stays indexed).
CREATE VIRTUAL TABLE IF NOT EXISTS nodes_fts USING fts5(
    name,
    content='nodes',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS nodes_fts_insert AFTER INSERT ON nodes BEGIN
    INSERT INTO nodes_fts(rowid, name) VALUES (new.id, new.name);
END;

CREATE TRIGGER IF NOT EXISTS nodes_fts_delete AFTER DELETE ON nodes BEGIN
    INSERT INTO nodes_fts(nodes_fts, rowid, name) VALUES ('delete', old.id, old.name);
END;

CREATE TRIGGER IF NOT EXISTS nodes_fts_update AFTER UPDATE OF name ON nodes BEGIN
    INSERT INTO nodes_fts(nodes_fts, rowid, name) VALUES ('delete', old.id, old.name);
    INSERT INTO nodes_fts(rowid, name) VALUES (new.id, new.name);
END;

CREATE TABLE IF NOT EXISTS email_generations (
    generation_id TEXT PRIMARY KEY,
    source_sha256 TEXT NOT NULL,
    source_size INTEGER NOT NULL CHECK (source_size >= 0),
    recipe_fingerprint TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    checksum TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (source_sha256, source_size, recipe_fingerprint)
);
CREATE INDEX IF NOT EXISTS content_versions_email_mime
ON content_versions(version_id)
WHERE instr(lower(COALESCE(mime_type,'')),'message/rfc822')>0;
CREATE TABLE IF NOT EXISTS email_part_artifacts (
    generation_id TEXT NOT NULL REFERENCES email_generations(generation_id) ON DELETE CASCADE,
    part_path TEXT NOT NULL,
    role TEXT NOT NULL,
    blob_hash TEXT NOT NULL REFERENCES blobs(hash),
    size INTEGER NOT NULL CHECK (size >= 0),
    PRIMARY KEY (generation_id, part_path, role)
);
CREATE INDEX IF NOT EXISTS email_part_artifacts_blob ON email_part_artifacts(blob_hash);
CREATE TABLE IF NOT EXISTS email_attachments (
    attachment_id TEXT PRIMARY KEY,
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    generation_id TEXT NOT NULL REFERENCES email_generations(generation_id),
    attached_at TEXT NOT NULL,
    UNIQUE (content_version_id, generation_id),
    UNIQUE (content_version_id, attachment_id)
);
CREATE INDEX IF NOT EXISTS email_attachments_generation ON email_attachments(generation_id);
CREATE TABLE IF NOT EXISTS email_heads (
    content_version_id TEXT PRIMARY KEY REFERENCES content_versions(version_id) ON DELETE CASCADE,
    attachment_id TEXT NOT NULL,
    published_at TEXT NOT NULL,
    FOREIGN KEY (content_version_id, attachment_id)
        REFERENCES email_attachments(content_version_id, attachment_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS email_body_results (
    email_attachment_id TEXT PRIMARY KEY REFERENCES email_attachments(attachment_id) ON DELETE CASCADE,
    body_recipe_fingerprint TEXT NOT NULL,
    state TEXT NOT NULL,
    part_path TEXT,
    rendition_attachment_id TEXT REFERENCES rendition_attachments(attachment_id),
    reason TEXT
);
CREATE INDEX IF NOT EXISTS email_body_results_rendition ON email_body_results(rendition_attachment_id);

-- Operational receipts retain exact MIME and ordinary document authority.
CREATE TABLE IF NOT EXISTS email_document_publications (
    operation_id TEXT PRIMARY KEY,
    request_digest TEXT NOT NULL,
    parent_version_id TEXT NOT NULL REFERENCES content_versions(version_id),
    email_attachment_id TEXT NOT NULL REFERENCES email_attachments(attachment_id),
    request_json TEXT NOT NULL,
    receipt_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS email_document_publications_parent ON email_document_publications(parent_version_id);
CREATE INDEX IF NOT EXISTS email_document_publications_attachment ON email_document_publications(email_attachment_id);
CREATE TABLE IF NOT EXISTS email_document_relations (
    operation_id TEXT NOT NULL REFERENCES email_document_publications(operation_id) ON DELETE CASCADE,
    occurrence_order INTEGER NOT NULL,
    child_version_id TEXT REFERENCES content_versions(version_id),
    PRIMARY KEY(operation_id, occurrence_order)
);
CREATE INDEX IF NOT EXISTS email_document_relations_child ON email_document_relations(child_version_id);

CREATE TABLE IF NOT EXISTS persons (
    person_id TEXT PRIMARY KEY NOT NULL,
    display_name TEXT NOT NULL,
    display_name_folded TEXT NOT NULL,
    origin TEXT NOT NULL,
    state TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS person_identities (
    identity_id TEXT PRIMARY KEY NOT NULL,
    person_id TEXT NOT NULL REFERENCES persons(person_id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    value_normalized TEXT NOT NULL,
    value_display TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_value TEXT NOT NULL,
    normalization TEXT NOT NULL,
    origin TEXT NOT NULL,
    evidence_kind TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    confidence TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS person_identities_identifier
    ON person_identities(person_id, kind, scope_kind, scope_value, value_normalized) WHERE kind <> 'name_alias';
CREATE UNIQUE INDEX IF NOT EXISTS person_identities_alias
    ON person_identities(person_id, value_normalized) WHERE kind = 'name_alias';
CREATE INDEX IF NOT EXISTS person_identities_value
    ON person_identities(value_normalized, kind, scope_kind, scope_value, person_id);

CREATE TABLE IF NOT EXISTS person_external_identities (
    person_id TEXT NOT NULL REFERENCES persons(person_id) ON DELETE CASCADE,
    system TEXT NOT NULL,
    archive_id TEXT NOT NULL,
    uid TEXT NOT NULL,
    uid_kind TEXT NOT NULL,
    uid_state TEXT NOT NULL,
    last_seen_revision INTEGER,
    display_name_snapshot TEXT NOT NULL,
    linked_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (system, archive_id, uid)
);
CREATE UNIQUE INDEX IF NOT EXISTS person_external_current
    ON person_external_identities(person_id, system, archive_id) WHERE uid_state = 'current';

CREATE TABLE IF NOT EXISTS person_external_uid_aliases (
    system TEXT NOT NULL,
    archive_id TEXT NOT NULL,
    retired_uid TEXT NOT NULL,
    surviving_uid TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    PRIMARY KEY (system, archive_id, retired_uid)
);
CREATE INDEX IF NOT EXISTS person_external_alias_target
    ON person_external_uid_aliases(system, archive_id, surviving_uid);

CREATE TABLE IF NOT EXISTS person_aliases (
    retired_person_id TEXT PRIMARY KEY NOT NULL,
    surviving_person_id TEXT,
    reason TEXT NOT NULL,
    retired_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS person_merges (
    merge_id TEXT PRIMARY KEY NOT NULL,
    operation_id TEXT NOT NULL UNIQUE,
    request_sha256 TEXT NOT NULL,
    survivor_person_id TEXT NOT NULL,
    absorbed_person_id TEXT NOT NULL,
    absorbed_display_name TEXT NOT NULL,
    moved_json BLOB NOT NULL,
    survivor_revision_before INTEGER NOT NULL,
    survivor_revision_after INTEGER NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS custodian_assignments (
    assignment_id TEXT PRIMARY KEY NOT NULL,
    scope_kind TEXT NOT NULL,
    ingest_id TEXT REFERENCES ingests(id) ON DELETE CASCADE,
    package_id TEXT,
    package_record_id TEXT,
    node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    content_version_id TEXT REFERENCES content_versions(version_id) ON DELETE CASCADE,
    person_id TEXT REFERENCES persons(person_id) ON DELETE SET NULL,
    raw_label TEXT NOT NULL,
    raw_label_folded TEXT NOT NULL,
    rank TEXT NOT NULL,
    basis TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    recorded_at TEXT NOT NULL,
    retired_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS custodian_primary_collection
    ON custodian_assignments(ingest_id) WHERE scope_kind='collection' AND rank='primary' AND retired_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS custodian_primary_package
    ON custodian_assignments(package_id, package_record_id) WHERE scope_kind='package' AND rank='primary' AND retired_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS custodian_primary_document
    ON custodian_assignments(content_version_id) WHERE scope_kind='document' AND rank='primary' AND retired_at IS NULL;

CREATE TABLE IF NOT EXISTS person_document_assertions (
    assertion_id TEXT PRIMARY KEY NOT NULL,
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    person_id TEXT NOT NULL REFERENCES persons(person_id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    action TEXT NOT NULL,
    note TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    UNIQUE (content_version_id, person_id, role)
);

CREATE TABLE IF NOT EXISTS person_match_candidates (
    candidate_id TEXT PRIMARY KEY NOT NULL,
    actor_key TEXT NOT NULL,
    display_name TEXT NOT NULL,
    suggested_person_id TEXT,
    reason TEXT NOT NULL,
    evidence_json BLOB NOT NULL,
    evidence_sha256 TEXT NOT NULL,
    occurrence_count INTEGER NOT NULL CHECK (occurrence_count >= 1),
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    state TEXT NOT NULL,
    decided_person_id TEXT,
    created_at TEXT NOT NULL,
    decided_at TEXT
);
CREATE INDEX IF NOT EXISTS person_candidate_state
    ON person_match_candidates(state, created_at, candidate_id);
CREATE UNIQUE INDEX IF NOT EXISTS person_candidate_identity
    ON person_match_candidates(actor_key, suggested_person_id, reason, evidence_sha256) WHERE suggested_person_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS person_candidate_identity_unsuggested
    ON person_match_candidates(actor_key, reason, evidence_sha256) WHERE suggested_person_id IS NULL;

CREATE TABLE IF NOT EXISTS document_people_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    binding_epoch INTEGER NOT NULL CHECK (binding_epoch > 0),
    publication_epoch INTEGER NOT NULL DEFAULT 1 CHECK (publication_epoch > 0),
    resolver_fingerprint TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS document_people_builds (
    operation_id TEXT PRIMARY KEY NOT NULL,
    request_sha256 TEXT NOT NULL, resolver_fingerprint TEXT NOT NULL,
    target_epoch INTEGER NOT NULL, state TEXT NOT NULL,
    scanned INTEGER NOT NULL DEFAULT 0, published INTEGER NOT NULL DEFAULT 0,
    failed INTEGER NOT NULL DEFAULT 0, started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS document_people_generations (
    generation_id TEXT PRIMARY KEY NOT NULL,
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    inputs_sha256 TEXT NOT NULL, resolver_fingerprint TEXT NOT NULL,
    canonical_json BLOB NOT NULL, checksum TEXT NOT NULL, created_at TEXT NOT NULL
);

CREATE TRIGGER IF NOT EXISTS document_people_generations_immutable_update
BEFORE UPDATE ON document_people_generations BEGIN
    SELECT RAISE(ABORT, 'document people generations are immutable');
END;

CREATE TABLE IF NOT EXISTS document_people_heads (
    content_version_id TEXT PRIMARY KEY REFERENCES content_versions(version_id) ON DELETE CASCADE,
    event_generation_id TEXT NOT NULL, inputs_sha256 TEXT NOT NULL, generation_id TEXT NOT NULL,
    unresolved_actors INTEGER NOT NULL, suppressed_actors INTEGER NOT NULL, candidate_overflow INTEGER NOT NULL,
    resolver_fingerprint TEXT NOT NULL, binding_epoch INTEGER NOT NULL, node_revision INTEGER NOT NULL,
    edge_count INTEGER NOT NULL CHECK (edge_count >= 0),
    state TEXT NOT NULL, failure_reason TEXT NOT NULL, published_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS document_people (
    content_version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    person_id TEXT NOT NULL REFERENCES persons(person_id) ON DELETE CASCADE,
    generation_id TEXT NOT NULL, role TEXT NOT NULL, actor_key TEXT NOT NULL,
    evidence_kind TEXT NOT NULL, evidence_id TEXT NOT NULL, confidence TEXT NOT NULL, basis TEXT NOT NULL,
    raw_label TEXT NOT NULL, claim_count INTEGER NOT NULL CHECK (claim_count >= 1),
    first_axis_key TEXT, last_axis_key TEXT,
    sensitive INTEGER NOT NULL CHECK (sensitive IN (0, 1)),
    PRIMARY KEY (content_version_id, person_id, role, actor_key, evidence_kind, evidence_id)
);
CREATE INDEX IF NOT EXISTS document_people_person ON document_people(person_id, content_version_id);

-- Invalidate before version or node deletion cascades remove assertions.
CREATE TRIGGER IF NOT EXISTS content_versions_invalidate_person_bindings
BEFORE DELETE ON content_versions
WHEN EXISTS (SELECT 1 FROM person_document_assertions WHERE content_version_id=OLD.version_id)
BEGIN
    UPDATE document_people_state SET binding_epoch=binding_epoch+1,
        updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE singleton=1;
END;

CREATE TABLE IF NOT EXISTS person_splits (
    operation_id TEXT PRIMARY KEY NOT NULL,
    request_sha256 TEXT NOT NULL,
    receipt_json BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS persons_name_folded ON persons(display_name_folded, person_id);
CREATE INDEX IF NOT EXISTS person_identities_person ON person_identities(person_id, kind);
CREATE INDEX IF NOT EXISTS person_external_person ON person_external_identities(person_id);
CREATE INDEX IF NOT EXISTS custodian_assignments_person ON custodian_assignments(person_id, scope_kind);

-- Mailbox containers retain ordered ordinary blobs without changing pack v1.
CREATE TABLE IF NOT EXISTS mailbox_containers (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    size INTEGER NOT NULL CHECK(size > 0),
    format TEXT NOT NULL,
    state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS mailbox_containers_owner ON mailbox_containers(owner,state);
CREATE TABLE IF NOT EXISTS mailbox_chunks (
    container_id TEXT NOT NULL REFERENCES mailbox_containers(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL CHECK(chunk_index >= 0),
    blob_hash TEXT NOT NULL REFERENCES blobs(hash),
    size INTEGER NOT NULL CHECK(size > 0),
    PRIMARY KEY(container_id,chunk_index)
);
CREATE INDEX IF NOT EXISTS mailbox_chunks_blob ON mailbox_chunks(blob_hash);
CREATE TABLE IF NOT EXISTS mailbox_archives (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    description TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mailbox_transfer_receipts (
    id TEXT PRIMARY KEY,
    archive_id TEXT NOT NULL REFERENCES mailbox_archives(id),
    source_ref TEXT NOT NULL,
    target_version_id TEXT NOT NULL REFERENCES content_versions(version_id),
    document_publication_id TEXT NOT NULL REFERENCES email_document_publications(operation_id),
    receipt_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS mailbox_transfer_versions ON mailbox_transfer_receipts(target_version_id);
CREATE TABLE IF NOT EXISTS mailbox_transfer_heads (
    archive_id TEXT NOT NULL REFERENCES mailbox_archives(id),
    source_ref TEXT NOT NULL,
    receipt_id TEXT NOT NULL REFERENCES mailbox_transfer_receipts(id),
    PRIMARY KEY(archive_id,source_ref)
);
CREATE TABLE IF NOT EXISTS mailbox_jobs (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    container_id TEXT NOT NULL REFERENCES mailbox_containers(id),
    state TEXT NOT NULL,
    claim TEXT NOT NULL,
    job_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS mailbox_jobs_state ON mailbox_jobs(state,owner,id);
CREATE TABLE IF NOT EXISTS mailbox_occurrences (
    job_id TEXT NOT NULL REFERENCES mailbox_jobs(id),
    ordinal INTEGER NOT NULL CHECK(ordinal > 0),
    receipt_id TEXT REFERENCES mailbox_transfer_receipts(id),
    occurrence_json TEXT NOT NULL,
    PRIMARY KEY(job_id,ordinal)
);

-- Production policy and privilege authority is private vault metadata. Closed
-- contract values stay in Go validators; SQLite carries only structural and
-- relational invariants so future contract additions do not rewrite storage.
CREATE TABLE IF NOT EXISTS production_policy_versions (
    policy_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    sha256 TEXT NOT NULL UNIQUE,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(policy_id, version)
);
CREATE TABLE IF NOT EXISTS production_approval_grants (
    approval_id TEXT PRIMARY KEY,
    subject_sha256 TEXT NOT NULL,
    subject_json BLOB NOT NULL,
    authority_sha256 TEXT NOT NULL,
    authority_json BLOB NOT NULL,
    grant_sha256 TEXT NOT NULL UNIQUE,
    grant_json BLOB NOT NULL,
    UNIQUE(approval_id, grant_sha256)
);
CREATE TABLE IF NOT EXISTS production_approval_events (
    event_id TEXT PRIMARY KEY,
    approval_id TEXT NOT NULL REFERENCES production_approval_grants(approval_id),
    event_sha256 TEXT NOT NULL UNIQUE,
    canonical_json BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS production_approval_events_grant
    ON production_approval_events(approval_id, event_id);
CREATE TABLE IF NOT EXISTS production_players_snapshots (
    snapshot_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    sha256 TEXT NOT NULL UNIQUE,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(snapshot_id, revision)
);
CREATE TABLE IF NOT EXISTS production_withheld_selections (
    selection_id TEXT PRIMARY KEY,
    set_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    policy_sha256 TEXT NOT NULL REFERENCES production_policy_versions(sha256),
    sha256 TEXT NOT NULL UNIQUE,
    canonical_json BLOB NOT NULL,
    UNIQUE(set_id, revision)
);
CREATE TABLE IF NOT EXISTS production_privilege_log_drafts (
    log_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    generation INTEGER NOT NULL CHECK(generation > 0),
    predecessor_log_id TEXT,
    predecessor_receipt_sha256 TEXT,
    withheld_selection_sha256 TEXT NOT NULL REFERENCES production_withheld_selections(sha256),
    policy_sha256 TEXT NOT NULL REFERENCES production_policy_versions(sha256),
    players_sha256 TEXT NOT NULL REFERENCES production_players_snapshots(sha256),
    produced_sha256 TEXT NOT NULL,
    produced_json BLOB NOT NULL,
    rows_sha256 TEXT NOT NULL,
    PRIMARY KEY(log_id, revision)
);
CREATE TABLE IF NOT EXISTS production_privilege_log_rows (
    log_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    row_ordinal INTEGER NOT NULL CHECK(row_ordinal > 0),
    row_id TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(log_id, revision, row_id),
    UNIQUE(log_id, revision, row_ordinal),
    FOREIGN KEY(log_id, revision)
        REFERENCES production_privilege_log_drafts(log_id, revision) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS production_privilege_log_validations (
    log_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    operation_id TEXT NOT NULL UNIQUE,
    request_sha256 TEXT NOT NULL,
    draft_generation INTEGER NOT NULL,
    inputs_sha256 TEXT NOT NULL,
    rows_sha256 TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(log_id, revision),
    FOREIGN KEY(log_id, revision)
        REFERENCES production_privilege_log_drafts(log_id, revision) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS production_privilege_log_approvals (
    log_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    approval_id TEXT NOT NULL REFERENCES production_approval_grants(approval_id),
    evaluation_sha256 TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(log_id, revision),
    FOREIGN KEY(log_id, revision)
        REFERENCES production_privilege_log_drafts(log_id, revision) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS production_privilege_log_receipts (
    log_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    sha256 TEXT NOT NULL UNIQUE,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(log_id, revision),
    UNIQUE(log_id, revision, sha256),
    FOREIGN KEY(log_id, revision)
        REFERENCES production_privilege_log_drafts(log_id, revision)
);
CREATE TABLE IF NOT EXISTS production_privilege_log_attachments (
    attachment_id TEXT PRIMARY KEY,
    privilege_log_receipt_sha256 TEXT NOT NULL
        REFERENCES production_privilege_log_receipts(sha256),
    sha256 TEXT NOT NULL UNIQUE,
    canonical_json BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS production_operation_receipts (
    operation_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    response_sha256 TEXT NOT NULL,
    response_json BLOB NOT NULL
);

CREATE TRIGGER IF NOT EXISTS production_policy_versions_immutable_update
BEFORE UPDATE ON production_policy_versions BEGIN
    SELECT RAISE(ABORT, 'production policy versions are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_policy_versions_immutable_delete
BEFORE DELETE ON production_policy_versions BEGIN
    SELECT RAISE(ABORT, 'production policy versions are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_approval_grants_immutable_update
BEFORE UPDATE ON production_approval_grants BEGIN
    SELECT RAISE(ABORT, 'production approval grants are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_approval_grants_immutable_delete
BEFORE DELETE ON production_approval_grants BEGIN
    SELECT RAISE(ABORT, 'production approval grants are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_approval_events_immutable_update
BEFORE UPDATE ON production_approval_events BEGIN
    SELECT RAISE(ABORT, 'production approval events are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_approval_events_immutable_delete
BEFORE DELETE ON production_approval_events BEGIN
    SELECT RAISE(ABORT, 'production approval events are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_players_snapshots_immutable_update
BEFORE UPDATE ON production_players_snapshots BEGIN
    SELECT RAISE(ABORT, 'production players snapshots are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_players_snapshots_immutable_delete
BEFORE DELETE ON production_players_snapshots BEGIN
    SELECT RAISE(ABORT, 'production players snapshots are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_withheld_selections_immutable_update
BEFORE UPDATE ON production_withheld_selections BEGIN
    SELECT RAISE(ABORT, 'production withheld selections are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_withheld_selections_immutable_delete
BEFORE DELETE ON production_withheld_selections BEGIN
    SELECT RAISE(ABORT, 'production withheld selections are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_log_validations_immutable_update
BEFORE UPDATE ON production_privilege_log_validations BEGIN
    SELECT RAISE(ABORT, 'production privilege validations are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_log_receipts_immutable_update
BEFORE UPDATE ON production_privilege_log_receipts BEGIN
    SELECT RAISE(ABORT, 'production privilege receipts are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_log_receipts_immutable_delete
BEFORE DELETE ON production_privilege_log_receipts BEGIN
    SELECT RAISE(ABORT, 'production privilege receipts are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_log_attachments_immutable_update
BEFORE UPDATE ON production_privilege_log_attachments BEGIN
    SELECT RAISE(ABORT, 'production privilege attachments are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_log_attachments_immutable_delete
BEFORE DELETE ON production_privilege_log_attachments BEGIN
    SELECT RAISE(ABORT, 'production privilege attachments are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_operation_receipts_immutable_update
BEFORE UPDATE ON production_operation_receipts BEGIN
    SELECT RAISE(ABORT, 'production operation receipts are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_operation_receipts_immutable_delete
BEFORE DELETE ON production_operation_receipts BEGIN
    SELECT RAISE(ABORT, 'production operation receipts are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_draft_frozen_update
BEFORE UPDATE ON production_privilege_log_drafts
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=OLD.log_id AND r.revision=OLD.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege draft is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_draft_frozen_delete
BEFORE DELETE ON production_privilege_log_drafts
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=OLD.log_id AND r.revision=OLD.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege draft is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_rows_frozen_update
BEFORE UPDATE ON production_privilege_log_rows
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=OLD.log_id AND r.revision=OLD.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege rows are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_rows_frozen_insert
BEFORE INSERT ON production_privilege_log_rows
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=NEW.log_id AND r.revision=NEW.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege rows are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_rows_frozen_delete
BEFORE DELETE ON production_privilege_log_rows
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=OLD.log_id AND r.revision=OLD.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege rows are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_validation_frozen_insert
BEFORE INSERT ON production_privilege_log_validations
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=NEW.log_id AND r.revision=NEW.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege validation is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_validation_frozen_delete
BEFORE DELETE ON production_privilege_log_validations
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=OLD.log_id AND r.revision=OLD.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege validation is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_approval_frozen_insert
BEFORE INSERT ON production_privilege_log_approvals
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=NEW.log_id AND r.revision=NEW.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege approval is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_approval_frozen_update
BEFORE UPDATE ON production_privilege_log_approvals
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=OLD.log_id AND r.revision=OLD.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege approval is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_privilege_approval_frozen_delete
BEFORE DELETE ON production_privilege_log_approvals
WHEN EXISTS(SELECT 1 FROM production_privilege_log_receipts r
            WHERE r.log_id=OLD.log_id AND r.revision=OLD.revision) BEGIN
    SELECT RAISE(ABORT, 'frozen production privilege approval is immutable');
END;

CREATE UNIQUE INDEX IF NOT EXISTS content_versions_production_identity
    ON content_versions(node_id, version_id, blob_hash, size);

-- Exact current page-document and page-frame identities. The production map
-- rows below cannot select a different frame checksum for the same version.
CREATE UNIQUE INDEX IF NOT EXISTS page_documents_production_identity
    ON page_documents(version_id, checksum);
CREATE UNIQUE INDEX IF NOT EXISTS page_frames_production_identity
    ON page_frames(version_id, page, checksum);

-- Exact derived-PDF authority. A production text map either uses source bytes
-- directly or names one attached rendition build and one retained artifact.
CREATE UNIQUE INDEX IF NOT EXISTS rendition_attachments_production_identity
    ON rendition_attachments(content_version_id, build_id, attachment_id);
CREATE UNIQUE INDEX IF NOT EXISTS rendition_artifacts_production_identity
    ON rendition_artifacts(build_id, artifact_id, blob_hash, size);

-- Bind policy ID, version, and digest to the same immutable policy row.
CREATE UNIQUE INDEX IF NOT EXISTS production_policy_versions_identity
    ON production_policy_versions(policy_id, version, sha256);

CREATE TABLE IF NOT EXISTS production_text_maps (
    map_sha256              TEXT PRIMARY KEY,
    source_node_id          INTEGER NOT NULL,
    version_id              TEXT NOT NULL,
    source_sha256           TEXT NOT NULL,
    source_size             INTEGER NOT NULL,
    pdf_sha256              TEXT NOT NULL REFERENCES blobs(hash) ON DELETE RESTRICT,
    pdf_size                INTEGER NOT NULL,
    page_document_sha256    TEXT NOT NULL,
    evidence_sha256         TEXT NOT NULL,
    page_inventory_sha256   TEXT NOT NULL,
    rendition_attachment_id TEXT,
    rendition_build_id      TEXT,
    rendition_artifact_id   TEXT,
    canonical_json          BLOB NOT NULL,
    created_at              TEXT NOT NULL,
    UNIQUE(map_sha256, page_inventory_sha256),
    UNIQUE(map_sha256, version_id),
    FOREIGN KEY(source_node_id, version_id, source_sha256, source_size)
        REFERENCES content_versions(node_id, version_id, blob_hash, size)
        ON DELETE RESTRICT,
    FOREIGN KEY(version_id, page_document_sha256)
        REFERENCES page_documents(version_id, checksum)
        ON DELETE RESTRICT,
    FOREIGN KEY(version_id, rendition_build_id, rendition_attachment_id)
        REFERENCES rendition_attachments(content_version_id, build_id, attachment_id)
        ON DELETE RESTRICT,
    FOREIGN KEY(rendition_build_id, rendition_artifact_id, pdf_sha256, pdf_size)
        REFERENCES rendition_artifacts(build_id, artifact_id, blob_hash, size)
        ON DELETE RESTRICT,
    CHECK (
        (rendition_attachment_id IS NULL AND rendition_build_id IS NULL
            AND rendition_artifact_id IS NULL
            AND pdf_sha256 = source_sha256 AND pdf_size = source_size)
        OR
        (rendition_attachment_id IS NOT NULL AND rendition_build_id IS NOT NULL
            AND rendition_artifact_id IS NOT NULL)
    )
);

CREATE TABLE IF NOT EXISTS production_text_map_frames (
    map_sha256    TEXT NOT NULL,
    version_id    TEXT NOT NULL,
    page          INTEGER NOT NULL,
    frame_sha256  TEXT NOT NULL,
    width         INTEGER NOT NULL,
    height        INTEGER NOT NULL,
    PRIMARY KEY(map_sha256, page),
    FOREIGN KEY(map_sha256, version_id)
        REFERENCES production_text_maps(map_sha256, version_id)
        ON DELETE RESTRICT,
    FOREIGN KEY(version_id, page, frame_sha256)
        REFERENCES page_frames(version_id, page, checksum)
        ON DELETE RESTRICT
);

CREATE TRIGGER IF NOT EXISTS production_text_maps_immutable_update
BEFORE UPDATE ON production_text_maps BEGIN
    SELECT RAISE(ABORT, 'production text maps are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_text_map_frames_immutable_update
BEFORE UPDATE ON production_text_map_frames BEGIN
    SELECT RAISE(ABORT, 'production text map frames are immutable');
END;

CREATE TABLE IF NOT EXISTS production_catalog_entries (
    kind           TEXT NOT NULL,
    id             TEXT NOT NULL,
    sha256         TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    created_at     TEXT NOT NULL,
    PRIMARY KEY(kind, id, sha256),
    UNIQUE(kind, id)
);

CREATE TRIGGER IF NOT EXISTS production_catalog_entries_immutable_update
BEFORE UPDATE ON production_catalog_entries BEGIN
    SELECT RAISE(ABORT, 'production catalog entries are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_catalog_entries_immutable_delete
BEFORE DELETE ON production_catalog_entries BEGIN
    SELECT RAISE(ABORT, 'production catalog entries are immutable');
END;

CREATE TABLE IF NOT EXISTS production_sets (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    creator       TEXT NOT NULL,
    head_revision INTEGER NOT NULL,
    created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS production_revisions (
    set_id                    TEXT NOT NULL REFERENCES production_sets(id) ON DELETE CASCADE,
    revision                  INTEGER NOT NULL,
    predecessor_revision      INTEGER,
    state                     TEXT NOT NULL,
    etag                      INTEGER NOT NULL,
    instructions              BLOB NOT NULL,
    instructions_sha256       TEXT NOT NULL,
    member_hash               TEXT NOT NULL,
    decisions_sha256          TEXT NOT NULL,
    membership_sealed         INTEGER NOT NULL,
    recipe_kind               TEXT NOT NULL,
    recipe_id                 TEXT NOT NULL,
    recipe_sha256             TEXT NOT NULL,
    profile_kind              TEXT NOT NULL,
    profile_id                TEXT NOT NULL,
    profile_sha256            TEXT NOT NULL,
    disclosure_profile_kind   TEXT NOT NULL,
    disclosure_profile_id     TEXT NOT NULL,
    disclosure_profile_sha256 TEXT NOT NULL,
    numbering_recipe_kind     TEXT NOT NULL,
    numbering_recipe_id       TEXT NOT NULL,
    numbering_recipe_sha256   TEXT NOT NULL,
    policy_id                 TEXT NOT NULL,
    policy_version            INTEGER NOT NULL,
    policy_sha256             TEXT NOT NULL,
    created_at                TEXT NOT NULL,
    PRIMARY KEY(set_id, revision),
    FOREIGN KEY(recipe_kind, recipe_id, recipe_sha256)
        REFERENCES production_catalog_entries(kind, id, sha256),
    FOREIGN KEY(profile_kind, profile_id, profile_sha256)
        REFERENCES production_catalog_entries(kind, id, sha256),
    FOREIGN KEY(disclosure_profile_kind, disclosure_profile_id, disclosure_profile_sha256)
        REFERENCES production_catalog_entries(kind, id, sha256),
    FOREIGN KEY(numbering_recipe_kind, numbering_recipe_id, numbering_recipe_sha256)
        REFERENCES production_catalog_entries(kind, id, sha256),
    FOREIGN KEY(policy_id, policy_version, policy_sha256)
        REFERENCES production_policy_versions(policy_id, version, sha256),
    FOREIGN KEY(set_id, predecessor_revision)
        REFERENCES production_revisions(set_id, revision)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS production_members (
    set_id               TEXT NOT NULL,
    revision             INTEGER NOT NULL,
    member_id            TEXT NOT NULL,
    ordinal              INTEGER NOT NULL,
    vault_id             TEXT NOT NULL,
    node_id              INTEGER NOT NULL,
    version_id           TEXT NOT NULL,
    source_sha256        TEXT NOT NULL,
    source_size          INTEGER NOT NULL,
    pdf_sha256           TEXT NOT NULL REFERENCES blobs(hash) ON DELETE RESTRICT,
    pdf_size             INTEGER NOT NULL,
    map_sha256           TEXT NOT NULL,
    page_inventory_sha256 TEXT NOT NULL,
    family_context_json  BLOB NOT NULL,
    canonical_json       BLOB NOT NULL,
    PRIMARY KEY(set_id, revision, member_id),
    UNIQUE(set_id, revision, ordinal),
    FOREIGN KEY(set_id, revision)
        REFERENCES production_revisions(set_id, revision) ON DELETE CASCADE,
    FOREIGN KEY(node_id, version_id, source_sha256, source_size)
        REFERENCES content_versions(node_id, version_id, blob_hash, size)
        ON DELETE RESTRICT,
    FOREIGN KEY(map_sha256, page_inventory_sha256)
        REFERENCES production_text_maps(map_sha256, page_inventory_sha256)
        ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS production_members_source
    ON production_members(version_id, source_sha256);

-- Gate evidence is selected by an immutable production revision. Facts name
-- one source-metadata generation; email families name one publication
-- operation; approval and privilege selectors name exact digests rather than
-- relying on whichever matching record a query happens to find.
CREATE TABLE IF NOT EXISTS production_member_policy_facts (
    set_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    member_id TEXT NOT NULL,
    source_sha256 TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    evidence_sha256 TEXT NOT NULL,
    allowlist_version TEXT NOT NULL,
    facts_sha256 TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(set_id, revision, member_id),
    FOREIGN KEY(set_id, revision, member_id)
        REFERENCES production_members(set_id, revision, member_id) ON DELETE RESTRICT,
    FOREIGN KEY(source_sha256, generation_id)
        REFERENCES source_metadata_generations(source_sha256, generation_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS production_revision_email_publications (
    set_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    root_version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE RESTRICT,
    operation_id TEXT NOT NULL REFERENCES email_document_publications(operation_id) ON DELETE RESTRICT,
    request_digest TEXT NOT NULL,
    receipt_sha256 TEXT NOT NULL,
    PRIMARY KEY(set_id, revision, root_version_id),
    FOREIGN KEY(set_id, revision)
        REFERENCES production_revisions(set_id, revision) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS production_revision_gate_authority (
    set_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    approval_id TEXT,
    approval_grant_sha256 TEXT,
    approval_subject_sha256 TEXT,
    privilege_log_id TEXT,
    privilege_log_revision INTEGER,
    privilege_log_receipt_sha256 TEXT,
    PRIMARY KEY(set_id, revision),
    FOREIGN KEY(set_id, revision)
        REFERENCES production_revisions(set_id, revision) ON DELETE RESTRICT,
    FOREIGN KEY(approval_id, approval_grant_sha256)
        REFERENCES production_approval_grants(approval_id, grant_sha256) ON DELETE RESTRICT,
    FOREIGN KEY(privilege_log_id, privilege_log_revision, privilege_log_receipt_sha256)
        REFERENCES production_privilege_log_receipts(log_id, revision, sha256) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS production_decisions (
    set_id         TEXT NOT NULL,
    revision       INTEGER NOT NULL,
    decision_id    TEXT NOT NULL,
    member_id      TEXT NOT NULL,
    actor          TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    PRIMARY KEY(set_id, revision, decision_id),
    FOREIGN KEY(set_id, revision, member_id)
        REFERENCES production_members(set_id, revision, member_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS production_decisions_member
    ON production_decisions(set_id, revision, member_id, decision_id);

CREATE TABLE IF NOT EXISTS production_operations (
    operation_id   TEXT PRIMARY KEY,
    set_id         TEXT NOT NULL REFERENCES production_sets(id),
    actor          TEXT NOT NULL,
    kind           TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    receipt_json   BLOB NOT NULL,
    created_at     TEXT NOT NULL
);

CREATE TRIGGER IF NOT EXISTS production_operations_immutable_update
BEFORE UPDATE ON production_operations BEGIN
    SELECT RAISE(ABORT, 'production operations are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_operations_immutable_delete
BEFORE DELETE ON production_operations BEGIN
    SELECT RAISE(ABORT, 'production operations are immutable');
END;

CREATE TABLE IF NOT EXISTS production_audit_evidence (
    sequence       INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id   TEXT NOT NULL UNIQUE REFERENCES production_operations(operation_id),
    set_id         TEXT NOT NULL REFERENCES production_sets(id),
    revision       INTEGER NOT NULL,
    actor          TEXT NOT NULL,
    kind           TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    receipt_sha256 TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    FOREIGN KEY(set_id, revision)
        REFERENCES production_revisions(set_id, revision)
);

CREATE TRIGGER IF NOT EXISTS production_audit_evidence_immutable_update
BEFORE UPDATE ON production_audit_evidence BEGIN
    SELECT RAISE(ABORT, 'production audit evidence is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_audit_evidence_immutable_delete
BEFORE DELETE ON production_audit_evidence BEGIN
    SELECT RAISE(ABORT, 'production audit evidence is immutable');
END;

CREATE TRIGGER IF NOT EXISTS production_member_policy_facts_immutable_update
BEFORE UPDATE ON production_member_policy_facts BEGIN
    SELECT RAISE(ABORT, 'production member policy facts are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_member_policy_facts_immutable_delete
BEFORE DELETE ON production_member_policy_facts BEGIN
    SELECT RAISE(ABORT, 'production member policy facts are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_revision_email_publications_immutable_update
BEFORE UPDATE ON production_revision_email_publications BEGIN
    SELECT RAISE(ABORT, 'production revision email publications are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_revision_email_publications_immutable_delete
BEFORE DELETE ON production_revision_email_publications BEGIN
    SELECT RAISE(ABORT, 'production revision email publications are immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_revision_gate_authority_immutable_update
BEFORE UPDATE ON production_revision_gate_authority BEGIN
    SELECT RAISE(ABORT, 'production revision gate authority is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_revision_gate_authority_immutable_delete
BEFORE DELETE ON production_revision_gate_authority BEGIN
    SELECT RAISE(ABORT, 'production revision gate authority is immutable');
END;
-- A finalized revision is the authority that a prepared-input gate alone
-- cannot provide. Job admission must reference this exact sealed revision.
CREATE TABLE IF NOT EXISTS production_finalized_revisions (
    set_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    etag INTEGER NOT NULL,
    draft_sha256 TEXT NOT NULL,          -- canonical draft_json digest
    prepared_sha256 TEXT NOT NULL,       -- sealed PreparedProduction digest
    prepared_input_sha256 TEXT NOT NULL, -- passing gate receipt digest
    draft_json BLOB NOT NULL,
    prepared_input_json BLOB NOT NULL,
    numbering_namespace_id TEXT NOT NULL,
    numbering_snapshot_id TEXT NOT NULL,
    numbering_recipe_sha256 TEXT NOT NULL,
    numbering_start_at INTEGER NOT NULL,
    finalized_at TEXT NOT NULL,
    PRIMARY KEY(set_id, revision),
    FOREIGN KEY(set_id, revision)
        REFERENCES production_revisions(set_id, revision) ON DELETE RESTRICT
);
CREATE TRIGGER IF NOT EXISTS production_finalized_revisions_immutable_update
BEFORE UPDATE ON production_finalized_revisions BEGIN
    SELECT RAISE(ABORT, 'finalized production revision is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_finalized_revisions_immutable_delete
BEFORE DELETE ON production_finalized_revisions BEGIN
    SELECT RAISE(ABORT, 'finalized production revision is immutable');
END;

CREATE TABLE IF NOT EXISTS production_jobs (
    job_id TEXT PRIMARY KEY,
    operation_id TEXT NOT NULL UNIQUE,
    owner TEXT NOT NULL,
    state TEXT NOT NULL,
    set_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    etag INTEGER NOT NULL,
    revision_sha256 TEXT NOT NULL,       -- finalized PreparedProduction digest
    prepared_input_sha256 TEXT NOT NULL, -- passing gate receipt digest
    receipt_sha256 TEXT NOT NULL DEFAULT '', -- set once, atomically with success
    request_json BLOB NOT NULL,
    allocation_id TEXT REFERENCES bates_allocations(allocation_id),
    receipt_json BLOB,
    artifact_manifest_json BLOB,
    endorsements_json BLOB,
    claim_epoch INTEGER NOT NULL DEFAULT 0,
    claim_token TEXT NOT NULL DEFAULT '',
    claim_owner TEXT NOT NULL DEFAULT '',
    lease_expires_at TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(set_id, revision) REFERENCES production_finalized_revisions(set_id, revision)
);
CREATE INDEX IF NOT EXISTS production_jobs_state ON production_jobs(state, job_id);
CREATE TRIGGER IF NOT EXISTS production_jobs_immutable_identity
BEFORE UPDATE ON production_jobs
WHEN NEW.job_id<>OLD.job_id OR NEW.operation_id<>OLD.operation_id OR NEW.owner<>OLD.owner
 OR NEW.set_id<>OLD.set_id OR NEW.revision<>OLD.revision OR NEW.etag<>OLD.etag
 OR NEW.revision_sha256<>OLD.revision_sha256
 OR NEW.prepared_input_sha256<>OLD.prepared_input_sha256
 OR NEW.request_json<>OLD.request_json OR NEW.created_at<>OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'production job identity is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_jobs_receipt_once
BEFORE UPDATE ON production_jobs
WHEN NEW.receipt_sha256<>OLD.receipt_sha256 AND
 (OLD.receipt_sha256<>'' OR NEW.receipt_sha256='' OR NEW.state<>'succeeded')
BEGIN
    SELECT RAISE(ABORT, 'production job receipt digest is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_jobs_published_payload_immutable
BEFORE UPDATE ON production_jobs
WHEN OLD.receipt_sha256<>'' AND
 (NEW.receipt_json IS NOT OLD.receipt_json OR
  NEW.artifact_manifest_json IS NOT OLD.artifact_manifest_json OR
  NEW.endorsements_json IS NOT OLD.endorsements_json OR
  NEW.allocation_id IS NOT OLD.allocation_id OR NEW.state<>OLD.state)
BEGIN
    SELECT RAISE(ABORT, 'published production payload is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_jobs_published_delete
BEFORE DELETE ON production_jobs
WHEN OLD.receipt_sha256<>''
BEGIN
    SELECT RAISE(ABORT, 'published production job is immutable');
END;

CREATE TABLE IF NOT EXISTS production_job_artifacts (
    job_id TEXT NOT NULL REFERENCES production_jobs(job_id),
    artifact_id TEXT NOT NULL,
    artifact_json BLOB NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(job_id, artifact_id)
);

-- A render claim may start only after this immutable, reservation-bound plan
-- is retained. The page list is ordered by sealed occurrence and source page.
CREATE TABLE IF NOT EXISTS production_job_render_plans (
    job_id TEXT PRIMARY KEY REFERENCES production_jobs(job_id) ON DELETE RESTRICT,
    allocation_id TEXT NOT NULL REFERENCES bates_allocations(allocation_id),
    reservation_sha256 TEXT NOT NULL,
    plan_sha256 TEXT NOT NULL,
    canonical_json BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS production_job_render_plans_immutable_update
BEFORE UPDATE ON production_job_render_plans BEGIN
    SELECT RAISE(ABORT, 'production render plan is immutable');
END;
CREATE TRIGGER IF NOT EXISTS production_job_render_plans_immutable_delete
BEFORE DELETE ON production_job_render_plans BEGIN
    SELECT RAISE(ABORT, 'production render plan is immutable');
END;
