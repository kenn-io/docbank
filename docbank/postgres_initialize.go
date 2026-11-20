package docbank

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"uuid"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

// InitializePostgres installs an empty vault and its private filesystem root.
// The caller owns the schema-owner pool. The root must be absent under an
// existing parent, or already own the catalog. A repeated call for the same
// bound root returns its current vault ID. Existing catalogs must already be
// at the current schema version. Use SetupPostgres explicitly to upgrade.
func InitializePostgres(
	ctx context.Context, owner *sql.DB, catalog PostgresCatalog, root string,
) (_ uuid.UUID, retErr error) {
	if owner == nil || root == "" {
		return uuid.Nil(), errors.New("docbank PostgreSQL initialization requires a schema-owner connection and vault root")
	}
	if err := catalog.schemaConfig().Validate(); err != nil {
		return uuid.Nil(), err
	}
	target, err := home.CanonicalRoot(root)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("resolving PostgreSQL vault root: %w", err)
	}
	layout := home.Layout{Root: target}
	if _, err := os.Lstat(target); err == nil {
		dir, lock, err := layout.OpenExistingAndLockExclusiveNoCreate()
		if err != nil {
			return uuid.Nil(), err
		}
		defer func() { retErr = errors.Join(retErr, lock.Release(), dir.Close()) }()
		return store.InitializePostgresPrimary(ctx, owner, catalog.schemaConfig(), true,
			func(ctx context.Context, expected packstore.Ownership) error {
				return checkInitializedPostgresRoot(ctx, layout, expected)
			})
	} else if !errors.Is(err, os.ErrNotExist) {
		return uuid.Nil(), fmt.Errorf("checking PostgreSQL vault root: %w", err)
	}

	ancestors, err := layout.TryLockExistingAncestors()
	if err != nil {
		return uuid.Nil(), fmt.Errorf("locking PostgreSQL vault ancestors: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, ancestors.Release()) }()
	parent := filepath.Dir(target)
	staged, err := os.MkdirTemp(parent, ".docbank-pg-init-")
	if err != nil {
		return uuid.Nil(), fmt.Errorf("creating staging directory for PostgreSQL vault: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(staged)) }()
	stage := home.Layout{Root: staged}
	if err := stage.EnsureDirectories(); err != nil {
		return uuid.Nil(), err
	}
	lock, err := stage.TryLockExclusive()
	if err != nil {
		return uuid.Nil(), fmt.Errorf("locking staged PostgreSQL vault: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, lock.Release()) }()

	installed := false
	id, err := store.InitializePostgresPrimary(ctx, owner, catalog.schemaConfig(), false,
		func(ctx context.Context, ownership packstore.Ownership) error {
			if err := writeInitializedPostgresRoot(ctx, stage, ownership); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := atomicfile.RenameNoReplace(staged, target); err != nil {
				return fmt.Errorf("installing PostgreSQL vault root: %w", err)
			}
			installed = true
			return atomicfile.SyncDir(parent)
		})
	if err != nil && installed {
		return uuid.Nil(), fmt.Errorf("PostgreSQL vault directory was installed at %q; retry initialization there to verify ownership: %w", target, err)
	}
	return id, err
}

func postgresRootBackend(layout home.Layout) (*packstore.FilesystemBackend, error) {
	physical, err := packstore.NewLayout(layout.BlobsDir(), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	if err != nil {
		return nil, fmt.Errorf("resolving PostgreSQL vault blob layout: %w", err)
	}
	backend, err := packstore.NewFilesystemBackend(physical, packstore.FilesystemBackendOptions{Limits: blob.StorageLimits()})
	if err != nil {
		return nil, fmt.Errorf("opening PostgreSQL primary storage: %w", err)
	}
	return backend, nil
}

func checkInitializedPostgresRoot(ctx context.Context, layout home.Layout, expected packstore.Ownership) (retErr error) {
	backend, err := postgresRootBackend(layout)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, backend.Close()) }()
	observed, err := backend.Ownership(ctx)
	if err != nil {
		return fmt.Errorf("reading PostgreSQL vault ownership: %w", err)
	}
	if observed != expected {
		return fmt.Errorf("primary filesystem marker does not match PostgreSQL catalog: %w", packstore.ErrStoreFenced)
	}
	return nil
}

func writeInitializedPostgresRoot(ctx context.Context, layout home.Layout, ownership packstore.Ownership) (retErr error) {
	backend, err := postgresRootBackend(layout)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, backend.Close()) }()
	if err := backend.ReplaceOwnership(ctx, ownership, nil); err != nil {
		return fmt.Errorf("writing PostgreSQL vault ownership: %w", err)
	}
	return atomicfile.SyncDir(layout.Root)
}
