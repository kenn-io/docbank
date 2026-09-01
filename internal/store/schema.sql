-- docbank core schema. Idempotent: applied on every Open.

-- Processing authority is one atomic schema surface. Before any idempotent
-- processing DDL can repair state, compare the complete V1 structural identity
-- (tables, constraints, foreign keys, explicit index, and immutable triggers).
-- Exact set comparison is the fingerprint; it does not trust a writable marker.
CREATE TEMP TABLE IF NOT EXISTS docbank_processing_schema_v1_expected (
    object_type TEXT NOT NULL,
    object_name TEXT NOT NULL,
    object_sql  TEXT NOT NULL,
    PRIMARY KEY (object_type, object_name)
);
DELETE FROM docbank_processing_schema_v1_expected;
INSERT INTO docbank_processing_schema_v1_expected(object_type, object_name, object_sql) VALUES
('table', 'processing_incarnations', 'CREATE TABLE processing_incarnations (
    incarnation_id TEXT PRIMARY KEY,
    created_at     TEXT NOT NULL
)'),
('table', 'current_processing_incarnation', 'CREATE TABLE current_processing_incarnation (
    singleton      INTEGER PRIMARY KEY CHECK (singleton = 1),
    incarnation_id TEXT NOT NULL UNIQUE REFERENCES processing_incarnations(incarnation_id)
)'),
('trigger', 'current_processing_incarnation_immutable_update', 'CREATE TRIGGER current_processing_incarnation_immutable_update
BEFORE UPDATE ON current_processing_incarnation BEGIN
    SELECT RAISE(ABORT, ''current processing incarnation is immutable'');
END'),
('trigger', 'current_processing_incarnation_immutable_delete', 'CREATE TRIGGER current_processing_incarnation_immutable_delete
BEFORE DELETE ON current_processing_incarnation BEGIN
    SELECT RAISE(ABORT, ''current processing incarnation is immutable'');
END'),
('table', 'processing_consent_grants', 'CREATE TABLE processing_consent_grants (
    grant_id                  TEXT PRIMARY KEY,
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
)'),
('index', 'processing_consent_grants_authority', 'CREATE INDEX processing_consent_grants_authority
    ON processing_consent_grants(
        vault_uid, incarnation_id, principal, scope, profile_fingerprint,
        disclosure_fingerprint, input_classes_json, retained_classes_json,
        issued_at, grant_id
    )'),
('table', 'processing_consent_revocations', 'CREATE TABLE processing_consent_revocations (
    revocation_id  TEXT PRIMARY KEY,
    vault_uid      TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    incarnation_id TEXT NOT NULL REFERENCES processing_incarnations(incarnation_id),
    principal      TEXT NOT NULL,
    scope          TEXT NOT NULL,
    fence          INTEGER NOT NULL CHECK (fence > 0),
    revoked_at     TEXT NOT NULL,
    UNIQUE (vault_uid, incarnation_id, principal, scope, fence)
)'),
('index', 'processing_consent_revocations_scope', 'CREATE INDEX processing_consent_revocations_scope
    ON processing_consent_revocations(
        vault_uid, incarnation_id, principal, scope, fence
    )'),
('trigger', 'processing_incarnations_immutable_update', 'CREATE TRIGGER processing_incarnations_immutable_update
BEFORE UPDATE ON processing_incarnations BEGIN
    SELECT RAISE(ABORT, ''processing incarnation records are immutable'');
END'),
('trigger', 'processing_incarnations_immutable_delete', 'CREATE TRIGGER processing_incarnations_immutable_delete
BEFORE DELETE ON processing_incarnations BEGIN
    SELECT RAISE(ABORT, ''processing incarnation records are immutable'');
END'),
('trigger', 'processing_consent_grants_immutable_update', 'CREATE TRIGGER processing_consent_grants_immutable_update
BEFORE UPDATE ON processing_consent_grants BEGIN
    SELECT RAISE(ABORT, ''processing consent grant records are immutable'');
END'),
('trigger', 'processing_consent_grants_immutable_delete', 'CREATE TRIGGER processing_consent_grants_immutable_delete
BEFORE DELETE ON processing_consent_grants BEGIN
    SELECT RAISE(ABORT, ''processing consent grant records are immutable'');
END'),
('trigger', 'processing_consent_revocations_immutable_update', 'CREATE TRIGGER processing_consent_revocations_immutable_update
BEFORE UPDATE ON processing_consent_revocations BEGIN
    SELECT RAISE(ABORT, ''processing consent revocation records are immutable'');
END'),
('trigger', 'processing_consent_revocations_immutable_delete', 'CREATE TRIGGER processing_consent_revocations_immutable_delete
BEFORE DELETE ON processing_consent_revocations BEGIN
    SELECT RAISE(ABORT, ''processing consent revocation records are immutable'');
END'),
('table', 'processing_profiles', 'CREATE TABLE processing_profiles (
    profile_fingerprint               TEXT PRIMARY KEY,
    canonical_profile                 TEXT NOT NULL
        CHECK (length(CAST(canonical_profile AS BLOB)) BETWEEN 2 AND 1048576),
    rendition_request_fingerprint     TEXT NOT NULL,
    evidence_lexical_fingerprint      TEXT NOT NULL,
    retention_disclosure_fingerprint  TEXT NOT NULL,
    attachment_policy_fingerprint     TEXT NOT NULL,
    consent_fingerprint               TEXT NOT NULL,
    rendition_disclosure_fingerprint  TEXT NOT NULL,
    trust_boundary                    TEXT NOT NULL
        CHECK (length(CAST(trust_boundary AS BLOB)) BETWEEN 1 AND 1024)
)'),
('table', 'rendition_builds', 'CREATE TABLE rendition_builds (
    build_id                             TEXT PRIMARY KEY,
    vault_uid                            TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    source_sha256                        TEXT NOT NULL REFERENCES blobs(hash),
    rendition_request_fingerprint        TEXT NOT NULL,
    evidence_lexical_fingerprint         TEXT NOT NULL,
    captured_artifact_policy_fingerprint TEXT NOT NULL,
    captured_artifact_policy_json        TEXT NOT NULL
        CHECK (length(CAST(captured_artifact_policy_json AS BLOB)) BETWEEN 2 AND 65536),
    authorization_checksum               TEXT NOT NULL,
    provider_operation_id                TEXT NOT NULL
        CHECK (length(CAST(provider_operation_id AS BLOB)) BETWEEN 1 AND 4096),
    provider_receipt_json                 TEXT NOT NULL
        CHECK (length(CAST(provider_receipt_json AS BLOB)) BETWEEN 2 AND 1048576),
    evidence_checksum                    TEXT NOT NULL,
    rendition_checksum                   TEXT NOT NULL,
    markdown_checksum                    TEXT NOT NULL,
    completeness                         TEXT NOT NULL CHECK (completeness IN (
        ''complete'', ''partial'', ''degraded_provenance''
    )),
    partial_success                      INTEGER NOT NULL CHECK (partial_success IN (0, 1)),
    truncated                            INTEGER NOT NULL CHECK (truncated IN (0, 1)),
    warnings_json                        TEXT NOT NULL
        CHECK (length(CAST(warnings_json AS BLOB)) BETWEEN 2 AND 2097152),
    completed_at                         TEXT NOT NULL,
    declared_artifact_count              INTEGER NOT NULL
        CHECK (declared_artifact_count BETWEEN 0 AND 1024),
    unit_count                           INTEGER NOT NULL
        CHECK (unit_count BETWEEN 0 AND 100000),
    lexical_segment_count                INTEGER NOT NULL
        CHECK (lexical_segment_count BETWEEN 0 AND 1000000),
    UNIQUE (vault_uid, build_id),
    UNIQUE (
        vault_uid, source_sha256, rendition_request_fingerprint,
        evidence_lexical_fingerprint, captured_artifact_policy_fingerprint
    )
)'),
('index', 'rendition_builds_source', 'CREATE INDEX rendition_builds_source
    ON rendition_builds(source_sha256, build_id)'),
('table', 'rendition_artifacts', 'CREATE TABLE rendition_artifacts (
    build_id    TEXT NOT NULL REFERENCES rendition_builds(build_id),
    artifact_id TEXT NOT NULL
        CHECK (length(CAST(artifact_id AS BLOB)) BETWEEN 1 AND 1024),
    role        TEXT NOT NULL
        CHECK (length(CAST(role AS BLOB)) BETWEEN 1 AND 64),
    blob_hash   TEXT NOT NULL REFERENCES blobs(hash),
    size        INTEGER NOT NULL CHECK (size >= 0),
    checksum    TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state = ''verified''),
    PRIMARY KEY (build_id, artifact_id),
    UNIQUE (build_id, role, artifact_id)
)'),
('index', 'rendition_artifacts_blob', 'CREATE INDEX rendition_artifacts_blob
    ON rendition_artifacts(blob_hash, build_id)'),
('table', 'rendition_units', 'CREATE TABLE rendition_units (
    build_id          TEXT NOT NULL REFERENCES rendition_builds(build_id),
    unit_id           TEXT NOT NULL
        CHECK (length(CAST(unit_id AS BLOB)) BETWEEN 1 AND 1024),
    evidence_unit_id  TEXT NOT NULL
        CHECK (length(CAST(evidence_unit_id AS BLOB)) BETWEEN 1 AND 1024),
    unit_order        INTEGER NOT NULL CHECK (unit_order >= 0),
    checksum          TEXT NOT NULL,
    heading_path_json TEXT NOT NULL
        CHECK (length(CAST(heading_path_json AS BLOB)) BETWEEN 2 AND 1048576),
    locator_json      TEXT NOT NULL
        CHECK (length(CAST(locator_json AS BLOB)) BETWEEN 2 AND 8192),
    PRIMARY KEY (build_id, unit_id),
    UNIQUE (build_id, unit_order)
)'),
('table', 'rendition_lexical_segments', 'CREATE TABLE rendition_lexical_segments (
    build_id     TEXT NOT NULL,
    segment_id   TEXT NOT NULL
        CHECK (length(CAST(segment_id AS BLOB)) BETWEEN 1 AND 1024),
    unit_id      TEXT NOT NULL
        CHECK (length(CAST(unit_id AS BLOB)) BETWEEN 1 AND 1024),
    segment_order INTEGER NOT NULL CHECK (segment_order >= 0),
    char_start   INTEGER NOT NULL CHECK (char_start >= 0),
    char_end     INTEGER NOT NULL CHECK (char_end >= char_start),
    checksum     TEXT NOT NULL,
    text         TEXT NOT NULL
        CHECK (length(text) <= 1048576 AND length(CAST(text AS BLOB)) <= 4194304),
    PRIMARY KEY (build_id, segment_id),
    UNIQUE (build_id, segment_order),
    FOREIGN KEY (build_id, unit_id)
        REFERENCES rendition_units(build_id, unit_id)
)'),
('table', 'rendition_attachments', 'CREATE TABLE rendition_attachments (
    attachment_id                    TEXT PRIMARY KEY,
    vault_uid                        TEXT NOT NULL,
    content_version_id               TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
    build_id                          TEXT NOT NULL,
    profile_fingerprint               TEXT NOT NULL REFERENCES processing_profiles(profile_fingerprint),
    retention_disclosure_fingerprint  TEXT NOT NULL,
    attachment_policy_fingerprint     TEXT NOT NULL,
    consent_fingerprint               TEXT NOT NULL,
    rendition_disclosure_fingerprint  TEXT NOT NULL,
    trust_boundary                    TEXT NOT NULL
        CHECK (length(CAST(trust_boundary AS BLOB)) BETWEEN 1 AND 1024),
    attached_at                       TEXT NOT NULL,
    FOREIGN KEY (vault_uid, build_id)
        REFERENCES rendition_builds(vault_uid, build_id),
    UNIQUE (content_version_id, profile_fingerprint, attachment_id),
    UNIQUE (content_version_id, profile_fingerprint, build_id)
)'),
('table', 'rendition_heads', 'CREATE TABLE rendition_heads (
    content_version_id          TEXT NOT NULL,
    profile_fingerprint         TEXT NOT NULL,
    attachment_id               TEXT NOT NULL,
    published_at                TEXT NOT NULL,
    PRIMARY KEY (content_version_id, profile_fingerprint),
    FOREIGN KEY (content_version_id, profile_fingerprint, attachment_id)
        REFERENCES rendition_attachments(
            content_version_id, profile_fingerprint, attachment_id
        ) ON DELETE CASCADE
)'),
('trigger', 'processing_profiles_immutable_update', 'CREATE TRIGGER processing_profiles_immutable_update
BEFORE UPDATE ON processing_profiles BEGIN
    SELECT RAISE(ABORT, ''processing profile records are immutable'');
END'),
('trigger', 'rendition_builds_immutable_update', 'CREATE TRIGGER rendition_builds_immutable_update
BEFORE UPDATE ON rendition_builds BEGIN
    SELECT RAISE(ABORT, ''rendition build records are immutable'');
END'),
('trigger', 'rendition_artifacts_immutable_update', 'CREATE TRIGGER rendition_artifacts_immutable_update
BEFORE UPDATE ON rendition_artifacts BEGIN
    SELECT RAISE(ABORT, ''rendition artifact records are immutable'');
END'),
('trigger', 'rendition_units_immutable_update', 'CREATE TRIGGER rendition_units_immutable_update
BEFORE UPDATE ON rendition_units BEGIN
    SELECT RAISE(ABORT, ''rendition unit records are immutable'');
END'),
('trigger', 'rendition_lexical_segments_immutable_update', 'CREATE TRIGGER rendition_lexical_segments_immutable_update
BEFORE UPDATE ON rendition_lexical_segments BEGIN
    SELECT RAISE(ABORT, ''rendition lexical segment records are immutable'');
END'),
('trigger', 'rendition_attachments_immutable_update', 'CREATE TRIGGER rendition_attachments_immutable_update
BEFORE UPDATE ON rendition_attachments BEGIN
    SELECT RAISE(ABORT, ''rendition attachment records are immutable'');
END'),
('table', 'current_rendition_roots', 'CREATE TABLE current_rendition_roots (
    root_id       TEXT PRIMARY KEY,
    root_kind     TEXT NOT NULL CHECK (root_kind IN (
        ''attachment'', ''head'', ''retention'', ''audit'', ''job'',
        ''reader_lease'', ''worker_lease'', ''backup_pin''
    )),
    target_kind   TEXT NOT NULL CHECK (target_kind IN (
        ''rendition_build'', ''lexical_generation''
    )),
    target_id     TEXT NOT NULL,
    fencing_token INTEGER NOT NULL CHECK (fencing_token > 0),
    recorded_at   TEXT NOT NULL,
    expires_at    TEXT,
    active        INTEGER NOT NULL CHECK (active IN (0, 1)),
    released_at   TEXT,
    CHECK ((root_kind IN (''reader_lease'', ''worker_lease'')) = (expires_at IS NOT NULL)),
    CHECK ((active = 1) = (released_at IS NULL))
)'),
('index', 'current_rendition_roots_target', 'CREATE INDEX current_rendition_roots_target
    ON current_rendition_roots(target_kind, target_id, root_kind)'),
('index', 'current_rendition_roots_expiry', 'CREATE INDEX current_rendition_roots_expiry
    ON current_rendition_roots(expires_at)
    WHERE expires_at IS NOT NULL'),
('table', 'derivative_purge_suppressions', 'CREATE TABLE derivative_purge_suppressions (
    source_sha256        TEXT NOT NULL,
    profile_fingerprint  TEXT NOT NULL,
    build_id             TEXT NOT NULL,
    purged_at            TEXT NOT NULL,
    active               INTEGER NOT NULL CHECK (active IN (0, 1)),
    superseded_at        TEXT,
    superseding_build_id TEXT,
    PRIMARY KEY (source_sha256, profile_fingerprint, build_id),
    CHECK ((active = 1) = (superseded_at IS NULL AND superseding_build_id IS NULL))
)'),
('index', 'derivative_purge_suppressions_active_source', 'CREATE INDEX derivative_purge_suppressions_active_source
    ON derivative_purge_suppressions(source_sha256, profile_fingerprint)
    WHERE active = 1'),
('table', 'rendition_blob_staging', 'CREATE TABLE rendition_blob_staging (
    blob_hash TEXT PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE
)'),
('table', 'derivative_blob_purge_pending', 'CREATE TABLE derivative_blob_purge_pending (
    blob_hash TEXT PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE
)'),
('table', 'derivative_pack_purge_pending', 'CREATE TABLE derivative_pack_purge_pending (
    store_id TEXT NOT NULL,
    pack_id  TEXT NOT NULL,
    PRIMARY KEY (store_id, pack_id),
    FOREIGN KEY (store_id, pack_id) REFERENCES blob_packs(store_id, pack_id) ON DELETE CASCADE
)'),
('trigger', 'content_versions_promote_rendition_staging', 'CREATE TRIGGER content_versions_promote_rendition_staging
AFTER INSERT ON content_versions BEGIN
    DELETE FROM rendition_blob_staging WHERE blob_hash=NEW.blob_hash;
END'),
('trigger', 'content_versions_revoke_derivative_purge', 'CREATE TRIGGER content_versions_revoke_derivative_purge
AFTER INSERT ON content_versions BEGIN
    DELETE FROM derivative_blob_purge_pending WHERE blob_hash=NEW.blob_hash;
END');

CREATE TEMP TABLE IF NOT EXISTS docbank_processing_schema_preflight (
    identity_matches INTEGER NOT NULL,
    CONSTRAINT processing_metadata_schema_identity CHECK (identity_matches = 1)
);
DELETE FROM docbank_processing_schema_preflight;
INSERT INTO docbank_processing_schema_preflight(identity_matches)
SELECT CASE
    WHEN NOT EXISTS (
        SELECT 1 FROM sqlite_schema
        WHERE name IN (SELECT object_name FROM docbank_processing_schema_v1_expected)
           OR (type IN ('index', 'trigger') AND sql IS NOT NULL AND tbl_name IN (
            'processing_incarnations', 'current_processing_incarnation',
            'processing_consent_grants', 'processing_consent_revocations',
            'processing_profiles', 'rendition_builds', 'rendition_artifacts',
            'rendition_units', 'rendition_lexical_segments',
            'rendition_attachments', 'rendition_heads',
            'current_rendition_roots', 'derivative_purge_suppressions',
            'rendition_blob_staging', 'derivative_blob_purge_pending',
            'derivative_pack_purge_pending'
        ))
    ) THEN 1
    WHEN NOT EXISTS (
        SELECT object_type, object_name, object_sql
        FROM docbank_processing_schema_v1_expected
        EXCEPT
        SELECT type, name, sql FROM sqlite_schema
        WHERE name IN (SELECT object_name FROM docbank_processing_schema_v1_expected)
           OR (type IN ('index', 'trigger') AND sql IS NOT NULL AND tbl_name IN (
            'processing_incarnations', 'current_processing_incarnation',
            'processing_consent_grants', 'processing_consent_revocations',
            'processing_profiles', 'rendition_builds', 'rendition_artifacts',
            'rendition_units', 'rendition_lexical_segments',
            'rendition_attachments', 'rendition_heads',
            'current_rendition_roots', 'derivative_purge_suppressions',
            'rendition_blob_staging', 'derivative_blob_purge_pending',
            'derivative_pack_purge_pending'
        ))
    ) AND NOT EXISTS (
        SELECT type, name, sql FROM sqlite_schema
        WHERE name IN (SELECT object_name FROM docbank_processing_schema_v1_expected)
           OR (type IN ('index', 'trigger') AND sql IS NOT NULL AND tbl_name IN (
            'processing_incarnations', 'current_processing_incarnation',
            'processing_consent_grants', 'processing_consent_revocations',
            'processing_profiles', 'rendition_builds', 'rendition_artifacts',
            'rendition_units', 'rendition_lexical_segments',
            'rendition_attachments', 'rendition_heads',
            'current_rendition_roots', 'derivative_purge_suppressions',
            'rendition_blob_staging', 'derivative_blob_purge_pending',
            'derivative_pack_purge_pending'
        ))
        EXCEPT
        SELECT object_type, object_name, object_sql
        FROM docbank_processing_schema_v1_expected
    ) THEN 1
    ELSE 0
END;
DROP TABLE docbank_processing_schema_preflight;
DROP TABLE docbank_processing_schema_v1_expected;

-- The lexical projection is created lazily, but once any part exists it must
-- be the complete current layout. This rejects partial unshipped schema-v4
-- states instead of silently layering new columns or membership tables onto
-- an older projection.
CREATE TEMP TABLE IF NOT EXISTS docbank_lexical_schema_expected (
    object_type TEXT NOT NULL,
    object_name TEXT NOT NULL,
    object_sql  TEXT NOT NULL,
    PRIMARY KEY (object_type, object_name)
);
DELETE FROM docbank_lexical_schema_expected;
INSERT INTO docbank_lexical_schema_expected(object_type, object_name, object_sql) VALUES
('table', 'rendition_lexical_generations', 'CREATE TABLE rendition_lexical_generations (
    generation_id TEXT PRIMARY KEY,
    segment_count INTEGER NOT NULL CHECK (segment_count >= 0),
    build_count   INTEGER NOT NULL CHECK (build_count >= 0),
    built_at      TEXT NOT NULL
)'),
('table', 'rendition_lexical_generation_manifests', 'CREATE TABLE rendition_lexical_generation_manifests (
    generation_id  TEXT PRIMARY KEY REFERENCES rendition_lexical_generations(generation_id),
    manifest_digest TEXT NOT NULL CHECK (length(manifest_digest) = 64),
    build_digest    TEXT NOT NULL CHECK (length(build_digest) = 64)
)'),
('table', 'rendition_lexical_generation_builds', 'CREATE TABLE rendition_lexical_generation_builds (
    generation_id TEXT NOT NULL REFERENCES rendition_lexical_generations(generation_id)
        ON DELETE CASCADE,
    build_id      TEXT NOT NULL REFERENCES rendition_builds(build_id),
    PRIMARY KEY (generation_id, build_id)
)'),
('table', 'rendition_lexical_fts', 'CREATE VIRTUAL TABLE rendition_lexical_fts USING fts5(
    generation_id UNINDEXED,
    build_id      UNINDEXED,
    segment_id    UNINDEXED,
    text
)'),
('table', 'rendition_lexical_heads', 'CREATE TABLE rendition_lexical_heads (
    singleton     INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation_id TEXT NOT NULL REFERENCES rendition_lexical_generations(generation_id)
)');

CREATE TEMP TABLE IF NOT EXISTS docbank_lexical_schema_preflight (
    identity_matches INTEGER NOT NULL,
    CONSTRAINT lexical_projection_schema_identity CHECK (identity_matches = 1)
);
DELETE FROM docbank_lexical_schema_preflight;
INSERT INTO docbank_lexical_schema_preflight(identity_matches)
SELECT CASE
    WHEN NOT EXISTS (
        SELECT 1 FROM sqlite_schema
        WHERE name IN (SELECT object_name FROM docbank_lexical_schema_expected)
    ) THEN 1
    WHEN NOT EXISTS (
        SELECT object_type, object_name, object_sql
        FROM docbank_lexical_schema_expected
        EXCEPT
        SELECT type, name, sql FROM sqlite_schema
        WHERE name IN (SELECT object_name FROM docbank_lexical_schema_expected)
    ) AND NOT EXISTS (
        SELECT type, name, sql FROM sqlite_schema
        WHERE name IN (SELECT object_name FROM docbank_lexical_schema_expected)
        EXCEPT
        SELECT object_type, object_name, object_sql
        FROM docbank_lexical_schema_expected
    ) THEN 1
    ELSE 0
END;
DROP TABLE docbank_lexical_schema_preflight;
DROP TABLE docbank_lexical_schema_expected;

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
CREATE INDEX IF NOT EXISTS nodes_trashed ON nodes(trashed_at) WHERE trashed_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS blobs (
    hash       TEXT PRIMARY KEY,
    size       INTEGER NOT NULL CHECK (size >= 0),
    created_at TEXT NOT NULL
);

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
    loose_encoding INTEGER NOT NULL CHECK (loose_encoding IN (1, 2)),
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
    state                 TEXT NOT NULL CHECK (state IN ('ready', 'unsupported', 'failed')),
    output_blob_hash      TEXT REFERENCES blobs(hash),
    output_size           INTEGER,
    output_media_type     TEXT,
    output_width          INTEGER,
    output_height         INTEGER,
    failure_code          TEXT,
    failure_detail        TEXT,
    created_at            TEXT NOT NULL,
    UNIQUE (content_version_id, recipe_fingerprint),
    UNIQUE (content_version_id, generation_id),
    CHECK (
        (state = 'ready'
         AND output_blob_hash IS NOT NULL
         AND output_size > 0
         AND output_media_type IS NOT NULL
         AND output_width > 0
         AND output_height > 0
         AND failure_code IS NULL
         AND failure_detail IS NULL)
        OR
        (state IN ('unsupported', 'failed')
         AND output_blob_hash IS NULL
         AND output_size IS NULL
         AND output_media_type IS NULL
         AND output_width IS NULL
         AND output_height IS NULL
         AND failure_code IS NOT NULL
         AND failure_detail IS NOT NULL)
    )
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
    canonical_profile                 TEXT NOT NULL
        CHECK (length(CAST(canonical_profile AS BLOB)) BETWEEN 2 AND 1048576),
    rendition_request_fingerprint     TEXT NOT NULL,
    evidence_lexical_fingerprint      TEXT NOT NULL,
    retention_disclosure_fingerprint  TEXT NOT NULL,
    attachment_policy_fingerprint     TEXT NOT NULL,
    consent_fingerprint               TEXT NOT NULL,
    rendition_disclosure_fingerprint  TEXT NOT NULL,
    trust_boundary                    TEXT NOT NULL
        CHECK (length(CAST(trust_boundary AS BLOB)) BETWEEN 1 AND 1024)
);

-- A completed rendition build is vault-local immutable authority. The unique
-- identity contains exactly the source, rendition request, evidence/lexical
-- policy, and captured-artifact policy; attachments carry the full profile.
CREATE TABLE IF NOT EXISTS rendition_builds (
    build_id                             TEXT PRIMARY KEY,
    vault_uid                            TEXT NOT NULL REFERENCES vault_metadata(vault_uid),
    source_sha256                        TEXT NOT NULL REFERENCES blobs(hash),
    rendition_request_fingerprint        TEXT NOT NULL,
    evidence_lexical_fingerprint         TEXT NOT NULL,
    captured_artifact_policy_fingerprint TEXT NOT NULL,
    captured_artifact_policy_json        TEXT NOT NULL
        CHECK (length(CAST(captured_artifact_policy_json AS BLOB)) BETWEEN 2 AND 65536),
    authorization_checksum               TEXT NOT NULL,
    provider_operation_id                TEXT NOT NULL
        CHECK (length(CAST(provider_operation_id AS BLOB)) BETWEEN 1 AND 4096),
    provider_receipt_json                 TEXT NOT NULL
        CHECK (length(CAST(provider_receipt_json AS BLOB)) BETWEEN 2 AND 1048576),
    evidence_checksum                    TEXT NOT NULL,
    rendition_checksum                   TEXT NOT NULL,
    markdown_checksum                    TEXT NOT NULL,
    completeness                         TEXT NOT NULL CHECK (completeness IN (
        'complete', 'partial', 'degraded_provenance'
    )),
    partial_success                      INTEGER NOT NULL CHECK (partial_success IN (0, 1)),
    truncated                            INTEGER NOT NULL CHECK (truncated IN (0, 1)),
    warnings_json                        TEXT NOT NULL
        CHECK (length(CAST(warnings_json AS BLOB)) BETWEEN 2 AND 2097152),
    completed_at                         TEXT NOT NULL,
    declared_artifact_count              INTEGER NOT NULL
        CHECK (declared_artifact_count BETWEEN 0 AND 1024),
    unit_count                           INTEGER NOT NULL
        CHECK (unit_count BETWEEN 0 AND 100000),
    lexical_segment_count                INTEGER NOT NULL
        CHECK (lexical_segment_count BETWEEN 0 AND 1000000),
    UNIQUE (vault_uid, build_id),
    UNIQUE (
        vault_uid, source_sha256, rendition_request_fingerprint,
        evidence_lexical_fingerprint, captured_artifact_policy_fingerprint
    )
);

CREATE INDEX IF NOT EXISTS rendition_builds_source
    ON rendition_builds(source_sha256, build_id);

CREATE TABLE IF NOT EXISTS rendition_artifacts (
    build_id    TEXT NOT NULL REFERENCES rendition_builds(build_id),
    artifact_id TEXT NOT NULL
        CHECK (length(CAST(artifact_id AS BLOB)) BETWEEN 1 AND 1024),
    role        TEXT NOT NULL
        CHECK (length(CAST(role AS BLOB)) BETWEEN 1 AND 64),
    blob_hash   TEXT NOT NULL REFERENCES blobs(hash),
    size        INTEGER NOT NULL CHECK (size >= 0),
    checksum    TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state = 'verified'),
    PRIMARY KEY (build_id, artifact_id),
    UNIQUE (build_id, role, artifact_id)
);

CREATE INDEX IF NOT EXISTS rendition_artifacts_blob
    ON rendition_artifacts(blob_hash, build_id);

CREATE TABLE IF NOT EXISTS rendition_units (
    build_id          TEXT NOT NULL REFERENCES rendition_builds(build_id),
    unit_id           TEXT NOT NULL
        CHECK (length(CAST(unit_id AS BLOB)) BETWEEN 1 AND 1024),
    evidence_unit_id  TEXT NOT NULL
        CHECK (length(CAST(evidence_unit_id AS BLOB)) BETWEEN 1 AND 1024),
    unit_order        INTEGER NOT NULL CHECK (unit_order >= 0),
    checksum          TEXT NOT NULL,
    heading_path_json TEXT NOT NULL
        CHECK (length(CAST(heading_path_json AS BLOB)) BETWEEN 2 AND 1048576),
    locator_json      TEXT NOT NULL
        CHECK (length(CAST(locator_json AS BLOB)) BETWEEN 2 AND 8192),
    PRIMARY KEY (build_id, unit_id),
    UNIQUE (build_id, unit_order)
);

CREATE TABLE IF NOT EXISTS rendition_lexical_segments (
    build_id     TEXT NOT NULL,
    segment_id   TEXT NOT NULL
        CHECK (length(CAST(segment_id AS BLOB)) BETWEEN 1 AND 1024),
    unit_id      TEXT NOT NULL
        CHECK (length(CAST(unit_id AS BLOB)) BETWEEN 1 AND 1024),
    segment_order INTEGER NOT NULL CHECK (segment_order >= 0),
    char_start   INTEGER NOT NULL CHECK (char_start >= 0),
    char_end     INTEGER NOT NULL CHECK (char_end >= char_start),
    checksum     TEXT NOT NULL,
    text         TEXT NOT NULL
        CHECK (length(text) <= 1048576 AND length(CAST(text AS BLOB)) <= 4194304),
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
    trust_boundary                    TEXT NOT NULL
        CHECK (length(CAST(trust_boundary AS BLOB)) BETWEEN 1 AND 1024),
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

-- Root producers outside the immutable rendition catalog retain one exact
-- build or lexical generation. Lease expiry uses canonical fixed-width UTC
-- timestamps; monotonic fencing prevents a stale worker from releasing a
-- renewed root. Attachments and heads remain normalized in their own tables.
CREATE TABLE IF NOT EXISTS current_rendition_roots (
    root_id       TEXT PRIMARY KEY,
    root_kind     TEXT NOT NULL CHECK (root_kind IN (
        'attachment', 'head', 'retention', 'audit', 'job',
        'reader_lease', 'worker_lease', 'backup_pin'
    )),
    target_kind   TEXT NOT NULL CHECK (target_kind IN (
        'rendition_build', 'lexical_generation'
    )),
    target_id     TEXT NOT NULL,
    fencing_token INTEGER NOT NULL CHECK (fencing_token > 0),
    recorded_at   TEXT NOT NULL,
    expires_at    TEXT,
    active        INTEGER NOT NULL CHECK (active IN (0, 1)),
    released_at   TEXT,
    CHECK ((root_kind IN ('reader_lease', 'worker_lease')) = (expires_at IS NOT NULL)),
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
