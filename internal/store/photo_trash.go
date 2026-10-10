package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// TrashPhotoAsset moves every live member to recoverable trash atomically.
func (s *Store) TrashPhotoAsset(ctx context.Context, assetID string, revision int64) (PhotoAsset, error) {
	var asset PhotoAsset
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		current, err := s.photoAssetForMutationTx(ctx, tx, assetID, revision)
		if err != nil {
			return err
		}
		asset, err = s.trashPhotoAssetTx(ctx, tx, current)
		return err
	})
	return asset, err
}

func (s *Store) trashPhotoAssetTx(ctx context.Context, tx *sql.Tx, asset PhotoAsset) (PhotoAsset, error) {
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return PhotoAsset{}, err
	}
	changed := false
	now := nowRFC3339()
	for _, file := range asset.Files {
		n, err := nodeByIDTx(tx, file.NodeID)
		if err != nil {
			return PhotoAsset{}, err
		}
		if n.TrashedAt != nil {
			continue
		}
		if active {
			_, _, err = s.trashAuditedTx(ctx, tx, n, n.Revision)
		} else {
			err = s.trashNodeTx(tx, n, now)
		}
		if err != nil {
			return PhotoAsset{}, err
		}
		changed = true
	}
	if !changed {
		return PhotoAsset{}, fmt.Errorf("%w: photo has no live files to trash", ErrInvalidPhotoAsset)
	}
	return commitPhotoAssetTx(ctx, tx, asset, asset, "trash")
}

type photoTrashGroup struct {
	roots  map[int64]Node
	assets map[string]PhotoAsset
	live   bool
}

func trashRootTx(ctx context.Context, tx *sql.Tx, node Node) (Node, error) {
	for node.TrashedAt != nil {
		var name sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT trash_name FROM nodes WHERE id=?`, node.ID).Scan(&name); err != nil {
			return Node{}, err
		}
		if name.Valid {
			return node, nil
		}
		if node.ParentID == nil {
			break
		}
		parent, err := nodeByIDTx(tx, *node.ParentID)
		if err != nil {
			return Node{}, err
		}
		if parent.TrashedAt == nil || *parent.TrashedAt != *node.TrashedAt {
			break
		}
		node = parent
	}
	return Node{}, fmt.Errorf("node %d has no trash root: %w", node.ID, ErrNotTrashed)
}

func photoTrashGroupTx(ctx context.Context, tx *sql.Tx, root Node) (photoTrashGroup, error) {
	group := photoTrashGroup{roots: map[int64]Node{root.ID: root}, assets: map[string]PhotoAsset{}}
	pending := []Node{root}
	for i := 0; i < len(pending); i++ {
		node := pending[i]
		ids, err := func() (ids []string, err error) {
			rows, err := tx.QueryContext(ctx, `WITH RECURSIVE tree(id) AS (
 SELECT id FROM nodes WHERE id=?
 UNION ALL SELECT child.id FROM nodes child JOIN tree ON child.parent_id=tree.id)
 SELECT DISTINCT asset_id FROM photo_files WHERE asset_id IS NOT NULL AND node_id IN (SELECT id FROM tree)`, node.ID)
			if err != nil {
				return nil, err
			}
			defer func() {
				if closeErr := rows.Close(); err == nil {
					err = closeErr
				}
			}()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return nil, err
				}
				ids = append(ids, id)
			}
			return ids, rows.Err()
		}()
		if err != nil {
			return group, err
		}
		for _, id := range ids {
			if _, seen := group.assets[id]; seen {
				continue
			}
			asset, err := photoAssetByIDQuery(ctx, tx, id)
			if err != nil {
				return group, err
			}
			group.assets[id] = asset
			for _, file := range asset.Files {
				member, err := nodeByIDTx(tx, file.NodeID)
				if err != nil {
					return group, err
				}
				if member.TrashedAt == nil {
					group.live = true
					continue
				}
				peer, err := trashRootTx(ctx, tx, member)
				if err != nil {
					return group, err
				}
				if _, seen := group.roots[peer.ID]; !seen {
					group.roots[peer.ID] = peer
					pending = append(pending, peer)
				}
			}
		}
	}
	return group, nil
}

func restoreRootTx(ctx context.Context, s *Store, tx *sql.Tx, node Node, active bool) (Node, error) {
	if active {
		return s.restoreAuditedTx(ctx, tx, node, node.Revision)
	}
	target, err := s.restoreTargetTx(tx, node)
	if err != nil {
		return Node{}, err
	}
	return s.restoreNodeTx(tx, node, target, nowRFC3339())
}

func (s *Store) restorePhotoGroupTx(ctx context.Context, tx *sql.Tx, node Node, ifRev int64, active bool) (Node, error) {
	if node.TrashedAt == nil {
		return Node{}, fmt.Errorf("node %d: %w", node.ID, ErrNotTrashed)
	}
	if ifRev != UnconditionalRev && node.Revision != ifRev {
		return Node{}, fmt.Errorf("node %d at revision %d, expected %d: %w",
			node.ID, node.Revision, ifRev, ErrStaleRevision)
	}
	_, owned, err := photoAssetOwningNodeTx(ctx, tx, node.ID)
	if err != nil {
		return Node{}, err
	}
	if !owned && !node.IsDir() {
		return restoreRootTx(ctx, s, tx, node, active)
	}
	root := node
	if owned {
		root, err = trashRootTx(ctx, tx, node)
		if err != nil {
			return Node{}, err
		}
	}
	group, err := photoTrashGroupTx(ctx, tx, root)
	if err != nil {
		return Node{}, err
	}
	if len(group.assets) == 0 {
		return restoreRootTx(ctx, s, tx, root, active)
	}
	order, err := photoRestoreOrderTx(ctx, tx, group.roots)
	if err != nil {
		return Node{}, err
	}
	for _, id := range order {
		current, err := nodeByIDTx(tx, id)
		if err != nil {
			return Node{}, err
		}
		if _, err = restoreRootTx(ctx, s, tx, current, active); err != nil {
			return Node{}, err
		}
	}
	for _, asset := range group.assets {
		if _, err := commitPhotoAssetTx(ctx, tx, asset, asset, "restore"); err != nil {
			return Node{}, err
		}
	}
	return nodeByIDTx(tx, node.ID)
}

func photoRestoreOrderTx(ctx context.Context, tx *sql.Tx, roots map[int64]Node) ([]int64, error) {
	dependencies := map[int64]int64{}
	var ids []int64
	for id := range roots {
		ids = append(ids, id)
		var parent sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT trash_parent FROM nodes WHERE id=?`, id).Scan(&parent); err != nil {
			return nil, err
		}
		for parent.Valid {
			if _, selected := roots[parent.Int64]; selected {
				dependencies[id] = parent.Int64
				break
			}
			err := tx.QueryRowContext(ctx, `SELECT parent_id FROM nodes WHERE id=?`, parent.Int64).Scan(&parent)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return nil, err
			}
		}
	}
	slices.Sort(ids)
	var order []int64
	state := map[int64]int{}
	var visit func(int64) error
	visit = func(id int64) error {
		if state[id] == 2 {
			return nil
		}
		if state[id] == 1 {
			return fmt.Errorf("trash root %d has a cyclic original-parent dependency", id)
		}
		state[id] = 1
		if parent, ok := dependencies[id]; ok {
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[id] = 2
		order = append(order, id)
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// photoTrashSelection is one trash-empty batch. Held counts eligible roots kept
// because a connected photo member is live, too new, or retained.
type photoTrashSelection struct {
	query string
	args  []any
	more  bool
	held  int64
}

func photoTrashSelectionTx(ctx context.Context, tx *sql.Tx, eligibleWhere string, args []any, maxRoots int) (photoTrashSelection, error) {
	// Walk upward from photo members once, so ordinary folders need no inspection.
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE photo_nodes(id) AS (
 SELECT node_id FROM photo_files WHERE asset_id IS NOT NULL
 UNION SELECT n.parent_id FROM nodes n JOIN photo_nodes p ON n.id=p.id WHERE n.parent_id IS NOT NULL)
 SELECT id, id IN (SELECT id FROM photo_nodes) FROM nodes WHERE `+eligibleWhere+` ORDER BY trashed_at ASC, id ASC`, args...)
	if err != nil {
		return photoTrashSelection{}, err
	}
	defer func() { _ = rows.Close() }()
	type candidate struct {
		id    int64
		photo bool
	}
	var candidates []candidate
	eligible := map[int64]bool{}
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.photo); err != nil {
			return photoTrashSelection{}, err
		}
		candidates = append(candidates, item)
		eligible[item.id] = true
	}
	if err := rows.Err(); err != nil {
		return photoTrashSelection{}, err
	}
	if err := rows.Close(); err != nil {
		return photoTrashSelection{}, err
	}
	seen := map[int64]bool{}
	var selected []int64
	more := false
	var held int64
	for _, candidate := range candidates {
		id := candidate.id
		if seen[id] {
			continue
		}
		if !candidate.photo {
			if maxRoots > 0 && len(selected) >= maxRoots {
				more = true
				break
			}
			selected = append(selected, id)
			continue
		}
		node, err := nodeByIDTx(tx, id)
		if err != nil {
			return photoTrashSelection{}, err
		}
		group, err := photoTrashGroupTx(ctx, tx, node)
		if err != nil {
			return photoTrashSelection{}, err
		}
		allowed := !group.live
		for peer := range group.roots {
			seen[peer] = true
			allowed = allowed && eligible[peer]
		}
		if !allowed {
			for peer := range group.roots {
				if eligible[peer] {
					held++
				}
			}
			continue
		}
		if maxRoots > 0 && len(selected) >= maxRoots {
			more = true
			break
		}
		for peer := range group.roots {
			selected = append(selected, peer)
		}
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return photoTrashSelection{}, err
	}
	return photoTrashSelection{
		query: `SELECT value FROM json_each(?)`, args: []any{string(encoded)}, more: more, held: held,
	}, nil
}
