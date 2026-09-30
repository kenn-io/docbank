package store

import (
	"context"
	"database/sql"
	"fmt"
)

type photoOwnerContextKey struct{}

// WithPhotoOwner selects the photo owner a request sees. An empty id selects
// the default owner. A context without a selection sees every photo.
func WithPhotoOwner(ctx context.Context, ownerID string) context.Context {
	return context.WithValue(ctx, photoOwnerContextKey{}, ownerID)
}

// PhotoOwnerFromContext returns the selected owner id, empty for the default
// owner, and whether the context selects an owner at all.
func PhotoOwnerFromContext(ctx context.Context) (string, bool) {
	ownerID, ok := ctx.Value(photoOwnerContextKey{}).(string)
	return ownerID, ok
}

// photoNodeVisibleSQL is the one visibility rule: a node is visible unless a
// photo asset owned by someone else, or hidden, contains it. The node
// expression's own arguments must precede the returned ones.
func photoNodeVisibleSQL(ctx context.Context, nodeExpr string) (string, []any) {
	ownerID, ok := PhotoOwnerFromContext(ctx)
	if !ok {
		return "1=1", nil
	}
	owner, args := "?", []any{ownerID}
	if ownerID == "" {
		owner, args = defaultPhotoOwnerSQL, nil
	}
	return `NOT EXISTS (SELECT 1 FROM photo_files pvf JOIN photo_assets pva ON pva.asset_id=pvf.asset_id
		WHERE pvf.node_id=` + nodeExpr + ` AND (pva.owner_id IS NOT ` + owner + ` OR pva.hidden_at IS NOT NULL))`, args
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

// CheckPhotoNodeVisible applies the request's owner rule to one node that a
// route addresses outside the store's node loaders.
func (s *Store) CheckPhotoNodeVisible(ctx context.Context, nodeID int64) error {
	if err := checkPhotoNodeVisibleTx(ctx, s.db, nodeID); err != nil {
		return fmt.Errorf("node %d: %w", nodeID, err)
	}
	return nil
}

// photoOwnerForCreateTx picks the owner of a new photo asset: the request's
// explicit owner, or the default owner, created on first use.
func photoOwnerForCreateTx(ctx context.Context, tx *sql.Tx) (string, error) {
	if ownerID, _ := PhotoOwnerFromContext(ctx); ownerID != "" {
		if _, err := photoOwnerByIDTx(ctx, tx, ownerID); err != nil {
			return "", err
		}
		return ownerID, nil
	}
	owner, err := ensureDefaultPhotoOwnerTx(ctx, tx)
	return owner.ID, err
}

// visiblePhotoAssetTx loads an asset for a route, applying the owner rule to
// the asset row itself so a memberless asset stays with its owner.
func visiblePhotoAssetTx(ctx context.Context, tx *sql.Tx, assetID string) (PhotoAsset, error) {
	if ownerID, ok := PhotoOwnerFromContext(ctx); ok && validateUUIDv4(assetID) == nil {
		owner, args := "?", []any{assetID, ownerID}
		if ownerID == "" {
			owner, args = defaultPhotoOwnerSQL, []any{assetID}
		}
		var visible bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_assets
			WHERE asset_id=? AND owner_id IS `+owner+` AND hidden_at IS NULL)`, args...).Scan(&visible)
		if err != nil {
			return PhotoAsset{}, fmt.Errorf("checking photo asset visibility: %w", err)
		}
		if !visible {
			return PhotoAsset{}, fmt.Errorf("photo asset %q: %w", assetID, ErrNotFound)
		}
	}
	return photoAssetByIDQuery(ctx, tx, assetID)
}
