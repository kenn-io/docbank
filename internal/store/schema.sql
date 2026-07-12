-- docbank core schema. Idempotent: applied on every Open.

-- AUTOINCREMENT: node ids are stored as origins (trash_parent) and will be
-- handed to agents over the HTTP API; a reused rowid would silently retarget
-- those references at an unrelated node.
CREATE TABLE IF NOT EXISTS nodes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id     INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('dir', 'file')),
    blob_hash     TEXT REFERENCES blobs(hash),
    size          INTEGER,
    mime_type     TEXT,
    revision      INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL,
    modified_at   TEXT NOT NULL,
    trashed_at    TEXT,
    trash_parent  INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
    trash_name    TEXT,
    CHECK ((kind = 'file') = (blob_hash IS NOT NULL))
);

-- Exactly one root. SQLite treats NULLs as distinct in unique indexes, so
-- uniqueness of the NULL parent needs a constant-expression partial index.
CREATE UNIQUE INDEX IF NOT EXISTS one_root ON nodes((1)) WHERE parent_id IS NULL;

-- Sibling names are unique among LIVE nodes only; trashed nodes never block
-- reuse of a name.
CREATE UNIQUE INDEX IF NOT EXISTS live_sibling_names
    ON nodes(parent_id, name) WHERE trashed_at IS NULL;

CREATE INDEX IF NOT EXISTS nodes_parent ON nodes(parent_id);
CREATE INDEX IF NOT EXISTS nodes_blob ON nodes(blob_hash);
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

CREATE TABLE IF NOT EXISTS node_versions (
    node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    blob_hash   TEXT NOT NULL REFERENCES blobs(hash),
    size        INTEGER NOT NULL,
    replaced_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS node_versions_node ON node_versions(node_id);
CREATE INDEX IF NOT EXISTS node_versions_blob ON node_versions(blob_hash);

CREATE TABLE IF NOT EXISTS ingests (
    id          INTEGER PRIMARY KEY,
    started_at  TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_desc TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS provenance (
    node_id       INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ingest_id     INTEGER NOT NULL REFERENCES ingests(id),
    original_path TEXT NOT NULL,
    original_mtime TEXT
);

CREATE INDEX IF NOT EXISTS provenance_node ON provenance(node_id);

CREATE TABLE IF NOT EXISTS tags (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS node_tags (
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    tag_id  INTEGER NOT NULL REFERENCES tags(id),
    PRIMARY KEY (node_id, tag_id)
);

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
