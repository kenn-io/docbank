package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// photoOwnerFoldersName names the top-level folder whose children named for
// enrolled owners are those owners' private folders.
const photoOwnerFoldersName = "photos"

type photoOwnerContextKey struct{}

// WithPhotoOwner selects the photo owner a request acts as. A context without
// a selection sees every node.
func WithPhotoOwner(ctx context.Context, ownerID string) context.Context {
	return context.WithValue(ctx, photoOwnerContextKey{}, ownerID)
}

// PhotoOwnerFromContext returns the selected owner id and whether the context
// selects an owner at all.
func PhotoOwnerFromContext(ctx context.Context) (string, bool) {
	ownerID, ok := ctx.Value(photoOwnerContextKey{}).(string)
	return ownerID, ok
}

// photoTrashAwareParentSQL is a node's parent for ownership: a trash root's
// origin, otherwise its parent.
func photoTrashAwareParentSQL(alias string) string {
	return `CASE WHEN ` + alias + `.trash_name IS NOT NULL THEN ` + alias + `.trash_parent ELSE ` + alias + `.parent_id END`
}

// photoOwnerFolderSQL matches a node aliased pof that is an owner folder: a
// direct child of the top-level photos folder named for an enrolled owner.
func photoOwnerFolderSQL() string {
	return `pof.name IN (SELECT person_id FROM photo_owners)
		AND EXISTS (SELECT 1 FROM nodes pot JOIN nodes por ON por.id=` + photoTrashAwareParentSQL("pot") + `
			WHERE pot.id=` + photoTrashAwareParentSQL("pof") + ` AND pot.name='` + photoOwnerFoldersName + `' AND por.parent_id IS NULL)`
}

// photoNodeVisibleSQL is the one visibility rule: with an owner selected, a
// node is hidden when it lies in another owner's folder. The node
// expression's own arguments must precede the returned ones.
func photoNodeVisibleSQL(ctx context.Context, nodeExpr string) (string, []any) {
	ownerID, ok := PhotoOwnerFromContext(ctx)
	if !ok {
		return "1=1", nil
	}
	return `NOT EXISTS (
		WITH RECURSIVE photo_owner_walk(id) AS (
			SELECT pvn.id FROM nodes pvn WHERE pvn.id=` + nodeExpr + `
			UNION ALL
			SELECT pvp.id FROM photo_owner_walk pvc JOIN nodes pvc_n ON pvc_n.id=pvc.id
			JOIN nodes pvp ON pvp.id=` + photoTrashAwareParentSQL("pvc_n") + `
		)
		SELECT 1 FROM photo_owner_walk pw JOIN nodes pof ON pof.id=pw.id
		WHERE pof.name<>? AND ` + photoOwnerFolderSQL() + `
	)`, []any{ownerID}
}

// checkPhotoNodeVisibleTx returns ErrNotFound when the request may not see
// the node.
func checkPhotoNodeVisibleTx(ctx context.Context, q rowQuerier, nodeID int64) error {
	return checkPhotoVisibleTx(ctx, q, "?", nodeID)
}

// checkPhotoVersionVisibleTx applies the rule to a content version's node.
func checkPhotoVersionVisibleTx(ctx context.Context, q rowQuerier, versionID string) error {
	return checkPhotoVisibleTx(ctx, q, "(SELECT node_id FROM content_versions WHERE version_id=?)", versionID)
}

func checkPhotoVisibleTx(ctx context.Context, q rowQuerier, nodeExpr string, id any) error {
	if _, ok := PhotoOwnerFromContext(ctx); !ok {
		return nil
	}
	predicate, args := photoNodeVisibleSQL(ctx, nodeExpr)
	var visible bool
	if err := q.QueryRowContext(ctx, `SELECT `+predicate, append([]any{id}, args...)...).Scan(&visible); err != nil {
		return fmt.Errorf("checking photo visibility: %w", err)
	}
	if !visible {
		return ErrNotFound
	}
	return nil
}

// PathVisible reports whether the request may see path's deepest existing
// node. It walks without the owner rule, so a hidden folder's missing
// descendants still answer false.
func (s *Store) PathVisible(ctx context.Context, p string) (bool, error) {
	if _, ok := PhotoOwnerFromContext(ctx); !ok {
		return true, nil
	}
	nodeID, err := s.deepestExistingNode(ctx, p)
	if err != nil {
		return false, err
	}
	err = checkPhotoNodeVisibleTx(ctx, s.db, nodeID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// PathPhotoOwner names the enrolled owner whose folder holds path's deepest
// existing node, or "" when the path lies outside every owner folder.
func (s *Store) PathPhotoOwner(ctx context.Context, p string) (string, error) {
	nodeID, err := s.deepestExistingNode(ctx, p)
	if err != nil {
		return "", err
	}
	var ownerID string
	err = s.db.QueryRowContext(ctx, `
		WITH RECURSIVE photo_owner_walk(id) AS (
			SELECT ?
			UNION ALL
			SELECT pvp.id FROM photo_owner_walk pvc JOIN nodes pvc_n ON pvc_n.id=pvc.id
			JOIN nodes pvp ON pvp.id=`+photoTrashAwareParentSQL("pvc_n")+`
		)
		SELECT pof.name FROM photo_owner_walk pw JOIN nodes pof ON pof.id=pw.id
		WHERE `+photoOwnerFolderSQL()+` LIMIT 1`, nodeID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("finding the photo owner of %q: %w", p, err)
	}
	return ownerID, nil
}

// deepestExistingNode walks path's live segments without the owner rule.
func (s *Store) deepestExistingNode(ctx context.Context, p string) (int64, error) {
	nodeID := s.rootID
	for _, segment := range splitPath(p) {
		name, nameErr := NormalizeName(segment)
		if nameErr != nil {
			break
		}
		next, childErr := childByName(ctx, s.db, nodeID, name)
		if errors.Is(childErr, ErrNotFound) {
			break
		}
		if childErr != nil {
			return 0, childErr
		}
		nodeID = next.ID
	}
	return nodeID, nil //nolint:nilerr // an invalid or missing segment ends the walk at its parent
}

// visiblePhotoAssetTx loads an asset for a route. An asset is hidden when any
// member node is; an asset without members stays visible.
func visiblePhotoAssetTx(ctx context.Context, tx *sql.Tx, assetID string) (PhotoAsset, error) {
	if _, ok := PhotoOwnerFromContext(ctx); ok {
		predicate, args := photoNodeVisibleSQL(ctx, "pvf.node_id")
		var hidden bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_files pvf
			WHERE pvf.asset_id=? AND NOT `+predicate+`)`, append([]any{assetID}, args...)...).Scan(&hidden)
		if err != nil {
			return PhotoAsset{}, fmt.Errorf("checking photo asset visibility: %w", err)
		}
		if hidden {
			return PhotoAsset{}, ErrNotFound // the answer a missing asset gets
		}
	}
	return photoAssetByIDQuery(ctx, tx, assetID)
}
