package cmd

import (
	"fmt"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

type vault struct {
	store *store.Store
	blobs *blob.Store
}

// openVault resolves the data directory, ensures the layout, opens the
// store, and clears stale blob temp files.
func openVault() (*vault, error) {
	layout, err := home.Resolve()
	if err != nil {
		return nil, err
	}
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	s, err := store.Open(layout.DBPath())
	if err != nil {
		return nil, err
	}
	bs := blob.New(layout.BlobsDir())
	if err := bs.CleanTmp(); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("clearing blob temp dir: %w", err)
	}
	return &vault{store: s, blobs: bs}, nil
}

func (v *vault) close() error { return v.store.Close() }
