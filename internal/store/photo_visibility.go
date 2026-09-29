package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type photoVisibilityContextKey struct{}

// PhotoVisibility carries the authenticated photo identity through store
// calls. The owner ID is request authority, while Trusted is reserved for
// maintenance and metadata validation paths.
type PhotoVisibility struct {
	OwnerID        string
	Enforce        bool
	NoPhotoOwner   bool
	HiddenUnlocked bool
	Trusted        bool
}

// WithPhotoOwner binds a request to one durable owner UUID.
func WithPhotoOwner(ctx context.Context, ownerID string) context.Context {
	return context.WithValue(ctx, photoVisibilityContextKey{}, PhotoVisibility{
		OwnerID: ownerID, Enforce: true,
	})
}

// WithPhotoOwnerPrincipal restores a durable owner carried by a detached
// processing job. Other principals retain their existing trusted context.
func WithPhotoOwnerPrincipal(ctx context.Context, principal string) context.Context {
	ownerID, ok := strings.CutPrefix(principal, "owner:")
	if !ok || ownerID == "" {
		return ctx
	}
	return WithPhotoOwner(ctx, ownerID)
}

// WithNoPhotoOwner allows ordinary document access while refusing all owned
// photo assets. It is used for browser sessions issued before first photo use.
func WithNoPhotoOwner(ctx context.Context) context.Context {
	return context.WithValue(ctx, photoVisibilityContextKey{}, PhotoVisibility{
		Enforce: true, NoPhotoOwner: true,
	})
}

// WithPhotoOwnerBinding restores a persisted binding for detached work.
func WithPhotoOwnerBinding(ctx context.Context, ownerID string, bound, noOwner bool) context.Context {
	if !bound {
		return ctx
	}
	if noOwner {
		return WithNoPhotoOwner(ctx)
	}
	return WithPhotoOwner(ctx, ownerID)
}

func photoOwnerBinding(ctx context.Context) (string, bool, bool) {
	authority, ok := photoVisibilityFromContext(ctx)
	if !ok || authority.Trusted || !authority.Enforce {
		return "", false, false
	}
	if authority.NoPhotoOwner {
		return "", true, true
	}
	return authority.OwnerID, true, false
}

func photoOwnerBindingTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (string, bool, bool, error) {
	ownerID, bound, noOwner := photoOwnerBinding(ctx)
	if !bound || noOwner || ownerID != "" {
		return ownerID, bound, noOwner, nil
	}
	authority, _ := photoVisibilityFromContext(ctx)
	ownerID, err := photoVisibilityOwnerTx(ctx, q, authority)
	if err != nil {
		return "", true, false, err
	}
	if ownerID == "" {
		return "", false, false, nil
	}
	return ownerID, true, false, nil
}

// WithTrustedPhotoVisibility marks an internal maintenance read as trusted.
func WithTrustedPhotoVisibility(ctx context.Context) context.Context {
	return context.WithValue(ctx, photoVisibilityContextKey{}, PhotoVisibility{Trusted: true})
}

func photoVisibilityFromContext(ctx context.Context) (PhotoVisibility, bool) {
	authority, ok := ctx.Value(photoVisibilityContextKey{}).(PhotoVisibility)
	return authority, ok
}

// PhotoOwnerFromContext returns a request's explicitly selected owner.
func PhotoOwnerFromContext(ctx context.Context) (string, bool) {
	authority, ok := photoVisibilityFromContext(ctx)
	return authority.OwnerID, ok && authority.Enforce && authority.OwnerID != ""
}

// PhotoOwnerForRequest resolves the durable owner bound to an authenticated
// request. The bool reports an owner-aware request; the second bool reports a
// session deliberately issued without a photo owner.
func (s *Store) PhotoOwnerForRequest(ctx context.Context) (string, bool, bool, error) {
	authority, ok := photoVisibilityFromContext(ctx)
	if !ok || authority.Trusted || !authority.Enforce {
		return "", false, false, nil
	}
	if authority.NoPhotoOwner {
		return "", true, true, nil
	}
	if authority.OwnerID != "" {
		if validateUUIDv4(authority.OwnerID) != nil {
			return "", true, false, ErrNotFound
		}
		if _, err := s.PhotoOwner(ctx, authority.OwnerID); err != nil {
			return "", true, false, err
		}
		return authority.OwnerID, true, false, nil
	}
	owner, err := defaultPhotoOwnerTx(ctx, s.db)
	if errors.Is(err, ErrNotFound) {
		return "", true, false, nil
	}
	if err != nil {
		return "", true, false, err
	}
	return owner.ID, true, false, nil
}

// CheckPhotoVisibilityForNode applies the caller's owner rule to a node that
// a delayed or derived route is about to serve.
func (s *Store) CheckPhotoVisibilityForNode(ctx context.Context, nodeID int64) error {
	return photoNodeVisibilityCheckTx(ctx, s.db, nodeID)
}

// CheckPhotoVisibilityForVersion resolves a retained version's owning node
// before a delayed route serves bytes or derived metadata.
func (s *Store) CheckPhotoVisibilityForVersion(ctx context.Context, versionID string) error {
	return photoVersionVisibilityCheckTx(ctx, s.db, versionID)
}

// checkPhotoVisibilityForSnapshotMember revalidates the exact live member a
// cached query captured before it serves the frozen row again.
func (s *Store) checkPhotoVisibilityForSnapshotMember(ctx context.Context, nodeID int64, versionID string) error {
	var versionNodeID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT node_id FROM content_versions WHERE version_id=?`, versionID).
		Scan(&versionNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if versionNodeID != nodeID {
		return ErrNotFound
	}
	var trashedAt sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT trashed_at FROM nodes WHERE id=?`, nodeID).Scan(&trashedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if trashedAt.Valid {
		return ErrNotFound
	}
	return photoVersionVisibilityCheckTx(ctx, s.db, versionID)
}

func photoVersionVisibilityCheckTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, versionID string) error {
	var nodeID int64
	if err := q.QueryRowContext(ctx, `SELECT node_id FROM content_versions WHERE version_id=?`, versionID).Scan(&nodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return photoNodeVisibilityCheckTx(ctx, q, nodeID)
}

// PhotoVisibilityDecision is the pure owner and hidden-state rule. Hidden
// unlocking never overrides an owner mismatch.
func PhotoVisibilityDecision(assetOwnerID, requestOwnerID string, hiddenAt *string, hiddenUnlocked bool) bool {
	if assetOwnerID == "" || assetOwnerID != requestOwnerID {
		return false
	}
	return hiddenAt == nil || hiddenUnlocked
}

func photoVisibilityAllows(authority PhotoVisibility, effectiveOwner string, assetOwnerID string, hiddenAt *string) bool {
	if authority.Trusted {
		return true
	}
	if assetOwnerID == "" {
		return false
	}
	if authority.NoPhotoOwner {
		return false
	}
	return PhotoVisibilityDecision(assetOwnerID, effectiveOwner, hiddenAt, authority.HiddenUnlocked)
}

func photoVisibilityOwnerTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, authority PhotoVisibility) (string, error) {
	if authority.OwnerID != "" {
		if validateUUIDv4(authority.OwnerID) != nil {
			return "", ErrNotFound
		}
		if _, err := photoOwnerByIDTx(ctx, q, authority.OwnerID); err != nil {
			return "", err
		}
		return authority.OwnerID, nil
	}
	if authority.NoPhotoOwner || !authority.Enforce {
		return "", nil
	}
	owner, err := defaultPhotoOwnerTx(ctx, q)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return owner.ID, nil
}

func photoVisibilityCheckTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, ownerID string, hiddenAt *string) error {
	authority, ok := photoVisibilityFromContext(ctx)
	if !ok || authority.Trusted {
		return nil
	}
	effectiveOwner, err := photoVisibilityOwnerTx(ctx, q, authority)
	if err != nil {
		return err
	}
	if !photoVisibilityAllows(authority, effectiveOwner, ownerID, hiddenAt) {
		return ErrNotFound
	}
	return nil
}

func photoNodeVisibilityCheckTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, nodeID int64) error {
	var ownerID, hiddenAt sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT a.owner_id, a.hidden_at
		FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id
		WHERE f.node_id=?`, nodeID).Scan(&ownerID, &hiddenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking photo visibility for node %d: %w", nodeID, err)
	}
	if !ownerID.Valid || ownerID.String == "" {
		return fmt.Errorf("node %d: %w", nodeID, ErrInvalidPhotoOwner)
	}
	if _, ownerErr := photoOwnerByIDTx(ctx, q, ownerID.String); ownerErr != nil {
		if errors.Is(ownerErr, ErrNotFound) {
			return fmt.Errorf("node %d: %w", nodeID, ErrInvalidPhotoOwner)
		}
		return ownerErr
	}
	var hidden *string
	if hiddenAt.Valid {
		hidden = &hiddenAt.String
	}
	return photoVisibilityCheckTx(ctx, q, ownerID.String, hidden)
}

func photoAssetVisibilitySQL(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, alias string) (string, []any, error) {
	authority, ok := photoVisibilityFromContext(ctx)
	if !ok || authority.Trusted {
		return "1=1", nil, nil
	}
	ownerID, err := photoVisibilityOwnerTx(ctx, q, authority)
	if err != nil {
		return "", nil, err
	}
	visible := "0=1"
	if ownerID != "" && !authority.NoPhotoOwner {
		visible = "(EXISTS (SELECT 1 FROM photo_owners po WHERE po.owner_id=" + alias + ".owner_id) AND " + alias + ".owner_id=?"
		args := []any{ownerID}
		if !authority.HiddenUnlocked {
			visible += " AND " + alias + ".hidden_at IS NULL"
		}
		visible += ")"
		return visible, args, nil
	}
	return visible, nil, nil
}

func photoNodeVisibilitySQLForAlias(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, nodeAlias, assetAlias string) (string, []any, error) {
	visible, args, err := photoAssetVisibilitySQL(ctx, q, assetAlias)
	if err != nil {
		return "", nil, err
	}
	return "NOT EXISTS (SELECT 1 FROM photo_files pvf JOIN photo_assets " + assetAlias +
		" ON " + assetAlias + ".asset_id=pvf.asset_id WHERE pvf.node_id=" + nodeAlias +
		".id AND NOT (" + visible + "))", args, nil
}

func photoNodeVisibilitySQL(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (string, []any, error) {
	return photoNodeVisibilitySQLForAlias(ctx, q, "n", "pva")
}

func photoVersionVisibilitySQL(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, versionAlias string) (string, []any, error) {
	visible, args, err := photoAssetVisibilitySQL(ctx, q, "pva")
	if err != nil {
		return "", nil, err
	}
	return `NOT EXISTS (SELECT 1 FROM content_versions pvv
		JOIN photo_files pvf ON pvf.node_id=pvv.node_id
		JOIN photo_assets pva ON pva.asset_id=pvf.asset_id
		WHERE pvv.version_id=` + versionAlias + ` AND NOT (` + visible + `))`, args, nil
}

// photoSubtreeVisibilityCheckTx rejects a directory operation that would
// expose or mutate a descendant owned by another photo owner.
func photoSubtreeVisibilityCheckTx(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, rootID int64) error {
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE subtree(id) AS (
		SELECT id FROM nodes WHERE id=?
		UNION ALL
		SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id=s.id
	)
	SELECT f.node_id, a.owner_id, a.hidden_at
	FROM subtree s JOIN photo_files f ON f.node_id=s.id
	JOIN photo_assets a ON a.asset_id=f.asset_id`, rootID)
	if err != nil {
		return fmt.Errorf("checking photo visibility for subtree %d: %w", rootID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var nodeID int64
		var ownerID, hiddenAt sql.NullString
		if err := rows.Scan(&nodeID, &ownerID, &hiddenAt); err != nil {
			return fmt.Errorf("checking photo visibility for subtree %d: %w", rootID, err)
		}
		if !ownerID.Valid || ownerID.String == "" {
			return fmt.Errorf("node %d: %w", nodeID, ErrInvalidPhotoOwner)
		}
		if _, ownerErr := photoOwnerByIDTx(ctx, q, ownerID.String); ownerErr != nil {
			if errors.Is(ownerErr, ErrNotFound) {
				return fmt.Errorf("node %d: %w", nodeID, ErrInvalidPhotoOwner)
			}
			return ownerErr
		}
		var hidden *string
		if hiddenAt.Valid {
			hidden = &hiddenAt.String
		}
		if err := photoVisibilityCheckTx(ctx, q, ownerID.String, hidden); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checking photo visibility for subtree %d: %w", rootID, err)
	}
	return nil
}

func scopedCurrentContentMembershipCTE(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (string, []any, error) {
	predicate, args, err := photoNodeVisibilitySQL(ctx, q)
	if err != nil {
		return "", nil, err
	}
	return `current_content_members AS (
    SELECT n.id AS node_id, v.version_id, v.blob_hash, b.size, n.modified_at
    FROM nodes n
    JOIN content_versions v ON v.node_id = n.id AND v.version_id = n.current_version_id
    JOIN blobs b ON b.hash = v.blob_hash AND b.size = v.size
    WHERE n.kind = 'file' AND n.trashed_at IS NULL AND ` + predicate + `
)`, args, nil
}

func scopedTrashRootsCTE(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (string, []any, error) {
	visible, args, err := photoAssetVisibilitySQL(ctx, q, "pva")
	if err != nil {
		return "", nil, err
	}
	return `WITH RECURSIVE trash_roots(root_id) AS (
    SELECT id FROM nodes WHERE trash_name IS NOT NULL
), trash_subtree(root_id,node_id) AS (
    SELECT root_id,root_id FROM trash_roots
    UNION ALL
    SELECT t.root_id,n.id FROM trash_subtree t JOIN nodes n ON n.parent_id=t.node_id
), visible_trash_roots(root_id) AS (
    SELECT r.root_id FROM trash_roots r
    WHERE NOT EXISTS (
        SELECT 1 FROM trash_subtree t
        JOIN photo_files pvf ON pvf.node_id=t.node_id
        JOIN photo_assets pva ON pva.asset_id=pvf.asset_id
        WHERE t.root_id=r.root_id AND NOT (` + visible + `)
    )
)`, args, nil
}

func photoOwnerForMutationTx(ctx context.Context, tx *sql.Tx) (string, error) {
	authority, ok := photoVisibilityFromContext(ctx)
	if ok && authority.Trusted {
		return "", ErrInvalidPhotoOwner
	}
	if ok && authority.NoPhotoOwner {
		return "", ErrNotFound
	}
	if ok && authority.OwnerID != "" {
		if validateUUIDv4(authority.OwnerID) != nil {
			return "", ErrNotFound
		}
		if _, err := photoOwnerByIDTx(ctx, tx, authority.OwnerID); err != nil {
			return "", err
		}
		return authority.OwnerID, nil
	}
	owner, err := ensureDefaultPhotoOwnerTx(ctx, tx)
	if err != nil {
		return "", err
	}
	return owner.ID, nil
}

func nullablePhotoStringFromPtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
