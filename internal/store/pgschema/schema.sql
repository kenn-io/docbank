-- The PostgreSQL identity follows the same logical vault through export and
-- restore.
CREATE TABLE {{schema}}.vault_metadata (
    singleton      SMALLINT PRIMARY KEY,
    vault_uid      UUID NOT NULL UNIQUE
);

-- Allocation is transactional: audit replay requires consecutive committed IDs.
-- Keep the high-water mark even when the highest node is deleted.
CREATE TABLE {{schema}}.node_allocation (
    singleton SMALLINT PRIMARY KEY,
    high_water BIGINT NOT NULL
);
-- Text uses SQLite byte order, independent of the database locale.
CREATE TABLE {{schema}}.nodes (
    id                 BIGINT PRIMARY KEY,
    parent_id          BIGINT REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE,
    name               TEXT COLLATE "C" NOT NULL,
    kind               TEXT COLLATE "C" NOT NULL,
    current_version_id TEXT COLLATE "C",
    revision           BIGINT NOT NULL DEFAULT 1,
    created_at         TEXT COLLATE "C" NOT NULL,
    modified_at        TEXT COLLATE "C" NOT NULL,
    trashed_at         TEXT COLLATE "C",
    trash_parent       BIGINT REFERENCES {{schema}}.nodes(id) ON DELETE SET NULL,
    trash_name         TEXT COLLATE "C"
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
    hash       TEXT COLLATE "C" PRIMARY KEY,
    size       BIGINT NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL
);

-- SHA-256 owns content identity; MD5 is auxiliary interoperability metadata.
CREATE TABLE {{schema}}.blob_checksums (
    blob_sha256 TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.blobs(hash) ON DELETE CASCADE,
    md5         TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.content_versions (
    version_id              TEXT COLLATE "C" PRIMARY KEY,
    node_id                 BIGINT NOT NULL REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE,
    blob_hash               TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.blobs(hash),
    size                    BIGINT NOT NULL,
    mime_type               TEXT COLLATE "C",
    recorded_at             TEXT COLLATE "C" NOT NULL,
    node_revision           BIGINT NOT NULL,
    introduced_operation_id TEXT COLLATE "C" NOT NULL,
    transition_kind         TEXT COLLATE "C" NOT NULL,
    source_version_id       TEXT COLLATE "C" REFERENCES {{schema}}.content_versions(version_id)
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
    digest             TEXT COLLATE "C" PRIMARY KEY,
    kind               TEXT COLLATE "C" NOT NULL,
    operation_id       TEXT COLLATE "C",
    operation_sequence BIGINT,
    scope_id           TEXT COLLATE "C",
    entry_count        BIGINT,
    event_id           TEXT COLLATE "C",
    event_ordinal      BIGINT,
    node_id            BIGINT,
    record_json        TEXT COLLATE "C" NOT NULL
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
    lineage_id                    TEXT COLLATE "C" NOT NULL UNIQUE,
    operation_sequence_high_water BIGINT NOT NULL,
    allocation_genesis_digest     TEXT COLLATE "C" NOT NULL UNIQUE REFERENCES {{schema}}.audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED,
    allocation_entry_count        BIGINT NOT NULL,
    allocation_head               TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE {{schema}}.audit_scopes (
    scope_id            TEXT COLLATE "C" PRIMARY KEY,
    target_node_id      BIGINT NOT NULL REFERENCES {{schema}}.nodes(id),
    enable_operation_id TEXT COLLATE "C" NOT NULL UNIQUE,
    entry_count         BIGINT NOT NULL,
    chain_head          TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE {{schema}}.audit_baselines (
    digest         TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.audit_records(digest)
        DEFERRABLE INITIALLY DEFERRED,
    scope_id       TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.audit_scopes(scope_id)
        DEFERRABLE INITIALLY DEFERRED,
    target_node_id BIGINT NOT NULL REFERENCES {{schema}}.nodes(id),
    operation_id   TEXT COLLATE "C" NOT NULL,
    UNIQUE (scope_id, target_node_id, operation_id)
);
CREATE TABLE {{schema}}.audit_memberships (
    scope_id        TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.audit_scopes(scope_id)
        DEFERRABLE INITIALLY DEFERRED,
    node_id         BIGINT NOT NULL REFERENCES {{schema}}.nodes(id),
    baseline_digest TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.audit_baselines(digest)
        DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (scope_id, node_id)
);
CREATE INDEX audit_membership_node ON {{schema}}.audit_memberships(node_id);

-- Audit baselines and photo re-export decisions share these catalogs. Mutation
-- and publication remain in Go; these tables do not enable a complete photo store.

CREATE TABLE {{schema}}.ingests (
    id          TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    started_at  TEXT COLLATE "C" NOT NULL,
    source_kind TEXT COLLATE "C" NOT NULL,
    source_desc TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.provenance (
    identity       TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    node_id        BIGINT NOT NULL REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE,
    ingest_id      TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.ingests(id),
    original_path  TEXT COLLATE "C" NOT NULL,
    original_mtime TEXT COLLATE "C",
    supersedes     TEXT COLLATE "C" REFERENCES {{schema}}.provenance(identity)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE {{schema}}.provenance_version_bindings (
    provenance_identity TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.provenance(identity) ON DELETE CASCADE,
    content_version_id  TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE,
    observed_at         TEXT COLLATE "C" NOT NULL,
    basis_ref           TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (provenance_identity, content_version_id)
);
CREATE INDEX provenance_version_bindings_version
    ON {{schema}}.provenance_version_bindings(content_version_id);

CREATE TABLE {{schema}}.derivative_purge_suppressions (
    source_sha256        TEXT COLLATE "C" NOT NULL,
    profile_fingerprint  TEXT COLLATE "C" NOT NULL,
    build_id             TEXT COLLATE "C" NOT NULL,
    purged_at            TEXT COLLATE "C" NOT NULL,
    active               BIGINT NOT NULL,
    superseded_at        TEXT COLLATE "C",
    superseding_build_id TEXT COLLATE "C",
    PRIMARY KEY (source_sha256, profile_fingerprint, build_id)
);

CREATE TABLE {{schema}}.source_metadata_generations (
    generation_id        TEXT COLLATE "C" PRIMARY KEY,
    source_sha256        TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.blobs(hash) ON DELETE CASCADE,
    contract_version     TEXT COLLATE "C" NOT NULL,
    extractor_fingerprint TEXT COLLATE "C" NOT NULL,
    canonical_json       BYTEA NOT NULL,
    checksum             TEXT COLLATE "C" NOT NULL,
    created_at           TEXT COLLATE "C" NOT NULL,
    UNIQUE (source_sha256, contract_version, extractor_fingerprint),
    UNIQUE (source_sha256, generation_id)
);

CREATE TABLE {{schema}}.source_metadata_heads (
    source_sha256 TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.blobs(hash) ON DELETE CASCADE,
    generation_id TEXT COLLATE "C" NOT NULL UNIQUE
        REFERENCES {{schema}}.source_metadata_generations(generation_id) ON DELETE CASCADE,
    published_at TEXT COLLATE "C" NOT NULL,
    FOREIGN KEY (source_sha256, generation_id)
        REFERENCES {{schema}}.source_metadata_generations(source_sha256, generation_id) ON DELETE CASCADE
);

CREATE TABLE {{schema}}.photo_technical_metadata (
    generation_id          TEXT COLLATE "C" PRIMARY KEY
        REFERENCES {{schema}}.source_metadata_generations(generation_id) ON DELETE CASCADE,
    camera_make            TEXT COLLATE "C",
    camera_model           TEXT COLLATE "C",
    body_serial_number     TEXT COLLATE "C",
    lens_make              TEXT COLLATE "C",
    lens_model             TEXT COLLATE "C",
    camera_make_folded     TEXT COLLATE "C" NOT NULL DEFAULT '',
    camera_model_folded    TEXT COLLATE "C" NOT NULL DEFAULT '',
    lens_make_folded       TEXT COLLATE "C" NOT NULL DEFAULT '',
    lens_model_folded      TEXT COLLATE "C" NOT NULL DEFAULT '',
    iso                    BIGINT,
    exposure_time_seconds  DOUBLE PRECISION,
    f_number               DOUBLE PRECISION,
    exposure_bias_ev       DOUBLE PRECISION,
    focal_length_mm        DOUBLE PRECISION,
    width_px               BIGINT,
    height_px              BIGINT,
    capture_time           TEXT COLLATE "C",
    capture_time_raw       TEXT COLLATE "C",
    capture_time_precision TEXT COLLATE "C",
    capture_time_timezone  TEXT COLLATE "C",
    capture_time_offset    TEXT COLLATE "C",
    capture_sort_key       TEXT COLLATE "C" NOT NULL DEFAULT '',
    capture_date           TEXT COLLATE "C" NOT NULL DEFAULT '',
    orientation            BIGINT,
    latitude               DOUBLE PRECISION,
    longitude              DOUBLE PRECISION,
    location_label         TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.tags (
    id       TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    name     TEXT COLLATE "C" NOT NULL UNIQUE,
    revision BIGINT NOT NULL DEFAULT 1
);

CREATE TABLE {{schema}}.node_tags (
    node_id BIGINT NOT NULL REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE,
    tag_id  TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.tags(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, tag_id)
);

CREATE TABLE {{schema}}.photo_assets (
    asset_id                  TEXT COLLATE "C" PRIMARY KEY,
    kind                      TEXT COLLATE "C" NOT NULL,
    revision                  BIGINT NOT NULL DEFAULT 1,
    hidden_at                 TEXT COLLATE "C",
    excluded_at               TEXT COLLATE "C",
    display_file_id           TEXT COLLATE "C",
    display_override_file_id  TEXT COLLATE "C",
    created_at                TEXT COLLATE "C" NOT NULL,
    updated_at                TEXT COLLATE "C" NOT NULL,
    -- The most recent explicit burst representative choice has the highest rank.
    representative_rank       BIGINT
);

CREATE TABLE {{schema}}.photo_files (
    source_provenance_identity TEXT COLLATE "C" REFERENCES {{schema}}.provenance(identity) DEFERRABLE INITIALLY DEFERRED,
    file_id              TEXT COLLATE "C" PRIMARY KEY,
    asset_id             TEXT COLLATE "C" REFERENCES {{schema}}.photo_assets(asset_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    node_id              BIGINT NOT NULL UNIQUE REFERENCES {{schema}}.nodes(id)
        ON DELETE CASCADE,
    role                 TEXT COLLATE "C" NOT NULL,
    sidecar_of_file_id   TEXT COLLATE "C",
    revision             BIGINT NOT NULL DEFAULT 1,
    rating               BIGINT NOT NULL DEFAULT 0,
    flag                 TEXT COLLATE "C" NOT NULL DEFAULT '',
    label                TEXT COLLATE "C" NOT NULL DEFAULT '',
    caption              TEXT COLLATE "C" NOT NULL DEFAULT '',
    creator              TEXT COLLATE "C" NOT NULL DEFAULT '',
    copyright            TEXT COLLATE "C" NOT NULL DEFAULT '',
    rotation             BIGINT NOT NULL DEFAULT 0,
    confirmed_fields     BIGINT NOT NULL DEFAULT 0,
    flag_owner           TEXT COLLATE "C" NOT NULL DEFAULT '',
    created_at           TEXT COLLATE "C" NOT NULL,
    FOREIGN KEY (sidecar_of_file_id) REFERENCES {{schema}}.photo_files(file_id)
        ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE {{schema}}.photo_assets ADD FOREIGN KEY (display_file_id) REFERENCES {{schema}}.photo_files(file_id)
        ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE {{schema}}.photo_assets ADD FOREIGN KEY (display_override_file_id) REFERENCES {{schema}}.photo_files(file_id)
        ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE {{schema}}.photo_sets (
    set_id TEXT COLLATE "C" PRIMARY KEY,
    name TEXT COLLATE "C" NOT NULL,
    starred BIGINT NOT NULL DEFAULT 0,
    revision BIGINT NOT NULL DEFAULT 1,
    cover_asset_id TEXT COLLATE "C" REFERENCES {{schema}}.photo_assets(asset_id),
    created_at TEXT COLLATE "C" NOT NULL,
    updated_at TEXT COLLATE "C" NOT NULL,
    deleted_at TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.photo_change_receipts (
    receipt_id     TEXT COLLATE "C" PRIMARY KEY,
    operation      TEXT COLLATE "C" NOT NULL,
    asset_id       TEXT COLLATE "C",
    settings_key   TEXT COLLATE "C",
    set_id         TEXT COLLATE "C" REFERENCES {{schema}}.photo_sets(set_id),
    before_revision BIGINT NOT NULL,
    after_revision BIGINT NOT NULL,
    before_json    TEXT COLLATE "C" NOT NULL,
    after_json     TEXT COLLATE "C" NOT NULL,
    created_at     TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.photo_library_settings (
    singleton       BIGINT PRIMARY KEY,
    preference      TEXT COLLATE "C",
    revision        BIGINT NOT NULL DEFAULT 1,
    updated_at      TEXT COLLATE "C" NOT NULL,
    burst_gap_seconds BIGINT,
    backup_interval_seconds BIGINT,
    backup_keep_last BIGINT,
    hidden_unlock_lifetime_seconds BIGINT,
    representative_sequence BIGINT
);

CREATE TABLE {{schema}}.photo_xmp_unsettled (asset_id TEXT COLLATE "C" PRIMARY KEY);

CREATE INDEX provenance_node ON {{schema}}.provenance(node_id);

CREATE INDEX provenance_source_path ON {{schema}}.provenance(original_path);

CREATE UNIQUE INDEX provenance_direct_successor
    ON {{schema}}.provenance(supersedes) WHERE supersedes IS NOT NULL;

CREATE INDEX photo_technical_metadata_camera
    ON {{schema}}.photo_technical_metadata(camera_make_folded, camera_model_folded);

CREATE INDEX photo_technical_metadata_lens
    ON {{schema}}.photo_technical_metadata(lens_make_folded, lens_model_folded);

CREATE INDEX photo_technical_metadata_iso
    ON {{schema}}.photo_technical_metadata(iso);

CREATE INDEX photo_technical_metadata_location
    ON {{schema}}.photo_technical_metadata(latitude, longitude);

CREATE INDEX node_tags_tag ON {{schema}}.node_tags(tag_id);

CREATE INDEX photo_files_asset ON {{schema}}.photo_files(asset_id, role, file_id);

CREATE INDEX photo_assets_representative ON {{schema}}.photo_assets(representative_rank) WHERE representative_rank IS NOT NULL;

CREATE INDEX photo_files_sidecar ON {{schema}}.photo_files(sidecar_of_file_id);

CREATE INDEX photo_files_sidecar_nodes ON {{schema}}.photo_files(node_id) WHERE role='sidecar';

CREATE INDEX photo_change_receipts_asset
    ON {{schema}}.photo_change_receipts(asset_id, receipt_id);

CREATE INDEX photo_change_receipts_set ON {{schema}}.photo_change_receipts(set_id,receipt_id);

CREATE INDEX photo_change_receipts_operation ON {{schema}}.photo_change_receipts(operation);

CREATE INDEX photo_change_receipts_xmp_file
    ON {{schema}}.photo_change_receipts(((after_json::jsonb ->> 'file_id') COLLATE "C"))
    WHERE operation='xmp_sync';
CREATE INDEX photo_change_receipts_xmp_sidecar
    ON {{schema}}.photo_change_receipts(((after_json::jsonb #>> '{sync,sidecar,file_id}') COLLATE "C"))
    WHERE operation='xmp_sync';
CREATE INDEX photo_change_receipts_face_version
    ON {{schema}}.photo_change_receipts(((before_json::jsonb ->> 'content_version_id') COLLATE "C"),receipt_id)
    WHERE operation='face_decision';

-- Keep the token byte-for-byte equal to SQLite, including 0/1 booleans and
-- member revisions ordered by numeric node ID. JSON extraction and ordered
-- aggregation are the only engine-specific expressions.
CREATE VIEW {{schema}}.photo_xmp_inputs AS
SELECT inputs.*, token<>COALESCE(saved_token,'') AS pending FROM (
 SELECT facts.*, token_base || occupied::integer || '|' || audit_active::integer AS token FROM (
 SELECT f.file_id,f.node_id,f.asset_id,xr.receipt_id,
 COALESCE((xr.after_json::jsonb ->> 'conflicts')::bigint,0) AS conflicts,
 (xr.after_json::jsonb ->> 'token') COLLATE "C" AS saved_token,
 (f.role IN ('raw','image') AND n.trashed_at IS NULL AND (xr.receipt_id IS NOT NULL OR f.confirmed_fields<>0 OR EXISTS(SELECT 1 FROM {{schema}}.node_tags WHERE node_id=f.node_id) OR EXISTS(SELECT 1 FROM {{schema}}.photo_files sf WHERE sf.sidecar_of_file_id=f.file_id))) AS eligible,
 h.generation_id IS NOT NULL AS extracted,
 COALESCE(t.orientation,1) AS orientation,
 (SELECT count(*) FROM {{schema}}.photo_files WHERE asset_id=f.asset_id) AS members,
 EXISTS(SELECT 1 FROM {{schema}}.nodes o WHERE o.parent_id=n.parent_id AND o.name=n.name || '.xmp' AND o.trashed_at IS NULL) AS occupied,
 EXISTS(SELECT 1 FROM {{schema}}.audit_authority) AS audit_active,
 EXISTS(SELECT 1 FROM {{schema}}.audit_memberships WHERE node_id=f.node_id) AS audited,
 f.revision || '|' || f.role || '|' || a.asset_id || '|' || a.revision || '|' || n.revision || '|' || n.current_version_id || '|' ||
 (h.generation_id IS NOT NULL)::integer || ':' ||
 COALESCE(t.orientation,1) || '|' || (SELECT string_agg(
 n.id || ':' || n.revision || ':' || f.revision || ':' || COALESCE(f.sidecar_of_file_id,'') ||
 CASE WHEN EXISTS (SELECT 1 FROM {{schema}}.audit_memberships m WHERE m.node_id=n.id) THEN ':audited' ELSE '' END,
 ',' ORDER BY n.id)
 FROM {{schema}}.photo_files f JOIN {{schema}}.nodes n ON n.id=f.node_id
 WHERE f.asset_id=a.asset_id) || '|' AS token_base
 FROM {{schema}}.photo_files f JOIN {{schema}}.photo_assets a ON a.asset_id=f.asset_id JOIN {{schema}}.nodes n ON n.id=f.node_id
 LEFT JOIN {{schema}}.content_versions xv ON xv.version_id=n.current_version_id
 LEFT JOIN {{schema}}.source_metadata_heads h ON h.source_sha256=xv.blob_hash
 LEFT JOIN {{schema}}.photo_technical_metadata t ON t.generation_id=h.generation_id
 LEFT JOIN {{schema}}.photo_change_receipts xr ON xr.operation='xmp_sync' AND (xr.after_json::jsonb ->> 'file_id') COLLATE "C"=f.file_id
) facts
) inputs;
