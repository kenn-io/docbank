package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const (
	// MaxWalkPageSize bounds every store-level snapshot page.
	MaxWalkPageSize = 5000
	// MaxWalkDepth bounds the indexed frontier expansion required by one walk.
	MaxWalkDepth = 256
	// MaxWalkPathBytes bounds each path materialized in a walk page. At the
	// maximum page size, paths consume at most about 80 MiB before node data.
	MaxWalkPathBytes = 16 << 10
)

// WalkEntry is one node and its canonical path in a pinned tree snapshot.
type WalkEntry struct {
	Path string
	Node Node
}

// WalkStats exposes deterministic traversal work without exposing the TEMP
// frontier implementation. RowsExamined counts frontier rows returned for the
// last page; each returned row performs at most two indexed child/sibling seeks.
type WalkStats struct {
	SetupNodeReads       int64
	Pages                int64
	EntriesReturned      int64
	LastPageRowsExamined int64
	LastPageIndexedSeeks int64
}

// Walker pages through one tree snapshot held by a dedicated read transaction.
type Walker struct {
	db             *sql.DB
	tx             *sql.Tx
	pageSize       int
	includeTrashed bool
	done           bool
	stats          WalkStats

	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// BeginWalk pins a snapshot and seeds an incremental ordered frontier rooted at
// rootPath. Setup work depends on root-path depth, never subtree cardinality.
func (s *Store) BeginWalk(
	ctx context.Context, rootPath string, pageSize int, includeTrashed bool,
) (_ *Walker, retErr error) {
	if pageSize < 1 || pageSize > MaxWalkPageSize {
		return nil, errors.New("walk page size must be between 1 and 5000")
	}
	rootDepth := len(splitPath(rootPath))
	if rootDepth > MaxWalkDepth {
		return nil, fmt.Errorf("walk depth exceeds %d", MaxWalkDepth)
	}
	if len(rootPath) > MaxWalkPathBytes {
		return nil, fmt.Errorf("walk path exceeds %d bytes", MaxWalkPathBytes)
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
	rootDepth = len(splitPath(canonicalRoot))
	if rootDepth > MaxWalkDepth {
		return nil, fmt.Errorf("beginning tree walk at %q: walk depth exceeds %d",
			rootPath, MaxWalkDepth)
	}
	if len(canonicalRoot) > MaxWalkPathBytes {
		return nil, fmt.Errorf("beginning tree walk at %q: walk path exceeds %d bytes",
			rootPath, MaxWalkPathBytes)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE walk_frontier (
			path TEXT COLLATE BINARY NOT NULL,
			node_id INTEGER NOT NULL,
			depth INTEGER NOT NULL,
			PRIMARY KEY (path, node_id)
		) WITHOUT ROWID`); err != nil {
		return nil, fmt.Errorf("beginning tree walk at %q: creating frontier: %w", rootPath, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO walk_frontier(path, node_id, depth) VALUES (?, ?, ?)`,
		canonicalRoot, root.ID, rootDepth); err != nil {
		return nil, fmt.Errorf("beginning tree walk at %q: seeding frontier: %w", rootPath, err)
	}
	return &Walker{
		db: db, tx: tx, pageSize: pageSize, includeTrashed: includeTrashed,
		stats: WalkStats{SetupNodeReads: int64(2 * (rootDepth + 1))},
	}, nil
}

// Next returns the next bounded page in canonical path then node-ID order.
// The frontier contains only the next candidate from each explored sibling
// iterator. Expanding one returned node performs at most one next-sibling seek
// and, for a directory, one first-child seek, both backed by nodes_parent_name_id.
func (w *Walker) Next(ctx context.Context) ([]WalkEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || w.tx == nil {
		return nil, io.EOF
	}
	if _, err := w.tx.ExecContext(ctx, `SAVEPOINT walk_page`); err != nil {
		return nil, fmt.Errorf("walking tree snapshot: starting page: %w", err)
	}

	entries := make([]WalkEntry, 0, w.pageSize)
	var rowsExamined, indexedSeeks int64
	exhausted := false
	for len(entries) < w.pageSize {
		entry, depth, err := w.popFrontier(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			exhausted = true
			break
		}
		if err != nil {
			return nil, w.rollbackPage(ctx, err)
		}
		rowsExamined++
		entries = append(entries, entry)

		if entry.Node.ParentID != nil {
			indexedSeeks++
			parentPath := "/"
			if slash := strings.LastIndexByte(entry.Path, '/'); slash > 0 {
				parentPath = entry.Path[:slash]
			}
			if err := w.seedNext(ctx, *entry.Node.ParentID, parentPath, depth,
				entry.Node.Name, entry.Node.ID, false); err != nil {
				return nil, w.rollbackPage(ctx, err)
			}
		}
		if entry.Node.IsDir() {
			indexedSeeks++
			if err := w.seedNext(ctx, entry.Node.ID, entry.Path, depth+1, "", 0,
				true); err != nil {
				return nil, w.rollbackPage(ctx, err)
			}
		}
	}
	if _, err := w.tx.ExecContext(ctx, `RELEASE walk_page`); err != nil {
		return nil, w.rollbackPage(ctx,
			fmt.Errorf("walking tree snapshot: committing page: %w", err))
	}
	w.done = exhausted
	w.stats.Pages++
	w.stats.EntriesReturned += int64(len(entries))
	w.stats.LastPageRowsExamined = rowsExamined
	w.stats.LastPageIndexedSeeks = indexedSeeks
	if len(entries) == 0 {
		return nil, io.EOF
	}
	return entries, nil
}

func (w *Walker) popFrontier(ctx context.Context) (WalkEntry, int, error) {
	var (
		entry WalkEntry
		depth int
	)
	err := w.tx.QueryRowContext(ctx, `
		SELECT f.path, f.depth, `+nodeCols+`
		FROM walk_frontier AS f
		JOIN nodes AS n ON n.id = f.node_id
		LEFT JOIN content_versions AS cv
			ON cv.node_id = n.id AND cv.version_id = n.current_version_id
		ORDER BY f.path, f.node_id
		LIMIT 1`,
	).Scan(&entry.Path, &depth,
		&entry.Node.ID, &entry.Node.ParentID, &entry.Node.Name, &entry.Node.Kind,
		&entry.Node.CurrentVersionID, &entry.Node.BlobHash, &entry.Node.Size,
		&entry.Node.MimeType, &entry.Node.Revision, &entry.Node.CreatedAt,
		&entry.Node.ModifiedAt, &entry.Node.TrashedAt,
	)
	if err != nil {
		return WalkEntry{}, 0, err
	}
	if _, err := w.tx.ExecContext(ctx,
		`DELETE FROM walk_frontier WHERE path = ? AND node_id = ?`,
		entry.Path, entry.Node.ID); err != nil {
		return WalkEntry{}, 0, fmt.Errorf("walking tree snapshot: advancing frontier: %w", err)
	}
	return entry, depth, nil
}

func (w *Walker) seedNext(
	ctx context.Context, parentID int64, parentPath string, depth int,
	afterName string, afterID int64, first bool,
) error {
	var id int64
	var name string
	query := `
		SELECT n.id, n.name
		FROM nodes AS n
		WHERE n.parent_id = ? AND (? OR n.trashed_at IS NULL)`
	args := []any{parentID, w.includeTrashed}
	if !first {
		query += ` AND (n.name > ? OR (n.name = ? AND n.id > ?))`
		args = append(args, afterName, afterName, afterID)
	}
	query += ` ORDER BY n.name, n.id LIMIT 1`
	err := w.tx.QueryRowContext(ctx, query, args...).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("walking tree snapshot: seeking frontier: %w", err)
	}
	if depth > MaxWalkDepth {
		return fmt.Errorf("walking tree snapshot: walk depth exceeds %d", MaxWalkDepth)
	}
	path := parentPath + "/" + name
	if parentPath == "/" {
		path = "/" + name
	}
	if len(path) > MaxWalkPathBytes {
		return fmt.Errorf("walking tree snapshot: walk path exceeds %d bytes", MaxWalkPathBytes)
	}
	if _, err := w.tx.ExecContext(ctx,
		`INSERT INTO walk_frontier(path, node_id, depth) VALUES (?, ?, ?)`,
		path, id, depth); err != nil {
		return fmt.Errorf("walking tree snapshot: extending frontier: %w", err)
	}
	return nil
}

func (w *Walker) rollbackPage(ctx context.Context, cause error) error {
	cleanupCtx := context.WithoutCancel(ctx)
	_, rollbackErr := w.tx.ExecContext(cleanupCtx, `ROLLBACK TO walk_page`)
	_, releaseErr := w.tx.ExecContext(cleanupCtx, `RELEASE walk_page`)
	return errors.Join(cause, rollbackErr, releaseErr)
}

// Stats returns a race-safe snapshot of deterministic walker work counters.
func (w *Walker) Stats() WalkStats {
	if w == nil {
		return WalkStats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
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
