-- The PostgreSQL identity follows the same logical vault through export and
-- restore. This version is independent of SQLite's physical schema version.
CREATE TABLE {{schema}}.vault_metadata (
    singleton      SMALLINT PRIMARY KEY,
    vault_uid      UUID NOT NULL UNIQUE,
    schema_version INTEGER NOT NULL
);

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
