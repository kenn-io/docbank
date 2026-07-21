package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Trash soft-deletes a live node and its live subtree as a unit. All subtree
// rows share one trashed_at stamp; only the top node records its original
// location for restore. Unless ifRev is UnconditionalRev, the mutation
// fails with ErrStaleRevision unless ifRev matches the node's current
// revision. Returns the node as it stands after trashing, plus its
// pre-trash path — computed inside the same transaction, because trashing
// re-parents the node (making the path uncomputable afterwards) and a
// concurrent ancestor move could stale a path captured beforehand.
func (s *Store) Trash(ctx context.Context, id, ifRev int64) (Node, string, error) {
	if id == s.rootID {
		return Node{}, "", ErrIsRoot
	}
	var trashed Node
	var origPath string
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, id)
		if err != nil {
			return err
		}
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if active {
			trashed, origPath, err = s.trashAuditedTx(ctx, tx, n, ifRev)
			return err
		}
		if n.TrashedAt != nil {
			return fmt.Errorf("node %d already trashed: %w", id, ErrNotFound)
		}
		if ifRev != UnconditionalRev && n.Revision != ifRev {
			return fmt.Errorf("node %d at revision %d, expected %d: %w",
				id, n.Revision, ifRev, ErrStaleRevision)
		}
		if origPath, err = pathOf(ctx, tx, id); err != nil {
			return err
		}
		if err := s.trashNodeTx(tx, n, nowRFC3339()); err != nil {
			return err
		}
		trashed, err = nodeByIDTx(tx, id)
		return err
	})
	if err != nil {
		return Node{}, "", err
	}
	return trashed, origPath, nil
}

// TrashPath resolves path and trashes the node inside one transaction, so a
// concurrent move cannot relocate the node or an ancestor between resolution
// and mutation. Returns the node that was trashed and its canonical
// pre-trash path (see Trash).
func (s *Store) TrashPath(ctx context.Context, path string) (Node, string, error) {
	return s.TrashPathRevision(ctx, path, UnconditionalRev)
}

// TrashPathRevision is TrashPath with an exact optional revision precondition.
func (s *Store) TrashPathRevision(
	ctx context.Context, path string, ifRev int64,
) (Node, string, error) {
	var trashed Node
	var origPath string
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByPath(ctx, tx, s.rootID, path)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", path, err)
		}
		if n.ID == s.rootID {
			return ErrIsRoot
		}
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if active {
			trashed, origPath, err = s.trashAuditedTx(
				ctx, tx, n, ifRev,
			)
			return err
		}
		if ifRev != UnconditionalRev && n.Revision != ifRev {
			return fmt.Errorf("node %d at revision %d, expected %d: %w",
				n.ID, n.Revision, ifRev, ErrStaleRevision)
		}
		if origPath, err = pathOf(ctx, tx, n.ID); err != nil {
			return err
		}
		if err := s.trashNodeTx(tx, n, nowRFC3339()); err != nil {
			return err
		}
		trashed, err = nodeByIDTx(tx, n.ID)
		return err
	})
	if err != nil {
		return Node{}, "", err
	}
	return trashed, origPath, nil
}

// trashNodeTx trashes a live node n (pre-checked by the caller) and its live
// subtree within the caller's transaction.
func (s *Store) trashNodeTx(tx *sql.Tx, n Node, now string) error {
	id := n.ID
	if _, err := tx.Exec(`
			WITH RECURSIVE subtree(id) AS (
				SELECT id FROM nodes WHERE id = ?
				UNION ALL
				SELECT n.id FROM nodes n
				JOIN subtree st ON n.parent_id = st.id
				WHERE n.trashed_at IS NULL
			)
			UPDATE nodes SET trashed_at = ?, revision = revision + 1, modified_at = ?
			WHERE id IN (SELECT id FROM subtree)`, id, now, now); err != nil {
		return fmt.Errorf("trashing subtree of node %d: %w", id, err)
	}
	// Detach the trash root from its original parent (parent_id has ON
	// DELETE CASCADE, so leaving it in place would silently destroy this
	// trash root if the original parent were ever hard-deleted). The
	// origin travels in trash_parent/trash_name; parent_id points at the
	// tree root because the one_root index forbids a second NULL parent.
	if _, err := tx.Exec(
		`UPDATE nodes SET parent_id = ?, trash_parent = ?, trash_name = ? WHERE id = ?`,
		s.rootID, *n.ParentID, n.Name, id); err != nil {
		return fmt.Errorf("recording trash origin of node %d: %w", id, err)
	}
	return bumpRevisionTx(tx, *n.ParentID, now)
}

// nextFreeNameTx finds the smallest free suffix candidate among live
// siblings: name, "name (2)", "name (3)", ...
func nextFreeNameTx(tx *sql.Tx, parentID int64, name string) (string, error) {
	base, ext := splitSuffix(name)
	for n := 1; ; n++ {
		candidate := suffixedName(base, ext, n)
		var one int
		err := tx.QueryRow(
			`SELECT 1 FROM nodes WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`,
			parentID, candidate).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("probing name %q: %w", candidate, err)
		}
	}
}

// Restore returns a trash root to its original location (or the tree root if
// that location is gone), re-suffixing on conflict. Descendants trashed in
// earlier separate operations stay trashed. Unless ifRev is
// UnconditionalRev, the mutation fails with ErrStaleRevision unless ifRev
// matches the node's current revision. The returned canonical path is captured
// in the restore transaction with the returned node.
func (s *Store) Restore(ctx context.Context, id, ifRev int64) (Node, string, error) {
	var restored Node
	var restoredPath string
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, id)
		if err != nil {
			return err
		}
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if active {
			restored, err = s.restoreAuditedTx(ctx, tx, n, ifRev)
		} else {
			if n.TrashedAt == nil {
				return fmt.Errorf("node %d: %w", id, ErrNotTrashed)
			}
			if ifRev != UnconditionalRev && n.Revision != ifRev {
				return fmt.Errorf("node %d at revision %d, expected %d: %w",
					id, n.Revision, ifRev, ErrStaleRevision)
			}
			target, targetErr := s.restoreTargetTx(tx, n)
			if targetErr != nil {
				return targetErr
			}
			restored, err = s.restoreNodeTx(tx, n, target, nowRFC3339())
		}
		if err == nil {
			restoredPath, err = pathOf(ctx, tx, restored.ID)
		}
		return err
	})
	if err != nil {
		return Node{}, "", err
	}
	return restored, restoredPath, nil
}

type restoreTarget struct {
	destID         int64
	originParentID *int64
	originalName   string
	finalName      string
}

func (s *Store) restoreTargetTx(tx *sql.Tx, node Node) (restoreTarget, error) {
	var trashParent sql.NullInt64
	var trashName sql.NullString
	if err := tx.QueryRow(
		`SELECT trash_parent, trash_name FROM nodes WHERE id = ?`, node.ID,
	).Scan(&trashParent, &trashName); err != nil {
		return restoreTarget{}, fmt.Errorf("reading trash origin of node %d: %w", node.ID, err)
	}
	if !trashName.Valid {
		return restoreTarget{}, fmt.Errorf("node %d is not a trash root: %w", node.ID, ErrNotTrashed)
	}
	target := restoreTarget{destID: s.rootID, originalName: trashName.String}
	if trashParent.Valid {
		originParentID := trashParent.Int64
		target.originParentID = &originParentID
		if _, err := liveDirTx(tx, originParentID); err == nil {
			target.destID = originParentID
		} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNotDir) {
			return restoreTarget{}, err
		}
	}
	finalName, err := nextFreeNameTx(tx, target.destID, target.originalName)
	if err != nil {
		return restoreTarget{}, err
	}
	target.finalName = finalName
	return target, nil
}

func (s *Store) restoreNodeTx(
	tx *sql.Tx, node Node, target restoreTarget, now string,
) (Node, error) {
	// Reattach the top node FIRST — parent, final (possibly re-suffixed)
	// name, and liveness in one statement. Un-trashing before renaming
	// would trip the live-sibling unique index whenever the original
	// name was reused while this node sat in the trash.
	if _, err := tx.Exec(
		`UPDATE nodes SET parent_id = ?, name = ?, trashed_at = NULL,
		        trash_parent = NULL, trash_name = NULL,
		        revision = revision + 1, modified_at = ? WHERE id = ?`,
		target.destID, target.finalName, now, node.ID); err != nil {
		return Node{}, fmt.Errorf("reattaching node %d: %w", node.ID, err)
	}
	// Then un-trash the descendants that share this operation's stamp.
	// Their (parent, name) pairs cannot conflict: nothing could be
	// created under a trashed directory in the interim.
	if _, err := tx.Exec(`
		WITH RECURSIVE subtree(id) AS (
			SELECT id FROM nodes WHERE id = ?
			UNION ALL
			SELECT n.id FROM nodes n
			JOIN subtree st ON n.parent_id = st.id
			WHERE n.trashed_at = ?
		)
		UPDATE nodes SET trashed_at = NULL, revision = revision + 1, modified_at = ?
		WHERE id IN (SELECT id FROM subtree) AND trashed_at = ?`,
		node.ID, *node.TrashedAt, now, *node.TrashedAt); err != nil {
		return Node{}, fmt.Errorf("restoring subtree of node %d: %w", node.ID, err)
	}
	if err := bumpRevisionTx(tx, target.destID, now); err != nil {
		return Node{}, err
	}
	return nodeByIDTx(tx, node.ID)
}

// TrashedRoots lists restorable trash roots, newest first.
func (s *Store) TrashedRoots(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeCols+` FROM `+nodeFrom+`
		 WHERE n.trash_name IS NOT NULL ORDER BY n.trashed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing trash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var roots []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		roots = append(roots, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing trash: %w", err)
	}
	return roots, nil
}

// TrashEmptyResult reports one trash-empty dry run or execution.
type TrashEmptyResult struct {
	Candidates int64
	Deleted    int64
	More       bool
	Run        bool
}

// TrashEmpty reports trash roots older than the cutoff and, when run is true,
// hard-deletes them. A zero age selects every trash root, including any with
// a future timestamp caused by clock skew. Subtrees follow via ON DELETE
// CASCADE.
func (s *Store) TrashEmpty(ctx context.Context, olderThan time.Duration, run bool) (TrashEmptyResult, error) {
	return s.trashEmpty(ctx, olderThan, 0, run)
}

// TrashEmptyBounded reports or deletes at most maxRoots eligible trash roots.
// More reports whether another eligible root existed beyond this batch.
func (s *Store) TrashEmptyBounded(
	ctx context.Context, olderThan time.Duration, maxRoots int, run bool,
) (TrashEmptyResult, error) {
	if maxRoots <= 0 {
		return TrashEmptyResult{}, errors.New("maximum trash roots must be positive")
	}
	return s.trashEmpty(ctx, olderThan, maxRoots, run)
}

func (s *Store) trashEmpty(
	ctx context.Context, olderThan time.Duration, maxRoots int, run bool,
) (TrashEmptyResult, error) {
	rep := TrashEmptyResult{Run: run}
	where := `trash_name IS NOT NULL`
	var args []any
	if olderThan > 0 {
		where += ` AND trashed_at <= ?`
		args = append(args, time.Now().UTC().Add(-olderThan).Format(timestampLayout))
	}
	selection := `SELECT id FROM nodes WHERE ` + where + ` ORDER BY trashed_at ASC, id ASC`
	selectionArgs := append([]any(nil), args...)
	if maxRoots > 0 {
		selection += ` LIMIT ?`
		selectionArgs = append(selectionArgs, maxRoots)
	}
	runTx := s.withStorageTx
	if run {
		runTx = s.withLogicalTx
	}
	err := runTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM (`+selection+`)`, selectionArgs...).Scan(&rep.Candidates); err != nil {
			return fmt.Errorf("counting trash-empty candidates: %w", err)
		}
		if maxRoots > 0 {
			moreArgs := append(append([]any(nil), args...), maxRoots)
			if err := tx.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM nodes WHERE `+where+` ORDER BY trashed_at ASC, id ASC LIMIT 1 OFFSET ?)`,
				moreArgs...,
			).Scan(&rep.More); err != nil {
				return fmt.Errorf("checking for more trash-empty candidates: %w", err)
			}
		}
		if !run {
			return nil
		}
		// One trash-empty operation advances each affected tag once, even when
		// several assignments disappear. A row-level node_tags trigger would
		// instead expose physical cascade cardinality as revision semantics.
		if _, err := tx.Exec(`
			WITH RECURSIVE roots(id) AS (`+selection+`),
			doomed(id) AS (
			  SELECT id FROM roots
			  UNION ALL
			  SELECT n.id FROM nodes n JOIN doomed d ON n.parent_id = d.id
			)
			UPDATE tags SET revision = revision + 1
			WHERE id IN (
			  SELECT nt.tag_id FROM node_tags nt JOIN doomed d ON d.id = nt.node_id
			)`, selectionArgs...); err != nil {
			return fmt.Errorf("advancing tags affected by trash empty: %w", err)
		}
		res, err := tx.Exec(
			`WITH roots(id) AS (`+selection+`) DELETE FROM nodes WHERE id IN (SELECT id FROM roots)`,
			selectionArgs...,
		)
		if err != nil {
			return fmt.Errorf("emptying trash: %w", err)
		}
		rep.Deleted, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("emptying trash: %w", err)
		}
		return nil
	})
	if err != nil {
		return TrashEmptyResult{}, err
	}
	return rep, nil
}
