package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
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
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1, updated_at=? WHERE asset_id=?`, now, assetID); err != nil {
			return err
		}
		asset, err = photoAssetByIDQuery(ctx, tx, assetID)
		return err
	})
	return asset, err
}

type photoTrashGraph struct {
	roots  map[int64]int64
	assets map[int64][]string
	peers  map[string][]int64
}

// Restore traverses only descendants that the existing subtree restore will recover.
func loadPhotoTrashGraph(ctx context.Context, tx *sql.Tx, restore bool) (photoTrashGraph, error) {
	graph := photoTrashGraph{roots: map[int64]int64{}, assets: map[int64][]string{}, peers: map[string][]int64{}}
	filter := ""
	if restore {
		filter = " WHERE child.trashed_at=tree.stamp"
	}
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE tree(id, root, stamp) AS (
		SELECT id, id, trashed_at FROM nodes WHERE trash_name IS NOT NULL
		UNION ALL SELECT child.id, tree.root, tree.stamp FROM nodes child JOIN tree ON child.parent_id=tree.id`+filter+`)
		SELECT tree.id, tree.root, COALESCE(file.asset_id, '') FROM tree LEFT JOIN photo_files file ON file.node_id=tree.id ORDER BY tree.root, tree.id`)
	if err != nil {
		return graph, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var node, root int64
		var asset string
		if err := rows.Scan(&node, &root, &asset); err != nil {
			return graph, err
		}
		graph.roots[node] = root
		if asset != "" {
			graph.assets[root] = append(graph.assets[root], asset)
			graph.peers[asset] = append(graph.peers[asset], root)
		}
	}
	return graph, rows.Err()
}

func (g photoTrashGraph) group(root int64) []int64 {
	group := []int64{root}
	seen := map[int64]bool{root: true}
	for i := 0; i < len(group); i++ {
		for _, asset := range g.assets[group[i]] {
			for _, peer := range g.peers[asset] {
				if !seen[peer] {
					seen[peer] = true
					group = append(group, peer)
				}
			}
		}
	}
	return group
}

func photoTrashSelectionTx(ctx context.Context, tx *sql.Tx, eligibleSelection string, args []any, maxRoots int) (string, []any, bool, error) {
	graph, err := loadPhotoTrashGraph(ctx, tx, false)
	if err != nil {
		return "", nil, false, err
	}
	rows, err := tx.QueryContext(ctx, eligibleSelection, args...)
	if err != nil {
		return "", nil, false, err
	}
	var ordered []int64
	eligible := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return "", nil, false, err
		}
		ordered = append(ordered, id)
		eligible[id] = true
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", nil, false, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT asset_id, node_id FROM photo_files`)
	if err != nil {
		return "", nil, false, err
	}
	blocked := map[string]bool{}
	for rows.Next() {
		var asset string
		var node int64
		if err := rows.Scan(&asset, &node); err != nil {
			_ = rows.Close()
			return "", nil, false, err
		}
		if !eligible[graph.roots[node]] {
			blocked[asset] = true
		}
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", nil, false, err
	}
	seen := map[int64]bool{}
	var selected []int64
	more := false
	for _, root := range ordered {
		if seen[root] {
			continue
		}
		group := graph.group(root)
		allowed := true
		for _, peer := range group {
			seen[peer] = true
			allowed = allowed && eligible[peer]
			for _, asset := range graph.assets[peer] {
				allowed = allowed && !blocked[asset]
			}
		}
		if !allowed {
			continue
		}
		if maxRoots > 0 && len(selected) >= maxRoots {
			more = true
			break
		}
		for _, peer := range group {
			selected = append(selected, peer)
		}
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return "", nil, false, err
	}
	return `SELECT value FROM json_each(?)`, []any{string(encoded)}, more, nil
}

func (s *Store) restorePhotoGroupTx(ctx context.Context, tx *sql.Tx, n Node, ifRev int64, active bool) (Node, error) {
	if n.TrashedAt == nil {
		return Node{}, ErrNotTrashed
	}
	if ifRev != UnconditionalRev && n.Revision != ifRev {
		return Node{}, ErrStaleRevision
	}
	graph, err := loadPhotoTrashGraph(ctx, tx, true)
	if err != nil {
		return Node{}, err
	}
	root, ok := graph.roots[n.ID]
	if !ok {
		return Node{}, fmt.Errorf("node %d has no restorable trash root: %w", n.ID, ErrNotTrashed)
	}
	if root != n.ID {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_files WHERE node_id=?`, n.ID).Scan(&count); err != nil {
			return Node{}, err
		}
		if count == 0 {
			return Node{}, ErrNotTrashed
		}
	}
	for _, id := range graph.group(root) {
		peer, err := nodeByIDTx(tx, id)
		if err != nil {
			return Node{}, err
		}
		if active {
			_, err = s.restoreAuditedTx(ctx, tx, peer, peer.Revision)
		} else {
			target, targetErr := s.restoreTargetTx(tx, peer)
			if targetErr != nil {
				return Node{}, targetErr
			}
			_, err = s.restoreNodeTx(tx, peer, target, nowRFC3339())
		}
		if err != nil {
			return Node{}, err
		}
	}
	return nodeByIDTx(tx, n.ID)
}
