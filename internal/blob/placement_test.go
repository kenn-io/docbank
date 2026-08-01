package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
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

func TestPlacementRunnerRetriesFinalPersistenceFailure(t *testing.T) {
	metadata, blobs, runner, destination, root := placementTestVaultWithSecondaries(
		t, "archive",
	)
	file, _ := placementTestFile(t, metadata, blobs, []byte("retry final receipt"))
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination[0].ID,
	})
	require.NoError(t, err)
	operationID := createPlacementOperation(t, metadata, plan)

	db, err := metadata.SQLiteDriver().Open(filepath.Join(root, "metadata.db"),
		docsqlite.OpenOptions{
			Access:          docsqlite.ReadWriteExisting,
			TransactionMode: docsqlite.Immediate,
		})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `
		CREATE TRIGGER reject_storage_operation_completion
		BEFORE UPDATE OF state ON storage_operations
		WHEN NEW.state='completed'
		BEGIN SELECT RAISE(ABORT, 'synthetic completion persistence failure'); END`)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	supervisor := jobs.New(ctx, nil)
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	runner.RetryDelay = 10 * time.Millisecond
	require.NoError(t, runner.Start(supervisor, operationID))
	require.Eventually(t, func() bool {
		operation, readErr := metadata.StorageOperation(t.Context(), operationID)
		if readErr != nil || operation.State != store.StorageOperationRunning ||
			operation.CompletedObjects != 1 {
			return false
		}
		snapshots := supervisor.Snapshot()
		return len(snapshots) == 1 && snapshots[0].Status == jobs.StatusRunning
	}, 3*time.Second, 10*time.Millisecond)

	_, err = db.ExecContext(t.Context(), `DROP TRIGGER reject_storage_operation_completion`)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		operation, readErr := metadata.StorageOperation(t.Context(), operationID)
		if readErr != nil || operation.State != store.StorageOperationCompleted {
			return false
		}
		snapshots := supervisor.Snapshot()
		return len(snapshots) == 1 && snapshots[0].Status == jobs.StatusCompleted
	}, 3*time.Second, 10*time.Millisecond)
}

func TestPlacementRunnerRevokesPackedSourceAuthority(t *testing.T) {
	metadata, blobs, runner, destination := placementTestVault(t)
	content := []byte("packed source placement")
	file, hash := placementTestFile(t, metadata, blobs, content)
	packed, err := blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)

	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination.ID, RetireSource: true,
	})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 1)
	require.NotNil(t, plan.Hashes[0].Source.Pack)
	assert.True(t, plan.Hashes[0].RetireSource)
	assert.True(t, plan.Hashes[0].PackRepackRequired)
	assert.Equal(t, int64(len(content)), plan.PackBlockedBytes)

	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, plan),
	))

	resolution, err := metadata.ResolveBlobLocations(
		t.Context(), packstore.Hash(hash),
	)
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 1)
	assert.Equal(t, packstore.StoreID(destination.ID), resolution.Candidates[0].StoreID)
}

func TestPlacementRunnerEvacuatesPackedSecondaryAfterPlacementRevokesMappings(
	t *testing.T,
) {
	metadata, blobs, runner, secondaries, root := placementTestVaultWithSecondaries(
		t, "archive",
	)
	secondary := secondaries[0]
	file, hash := placementTestFile(
		t, metadata, blobs, []byte("packed secondary evacuation"),
	)
	packed, err := blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)
	catalog := store.NewPackCatalog(metadata)
	records, err := catalog.ListPackRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	entries, err := catalog.ListPackEntries(t.Context(), records[0].PackID)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	backend, ok := blobs.WritableBackend(packstore.StoreID(secondary.ID))
	require.True(t, ok)
	filesystem, ok := backend.(*packstore.FilesystemBackend)
	require.True(t, ok)
	source, err := os.Open(blobs.layout.PackPath(records[0].PackID))
	require.NoError(t, err)
	published, publishErr := backend.PublishPack(
		t.Context(), records[0].PackID, source, packstore.PublishOptions{
			ExpectedSize: records[0].StoredBytes, SizeKnown: true,
		},
	)
	require.NoError(t, errors.Join(publishErr, source.Close()))

	db, err := metadata.SQLiteDriver().Open(filepath.Join(root, "metadata.db"),
		docsqlite.OpenOptions{
			Access:          docsqlite.ReadWriteExisting,
			TransactionMode: docsqlite.Immediate,
		})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO blob_packs(
			store_id,pack_id,entry_count,stored_bytes,created_at
		) VALUES(?,?,?,?,?)`,
		secondary.ID, records[0].PackID, records[0].EntryCount,
		records[0].StoredBytes, records[0].CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)
	entry := entries[0]
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO blob_pack_entries(
			blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c
		) VALUES(?,?,?,?,?,?,?,?)`,
		entry.Hash.String(), secondary.ID, entry.PackID, entry.Offset,
		entry.StoredLen, entry.RawLen, entry.Flags, entry.CRC32C,
	)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'packed',NULL,?,1)`,
		hash, secondary.ID, published.Generation, entry.StoredLen,
	)
	require.NoError(t, err)

	placement, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, placement),
	))
	var remainingMappings int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM blob_pack_entries WHERE store_id=?`,
		secondary.ID,
	).Scan(&remainingMappings))
	assert.Zero(t, remainingMappings)
	require.FileExists(t, filesystem.Layout().PackPath(records[0].PackID))
	secondPackID := pack.NewPackID()
	packBytes, err := os.ReadFile(filesystem.Layout().PackPath(records[0].PackID))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filesystem.Layout().PackPath(secondPackID), packBytes, 0o600,
	))
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO blob_packs(
			store_id,pack_id,entry_count,stored_bytes,created_at
		) VALUES(?,?,1,?,?)`,
		secondary.ID, secondPackID, int64(len(packBytes)),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)

	evacuation, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(),
		RetireSource:       true, Evacuate: true,
	})
	require.NoError(t, err)
	require.Empty(t, evacuation.Hashes)
	require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
	evacuationID := createStorageOperation(t, metadata, "evacuate", evacuation)
	backend = blobs.registry.backends[packstore.StoreID(secondary.ID)]
	failing := &failOnceRetireBackend{Backend: backend, failed: make(chan struct{})}
	failing.failAt.Store(2)
	blobs.registry.backends[packstore.StoreID(secondary.ID)] = failing
	require.ErrorIs(t, runner.Run(t.Context(), evacuationID), errStorageOperationDeferred)
	operation, err := metadata.StorageOperation(t.Context(), evacuationID)
	require.NoError(t, err)
	var progress PlacementReceipt
	require.NoError(t, json.Unmarshal([]byte(operation.ReceiptJSON), &progress))
	assert.Equal(t, int64(1), progress.CleanupPending)

	require.NoError(t, runner.Run(t.Context(), evacuationID))

	require.NoFileExists(t, filesystem.Layout().PackPath(records[0].PackID))
	require.NoFileExists(t, filesystem.Layout().PackPath(secondPackID))
	cleanups, err := metadata.StorageOperationCleanups(t.Context(), evacuationID)
	require.NoError(t, err)
	assert.Empty(t, cleanups)
	evacuated, err := metadata.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", evacuated.Lifecycle)
	operation, err = metadata.StorageOperation(t.Context(), evacuationID)
	require.NoError(t, err)
	var receipt PlacementReceipt
	require.NoError(t, json.Unmarshal([]byte(operation.ReceiptJSON), &receipt))
	assert.Zero(t, receipt.CleanupPending)
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

func TestPlacementRunnerCompletesCleanupWhenRetiredObjectIsAlreadyMissing(t *testing.T) {
	metadata, blobs, runner, secondaryID, operationID := pendingCleanupReplay(t)
	backend := blobs.registry.backends[packstore.StoreID(secondaryID)]
	blobs.registry.backends[packstore.StoreID(secondaryID)] =
		&missingRetireBackend{Backend: backend, err: packstore.ErrPhysicalMissing}

	require.NoError(t, runner.Run(t.Context(), operationID))
	cleanups, err := metadata.StorageOperationCleanups(t.Context(), operationID)
	require.NoError(t, err)
	assert.Empty(t, cleanups)
	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
}

func TestPlacementRunnerRetriesCleanupWhenStoreRootIsUnavailable(t *testing.T) {
	metadata, blobs, runner, secondaryID, operationID := pendingCleanupReplay(t)
	backend := blobs.registry.backends[packstore.StoreID(secondaryID)]
	blobs.registry.backends[packstore.StoreID(secondaryID)] =
		&missingRetireBackend{Backend: backend, err: fs.ErrNotExist}

	err := runner.Run(t.Context(), operationID)
	require.ErrorIs(t, err, fs.ErrNotExist)
	cleanups, cleanupErr := metadata.StorageOperationCleanups(t.Context(), operationID)
	require.NoError(t, cleanupErr)
	assert.Len(t, cleanups, 1)
	operation, operationErr := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, operationErr)
	assert.Equal(t, store.StorageOperationQueued, operation.State)
}

func pendingCleanupReplay(
	t *testing.T,
) (*store.Store, *Store, PlacementRunner, string, string) {
	t.Helper()
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, _ := placementTestFile(t, metadata, blobs, []byte("retired before replay"))
	copyPlan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, copyPlan),
	))
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
	})
	require.NoError(t, err)
	operationID := createPlacementOperation(t, metadata, plan)
	_, err = metadata.ClaimStorageOperation(t.Context(), operationID)
	require.NoError(t, err)

	result, err := runner.placeOne(
		t.Context(), operationID, plan.Request, plan.Hashes[0],
	)
	require.NoError(t, err)
	assert.True(t, result.SourceRevoked)
	cleanups, err := metadata.StorageOperationCleanups(t.Context(), operationID)
	require.NoError(t, err)
	require.Len(t, cleanups, 1)
	return metadata, blobs, runner, secondary.ID, operationID
}

func TestPlacementRunnerCleanupRetryWaitsForConcurrentIngestPublication(t *testing.T) {
	metadata, blobs, runner, destination := placementTestVault(t)
	content := []byte("reauthorize while cleanup waits")
	file, hash := placementTestFile(t, metadata, blobs, content)
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination.ID, RetireSource: true,
	})
	require.NoError(t, err)
	operationID := createPlacementOperation(t, metadata, plan)
	_, err = metadata.ClaimStorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	_, err = runner.placeOne(t.Context(), operationID, plan.Request, plan.Hashes[0])
	require.NoError(t, err)
	cleanups, err := metadata.StorageOperationCleanups(t.Context(), operationID)
	require.NoError(t, err)
	require.Len(t, cleanups, 1)
	primary, ok := blobs.WritableBackend(packstore.StoreID(metadata.PrimaryBlobStoreID()))
	require.True(t, ok)
	require.NoError(t, primary.Retire(t.Context(), cleanups[0].Ref))

	written := make(chan WriteReceipt, 1)
	publish := make(chan struct{})
	ingestDone := make(chan error, 1)
	go func() {
		ingestDone <- blobs.WithMutation(t.Context(), func() error {
			receipt, writeErr := blobs.WriteDetailedContext(
				t.Context(), bytes.NewReader(content),
			)
			if writeErr != nil {
				return writeErr
			}
			written <- receipt
			<-publish
			encoding, encodingErr := receipt.EncodingName()
			if encodingErr != nil {
				return encodingErr
			}
			_, createErr := metadata.CreateFile(
				t.Context(), metadata.RootID(), "reauthorized.txt",
				receipt.Hash, receipt.Size, "text/plain", store.BlobPhysical{
					Encoding: encoding, StoredBytes: receipt.StoredSize,
					PackEligible: receipt.PackEligible, Created: receipt.Created,
				},
			)
			return createErr
		})
	}()
	republished := <-written
	require.True(t, republished.Created)

	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- runner.Run(t.Context(), operationID) }()
	maintenanceQueued := false
	deadline := time.After(time.Second)
	for !maintenanceQueued {
		select {
		case <-deadline:
			t.Fatal("cleanup did not wait behind the active ingest mutation")
		default:
		}
		probeCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		probeErr := blobs.WithMutation(probeCtx, func() error { return nil })
		cancel()
		maintenanceQueued = errors.Is(probeErr, context.DeadlineExceeded)
	}
	close(publish)
	require.NoError(t, <-ingestDone)
	require.NoError(t, <-cleanupDone)

	cleanups, err = metadata.StorageOperationCleanups(t.Context(), operationID)
	require.NoError(t, err)
	assert.Empty(t, cleanups)
	primaryLocation, err := runner.currentLocation(
		t.Context(), hash, metadata.PrimaryBlobStoreID(),
	)
	require.NoError(t, err)
	_, err = blobs.VerifyLocation(t.Context(), hash, primaryLocation)
	require.NoError(t, err)
}

func TestPlacementRunnerResumesAfterCommittedDestinationRepair(t *testing.T) {
	metadata, blobs, runner, secondaries, _ := placementTestVaultWithSecondaries(
		t, "archive", "mirror",
	)
	archive, mirror := secondaries[0], secondaries[1]
	file, hash := placementTestFile(t, metadata, blobs, []byte("repair then resume"))
	for _, destination := range []store.BlobStore{archive, mirror} {
		plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
			TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
			DestinationStoreID: destination.ID,
		})
		require.NoError(t, err)
		require.NoError(t, runner.Run(
			t.Context(), createPlacementOperation(t, metadata, plan),
		))
	}

	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: archive.ID, RetireSource: true,
	})
	require.NoError(t, err)
	require.NotNil(t, plan.Hashes[0].Destination)
	operationID := createPlacementOperation(t, metadata, plan)
	_, err = metadata.ClaimStorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	result, err := runner.placeOne(
		t.Context(), operationID, plan.Request, plan.Hashes[0],
	)
	require.NoError(t, err)
	require.True(t, result.SourceRevoked)

	recovery, err := metadata.PlanStorageRecovery(t.Context(), "repair", hash, archive.ID)
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createRecoveryOperation(t, metadata, recovery),
	))
	repaired, err := runner.currentLocation(t.Context(), hash, archive.ID)
	require.NoError(t, err)
	require.NotEqual(t, plan.Hashes[0].Destination.Generation, repaired.Generation)

	require.NoError(t, runner.Run(t.Context(), operationID))
	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
	_, err = blobs.VerifyLocation(t.Context(), hash, repaired)
	require.NoError(t, err)
}

func TestPlacementRunnerReschedulesFailedPhysicalCleanup(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, _ := placementTestFile(t, metadata, blobs, []byte("retry cleanup"))
	copyPlan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, copyPlan),
	))

	backend, ok := blobs.registry.backends[packstore.StoreID(secondary.ID)]
	require.True(t, ok)
	failing := &failOnceRetireBackend{Backend: backend, failed: make(chan struct{})}
	failing.fail.Store(true)
	blobs.registry.backends[packstore.StoreID(secondary.ID)] = failing

	retirePlan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
	})
	require.NoError(t, err)
	operationID := createPlacementOperation(t, metadata, retirePlan)
	runner.RetryDelay = 100 * time.Millisecond
	supervisor := jobs.New(t.Context(), nil)
	t.Cleanup(supervisor.Stop)
	require.NoError(t, runner.Start(supervisor, operationID))

	select {
	case <-failing.failed:
	case <-time.After(time.Second):
		t.Fatal("cleanup failure was not observed")
	}
	require.Eventually(t, func() bool {
		operation, operationErr := metadata.StorageOperation(t.Context(), operationID)
		return operationErr == nil && operation.State == store.StorageOperationQueued &&
			operation.Error != ""
	}, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		operation, operationErr := metadata.StorageOperation(t.Context(), operationID)
		return operationErr == nil && operation.State == store.StorageOperationCompleted
	}, time.Second, 10*time.Millisecond)
}

func TestPlacementRunnerRecordsCommittedProgressBeforeCleanupRetry(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, _ := placementTestFile(t, metadata, blobs, []byte("durable progress"))
	copyPlan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, copyPlan),
	))

	backend, ok := blobs.registry.backends[packstore.StoreID(secondary.ID)]
	require.True(t, ok)
	failing := &failOnceRetireBackend{Backend: backend, failed: make(chan struct{})}
	failing.fail.Store(true)
	blobs.registry.backends[packstore.StoreID(secondary.ID)] = failing
	retirePlan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
	})
	require.NoError(t, err)
	operationID := createPlacementOperation(t, metadata, retirePlan)

	err = runner.Run(t.Context(), operationID)
	require.ErrorIs(t, err, errStorageOperationDeferred)
	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationQueued, operation.State)
	assert.Equal(t, int64(1), operation.CompletedObjects)
	var progress PlacementReceipt
	require.NoError(t, json.Unmarshal([]byte(operation.ReceiptJSON), &progress))
	assert.Equal(t, int64(1), progress.Completed)
	assert.Equal(t, int64(1), progress.SourceRevoked)
	assert.Equal(t, int64(1), progress.CleanupPending)
	require.Len(t, progress.Objects, 1)
	assert.True(t, progress.Objects[0].CleanupPending)

	require.NoError(t, metadata.RequestStorageOperationCancel(t.Context(), operationID))
	require.NoError(t, runner.Run(t.Context(), operationID))
	operation, err = metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
	var receipt PlacementReceipt
	require.NoError(t, json.Unmarshal([]byte(operation.ReceiptJSON), &receipt))
	assert.Equal(t, int64(1), receipt.Completed)
	assert.Equal(t, int64(1), receipt.SourceRevoked)
	assert.Zero(t, receipt.CleanupPending)
	require.Len(t, receipt.Objects, 1)
	assert.False(t, receipt.Objects[0].CleanupPending)
}

func TestPlacementRunnerDoesNotRetireInverseMoveDestination(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	content := []byte("inverse move authority")
	file, hash := placementTestFile(t, metadata, blobs, content)
	copyPlan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, copyPlan),
	))

	toPrimary, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
	})
	require.NoError(t, err)
	primaryOperation := createPlacementOperation(t, metadata, toPrimary)
	backend := blobs.registry.backends[packstore.StoreID(secondary.ID)]
	failing := &failOnceRetireBackend{Backend: backend, failed: make(chan struct{})}
	failing.fail.Store(true)
	blobs.registry.backends[packstore.StoreID(secondary.ID)] = failing

	err = runner.Run(t.Context(), primaryOperation)
	require.ErrorIs(t, err, errStorageOperationDeferred)
	toSecondary, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID, RetireSource: true,
	})
	require.NoError(t, err)
	secondaryOperation := createPlacementOperation(t, metadata, toSecondary)
	require.NoError(t, runner.Run(t.Context(), secondaryOperation))
	require.NoError(t, runner.Run(t.Context(), primaryOperation))

	resolution, err := metadata.ResolveBlobLocations(t.Context(), packstore.Hash(hash))
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 1)
	assert.Equal(t, packstore.StoreID(secondary.ID), resolution.Candidates[0].StoreID)
	stream, size, err := blobs.OpenStreamContext(t.Context(), hash)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, int64(len(content)), size)
	assert.Equal(t, content, got)
}

type failOnceRetireBackend struct {
	packstore.Backend

	fail     atomic.Bool
	failAt   atomic.Int64
	attempts atomic.Int64
	failed   chan struct{}
}

type missingRetireBackend struct {
	packstore.Backend

	err error
}

func (b *missingRetireBackend) Retire(context.Context, packstore.ObjectRef) error {
	return b.err
}

func (b *failOnceRetireBackend) Retire(
	ctx context.Context, ref packstore.ObjectRef,
) error {
	if b.fail.Swap(false) {
		close(b.failed)
		return errors.New("synthetic cleanup failure")
	}
	if failAt := b.failAt.Load(); failAt > 0 && b.attempts.Add(1) == failAt {
		close(b.failed)
		return errors.New("synthetic cleanup failure")
	}
	if err := b.Backend.Retire(ctx, ref); err != nil {
		return fmt.Errorf("retiring through test backend: %w", err)
	}
	return nil
}

func TestPlacementRunnerVerifiesExistingDestinationBeforeRetiringSource(t *testing.T) {
	metadata, blobs, runner, destination := placementTestVault(t)
	content := []byte("keep the last healthy authority")
	file, hash := placementTestFile(t, metadata, blobs, content)
	copyPlan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination.ID,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, copyPlan),
	))
	backend, ok := blobs.WritableBackend(packstore.StoreID(destination.ID))
	require.True(t, ok)
	filesystem, ok := backend.(*packstore.FilesystemBackend)
	require.True(t, ok)
	require.NoError(t, os.WriteFile(
		filesystem.Layout().LoosePath(packstore.Hash(hash)), []byte("damaged"), 0o600,
	))

	retirePlan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination.ID, RetireSource: true,
	})
	require.NoError(t, err)
	err = runner.Run(
		t.Context(), createPlacementOperation(t, metadata, retirePlan),
	)
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)

	resolution, err := metadata.ResolveBlobLocations(
		t.Context(), packstore.Hash(hash),
	)
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 2)
	var primary packstore.ReadLocation
	for _, candidate := range resolution.Candidates {
		if candidate.StoreID == packstore.StoreID(metadata.PrimaryBlobStoreID()) {
			primary = candidate
		}
	}
	require.NotEmpty(t, primary.StoreID)
	primaryBackend, ok := blobs.ReadBackend(primary.StoreID)
	require.True(t, ok)
	stream, size, err := openRecoverySource(
		t.Context(), primaryBackend, packstore.Hash(hash), primary,
	)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
	assert.Equal(t, int64(len(content)), size)
	assert.Equal(t, content, got)
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

	inbound, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
		Evacuate: true,
	})
	require.NoError(t, err)
	require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
	operationID := createStorageOperation(t, metadata, "evacuate", inbound)

	require.NoError(t, runner.Run(t.Context(), operationID))

	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
	evacuated, err := metadata.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", evacuated.Lifecycle)
	assert.Equal(t, StoreDetached, blobs.registry.Observation(secondary.ID).State)
	_, readable := blobs.ReadBackend(packstore.StoreID(secondary.ID))
	assert.False(t, readable)
	_, writable := blobs.WritableBackend(packstore.StoreID(secondary.ID))
	assert.False(t, writable)

	stream, _, err := blobs.OpenStreamContext(t.Context(), inbound.Hashes[0].Hash)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, []byte("bring this home"), got)
}

func TestPlacementRunnerDefersEvacuationWhileAnotherOperationUsesSource(t *testing.T) {
	metadata, _, runner, secondary := placementTestVault(t)
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
		Evacuate: true,
	})
	require.NoError(t, err)
	require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
	evacuationID := createStorageOperation(t, metadata, "evacuate", plan)
	conflict, err := metadata.CreateStorageOperation(t.Context(), store.StorageOperationCreate{
		Kind: "repair", StoreReferences: []store.StorageOperationStoreReference{
			{StoreID: secondary.ID, Role: "source"},
			{StoreID: metadata.PrimaryBlobStoreID(), Role: "destination"},
		},
		RequestDigest: "abababababababababababababababababababababababababababababababab",
		RequestJSON:   `{}`, PlanJSON: `{}`,
	})
	require.NoError(t, err)
	_, err = metadata.ClaimStorageOperation(t.Context(), conflict.ID)
	require.NoError(t, err)

	err = runner.Run(t.Context(), evacuationID)
	require.ErrorIs(t, err, errStorageOperationDeferred)
	evacuation, err := metadata.StorageOperation(t.Context(), evacuationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationQueued, evacuation.State)

	require.NoError(t, metadata.FinishStorageOperation(
		t.Context(), conflict.ID, store.StorageOperationCompleted, `{}`, "",
		time.Now().Add(time.Hour),
	))
	require.NoError(t, runner.Run(t.Context(), evacuationID))
	evacuation, err = metadata.StorageOperation(t.Context(), evacuationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, evacuation.State)
	detached, err := metadata.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", detached.Lifecycle)
}

func TestPlacementRunnerEvacuatesRetainedBlobWithoutVersionReferences(t *testing.T) {
	t.Run("missing primary is restored before source retirement", func(t *testing.T) {
		metadata, blobs, runner, secondary := placementTestVault(t)
		content := []byte("retained during the GC regret window")
		file, hash := placementTestFile(t, metadata, blobs, content)
		outbound, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
			TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
			DestinationStoreID: secondary.ID, RetireSource: true,
		})
		require.NoError(t, err)
		require.NoError(t, runner.Run(
			t.Context(), createPlacementOperation(t, metadata, outbound),
		))
		deletePlacementTestNode(t, metadata, file)

		plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
			TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
			DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
			Evacuate: true,
		})
		require.NoError(t, err)
		require.Len(t, plan.Hashes, 1)
		assert.Equal(t, hash, plan.Hashes[0].Hash)
		require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
		operationID := createStorageOperation(t, metadata, "evacuate", plan)
		require.NoError(t, runner.Run(t.Context(), operationID))

		resolution, err := metadata.ResolveBlobLocations(
			t.Context(), packstore.Hash(hash),
		)
		require.NoError(t, err)
		require.Len(t, resolution.Candidates, 1)
		assert.Equal(t, packstore.StoreID(metadata.PrimaryBlobStoreID()),
			resolution.Candidates[0].StoreID)
		stream, _, err := blobs.OpenStreamContext(t.Context(), hash)
		require.NoError(t, err)
		got, err := io.ReadAll(stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
		assert.Equal(t, content, got)
	})

	t.Run("corrupt primary never authorizes source retirement", func(t *testing.T) {
		metadata, blobs, runner, secondary := placementTestVault(t)
		file, hash := placementTestFile(t, metadata, blobs, []byte("healthy source"))
		outbound, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
			TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
			DestinationStoreID: secondary.ID,
		})
		require.NoError(t, err)
		require.NoError(t, runner.Run(
			t.Context(), createPlacementOperation(t, metadata, outbound),
		))
		deletePlacementTestNode(t, metadata, file)
		require.NoError(t, os.WriteFile(
			blobs.layout.LoosePath(packstore.Hash(hash)), []byte("damaged"), 0o600,
		))

		plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
			TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
			DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
			Evacuate: true,
		})
		require.NoError(t, err)
		require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
		operationID := createStorageOperation(t, metadata, "evacuate", plan)
		err = runner.Run(t.Context(), operationID)
		require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)

		resolution, err := metadata.ResolveBlobLocations(
			t.Context(), packstore.Hash(hash),
		)
		require.NoError(t, err)
		require.Len(t, resolution.Candidates, 2)
		assert.Equal(t, packstore.StoreID(secondary.ID), resolution.Candidates[1].StoreID)
	})
}

func TestPlacementRunnerHonorsCancellationAtEvacuationFinalization(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, _ := placementTestFile(t, metadata, blobs, []byte("keep secondary"))
	outbound, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID, RetireSource: true,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, outbound),
	))

	inbound, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
		Evacuate: true,
	})
	require.NoError(t, err)
	require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
	operationID := createStorageOperation(t, metadata, "evacuate", inbound)
	commits := 0
	runner.Commit = func(fn func() error) error {
		commits++
		if commits == 2 {
			require.NoError(t, metadata.RequestStorageOperationCancel(
				t.Context(), operationID,
			))
		}
		return fn()
	}

	require.NoError(t, runner.Run(t.Context(), operationID))
	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCancelled, operation.State)
	remaining, err := metadata.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "draining", remaining.Lifecycle)
}

func TestPlacementRunnerEvacuationAdoptsPendingSourceCleanup(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, _ := placementTestFile(t, metadata, blobs, []byte("cleanup handoff"))
	outbound, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createPlacementOperation(t, metadata, outbound),
	))

	retire, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
	})
	require.NoError(t, err)
	retireOperationID := createPlacementOperation(t, metadata, retire)
	cancelled, cancel := context.WithCancel(t.Context())
	interrupted := runner
	interrupted.Commit = func(fn func() error) error {
		err := fn()
		cancel()
		return err
	}
	require.ErrorIs(t, interrupted.Run(cancelled, retireOperationID), context.Canceled)
	cleanups, err := metadata.StorageOperationCleanups(
		t.Context(), retireOperationID,
	)
	require.NoError(t, err)
	require.Len(t, cleanups, 1)

	evacuation, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(),
		RetireSource:       true, Evacuate: true,
	})
	require.NoError(t, err)
	require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
	evacuationID := createStorageOperation(
		t, metadata, "evacuate", evacuation,
	)
	require.NoError(t, runner.Run(t.Context(), evacuationID))

	operation, err := metadata.StorageOperation(t.Context(), evacuationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
	evacuated, err := metadata.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", evacuated.Lifecycle)
	cleanups, err = metadata.StorageOperationCleanups(t.Context(), retireOperationID)
	require.NoError(t, err)
	assert.Empty(t, cleanups)
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

type corruptAfterRepairBackend struct {
	packstore.RepairBackend

	path string
}

func (b corruptAfterRepairBackend) RepairLoose(
	ctx context.Context,
	hash packstore.Hash,
	content io.Reader,
	options packstore.PublishOptions,
) (packstore.LooseReceipt, error) {
	receipt, err := b.RepairBackend.RepairLoose(ctx, hash, content, options)
	if err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf("repairing through test backend: %w", err)
	}
	if err := os.WriteFile(b.path, []byte("corrupted after publication"), 0o600); err != nil {
		return packstore.LooseReceipt{}, err
	}
	return receipt, nil
}

type cancelAfterRepairBackend struct {
	packstore.Backend

	repair packstore.RepairBackend
	cancel context.CancelFunc
}

func (b cancelAfterRepairBackend) RepairLoose(
	ctx context.Context,
	hash packstore.Hash,
	content io.Reader,
	options packstore.PublishOptions,
) (packstore.LooseReceipt, error) {
	receipt, err := b.repair.RepairLoose(ctx, hash, content, options)
	if err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf("repairing before cancellation: %w", err)
	}
	b.cancel()
	return receipt, nil
}

func TestRepairOneRejectsCorruptDestinationReadback(t *testing.T) {
	metadata, blobs, _, secondary := placementTestVault(t)
	_, hash := placementTestFile(t, metadata, blobs, []byte("verify repaired destination"))
	parsed := packstore.Hash(hash)
	sourceLocation, err := metadata.ResolveBlobLocations(t.Context(), parsed)
	require.NoError(t, err)
	require.NotEmpty(t, sourceLocation.Candidates)
	source, ok := blobs.ReadBackend(sourceLocation.Candidates[0].StoreID)
	require.True(t, ok)
	destination, ok := blobs.RepairBackend(packstore.StoreID(secondary.ID))
	require.True(t, ok)
	writable, ok := blobs.WritableBackend(packstore.StoreID(secondary.ID))
	require.True(t, ok)
	filesystem, ok := writable.(*packstore.FilesystemBackend)
	require.True(t, ok)

	_, err = repairOne(
		t.Context(), source,
		corruptAfterRepairBackend{
			RepairBackend: destination,
			path:          filesystem.Layout().LoosePath(parsed),
		},
		parsed, sourceLocation.Candidates[0], int64(len("verify repaired destination")),
	)
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
}

func TestPlacementRunnerRepairFallsBackToAnotherVerifiedSource(t *testing.T) {
	metadata, blobs, runner, secondaries, _ := placementTestVaultWithSecondaries(
		t, "archive", "mirror",
	)
	archive, mirror := secondaries[0], secondaries[1]
	content := []byte("fallback repair authority")
	file, hash := placementTestFile(t, metadata, blobs, content)
	for _, destination := range []store.BlobStore{archive, mirror} {
		plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
			TargetNodeID: file.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
			DestinationStoreID: destination.ID,
		})
		require.NoError(t, err)
		require.NoError(t, runner.Run(
			t.Context(), createPlacementOperation(t, metadata, plan),
		))
	}
	archiveBackend, ok := blobs.WritableBackend(packstore.StoreID(archive.ID))
	require.True(t, ok)
	archiveFilesystem, ok := archiveBackend.(*packstore.FilesystemBackend)
	require.True(t, ok)
	require.NoError(t, os.WriteFile(
		archiveFilesystem.Layout().LoosePath(packstore.Hash(hash)),
		[]byte("damaged archive"), 0o600,
	))
	require.NoError(t, os.WriteFile(
		blobs.layout.LoosePath(packstore.Hash(hash)), []byte("damaged primary"), 0o600,
	))

	plan, err := metadata.PlanStorageRecovery(
		t.Context(), "repair", hash, archive.ID,
	)
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createRecoveryOperation(t, metadata, plan),
	))

	location, err := runner.currentLocation(t.Context(), hash, archive.ID)
	require.NoError(t, err)
	stream, _, err := archiveBackend.OpenLoose(
		t.Context(), packstore.Hash(hash), *location.Loose,
	)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, content, got)
}

func TestPlacementRunnerSalvagesVerifiedBytesOverCorruptPrimary(t *testing.T) {
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
	require.NoError(t, os.WriteFile(
		blobs.layout.LoosePath(packstore.Hash(hash)), []byte("corrupt primary"), 0o600,
	))

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

func TestSalvageCancellationLeavesRecoveryResumable(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, hash := placementTestFile(t, metadata, blobs, []byte("cancel salvage repair"))
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
	taken.Epoch = "50000000-0000-4000-8000-000000000002"
	require.NoError(t, backend.ReplaceOwnership(t.Context(), taken, &prior))
	observation := blobs.RefreshStore(t.Context(), secondary.ID)
	require.Equal(t, StoreFenced, observation.State)
	require.NoError(t, os.WriteFile(
		blobs.layout.LoosePath(packstore.Hash(hash)), []byte("corrupt primary"), 0o600,
	))

	plan, err := metadata.PlanStorageRecovery(
		t.Context(), "salvage", hash, secondary.ID,
	)
	require.NoError(t, err)
	operationID := createRecoveryOperation(t, metadata, plan)
	cancelled, cancel := context.WithCancel(t.Context())
	originalReadBackend := blobs.readBackend
	primaryID := packstore.StoreID(metadata.PrimaryBlobStoreID())
	primary, ok := blobs.WritableBackend(primaryID)
	require.True(t, ok)
	repair, ok := blobs.RepairBackend(primaryID)
	require.True(t, ok)
	blobs.readBackend = nil
	blobs.registry.mu.Lock()
	blobs.registry.backends[primaryID] = cancelAfterRepairBackend{
		Backend: primary, repair: repair, cancel: cancel,
	}
	blobs.registry.mu.Unlock()
	t.Cleanup(func() {
		blobs.registry.mu.Lock()
		delete(blobs.registry.backends, primaryID)
		blobs.registry.mu.Unlock()
		blobs.readBackend = originalReadBackend
	})

	err = runner.Run(cancelled, operationID)
	require.ErrorIs(t, err, context.Canceled)
	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationRunning, operation.State)
	assert.Empty(t, operation.Error)
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

func TestPlacementRunnerCompletesRecoveryAfterPhysicalPublication(t *testing.T) {
	metadata, blobs, runner, secondary := placementTestVault(t)
	file, hash := placementTestFile(t, metadata, blobs, []byte("cancel recovery commit"))
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
	runner.Commit = func(fn func() error) error {
		require.ErrorIs(t, metadata.RequestStorageOperationCancel(
			t.Context(), operationID,
		), store.ErrStorageOperationTerminal)
		require.NoError(t, fn())
		current, err := metadata.StorageOperation(t.Context(), operationID)
		require.NoError(t, err)
		assert.Equal(t, store.StorageOperationCompleted, current.State)
		require.ErrorIs(t, metadata.RequestStorageOperationCancel(
			t.Context(), operationID,
		), store.ErrStorageOperationTerminal)
		return nil
	}

	require.NoError(t, runner.Run(t.Context(), operationID))
	operation, err := metadata.StorageOperation(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, operation.State)
}

func TestRemainingPlacementScratchSkipsCompletedObjects(t *testing.T) {
	plan := store.PlacementPlan{Hashes: []store.PlacementHash{{
		Hash:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ScratchBytes: 4096,
	}}}
	assert.Zero(t, remainingPlacementScratch(plan, 1))
}

func placementTestVault(
	t *testing.T,
) (*store.Store, *Store, PlacementRunner, store.BlobStore) {
	t.Helper()
	metadata, blobs, runner, secondaries, _ := placementTestVaultWithSecondaries(t, "archive")
	return metadata, blobs, runner, secondaries[0]
}

func placementTestVaultWithSecondaries(
	t *testing.T, names ...string,
) (*store.Store, *Store, PlacementRunner, []store.BlobStore, string) {
	t.Helper()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	bindings := make(map[string]config.StoreBindingConfig, len(names))
	specs := make([]StoreSpec, 0, len(names))
	secondaries := make([]store.BlobStore, 0, len(names))
	for index, name := range names {
		destination, err := metadata.PrepareSecondaryBlobStore(
			name, storeKindFilesystem, name,
		)
		require.NoError(t, err)
		destinationPath := filepath.Join(root, name)
		require.NoError(t, EnsureFilesystemNamespace(destinationPath))
		backend, err := NewFilesystemBackend(destinationPath, nil)
		require.NoError(t, err)
		require.NoError(t, backend.ReplaceOwnership(t.Context(), packstore.Ownership{
			Format: packstore.OwnershipFormatV1, Vault: metadata.VaultID(),
			Store: packstore.StoreID(destination.ID), Epoch: destination.OwnershipEpoch,
		}, nil))
		require.NoError(t, backend.Close())
		require.NoError(t, metadata.RegisterBlobStore(t.Context(), destination))
		bindings[name] = config.StoreBindingConfig{
			Kind: storeKindFilesystem, Path: destinationPath, Priority: 20 + index,
		}
		specs = append(specs, StoreSpec{
			ID: destination.ID, Kind: destination.Kind, Role: destination.Role,
			Lifecycle: destination.Lifecycle, Binding: destination.Binding,
			OwnershipEpoch: destination.OwnershipEpoch,
		})
		secondaries = append(secondaries, destination)
	}
	registry := NewRegistry(t.Context(), metadata.VaultID(), bindings, specs)
	options := Options{Registry: registry}
	blobs, err := NewWithOptions(
		store.NewPackCatalog(metadata), filepath.Join(root, "blobs"), options,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	return metadata, blobs, PlacementRunner{Metadata: metadata, Blobs: blobs}, secondaries, root
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

func deletePlacementTestNode(t *testing.T, metadata *store.Store, node store.Node) {
	t.Helper()
	_, _, err := metadata.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	result, err := metadata.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Deleted)
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
		SourceStoreID: func() string {
			if kind == "evacuate" {
				return plan.Request.SourceStoreID
			}
			return ""
		}(),
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
