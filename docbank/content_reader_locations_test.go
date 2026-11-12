package docbank

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestContentReaderLocations(t *testing.T) {
	t.Parallel()
	root, secondaryRoot := t.TempDir(), t.TempDir()
	require.NoError(t, blob.EnsureFilesystemNamespace(secondaryRoot))
	metadata, err := store.Open(t.Context(), filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	secondary, err := metadata.PrepareSecondaryBlobStore("archive", "filesystem", "archive")
	require.NoError(t, err)
	backend, err := blob.NewFilesystemBackend(secondaryRoot, nil)
	require.NoError(t, err)
	require.NoError(t, backend.ReplaceOwnership(t.Context(), packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: metadata.VaultID(),
		Store: packstore.StoreID(secondary.ID), Epoch: secondary.OwnershipEpoch,
	}, nil))
	require.NoError(t, backend.Close())
	require.NoError(t, metadata.RegisterBlobStore(t.Context(), secondary))
	require.NoError(t, metadata.Close())
	vault, err := New(t.Context(), Config{Root: root, StoreBindings: map[string]StoreBinding{
		"archive": {Kind: "filesystem", Path: secondaryRoot},
	}})
	require.NoError(t, err)
	receipt, err := vault.Put(t.Context(), "/source", strings.NewReader("synthetic original"), PutOptions{})
	require.NoError(t, err)
	plan, err := vault.metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: vault.metadata.RootID(), SourceStoreID: vault.metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID, RetireSource: true, AllowAuditedRemoteOnly: true,
	})
	require.NoError(t, err)
	planJSON, err := json.Marshal(plan)
	require.NoError(t, err)
	requestJSON, err := json.Marshal(plan.Request)
	require.NoError(t, err)
	operation, err := vault.metadata.CreateStorageOperation(t.Context(), store.StorageOperationCreate{
		Kind: "place", RequestDigest: plan.Digest, RequestJSON: string(requestJSON),
		PlanJSON: string(planJSON), TotalObjects: int64(len(plan.Hashes)),
	})
	require.NoError(t, err)
	require.NoError(t, (blob.PlacementRunner{Metadata: vault.metadata, Blobs: vault.blobs}).Run(t.Context(), operation.ID))
	locations, err := vault.metadata.ResolveBlobLocations(t.Context(), packstore.Hash(receipt.Computed.SHA256))
	require.NoError(t, err)
	require.Len(t, locations.Candidates, 1)
	require.NoError(t, vault.Close())
	reader, err := OpenContentReader(t.Context(), ContentReaderConfig{Root: root})
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	_, err = reader.OpenContent(t.Context(), "/source")
	require.ErrorIs(t, err, ErrContentUnavailable)
}
