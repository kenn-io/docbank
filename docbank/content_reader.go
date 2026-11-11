package docbank

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/waitlock"
	docsqlite "go.kenn.io/docbank/sqlite"
)

var (
	// ErrVaultLocked indicates an overlapping vault or restore owns the hierarchy.
	ErrVaultLocked = home.ErrVaultLocked
	// ErrVaultNeedsOwner indicates initialization or recovery by an owner is required.
	ErrVaultNeedsOwner = store.ErrNeedsOwner
)

// ContentReaderConfig selects an existing SQLite vault.
type ContentReaderConfig struct {
	Root   string
	SQLite docsqlite.Driver
}

// ContentReader reads verified originals while holding the vault hierarchy lock.
type ContentReader struct{ vault *Vault }

// OpenContentReader opens a checkpointed existing vault without initializing or recovering it.
func OpenContentReader(ctx context.Context, config ContentReaderConfig) (_ *ContentReader, retErr error) {
	normalized, _, err := normalizeVaultConfig(ctx, Config{Root: config.Root, SQLite: config.SQLite})
	if err != nil {
		return nil, err
	}
	layout := home.Layout{Root: normalized.Root}
	root, lock, err := layout.OpenExistingAndLockExclusiveNoCreate()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("opening existing vault: %w", ErrVaultNeedsOwner)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, lock.Release(), root.Close())
		}
	}()
	pending, err := blob.PrimaryRestoreHandoffPending(layout.BlobsDir())
	if err != nil {
		return nil, err
	}
	if pending {
		return nil, fmt.Errorf("pending restore handoff: %w", ErrVaultNeedsOwner)
	}
	metadata, err := store.OpenExisting(ctx, layout.DBPath(), normalized.SQLite)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, metadata.Close())
		}
	}()
	registry := blob.NewRegistry(ctx, metadata.VaultID(), nil, nil)
	blobs, err := blob.NewExistingReader(store.NewPackCatalog(metadata), layout.BlobsDir(), registry)
	if err != nil {
		err = errors.Join(err, registry.Close())
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("missing primary ownership marker: %w", ErrVaultNeedsOwner)
		}
		return nil, err
	}
	return &ContentReader{vault: &Vault{vaultRoot: normalized.Root, root: root, lock: lock, metadata: metadata, blobs: blobs, lifecycle: waitlock.New()}}, nil
}

// ID returns the stable vault identity.
func (r *ContentReader) ID() string { return r.vault.ID() }

// OpenContent opens a verified stream for a live logical path.
func (r *ContentReader) OpenContent(ctx context.Context, path string) (*Content, error) {
	return r.vault.OpenContent(ctx, path)
}

// Close waits for open streams and releases the hierarchy lock.
func (r *ContentReader) Close() error {
	if r == nil {
		return nil
	}
	return r.vault.Close()
}
