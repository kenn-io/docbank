package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"sort"
)

// TrashPhotoAsset moves every live member to recoverable trash atomically.
func (s *Store) TrashPhotoAsset(ctx context.Context, assetID string, revision int64) (PhotoAsset, error) {
	var asset PhotoAsset
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		asset, err = photoAssetForMutationTx(ctx, tx, assetID, revision)
		if err != nil {
			return err
		}
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		changed := false
		now := nowRFC3339()
		for _, file := range asset.Files {
			n, err := nodeByIDTx(tx, file.NodeID)
			if err != nil {
				return err
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
				return err
			}
			changed = true
		}
		if !changed {
			return fmt.Errorf("%w: photo has no live files to trash", ErrInvalidPhotoAsset)
		}
		asset, err = commitPhotoAssetTx(ctx, tx, asset, asset, "trash")
		return err
	})
	return asset, err
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
	return Node{}, ErrNotTrashed
}

func photoTrashGroupTx(ctx context.Context, tx *sql.Tx, root Node, restore bool) (photoTrashGroup, error) {
	group := photoTrashGroup{roots: map[int64]Node{root.ID: root}, assets: map[string]PhotoAsset{}}
	pending := []Node{root}
	for i := 0; i < len(pending); i++ {
		node := pending[i]
		filter := ""
		if restore {
			filter = " WHERE child.trashed_at=tree.stamp"
		}
		rows, err := tx.QueryContext(ctx, `WITH RECURSIVE tree(id,stamp) AS (
   SELECT id, trashed_at FROM nodes WHERE id=?
   UNION ALL SELECT child.id, tree.stamp FROM nodes child JOIN tree ON child.parent_id=tree.id`+filter+`)
   SELECT DISTINCT file.asset_id FROM tree JOIN photo_files file ON file.node_id=tree.id`, node.ID)
		if err != nil {
			return group, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return group, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
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
		return Node{}, ErrNotTrashed
	}
	if ifRev != UnconditionalRev && node.Revision != ifRev {
		return Node{}, ErrStaleRevision
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
	group, err := photoTrashGroupTx(ctx, tx, root, true)
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
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var order []int64
	state := map[int64]int{}
	var visit func(int64) error
	visit = func(id int64) error {
		if state[id] == 2 {
			return nil
		}
		if state[id] == 1 {
			return fmt.Errorf("cyclic trash parent dependency: %w", ErrNotTrashed)
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

func photoTrashSelectionTx(ctx context.Context, tx *sql.Tx, eligibleWhere string, args []any, maxRoots int) (string, []any, bool, error) {
	eligibleSelection := `SELECT id, kind, EXISTS(SELECT 1 FROM photo_files WHERE node_id=nodes.id) FROM nodes WHERE ` + eligibleWhere + ` ORDER BY trashed_at ASC, id ASC`
	seen := map[int64]bool{}
	var selected []int64
	more := false
	for offset := 0; ; offset += 100 {
		rows, err := tx.QueryContext(ctx, eligibleSelection+` LIMIT 100 OFFSET ?`, append(append([]any{}, args...), offset)...)
		if err != nil {
			return "", nil, false, err
		}
		type candidate struct {
			id    int64
			kind  string
			photo bool
		}
		var candidates []candidate
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.id, &item.kind, &item.photo); err != nil {
				_ = rows.Close()
				return "", nil, false, err
			}
			candidates = append(candidates, item)
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return "", nil, false, err
		}
		for _, candidate := range candidates {
			id := candidate.id
			if seen[id] {
				continue
			}
			if candidate.kind == "file" && !candidate.photo {
				if maxRoots > 0 && len(selected) >= maxRoots {
					more = true
					break
				}
				selected = append(selected, id)
				continue
			}
			node, err := nodeByIDTx(tx, id)
			if err != nil {
				return "", nil, false, err
			}
			group, err := photoTrashGroupTx(ctx, tx, node, false)
			if err != nil {
				return "", nil, false, err
			}
			allowed := !group.live
			for peer := range group.roots {
				seen[peer] = true
				if peer == id {
					continue
				}
				var eligible bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE `+eligibleWhere+` AND id=?)`, append(append([]any{}, args...), peer)...).Scan(&eligible); err != nil {
					return "", nil, false, err
				}
				allowed = allowed && eligible
			}
			if !allowed {
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
		if more || len(candidates) < 100 {
			break
		}
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return "", nil, false, err
	}
	return `SELECT value FROM json_each(?)`, []any{string(encoded)}, more, nil
}
