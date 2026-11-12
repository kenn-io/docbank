package blob

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestExistingReaderLocations(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	want := []byte("synthetic placement content")
	_, hash := placementTestFile(t, metadata, blobs, want)
	registry := NewRegistry(t.Context(), metadata.VaultID(), nil, nil)
	reader, err := NewExistingReader(store.NewPackCatalog(metadata), blobs.dir, registry)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	for _, retire := range []bool{false, true} {
		plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
			TargetNodeID: metadata.RootID(), SourceStoreID: metadata.PrimaryBlobStoreID(),
			DestinationStoreID: secondary.ID, RetireSource: retire, AllowAuditedRemoteOnly: retire,
		})
		require.NoError(t, err)
		require.NoError(t, runner.Run(t.Context(), createPlacementOperation(t, metadata, plan)))
		locations, err := metadata.ResolveBlobLocations(t.Context(), packstore.Hash(hash))
		require.NoError(t, err)
		if retire {
			require.Len(t, locations.Candidates, 1)
			_, _, err = reader.OpenStreamContext(t.Context(), hash)
			require.Error(t, err)
		} else {
			require.Len(t, locations.Candidates, 2)
			stream, _, err := reader.OpenStreamContext(t.Context(), hash)
			require.NoError(t, err)
			got, err := io.ReadAll(stream)
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.NoError(t, stream.Verify())
			require.NoError(t, stream.Close())
		}
	}
}

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

func TestExistingReaderRejectsNonPrimaryCatalog(t *testing.T) {
	_, err := NewExistingReader(nil, t.TempDir(), nil)
	require.ErrorContains(t, err, "primary location catalog")
}
