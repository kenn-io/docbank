package blob

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestPlacementRunnerCopiesVerifiesAndRetiresLooseSource(t *testing.T) {
	metadata, blobs, runner, destination := placementTestVault(t)
	content := []byte("durable placement content")
	file, hash := placementTestFile(t, metadata, blobs, content)
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination.ID, RetireSource: true,
	})
	require.NoError(t, err)
	operationID := createPlacementOperation(t, metadata, plan)

	require.NoError(t, runner.Run(t.Context(), operationID))

	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
	var receipt PlacementReceipt
	require.NoError(t, json.Unmarshal([]byte(operation.ReceiptJSON), &receipt))
	assert.Equal(t, int64(1), receipt.Completed)
	assert.Equal(t, int64(1), receipt.Copied)
	assert.Equal(t, int64(1), receipt.SourceRevoked)

	resolution, err := metadata.ResolveBlobLocations(t.Context(), packstore.Hash(hash))
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 1)
	assert.Equal(t, packstore.StoreID(destination.ID), resolution.Candidates[0].StoreID)
	stream, size, err := blobs.OpenStreamContext(t.Context(), hash)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, int64(len(content)), size)
	assert.Equal(t, content, got)
}

func TestPlacementRunnerResumesAfterCatalogCommitBeforeProgress(t *testing.T) {
	metadata, blobs, runner, destination := placementTestVault(t)
	file, _ := placementTestFile(t, metadata, blobs, []byte("resume me"))
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination.ID, RetireSource: true,
	})
	require.NoError(t, err)
	operationID := createPlacementOperation(t, metadata, plan)
	_, err = metadata.ClaimStorageOperation(t.Context(), operationID)
	require.NoError(t, err)

	result, err := runner.placeOne(
		t.Context(), operationID, plan.Request, plan.Hashes[0],
	)
	require.NoError(t, err)
	assert.True(t, result.DestinationAuthorized)
	assert.True(t, result.SourceRevoked)
	cleanups, err := metadata.StorageOperationCleanups(t.Context(), operationID)
	require.NoError(t, err)
	require.Len(t, cleanups, 1)

	require.NoError(t, runner.Run(t.Context(), operationID))
	cleanups, err = metadata.StorageOperationCleanups(t.Context(), operationID)
	require.NoError(t, err)
	assert.Empty(t, cleanups)
	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
	var receipt PlacementReceipt
	require.NoError(t, json.Unmarshal([]byte(operation.ReceiptJSON), &receipt))
	assert.Equal(t, int64(1), receipt.Completed)
	assert.Equal(t, int64(1), receipt.Copied)
	assert.Equal(t, int64(1), receipt.SourceRevoked)
}

func TestPlacementCommitRejectsDetachedDestination(t *testing.T) {
	metadata, blobs, _, destination := placementTestVault(t)
	file, hashText := placementTestFile(t, metadata, blobs, []byte("lifecycle fence"))
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination.ID,
	})
	require.NoError(t, err)
	operationID := createPlacementOperation(t, metadata, plan)
	hash := packstore.Hash(hashText)
	source, ok := blobs.ReadBackend(packstore.StoreID(metadata.PrimaryBlobStoreID()))
	require.True(t, ok)
	target, ok := blobs.WritableBackend(packstore.StoreID(destination.ID))
	require.True(t, ok)
	moved, err := packstore.Move(t.Context(), source, target, packstore.MoveRequest{
		Source: plan.Hashes[0].Source, Destination: packstore.StoreID(destination.ID),
		Identity: packstore.BlobIdentity{Hash: hash, Size: plan.Hashes[0].Size},
	})
	require.NoError(t, err)
	require.NoError(t, metadata.DetachBlobStore(t.Context(), destination.ID))

	_, err = metadata.CommitPlacement(
		t.Context(), operationID, plan.Request, plan.Hashes[0], moved.Destination,
	)
	require.ErrorIs(t, err, store.ErrBlobStoreState)
}

func TestPlacementRunnerEvacuatesSecondaryToPrimaryAndDetachesIt(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, _ := placementTestFile(t, metadata, blobs, []byte("bring this home"))
	outbound, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID, RetireSource: true,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, outbound),
	))

	require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
	inbound, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
	})
	require.NoError(t, err)
	operationID := createStorageOperation(t, metadata, "evacuate", inbound)

	require.NoError(t, runner.Run(t.Context(), operationID))

	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
	evacuated, err := metadata.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", evacuated.Lifecycle)

	stream, _, err := blobs.OpenStreamContext(t.Context(), inbound.Hashes[0].Hash)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, []byte("bring this home"), got)
}

func TestPlacementRunnerRepairsDamagedSecondaryFromVerifiedPrimary(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, hash := placementTestFile(t, metadata, blobs, []byte("repair authority"))
	placement, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, placement),
	))
	backend, ok := blobs.WritableBackend(packstore.StoreID(secondary.ID))
	require.True(t, ok)
	filesystem, ok := backend.(*packstore.FilesystemBackend)
	require.True(t, ok)
	require.NoError(t, os.WriteFile(
		filesystem.Layout().LoosePath(packstore.Hash(hash)), []byte("damaged"), 0o600,
	))

	plan, err := metadata.PlanStorageRecovery(
		t.Context(), "repair", hash, secondary.ID,
	)
	require.NoError(t, err)
	operationID := createRecoveryOperation(t, metadata, plan)
	require.NoError(t, runner.Run(t.Context(), operationID))

	location, err := runner.currentLocation(t.Context(), hash, secondary.ID)
	require.NoError(t, err)
	stream, _, err := backend.OpenLoose(
		t.Context(), packstore.Hash(hash), *location.Loose,
	)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, []byte("repair authority"), got)
}

func TestPlacementRunnerSalvagesVerifiedBytesFromFencedSecondary(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, hash := placementTestFile(t, metadata, blobs, []byte("salvage authority"))
	placement, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID, RetireSource: true,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, placement),
	))
	backend, ok := blobs.WritableBackend(packstore.StoreID(secondary.ID))
	require.True(t, ok)
	prior, err := backend.Ownership(t.Context())
	require.NoError(t, err)
	taken := prior
	taken.Epoch = "50000000-0000-4000-8000-000000000001"
	require.NoError(t, backend.ReplaceOwnership(t.Context(), taken, &prior))
	observation := blobs.RefreshStore(t.Context(), secondary.ID)
	assert.Equal(t, StoreFenced, observation.State)

	plan, err := metadata.PlanStorageRecovery(
		t.Context(), "salvage", hash, secondary.ID,
	)
	require.NoError(t, err)
	operationID := createRecoveryOperation(t, metadata, plan)
	require.NoError(t, runner.Run(t.Context(), operationID))

	stream, _, err := blobs.OpenStreamContext(t.Context(), hash)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, []byte("salvage authority"), got)
}

func TestPlacementRunnerHonorsRecoveryCancellationBeforePublication(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, hash := placementTestFile(t, metadata, blobs, []byte("cancel recovery"))
	placement, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, placement),
	))
	plan, err := metadata.PlanStorageRecovery(
		t.Context(), "repair", hash, secondary.ID,
	)
	require.NoError(t, err)
	operationID := createRecoveryOperation(t, metadata, plan)
	require.NoError(t, metadata.RequestStorageOperationCancel(t.Context(), operationID))

	require.NoError(t, runner.Run(t.Context(), operationID))
	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCancelled, operation.State)
}

func placementTestVault(
	t *testing.T,
) (*store.Store, *Store, PlacementRunner, store.BlobStore) {
	t.Helper()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	destination, err := metadata.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive",
	)
	require.NoError(t, err)
	destinationPath := filepath.Join(root, "archive")
	backend, err := NewFilesystemBackend(destinationPath, nil)
	require.NoError(t, err)
	require.NoError(t, backend.ReplaceOwnership(t.Context(), packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: metadata.VaultID(),
		Store: packstore.StoreID(destination.ID), Epoch: destination.OwnershipEpoch,
	}, nil))
	require.NoError(t, backend.Close())
	require.NoError(t, metadata.RegisterBlobStore(t.Context(), destination))
	bindings := map[string]config.StoreBindingConfig{
		"archive": {Kind: "filesystem", Path: destinationPath, Priority: 20},
	}
	registry := NewRegistry(t.Context(), metadata.VaultID(), bindings, []StoreSpec{{
		ID: destination.ID, Kind: destination.Kind, Role: destination.Role,
		Lifecycle: destination.Lifecycle, Binding: destination.Binding,
		OwnershipEpoch: destination.OwnershipEpoch,
	}})
	options := Options{Registry: registry}
	blobs, err := NewWithOptions(
		store.NewPackCatalog(metadata), filepath.Join(root, "blobs"), options,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	return metadata, blobs, PlacementRunner{Metadata: metadata, Blobs: blobs}, destination
}

func placementTestFile(
	t *testing.T, metadata *store.Store, blobs *Store, content []byte,
) (store.Node, string) {
	t.Helper()
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(content))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	file, err := metadata.CreateFile(
		t.Context(), metadata.RootID(), "document.txt", written.Hash, written.Size,
		"text/plain", store.BlobPhysical{
			Encoding: encoding, StoredBytes: written.StoredSize,
			PackEligible: written.PackEligible, Created: written.Created,
		},
	)
	require.NoError(t, err)
	return file, written.Hash
}

func createPlacementOperation(
	t *testing.T, metadata *store.Store, plan store.PlacementPlan,
) string {
	t.Helper()
	return createStorageOperation(t, metadata, "place", plan)
}

func createStorageOperation(
	t *testing.T, metadata *store.Store, kind string, plan store.PlacementPlan,
) string {
	t.Helper()
	planJSON, err := json.Marshal(plan)
	require.NoError(t, err)
	requestJSON, err := json.Marshal(plan.Request)
	require.NoError(t, err)
	operation, err := metadata.CreateStorageOperation(t.Context(), store.StorageOperationCreate{
		Kind: kind, RequestDigest: plan.Digest, RequestJSON: string(requestJSON),
		PlanJSON: string(planJSON), TotalObjects: int64(len(plan.Hashes)),
	})
	require.NoError(t, err)
	return operation.ID
}

func createRecoveryOperation(
	t *testing.T, metadata *store.Store, plan store.StorageRecoveryPlan,
) string {
	t.Helper()
	planJSON, err := json.Marshal(plan)
	require.NoError(t, err)
	operation, err := metadata.CreateStorageOperation(t.Context(), store.StorageOperationCreate{
		Kind: plan.Kind, RequestDigest: plan.Digest, RequestJSON: string(planJSON),
		PlanJSON: string(planJSON), TotalObjects: 1,
	})
	require.NoError(t, err)
	return operation.ID
}
