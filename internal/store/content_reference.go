package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kit/packstore"
)

// ContentReference joins one immutable content version to the stable node that
// retains it. Path is populated only for a live node; trashed nodes deliberately
// have no resolvable virtual path.
type ContentReference struct {
	Version   ContentVersion
	Node      Node
	Path      string
	IsCurrent bool
}

// ContentReferencesByHash returns one bounded, deterministic page of logical
// references to canonical SHA-256 content. A physical blob with no retained
// content version is not a match. Live current references sort first, followed
// by live history and then trashed references.
func (s *Store) ContentReferencesByHash(
	ctx context.Context, hash string, limit, offset int,
) ([]ContentReference, int, error) {
	if _, err := packstore.ParseHash(hash); err != nil {
		return nil, 0, errors.New("content hash must be canonical lowercase SHA-256")
	}
	if limit < 1 || limit > 1000 {
		return nil, 0, errors.New("content-reference limit must be between 1 and 1000")
	}
	if offset < 0 {
		return nil, 0, errors.New("content-reference offset must not be negative")
	}

	// Totals and page are one statement so concurrent replacement, trash, or
	// trash-empty is observed entirely before or after the mutation. The
	// recursive path projection runs only for live nodes in the selected page.
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE matching AS (
			SELECT v.version_id, v.node_id, v.blob_hash, v.size, v.mime_type,
			       v.recorded_at, v.node_revision, v.introduced_operation_id,
			       v.transition_kind, v.source_version_id,
			       n.parent_id, n.name, n.kind, n.current_version_id,
			       current.blob_hash AS current_blob_hash,
			       current.size AS current_size, current.mime_type AS current_mime_type,
			       n.revision, n.created_at, n.modified_at, n.trashed_at,
			       n.trashed_at IS NOT NULL AS trashed_sort,
			       v.version_id != n.current_version_id AS historical_sort
			FROM content_versions v
			JOIN blobs b ON b.hash = v.blob_hash AND b.size = v.size
			JOIN nodes n ON n.id = v.node_id
			LEFT JOIN content_versions current
			  ON current.node_id = n.id AND current.version_id = n.current_version_id
			WHERE v.blob_hash = ?
		), page AS (
			SELECT * FROM matching
			ORDER BY trashed_sort, historical_sort, node_id, node_revision DESC, version_id
			LIMIT ? OFFSET ?
		), totals AS (
			SELECT COUNT(*) AS total FROM matching
		), ancestry(version_id, id, parent_id, path) AS (
			SELECT version_id, node_id, parent_id, name
			FROM page WHERE trashed_at IS NULL
			UNION ALL
			SELECT a.version_id, n.id, n.parent_id,
			       CASE WHEN n.name = '' THEN '/' || a.path ELSE n.name || '/' || a.path END
			FROM nodes n JOIN ancestry a ON n.id = a.parent_id
			WHERE n.trashed_at IS NULL
		), paths AS (
			SELECT version_id, path FROM ancestry WHERE parent_id IS NULL
		)
		SELECT totals.total,
		       COALESCE(page.version_id, ''), COALESCE(page.node_id, 0),
		       COALESCE(page.blob_hash, ''), COALESCE(page.size, 0),
		       COALESCE(page.mime_type, ''), COALESCE(page.recorded_at, ''),
		       COALESCE(page.node_revision, 0),
		       COALESCE(page.introduced_operation_id, ''),
		       COALESCE(page.transition_kind, ''), page.source_version_id,
		       COALESCE(page.node_id, 0), page.parent_id, COALESCE(page.name, ''),
		       COALESCE(page.kind, ''), COALESCE(page.current_version_id, ''),
		       COALESCE(page.current_blob_hash, ''), COALESCE(page.current_size, 0),
		       COALESCE(page.current_mime_type, ''), COALESCE(page.revision, 0),
		       COALESCE(page.created_at, ''), COALESCE(page.modified_at, ''),
		       page.trashed_at, COALESCE(paths.path, ''),
		       COALESCE(page.historical_sort = 0, false)
		FROM totals LEFT JOIN page ON true
		LEFT JOIN paths ON paths.version_id = page.version_id
		ORDER BY page.trashed_sort, page.historical_sort, page.node_id,
		         page.node_revision DESC, page.version_id`, hash, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("looking up content hash %s: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()

	references := make([]ContentReference, 0)
	var total int
	for rows.Next() {
		var ref ContentReference
		if err := rows.Scan(
			&total,
			&ref.Version.ID, &ref.Version.NodeID, &ref.Version.BlobHash,
			&ref.Version.Size, &ref.Version.MimeType, &ref.Version.RecordedAt,
			&ref.Version.NodeRevision, &ref.Version.IntroducedOperationID,
			&ref.Version.TransitionKind, &ref.Version.SourceVersionID,
			&ref.Node.ID, &ref.Node.ParentID, &ref.Node.Name, &ref.Node.Kind,
			&ref.Node.CurrentVersionID, &ref.Node.BlobHash, &ref.Node.Size,
			&ref.Node.MimeType, &ref.Node.Revision, &ref.Node.CreatedAt,
			&ref.Node.ModifiedAt, &ref.Node.TrashedAt, &ref.Path, &ref.IsCurrent,
		); err != nil {
			return nil, 0, fmt.Errorf("looking up content hash %s: scanning page: %w", hash, err)
		}
		if ref.Version.ID != "" {
			references = append(references, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("looking up content hash %s: %w", hash, err)
	}
	return references, total, nil
}
