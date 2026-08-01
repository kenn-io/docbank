package backupapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
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

type retainedRestoreCoordinator struct {
	backup.RestoreTargetCoordinator

	lease backup.RestoreTargetLease
}

func (c *retainedRestoreCoordinator) AcquireRestoreTarget(
	ctx context.Context, root *os.Root,
) (backup.RestoreTargetLease, error) {
	lease, err := c.RestoreTargetCoordinator.AcquireRestoreTarget(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("backupapp: retaining restore target: %w", err)
	}
	c.lease = lease
	return retainedRestoreLease{}, nil
}

func (c *retainedRestoreCoordinator) release() error {
	if c.lease == nil {
		return nil
	}
	err := c.lease.Release()
	c.lease = nil
	if err != nil {
		return fmt.Errorf("backupapp: releasing retained restore target: %w", err)
	}
	return nil
}

type retainedRestoreLease struct{}

func (retainedRestoreLease) Release() error { return nil }

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
	return RestoreWithDriver(ctx, repo, version, store.DefaultSQLiteDriver(), opts)
}

// RestoreWithDriver restores through one SQLite adapter for metadata import,
// Kit proof queries, and packed-catalog publication.
func RestoreWithDriver(
	ctx context.Context,
	repo *backup.Repo,
	version string,
	driver docsqlite.Driver,
	opts backup.RestoreOptions,
) (*backup.RestoreResult, error) {
	return RestoreWithPlacement(
		ctx, repo, version, driver, opts, RestorePlacementOptions{},
	)
}

// RestoreWithPlacement restores through one SQLite adapter and optionally
// reconstructs explicitly mapped source placement while the caller retains
// target coordination.
func RestoreWithPlacement(
	ctx context.Context,
	repo *backup.Repo,
	version string,
	driver docsqlite.Driver,
	opts backup.RestoreOptions,
	placement RestorePlacementOptions,
) (*backup.RestoreResult, error) {
	if opts.PackedContent != nil {
		return nil, errors.New("backupapp: restore options must not supply packed content policy")
	}
	if opts.MetadataRestorer != nil {
		return nil, errors.New("backupapp: restore options must not supply a metadata restorer")
	}
	if opts.SQLiteOpener != nil {
		return nil, errors.New("backupapp: restore options must not supply a SQLite opener")
	}
	if opts.AuxiliaryTarget != nil {
		return nil, errors.New("backupapp: restore options must not supply an auxiliary target")
	}
	if opts.BeforePublication != nil {
		return nil, errors.New("backupapp: restore options must not supply a publication callback")
	}
	if err := docsqlite.Validate(driver); err != nil {
		return nil, fmt.Errorf("backupapp: restore SQLite driver: %w", err)
	}
	var retainedCoordinator *retainedRestoreCoordinator
	if opts.TargetCoordinator != nil {
		retainedCoordinator = &retainedRestoreCoordinator{
			RestoreTargetCoordinator: opts.TargetCoordinator,
		}
		opts.TargetCoordinator = retainedCoordinator
	}
	opts.SQLiteOpener = SQLiteOpener(driver)
	opts.MetadataRestorer = metadataRestorer{driver: driver}
	var sourcePlacement placementManifest
	opts.AuxiliaryTarget = placementRestoreTarget{manifest: &sourcePlacement}
	var app backup.App = New(version)
	var primaryHandoff *blob.PrimaryRestoreHandoff
	if placement.Map == nil {
		opts.PackedContent = newPackedRestoreTarget()
		app = &packedRestoreApp{App: New(version)}
	}
	opts.BeforePublication = func(
		hookCtx context.Context, staged backup.RestorePublicationTarget,
	) error {
		blobsDir := filepath.Join(staged.TargetDir, "blobs")
		if err := recoverInterruptedPrimaryHandoff(
			hookCtx, staged.TargetDir, blobsDir, driver,
		); err != nil {
			return err
		}
		next, err := restoredPrimaryOwnership(hookCtx, staged.DBPath, driver)
		if err != nil {
			return err
		}
		primaryHandoff, err = blob.NewPrimaryRestoreHandoff(blobsDir, next)
		if err != nil {
			return err
		}
		if err := primaryHandoff.Prepare(hookCtx); err != nil {
			return err
		}
		if placement.Map == nil {
			return nil
		}
		return applyRestorePlacement(
			hookCtx, staged.TargetDir, staged.DBPath,
			driver, sourcePlacement, placement,
		)
	}
	result, restoreErr := backup.Restore(ctx, repo, app, opts)
	if restoreErr != nil {
		if primaryHandoff != nil {
			restoreErr = errors.Join(
				restoreErr, primaryHandoff.Rollback(context.WithoutCancel(ctx)),
			)
		}
	} else if primaryHandoff != nil {
		if err := primaryHandoff.Commit(context.WithoutCancel(ctx)); err != nil {
			restoreErr = fmt.Errorf("backupapp: completing primary restore handoff: %w", err)
		}
	}
	if retainedCoordinator != nil {
		restoreErr = errors.Join(restoreErr, retainedCoordinator.release())
	}
	if restoreErr != nil {
		if result == nil {
			return nil, fmt.Errorf("backupapp: restoring snapshot: %w", restoreErr)
		}
		return result, restoreErr
	}
	return result, nil
}

func recoverInterruptedPrimaryHandoff(
	ctx context.Context, target, blobsDir string, driver docsqlite.Driver,
) error {
	pending, err := blob.PrimaryRestoreHandoffPending(blobsDir)
	if err != nil || !pending {
		return err
	}
	var published *packstore.Ownership
	databasePath := filepath.Join(target, "docbank.db")
	if _, err := os.Lstat(databasePath); err == nil {
		ownership, ownershipErr := restoredPrimaryOwnership(ctx, databasePath, driver)
		if ownershipErr != nil {
			return fmt.Errorf("backupapp: reading published primary ownership: %w", ownershipErr)
		}
		published = &ownership
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backupapp: checking published restore database: %w", err)
	}
	if err := blob.RecoverPrimaryRestoreHandoff(ctx, blobsDir, published); err != nil {
		return fmt.Errorf("backupapp: recovering interrupted primary handoff: %w", err)
	}
	return nil
}

func restoredPrimaryOwnership(
	ctx context.Context, databasePath string, driver docsqlite.Driver,
) (ownership packstore.Ownership, resultErr error) {
	metadata, err := store.Open(databasePath, driver)
	if err != nil {
		return ownership, fmt.Errorf("backupapp: opening restored primary catalog: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, metadata.Close())
	}()
	ownership = store.NewPackCatalog(metadata).PrimaryOwnership()
	if err := ownership.Validate(); err != nil {
		return packstore.Ownership{}, fmt.Errorf(
			"backupapp: validating restored primary ownership: %w", err,
		)
	}
	if err := ctx.Err(); err != nil {
		return packstore.Ownership{}, err
	}
	return ownership, nil
}

// InspectRestoredStorage reads the physical inventory of a proved restore
// while its caller still owns target-tree coordination. It does not run
// maintenance or change blob authority.
func InspectRestoredStorage(
	ctx context.Context,
	target, databasePath string,
	driver docsqlite.Driver,
) (stats blob.StorageStats, retErr error) {
	info, err := os.Lstat(databasePath)
	if err != nil {
		return stats, fmt.Errorf("checking restored database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return stats, fmt.Errorf("restored database is not a regular file: %s", databasePath)
	}
	metadata, err := store.Open(databasePath, driver)
	if err != nil {
		return stats, fmt.Errorf("opening restored metadata: %w", err)
	}
	defer func() {
		if err := metadata.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing restored metadata: %w", err))
		}
	}()
	physical, err := blob.New(store.NewPackCatalog(metadata), filepath.Join(target, "blobs"))
	if err != nil {
		return stats, fmt.Errorf("opening restored blob storage: %w", err)
	}
	defer func() {
		if err := physical.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing restored blob storage: %w", err))
		}
	}()
	stats, err = physical.Stats(ctx)
	if err != nil {
		return stats, fmt.Errorf("reading restored physical storage: %w", err)
	}
	return stats, nil
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
