package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ContentVersion is one immutable byte identity recorded for a stable file
// node. Initial ingest currently creates content_create records; later editing
// operations can add new heads without changing this read contract.
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
	n, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		return nil, 0, err
	}
	if n.IsDir() {
		return nil, 0, fmt.Errorf("node %d: %w", nodeID, ErrNotFile)
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM content_versions WHERE node_id = ?`, nodeID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting content versions of node %d: %w", nodeID, err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions
		 WHERE node_id = ? ORDER BY node_revision DESC, version_id LIMIT ? OFFSET ?`,
		nodeID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("listing content versions of node %d: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()
	versions := make([]ContentVersion, 0)
	for rows.Next() {
		v, err := scanContentVersion(rows)
		if err != nil {
			return nil, 0, err
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing content versions of node %d: %w", nodeID, err)
	}
	return versions, total, nil
}
