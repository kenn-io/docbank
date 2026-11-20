package docbank

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/docbank/internal/version"
)

// PostgresCatalog identifies one dedicated PostgreSQL vault schema and its
// owner. Callers provision separate owner and runtime roles.
type PostgresCatalog struct {
	Schema      string
	SchemaOwner string
}

var (
	// ErrPostgresPrimaryBound means the catalog already owns another root.
	ErrPostgresPrimaryBound = store.ErrPostgresPrimaryBound
	// ErrPostgresPrimaryUnbound means the catalog has no committed storage root.
	ErrPostgresPrimaryUnbound = store.ErrPostgresPrimaryUnbound
)

func (c PostgresCatalog) schemaConfig() pgschema.Config {
	return pgschema.Config{Schema: c.Schema, SchemaOwner: c.SchemaOwner}
}

// SetupPostgres installs an empty vault catalog or upgrades a supported catalog.
// The caller owns the pool, which must authenticate as catalog.SchemaOwner.
// Close the vault before upgrading and apply the current runtime grants before
// reopening it.
func SetupPostgres(ctx context.Context, owner *sql.DB, catalog PostgresCatalog) (uuid.UUID, error) {
	if owner == nil {
		return uuid.Nil(), errors.New("docbank PostgreSQL setup requires a schema-owner connection")
	}
	return pgschema.Setup(ctx, owner, catalog.schemaConfig())
}

// OpenPostgres exclusively opens an initialized or restored root with the runtime role.
// The root must hold the matching primary ownership marker. Close releases the
// pool and root lock. Run one owning process per catalog.
func OpenPostgres(ctx context.Context, config Config, catalog PostgresCatalog, runtime *pgx.ConnConfig) (*Vault, error) {
	if config.SQLite != nil {
		return nil, errors.New("docbank PostgreSQL opening does not take a SQLite driver")
	}
	config, blobOptions, err := normalizeVaultConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	layout := home.Layout{Root: config.Root}
	return openVaultWithCatalog(ctx, config, blobOptions, layout, layout.OpenExistingAndLockExclusiveNoCreate,
		func(ctx context.Context) (*store.Store, error) {
			metadata, err := store.OpenPostgres(ctx, runtime, catalog.schemaConfig(), layout.Root)
			if err != nil {
				return nil, err
			}
			if !metadata.PostgresPrimaryBound() {
				return nil, errors.Join(ErrPostgresPrimaryUnbound, metadata.Close())
			}
			return metadata, nil
		},
	)
}

// PostgresRestoreReport identifies the installed root. CatalogCommitted is
// true once PostgreSQL confirms the import, even if rebuilding projections
// fails. A report with a target and no confirmed commit requires ownership
// reconciliation before retrying.
type PostgresRestoreReport struct {
	SnapshotID       string
	VaultID          string
	Target           string
	CatalogCommitted bool
}

// RestorePostgres restores a portable backup into a pristine, installed
// catalog and a new root. The owner imports metadata and the separate runtime
// role validates the destination and rebuilds derived projections. When the
// catalog has committed, RebuildPostgresRestore can retry a failed rebuild.
func (r *BackupRepository) RestorePostgres(
	ctx context.Context, owner *sql.DB, catalog PostgresCatalog, runtime *pgx.ConnConfig, opts BackupRestoreOptions,
) (PostgresRestoreReport, error) {
	if r == nil || r.repo == nil {
		return PostgresRestoreReport{}, errors.New("docbank backup repository is required")
	}
	if owner == nil || runtime == nil || opts.Target == "" || opts.Overwrite {
		return PostgresRestoreReport{}, errors.New("docbank PostgreSQL restore requires owner and runtime connections and a new target root")
	}
	target, err := home.CanonicalRoot(opts.Target)
	if err != nil {
		return PostgresRestoreReport{}, fmt.Errorf("resolving backup restore target: %w", err)
	}
	protected := append([]string{r.repo.Root()}, opts.ProtectedRoots...)
	if err := backupapp.ValidateDisjointRoots(target, protected...); err != nil {
		return PostgresRestoreReport{}, err
	}
	driver := opts.SQLite
	if driver == nil {
		driver = store.DefaultSQLiteDriver()
	}
	result, err := backupapp.RestorePostgres(ctx, r.repo, version.Version, driver,
		backupapp.PostgresRestoreTarget{Owner: owner, Runtime: runtime, Schema: catalog.schemaConfig()},
		backup.RestoreOptions{
			SnapshotID: opts.SnapshotID, TargetDir: target, Jobs: opts.Jobs,
			ForceUnlock: opts.ForceUnlock, Progress: backupProgressCallback(opts.Progress),
		},
	)
	if result == nil {
		return PostgresRestoreReport{}, err
	}
	return PostgresRestoreReport{
		SnapshotID: result.SnapshotID, VaultID: result.VaultID, Target: result.TargetDir,
		CatalogCommitted: result.CatalogCommitted,
	}, err
}

// RebuildPostgresRestore retries derived projection rebuilding on the
// matching restored root. It requires exclusive ownership of that root.
func RebuildPostgresRestore(ctx context.Context, root string, catalog PostgresCatalog, runtime *pgx.ConnConfig) (retErr error) {
	if root == "" {
		return errors.New("docbank PostgreSQL rebuild requires a vault root")
	}
	target, err := home.CanonicalRoot(root)
	if err != nil {
		return err
	}
	layout := home.Layout{Root: target}
	dir, lock, err := layout.OpenExistingAndLockExclusiveNoCreate()
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, lock.Release(), dir.Close()) }()
	return backupapp.RebuildPostgresRestoreProjections(ctx, runtime, catalog.schemaConfig(), target)
}
