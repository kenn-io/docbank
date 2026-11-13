package store

import (
	"context"
	"errors"
	"fmt"
)

// HasEmbeddingHeads reports retained vectors for a binding, including trashed documents.
func (s *Store) HasEmbeddingHeads(ctx context.Context, profile, binding string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_heads WHERE profile_fingerprint=$1 AND binding_id=$2)`, profile, binding).Scan(&exists)
	return exists, err
}

const liveNodesSQL = `WITH RECURSIVE live(id) AS (
 SELECT id FROM nodes WHERE parent_id IS NULL AND trashed_at IS NULL
 UNION ALL SELECT n.id FROM nodes n JOIN live l ON n.parent_id=l.id WHERE n.trashed_at IS NULL
 ) `

const liveProcessingTargetsSQL = liveNodesSQL + `SELECT n.id,v.version_id,n.name FROM nodes n JOIN live l ON l.id=n.id
 JOIN content_versions v ON v.version_id=n.current_version_id
`

// MissingProcessingTargetsAfter excludes published work at the selected
// profile, including after daemon restart. Failed text jobs remain eligible.
func (s *Store) MissingProcessingTargetsAfter(ctx context.Context, profile, after string, bindings, limit int) ([]DocumentSummary, error) {
	if bindings < 1 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid processing backfill bounds")
	}
	rows, err := s.db.QueryContext(ctx, liveProcessingTargetsSQL+s.missingTargetsFilter(false, "", 0), after, profile, bindings, limit)
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

// WatchedRenditionTarget is a current version that still holds the bytes a
// watch last recorded for its source. MediaType and SourceRef let the caller
// resolve the watch's route the same way import admission does.
type WatchedRenditionTarget struct {
	DocumentSummary

	WatchName string
	SourceRef string
	MediaType string
}

// MissingWatchedRenditionTargetsAfter pages current versions with a rendition
// head for the profile whose node has a watch_sources row from one of the
// named watches with the version's hash and size. Only rendition chunk
// bindings settle a target. A rendition head does not record whether a watch
// or an operator built it.
func (s *Store) MissingWatchedRenditionTargetsAfter(ctx context.Context, profile, after string, bindings, limit int, watches []string) ([]WatchedRenditionTarget, error) {
	if bindings < 1 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid processing backfill bounds")
	}
	if len(watches) == 0 {
		return nil, nil
	}
	query := liveNodesSQL + `SELECT n.id,v.version_id,n.name,ws.watch_name,ws.source_ref,COALESCE(v.mime_type,'') FROM nodes n JOIN live l ON l.id=n.id
 JOIN content_versions v ON v.version_id=n.current_version_id
 JOIN watch_sources ws ON ws.node_id=n.id AND ws.blob_hash=v.blob_hash AND ws.size=v.size
` + s.missingTargetsFilter(true, ` AND ws.watch_name IN (`+numberedPlaceholders(len(watches), 2)+`)`, len(watches))
	args := []any{after, profile}
	for _, watch := range watches {
		args = append(args, watch)
	}
	args = append(args, bindings, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WatchedRenditionTarget
	for rows.Next() {
		var item WatchedRenditionTarget
		if err := rows.Scan(&item.NodeID, &item.ContentVersionID, &item.Name, &item.WatchName, &item.SourceRef, &item.MediaType); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// missingTargetsFilter uses $1 after and $2 profile, then the scope's own
// scopeArgs placeholders from $3, then bindings and limit. SQLite numbers $N
// parameters by first appearance, so they appear in numeric order.
// Rendered targets must have a rendition head for the profile and are
// settled only by its chunk bindings.
func (s *Store) missingTargetsFilter(rendered bool, scope string, scopeArgs int) string {
	d := metadataJSONDialect{s.postgresSchema != ""}
	bindings, limit := fmt.Sprintf("$%d", 3+scopeArgs), fmt.Sprintf("$%d", 4+scopeArgs)
	headKind := "h.input_kind<>'image'"
	renderedFilter := ""
	if rendered {
		headKind = "h.input_kind='rendition_chunk'"
		renderedFilter = ` AND EXISTS(SELECT 1 FROM rendition_heads rh WHERE rh.content_version_id=v.version_id AND rh.profile_fingerprint=$2)`
	}
	return ` WHERE n.trashed_at IS NULL AND v.version_id>$1` + renderedFilter + scope + `
 AND (SELECT COUNT(DISTINCT binding_id) FROM (
 SELECT h.binding_id FROM embedding_heads h
 WHERE h.content_version_id=v.version_id AND h.profile_fingerprint=$2 AND ` + headKind + `
 UNION SELECT ` + d.text("value", "name") + ` FROM processing_profiles p CROSS JOIN ` + d.arrayValues(d.text("p.canonical_profile", "embeddings")) + `
 WHERE p.profile_fingerprint=$2 AND ` + d.text("value", "input_kind") + `='rendition_chunk'
 AND ` + emptyRenditionChunkSQL("v.version_id", "$2") + `
 ) settled) < ` + bindings + `
 ORDER BY v.version_id LIMIT ` + limit
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
