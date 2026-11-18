package backupapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/store/pgschema"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/safefileio"
)

// PostgresRestoreResult identifies the snapshot and the new local vault root.
type PostgresRestoreResult struct {
	SnapshotID string
	VaultID    string
	TargetDir  string
}

// PostgresRestoreTarget separates the importer from the runtime role that
// checks the restored storage and rebuilds derived indexes.
type PostgresRestoreTarget struct {
	Owner   *sql.DB
	Runtime *pgx.ConnConfig
	Schema  pgschema.Config
}

// RestorePostgres restores a portable backup into a new local directory and a
// pristine PostgreSQL schema. The connection must authenticate as the schema
// owner. The caller supplies a locally controlled target parent directory.
func RestorePostgres(
	ctx context.Context, repo *backup.Repo, version string, driver docsqlite.Driver,
	destination PostgresRestoreTarget, opts backup.RestoreOptions,
) (_ *PostgresRestoreResult, retErr error) {
	if destination.Owner == nil || destination.Runtime == nil || opts.TargetDir == "" || opts.Overwrite || opts.TargetCoordinator != nil {
		return nil, errors.New("PostgreSQL restore requires owner and runtime credentials and a new local vault directory")
	}
	if err := destination.Schema.Validate(); err != nil {
		return nil, err
	}
	target, err := home.CanonicalRoot(opts.TargetDir)
	if err != nil {
		return nil, err
	}
	if err := ValidateDisjointRoots(target, repo.Root()); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, err
		}
		return nil, ErrRestoreTargetNotEmpty
	}
	parent := filepath.Dir(target)
	scratch, err := os.MkdirTemp(parent, ".docbank-pg-restore-")
	if err != nil {
		return nil, fmt.Errorf("creating private restore directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()
	if err := safefileio.EnsurePrivateDir(scratch); err != nil {
		return nil, fmt.Errorf("securing private restore directory: %w", err)
	}
	logical, err := os.CreateTemp(scratch, "metadata-")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, logical.Close()) }()
	opts.TargetDir = filepath.Join(scratch, "vault")
	verified, err := restoreWithPlacement(ctx, repo, version, driver, opts, RestorePlacementOptions{}, logical)
	if err != nil {
		return nil, err
	}
	size, err := logical.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, errors.New("PostgreSQL restore requires a portable metadata backup")
	}
	if _, err := logical.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// Only files belong in the PostgreSQL vault directory. Move the verified
	// scratch catalog out before opening it for logical and physical readback.
	catalogPath := filepath.Join(scratch, "catalog.db")
	if err := atomicfile.RenameNoReplace(verified.DBPath, catalogPath); err != nil {
		return nil, fmt.Errorf("separating the verified catalog from its files: %w", err)
	}
	source, err := store.OpenExisting(ctx, catalogPath, driver, scratch)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	blobsDir := filepath.Join(opts.TargetDir, "blobs")
	layout, err := packstore.NewLayout(blobsDir, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	if err != nil {
		return nil, fmt.Errorf("resolving restored blob layout: %w", err)
	}
	physical, err := packstore.NewFilesystemBackend(layout, packstore.FilesystemBackendOptions{Limits: blob.StorageLimits()})
	if err != nil {
		return nil, fmt.Errorf("opening restored blob storage: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, physical.Close()) }()
	prior := store.NewPackCatalog(source).PrimaryOwnership()
	result := &PostgresRestoreResult{SnapshotID: verified.SnapshotID, VaultID: source.VaultID(), TargetDir: target}
	installed := false
	err = store.RestorePostgresMetadata(ctx, destination.Owner, destination.Schema, logical, source, func(ctx context.Context, next packstore.Ownership) error {
		if err := physical.ReplaceOwnership(ctx, next, &prior); err != nil {
			return fmt.Errorf("binding restored files to the PostgreSQL catalog: %w", err)
		}
		if err := atomicfile.SyncDir(opts.TargetDir); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := atomicfile.RenameNoReplace(opts.TargetDir, target); err != nil {
			return fmt.Errorf("installing restored vault directory: %w", err)
		}
		installed = true
		return errors.Join(atomicfile.SyncDir(parent), atomicfile.SyncDir(scratch))
	})
	if err != nil {
		if installed {
			return result, fmt.Errorf("restore directory was installed at %q; compare its primary ownership marker with the committed schema and retry in a new directory only if the schema is still pristine: %w", target, err)
		}
		return nil, fmt.Errorf("restoring PostgreSQL catalog: %w", err)
	}
	if err := RebuildPostgresRestoreProjections(ctx, destination.Runtime, destination.Schema, target); err != nil {
		return result, fmt.Errorf("PostgreSQL restore committed; rebuilding projections: %w", err)
	}
	return result, nil
}

// RebuildPostgresRestoreProjections checks the committed restore's ownership
// and rebuilds its derived indexes. The caller exclusively owns the vault
// root while running this after restore or retrying an interrupted rebuild.
func RebuildPostgresRestoreProjections(
	ctx context.Context, runtime *pgx.ConnConfig, schema pgschema.Config, target string,
) (retErr error) {
	metadata, err := store.OpenPostgres(ctx, runtime, schema, target)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, metadata.Close()) }()
	physical, err := blob.NewExistingReader(store.NewPackCatalog(metadata), filepath.Join(target, "blobs"), nil)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, physical.Close()) }()
	if err := metadata.VerifyRestoredRenditionBlobAuthority(ctx); err != nil {
		return err
	}
	return rebuildRestoredProjections(ctx, metadata, physical)
}
