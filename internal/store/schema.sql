-- docbank core schema. Idempotent: applied on every Open.

-- One stable logical identity follows the vault through JSONL backup and
-- restore. Filesystem location is deliberately not identity.
CREATE TABLE IF NOT EXISTS vault_metadata (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    vault_id  TEXT NOT NULL UNIQUE
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
CREATE INDEX IF NOT EXISTS nodes_trashed ON nodes(trashed_at) WHERE trashed_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS blobs (
    hash       TEXT PRIMARY KEY,
    size       INTEGER NOT NULL,
    created_at TEXT NOT NULL
);

-- Physical packed-CAS metadata. blobs remains the membership authority:
-- deleting a blob row revokes reads, while maintenance later prunes any stale
-- mapping and reclaims dead bytes from the immutable pack. Pack rows remain
-- until their files have been retired so the table is a truthful inventory.
CREATE TABLE IF NOT EXISTS blob_packs (
    pack_id      TEXT PRIMARY KEY,
    entry_count  INTEGER NOT NULL CHECK (entry_count >= 0),
    stored_bytes INTEGER NOT NULL CHECK (stored_bytes >= 0),
    created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS blob_pack_index (
    blob_hash   TEXT PRIMARY KEY,
    pack_id     TEXT NOT NULL REFERENCES blob_packs(pack_id) ON DELETE CASCADE,
    pack_offset INTEGER NOT NULL CHECK (pack_offset >= 0),
    stored_len  INTEGER NOT NULL CHECK (stored_len >= 0),
    raw_len     INTEGER NOT NULL CHECK (raw_len >= 0),
    flags       INTEGER NOT NULL CHECK (flags BETWEEN 0 AND 255),
    crc32c      INTEGER NOT NULL CHECK (crc32c BETWEEN 0 AND 4294967295)
);

CREATE INDEX IF NOT EXISTS blob_pack_index_pack ON blob_pack_index(pack_id);

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

CREATE TABLE IF NOT EXISTS ingests (
    id          TEXT PRIMARY KEY NOT NULL,
    started_at  TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_desc TEXT NOT NULL
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
    record_json        TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS audit_record_event
    ON audit_records(event_id) WHERE event_id IS NOT NULL;
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
    blob_hash         TEXT NOT NULL,
    extractor         TEXT NOT NULL,
    extractor_version INTEGER NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('ok', 'failed')),
    error             TEXT,
    attempts          INTEGER NOT NULL DEFAULT 0,
    text              TEXT,
    extracted_at      TEXT NOT NULL,
    PRIMARY KEY (blob_hash, extractor)
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
