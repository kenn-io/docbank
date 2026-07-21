package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

// MaxWalkPageSize bounds every store-level snapshot page.
const MaxWalkPageSize = 5000

// WalkEntry is one node and its canonical path in a pinned tree snapshot.
type WalkEntry struct {
	Path string
	Node Node
}

// Walker pages through one tree snapshot held by a dedicated read transaction.
type Walker struct {
	db             *sql.DB
	tx             *sql.Tx
	pageSize       int
	includeTrashed bool
	rootPath       string
	rootID         int64
	lastPath       string
	lastID         int64
	done           bool

	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// BeginWalk pins a snapshot and prepares a bounded traversal rooted at rootPath.
func (s *Store) BeginWalk(
	ctx context.Context, rootPath string, pageSize int, includeTrashed bool,
) (_ *Walker, retErr error) {
	if pageSize < 1 || pageSize > MaxWalkPageSize {
		return nil, errors.New("walk page size must be between 1 and 5000")
	}
	db, err := s.driver.Open(s.path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	if err != nil {
		return nil, fmt.Errorf("beginning tree walk: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, db.Close())
		}
	}()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("beginning tree walk: %w", err)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, tx.Rollback())
		}
	}()

	root, err := nodeByPath(ctx, tx, s.rootID, rootPath)
	if err != nil {
		return nil, fmt.Errorf("beginning tree walk at %q: %w", rootPath, err)
	}
	canonicalRoot, err := pathOf(ctx, tx, root.ID)
	if err != nil {
		return nil, fmt.Errorf("beginning tree walk at %q: %w", rootPath, err)
	}
	return &Walker{
		db: db, tx: tx, pageSize: pageSize, includeTrashed: includeTrashed,
		rootPath: canonicalRoot, rootID: root.ID,
	}, nil
}

// Next returns the next bounded page in canonical path then node-ID order.
func (w *Walker) Next(ctx context.Context) ([]WalkEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || w.tx == nil {
		return nil, io.EOF
	}

	rows, err := w.tx.QueryContext(ctx, `
		WITH RECURSIVE tree(id, path) AS (
			SELECT id, ? FROM nodes WHERE id = ?
			UNION ALL
			SELECT child.id,
			       CASE WHEN tree.path = '/'
			            THEN '/' || child.name
			            ELSE tree.path || '/' || child.name END
			FROM nodes AS child
			JOIN tree ON child.parent_id = tree.id
			WHERE ? OR child.trashed_at IS NULL
		)
		SELECT tree.path, `+nodeCols+`
		FROM tree
		JOIN nodes AS n ON n.id = tree.id
		LEFT JOIN content_versions AS cv
			ON cv.node_id = n.id AND cv.version_id = n.current_version_id
		WHERE tree.path COLLATE BINARY > ? COLLATE BINARY
		   OR (tree.path COLLATE BINARY = ? COLLATE BINARY AND n.id > ?)
		ORDER BY tree.path COLLATE BINARY, n.id
		LIMIT ?`, w.rootPath, w.rootID, w.includeTrashed,
		w.lastPath, w.lastPath, w.lastID, w.pageSize)
	if err != nil {
		return nil, fmt.Errorf("walking tree snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := make([]WalkEntry, 0, w.pageSize)
	for rows.Next() {
		var entry WalkEntry
		if err := rows.Scan(&entry.Path,
			&entry.Node.ID, &entry.Node.ParentID, &entry.Node.Name, &entry.Node.Kind,
			&entry.Node.CurrentVersionID, &entry.Node.BlobHash, &entry.Node.Size,
			&entry.Node.MimeType, &entry.Node.Revision, &entry.Node.CreatedAt,
			&entry.Node.ModifiedAt, &entry.Node.TrashedAt,
		); err != nil {
			return nil, fmt.Errorf("walking tree snapshot: scanning page: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("walking tree snapshot: %w", err)
	}
	if len(entries) == 0 {
		w.done = true
		return nil, io.EOF
	}
	w.lastPath = entries[len(entries)-1].Path
	w.lastID = entries[len(entries)-1].Node.ID
	if len(entries) < w.pageSize {
		w.done = true
	}
	return entries, nil
}

// Close releases the snapshot transaction and its dedicated connection.
func (w *Walker) Close() error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		rollbackErr := w.tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		w.closeErr = errors.Join(rollbackErr, w.db.Close())
		w.tx = nil
		w.db = nil
	})
	return w.closeErr
}
