-- Test-only constructor for the exact physical schema-v2 relations shipped in
-- v0.10.0. It is applied to the complete v0.9.0 fixture before any rows are
-- inserted; logical relations unchanged between those releases remain in the
-- base fixture.
DROP TABLE vault_metadata;
CREATE TABLE vault_metadata (
    singleton      INTEGER PRIMARY KEY CHECK (singleton = 1),
    vault_uid      TEXT NOT NULL UNIQUE,
    schema_version INTEGER NOT NULL CHECK (schema_version >= 1)
);

DROP TABLE blob_pack_index;
DROP TABLE blob_packs;
DROP TABLE blobs;

CREATE TABLE blobs (
    hash              TEXT PRIMARY KEY,
    size              INTEGER NOT NULL,
    created_at        TEXT NOT NULL,
    loose_encoding    TEXT CHECK (loose_encoding IN ('raw', 'zstd')),
    loose_stored_size INTEGER CHECK (loose_stored_size >= 0),
    pack_eligible     INTEGER NOT NULL DEFAULT 1 CHECK (pack_eligible IN (0, 1)),
    CHECK ((loose_encoding IS NULL) = (loose_stored_size IS NULL))
);

CREATE TABLE blob_packs (
    pack_id      TEXT PRIMARY KEY,
    entry_count  INTEGER NOT NULL CHECK (entry_count >= 0),
    stored_bytes INTEGER NOT NULL CHECK (stored_bytes >= 0),
    created_at   TEXT NOT NULL,
    scan_hash             TEXT NOT NULL DEFAULT '',
    live_entries          INTEGER NOT NULL DEFAULT 0 CHECK (live_entries >= 0),
    live_stored_bytes     INTEGER NOT NULL DEFAULT 0 CHECK (live_stored_bytes >= 0),
    live_raw_bytes        INTEGER NOT NULL DEFAULT 0 CHECK (live_raw_bytes >= 0),
    max_live_stored_len   INTEGER NOT NULL DEFAULT 0 CHECK (max_live_stored_len >= 0),
    max_live_raw_len      INTEGER NOT NULL DEFAULT 0 CHECK (max_live_raw_len >= 0)
);

CREATE TABLE blob_pack_index (
    blob_hash   TEXT PRIMARY KEY,
    pack_id     TEXT NOT NULL REFERENCES blob_packs(pack_id) ON DELETE CASCADE,
    pack_offset INTEGER NOT NULL CHECK (pack_offset >= 0),
    stored_len  INTEGER NOT NULL CHECK (stored_len >= 0),
    raw_len     INTEGER NOT NULL CHECK (raw_len >= 0),
    flags       INTEGER NOT NULL CHECK (flags BETWEEN 0 AND 255),
    crc32c      INTEGER NOT NULL CHECK (crc32c BETWEEN 0 AND 4294967295)
);

CREATE INDEX blob_pack_index_pack ON blob_pack_index(pack_id);

CREATE TRIGGER blob_pack_summary_mapping_insert
AFTER INSERT ON blob_pack_index
WHEN EXISTS (SELECT 1 FROM blobs WHERE hash=NEW.blob_hash)
BEGIN
    UPDATE blob_packs SET
        scan_hash=CASE WHEN scan_hash='' THEN NEW.blob_hash ELSE scan_hash END,
        live_entries=live_entries+1,
        live_stored_bytes=live_stored_bytes+NEW.stored_len,
        live_raw_bytes=live_raw_bytes+NEW.raw_len,
        max_live_stored_len=MAX(max_live_stored_len, NEW.stored_len),
        max_live_raw_len=MAX(max_live_raw_len, NEW.raw_len)
    WHERE pack_id=NEW.pack_id;
END;

CREATE TRIGGER blob_pack_summary_mapping_delete
AFTER DELETE ON blob_pack_index
WHEN EXISTS (SELECT 1 FROM blobs WHERE hash=OLD.blob_hash)
BEGIN
    UPDATE blob_packs SET
        live_entries=live_entries-1,
        live_stored_bytes=live_stored_bytes-OLD.stored_len,
        live_raw_bytes=live_raw_bytes-OLD.raw_len,
        max_live_stored_len=CASE WHEN max_live_stored_len=OLD.stored_len
            THEN COALESCE((SELECT MAX(i.stored_len) FROM blob_pack_index i
                JOIN blobs b ON b.hash=i.blob_hash WHERE i.pack_id=OLD.pack_id),0)
            ELSE max_live_stored_len END,
        max_live_raw_len=CASE WHEN max_live_raw_len=OLD.raw_len
            THEN COALESCE((SELECT MAX(i.raw_len) FROM blob_pack_index i
                JOIN blobs b ON b.hash=i.blob_hash WHERE i.pack_id=OLD.pack_id),0)
            ELSE max_live_raw_len END
    WHERE pack_id=OLD.pack_id;
END;

CREATE TRIGGER blob_pack_summary_mapping_update
AFTER UPDATE ON blob_pack_index
WHEN EXISTS (SELECT 1 FROM blobs WHERE hash=OLD.blob_hash)
BEGIN
    UPDATE blob_packs SET
        live_entries=live_entries-1,
        live_stored_bytes=live_stored_bytes-OLD.stored_len,
        live_raw_bytes=live_raw_bytes-OLD.raw_len,
        max_live_stored_len=COALESCE((SELECT MAX(i.stored_len) FROM blob_pack_index i
            JOIN blobs b ON b.hash=i.blob_hash WHERE i.pack_id=OLD.pack_id),0),
        max_live_raw_len=COALESCE((SELECT MAX(i.raw_len) FROM blob_pack_index i
            JOIN blobs b ON b.hash=i.blob_hash WHERE i.pack_id=OLD.pack_id),0)
    WHERE pack_id=OLD.pack_id;
    UPDATE blob_packs SET
        scan_hash=CASE WHEN scan_hash='' THEN NEW.blob_hash ELSE scan_hash END,
        live_entries=live_entries+1,
        live_stored_bytes=live_stored_bytes+NEW.stored_len,
        live_raw_bytes=live_raw_bytes+NEW.raw_len,
        max_live_stored_len=MAX(max_live_stored_len, NEW.stored_len),
        max_live_raw_len=MAX(max_live_raw_len, NEW.raw_len)
    WHERE pack_id=NEW.pack_id;
END;

CREATE TRIGGER blob_pack_summary_blob_delete
AFTER DELETE ON blobs
WHEN EXISTS (SELECT 1 FROM blob_pack_index WHERE blob_hash=OLD.hash)
BEGIN
    UPDATE blob_packs SET
        live_entries=live_entries-1,
        live_stored_bytes=live_stored_bytes-(SELECT stored_len FROM blob_pack_index WHERE blob_hash=OLD.hash),
        live_raw_bytes=live_raw_bytes-(SELECT raw_len FROM blob_pack_index WHERE blob_hash=OLD.hash),
        max_live_stored_len=COALESCE((SELECT MAX(i.stored_len) FROM blob_pack_index i
            JOIN blobs b ON b.hash=i.blob_hash WHERE i.pack_id=blob_packs.pack_id),0),
        max_live_raw_len=COALESCE((SELECT MAX(i.raw_len) FROM blob_pack_index i
            JOIN blobs b ON b.hash=i.blob_hash WHERE i.pack_id=blob_packs.pack_id),0)
    WHERE pack_id=(SELECT pack_id FROM blob_pack_index WHERE blob_hash=OLD.hash);
END;

CREATE TRIGGER blob_pack_summary_blob_insert
AFTER INSERT ON blobs
WHEN EXISTS (SELECT 1 FROM blob_pack_index WHERE blob_hash=NEW.hash)
BEGIN
    UPDATE blob_packs SET
        live_entries=live_entries+1,
        live_stored_bytes=live_stored_bytes+(SELECT stored_len FROM blob_pack_index WHERE blob_hash=NEW.hash),
        live_raw_bytes=live_raw_bytes+(SELECT raw_len FROM blob_pack_index WHERE blob_hash=NEW.hash),
        max_live_stored_len=MAX(max_live_stored_len,
            (SELECT stored_len FROM blob_pack_index WHERE blob_hash=NEW.hash)),
        max_live_raw_len=MAX(max_live_raw_len,
            (SELECT raw_len FROM blob_pack_index WHERE blob_hash=NEW.hash))
    WHERE pack_id=(SELECT pack_id FROM blob_pack_index WHERE blob_hash=NEW.hash);
END;

CREATE INDEX blob_packs_dead_scan
ON blob_packs(scan_hash, pack_id) WHERE live_entries=0;
CREATE INDEX blob_packs_live_scan
ON blob_packs(scan_hash, pack_id) WHERE live_entries>0;

CREATE INDEX nodes_parent_name_id ON nodes(parent_id, name, id);
