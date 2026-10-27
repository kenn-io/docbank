-- The PostgreSQL identity follows the same logical vault through export and
-- restore. This version is independent of SQLite's physical schema version.
CREATE TABLE {{schema}}.vault_metadata (
    singleton      SMALLINT PRIMARY KEY,
    vault_uid      UUID NOT NULL UNIQUE,
    schema_version INTEGER NOT NULL
);

-- Private local records are excluded from portable metadata.
CREATE TABLE {{schema}}.miss_log_records (
    id TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    receipt TEXT NOT NULL
);
CREATE INDEX miss_log_records_created ON {{schema}}.miss_log_records(created_at DESC, id DESC);

-- Keep timestamps and content identities in the shared store's text form.
-- Identity allocation must not reuse a committed node ID after deletion.
CREATE TABLE {{schema}}.nodes (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    parent_id          BIGINT REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    kind               TEXT NOT NULL,
    current_version_id TEXT,
    revision           BIGINT NOT NULL DEFAULT 1,
    created_at         TEXT NOT NULL,
    modified_at        TEXT NOT NULL,
    trashed_at         TEXT,
    trash_parent       BIGINT REFERENCES {{schema}}.nodes(id) ON DELETE SET NULL,
    trash_name         TEXT
);
CREATE UNIQUE INDEX one_root ON {{schema}}.nodes((1)) WHERE parent_id IS NULL;
CREATE UNIQUE INDEX live_sibling_names ON {{schema}}.nodes(parent_id, name)
    WHERE trashed_at IS NULL;
CREATE INDEX nodes_parent ON {{schema}}.nodes(parent_id);
CREATE INDEX nodes_parent_name_id ON {{schema}}.nodes(parent_id, name, id);
CREATE INDEX nodes_live_modified ON {{schema}}.nodes(modified_at DESC, name, id)
    WHERE trashed_at IS NULL;
CREATE INDEX nodes_trashed ON {{schema}}.nodes(trashed_at) WHERE trashed_at IS NOT NULL;

CREATE TABLE {{schema}}.blobs (
    hash       TEXT PRIMARY KEY,
    size       BIGINT NOT NULL,
    created_at TEXT NOT NULL
);

-- SHA-256 owns content identity; MD5 is auxiliary interoperability metadata.
CREATE TABLE {{schema}}.blob_checksums (
    blob_sha256 TEXT PRIMARY KEY REFERENCES {{schema}}.blobs(hash) ON DELETE CASCADE,
    md5         TEXT NOT NULL
);

CREATE TABLE {{schema}}.content_versions (
    version_id              TEXT PRIMARY KEY,
    node_id                 BIGINT NOT NULL REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE,
    blob_hash               TEXT NOT NULL REFERENCES {{schema}}.blobs(hash),
    size                    BIGINT NOT NULL,
    mime_type               TEXT,
    recorded_at             TEXT NOT NULL,
    node_revision           BIGINT NOT NULL,
    introduced_operation_id TEXT NOT NULL,
    transition_kind         TEXT NOT NULL,
    source_version_id       TEXT REFERENCES {{schema}}.content_versions(version_id)
        DEFERRABLE INITIALLY DEFERRED,
    UNIQUE (node_id, node_revision),
    UNIQUE (node_id, introduced_operation_id),
    UNIQUE (node_id, version_id)
);
ALTER TABLE {{schema}}.nodes ADD FOREIGN KEY (id, current_version_id)
    REFERENCES {{schema}}.content_versions(node_id, version_id)
    DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX content_versions_node ON {{schema}}.content_versions(node_id, node_revision DESC);
CREATE INDEX content_versions_blob ON {{schema}}.content_versions(blob_hash);
CREATE INDEX content_versions_source ON {{schema}}.content_versions(source_version_id)
    WHERE source_version_id IS NOT NULL;

-- Canonical audit records and their projections retain the shared store's
-- content hashes and typed JSON. Validation stays in the Go audit owner.
CREATE TABLE {{schema}}.audit_records (
    digest             TEXT PRIMARY KEY,
    kind               TEXT NOT NULL,
    operation_id       TEXT,
    operation_sequence BIGINT,
    scope_id           TEXT,
    entry_count        BIGINT,
    event_id           TEXT,
    event_ordinal      BIGINT,
    node_id            BIGINT,
    record_json        TEXT NOT NULL
);
CREATE UNIQUE INDEX audit_record_event
    ON {{schema}}.audit_records(event_id) WHERE event_id IS NOT NULL;
CREATE INDEX audit_record_event_node
    ON {{schema}}.audit_records(node_id) WHERE kind = 'event';
CREATE INDEX audit_record_event_scope
    ON {{schema}}.audit_records(scope_id) WHERE kind = 'event';
CREATE UNIQUE INDEX audit_record_mutation_operation
    ON {{schema}}.audit_records(operation_id) WHERE kind = 'canonical_mutation';
CREATE UNIQUE INDEX audit_record_mutation_sequence
    ON {{schema}}.audit_records(operation_sequence) WHERE kind = 'canonical_mutation';
CREATE UNIQUE INDEX audit_record_scope_entry
    ON {{schema}}.audit_records(scope_id, entry_count) WHERE kind = 'scope_chain_entry';
CREATE UNIQUE INDEX audit_record_allocation_operation
    ON {{schema}}.audit_records(operation_id) WHERE kind = 'allocation_entry';
CREATE UNIQUE INDEX audit_record_allocation_sequence
    ON {{schema}}.audit_records(operation_sequence) WHERE kind = 'allocation_entry';
CREATE UNIQUE INDEX audit_record_single_genesis
    ON {{schema}}.audit_records(kind) WHERE kind IN (
        'topology_genesis', 'attached_metadata_genesis', 'allocation_genesis'
    );

CREATE TABLE {{schema}}.audit_authority (
    singleton                     SMALLINT PRIMARY KEY,
    lineage_id                    TEXT NOT NULL UNIQUE,
    operation_sequence_high_water BIGINT NOT NULL,
    allocation_genesis_digest     TEXT NOT NULL UNIQUE REFERENCES {{schema}}.audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED,
    allocation_entry_count        BIGINT NOT NULL,
    allocation_head               TEXT NOT NULL REFERENCES {{schema}}.audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE {{schema}}.audit_scopes (
    scope_id            TEXT PRIMARY KEY,
    target_node_id      BIGINT NOT NULL REFERENCES {{schema}}.nodes(id),
    enable_operation_id TEXT NOT NULL UNIQUE,
    entry_count         BIGINT NOT NULL,
    chain_head          TEXT NOT NULL REFERENCES {{schema}}.audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE {{schema}}.audit_baselines (
    digest         TEXT PRIMARY KEY REFERENCES {{schema}}.audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED,
    scope_id       TEXT NOT NULL REFERENCES {{schema}}.audit_scopes(scope_id)
        DEFERRABLE INITIALLY DEFERRED,
    target_node_id BIGINT NOT NULL REFERENCES {{schema}}.nodes(id),
    operation_id   TEXT NOT NULL,
    UNIQUE (scope_id, target_node_id, operation_id)
);
CREATE TABLE {{schema}}.audit_memberships (
    scope_id        TEXT NOT NULL REFERENCES {{schema}}.audit_scopes(scope_id)
        DEFERRABLE INITIALLY DEFERRED,
    node_id         BIGINT NOT NULL REFERENCES {{schema}}.nodes(id),
    baseline_digest TEXT NOT NULL REFERENCES {{schema}}.audit_baselines(digest)
        DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (scope_id, node_id)
);
CREATE INDEX audit_membership_node ON {{schema}}.audit_memberships(node_id);
