-- Docbank metadata-v1 logical schema. Before the first public release this is
-- the only supported shape; older development vaults are intentionally not
-- migration inputs. Physical pack authority and rebuildable search indexes do
-- not belong to the portable model.

CREATE TABLE vault_metadata (
    singleton      INTEGER PRIMARY KEY CHECK (singleton = 1),
    format_version INTEGER NOT NULL CHECK (format_version = 1),
    vault_id       TEXT NOT NULL UNIQUE
);

CREATE TABLE blobs (
    hash       TEXT PRIMARY KEY,
    size       INTEGER NOT NULL CHECK (size >= 0),
    created_at TEXT NOT NULL
);

CREATE TABLE nodes (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id          INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    name               BLOB NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('dir', 'file')),
    current_version_id TEXT,
    revision           INTEGER NOT NULL CHECK (revision > 0),
    created_at         TEXT NOT NULL,
    modified_at        TEXT NOT NULL,
    trashed_at         TEXT,
    trash_parent       INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
    trash_name         BLOB,
    CHECK ((kind = 'file') = (current_version_id IS NOT NULL)),
    FOREIGN KEY (current_version_id) REFERENCES content_versions(version_id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE UNIQUE INDEX one_root ON nodes((1)) WHERE parent_id IS NULL;
CREATE UNIQUE INDEX live_sibling_names
    ON nodes(parent_id, name) WHERE trashed_at IS NULL;
CREATE INDEX nodes_parent ON nodes(parent_id);
CREATE INDEX nodes_trashed ON nodes(trashed_at) WHERE trashed_at IS NOT NULL;

CREATE TABLE content_versions (
    version_id              TEXT PRIMARY KEY,
    node_id                 INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    blob_hash               TEXT NOT NULL REFERENCES blobs(hash),
    size                    INTEGER NOT NULL CHECK (size >= 0),
    media_type              TEXT,
    recorded_at             TEXT NOT NULL,
    node_revision           INTEGER NOT NULL CHECK (node_revision > 0),
    introduced_operation_id TEXT NOT NULL,
    transition_kind         TEXT NOT NULL CHECK (transition_kind IN (
                                'content_create', 'content_replace', 'content_revert'
                            )),
    source_version_id       TEXT REFERENCES content_versions(version_id)
                                DEFERRABLE INITIALLY DEFERRED,
    CHECK ((transition_kind = 'content_revert') = (source_version_id IS NOT NULL))
);

CREATE INDEX content_versions_node ON content_versions(node_id);
CREATE INDEX content_versions_blob ON content_versions(blob_hash);
CREATE UNIQUE INDEX content_versions_node_revision
    ON content_versions(node_id, node_revision);
CREATE UNIQUE INDEX content_versions_node_operation
    ON content_versions(node_id, introduced_operation_id);

CREATE TABLE ingests (
    ingest_id   TEXT PRIMARY KEY,
    started_at  TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_desc BLOB NOT NULL
);

CREATE TABLE provenance (
    identity       TEXT PRIMARY KEY,
    node_id        INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ingest_id      TEXT NOT NULL REFERENCES ingests(ingest_id),
    original_path  BLOB,
    original_mtime TEXT,
    supersedes     TEXT REFERENCES provenance(identity)
                       DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX provenance_node ON provenance(node_id);
CREATE INDEX provenance_ingest ON provenance(ingest_id);
CREATE UNIQUE INDEX provenance_one_successor
    ON provenance(supersedes) WHERE supersedes IS NOT NULL;

CREATE TABLE tags (
    tag_id TEXT PRIMARY KEY,
    name   TEXT NOT NULL UNIQUE
);

CREATE TABLE node_tags (
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    tag_id  TEXT NOT NULL REFERENCES tags(tag_id),
    PRIMARY KEY (node_id, tag_id)
);

CREATE TABLE extracted_text (
    blob_hash         TEXT NOT NULL REFERENCES blobs(hash),
    extractor         TEXT NOT NULL,
    extractor_version INTEGER NOT NULL CHECK (extractor_version >= 0),
    status            TEXT NOT NULL CHECK (status IN ('ok', 'failed')),
    error             TEXT,
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    text              TEXT,
    extracted_at      TEXT NOT NULL,
    PRIMARY KEY (blob_hash, extractor)
);
