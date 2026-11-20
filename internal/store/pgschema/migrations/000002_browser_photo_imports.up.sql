CREATE TABLE {{schema}}.browser_photo_imports (
    import_id TEXT COLLATE "C" PRIMARY KEY,
    vault_id TEXT COLLATE "C" NOT NULL,
    format_version BIGINT NOT NULL,
    source_id TEXT COLLATE "C" NOT NULL,
    label TEXT COLLATE "C" NOT NULL,
    destination TEXT COLLATE "C" NOT NULL,
    anchor_id BIGINT NOT NULL,
    suffix_json TEXT COLLATE "C" NOT NULL,
    skipped_json TEXT COLLATE "C" NOT NULL,
    destination_id BIGINT,
    created_at TEXT COLLATE "C" NOT NULL,
    UNIQUE (vault_id, source_id)
);

CREATE TABLE {{schema}}.browser_photo_import_groups (
    import_id TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.browser_photo_imports(import_id),
    ordinal BIGINT NOT NULL,
    members_json TEXT COLLATE "C" NOT NULL,
    hashes_json TEXT COLLATE "C",
    receipt_json TEXT COLLATE "C",
    PRIMARY KEY (import_id, ordinal)
);
