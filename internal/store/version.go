package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// ContentVersion is one immutable byte identity recorded for a stable file
// node. Initial ingest creates content_create records and verified replacement
// adds content_replace heads without changing this read contract.
type ContentVersion struct {
	ID                    string
	NodeID                int64
	BlobHash              string
	Size                  int64
	MimeType              string
	RecordedAt            string
	NodeRevision          int64
	IntroducedOperationID string
	TransitionKind        string
	SourceVersionID       *string
}

const contentVersionCols = `version_id, node_id, blob_hash, size,
	COALESCE(mime_type, ''), recorded_at, node_revision,
	introduced_operation_id, transition_kind, source_version_id`

func scanContentVersion(row interface{ Scan(args ...any) error }) (ContentVersion, error) {
	var v ContentVersion
	err := row.Scan(&v.ID, &v.NodeID, &v.BlobHash, &v.Size, &v.MimeType,
		&v.RecordedAt, &v.NodeRevision, &v.IntroducedOperationID,
		&v.TransitionKind, &v.SourceVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ContentVersion{}, ErrNotFound
	}
	if err != nil {
		return ContentVersion{}, fmt.Errorf("scanning content version: %w", err)
	}
	return v, nil
}

// ContentVersionByID returns one version by its globally stable identity.
func (s *Store) ContentVersionByID(ctx context.Context, id string) (ContentVersion, error) {
	if err := validateUUIDv4(id); err != nil {
		return ContentVersion{}, fmt.Errorf("content version %q: %w", id, ErrNotFound)
	}
	v, err := scanContentVersion(s.db.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id = ?`, id))
	if err != nil {
		return ContentVersion{}, fmt.Errorf("content version %q: %w", id, err)
	}
	return v, nil
}

// ContentVersions lists one bounded page newest-first and returns the total
// number of versions recorded for the node.
func (s *Store) ContentVersions(
	ctx context.Context, nodeID int64, limit, offset int,
) ([]ContentVersion, int, error) {
	if limit < 1 || limit > 1000 {
		return nil, 0, errors.New("content-version limit must be between 1 and 1000")
	}
	if offset < 0 {
		return nil, 0, errors.New("content-version offset must not be negative")
	}
	// Existence, kind, total, and page are deliberately one statement so a
	// concurrent trash-empty observes either side of deletion, never a mixture.
	rows, err := s.db.QueryContext(ctx,
		`WITH target AS (
		   SELECT kind FROM nodes WHERE id = ?
		 ), page AS (
		   SELECT version_id, node_id, blob_hash, size, mime_type, recorded_at,
		          node_revision, introduced_operation_id, transition_kind,
		          source_version_id
		   FROM content_versions
		   WHERE node_id = ?
		   ORDER BY node_revision DESC, version_id LIMIT ? OFFSET ?
		 ), totals AS (
		   SELECT COUNT(*) AS total FROM content_versions WHERE node_id = ?
		 )
		 SELECT target.kind, totals.total,
		        COALESCE(page.version_id, ''), COALESCE(page.node_id, 0),
		        COALESCE(page.blob_hash, ''), COALESCE(page.size, 0),
		        COALESCE(page.mime_type, ''), COALESCE(page.recorded_at, ''),
		        COALESCE(page.node_revision, 0),
		        COALESCE(page.introduced_operation_id, ''),
		        COALESCE(page.transition_kind, ''), page.source_version_id
		 FROM target CROSS JOIN totals LEFT JOIN page ON true
		 ORDER BY page.node_revision DESC, page.version_id`,
		nodeID, nodeID, limit, offset, nodeID)
	if err != nil {
		return nil, 0, fmt.Errorf("listing content versions of node %d: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()
	versions := make([]ContentVersion, 0)
	var total int
	found := false
	for rows.Next() {
		found = true
		var kind string
		var v ContentVersion
		if err := rows.Scan(&kind, &total, &v.ID, &v.NodeID, &v.BlobHash, &v.Size,
			&v.MimeType, &v.RecordedAt, &v.NodeRevision, &v.IntroducedOperationID,
			&v.TransitionKind, &v.SourceVersionID); err != nil {
			return nil, 0, fmt.Errorf("listing content versions of node %d: scanning page: %w", nodeID, err)
		}
		if kind != "file" {
			return nil, 0, fmt.Errorf("node %d: %w", nodeID, ErrNotFile)
		}
		if v.ID != "" {
			versions = append(versions, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing content versions of node %d: %w", nodeID, err)
	}
	if !found {
		return nil, 0, fmt.Errorf("node %d: %w", nodeID, ErrNotFound)
	}
	return versions, total, nil
}

// ReplaceContent installs one already-durable blob as a file's new immutable
// head. Unless ifRev is UnconditionalRev, the node must still be at ifRev;
// version creation, pointer replacement, and the node revision bump commit as
// one transaction.
func (s *Store) ReplaceContent(
	ctx context.Context, nodeID, ifRev int64, blobHash string, size int64, mimeType string,
) (Node, ContentVersion, error) {
	if size < 0 {
		return Node{}, ContentVersion{}, errors.New("content size must not be negative")
	}
	if err := validateUTF8Field("content MIME type", mimeType); err != nil {
		return Node{}, ContentVersion{}, err
	}
	var (
		updated Node
		version ContentVersion
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		if err := validateContentReplacementTarget(n, ifRev); err != nil {
			return err
		}
		if err := s.EnsureBlobTx(tx, blobHash, size); err != nil {
			return err
		}
		versionID, err := newUUIDv4()
		if err != nil {
			return err
		}
		operationID, err := newUUIDv4()
		if err != nil {
			return err
		}
		now := nowRFC3339()
		newRevision := n.Revision + 1
		var storedMime any
		if mimeType != "" {
			storedMime = mimeType
		}
		if _, err := tx.Exec(
			`INSERT INTO content_versions (
				version_id, node_id, blob_hash, size, mime_type, recorded_at,
				node_revision, introduced_operation_id, transition_kind, source_version_id
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'content_replace', NULL)`,
			versionID, nodeID, blobHash, size, storedMime, now, newRevision, operationID); err != nil {
			return fmt.Errorf("recording replacement content version for node %d: %w", nodeID, err)
		}
		if _, err := tx.Exec(
			`UPDATE nodes SET current_version_id = ?, revision = ?, modified_at = ? WHERE id = ?`,
			versionID, newRevision, now, nodeID); err != nil {
			return fmt.Errorf("installing replacement content version for node %d: %w", nodeID, err)
		}
		updated, err = nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		version, err = scanContentVersion(tx.QueryRow(
			`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id = ?`, versionID))
		return err
	})
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	return updated, version, nil
}

// CheckContentReplacementTarget performs the cheap target and revision checks
// before a caller streams bytes. ReplaceContent repeats them transactionally,
// because this preflight is an optimization rather than mutation authority.
func (s *Store) CheckContentReplacementTarget(ctx context.Context, nodeID, ifRev int64) error {
	n, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		return err
	}
	return validateContentReplacementTarget(n, ifRev)
}

func validateContentReplacementTarget(n Node, ifRev int64) error {
	if n.TrashedAt != nil {
		return fmt.Errorf("node %d is trashed: %w", n.ID, ErrNotFound)
	}
	if n.IsDir() {
		return fmt.Errorf("node %d: %w", n.ID, ErrNotFile)
	}
	if ifRev != UnconditionalRev && n.Revision != ifRev {
		return fmt.Errorf("node %d at revision %d, expected %d: %w",
			n.ID, n.Revision, ifRev, ErrStaleRevision)
	}
	if n.Revision == math.MaxInt64 {
		return fmt.Errorf("node %d revision cannot advance beyond %d", n.ID, n.Revision)
	}
	return nil
}
