package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func nodeByIDTx(tx *sql.Tx, id int64) (Node, error) {
	n, err := scanNode(tx.QueryRow(`SELECT `+nodeCols+` FROM `+nodeFrom+` WHERE n.id = ?`, id))
	if err != nil {
		return Node{}, fmt.Errorf("node %d: %w", id, err)
	}
	return n, nil
}

func bumpRevisionTx(tx *sql.Tx, id int64, now string) error {
	if _, err := tx.Exec(
		`UPDATE nodes SET revision = revision + 1, modified_at = ? WHERE id = ?`,
		now, id); err != nil {
		return fmt.Errorf("bumping revision of node %d: %w", id, err)
	}
	return nil
}

// liveDirTx loads id and errors unless it is a live directory.
func liveDirTx(tx *sql.Tx, id int64) (Node, error) {
	n, err := nodeByIDTx(tx, id)
	if err != nil {
		return Node{}, err
	}
	if n.TrashedAt != nil {
		return Node{}, fmt.Errorf("node %d: %w", id, ErrNotFound)
	}
	if !n.IsDir() {
		return Node{}, fmt.Errorf("node %d: %w", id, ErrNotDir)
	}
	return n, nil
}

// Mkdir creates a directory under parentID.
func (s *Store) Mkdir(ctx context.Context, parentID int64, name string) (Node, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Node{}, err
	}
	var created Node
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if !active {
			created, err = s.mkdirTx(tx, parentID, name, nowRFC3339())
			return err
		}
		priorParent, err := liveDirTx(tx, parentID)
		if err != nil {
			return err
		}
		authority, scopes, _, err := loadAuditedNodeAuthority(ctx, tx, parentID)
		if err != nil {
			return err
		}
		operationID, err := newUUIDv4()
		if err != nil {
			return err
		}
		recordedAt := nowRFC3339()
		created, err = s.mkdirTx(tx, parentID, name, recordedAt)
		if err != nil {
			return err
		}
		resultingParent, err := nodeByIDTx(tx, parentID)
		if err != nil {
			return err
		}
		return persistAuditedNodeCreation(ctx, tx, s.vaultID, authority, scopes,
			priorParent, resultingParent, created, ContentVersion{}, operationID, recordedAt, nil)
	})
	if err != nil {
		return Node{}, err
	}
	return created, nil
}

func (s *Store) mkdirTx(
	tx *sql.Tx, parentID int64, name, recordedAt string,
) (Node, error) {
	if _, err := liveDirTx(tx, parentID); err != nil {
		return Node{}, err
	}
	res, err := tx.Exec(
		`INSERT INTO nodes (parent_id, name, kind, created_at, modified_at)
		 VALUES (?, ?, 'dir', ?, ?)`, parentID, name, recordedAt, recordedAt)
	if s.driver.IsUniqueViolation(err) {
		return Node{}, fmt.Errorf("mkdir %q under node %d: %w", name, parentID, ErrExists)
	}
	if err != nil {
		return Node{}, fmt.Errorf("mkdir %q under node %d: %w", name, parentID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Node{}, fmt.Errorf("mkdir %q: reading id: %w", name, err)
	}
	if err := bumpRevisionTx(tx, parentID, recordedAt); err != nil {
		return Node{}, err
	}
	return nodeByIDTx(tx, id)
}

// childByName returns the live child of dirID named name (name must already
// be normalized).
func (s *Store) childByName(ctx context.Context, dirID int64, name string) (Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeCols+` FROM `+nodeFrom+`
		 WHERE n.parent_id = ? AND n.name = ? AND n.trashed_at IS NULL`, dirID, name)
	return scanNode(row)
}

// MkdirAll creates every missing directory along path and returns the leaf.
func (s *Store) MkdirAll(ctx context.Context, path string) (Node, error) {
	n, err := s.NodeByID(ctx, s.rootID)
	if err != nil {
		return Node{}, err
	}
	for _, seg := range splitPath(path) {
		n, err = s.EnsureDir(ctx, n.ID, seg)
		if err != nil {
			return Node{}, fmt.Errorf("mkdir -p %q: %w", path, err)
		}
	}
	return n, nil
}

// EnsureDir returns the live directory named name under parentID, creating
// it if missing and converging on a concurrent creation. ID-based on
// purpose: callers that resolved a destination once (ingest) must not
// re-derive it from a path, which a concurrent move or trash can redirect.
func (s *Store) EnsureDir(ctx context.Context, parentID int64, name string) (Node, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Node{}, err
	}
	next, err := s.childByName(ctx, parentID, name)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		created, mkErr := s.Mkdir(ctx, parentID, name)
		if mkErr == nil {
			return created, nil
		}
		if !errors.Is(mkErr, ErrExists) {
			return Node{}, mkErr
		}
		// Lost a race with a concurrent create: re-read the now-existing
		// child and converge on it.
		next, err = s.childByName(ctx, parentID, name)
		if err != nil {
			return Node{}, err
		}
	default:
		return Node{}, err
	}
	if !next.IsDir() {
		return Node{}, fmt.Errorf("%q is a file: %w", name, ErrNotDir)
	}
	return next, nil
}

// EnsureBlobTx records a blob row if missing. The blob file must already be
// durable on disk before the enclosing transaction commits. If a blob row
// already exists under hash, its recorded size must match size: a mismatch
// means two different contents hashed to the same value, or a caller passed
// a wrong size, either of which is a corruption signal rather than ordinary
// dedup and must not be silently accepted.
func (s *Store) EnsureBlobTx(tx *sql.Tx, hash string, size int64) error {
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO blobs (hash, size, created_at) VALUES (?, ?, ?)`,
		hash, size, nowRFC3339()); err != nil {
		return fmt.Errorf("recording blob %s: %w", hash, err)
	}
	var stored int64
	if err := tx.QueryRow(`SELECT size FROM blobs WHERE hash = ?`, hash).Scan(&stored); err != nil {
		return fmt.Errorf("verifying blob %s: %w", hash, err)
	}
	if stored != size {
		return fmt.Errorf("blob %s: recorded size %d does not match caller size %d", hash, stored, size)
	}
	return nil
}

func (s *Store) createFileTx(tx *sql.Tx, parentID int64, name, blobHash string, size int64, mimeType string) (Node, error) {
	operation, err := newContentVersionOperation()
	if err != nil {
		return Node{}, err
	}
	created, _, err := s.createFileWithOperationTx(
		tx, parentID, name, blobHash, size, mimeType, operation,
	)
	return created, err
}

func (s *Store) createFileWithOperationTx(
	tx *sql.Tx, parentID int64, name, blobHash string, size int64, mimeType string,
	operation contentVersionOperation,
) (Node, ContentVersion, error) {
	if err := validateUTF8Field("content MIME type", mimeType); err != nil {
		return Node{}, ContentVersion{}, err
	}
	if _, err := liveDirTx(tx, parentID); err != nil {
		return Node{}, ContentVersion{}, err
	}
	if err := s.EnsureBlobTx(tx, blobHash, size); err != nil {
		return Node{}, ContentVersion{}, err
	}
	res, err := tx.Exec(
		`INSERT INTO nodes (parent_id, name, kind, current_version_id, created_at, modified_at)
		 VALUES (?, ?, 'file', ?, ?, ?)`,
		parentID, name, operation.versionID, operation.recordedAt, operation.recordedAt)
	if s.driver.IsUniqueViolation(err) {
		return Node{}, ContentVersion{}, fmt.Errorf(
			"creating file %q under node %d: %w", name, parentID, ErrExists)
	}
	if err != nil {
		return Node{}, ContentVersion{}, fmt.Errorf(
			"creating file %q under node %d: %w", name, parentID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Node{}, ContentVersion{}, fmt.Errorf("creating file %q: reading id: %w", name, err)
	}
	var storedMime any
	if mimeType != "" {
		storedMime = mimeType
	}
	if _, err := tx.Exec(
		`INSERT INTO content_versions (
			version_id, node_id, blob_hash, size, mime_type, recorded_at,
			node_revision, introduced_operation_id, transition_kind, source_version_id
		 ) VALUES (?, ?, ?, ?, ?, ?, 1, ?, 'content_create', NULL)`,
		operation.versionID, id, blobHash, size, storedMime, operation.recordedAt,
		operation.operationID); err != nil {
		return Node{}, ContentVersion{}, fmt.Errorf(
			"recording initial content version for node %d: %w", id, err)
	}
	if err := queueTextExtractionTx(tx, blobHash, mimeType); err != nil {
		return Node{}, ContentVersion{}, err
	}
	if err := bumpRevisionTx(tx, parentID, operation.recordedAt); err != nil {
		return Node{}, ContentVersion{}, err
	}
	created, err := nodeByIDTx(tx, id)
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	version, err := scanContentVersion(tx.QueryRow(
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`, operation.versionID,
	))
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	return created, version, nil
}

// CreateFile creates a file node pointing at an already-durable blob.
func (s *Store) CreateFile(ctx context.Context, parentID int64, name, blobHash string, size int64, mimeType string) (Node, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Node{}, err
	}
	var created Node
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if !active {
			created, err = s.createFileTx(tx, parentID, name, blobHash, size, mimeType)
			return err
		}
		priorParent, err := liveDirTx(tx, parentID)
		if err != nil {
			return err
		}
		authority, scopes, _, err := loadAuditedNodeAuthority(ctx, tx, parentID)
		if err != nil {
			return err
		}
		operation, err := newContentVersionOperation()
		if err != nil {
			return err
		}
		var version ContentVersion
		created, version, err = s.createFileWithOperationTx(
			tx, parentID, name, blobHash, size, mimeType, operation,
		)
		if err != nil {
			return err
		}
		resultingParent, err := nodeByIDTx(tx, parentID)
		if err != nil {
			return err
		}
		return persistAuditedNodeCreation(ctx, tx, s.vaultID, authority, scopes,
			priorParent, resultingParent, created, version, operation.operationID,
			operation.recordedAt, nil)
	})
	if err != nil {
		return Node{}, err
	}
	return created, nil
}

// isAncestorTx reports whether maybeAncestor is candidate itself or one of
// its ancestors (walking parent links).
func isAncestorTx(tx *sql.Tx, maybeAncestor, candidate int64) (bool, error) {
	cur := candidate
	for {
		if cur == maybeAncestor {
			return true, nil
		}
		var parent sql.NullInt64
		err := tx.QueryRow(`SELECT parent_id FROM nodes WHERE id = ?`, cur).Scan(&parent)
		if err != nil {
			return false, fmt.Errorf("walking ancestry of node %d: %w", candidate, err)
		}
		if !parent.Valid {
			return false, nil
		}
		cur = parent.Int64
	}
}

// Move renames and/or reparents a live node in one transaction. Unless
// ifRev is UnconditionalRev, the mutation fails with ErrStaleRevision
// unless ifRev matches the node's current revision. The returned canonical
// path is captured in the mutation transaction with the returned node.
func (s *Store) Move(
	ctx context.Context, id, newParentID int64, newName string, ifRev int64,
) (Node, string, error) {
	var moved Node
	var movedPath string
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if active {
			moved, err = s.moveAuditedTx(ctx, tx, id, newParentID, newName, ifRev)
		} else {
			moved, err = s.moveTx(tx, id, newParentID, newName, ifRev)
		}
		if err == nil {
			movedPath, err = pathOf(ctx, tx, moved.ID)
		}
		return err
	})
	if err != nil {
		return Node{}, "", err
	}
	return moved, movedPath, nil
}

// MovePath resolves srcPath and destPath and moves inside one transaction,
// so a concurrent operation cannot relocate either path between resolution
// and mutation. A destPath naming an existing live directory means "move
// into, keep name"; otherwise its parent must exist and its basename becomes
// the new name. The returned canonical path is captured in the mutation
// transaction with the returned node.
func (s *Store) MovePath(ctx context.Context, srcPath, destPath string) (Node, string, error) {
	var moved Node
	var movedPath string
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		src, err := nodeByPath(ctx, tx, s.rootID, srcPath)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", srcPath, err)
		}
		if src.ID == s.rootID {
			return ErrIsRoot
		}
		newParentID, newName, err := s.resolveMoveTargetTx(ctx, tx, destPath, src.Name)
		if err != nil {
			return err
		}
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if active {
			moved, err = s.moveAuditedTx(
				ctx, tx, src.ID, newParentID, newName, UnconditionalRev,
			)
		} else {
			moved, err = s.moveTx(tx, src.ID, newParentID, newName, UnconditionalRev)
		}
		if err == nil {
			movedPath, err = pathOf(ctx, tx, moved.ID)
		}
		return err
	})
	if err != nil {
		return Node{}, "", err
	}
	return moved, movedPath, nil
}

func (s *Store) resolveMoveTargetTx(
	ctx context.Context, tx *sql.Tx, destPath, keepName string,
) (int64, string, error) {
	// Validate every segment up front, literally. Virtual paths have no
	// dot-segment semantics, and deriving the parent with path.Dir would
	// Clean them: "/missing/../renamed" must be rejected, not silently
	// collapsed into a rename at the root.
	segs := splitPath(destPath)
	for i, seg := range segs {
		seg, err := NormalizeName(seg)
		if err != nil {
			return 0, "", fmt.Errorf("destination %q: %w", destPath, err)
		}
		segs[i] = seg
	}
	if dest, err := nodeByPath(ctx, tx, s.rootID, destPath); err == nil {
		if dest.IsDir() {
			return dest.ID, keepName, nil
		}
		return 0, "", fmt.Errorf("destination %q: %w", destPath, ErrExists)
	} else if !errors.Is(err, ErrNotFound) {
		return 0, "", fmt.Errorf("resolving destination %q: %w", destPath, err)
	}
	if len(segs) == 0 {
		return 0, "", fmt.Errorf("destination %q: %w", destPath, ErrExists)
	}
	parentPath := "/" + strings.Join(segs[:len(segs)-1], "/")
	parent, err := nodeByPath(ctx, tx, s.rootID, parentPath)
	if err != nil {
		return 0, "", fmt.Errorf("resolving destination parent %q: %w", parentPath, err)
	}
	return parent.ID, segs[len(segs)-1], nil
}

func (s *Store) moveTx(tx *sql.Tx, id, newParentID int64, newName string, ifRev int64) (Node, error) {
	return s.moveAtTx(tx, id, newParentID, newName, ifRev, nowRFC3339())
}

func (s *Store) moveAtTx(
	tx *sql.Tx, id, newParentID int64, newName string, ifRev int64, recordedAt string,
) (Node, error) {
	if id == s.rootID {
		return Node{}, ErrIsRoot
	}
	newName, err := NormalizeName(newName)
	if err != nil {
		return Node{}, err
	}
	n, err := nodeByIDTx(tx, id)
	if err != nil {
		return Node{}, err
	}
	if n.TrashedAt != nil {
		return Node{}, fmt.Errorf("node %d is trashed: %w", id, ErrNotFound)
	}
	if ifRev != UnconditionalRev && n.Revision != ifRev {
		return Node{}, fmt.Errorf("node %d at revision %d, expected %d: %w",
			id, n.Revision, ifRev, ErrStaleRevision)
	}
	if _, err := liveDirTx(tx, newParentID); err != nil {
		return Node{}, err
	}
	if *n.ParentID == newParentID && n.Name == newName {
		return n, nil
	}
	// The destination may not be the node itself or any of its
	// descendants (equivalently: id may not be an ancestor of dest).
	inCycle, err := isAncestorTx(tx, id, newParentID)
	if err != nil {
		return Node{}, err
	}
	if inCycle {
		return Node{}, fmt.Errorf("moving node %d under %d: %w", id, newParentID, ErrCycle)
	}
	_, err = tx.Exec(
		`UPDATE nodes SET parent_id = ?, name = ?, revision = revision + 1,
		        modified_at = ? WHERE id = ?`,
		newParentID, newName, recordedAt, id)
	if s.driver.IsUniqueViolation(err) {
		return Node{}, fmt.Errorf("moving node %d to %q: %w", id, newName, ErrExists)
	}
	if err != nil {
		return Node{}, fmt.Errorf("moving node %d: %w", id, err)
	}
	oldParent := *n.ParentID
	if err := bumpRevisionTx(tx, oldParent, recordedAt); err != nil {
		return Node{}, err
	}
	if newParentID != oldParent {
		if err := bumpRevisionTx(tx, newParentID, recordedAt); err != nil {
			return Node{}, err
		}
	}
	return nodeByIDTx(tx, id)
}
