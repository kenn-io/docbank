package store

import (
	"context"
	"errors"
)

// HasEmbeddingHeads reports retained vectors for a binding, including trashed documents.
func (s *Store) HasEmbeddingHeads(ctx context.Context, profile, binding string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_heads WHERE profile_fingerprint=$1 AND binding_id=$2)`, profile, binding).Scan(&exists)
	return exists, err
}

const liveProcessingTargetsSQL = `WITH RECURSIVE live(id) AS (
 SELECT id FROM nodes WHERE parent_id IS NULL AND trashed_at IS NULL
 UNION ALL SELECT n.id FROM nodes n JOIN live l ON n.parent_id=l.id WHERE n.trashed_at IS NULL
 ) SELECT n.id,v.version_id,n.name FROM nodes n JOIN live l ON l.id=n.id
 JOIN content_versions v ON v.version_id=n.current_version_id
`

// MissingProcessingTargetsAfter excludes published work at the selected
// profile, including after daemon restart. Failed text jobs remain eligible.
func (s *Store) MissingProcessingTargetsAfter(ctx context.Context, profile, after string, bindings, limit int) ([]DocumentSummary, error) {
	if bindings < 1 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid processing backfill bounds")
	}
	d := metadataJSONDialect{s.postgresSchema != ""}
	bindingRows := d.arrayValues(d.text("p.canonical_profile", "embeddings"))
	if d.postgres {
		bindingRows = d.arrayValues("p.embedding_bindings_json")
	}
	filter := ` WHERE n.trashed_at IS NULL AND v.version_id>$1
 AND (SELECT COUNT(DISTINCT binding_id) FROM (
 SELECT h.binding_id FROM embedding_heads h
 WHERE h.content_version_id=v.version_id AND h.profile_fingerprint=$2 AND h.input_kind<>'image'
 UNION SELECT ` + d.text("value", "name") + ` FROM processing_profiles p CROSS JOIN ` + bindingRows + `
 WHERE p.profile_fingerprint=$2 AND ` + d.text("value", "input_kind") + `='rendition_chunk'
 AND ` + emptyRenditionChunkSQL("v.version_id", "$2") + `
 ) settled) < $3
 ORDER BY v.version_id LIMIT $4`

	rows, err := s.db.QueryContext(ctx, liveProcessingTargetsSQL+filter, after, profile, bindings, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DocumentSummary
	for rows.Next() {
		var item DocumentSummary
		if err := rows.Scan(&item.NodeID, &item.ContentVersionID, &item.Name); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// An active omission-only build is the durable outcome for chunk bindings
// that have no text to send. Publication validates its empty evidence authority.
func emptyRenditionChunkSQL(version, profile string) string {
	return `EXISTS(SELECT 1 FROM rendition_heads rh
 JOIN rendition_attachments ra ON ra.attachment_id=rh.attachment_id
 JOIN rendition_builds rb ON rb.build_id=ra.build_id
 WHERE rh.content_version_id=` + version + ` AND rh.profile_fingerprint=` + profile + `
 AND rb.unit_count=0 AND rb.lexical_segment_count=0 AND rb.completeness='partial')`
}
