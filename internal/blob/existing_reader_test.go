package blob

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestExistingReaderOwnership(t *testing.T) {
	t.Run("missing marker", func(t *testing.T) {
		metadata, blobs, _, _ := placementTestVault(t)
		marker := blobs.layout.OwnershipPath()
		require.NoError(t, os.Remove(marker))
		registry := NewRegistry(t.Context(), metadata.VaultID(), nil, nil)
		defer func() { require.NoError(t, registry.Close()) }()
		_, err := NewExistingReader(store.NewPackCatalog(metadata), blobs.dir, registry)
		require.ErrorIs(t, err, fs.ErrNotExist)
		_, err = os.Lstat(marker)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})
	t.Run("wrong marker and handoff", func(t *testing.T) {
		metadata, blobs, _, _ := placementTestVault(t)
		prior, err := blobs.readBackend.Ownership(t.Context())
		require.NoError(t, err)
		wrong := prior
		wrong.Epoch = "50000000-0000-4000-8000-000000000001"
		require.NoError(t, blobs.readBackend.ReplaceOwnership(t.Context(), wrong, &prior))
		handoff := filepath.Join(blobs.dir, "tmp", primaryRestoreHandoffName)
		require.NoError(t, os.MkdirAll(filepath.Dir(handoff), 0o700))
		require.NoError(t, os.WriteFile(handoff, []byte("pending"), 0o600))
		registry := NewRegistry(t.Context(), metadata.VaultID(), nil, nil)
		defer func() { require.NoError(t, registry.Close()) }()
		_, err = NewExistingReader(store.NewPackCatalog(metadata), blobs.dir, registry)
		require.ErrorIs(t, err, packstore.ErrStoreFenced)
		data, err := os.ReadFile(handoff)
		require.NoError(t, err)
		require.Equal(t, "pending", string(data))
	})
}
