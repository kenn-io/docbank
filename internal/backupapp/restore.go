package backupapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// packedRestoreTarget supplies docbank's storage policy and catalog adapter to
// Kit while it restores into an unpublished target database.
type packedRestoreTarget struct {
	coordinator *packstore.Coordinator
	limits      packstore.Limits
}

var _ backup.PackedContentTarget = (*packedRestoreTarget)(nil)

type packedRestoreApp struct{ *App }

var _ backup.App = (*packedRestoreApp)(nil)

func (a *packedRestoreApp) RestoredContentPaths(
	ctx context.Context, db *sql.DB,
) (map[string][]string, error) {
	return restoredContentPaths(ctx, db, true)
}

func newPackedRestoreTarget() *packedRestoreTarget {
	return &packedRestoreTarget{
		coordinator: packstore.NewCoordinator(),
		limits:      blob.StorageLimits(),
	}
}

// Restore restores a snapshot with docbank's packed-storage policy. It owns the
// application adapter and packed target as one operation so callers cannot
// accidentally authorize captured pack metadata while omitting catalog
// replacement from Kit's restore options.
func Restore(
	ctx context.Context, repo *backup.Repo, version string, opts backup.RestoreOptions,
) (*backup.RestoreResult, error) {
	if opts.PackedContent != nil {
		return nil, errors.New("backupapp: restore options must not supply packed content policy")
	}
	if opts.MetadataRestorer != nil {
		return nil, errors.New("backupapp: restore options must not supply a metadata restorer")
	}
	opts.PackedContent = newPackedRestoreTarget()
	opts.MetadataRestorer = metadataRestorer{}
	app := &packedRestoreApp{App: New(version)}
	result, err := backup.Restore(ctx, repo, app, opts)
	if err != nil {
		return nil, fmt.Errorf("backupapp: restoring snapshot: %w", err)
	}
	return result, nil
}

func (t *packedRestoreTarget) Limits() packstore.Limits { return t.limits }

func (t *packedRestoreTarget) AcquireRestoreLease(ctx context.Context) (*packstore.Lease, error) {
	lease, err := t.coordinator.AcquireMutation(ctx)
	if err != nil {
		return nil, fmt.Errorf("backupapp: acquiring packed restore lease: %w", err)
	}
	return lease, nil
}

func (t *packedRestoreTarget) OpenRestoreCatalog(
	_ context.Context, db *sql.DB,
) (packstore.RestoreCatalog, error) {
	return store.NewPackRestoreCatalog(db), nil
}
