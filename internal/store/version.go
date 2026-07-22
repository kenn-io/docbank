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
	physical ...BlobPhysical,
) (Node, ContentVersion, error) {
	receipt, err := s.ReplaceContentWithReceipt(
		ctx, nodeID, ifRev, blobHash, size, mimeType, physical...,
	)
	return receipt.Node, receipt.Version, err
}

// ReplaceContentWithReceipt installs a new immutable head and returns its
// complete authority from the committing transaction.
func (s *Store) ReplaceContentWithReceipt(
	ctx context.Context, nodeID, ifRev int64, blobHash string, size int64, mimeType string,
	physical ...BlobPhysical,
) (ContentWriteReceipt, error) {
	if size < 0 {
		return ContentWriteReceipt{}, errors.New("content size must not be negative")
	}
	if err := validateUTF8Field("content MIME type", mimeType); err != nil {
		return ContentWriteReceipt{}, err
	}
	var receipt ContentWriteReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		receipt.Node, receipt.Version, err = s.replaceContentTx(
			ctx, tx, n, ifRev, blobHash, size, mimeType, physical...,
		)
		if err != nil {
			return err
		}
		receipt.Physical, err = physicalContentTx(tx, blobHash)
		return err
	})
	if err != nil {
		return ContentWriteReceipt{}, err
	}
	return receipt, nil
}

// ConfirmContentWithReceipt confirms that a file still has the expected
// immutable head while reconciling a newly published physical receipt. It is
// the transactional completion path for an otherwise idempotent write.
func (s *Store) ConfirmContentWithReceipt(
	ctx context.Context, nodeID, ifRev int64, blobHash string, size int64, mimeType string,
	physical ...BlobPhysical,
) (ContentWriteReceipt, error) {
	if size < 0 {
		return ContentWriteReceipt{}, errors.New("content size must not be negative")
	}
	if err := validateUTF8Field("content MIME type", mimeType); err != nil {
		return ContentWriteReceipt{}, err
	}
	var receipt ContentWriteReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		receipt, err = s.confirmContentWithReceiptTx(
			tx, n, ifRev, blobHash, size, mimeType, physical...,
		)
		return err
	})
	if err != nil {
		return ContentWriteReceipt{}, err
	}
	return receipt, nil
}

// ConfirmIngestedContentWithReceipt is the idempotent completion path for an
// embedded immutable create with provenance. It succeeds only when the
// existing node has the same content authority and an active matching source
// fact, so a retry cannot silently claim evidence that was never recorded.
func (s *Store) ConfirmIngestedContentWithReceipt(
	ctx context.Context, nodeID, ifRev int64, blobHash string, size int64, mimeType,
	sourceKind, sourceDescription, sourceReference, sourceModifiedAt string,
	physical ...BlobPhysical,
) (ContentWriteReceipt, error) {
	if size < 0 {
		return ContentWriteReceipt{}, errors.New("content size must not be negative")
	}
	if err := validateUTF8Field("content MIME type", mimeType); err != nil {
		return ContentWriteReceipt{}, err
	}
	if err := validateProvenanceSourceFields(
		sourceKind, sourceDescription, sourceReference, sourceModifiedAt,
	); err != nil {
		return ContentWriteReceipt{}, err
	}
	storedSourceKind := embeddedSourceKindPrefix + sourceKind
	var receipt ContentWriteReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		receipt, err = s.confirmContentWithReceiptTx(
			tx, n, ifRev, blobHash, size, mimeType, physical...,
		)
		if err != nil {
			return err
		}
		matched, err := activeProvenanceMatchesTx(
			ctx, tx, nodeID, storedSourceKind, sourceDescription,
			sourceReference, sourceModifiedAt,
		)
		if err != nil {
			return err
		}
		if !matched {
			return fmt.Errorf("node %d: %w", nodeID, ErrProvenanceMismatch)
		}
		return nil
	})
	if err != nil {
		return ContentWriteReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) confirmContentWithReceiptTx(
	tx *sql.Tx, n Node, ifRev int64, blobHash string, size int64, mimeType string,
	physical ...BlobPhysical,
) (ContentWriteReceipt, error) {
	if n.TrashedAt != nil {
		return ContentWriteReceipt{}, fmt.Errorf("node %d is trashed: %w", n.ID, ErrNotFound)
	}
	if n.IsDir() {
		return ContentWriteReceipt{}, fmt.Errorf("node %d: %w", n.ID, ErrNotFile)
	}
	if n.Revision != ifRev || n.BlobHash != blobHash || n.Size != size || n.MimeType != mimeType {
		return ContentWriteReceipt{}, fmt.Errorf(
			"node %d content changed while confirming revision %d: %w",
			n.ID, ifRev, ErrStaleRevision,
		)
	}
	if err := s.EnsureBlobTx(tx, blobHash, size, physical...); err != nil {
		return ContentWriteReceipt{}, err
	}
	receipt := ContentWriteReceipt{Node: n}
	var err error
	receipt.Version, err = scanContentVersion(tx.QueryRow(
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id = ?`,
		n.CurrentVersionID,
	))
	if err != nil {
		return ContentWriteReceipt{}, fmt.Errorf("reading current version of node %d: %w", n.ID, err)
	}
	receipt.Physical, err = physicalContentTx(tx, blobHash)
	if err != nil {
		return ContentWriteReceipt{}, err
	}
	return receipt, nil
}

// SyncWatchedContent resolves a watched source's stable node and compares the
// incoming bytes with the last bytes accepted from that source. The source
// cursor is deliberately independent of the node's selected version: a manual
// edit or revert must survive daemon restart when the source did not change.
func (s *Store) SyncWatchedContent(
	ctx context.Context, watchName, sourceRef, blobHash string, size int64, mimeType string,
	physical ...BlobPhysical,
) (Node, ContentVersion, bool, error) {
	if err := validateWatchSourceRecord(metadataWatchSource{
		Type: metadataWatchSourceType, WatchName: watchName, SourceRef: sourceRef,
		NodeID: 1, BlobHash: blobHash, Size: size,
	}); err != nil {
		return Node{}, ContentVersion{}, false, err
	}
	if err := validateUTF8Field("content MIME type", mimeType); err != nil {
		return Node{}, ContentVersion{}, false, err
	}
	var (
		updated Node
		version ContentVersion
		changed bool
	)
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		cursor, err := watchSourceTx(ctx, tx, watchName, sourceRef)
		if err != nil {
			return err
		}
		n, err := nodeByIDTx(tx, cursor.nodeID)
		if err != nil {
			return err
		}
		if n.TrashedAt != nil {
			return fmt.Errorf("watched source %q/%q maps to trashed node %d",
				watchName, sourceRef, n.ID)
		}
		if err := validateContentReplacementTarget(n, UnconditionalRev); err != nil {
			return err
		}
		if cursor.blobHash == blobHash && cursor.size == size {
			if err := s.EnsureBlobTx(tx, blobHash, size, physical...); err != nil {
				return fmt.Errorf("reconciling unchanged watched content: %w", err)
			}
			if n.BlobHash != blobHash {
				if _, err := requirePhysicalAuthorityTx(tx, n.BlobHash); err != nil {
					return fmt.Errorf("checking unchanged watched content: %w", err)
				}
			}
			updated = n
			return nil
		}
		if n.BlobHash == blobHash && n.Size == size {
			if err := s.EnsureBlobTx(tx, blobHash, size, physical...); err != nil {
				return fmt.Errorf("checking unchanged watched content: %w", err)
			}
			updated = n
			return updateWatchSourceTx(ctx, tx, watchName, sourceRef, blobHash, size)
		}
		updated, version, err = s.replaceContentTx(
			ctx, tx, n, UnconditionalRev, blobHash, size, mimeType, physical...,
		)
		if err != nil {
			return err
		}
		if err := updateWatchSourceTx(ctx, tx, watchName, sourceRef, blobHash, size); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return Node{}, ContentVersion{}, false, err
	}
	return updated, version, changed, nil
}

type watchSourceState struct {
	nodeID   int64
	blobHash string
	size     int64
}

func watchSourceTx(
	ctx context.Context, tx *sql.Tx, watchName, sourceRef string,
) (watchSourceState, error) {
	var state watchSourceState
	err := tx.QueryRowContext(ctx, `
		SELECT node_id, blob_hash, size
		FROM watch_sources
		WHERE watch_name = ? AND source_ref = ?`,
		watchName, sourceRef,
	).Scan(&state.nodeID, &state.blobHash, &state.size)
	if errors.Is(err, sql.ErrNoRows) {
		return watchSourceState{}, ErrNotFound
	}
	if err != nil {
		return watchSourceState{}, fmt.Errorf(
			"resolving watched source %q/%q: %w", watchName, sourceRef, err,
		)
	}
	return state, nil
}

func updateWatchSourceTx(
	ctx context.Context, tx *sql.Tx, watchName, sourceRef, blobHash string, size int64,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE watch_sources SET blob_hash = ?, size = ?
		WHERE watch_name = ? AND source_ref = ?`,
		blobHash, size, watchName, sourceRef,
	)
	if err != nil {
		return fmt.Errorf("updating watched source %q/%q: %w", watchName, sourceRef, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking watched source update %q/%q: %w", watchName, sourceRef, err)
	}
	if updated != 1 {
		return fmt.Errorf("updating watched source %q/%q: %w", watchName, sourceRef, ErrNotFound)
	}
	return nil
}

func (s *Store) replaceContentTx(
	ctx context.Context, tx *sql.Tx, n Node, ifRev int64,
	blobHash string, size int64, mimeType string, physical ...BlobPhysical,
) (Node, ContentVersion, error) {
	if err := validateContentReplacementTarget(n, ifRev); err != nil {
		return Node{}, ContentVersion{}, err
	}
	audited, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	if audited {
		return installAuditedContentVersionTx(
			ctx, tx, s, n, blobHash, size, mimeType, "content_replace", nil, physical...,
		)
	}
	if err := s.EnsureBlobTx(tx, blobHash, size, physical...); err != nil {
		return Node{}, ContentVersion{}, err
	}
	return installContentVersionTx(
		tx, n, blobHash, size, mimeType, "content_replace", nil,
	)
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
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
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
		catalogSize, err := requirePhysicalAuthorityTx(tx, source.BlobHash)
		if err != nil {
			return fmt.Errorf("checking source blob %s: %w", source.BlobHash, err)
		}
		if catalogSize != source.Size {
			return fmt.Errorf("source version %s records %d bytes but blob %s records %d",
				source.ID, source.Size, source.BlobHash, catalogSize)
		}
		audited, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if audited {
			updated, version, err = installAuditedContentVersionTx(
				ctx, tx, s, n, source.BlobHash, source.Size, source.MimeType,
				"content_revert", &source.ID,
			)
			return err
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
	if err := queueTextExtractionTx(tx, operation.versionID, blobHash, mimeType); err != nil {
		return Node{}, ContentVersion{}, err
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
