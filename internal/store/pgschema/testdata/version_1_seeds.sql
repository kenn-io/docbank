-- Reproduce the inserts made by version 1 setup using fixed generated values.
-- Keep this fixture at version 1. Later migrations transform these rows.
INSERT INTO {{schema}}.vault_metadata (singleton, vault_uid, schema_version, schema_fingerprint)
VALUES (1, '00000000-0000-4000-8000-000000000001', 1, '{{fingerprint}}');

INSERT INTO {{schema}}.node_allocation (singleton, high_water) VALUES (1, 1);
INSERT INTO {{schema}}.vault_change_head (singleton, epoch)
VALUES (1, '00000000-0000-4000-8000-000000000002');

INSERT INTO {{schema}}.nodes (id, name, kind, created_at, modified_at)
VALUES (1, '', 'dir', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z');

INSERT INTO {{schema}}.document_people_state (singleton, binding_epoch, updated_at)
VALUES (1, 1, '2026-01-01T00:00:00.000000000Z');

INSERT INTO {{schema}}.processing_incarnations (incarnation_id, created_at)
VALUES ('00000000-0000-4000-8000-000000000003', '2026-01-01T00:00:00.000000000Z');

INSERT INTO {{schema}}.current_processing_incarnation (singleton, incarnation_id)
SELECT 1, incarnation_id FROM {{schema}}.processing_incarnations;

INSERT INTO {{schema}}.blob_stores (store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at)
VALUES ('00000000-0000-4000-8000-000000000004', 'primary', 'filesystem',
    'primary', 'active', 'builtin-primary', '00000000-0000-4000-8000-000000000005',
    '2026-01-01T00:00:00.000000000Z');
