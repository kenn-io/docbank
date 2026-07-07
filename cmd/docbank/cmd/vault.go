package cmd

import (
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

type vault struct {
	store *store.Store
	blobs *blob.Store
	lock  *home.Lock
}

// openVault opens the vault holding the shared inter-process lock, so
// normal commands can run concurrently with each other but never with an
// exclusive holder (gc).
func openVault() (*vault, error) { return openVaultLocked(false) }

// openVaultExclusive opens the vault holding the exclusive lock, blocking
// out every other docbank process. gc --run requires this: between the
// unreachable-blobs query and blob file removal, a concurrent ingest could
// otherwise dedup against a blob file gc is about to delete, leaving a live
// node pointing at nothing.
func openVaultExclusive() (*vault, error) { return openVaultLocked(true) }

func openVaultLocked(exclusive bool) (*vault, error) {
	layout, err := home.Resolve()
	if err != nil {
		return nil, err
	}
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	lock, err := layout.AcquireLock(exclusive)
	if err != nil {
		return nil, err
	}
	s, err := store.Open(layout.DBPath())
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	bs := blob.New(layout.BlobsDir())
	if err := cleanTmpIfSole(bs, lock, exclusive); err != nil {
		_ = s.Close()
		_ = lock.Release()
		return nil, err
	}
	return &vault{store: s, blobs: bs, lock: lock}, nil
}

// cleanTmpIfSole clears stale blob temp files only while this process holds
// the vault exclusively, so a concurrent add's in-flight temp file is never
// deleted. Shared openers try a non-blocking upgrade and skip cleanup when
// any other process is active (a later sole opener or gc picks it up).
func cleanTmpIfSole(bs *blob.Store, lock *home.Lock, exclusive bool) error {
	if exclusive {
		return bs.CleanTmp()
	}
	ok, err := lock.TryUpgrade()
	if err != nil || !ok {
		return err
	}
	if err := bs.CleanTmp(); err != nil {
		_ = lock.Downgrade()
		return err
	}
	return lock.Downgrade()
}

func (v *vault) close() error {
	err := v.store.Close()
	if lerr := v.lock.Release(); err == nil {
		err = lerr
	}
	return err
}
