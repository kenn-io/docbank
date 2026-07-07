package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BeginIngest records the start of an ingest run and returns its id.
func (s *Store) BeginIngest(ctx context.Context, sourceKind, sourceDesc string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ingests (started_at, source_kind, source_desc) VALUES (?, ?, ?)`,
		nowRFC3339(), sourceKind, sourceDesc)
	if err != nil {
		return 0, fmt.Errorf("recording ingest start: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading ingest id: %w", err)
	}
	return id, nil
}

// resolveIngestNameTx applies the import idempotency rule: enumerate all
// live suffix candidates of name under parentID; if any has blobHash the
// file is already imported (skip); otherwise return the smallest free
// candidate name.
func resolveIngestNameTx(tx *sql.Tx, parentID int64, name, blobHash string) (string, bool, error) {
	base, ext := splitSuffix(name)
	rows, err := tx.Query(
		`SELECT name, COALESCE(blob_hash, '') FROM nodes
		 WHERE parent_id = ? AND trashed_at IS NULL AND kind = 'file'`, parentID)
	if err != nil {
		return "", false, fmt.Errorf("listing siblings for %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	taken := map[int]bool{}
	for rows.Next() {
		var sibName, sibHash string
		if err := rows.Scan(&sibName, &sibHash); err != nil {
			return "", false, fmt.Errorf("scanning sibling: %w", err)
		}
		n, ok := parseSuffix(sibName, base, ext)
		if !ok {
			continue
		}
		if sibHash == blobHash {
			return "", true, nil // already imported (possibly under a suffix)
		}
		taken[n] = true
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("listing siblings for %q: %w", name, err)
	}
	// Directories can occupy candidate names too; they don't carry content,
	// but their names are still taken. Probe them via the unique index by
	// walking ordinals and consulting taken plus a dir-name check.
	n := 1
	for {
		if !taken[n] {
			candidate := suffixedName(base, ext, n)
			var one int
			err := tx.QueryRow(
				`SELECT 1 FROM nodes WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`,
				parentID, candidate).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return candidate, false, nil
			}
			if err != nil {
				return "", false, fmt.Errorf("probing name %q: %w", candidate, err)
			}
		}
		n++
	}
}

// IngestFile imports one already-durable blob as a node under parentID,
// applying the idempotency rule and recording provenance. Returns
// added=false when the content is already present under a candidate name.
func (s *Store) IngestFile(ctx context.Context, ingestID, parentID int64, name, blobHash string, size int64, mimeType, originalPath, originalMtime string) (Node, bool, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Node{}, false, err
	}
	var (
		created Node
		added   bool
	)
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		finalName, skip, err := resolveIngestNameTx(tx, parentID, name, blobHash)
		if err != nil {
			return err
		}
		if skip {
			return nil
		}
		created, err = s.createFileTx(tx, parentID, finalName, blobHash, size, mimeType)
		if err != nil {
			return err
		}
		var mtime any
		if originalMtime != "" {
			mtime = originalMtime
		}
		if _, err := tx.Exec(
			`INSERT INTO provenance (node_id, ingest_id, original_path, original_mtime)
			 VALUES (?, ?, ?, ?)`,
			created.ID, ingestID, originalPath, mtime); err != nil {
			return fmt.Errorf("recording provenance for %q: %w", finalName, err)
		}
		added = true
		return nil
	})
	if err != nil {
		return Node{}, false, err
	}
	return created, added, nil
}
