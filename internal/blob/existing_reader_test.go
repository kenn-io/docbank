package blob

import (
	"io"
	"os"
	"path/filepath"
	"strings"
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
			require.ErrorIs(t, err, packstore.ErrStoreUnavailable)
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
	t.Run("wrong marker and handoff", func(t *testing.T) {
		metadata, blobs, _, _ := placementTestVault(t)
		prior, err := blobs.readBackend.Ownership(t.Context())
		require.NoError(t, err)
		wrong := prior
		wrong.Epoch = "50000000-0000-4000-8000-000000000001"
		digest := strings.Repeat("a", 64)
		handoffRecord, err := NewPrimaryRestoreHandoff(blobs.dir, wrong, &digest)
		require.NoError(t, err)
		require.NoError(t, handoffRecord.Prepare(t.Context()))
		handoff := filepath.Join(blobs.dir, "tmp", primaryRestoreHandoffName)
		before, err := os.ReadFile(handoff)
		require.NoError(t, err)
		registry := NewRegistry(t.Context(), metadata.VaultID(), nil, nil)
		defer func() { require.NoError(t, registry.Close()) }()
		_, err = NewExistingReader(store.NewPackCatalog(metadata), blobs.dir, registry)
		require.ErrorIs(t, err, packstore.ErrStoreFenced)
		data, err := os.ReadFile(handoff)
		require.NoError(t, err)
		require.Equal(t, before, data)
	})
}
