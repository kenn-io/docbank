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

	transactionContext := context.WithoutCancel(ctx)
	tx, err := db.BeginTx(transactionContext, &sql.TxOptions{ReadOnly: true})
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
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE walk_snapshot (
			path TEXT COLLATE BINARY NOT NULL,
			node_id INTEGER NOT NULL,
			PRIMARY KEY (path, node_id)
		) WITHOUT ROWID`); err != nil {
		return nil, fmt.Errorf("beginning tree walk at %q: creating traversal state: %w", rootPath, err)
	}
	if _, err := tx.ExecContext(ctx, `
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
		INSERT INTO walk_snapshot(path, node_id)
		SELECT path, id FROM tree`, canonicalRoot, root.ID, includeTrashed); err != nil {
		return nil, fmt.Errorf("beginning tree walk at %q: building traversal state: %w", rootPath, err)
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
		SELECT snapshot.path, `+nodeCols+`
		FROM walk_snapshot AS snapshot
		JOIN nodes AS n ON n.id = snapshot.node_id
		LEFT JOIN content_versions AS cv
			ON cv.node_id = n.id AND cv.version_id = n.current_version_id
		WHERE snapshot.path > ?
		   OR (snapshot.path = ? AND snapshot.node_id > ?)
		ORDER BY snapshot.path, snapshot.node_id
		LIMIT ?`, w.lastPath, w.lastPath, w.lastID, w.pageSize)
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
		var rollbackErr error
		if w.tx != nil {
			rollbackErr = w.tx.Rollback()
		}
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		var closeErr error
		if w.db != nil {
			closeErr = w.db.Close()
		}
		w.closeErr = errors.Join(rollbackErr, closeErr)
		w.tx = nil
		w.db = nil
	})
	return w.closeErr
}
