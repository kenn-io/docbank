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

-- Retained metadata catalogs used by full enrollment validation.
CREATE TABLE {{schema}}.export_sources (
    id TEXT COLLATE "C" PRIMARY KEY,
    owner TEXT COLLATE "C" NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    request_json BYTEA NOT NULL,
    canonical_json BYTEA NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    expires_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.export_chunks (
    source_id TEXT COLLATE "C" NOT NULL,
    chunk_index BIGINT NOT NULL,
    canonical_json BYTEA NOT NULL,
    PRIMARY KEY(source_id,chunk_index)
);

CREATE TABLE {{schema}}.export_members (
    source_id TEXT COLLATE "C" NOT NULL,
    node_id BIGINT NOT NULL,
    version_id TEXT COLLATE "C" NOT NULL,
    blob_hash TEXT COLLATE "C" NOT NULL,
    canonical_json BYTEA NOT NULL,
    PRIMARY KEY(source_id,node_id,version_id)
);

CREATE TABLE {{schema}}.export_plans (
    id TEXT COLLATE "C" PRIMARY KEY,
    owner TEXT COLLATE "C" NOT NULL,
    source_id TEXT COLLATE "C" NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    canonical_json BYTEA NOT NULL,
    expires_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.export_documents (
    plan_id TEXT COLLATE "C" NOT NULL,
    ordinal BIGINT NOT NULL,
    canonical_json BYTEA NOT NULL,
    PRIMARY KEY(plan_id,ordinal)
);

CREATE TABLE {{schema}}.export_role_roots (
    plan_id TEXT COLLATE "C" NOT NULL,
    blob_hash TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY(plan_id,blob_hash)
);

CREATE TABLE {{schema}}.export_jobs (
    id TEXT COLLATE "C" PRIMARY KEY,
    owner TEXT COLLATE "C" NOT NULL,
    plan_id TEXT COLLATE "C" NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    canonical_json BYTEA NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    epoch BIGINT NOT NULL,
    token TEXT COLLATE "C" NOT NULL,
    archive_name TEXT COLLATE "C" NOT NULL,
    expires_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.collection_snapshots (
    snapshot_id TEXT COLLATE "C" PRIMARY KEY,
    vault_uid UUID NOT NULL,
    predecessor_id TEXT COLLATE "C",
    source_collection_ids_json BYTEA NOT NULL DEFAULT '[]',
    member_count BIGINT NOT NULL,
    page_count BIGINT NOT NULL,
    member_hash TEXT COLLATE "C" NOT NULL,
    manifest_sha256 TEXT COLLATE "C" NOT NULL,
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL,
    sealed_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.collection_snapshot_members (
    snapshot_id TEXT COLLATE "C" NOT NULL,
    ordinal BIGINT NOT NULL,
    occurrence_id TEXT COLLATE "C" NOT NULL,
    node_id BIGINT NOT NULL,
    content_version_id TEXT COLLATE "C" NOT NULL,
    blob_sha256 TEXT COLLATE "C" NOT NULL,
    size BIGINT NOT NULL,
    family_id TEXT COLLATE "C" NOT NULL,
    parent_occurrence_id TEXT COLLATE "C",
    family_order BIGINT NOT NULL,
    display_name TEXT COLLATE "C" NOT NULL,
    frozen_fields_json BYTEA NOT NULL,
    document_kind TEXT COLLATE "C" NOT NULL,
    selected_source_pages_json BYTEA,
    selected_pdf_sha256 TEXT COLLATE "C",
    source_page_count BIGINT NOT NULL DEFAULT 0,
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (snapshot_id, ordinal),
    UNIQUE (snapshot_id, occurrence_id)
);

CREATE TABLE {{schema}}.collection_snapshot_representations (
    snapshot_id TEXT COLLATE "C" NOT NULL,
    occurrence_id TEXT COLLATE "C" NOT NULL,
    role TEXT COLLATE "C" NOT NULL,
    ordinal BIGINT NOT NULL,
    status TEXT COLLATE "C" NOT NULL,
    text_authority TEXT COLLATE "C" NOT NULL DEFAULT 'none',
    content_version_id TEXT COLLATE "C",
    blob_sha256 TEXT COLLATE "C",
    size BIGINT,
    media_type TEXT COLLATE "C" NOT NULL,
    page_number BIGINT,
    verified_page_count BIGINT,
    rendition_build_id TEXT COLLATE "C",
    lexical_generation_id TEXT COLLATE "C",
    recipe_sha256 TEXT COLLATE "C",
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (snapshot_id, occurrence_id, role, ordinal)
);

CREATE TABLE {{schema}}.packages (
    package_id TEXT COLLATE "C" PRIMARY KEY,
    snapshot_id TEXT COLLATE "C",
    direction TEXT COLLATE "C" NOT NULL,
    package_name TEXT COLLATE "C" NOT NULL,
    party_label TEXT COLLATE "C" NOT NULL,
    profile_sha256 TEXT COLLATE "C" NOT NULL,
    profile_json BYTEA NOT NULL,
    mapping_sha256 TEXT COLLATE "C" NOT NULL,
    mapping_json BYTEA NOT NULL,
    manifest_sha256 TEXT COLLATE "C" NOT NULL,
    manifest_blob_sha256 TEXT COLLATE "C" NOT NULL,
    predecessor_package_id TEXT COLLATE "C",
    relation TEXT COLLATE "C" NOT NULL,
    ingest_id TEXT COLLATE "C",
    export_plan_id TEXT COLLATE "C",
    state TEXT COLLATE "C" NOT NULL,
    produced_on TEXT COLLATE "C",
    created_at TEXT COLLATE "C" NOT NULL,
    completed_at TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.package_volumes (
    package_id TEXT COLLATE "C" NOT NULL,
    ordinal BIGINT NOT NULL,
    volume_name TEXT COLLATE "C" NOT NULL,
    declared_root TEXT COLLATE "C" NOT NULL,
    mapped_root TEXT COLLATE "C" NOT NULL,
    resolved_root_sha256 TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (package_id, volume_name)
);

CREATE TABLE {{schema}}.bates_namespaces (
    namespace_id TEXT COLLATE "C" PRIMARY KEY,
    prefix TEXT COLLATE "C" NOT NULL,
    suffix TEXT COLLATE "C" NOT NULL,
    padding BIGINT NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    UNIQUE(prefix,suffix)
);

CREATE TABLE {{schema}}.bates_namespace_cursors (
    namespace_id TEXT COLLATE "C" PRIMARY KEY,
    next_sequence BIGINT NOT NULL
);

CREATE TABLE {{schema}}.bates_allocations (
    allocation_id TEXT COLLATE "C" PRIMARY KEY,
    operation_id TEXT COLLATE "C" NOT NULL UNIQUE,
    namespace_id TEXT COLLATE "C" NOT NULL,
    snapshot_id TEXT COLLATE "C" NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    recipe_sha256 TEXT COLLATE "C" NOT NULL,
    start_sequence BIGINT NOT NULL,
    end_sequence BIGINT NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    committed_at TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.bates_page_labels (
    allocation_id TEXT COLLATE "C" NOT NULL,
    ordinal BIGINT NOT NULL,
    namespace_id TEXT COLLATE "C" NOT NULL,
    sequence BIGINT NOT NULL,
    occurrence_id TEXT COLLATE "C" NOT NULL,
    source_page BIGINT NOT NULL,
    output_page BIGINT NOT NULL,
    label TEXT COLLATE "C" NOT NULL UNIQUE,
    PRIMARY KEY(allocation_id,ordinal),
    UNIQUE(namespace_id,sequence)
);

CREATE TABLE {{schema}}.bates_artifacts (
    artifact_id TEXT COLLATE "C" PRIMARY KEY,
    allocation_id TEXT COLLATE "C" NOT NULL UNIQUE,
    blob_hash TEXT COLLATE "C" NOT NULL,
    size BIGINT NOT NULL,
    media_type TEXT COLLATE "C" NOT NULL,
    page_count BIGINT NOT NULL,
    recipe_json BYTEA NOT NULL,
    manifest_sha256 TEXT COLLATE "C" NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.bates_artifact_pages (
    artifact_id TEXT COLLATE "C" NOT NULL,
    ordinal BIGINT NOT NULL,
    occurrence_id TEXT COLLATE "C" NOT NULL,
    source_blob_sha256 TEXT COLLATE "C" NOT NULL,
    source_page BIGINT NOT NULL,
    output_page BIGINT NOT NULL,
    label TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY(artifact_id,ordinal)
);

CREATE TABLE {{schema}}.package_records (
    package_id TEXT COLLATE "C" NOT NULL,
    row_id TEXT COLLATE "C" NOT NULL,
    load_file TEXT COLLATE "C" NOT NULL,
    row_ordinal BIGINT NOT NULL,
    occurrence_id TEXT COLLATE "C" NOT NULL,
    raw_json BYTEA NOT NULL,
    raw_sha256 TEXT COLLATE "C" NOT NULL,
    sensitive BIGINT NOT NULL,
    PRIMARY KEY (package_id, row_id),
    UNIQUE (package_id, occurrence_id)
);

CREATE TABLE {{schema}}.package_labels (
    package_id TEXT COLLATE "C" NOT NULL,
    provenance TEXT COLLATE "C" NOT NULL,
    label_set TEXT COLLATE "C" NOT NULL,
    label TEXT COLLATE "C" NOT NULL,
    occurrence_id TEXT COLLATE "C" NOT NULL,
    content_version_id TEXT COLLATE "C" NOT NULL,
    artifact_id TEXT COLLATE "C",
    page_number BIGINT NOT NULL DEFAULT 0,
    page_state TEXT COLLATE "C" NOT NULL,
    endpoint TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (package_id, provenance, label_set, label, endpoint, occurrence_id, page_number)
);

CREATE TABLE {{schema}}.package_import_jobs (
    id TEXT COLLATE "C" PRIMARY KEY,
    owner TEXT COLLATE "C" NOT NULL,
    operation_id TEXT COLLATE "C" NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    preflight_id TEXT COLLATE "C" NOT NULL,
    package_id TEXT COLLATE "C" NOT NULL,
    claim_owner TEXT COLLATE "C",
    lease_expires_at TEXT COLLATE "C",
    state TEXT COLLATE "C" NOT NULL,
    epoch BIGINT NOT NULL,
    token TEXT COLLATE "C" NOT NULL,
    job_json BYTEA NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    updated_at TEXT COLLATE "C" NOT NULL,
    UNIQUE(owner, operation_id),
    UNIQUE(package_id)
);

CREATE TABLE {{schema}}.package_import_receipts (
    receipt_id TEXT COLLATE "C" PRIMARY KEY,
    package_id TEXT COLLATE "C" NOT NULL,
    record_key TEXT COLLATE "C" NOT NULL,
    occurrence_id TEXT COLLATE "C" NOT NULL,
    content_version_id TEXT COLLATE "C",
    state TEXT COLLATE "C" NOT NULL,
    receipt_json BYTEA NOT NULL,
    recorded_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.page_documents (
    version_id TEXT COLLATE "C" PRIMARY KEY,
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.page_frames (
    version_id TEXT COLLATE "C" NOT NULL,
    page BIGINT NOT NULL,
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY(version_id,page)
);

CREATE TABLE {{schema}}.page_recipes (
    checksum TEXT COLLATE "C" PRIMARY KEY,
    canonical_json BYTEA NOT NULL
);

CREATE TABLE {{schema}}.page_images (
    version_id TEXT COLLATE "C" NOT NULL,
    page BIGINT NOT NULL,
    recipe_sha256 TEXT COLLATE "C" NOT NULL,
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL,
    blob_hash TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY(version_id,recipe_sha256,page)
);

CREATE TABLE {{schema}}.page_render_jobs (
    id TEXT COLLATE "C" PRIMARY KEY,
    node_id BIGINT NOT NULL,
    version_id TEXT COLLATE "C" NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    request_json BYTEA NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    results_json BYTEA NOT NULL,
    failure_code TEXT COLLATE "C" NOT NULL,
    epoch BIGINT NOT NULL,
    token TEXT COLLATE "C" NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    updated_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.package_preflights (
    preflight_id TEXT COLLATE "C" PRIMARY KEY,
    owner TEXT COLLATE "C" NOT NULL,
    source_kind TEXT COLLATE "C" NOT NULL,
    source_ref TEXT COLLATE "C" NOT NULL,
    source_locator TEXT COLLATE "C" NOT NULL DEFAULT '',
    profile_sha256 TEXT COLLATE "C" NOT NULL,
    profile_json TEXT COLLATE "C" NOT NULL DEFAULT '',
    mapping_sha256 TEXT COLLATE "C" NOT NULL,
    mapping_json TEXT COLLATE "C" NOT NULL DEFAULT '',
    manifest_sha256 TEXT COLLATE "C" NOT NULL,
    manifest_blob_sha256 TEXT COLLATE "C" NOT NULL,
    diagnostics_blob_sha256 TEXT COLLATE "C",
    canonical_json BYTEA NOT NULL,
    diagnostics_json BYTEA NOT NULL,
    blocking BIGINT NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    expires_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.blob_stores (
    store_id        TEXT COLLATE "C" PRIMARY KEY,
    name            TEXT COLLATE "C" NOT NULL UNIQUE,
    kind            TEXT COLLATE "C" NOT NULL,
    role            TEXT COLLATE "C" NOT NULL,
    lifecycle       TEXT COLLATE "C" NOT NULL,
    binding         TEXT COLLATE "C" NOT NULL,
    ownership_epoch TEXT COLLATE "C" NOT NULL,
    created_at      TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.storage_operations (
    operation_id      TEXT COLLATE "C" PRIMARY KEY,
    kind              TEXT COLLATE "C" NOT NULL,
    source_store_id   TEXT COLLATE "C",
    request_version   BIGINT NOT NULL,
    request_digest    TEXT COLLATE "C" NOT NULL,
    request_json      TEXT COLLATE "C" NOT NULL,
    plan_json         TEXT COLLATE "C" NOT NULL,
    state             TEXT COLLATE "C" NOT NULL,
    cursor            TEXT COLLATE "C" NOT NULL DEFAULT '',
    total_objects     BIGINT NOT NULL DEFAULT 0,
    completed_objects BIGINT NOT NULL DEFAULT 0,
    copied_objects    BIGINT NOT NULL DEFAULT 0,
    copied_bytes      BIGINT NOT NULL DEFAULT 0,
    cancel_requested  BIGINT NOT NULL DEFAULT 0,
    error             TEXT COLLATE "C" NOT NULL DEFAULT '',
    receipt_json      TEXT COLLATE "C" NOT NULL DEFAULT '',
    created_at        TEXT COLLATE "C" NOT NULL,
    updated_at        TEXT COLLATE "C" NOT NULL,
    finished_at       TEXT COLLATE "C",
    retention_until   TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.media_sources (
    source_id       TEXT COLLATE "C" PRIMARY KEY,
    kind            TEXT COLLATE "C" NOT NULL,
    provider        TEXT COLLATE "C" NOT NULL,
    origin_scope    TEXT COLLATE "C" NOT NULL,
    identity_sha256 TEXT COLLATE "C" NOT NULL UNIQUE,
    created_at      TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.media_source_versions (
    source_version_id  TEXT COLLATE "C" PRIMARY KEY,
    source_id          TEXT COLLATE "C" NOT NULL,
    revision           BIGINT NOT NULL,
    content_version_id TEXT COLLATE "C" NOT NULL,
    capture_json       TEXT COLLATE "C" NOT NULL,
    created_at         TEXT COLLATE "C" NOT NULL,
    UNIQUE (source_id, revision)
);

CREATE TABLE {{schema}}.media_occurrences (
    occurrence_id         TEXT COLLATE "C" PRIMARY KEY,
    source_id             TEXT COLLATE "C" NOT NULL,
    source_version_id     TEXT COLLATE "C",
    caller_principal      TEXT COLLATE "C" NOT NULL,
    caller_occurrence_ref TEXT COLLATE "C" NOT NULL,
    caller_revision       TEXT COLLATE "C" NOT NULL,
    caller_filename       TEXT COLLATE "C" NOT NULL,
    caller_person_ref     TEXT COLLATE "C" NOT NULL,
    speaker_label         TEXT COLLATE "C" NOT NULL,
    message_json          TEXT COLLATE "C" NOT NULL,
    visible               BIGINT NOT NULL,
    first_seen_at         TEXT COLLATE "C" NOT NULL,
    revoked_at            TEXT COLLATE "C",
    UNIQUE (caller_principal, caller_occurrence_ref, caller_revision)
);

CREATE TABLE {{schema}}.media_input_artifacts (
    input_id           TEXT COLLATE "C" PRIMARY KEY,
    occurrence_id      TEXT COLLATE "C" NOT NULL,
    source_id          TEXT COLLATE "C" NOT NULL,
    source_version_id  TEXT COLLATE "C",
    content_version_id TEXT COLLATE "C" NOT NULL,
    kind               TEXT COLLATE "C" NOT NULL,
    origin             TEXT COLLATE "C" NOT NULL,
    provider           TEXT COLLATE "C" NOT NULL,
    language           TEXT COLLATE "C" NOT NULL,
    input_sha256       TEXT COLLATE "C" NOT NULL,
    created_at         TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.media_operations (
    operation_id  TEXT COLLATE "C" PRIMARY KEY,
    principal     TEXT COLLATE "C" NOT NULL,
    verb          TEXT COLLATE "C" NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    source_id     TEXT COLLATE "C",
    receipt_json  TEXT COLLATE "C" NOT NULL,
    created_at    TEXT COLLATE "C" NOT NULL,
    updated_at    TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.visual_preview_generations (
    generation_id         TEXT COLLATE "C" PRIMARY KEY,
    vault_uid UUID NOT NULL,
    content_version_id    TEXT COLLATE "C" NOT NULL,
    source_sha256         TEXT COLLATE "C" NOT NULL,
    contract_version      TEXT COLLATE "C" NOT NULL,
    recipe_fingerprint    TEXT COLLATE "C" NOT NULL,
    canonical_result      BYTEA NOT NULL,
    checksum              TEXT COLLATE "C" NOT NULL,
    state                 TEXT COLLATE "C" NOT NULL,
    output_blob_hash      TEXT COLLATE "C",
    output_size           BIGINT,
    output_media_type     TEXT COLLATE "C",
    output_width          BIGINT,
    output_height         BIGINT,
    failure_code          TEXT COLLATE "C",
    failure_detail        TEXT COLLATE "C",
    created_at            TEXT COLLATE "C" NOT NULL,
    UNIQUE (content_version_id, recipe_fingerprint),
    UNIQUE (content_version_id, generation_id)
);

CREATE TABLE {{schema}}.visual_preview_heads (
    content_version_id TEXT COLLATE "C" PRIMARY KEY,
    generation_id      TEXT COLLATE "C" NOT NULL UNIQUE,
    published_at       TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.collection_labels (
    ingest_id  TEXT COLLATE "C" PRIMARY KEY,
    label      TEXT COLLATE "C" UNIQUE,
    revision   BIGINT NOT NULL,
    updated_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.watch_sources (
    watch_name TEXT COLLATE "C" NOT NULL,
    source_ref TEXT COLLATE "C" NOT NULL,
    node_id    BIGINT NOT NULL UNIQUE,
    blob_hash  TEXT COLLATE "C" NOT NULL,
    size       BIGINT NOT NULL,
    PRIMARY KEY (watch_name, source_ref)
);

CREATE TABLE {{schema}}.watch_source_absences (
    watch_name TEXT COLLATE "C" NOT NULL,
    source_ref TEXT COLLATE "C" NOT NULL,
    absent_since TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (watch_name, source_ref),
    FOREIGN KEY (watch_name, source_ref)
        REFERENCES {{schema}}.watch_sources(watch_name, source_ref)
        ON UPDATE CASCADE ON DELETE CASCADE
);

CREATE TABLE {{schema}}.push_sources (
    push_name          TEXT COLLATE "C" NOT NULL,
    source_ref         TEXT COLLATE "C" NOT NULL,
    node_id            BIGINT NOT NULL UNIQUE,
    provenance_identity TEXT COLLATE "C" NOT NULL UNIQUE,
    blob_hash          TEXT COLLATE "C" NOT NULL,
    size               BIGINT NOT NULL,
    accepted_at        TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (push_name, source_ref)
);

CREATE TABLE {{schema}}.processing_incarnations (
    incarnation_id TEXT COLLATE "C" PRIMARY KEY,
    created_at     TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.current_processing_incarnation (
    singleton      BIGINT PRIMARY KEY,
    incarnation_id TEXT COLLATE "C" NOT NULL UNIQUE
);

CREATE TABLE {{schema}}.processing_consent_grants (
    grant_id                  TEXT COLLATE "C" PRIMARY KEY,
    consent_set_id            TEXT COLLATE "C" NOT NULL,
    vault_uid UUID NOT NULL,
    incarnation_id            TEXT COLLATE "C" NOT NULL,
    principal                 TEXT COLLATE "C" NOT NULL,
    scope                     TEXT COLLATE "C" NOT NULL,
    profile_fingerprint       TEXT COLLATE "C" NOT NULL,
    disclosure_fingerprint    TEXT COLLATE "C" NOT NULL,
    input_classes_json        TEXT COLLATE "C" NOT NULL,
    retained_classes_json     TEXT COLLATE "C" NOT NULL,
    revocation_fence          BIGINT NOT NULL,
    issued_at                 TEXT COLLATE "C" NOT NULL,
    expires_at                TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.processing_consent_revocations (
    revocation_id  TEXT COLLATE "C" PRIMARY KEY,
    vault_uid UUID NOT NULL,
    incarnation_id TEXT COLLATE "C" NOT NULL,
    principal      TEXT COLLATE "C" NOT NULL,
    scope          TEXT COLLATE "C" NOT NULL,
    fence          BIGINT NOT NULL,
    revoked_at     TEXT COLLATE "C" NOT NULL,
    UNIQUE (vault_uid, incarnation_id, principal, scope, fence)
);

CREATE TABLE {{schema}}.processing_profiles (
    profile_fingerprint               TEXT COLLATE "C" PRIMARY KEY,
    canonical_profile                 TEXT COLLATE "C" NOT NULL,
    rendition_request_fingerprint     TEXT COLLATE "C" NOT NULL,
    evidence_lexical_fingerprint      TEXT COLLATE "C" NOT NULL,
    retention_disclosure_fingerprint  TEXT COLLATE "C" NOT NULL,
    attachment_policy_fingerprint     TEXT COLLATE "C" NOT NULL,
    consent_fingerprint               TEXT COLLATE "C" NOT NULL,
    rendition_disclosure_fingerprint  TEXT COLLATE "C" NOT NULL,
    trust_boundary                    TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.rendition_builds (
    build_id                             TEXT COLLATE "C" PRIMARY KEY,
    vault_uid UUID NOT NULL,
    source_sha256                        TEXT COLLATE "C" NOT NULL,
    rendition_request_fingerprint        TEXT COLLATE "C" NOT NULL,
    evidence_lexical_fingerprint         TEXT COLLATE "C" NOT NULL,
    captured_artifact_policy_fingerprint TEXT COLLATE "C" NOT NULL,
    captured_artifact_policy_json        TEXT COLLATE "C" NOT NULL,
    authorization_checksum               TEXT COLLATE "C" NOT NULL,
    provider_operation_id                TEXT COLLATE "C" NOT NULL,
    provider_receipt_json                 TEXT COLLATE "C" NOT NULL,
    evidence_checksum                    TEXT COLLATE "C" NOT NULL,
    rendition_checksum                   TEXT COLLATE "C" NOT NULL,
    markdown_checksum                    TEXT COLLATE "C" NOT NULL,
    completeness                         TEXT COLLATE "C" NOT NULL,
    partial_success                      BIGINT NOT NULL,
    truncated                            BIGINT NOT NULL,
    warnings_json                        TEXT COLLATE "C" NOT NULL,
    completed_at                         TEXT COLLATE "C" NOT NULL,
    declared_artifact_count              BIGINT NOT NULL,
    unit_count                           BIGINT NOT NULL,
    lexical_segment_count                BIGINT NOT NULL,
    UNIQUE (vault_uid, build_id)
);

CREATE TABLE {{schema}}.rendition_artifacts (
    build_id    TEXT COLLATE "C" NOT NULL,
    artifact_id TEXT COLLATE "C" NOT NULL,
    role        TEXT COLLATE "C" NOT NULL,
    blob_hash   TEXT COLLATE "C" NOT NULL,
    size        BIGINT NOT NULL,
    checksum    TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (build_id, artifact_id)
);

CREATE TABLE {{schema}}.rendition_units (
    build_id          TEXT COLLATE "C" NOT NULL,
    unit_id           TEXT COLLATE "C" NOT NULL,
    evidence_unit_id  TEXT COLLATE "C" NOT NULL,
    unit_order        BIGINT NOT NULL,
    checksum          TEXT COLLATE "C" NOT NULL,
    heading_path_json TEXT COLLATE "C" NOT NULL,
    locator_json      TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (build_id, unit_id),
    UNIQUE (build_id, unit_order)
);

CREATE TABLE {{schema}}.rendition_lexical_segments (
    build_id     TEXT COLLATE "C" NOT NULL,
    segment_id   TEXT COLLATE "C" NOT NULL,
    unit_id      TEXT COLLATE "C" NOT NULL,
    segment_order BIGINT NOT NULL,
    char_start   BIGINT NOT NULL,
    char_end     BIGINT NOT NULL,
    checksum     TEXT COLLATE "C" NOT NULL,
    text         TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (build_id, segment_id),
    UNIQUE (build_id, segment_order)
);

CREATE TABLE {{schema}}.rendition_attachments (
    attachment_id                    TEXT COLLATE "C" PRIMARY KEY,
    vault_uid UUID NOT NULL,
    content_version_id               TEXT COLLATE "C" NOT NULL,
    build_id                          TEXT COLLATE "C" NOT NULL,
    profile_fingerprint               TEXT COLLATE "C" NOT NULL,
    retention_disclosure_fingerprint  TEXT COLLATE "C" NOT NULL,
    attachment_policy_fingerprint     TEXT COLLATE "C" NOT NULL,
    consent_fingerprint               TEXT COLLATE "C" NOT NULL,
    rendition_disclosure_fingerprint  TEXT COLLATE "C" NOT NULL,
    trust_boundary                    TEXT COLLATE "C" NOT NULL,
    attached_at                       TEXT COLLATE "C" NOT NULL,
    UNIQUE (content_version_id, profile_fingerprint, attachment_id),
    UNIQUE (content_version_id, profile_fingerprint, build_id)
);

CREATE TABLE {{schema}}.rendition_heads (
    content_version_id          TEXT COLLATE "C" NOT NULL,
    profile_fingerprint         TEXT COLLATE "C" NOT NULL,
    attachment_id               TEXT COLLATE "C" NOT NULL,
    published_at                TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (content_version_id, profile_fingerprint)
);

CREATE TABLE {{schema}}.embedding_vector_spaces (
    vector_space_id          TEXT COLLATE "C" PRIMARY KEY,
    contract_version         TEXT COLLATE "C" NOT NULL,
    descriptor_json          BYTEA NOT NULL,
    provider_descriptor      TEXT COLLATE "C" NOT NULL,
    provider_revision        TEXT COLLATE "C" NOT NULL,
    descriptor_fingerprint   TEXT COLLATE "C" NOT NULL,
    compatibility_id         TEXT COLLATE "C" NOT NULL,
    dimensions               BIGINT NOT NULL,
    metric                   TEXT COLLATE "C" NOT NULL,
    normalization            TEXT COLLATE "C" NOT NULL,
    scalar_encoding          TEXT COLLATE "C" NOT NULL,
    document_formatter       TEXT COLLATE "C" NOT NULL,
    query_formatter          TEXT COLLATE "C" NOT NULL,
    model_input_fingerprint  TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.embedding_input_generations (
    generation_id                  TEXT COLLATE "C" PRIMARY KEY,
    generation_blob_hash           TEXT COLLATE "C",
    generation_encoded_size        BIGINT NOT NULL,
    generation_checksum            TEXT COLLATE "C" NOT NULL,
    source_version_id              TEXT COLLATE "C" NOT NULL,
    profile_fingerprint            TEXT COLLATE "C" NOT NULL,
    evidence_fingerprint           TEXT COLLATE "C" NOT NULL,
    tokenizer_fingerprint          TEXT COLLATE "C" NOT NULL,
    chunk_policy_fingerprint       TEXT COLLATE "C" NOT NULL,
    formatter_fingerprint          TEXT COLLATE "C" NOT NULL,
    attachment_context_fingerprint TEXT COLLATE "C" NOT NULL,
    attachment_id                  TEXT COLLATE "C",
    input_count                    BIGINT NOT NULL,
    created_at                     TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.embedding_jobs (
    job_id              TEXT COLLATE "C" PRIMARY KEY,
    vault_uid UUID NOT NULL,
    content_version_id  TEXT COLLATE "C" NOT NULL,
    profile_fingerprint TEXT COLLATE "C" NOT NULL,
    binding_id          TEXT COLLATE "C" NOT NULL,
    input_kind          TEXT COLLATE "C" NOT NULL,
    generation_id       TEXT COLLATE "C" NOT NULL,
    vector_space_id     TEXT COLLATE "C" NOT NULL,
    principal           TEXT COLLATE "C" NOT NULL,
    scope               TEXT COLLATE "C" NOT NULL,
    authorization_grant_id TEXT COLLATE "C" NOT NULL,
    authorization_incarnation_id TEXT COLLATE "C" NOT NULL,
    authorization_revocation_fence BIGINT NOT NULL,
    state               TEXT COLLATE "C" NOT NULL,
    claim_owner         TEXT COLLATE "C",
    claim_epoch         BIGINT NOT NULL DEFAULT 0,
    claim_count         BIGINT NOT NULL DEFAULT 0,
    lease_expires_at    TEXT COLLATE "C",
    available_at        TEXT COLLATE "C" NOT NULL,
    failure_code        TEXT COLLATE "C",
    receipt_json        TEXT COLLATE "C",
    created_at          TEXT COLLATE "C" NOT NULL,
    updated_at          TEXT COLLATE "C" NOT NULL,
    UNIQUE (content_version_id,profile_fingerprint,binding_id,input_kind,generation_id,
            authorization_grant_id,authorization_incarnation_id,authorization_revocation_fence)
);

CREATE TABLE {{schema}}.embedding_generation_inputs (
    generation_id     TEXT COLLATE "C" NOT NULL,
    input_id          TEXT COLLATE "C" NOT NULL,
    input_order       BIGINT NOT NULL,
    rendered_checksum TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (generation_id, input_id),
    UNIQUE (generation_id, input_order)
);

CREATE TABLE {{schema}}.embedding_vector_sets (
    vector_set_id      TEXT COLLATE "C" PRIMARY KEY,
    contract_version   TEXT COLLATE "C" NOT NULL,
    vector_space_id    TEXT COLLATE "C" NOT NULL,
    payload_blob_hash  TEXT COLLATE "C" NOT NULL,
    payload_size       BIGINT NOT NULL,
    payload_checksum   TEXT COLLATE "C" NOT NULL,
    manifest_checksum  TEXT COLLATE "C" NOT NULL,
    row_count          BIGINT NOT NULL,
    dimensions         BIGINT NOT NULL
);

CREATE TABLE {{schema}}.embedding_vector_rows (
    vector_set_id  TEXT COLLATE "C" NOT NULL,
    row_id         TEXT COLLATE "C" NOT NULL,
    row_order      BIGINT NOT NULL,
    input_id       TEXT COLLATE "C" NOT NULL,
    dimensions     BIGINT NOT NULL,
    checksum       TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (vector_set_id, row_id),
    UNIQUE (vector_set_id, row_order),
    UNIQUE (vector_set_id, input_id)
);

CREATE TABLE {{schema}}.embedding_sets (
    embedding_set_id             TEXT COLLATE "C" PRIMARY KEY,
    vault_uid UUID NOT NULL,
    binding_id                   TEXT COLLATE "C" NOT NULL,
    input_kind                   TEXT COLLATE "C" NOT NULL,
    content_version_id           TEXT COLLATE "C" NOT NULL,
    profile_fingerprint          TEXT COLLATE "C" NOT NULL,
    embedding_input_fingerprint  TEXT COLLATE "C" NOT NULL,
    vector_space_id              TEXT COLLATE "C" NOT NULL,
    input_generation_id          TEXT COLLATE "C" NOT NULL,
    vector_set_id                TEXT COLLATE "C" NOT NULL,
    created_at                   TEXT COLLATE "C" NOT NULL,
    UNIQUE (vault_uid, content_version_id, binding_id, input_kind,
            vector_space_id, input_generation_id, vector_set_id)
);

CREATE TABLE {{schema}}.embedding_heads (
    content_version_id   TEXT COLLATE "C" NOT NULL,
    binding_id           TEXT COLLATE "C" NOT NULL,
    input_kind           TEXT COLLATE "C" NOT NULL,
    embedding_set_id     TEXT COLLATE "C" NOT NULL,
    vector_space_id      TEXT COLLATE "C" NOT NULL,
    profile_fingerprint  TEXT COLLATE "C" NOT NULL,
    published_at         TEXT COLLATE "C" NOT NULL,
    fencing_token        BIGINT NOT NULL,
    PRIMARY KEY (content_version_id, profile_fingerprint, binding_id, input_kind)
);

CREATE TABLE {{schema}}.embedding_failures (
    content_version_id   TEXT COLLATE "C" NOT NULL,
    profile_fingerprint  TEXT COLLATE "C" NOT NULL,
    binding_id           TEXT COLLATE "C" NOT NULL,
    input_kind           TEXT COLLATE "C" NOT NULL,
    failure_code         TEXT COLLATE "C" NOT NULL,
    failed_at            TEXT COLLATE "C" NOT NULL,
    fencing_token        BIGINT NOT NULL,
    attachment_id        TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (content_version_id, profile_fingerprint, binding_id, input_kind)
);

CREATE TABLE {{schema}}.rendition_lexical_index (
    row_id     BIGINT PRIMARY KEY,
    build_id   TEXT COLLATE "C" NOT NULL,
    segment_id TEXT COLLATE "C" NOT NULL,
    text       TEXT COLLATE "C" NOT NULL,
    UNIQUE (build_id, segment_id)
);

CREATE TABLE {{schema}}.rendition_lexical_generations (
    generation_id TEXT COLLATE "C" PRIMARY KEY,
    segment_count BIGINT NOT NULL,
    build_count   BIGINT NOT NULL,
    built_at      TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.rendition_lexical_generation_manifests (
    generation_id  TEXT COLLATE "C" PRIMARY KEY,
    manifest_digest TEXT COLLATE "C" NOT NULL,
    build_digest    TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.rendition_lexical_generation_builds (
    generation_id TEXT COLLATE "C" NOT NULL,
    build_id      TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (generation_id, build_id)
);

CREATE TABLE {{schema}}.rendition_lexical_heads (
    singleton     BIGINT PRIMARY KEY,
    generation_id TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.rendition_lexical_superseded (
    generation_id TEXT COLLATE "C" PRIMARY KEY
);

CREATE TABLE {{schema}}.rendition_jobs (
    job_id                               TEXT COLLATE "C" PRIMARY KEY,
    vault_uid UUID NOT NULL,
    source_sha256                        TEXT COLLATE "C" NOT NULL,
    rendition_request_fingerprint        TEXT COLLATE "C" NOT NULL,
    evidence_lexical_fingerprint         TEXT COLLATE "C" NOT NULL,
    captured_artifact_policy_fingerprint TEXT COLLATE "C" NOT NULL,
    execution_identity_fingerprint        TEXT COLLATE "C" NOT NULL,
    execution_identity_json               TEXT COLLATE "C" NOT NULL,
    execution_snapshot_json               TEXT COLLATE "C",
    captured_artifact_policy_json        TEXT COLLATE "C" NOT NULL,
    state                                TEXT COLLATE "C" NOT NULL,
    phase                                TEXT COLLATE "C" NOT NULL,
    claim_owner                          TEXT COLLATE "C",
    claim_epoch                          BIGINT NOT NULL DEFAULT 0,
    lease_expires_at                     TEXT COLLATE "C",
    available_at                         TEXT COLLATE "C" NOT NULL,
    provider_started                     BIGINT NOT NULL DEFAULT 0,
    provider_attempts                    BIGINT NOT NULL DEFAULT 0,
    provider_resume_handle               TEXT COLLATE "C",
    selected_waiter_id                   TEXT COLLATE "C",
    authorization_grant_id               TEXT COLLATE "C",
    authorization_incarnation_id         TEXT COLLATE "C",
    authorization_revocation_fence       BIGINT,
    lexical_generation_id                TEXT COLLATE "C",
    failure_code                         TEXT COLLATE "C",
    created_at                           TEXT COLLATE "C" NOT NULL,
    updated_at                           TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.rendition_job_waiters (
    waiter_id                TEXT COLLATE "C" PRIMARY KEY,
    job_id                   TEXT COLLATE "C" NOT NULL,
    content_version_id       TEXT COLLATE "C" NOT NULL,
    profile_fingerprint      TEXT COLLATE "C" NOT NULL,
    principal                TEXT COLLATE "C" NOT NULL,
    scope                    TEXT COLLATE "C" NOT NULL,
    disclosure_fingerprint   TEXT COLLATE "C" NOT NULL,
    input_classes_json       TEXT COLLATE "C" NOT NULL,
    retained_classes_json    TEXT COLLATE "C" NOT NULL,
    authorization_grant_id   TEXT COLLATE "C" NOT NULL,
    authorization_incarnation_id TEXT COLLATE "C" NOT NULL,
    authorization_revocation_fence BIGINT NOT NULL,
    state                    TEXT COLLATE "C" NOT NULL,
    failure_code             TEXT COLLATE "C",
    attachment_id            TEXT COLLATE "C" NOT NULL,
    created_at               TEXT COLLATE "C" NOT NULL,
    updated_at               TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.current_rendition_roots (
    root_id       TEXT COLLATE "C" PRIMARY KEY,
    root_kind     TEXT COLLATE "C" NOT NULL,
    target_kind   TEXT COLLATE "C" NOT NULL,
    target_id     TEXT COLLATE "C" NOT NULL,
    fencing_token BIGINT NOT NULL,
    recorded_at   TEXT COLLATE "C" NOT NULL,
    expires_at    TEXT COLLATE "C",
    active        BIGINT NOT NULL,
    released_at   TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.batch_tag_receipts (
    operation_id  TEXT COLLATE "C" PRIMARY KEY,
    request_digest TEXT COLLATE "C" NOT NULL,
    receipt_json  BYTEA NOT NULL
);

CREATE TABLE {{schema}}.saved_queries (
    id          TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    name        TEXT COLLATE "C" NOT NULL UNIQUE,
    description TEXT COLLATE "C" NOT NULL,
    kind        TEXT COLLATE "C" NOT NULL,
    payload     BYTEA NOT NULL,
    fingerprint TEXT COLLATE "C" NOT NULL,
    revision    BIGINT NOT NULL DEFAULT 1,
    created_at  TEXT COLLATE "C" NOT NULL,
    updated_at  TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.saved_query_runs (
    run_id                     TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    saved_query_id             TEXT COLLATE "C" NOT NULL,
    saved_query_revision       BIGINT NOT NULL,
    query_fingerprint          TEXT COLLATE "C" NOT NULL,
    snapshot_id                TEXT COLLATE "C" NOT NULL,
    member_hash                TEXT COLLATE "C" NOT NULL,
    total                      BIGINT NOT NULL,
    total_bytes                BIGINT NOT NULL,
    ran_at                     TEXT COLLATE "C" NOT NULL,
    expires_at                 TEXT COLLATE "C" NOT NULL,
    previous_run_id            TEXT COLLATE "C",
    previous_member_hash       TEXT COLLATE "C",
    previous_total             BIGINT,
    previous_query_fingerprint TEXT COLLATE "C",
    photos                     BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE {{schema}}.term_report_history (
    id           TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    parent_id    TEXT COLLATE "C" NOT NULL,
    observed_at  TEXT COLLATE "C" NOT NULL,
    request_json BYTEA NOT NULL,
    summary_json BYTEA NOT NULL
);

CREATE TABLE {{schema}}.extracted_text (
    id                BIGINT PRIMARY KEY,
    blob_hash         TEXT COLLATE "C" NOT NULL,
    extractor         TEXT COLLATE "C" NOT NULL,
    extractor_version BIGINT NOT NULL,
    status            TEXT COLLATE "C" NOT NULL,
    error             TEXT COLLATE "C",
    attempts          BIGINT NOT NULL DEFAULT 0,
    text              TEXT COLLATE "C",
    extracted_at      TEXT COLLATE "C" NOT NULL,
    UNIQUE (blob_hash, extractor)
);

CREATE TABLE {{schema}}.email_generations (
    generation_id TEXT COLLATE "C" PRIMARY KEY,
    source_sha256 TEXT COLLATE "C" NOT NULL,
    source_size BIGINT NOT NULL,
    recipe_fingerprint TEXT COLLATE "C" NOT NULL,
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    UNIQUE (source_sha256, source_size, recipe_fingerprint)
);

CREATE TABLE {{schema}}.email_part_artifacts (
    generation_id TEXT COLLATE "C" NOT NULL,
    part_path TEXT COLLATE "C" NOT NULL,
    role TEXT COLLATE "C" NOT NULL,
    blob_hash TEXT COLLATE "C" NOT NULL,
    size BIGINT NOT NULL,
    PRIMARY KEY (generation_id, part_path, role)
);

CREATE TABLE {{schema}}.email_attachments (
    attachment_id TEXT COLLATE "C" PRIMARY KEY,
    content_version_id TEXT COLLATE "C" NOT NULL,
    generation_id TEXT COLLATE "C" NOT NULL,
    attached_at TEXT COLLATE "C" NOT NULL,
    UNIQUE (content_version_id, generation_id),
    UNIQUE (content_version_id, attachment_id)
);

CREATE TABLE {{schema}}.email_heads (
    content_version_id TEXT COLLATE "C" PRIMARY KEY,
    attachment_id TEXT COLLATE "C" NOT NULL,
    published_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.email_body_results (
    email_attachment_id TEXT COLLATE "C" PRIMARY KEY,
    body_recipe_fingerprint TEXT COLLATE "C" NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    part_path TEXT COLLATE "C",
    rendition_attachment_id TEXT COLLATE "C",
    reason TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.email_document_publications (
    operation_id TEXT COLLATE "C" PRIMARY KEY,
    request_digest TEXT COLLATE "C" NOT NULL,
    parent_version_id TEXT COLLATE "C" NOT NULL,
    email_attachment_id TEXT COLLATE "C" NOT NULL,
    request_json TEXT COLLATE "C" NOT NULL,
    receipt_json TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.email_document_relations (
    operation_id TEXT COLLATE "C" NOT NULL,
    occurrence_order BIGINT NOT NULL,
    child_version_id TEXT COLLATE "C",
    PRIMARY KEY(operation_id, occurrence_order)
);

CREATE TABLE {{schema}}.photo_set_members (
    set_id TEXT COLLATE "C" NOT NULL,
    asset_id TEXT COLLATE "C" NOT NULL,
    added_at TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (set_id,asset_id)
);

CREATE TABLE {{schema}}.persons (
    key_face_id TEXT COLLATE "C",
    person_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    display_name TEXT COLLATE "C" NOT NULL,
    display_name_folded TEXT COLLATE "C" NOT NULL,
    origin TEXT COLLATE "C" NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TEXT COLLATE "C" NOT NULL,
    updated_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.person_identities (
    identity_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    person_id TEXT COLLATE "C" NOT NULL,
    kind TEXT COLLATE "C" NOT NULL,
    value_normalized TEXT COLLATE "C" NOT NULL,
    value_display TEXT COLLATE "C" NOT NULL,
    scope_kind TEXT COLLATE "C" NOT NULL,
    scope_value TEXT COLLATE "C" NOT NULL,
    normalization TEXT COLLATE "C" NOT NULL,
    origin TEXT COLLATE "C" NOT NULL,
    evidence_kind TEXT COLLATE "C" NOT NULL,
    evidence_id TEXT COLLATE "C" NOT NULL,
    confidence TEXT COLLATE "C" NOT NULL,
    recorded_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.person_external_identities (
    person_id TEXT COLLATE "C" NOT NULL,
    system TEXT COLLATE "C" NOT NULL,
    archive_id TEXT COLLATE "C" NOT NULL,
    uid TEXT COLLATE "C" NOT NULL,
    uid_kind TEXT COLLATE "C" NOT NULL,
    uid_state TEXT COLLATE "C" NOT NULL,
    last_seen_revision BIGINT,
    display_name_snapshot TEXT COLLATE "C" NOT NULL,
    linked_at TEXT COLLATE "C" NOT NULL,
    updated_at TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (system, archive_id, uid)
);

CREATE TABLE {{schema}}.person_external_uid_aliases (
    system TEXT COLLATE "C" NOT NULL,
    archive_id TEXT COLLATE "C" NOT NULL,
    retired_uid TEXT COLLATE "C" NOT NULL,
    surviving_uid TEXT COLLATE "C" NOT NULL,
    observed_at TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (system, archive_id, retired_uid)
);

CREATE TABLE {{schema}}.person_aliases (
    retired_person_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    surviving_person_id TEXT COLLATE "C",
    reason TEXT COLLATE "C" NOT NULL,
    retired_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.person_merges (
    merge_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    operation_id TEXT COLLATE "C" NOT NULL UNIQUE,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    survivor_person_id TEXT COLLATE "C" NOT NULL,
    absorbed_person_id TEXT COLLATE "C" NOT NULL,
    absorbed_display_name TEXT COLLATE "C" NOT NULL,
    moved_json BYTEA NOT NULL,
    survivor_revision_before BIGINT NOT NULL,
    survivor_revision_after BIGINT NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.custodian_assignments (
    assignment_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    scope_kind TEXT COLLATE "C" NOT NULL,
    ingest_id TEXT COLLATE "C",
    package_id TEXT COLLATE "C",
    package_record_id TEXT COLLATE "C",
    node_id BIGINT,
    content_version_id TEXT COLLATE "C",
    person_id TEXT COLLATE "C",
    raw_label TEXT COLLATE "C" NOT NULL,
    raw_label_folded TEXT COLLATE "C" NOT NULL,
    rank TEXT COLLATE "C" NOT NULL,
    basis TEXT COLLATE "C" NOT NULL,
    source_ref TEXT COLLATE "C" NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    recorded_at TEXT COLLATE "C" NOT NULL,
    retired_at TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.person_document_assertions (
    photo_mark BIGINT NOT NULL DEFAULT 0,
    assertion_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    content_version_id TEXT COLLATE "C" NOT NULL,
    person_id TEXT COLLATE "C" NOT NULL,
    role TEXT COLLATE "C" NOT NULL,
    action TEXT COLLATE "C" NOT NULL,
    note TEXT COLLATE "C" NOT NULL,
    recorded_at TEXT COLLATE "C" NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    UNIQUE (content_version_id, person_id, role)
);

CREATE TABLE {{schema}}.photo_faces (
    roll DOUBLE PRECISION,
    facing DOUBLE PRECISION,
    eyes_open BIGINT,
    smile BIGINT,
    detector_fingerprint TEXT COLLATE "C",
    face_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    content_version_id TEXT COLLATE "C" NOT NULL,
    x DOUBLE PRECISION NOT NULL,
    y DOUBLE PRECISION NOT NULL,
    width DOUBLE PRECISION NOT NULL,
    height DOUBLE PRECISION NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    assertion_id TEXT COLLATE "C",
    created_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.person_match_candidates (
    candidate_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    actor_key TEXT COLLATE "C" NOT NULL,
    display_name TEXT COLLATE "C" NOT NULL,
    suggested_person_id TEXT COLLATE "C",
    reason TEXT COLLATE "C" NOT NULL,
    evidence_json BYTEA NOT NULL,
    evidence_sha256 TEXT COLLATE "C" NOT NULL,
    occurrence_count BIGINT NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    state TEXT COLLATE "C" NOT NULL,
    decided_person_id TEXT COLLATE "C",
    created_at TEXT COLLATE "C" NOT NULL,
    decided_at TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.document_people_state (
    singleton BIGINT PRIMARY KEY,
    binding_epoch BIGINT NOT NULL,
    publication_epoch BIGINT NOT NULL DEFAULT 1,
    resolver_fingerprint TEXT COLLATE "C" NOT NULL DEFAULT '',
    updated_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.document_people_builds (
    operation_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    resolver_fingerprint TEXT COLLATE "C" NOT NULL,
    target_epoch BIGINT NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    scanned BIGINT NOT NULL DEFAULT 0,
    published BIGINT NOT NULL DEFAULT 0,
    failed BIGINT NOT NULL DEFAULT 0,
    started_at TEXT COLLATE "C" NOT NULL,
    updated_at TEXT COLLATE "C" NOT NULL,
    finished_at TEXT COLLATE "C" NOT NULL DEFAULT ''
);

CREATE TABLE {{schema}}.document_people_generations (
    generation_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    content_version_id TEXT COLLATE "C" NOT NULL,
    inputs_sha256 TEXT COLLATE "C" NOT NULL,
    resolver_fingerprint TEXT COLLATE "C" NOT NULL,
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.document_people_heads (
    content_version_id TEXT COLLATE "C" PRIMARY KEY,
    event_generation_id TEXT COLLATE "C" NOT NULL,
    inputs_sha256 TEXT COLLATE "C" NOT NULL,
    generation_id TEXT COLLATE "C" NOT NULL,
    unresolved_actors BIGINT NOT NULL,
    suppressed_actors BIGINT NOT NULL,
    candidate_overflow BIGINT NOT NULL,
    resolver_fingerprint TEXT COLLATE "C" NOT NULL,
    binding_epoch BIGINT NOT NULL,
    node_revision BIGINT NOT NULL,
    edge_count BIGINT NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    failure_reason TEXT COLLATE "C" NOT NULL,
    published_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.document_people (
    content_version_id TEXT COLLATE "C" NOT NULL,
    node_id BIGINT NOT NULL,
    person_id TEXT COLLATE "C" NOT NULL,
    generation_id TEXT COLLATE "C" NOT NULL,
    role TEXT COLLATE "C" NOT NULL,
    actor_key TEXT COLLATE "C" NOT NULL,
    evidence_kind TEXT COLLATE "C" NOT NULL,
    evidence_id TEXT COLLATE "C" NOT NULL,
    confidence TEXT COLLATE "C" NOT NULL,
    basis TEXT COLLATE "C" NOT NULL,
    raw_label TEXT COLLATE "C" NOT NULL,
    claim_count BIGINT NOT NULL,
    first_axis_key TEXT COLLATE "C",
    last_axis_key TEXT COLLATE "C",
    sensitive BIGINT NOT NULL,
    PRIMARY KEY (content_version_id, person_id, role, actor_key, evidence_kind, evidence_id)
);

CREATE TABLE {{schema}}.person_splits (
    operation_id TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    request_sha256 TEXT COLLATE "C" NOT NULL,
    receipt_json BYTEA NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.mailbox_containers (
    id TEXT COLLATE "C" PRIMARY KEY,
    owner TEXT COLLATE "C" NOT NULL,
    sha256 TEXT COLLATE "C" NOT NULL,
    size BIGINT NOT NULL,
    format TEXT COLLATE "C" NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    manifest_sha256 TEXT COLLATE "C" NOT NULL DEFAULT ''
);

CREATE TABLE {{schema}}.mailbox_chunks (
    container_id TEXT COLLATE "C" NOT NULL,
    chunk_index BIGINT NOT NULL,
    blob_hash TEXT COLLATE "C" NOT NULL,
    size BIGINT NOT NULL,
    PRIMARY KEY(container_id,chunk_index)
);

CREATE TABLE {{schema}}.mailbox_archives (
    id TEXT COLLATE "C" PRIMARY KEY,
    owner TEXT COLLATE "C" NOT NULL,
    description TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.mailbox_transfer_receipts (
    id TEXT COLLATE "C" PRIMARY KEY,
    archive_id TEXT COLLATE "C" NOT NULL,
    source_ref TEXT COLLATE "C" NOT NULL,
    target_version_id TEXT COLLATE "C" NOT NULL,
    document_publication_id TEXT COLLATE "C" NOT NULL,
    receipt_json TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.mailbox_jobs (
    id TEXT COLLATE "C" PRIMARY KEY,
    owner TEXT COLLATE "C" NOT NULL,
    container_id TEXT COLLATE "C" NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    claim TEXT COLLATE "C" NOT NULL,
    job_json TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.mailbox_occurrences (
    job_id TEXT COLLATE "C" NOT NULL,
    ordinal BIGINT NOT NULL,
    receipt_id TEXT COLLATE "C",
    occurrence_json TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY(job_id,ordinal)
);

CREATE TABLE {{schema}}.photo_quality_signals (
    content_version_id TEXT COLLATE "C" NOT NULL,
    evaluator_fingerprint TEXT COLLATE "C" NOT NULL,
    state TEXT COLLATE "C" NOT NULL,
    focus DOUBLE PRECISION,
    blur DOUBLE PRECISION,
    brightness DOUBLE PRECISION,
    color_red DOUBLE PRECISION,
    color_green DOUBLE PRECISION,
    color_blue DOUBLE PRECISION,
    framing DOUBLE PRECISION,
    aesthetics DOUBLE PRECISION,
    face_reports_json TEXT COLLATE "C",
    face_inputs TEXT COLLATE "C" NOT NULL DEFAULT '',
    PRIMARY KEY(content_version_id,evaluator_fingerprint)
);

CREATE TABLE {{schema}}.photo_hidden_credentials (
    singleton BIGINT PRIMARY KEY,
    passcode_hash TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.photo_hidden_lockout (
    singleton BIGINT PRIMARY KEY,
    locked_until TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.photo_hidden_failures (
    failure_id BIGINT PRIMARY KEY,
    occurred_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.person_candidate_faces (
    candidate_id TEXT COLLATE "C" NOT NULL,
    face_id TEXT COLLATE "C" NOT NULL,
    content_version_id TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY(candidate_id,face_id,content_version_id)
);

CREATE TABLE {{schema}}.photo_checkouts (
    checkout_id TEXT COLLATE "C" PRIMARY KEY,
    selection_json TEXT COLLATE "C" NOT NULL,
    skipped_photos_json TEXT COLLATE "C" NOT NULL DEFAULT '[]',
    skipped_photo_count BIGINT NOT NULL DEFAULT 0,
    byte_limit BIGINT NOT NULL,
    created_at TEXT COLLATE "C" NOT NULL,
    destination TEXT COLLATE "C",
    operation_id TEXT COLLATE "C",
    last_operation_id TEXT COLLATE "C",
    destination_identity TEXT COLLATE "C" NOT NULL DEFAULT '',
    destination_reserved BIGINT NOT NULL DEFAULT 0,
    retirement_requested BIGINT NOT NULL DEFAULT 0,
    state TEXT COLLATE "C" NOT NULL DEFAULT 'unbound',
    error TEXT COLLATE "C" NOT NULL DEFAULT '',
    retention_released BIGINT NOT NULL DEFAULT 0,
    retired_at TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.photo_checkout_entries (
    checkout_id TEXT COLLATE "C" NOT NULL,
    file_id TEXT COLLATE "C" NOT NULL,
    asset_id TEXT COLLATE "C" NOT NULL,
    version_id TEXT COLLATE "C" NOT NULL,
    blob_hash TEXT COLLATE "C" NOT NULL,
    sidecar_of_file_id TEXT COLLATE "C",
    size BIGINT NOT NULL,
    relative_path TEXT COLLATE "C" NOT NULL,
    published BIGINT NOT NULL DEFAULT 0,
    base_held BIGINT NOT NULL DEFAULT 0,
    observed_identity TEXT COLLATE "C" NOT NULL DEFAULT '',
    observed_hash TEXT COLLATE "C" NOT NULL DEFAULT '',
    observed_size BIGINT NOT NULL DEFAULT 0,
    observed_mtime TEXT COLLATE "C" NOT NULL DEFAULT '',
    observed_since TEXT COLLATE "C" NOT NULL DEFAULT '',
    working_state TEXT COLLATE "C" NOT NULL DEFAULT 'unobserved',
    PRIMARY KEY (checkout_id,file_id)
);

CREATE TABLE {{schema}}.photo_suggestions (
    id                   BIGINT PRIMARY KEY,
    suggestion_id        TEXT COLLATE "C" NOT NULL UNIQUE,
    content_version_id   TEXT COLLATE "C" NOT NULL,
    producer_fingerprint TEXT COLLATE "C" NOT NULL,
    rotation             BIGINT,
    caption              TEXT COLLATE "C" NOT NULL DEFAULT '',
    tags_json            TEXT COLLATE "C" NOT NULL DEFAULT '[]',
    visible_text         TEXT COLLATE "C" NOT NULL DEFAULT '',
    dismissed            BIGINT NOT NULL DEFAULT 0,
    caption_resolved     BIGINT NOT NULL DEFAULT 0,
    resolved_tags_json   TEXT COLLATE "C" NOT NULL DEFAULT '[]',
    awaiting             BIGINT NOT NULL DEFAULT 0,
    text_settled         BIGINT NOT NULL DEFAULT 0,
    created_at           TEXT COLLATE "C" NOT NULL,
    UNIQUE (content_version_id, producer_fingerprint)
);

ALTER TABLE {{schema}}.export_chunks ADD FOREIGN KEY (source_id) REFERENCES {{schema}}.export_sources(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.export_members ADD FOREIGN KEY (source_id) REFERENCES {{schema}}.export_sources(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.export_members ADD FOREIGN KEY (version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.export_members ADD FOREIGN KEY (blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.export_plans ADD FOREIGN KEY (source_id) REFERENCES {{schema}}.export_sources(id);

ALTER TABLE {{schema}}.export_documents ADD FOREIGN KEY (plan_id) REFERENCES {{schema}}.export_plans(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.export_role_roots ADD FOREIGN KEY (plan_id) REFERENCES {{schema}}.export_plans(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.export_role_roots ADD FOREIGN KEY (blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.export_jobs ADD FOREIGN KEY (plan_id) REFERENCES {{schema}}.export_plans(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.collection_snapshots ADD FOREIGN KEY (vault_uid) REFERENCES {{schema}}.vault_metadata(vault_uid);

ALTER TABLE {{schema}}.collection_snapshots ADD FOREIGN KEY (predecessor_id) REFERENCES {{schema}}.collection_snapshots(snapshot_id);

ALTER TABLE {{schema}}.collection_snapshot_members ADD FOREIGN KEY (snapshot_id) REFERENCES {{schema}}.collection_snapshots(snapshot_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.collection_snapshot_members ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.collection_snapshot_representations ADD FOREIGN KEY (snapshot_id, occurrence_id)
        REFERENCES {{schema}}.collection_snapshot_members(snapshot_id, occurrence_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.packages ADD FOREIGN KEY (snapshot_id) REFERENCES {{schema}}.collection_snapshots(snapshot_id);

ALTER TABLE {{schema}}.packages ADD FOREIGN KEY (predecessor_package_id) REFERENCES {{schema}}.packages(package_id);

ALTER TABLE {{schema}}.packages ADD FOREIGN KEY (ingest_id) REFERENCES {{schema}}.ingests(id);

ALTER TABLE {{schema}}.package_volumes ADD FOREIGN KEY (package_id) REFERENCES {{schema}}.packages(package_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.bates_namespace_cursors ADD FOREIGN KEY (namespace_id) REFERENCES {{schema}}.bates_namespaces(namespace_id);

ALTER TABLE {{schema}}.bates_allocations ADD FOREIGN KEY (namespace_id) REFERENCES {{schema}}.bates_namespaces(namespace_id);

ALTER TABLE {{schema}}.bates_allocations ADD FOREIGN KEY (snapshot_id) REFERENCES {{schema}}.collection_snapshots(snapshot_id);

ALTER TABLE {{schema}}.bates_page_labels ADD FOREIGN KEY (allocation_id) REFERENCES {{schema}}.bates_allocations(allocation_id);

ALTER TABLE {{schema}}.bates_page_labels ADD FOREIGN KEY (namespace_id) REFERENCES {{schema}}.bates_namespaces(namespace_id);

ALTER TABLE {{schema}}.bates_artifacts ADD FOREIGN KEY (allocation_id) REFERENCES {{schema}}.bates_allocations(allocation_id);

ALTER TABLE {{schema}}.bates_artifacts ADD FOREIGN KEY (blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.bates_artifact_pages ADD FOREIGN KEY (artifact_id) REFERENCES {{schema}}.bates_artifacts(artifact_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.package_records ADD FOREIGN KEY (package_id) REFERENCES {{schema}}.packages(package_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.package_labels ADD FOREIGN KEY (package_id) REFERENCES {{schema}}.packages(package_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.package_labels ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.package_import_jobs ADD FOREIGN KEY (package_id) REFERENCES {{schema}}.packages(package_id);

ALTER TABLE {{schema}}.package_import_receipts ADD FOREIGN KEY (package_id) REFERENCES {{schema}}.packages(package_id);

ALTER TABLE {{schema}}.package_import_receipts ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.page_documents ADD FOREIGN KEY (version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.page_frames ADD FOREIGN KEY (version_id) REFERENCES {{schema}}.page_documents(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.page_images ADD FOREIGN KEY (recipe_sha256) REFERENCES {{schema}}.page_recipes(checksum);

ALTER TABLE {{schema}}.page_images ADD FOREIGN KEY (blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.page_images ADD FOREIGN KEY(version_id,page) REFERENCES {{schema}}.page_frames(version_id,page) ON DELETE CASCADE;

ALTER TABLE {{schema}}.page_render_jobs ADD FOREIGN KEY (node_id) REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.page_render_jobs ADD FOREIGN KEY (version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.storage_operations ADD FOREIGN KEY (source_store_id) REFERENCES {{schema}}.blob_stores(store_id) ON DELETE SET NULL;

ALTER TABLE {{schema}}.media_source_versions ADD FOREIGN KEY (source_id) REFERENCES {{schema}}.media_sources(source_id);

ALTER TABLE {{schema}}.media_source_versions ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.media_occurrences ADD FOREIGN KEY (source_id) REFERENCES {{schema}}.media_sources(source_id);

ALTER TABLE {{schema}}.media_occurrences ADD FOREIGN KEY (source_version_id) REFERENCES {{schema}}.media_source_versions(source_version_id);

ALTER TABLE {{schema}}.media_input_artifacts ADD FOREIGN KEY (occurrence_id) REFERENCES {{schema}}.media_occurrences(occurrence_id);

ALTER TABLE {{schema}}.media_input_artifacts ADD FOREIGN KEY (source_id) REFERENCES {{schema}}.media_sources(source_id);

ALTER TABLE {{schema}}.media_input_artifacts ADD FOREIGN KEY (source_version_id) REFERENCES {{schema}}.media_source_versions(source_version_id);

ALTER TABLE {{schema}}.media_input_artifacts ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.media_operations ADD FOREIGN KEY (source_id) REFERENCES {{schema}}.media_sources(source_id);

ALTER TABLE {{schema}}.visual_preview_generations ADD FOREIGN KEY (vault_uid) REFERENCES {{schema}}.vault_metadata(vault_uid);

ALTER TABLE {{schema}}.visual_preview_generations ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.visual_preview_generations ADD FOREIGN KEY (source_sha256) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.visual_preview_generations ADD FOREIGN KEY (output_blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.visual_preview_heads ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.visual_preview_heads ADD FOREIGN KEY (content_version_id, generation_id)
        REFERENCES {{schema}}.visual_preview_generations(content_version_id, generation_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.collection_labels ADD FOREIGN KEY (ingest_id) REFERENCES {{schema}}.ingests(id);

ALTER TABLE {{schema}}.watch_sources ADD FOREIGN KEY (node_id) REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.push_sources ADD FOREIGN KEY (node_id) REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.push_sources ADD FOREIGN KEY (provenance_identity) REFERENCES {{schema}}.provenance(identity) ON DELETE CASCADE;

ALTER TABLE {{schema}}.current_processing_incarnation ADD FOREIGN KEY (incarnation_id) REFERENCES {{schema}}.processing_incarnations(incarnation_id);

ALTER TABLE {{schema}}.processing_consent_grants ADD FOREIGN KEY (vault_uid) REFERENCES {{schema}}.vault_metadata(vault_uid);

ALTER TABLE {{schema}}.processing_consent_grants ADD FOREIGN KEY (incarnation_id) REFERENCES {{schema}}.processing_incarnations(incarnation_id);

ALTER TABLE {{schema}}.processing_consent_revocations ADD FOREIGN KEY (vault_uid) REFERENCES {{schema}}.vault_metadata(vault_uid);

ALTER TABLE {{schema}}.processing_consent_revocations ADD FOREIGN KEY (incarnation_id) REFERENCES {{schema}}.processing_incarnations(incarnation_id);

ALTER TABLE {{schema}}.rendition_builds ADD FOREIGN KEY (vault_uid) REFERENCES {{schema}}.vault_metadata(vault_uid);

ALTER TABLE {{schema}}.rendition_builds ADD FOREIGN KEY (source_sha256) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.rendition_artifacts ADD FOREIGN KEY (build_id) REFERENCES {{schema}}.rendition_builds(build_id);

ALTER TABLE {{schema}}.rendition_artifacts ADD FOREIGN KEY (blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.rendition_units ADD FOREIGN KEY (build_id) REFERENCES {{schema}}.rendition_builds(build_id);

ALTER TABLE {{schema}}.rendition_lexical_segments ADD FOREIGN KEY (build_id, unit_id)
        REFERENCES {{schema}}.rendition_units(build_id, unit_id);

ALTER TABLE {{schema}}.rendition_attachments ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.rendition_attachments ADD FOREIGN KEY (profile_fingerprint) REFERENCES {{schema}}.processing_profiles(profile_fingerprint);

ALTER TABLE {{schema}}.rendition_attachments ADD FOREIGN KEY (vault_uid, build_id)
        REFERENCES {{schema}}.rendition_builds(vault_uid, build_id);

ALTER TABLE {{schema}}.rendition_heads ADD FOREIGN KEY (content_version_id, profile_fingerprint, attachment_id)
        REFERENCES {{schema}}.rendition_attachments(
            content_version_id, profile_fingerprint, attachment_id
        ) ON DELETE CASCADE;

ALTER TABLE {{schema}}.embedding_input_generations ADD FOREIGN KEY (generation_blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.embedding_input_generations ADD FOREIGN KEY (profile_fingerprint) REFERENCES {{schema}}.processing_profiles(profile_fingerprint);

ALTER TABLE {{schema}}.embedding_jobs ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.embedding_jobs ADD FOREIGN KEY (profile_fingerprint) REFERENCES {{schema}}.processing_profiles(profile_fingerprint);

ALTER TABLE {{schema}}.embedding_jobs ADD FOREIGN KEY (generation_id) REFERENCES {{schema}}.embedding_input_generations(generation_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.embedding_jobs ADD FOREIGN KEY (vector_space_id) REFERENCES {{schema}}.embedding_vector_spaces(vector_space_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.embedding_generation_inputs ADD FOREIGN KEY (generation_id) REFERENCES {{schema}}.embedding_input_generations(generation_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.embedding_vector_sets ADD FOREIGN KEY (vector_space_id) REFERENCES {{schema}}.embedding_vector_spaces(vector_space_id);

ALTER TABLE {{schema}}.embedding_vector_sets ADD FOREIGN KEY (payload_blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.embedding_vector_rows ADD FOREIGN KEY (vector_set_id) REFERENCES {{schema}}.embedding_vector_sets(vector_set_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.embedding_sets ADD FOREIGN KEY (vault_uid) REFERENCES {{schema}}.vault_metadata(vault_uid);

ALTER TABLE {{schema}}.embedding_sets ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.embedding_sets ADD FOREIGN KEY (profile_fingerprint) REFERENCES {{schema}}.processing_profiles(profile_fingerprint);

ALTER TABLE {{schema}}.embedding_sets ADD FOREIGN KEY (vector_space_id) REFERENCES {{schema}}.embedding_vector_spaces(vector_space_id);

ALTER TABLE {{schema}}.embedding_sets ADD FOREIGN KEY (input_generation_id) REFERENCES {{schema}}.embedding_input_generations(generation_id);

ALTER TABLE {{schema}}.embedding_sets ADD FOREIGN KEY (vector_set_id) REFERENCES {{schema}}.embedding_vector_sets(vector_set_id);

ALTER TABLE {{schema}}.embedding_heads ADD FOREIGN KEY (embedding_set_id) REFERENCES {{schema}}.embedding_sets(embedding_set_id);

ALTER TABLE {{schema}}.embedding_heads ADD FOREIGN KEY (vector_space_id) REFERENCES {{schema}}.embedding_vector_spaces(vector_space_id);

ALTER TABLE {{schema}}.embedding_heads ADD FOREIGN KEY (profile_fingerprint) REFERENCES {{schema}}.processing_profiles(profile_fingerprint);

ALTER TABLE {{schema}}.embedding_failures ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.embedding_failures ADD FOREIGN KEY (profile_fingerprint) REFERENCES {{schema}}.processing_profiles(profile_fingerprint);

ALTER TABLE {{schema}}.rendition_lexical_index ADD FOREIGN KEY (build_id) REFERENCES {{schema}}.rendition_builds(build_id);

ALTER TABLE {{schema}}.rendition_lexical_generation_manifests ADD FOREIGN KEY (generation_id) REFERENCES {{schema}}.rendition_lexical_generations(generation_id);

ALTER TABLE {{schema}}.rendition_lexical_generation_builds ADD FOREIGN KEY (generation_id) REFERENCES {{schema}}.rendition_lexical_generations(generation_id)
        ON DELETE CASCADE;

ALTER TABLE {{schema}}.rendition_lexical_generation_builds ADD FOREIGN KEY (build_id) REFERENCES {{schema}}.rendition_builds(build_id);

ALTER TABLE {{schema}}.rendition_lexical_heads ADD FOREIGN KEY (generation_id) REFERENCES {{schema}}.rendition_lexical_generations(generation_id);

ALTER TABLE {{schema}}.rendition_lexical_superseded ADD FOREIGN KEY (generation_id) REFERENCES {{schema}}.rendition_lexical_generations(generation_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.rendition_jobs ADD FOREIGN KEY (vault_uid) REFERENCES {{schema}}.vault_metadata(vault_uid);

ALTER TABLE {{schema}}.rendition_jobs ADD FOREIGN KEY (source_sha256) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.rendition_job_waiters ADD FOREIGN KEY (job_id) REFERENCES {{schema}}.rendition_jobs(job_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.rendition_job_waiters ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.rendition_job_waiters ADD FOREIGN KEY (profile_fingerprint) REFERENCES {{schema}}.processing_profiles(profile_fingerprint);

ALTER TABLE {{schema}}.saved_query_runs ADD FOREIGN KEY (saved_query_id) REFERENCES {{schema}}.saved_queries(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.email_part_artifacts ADD FOREIGN KEY (generation_id) REFERENCES {{schema}}.email_generations(generation_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.email_part_artifacts ADD FOREIGN KEY (blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.email_attachments ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.email_attachments ADD FOREIGN KEY (generation_id) REFERENCES {{schema}}.email_generations(generation_id);

ALTER TABLE {{schema}}.email_heads ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.email_heads ADD FOREIGN KEY (content_version_id, attachment_id)
        REFERENCES {{schema}}.email_attachments(content_version_id, attachment_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.email_body_results ADD FOREIGN KEY (email_attachment_id) REFERENCES {{schema}}.email_attachments(attachment_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.email_body_results ADD FOREIGN KEY (rendition_attachment_id) REFERENCES {{schema}}.rendition_attachments(attachment_id);

ALTER TABLE {{schema}}.email_document_publications ADD FOREIGN KEY (parent_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.email_document_publications ADD FOREIGN KEY (email_attachment_id) REFERENCES {{schema}}.email_attachments(attachment_id);

ALTER TABLE {{schema}}.email_document_relations ADD FOREIGN KEY (operation_id) REFERENCES {{schema}}.email_document_publications(operation_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.email_document_relations ADD FOREIGN KEY (child_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.photo_set_members ADD FOREIGN KEY (set_id) REFERENCES {{schema}}.photo_sets(set_id);

ALTER TABLE {{schema}}.photo_set_members ADD FOREIGN KEY (asset_id) REFERENCES {{schema}}.photo_assets(asset_id);

ALTER TABLE {{schema}}.persons ADD FOREIGN KEY (key_face_id) REFERENCES {{schema}}.photo_faces(face_id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE {{schema}}.person_identities ADD FOREIGN KEY (person_id) REFERENCES {{schema}}.persons(person_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.person_external_identities ADD FOREIGN KEY (person_id) REFERENCES {{schema}}.persons(person_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.custodian_assignments ADD FOREIGN KEY (ingest_id) REFERENCES {{schema}}.ingests(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.custodian_assignments ADD FOREIGN KEY (node_id) REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.custodian_assignments ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.custodian_assignments ADD FOREIGN KEY (person_id) REFERENCES {{schema}}.persons(person_id) ON DELETE SET NULL;

ALTER TABLE {{schema}}.person_document_assertions ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.person_document_assertions ADD FOREIGN KEY (person_id) REFERENCES {{schema}}.persons(person_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.photo_faces ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.photo_faces ADD FOREIGN KEY (assertion_id) REFERENCES {{schema}}.person_document_assertions(assertion_id);

ALTER TABLE {{schema}}.document_people_generations ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.document_people_heads ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.document_people ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.document_people ADD FOREIGN KEY (node_id) REFERENCES {{schema}}.nodes(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.document_people ADD FOREIGN KEY (person_id) REFERENCES {{schema}}.persons(person_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.mailbox_chunks ADD FOREIGN KEY (container_id) REFERENCES {{schema}}.mailbox_containers(id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.mailbox_chunks ADD FOREIGN KEY (blob_hash) REFERENCES {{schema}}.blobs(hash);

ALTER TABLE {{schema}}.mailbox_transfer_receipts ADD FOREIGN KEY (archive_id) REFERENCES {{schema}}.mailbox_archives(id);

ALTER TABLE {{schema}}.mailbox_transfer_receipts ADD FOREIGN KEY (target_version_id) REFERENCES {{schema}}.content_versions(version_id);

ALTER TABLE {{schema}}.mailbox_transfer_receipts ADD FOREIGN KEY (document_publication_id) REFERENCES {{schema}}.email_document_publications(operation_id);

ALTER TABLE {{schema}}.mailbox_jobs ADD FOREIGN KEY (container_id) REFERENCES {{schema}}.mailbox_containers(id);

ALTER TABLE {{schema}}.mailbox_occurrences ADD FOREIGN KEY (job_id) REFERENCES {{schema}}.mailbox_jobs(id);

ALTER TABLE {{schema}}.mailbox_occurrences ADD FOREIGN KEY (receipt_id) REFERENCES {{schema}}.mailbox_transfer_receipts(id);

ALTER TABLE {{schema}}.photo_quality_signals ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.person_candidate_faces ADD FOREIGN KEY (candidate_id) REFERENCES {{schema}}.person_match_candidates(candidate_id) ON DELETE CASCADE;

ALTER TABLE {{schema}}.photo_checkout_entries ADD FOREIGN KEY (checkout_id) REFERENCES {{schema}}.photo_checkouts(checkout_id);

ALTER TABLE {{schema}}.photo_suggestions ADD FOREIGN KEY (content_version_id) REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE;

CREATE UNIQUE INDEX package_import_receipts_record ON {{schema}}.package_import_receipts(package_id, record_key);

CREATE UNIQUE INDEX one_primary_blob_store ON {{schema}}.blob_stores((1)) WHERE role = 'primary';

CREATE UNIQUE INDEX person_identities_identifier ON {{schema}}.person_identities(person_id, kind, scope_kind, scope_value, value_normalized) WHERE kind <> 'name_alias';

CREATE UNIQUE INDEX person_identities_alias ON {{schema}}.person_identities(person_id, value_normalized) WHERE kind = 'name_alias';

CREATE UNIQUE INDEX person_external_current ON {{schema}}.person_external_identities(person_id, system, archive_id) WHERE uid_state = 'current';

CREATE UNIQUE INDEX custodian_primary_collection ON {{schema}}.custodian_assignments(ingest_id) WHERE scope_kind='collection' AND rank='primary' AND retired_at IS NULL;

CREATE UNIQUE INDEX custodian_primary_package ON {{schema}}.custodian_assignments(package_id, package_record_id) WHERE scope_kind='package' AND rank='primary' AND retired_at IS NULL;

CREATE UNIQUE INDEX custodian_primary_document ON {{schema}}.custodian_assignments(content_version_id) WHERE scope_kind='document' AND rank='primary' AND retired_at IS NULL;

CREATE UNIQUE INDEX person_candidate_identity ON {{schema}}.person_match_candidates(actor_key, suggested_person_id, reason, evidence_sha256) WHERE suggested_person_id IS NOT NULL;

CREATE UNIQUE INDEX person_candidate_identity_unsuggested ON {{schema}}.person_match_candidates(actor_key, reason, evidence_sha256) WHERE suggested_person_id IS NULL;

CREATE TABLE {{schema}}.blob_locations (
    blob_hash    TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.blobs(hash) ON DELETE CASCADE,
    store_id     TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.blob_stores(store_id),
    generation   TEXT COLLATE "C" NOT NULL,
    kind         TEXT COLLATE "C" NOT NULL,
    encoding     TEXT COLLATE "C",
    stored_size  BIGINT NOT NULL,
    pack_eligible BIGINT NOT NULL,
    PRIMARY KEY (blob_hash, store_id)
);

CREATE TABLE {{schema}}.blob_packs (
    store_id     TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.blob_stores(store_id),
    pack_id      TEXT COLLATE "C" NOT NULL,
    entry_count  BIGINT NOT NULL,
    stored_bytes BIGINT NOT NULL,
    created_at   TEXT COLLATE "C" NOT NULL,
    scan_hash             TEXT COLLATE "C" NOT NULL DEFAULT '',
    live_entries          BIGINT NOT NULL DEFAULT 0,
    live_stored_bytes     BIGINT NOT NULL DEFAULT 0,
    live_raw_bytes        BIGINT NOT NULL DEFAULT 0,
    max_live_stored_len   BIGINT NOT NULL DEFAULT 0,
    max_live_raw_len      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (store_id, pack_id)
);

CREATE TABLE {{schema}}.blob_pack_entries (
    blob_hash   TEXT COLLATE "C" NOT NULL,
    store_id    TEXT COLLATE "C" NOT NULL,
    pack_id     TEXT COLLATE "C" NOT NULL,
    pack_offset BIGINT NOT NULL,
    stored_len  BIGINT NOT NULL,
    raw_len     BIGINT NOT NULL,
    flags       BIGINT NOT NULL,
    crc32c      BIGINT NOT NULL,
    PRIMARY KEY (blob_hash, store_id),
    FOREIGN KEY (store_id, pack_id)
        REFERENCES {{schema}}.blob_packs(store_id, pack_id) ON DELETE CASCADE
);

CREATE TABLE {{schema}}.rendition_blob_staging (
    blob_hash TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.blobs(hash) ON DELETE CASCADE
);

CREATE TABLE {{schema}}.derivative_blob_purge_pending (
    blob_hash TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.blobs(hash) ON DELETE CASCADE
);

CREATE TABLE {{schema}}.text_extraction_queue (
    blob_hash       TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.blobs(hash) ON DELETE CASCADE,
    next_attempt_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.text_searchable_versions (
    version_id TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE
);

CREATE TABLE {{schema}}.photo_timeline (
    file_id       TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.photo_files(file_id) ON DELETE CASCADE,
    sort_key      TEXT COLLATE "C" NOT NULL,
    day           TEXT COLLATE "C" NOT NULL,
    modified_time TEXT COLLATE "C",
    folder_key    TEXT COLLATE "C"
);

CREATE TABLE {{schema}}.photo_source_claims (
    file_id TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.photo_files(file_id) ON DELETE CASCADE,
    original_path TEXT COLLATE "C",
    original_mtime TEXT COLLATE "C",
    folder_key TEXT COLLATE "C"
);

CREATE INDEX blob_locations_store
    ON {{schema}}.blob_locations(store_id, blob_hash);

CREATE INDEX blob_pack_entries_pack
    ON {{schema}}.blob_pack_entries(store_id, pack_id, blob_hash);

CREATE INDEX blob_pack_entries_store_hash
    ON {{schema}}.blob_pack_entries(store_id, blob_hash);

CREATE INDEX blob_packs_dead_scan
ON {{schema}}.blob_packs(store_id, scan_hash, pack_id) WHERE live_entries=0;

CREATE INDEX blob_packs_live_scan
ON {{schema}}.blob_packs(store_id, scan_hash, pack_id) WHERE live_entries>0;

CREATE INDEX photo_timeline_sort_key ON {{schema}}.photo_timeline(sort_key);

CREATE INDEX photo_timeline_folder ON {{schema}}.photo_timeline(folder_key, sort_key) WHERE folder_key IS NOT NULL;

CREATE INDEX derivative_purge_suppressions_active_source
    ON {{schema}}.derivative_purge_suppressions(source_sha256, profile_fingerprint)
    WHERE active = 1;

-- Derived event catalogs invalidated by provenance changes.
CREATE TABLE {{schema}}.document_event_state (
    singleton BIGINT PRIMARY KEY, contract_version TEXT COLLATE "C" NOT NULL,
    deriver_fingerprint TEXT COLLATE "C" NOT NULL, input_epoch BIGINT NOT NULL,
    publication_epoch BIGINT NOT NULL, updated_at TEXT COLLATE "C" NOT NULL
);

CREATE TABLE {{schema}}.document_event_generations (
    generation_id TEXT COLLATE "C" PRIMARY KEY,
    content_version_id TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE,
    contract_version TEXT COLLATE "C" NOT NULL, deriver_fingerprint TEXT COLLATE "C" NOT NULL, inputs_sha256 TEXT COLLATE "C" NOT NULL,
    document_kind TEXT COLLATE "C" NOT NULL,
    canonical_json BYTEA NOT NULL,
    checksum TEXT COLLATE "C" NOT NULL, event_count BIGINT NOT NULL, created_at TEXT COLLATE "C" NOT NULL,
    UNIQUE (content_version_id, generation_id)
);

CREATE TABLE {{schema}}.document_event_heads (
    content_version_id TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE,
    generation_id TEXT COLLATE "C" NOT NULL UNIQUE REFERENCES {{schema}}.document_event_generations(generation_id) ON DELETE CASCADE,
    input_epoch BIGINT NOT NULL, published_at TEXT COLLATE "C" NOT NULL,
    FOREIGN KEY (content_version_id, generation_id)
        REFERENCES {{schema}}.document_event_generations(content_version_id, generation_id) ON DELETE CASCADE
);

CREATE TABLE {{schema}}.document_events (
    generation_id TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.document_event_generations(generation_id) ON DELETE CASCADE,
    event_id TEXT COLLATE "C" NOT NULL, source_key TEXT COLLATE "C" NOT NULL, date_kind TEXT COLLATE "C" NOT NULL,
    source_kind_raw TEXT COLLATE "C" NOT NULL, date_value TEXT COLLATE "C" NOT NULL, raw_value TEXT COLLATE "C" NOT NULL,
    precision TEXT COLLATE "C" NOT NULL, fraction_digits BIGINT NOT NULL,
    timezone_kind TEXT COLLATE "C" NOT NULL, zone_text TEXT COLLATE "C" NOT NULL, offset_seconds BIGINT,
    axis_key TEXT COLLATE "C" NOT NULL, utc_key TEXT COLLATE "C", claim_basis TEXT COLLATE "C" NOT NULL, parse_confidence TEXT COLLATE "C" NOT NULL,
    evidence_kind TEXT COLLATE "C" NOT NULL, evidence_id TEXT COLLATE "C" NOT NULL, evidence_sha256 TEXT COLLATE "C" NOT NULL,
    evidence_locator BYTEA NOT NULL, sensitive BIGINT NOT NULL,
    PRIMARY KEY (generation_id, event_id),
    UNIQUE (generation_id, source_key, date_kind)
);

CREATE TABLE {{schema}}.document_event_actors (
    generation_id TEXT COLLATE "C" NOT NULL, event_id TEXT COLLATE "C" NOT NULL, role TEXT COLLATE "C" NOT NULL,
    ordinal BIGINT NOT NULL, actor_key TEXT COLLATE "C" NOT NULL,
    display_name TEXT COLLATE "C" NOT NULL, address TEXT COLLATE "C" NOT NULL, claim_json BYTEA NOT NULL,
    evidence_kind TEXT COLLATE "C" NOT NULL, evidence_id TEXT COLLATE "C" NOT NULL,
    sensitive BIGINT NOT NULL,
    PRIMARY KEY (generation_id, event_id, role, ordinal),
    FOREIGN KEY (generation_id, event_id)
        REFERENCES {{schema}}.document_events(generation_id, event_id) ON DELETE CASCADE
);

CREATE TABLE {{schema}}.document_event_primaries (
    generation_id TEXT COLLATE "C" NOT NULL, scope_class TEXT COLLATE "C" NOT NULL, disclosure TEXT COLLATE "C" NOT NULL,
    event_id TEXT COLLATE "C" NOT NULL, rule_id TEXT COLLATE "C" NOT NULL, reason TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (generation_id, scope_class, disclosure),
    FOREIGN KEY (generation_id, event_id)
        REFERENCES {{schema}}.document_events(generation_id, event_id) ON DELETE CASCADE
);

CREATE TABLE {{schema}}.document_event_dirty (
    content_version_id TEXT COLLATE "C" PRIMARY KEY REFERENCES {{schema}}.content_versions(version_id) ON DELETE CASCADE,
    revision BIGINT NOT NULL, reason TEXT COLLATE "C" NOT NULL
);

-- Permanent document cleanup follows these references for retention checks and
-- cascades. PostgreSQL does not create indexes for foreign-key child columns.
CREATE INDEX nodes_trash_parent ON {{schema}}.nodes(trash_parent);
CREATE INDEX collection_snapshot_members_version ON {{schema}}.collection_snapshot_members(content_version_id);
CREATE INDEX export_members_version ON {{schema}}.export_members(version_id);
CREATE INDEX package_labels_version ON {{schema}}.package_labels(content_version_id);
CREATE INDEX package_import_receipts_version ON {{schema}}.package_import_receipts(content_version_id);
CREATE INDEX media_versions_content ON {{schema}}.media_source_versions(content_version_id, source_id);
CREATE INDEX media_input_artifacts_version ON {{schema}}.media_input_artifacts(content_version_id);
CREATE INDEX mailbox_transfer_receipts_version ON {{schema}}.mailbox_transfer_receipts(target_version_id);
CREATE INDEX page_render_jobs_node ON {{schema}}.page_render_jobs(node_id);
CREATE INDEX page_render_jobs_version ON {{schema}}.page_render_jobs(version_id);
CREATE INDEX rendition_job_waiters_job ON {{schema}}.rendition_job_waiters(job_id, state, waiter_id);
CREATE INDEX rendition_job_waiters_content_version ON {{schema}}.rendition_job_waiters(content_version_id, job_id);
CREATE INDEX embedding_sets_content_version ON {{schema}}.embedding_sets(content_version_id, embedding_set_id);
CREATE INDEX embedding_heads_set ON {{schema}}.embedding_heads(embedding_set_id);
CREATE INDEX email_body_results_rendition ON {{schema}}.email_body_results(rendition_attachment_id);
CREATE INDEX email_document_publications_attachment ON {{schema}}.email_document_publications(email_attachment_id);
CREATE INDEX email_document_publications_version ON {{schema}}.email_document_publications(parent_version_id);
CREATE INDEX email_document_relations_version ON {{schema}}.email_document_relations(child_version_id);
CREATE INDEX custodian_assignments_node ON {{schema}}.custodian_assignments(node_id);
CREATE INDEX custodian_assignments_version ON {{schema}}.custodian_assignments(content_version_id);
CREATE INDEX document_people_node ON {{schema}}.document_people(node_id);
CREATE INDEX document_people_generations_version ON {{schema}}.document_people_generations(content_version_id);
CREATE INDEX document_event_generations_version ON {{schema}}.document_event_generations(content_version_id);
CREATE INDEX photo_assets_display_file ON {{schema}}.photo_assets(display_file_id);
CREATE INDEX photo_assets_display_override_file ON {{schema}}.photo_assets(display_override_file_id);
CREATE INDEX photo_faces_version ON {{schema}}.photo_faces(content_version_id, face_id);
CREATE INDEX person_candidate_faces_version ON {{schema}}.person_candidate_faces(content_version_id, candidate_id);
CREATE INDEX photo_checkout_entries_version ON {{schema}}.photo_checkout_entries(version_id);

-- Exact cleanup survives removal of logical blob membership.
CREATE TABLE {{schema}}.gc_loose_retirements (
    store_id TEXT COLLATE "C" NOT NULL REFERENCES {{schema}}.blob_stores(store_id),
    blob_hash TEXT COLLATE "C" NOT NULL,
    loose_encoding BIGINT NOT NULL,
    PRIMARY KEY (store_id, blob_hash, loose_encoding)
);

CREATE TABLE {{schema}}.derivative_pack_purge_pending (
    store_id TEXT COLLATE "C" NOT NULL,
    pack_id TEXT COLLATE "C" NOT NULL,
    PRIMARY KEY (store_id, pack_id),
    FOREIGN KEY (store_id, pack_id) REFERENCES {{schema}}.blob_packs(store_id, pack_id) ON DELETE CASCADE
);
