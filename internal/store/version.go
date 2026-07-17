package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// ContentVersion is one immutable byte identity recorded for a stable file
// node. Initial ingest creates content_create records, verified replacement
// adds content_replace heads, and reversion adds content_revert heads that
// retain their source identity.
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
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		if err := validateContentReplacementTarget(n, ifRev); err != nil {
			return err
		}
		audited, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if audited {
			updated, version, err = replaceAuditedContentTx(
				ctx, tx, s, n, blobHash, size, mimeType,
			)
			return err
		}
		if err := s.EnsureBlobTx(tx, blobHash, size); err != nil {
			return err
		}
		updated, version, err = installContentVersionTx(
			tx, n, blobHash, size, mimeType, "content_replace", nil,
		)
		return err
	})
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	return updated, version, nil
}

// RevertContent creates a new immutable head with the exact content authority
// and media type of sourceVersionID. It records the source identity rather than
// rewinding the node or changing any historical row.
func (s *Store) RevertContent(
	ctx context.Context, nodeID, ifRev int64, sourceVersionID string,
) (Node, ContentVersion, ContentVersion, error) {
	if err := validateUUIDv4(sourceVersionID); err != nil {
		return Node{}, ContentVersion{}, ContentVersion{},
			fmt.Errorf("content version %q: %w", sourceVersionID, ErrNotFound)
	}
	var (
		updated Node
		version ContentVersion
		source  ContentVersion
	)
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		if err := validateContentReplacementTarget(n, ifRev); err != nil {
			return err
		}
		source, err = scanContentVersion(tx.QueryRow(
			`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id = ?`, sourceVersionID))
		if err != nil {
			return fmt.Errorf("content version %q: %w", sourceVersionID, err)
		}
		if source.NodeID != nodeID {
			return fmt.Errorf("content version %s belongs to node %d, not node %d: %w",
				source.ID, source.NodeID, nodeID, ErrVersionNodeMismatch)
		}
		if source.ID == n.CurrentVersionID {
			return fmt.Errorf("content version %s is the current head of node %d: %w",
				source.ID, nodeID, ErrVersionAlreadyCurrent)
		}
		if source.NodeRevision >= n.Revision+1 {
			return fmt.Errorf("source version %s revision %d is not older than node %d next revision %d",
				source.ID, source.NodeRevision, nodeID, n.Revision+1)
		}
		var catalogSize int64
		if err := tx.QueryRow(`SELECT size FROM blobs WHERE hash = ?`, source.BlobHash).
			Scan(&catalogSize); err != nil {
			return fmt.Errorf("checking source blob %s: %w", source.BlobHash, err)
		}
		if catalogSize != source.Size {
			return fmt.Errorf("source version %s records %d bytes but blob %s records %d",
				source.ID, source.Size, source.BlobHash, catalogSize)
		}
		updated, version, err = installContentVersionTx(
			tx, n, source.BlobHash, source.Size, source.MimeType, "content_revert", &source.ID,
		)
		return err
	})
	if err != nil {
		return Node{}, ContentVersion{}, ContentVersion{}, err
	}
	return updated, version, source, nil
}

func installContentVersionTx(
	tx *sql.Tx, n Node, blobHash string, size int64, mimeType, transitionKind string,
	sourceVersionID *string,
) (Node, ContentVersion, error) {
	operation, err := newContentVersionOperation()
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	return installContentVersionWithOperationTx(
		tx, n, blobHash, size, mimeType, transitionKind, sourceVersionID, operation,
	)
}

type contentVersionOperation struct {
	versionID   string
	operationID string
	recordedAt  string
}

func newContentVersionOperation() (contentVersionOperation, error) {
	versionID, err := newUUIDv4()
	if err != nil {
		return contentVersionOperation{}, err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return contentVersionOperation{}, err
	}
	return contentVersionOperation{
		versionID: versionID, operationID: operationID, recordedAt: nowRFC3339(),
	}, nil
}

func installContentVersionWithOperationTx(
	tx *sql.Tx, n Node, blobHash string, size int64, mimeType, transitionKind string,
	sourceVersionID *string, operation contentVersionOperation,
) (Node, ContentVersion, error) {
	newRevision := n.Revision + 1
	var storedMime any
	if mimeType != "" {
		storedMime = mimeType
	}
	if _, err := tx.Exec(
		`INSERT INTO content_versions (
			version_id, node_id, blob_hash, size, mime_type, recorded_at,
			node_revision, introduced_operation_id, transition_kind, source_version_id
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		operation.versionID, n.ID, blobHash, size, storedMime, operation.recordedAt,
		newRevision, operation.operationID,
		transitionKind, sourceVersionID); err != nil {
		return Node{}, ContentVersion{}, fmt.Errorf(
			"recording %s content version for node %d: %w", transitionKind, n.ID, err)
	}
	if _, err := tx.Exec(
		`UPDATE nodes SET current_version_id = ?, revision = ?, modified_at = ? WHERE id = ?`,
		operation.versionID, newRevision, operation.recordedAt, n.ID); err != nil {
		return Node{}, ContentVersion{}, fmt.Errorf(
			"installing %s content version for node %d: %w", transitionKind, n.ID, err)
	}
	updated, err := nodeByIDTx(tx, n.ID)
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	version, err := scanContentVersion(tx.QueryRow(
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id = ?`, operation.versionID))
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	return updated, version, nil
}

// CheckContentReplacementTarget performs the cheap target and revision checks
// before a caller streams bytes. ReplaceContent repeats them transactionally,
// because this preflight is an optimization rather than mutation authority.
func (s *Store) CheckContentReplacementTarget(ctx context.Context, nodeID, ifRev int64) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		if err := validateContentReplacementTarget(n, ifRev); err != nil {
			return err
		}
		audited, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil || !audited {
			return err
		}
		var member bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM audit_memberships WHERE node_id=?)`, nodeID,
		).Scan(&member); err != nil {
			return fmt.Errorf("checking audit membership for node %d: %w", nodeID, err)
		}
		if !member {
			return unsupportedAuditedNodeMutation(nodeID)
		}
		return nil
	})
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
