package maintenance

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestVerifyReportsDamagedRedundantLocation(t *testing.T) {
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	secondary, err := metadata.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive",
	)
	require.NoError(t, err)
	secondaryPath := filepath.Join(root, "archive")
	secondaryBackend, err := blob.NewFilesystemBackend(secondaryPath, nil)
	require.NoError(t, err)
	ownership := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  metadata.VaultID(),
		Store:  packstore.StoreID(secondary.ID),
		Epoch:  secondary.OwnershipEpoch,
	}
	require.NoError(t, secondaryBackend.ReplaceOwnership(
		t.Context(), ownership, nil,
	))
	require.NoError(t, secondaryBackend.Close())
	require.NoError(t, metadata.RegisterBlobStore(t.Context(), secondary))
	registry := blob.NewRegistry(
		t.Context(), metadata.VaultID(),
		map[string]config.StoreBindingConfig{
			"archive": {
				Kind: "filesystem", Path: secondaryPath, Priority: 20,
			},
		},
		[]blob.StoreSpec{{
			ID: secondary.ID, Kind: secondary.Kind, Role: secondary.Role,
			Lifecycle: secondary.Lifecycle, Binding: secondary.Binding,
			OwnershipEpoch: secondary.OwnershipEpoch,
		}},
	)
	blobs, err := blob.NewWithOptions(
		store.NewPackCatalog(metadata), filepath.Join(root, "blobs"),
		blob.Options{Registry: registry},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	content := []byte("verify every authority")
	written, err := blobs.WriteDetailedContext(
		t.Context(), bytes.NewReader(content),
	)
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	file, err := metadata.CreateFile(
		t.Context(), metadata.RootID(), "document.txt",
		written.Hash, written.Size, "text/plain",
		store.BlobPhysical{
			Encoding: encoding, StoredBytes: written.StoredSize,
			PackEligible: written.PackEligible, Created: written.Created,
		},
	)
	require.NoError(t, err)
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	requestJSON, err := json.Marshal(plan.Request)
	require.NoError(t, err)
	planJSON, err := json.Marshal(plan)
	require.NoError(t, err)
	operation, err := metadata.CreateStorageOperation(
		t.Context(), store.StorageOperationCreate{
			Kind: "place", RequestDigest: plan.Digest,
			RequestJSON: string(requestJSON), PlanJSON: string(planJSON),
			TotalObjects: int64(len(plan.Hashes)),
		},
	)
	require.NoError(t, err)
	runner := blob.PlacementRunner{Metadata: metadata, Blobs: blobs}
	require.NoError(t, runner.Run(t.Context(), operation.ID))
	require.NoError(t, os.WriteFile(
		secondaryBackend.Layout().LoosePath(packstore.Hash(written.Hash)),
		[]byte("damaged"), 0o600,
	))

	report, err := Verify(
		t.Context(), metadata, blobs,
		VerifyOptions{Budget: Budget{MaxObjects: 1}},
	)
	require.NoError(t, err)
	assert.Zero(t, report.OK)
	require.Len(t, report.Problems, 1)
	assert.Equal(t, written.Hash, report.Problems[0].Hash)
	assert.Equal(t, secondary.ID, report.Problems[0].StoreID)
	assert.Equal(t, "corrupt", report.Problems[0].Problem)

	takeoverBackend, err := blob.NewFilesystemBackend(secondaryPath, nil)
	require.NoError(t, err)
	taken := ownership
	taken.Epoch = "50000000-0000-4000-8000-000000000001"
	require.NoError(t, takeoverBackend.ReplaceOwnership(
		t.Context(), taken, &ownership,
	))
	require.NoError(t, takeoverBackend.Close())

	report, err = Verify(
		t.Context(), metadata, blobs,
		VerifyOptions{Budget: Budget{MaxObjects: 1}},
	)
	require.NoError(t, err)
	assert.Zero(t, report.OK)
	require.Len(t, report.Problems, 1)
	assert.Equal(t, written.Hash, report.Problems[0].Hash)
	assert.Equal(t, secondary.ID, report.Problems[0].StoreID)
	assert.Equal(t, "unreadable", report.Problems[0].Problem)
}
