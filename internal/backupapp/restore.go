package backupapp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
	docsqlite "go.kenn.io/docbank/sqlite"
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
	var app backup.App = New(version)
	var primaryHandoff *blob.PrimaryRestoreHandoff
	if placement.Map == nil {
		opts.PackedContent = newPackedRestoreTarget()
		app = &packedRestoreApp{App: New(version)}
	} else {
		var err error
		snapshotID := opts.SnapshotID
		if snapshotID == "" {
			latest, latestErr := repo.LatestSnapshot()
			if latestErr != nil {
				return nil, fmt.Errorf(
					"backupapp: selecting latest restore snapshot: %w", latestErr,
				)
			}
			if latest == nil {
				return nil, errors.New("backupapp: repository has no snapshots to restore")
			}
			snapshotID = latest.SnapshotID
		}
		sourcePlacement, opts.SnapshotID, err = loadPlacementManifest(
			ctx, repo, snapshotID, app.PackFileExtension(),
		)
		if err != nil {
			return nil, fmt.Errorf("backupapp: loading restore placement: %w", err)
		}
	}
	opts.AuxiliaryTarget = placementRestoreTarget{}
	opts.BeforePublication = func(
		hookCtx context.Context, staged backup.RestorePublicationTarget,
	) error {
		blobsDir := filepath.Join(staged.TargetDir, "blobs")
		if err := RecoverInterruptedPrimaryHandoff(
			hookCtx, staged.TargetDir, driver,
		); err != nil {
			return err
		}
		next, err := restoredPrimaryOwnership(hookCtx, staged.DBPath, driver)
		if err != nil {
			return err
		}
		priorDatabaseDigest, err := restoreDatabaseDigest(
			filepath.Join(staged.TargetDir, "docbank.db"),
		)
		if err != nil {
			return err
		}
		primaryHandoff, err = blob.NewPrimaryRestoreHandoff(
			blobsDir, next, &priorDatabaseDigest,
		)
		if err != nil {
			return err
		}
		if err := primaryHandoff.Prepare(hookCtx); err != nil {
			return err
		}
		if err := verifyRestoredProcessingAuthority(
			hookCtx, staged.TargetDir, staged.DBPath, driver,
		); err != nil {
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

func verifyRestoredProcessingAuthority(
	ctx context.Context, target, databasePath string, driver docsqlite.Driver,
) (retErr error) {
	metadata, err := store.OpenForRestore(databasePath, driver)
	if err != nil {
		return fmt.Errorf("backupapp: opening staged processing authority: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, metadata.Close()) }()
	if err := metadata.VerifyRestoredRenditionBlobAuthority(ctx); err != nil {
		return err
	}
	physical, err := blob.NewPreparedRestoreReader(
		store.NewPackCatalog(metadata), filepath.Join(target, "blobs"),
	)
	if err != nil {
		return fmt.Errorf("backupapp: opening restored rendition storage: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, physical.Close()) }()
	if err := metadata.VerifyRenditionBlobBytes(ctx, physical); err != nil {
		return fmt.Errorf("backupapp: verifying restored rendition bytes: %w", err)
	}
	if err := metadata.RebuildRenditionLexicalProjection(ctx); err != nil {
		return err
	}
	if _, err := rebuildRestoredVectorIndexes(ctx, metadata); err != nil {
		return err
	}
	return nil
}

func rebuildRestoredVectorIndexes(
	ctx context.Context, metadata *store.Store,
) (vectorworker.IndexRestoreReport, error) {
	worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
		Catalog: metadata, Owner: "restore-vector-index-worker",
		BuildLease: 30 * time.Minute, ReaderLease: 5 * time.Minute,
		IdleDelay: time.Second,
	})
	if err != nil {
		return vectorworker.IndexRestoreReport{}, fmt.Errorf("backupapp: configuring restored vector indexes: %w", err)
	}
	report, err := worker.Restore(ctx)
	if err != nil {
		return report, fmt.Errorf("backupapp: rebuilding restored vector indexes: %w", err)
	}
	return report, nil
}

// RecoverInterruptedPrimaryHandoff resolves a durable restore marker against
// the database publication boundary before any caller opens SQLite mutably.
// The caller must hold the vault's exclusive hierarchy lock.
func RecoverInterruptedPrimaryHandoff(
	ctx context.Context, target string, driver docsqlite.Driver,
) error {
	blobsDir := filepath.Join(target, "blobs")
	pending, err := blob.PrimaryRestoreHandoffPending(blobsDir)
	if err != nil || !pending {
		return err
	}
	var published *packstore.Ownership
	databasePath := filepath.Join(target, "docbank.db")
	databaseDigest, err := restoreDatabaseDigest(databasePath)
	if err != nil {
		return err
	}
	ownership, ownershipErr := restoredPrimaryOwnership(ctx, databasePath, driver)
	if ownershipErr == nil {
		published = &ownership
	}
	if err := blob.RecoverPrimaryRestoreHandoff(
		ctx, blobsDir, published, &databaseDigest,
	); err != nil {
		return errors.Join(
			fmt.Errorf("backupapp: recovering interrupted primary handoff: %w", err),
			ownershipErr,
		)
	}
	return nil
}

func restoredPrimaryOwnership(
	ctx context.Context, databasePath string, driver docsqlite.Driver,
) (ownership packstore.Ownership, resultErr error) {
	db, err := driver.Open(databasePath, docsqlite.OpenOptions{
		Access: docsqlite.ReadOnlyImmutable,
	})
	if err != nil {
		return ownership, fmt.Errorf("backupapp: opening restored primary authority: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, db.Close())
	}()
	ownership.Format = packstore.OwnershipFormatV1
	if err := db.QueryRowContext(ctx, `
		SELECT v.vault_uid, b.store_id, b.ownership_epoch
		FROM vault_metadata AS v
		JOIN blob_stores AS b ON b.role = 'primary'
		WHERE v.singleton = 1`).Scan(
		&ownership.Vault, &ownership.Store, &ownership.Epoch,
	); err != nil {
		return packstore.Ownership{},
			fmt.Errorf("backupapp: reading restored primary authority: %w", err)
	}
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

func restoreDatabaseDigest(databasePath string) (digest string, resultErr error) {
	file, err := os.Open(databasePath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("backupapp: opening restore database discriminator: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("backupapp: inspecting restore database discriminator: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("backupapp: restore database discriminator is not a regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("backupapp: hashing restore database discriminator: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
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
