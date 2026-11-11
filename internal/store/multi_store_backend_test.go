package store

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func multiStoreOperation(t *testing.T, s *Store, plan PlacementPlan, kind string) StorageOperation {
	t.Helper()
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	input := StorageOperationCreate{Kind: kind, RequestDigest: plan.Digest, RequestJSON: string(raw), PlanJSON: string(raw), TotalObjects: int64(len(plan.Hashes)), StoreReferences: []StorageOperationStoreReference{{StoreID: plan.Request.SourceStoreID, Role: storageOperationRoleSource}, {StoreID: plan.Request.DestinationStoreID, Role: storageOperationRoleDestination}}}
	if kind == storageOperationKindEvacuate {
		input.SourceStoreID = plan.Request.SourceStoreID
	}
	operation, err := s.CreateStorageOperation(t.Context(), input)
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(t.Context(), operation.ID)
	require.NoError(t, err)
	return operation
}

func checkMultiStorePhysicalRoundTripPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	catalog := NewPackCatalog(s)
	primaryLayout, err := packstore.NewLayout(t.TempDir(), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(t, err)
	maintainer, err := packstore.NewMaintainer(catalog, primaryLayout, packstore.MaintainerOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, maintainer.Close()) })
	content := []byte("synthetic document moved between stores")
	hash, err := packstore.ParseHash(fmt.Sprintf("%x", sha256.Sum256(content)))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(primaryLayout.LoosePath(hash)), 0700))
	require.NoError(t, os.WriteFile(primaryLayout.LoosePath(hash), content, 0600))
	node, err := s.CreateFile(ctx, s.RootID(), "move.txt", hash.String(), int64(len(content)), "text/plain")
	require.NoError(t, err)
	_, err = maintainer.Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive")
	require.NoError(t, err)
	secondaryLayout, err := packstore.NewLayout(t.TempDir(), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(t, err)
	ownership := packstore.Ownership{Format: packstore.OwnershipFormatV1, Vault: s.VaultID(), Store: packstore.StoreID(secondary.ID), Epoch: secondary.OwnershipEpoch}
	target, err := packstore.NewFilesystemBackend(secondaryLayout, packstore.FilesystemBackendOptions{ExpectedOwnership: &ownership})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ReplaceOwnership(ctx, ownership, nil))
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	primaryOwnership := catalog.PrimaryOwnership()
	primary, err := packstore.NewFilesystemBackend(primaryLayout, packstore.FilesystemBackendOptions{ExpectedOwnership: &primaryOwnership})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, primary.Close()) })
	require.NoError(t, primary.ReplaceOwnership(ctx, primaryOwnership, nil))
	plan, err := s.PlanPlacement(ctx, PlacementRequest{TargetNodeID: node.ID, SourceStoreID: s.primaryStoreID, DestinationStoreID: secondary.ID, RetireSource: true})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 1)
	require.NotNil(t, plan.Hashes[0].Source.Pack)
	operation := multiStoreOperation(t, s, plan, storageOperationKindPlace)
	moved, err := packstore.Move(ctx, primary, target, packstore.MoveRequest{Source: plan.Hashes[0].Source, Destination: packstore.StoreID(secondary.ID), Identity: packstore.BlobIdentity{Hash: hash, Size: int64(len(content))}})
	require.NoError(t, err)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "REVOKE UPDATE(live_entries,live_stored_bytes,live_raw_bytes,max_live_stored_len,max_live_raw_len) ON documents.blob_packs FROM "+role)
	require.NoError(t, err)
	_, err = s.CommitPlacement(ctx, operation.ID, plan.Request, plan.Hashes[0], moved.Destination)
	require.ErrorContains(t, err, "permission denied")
	locations, err := s.ResolveBlobLocations(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, []packstore.ReadLocation{plan.Hashes[0].Source}, locations.Candidates)
	usage, err := catalog.ListPackUsage(ctx)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	require.EqualValues(t, 1, usage[0].LiveEntries)
	_, err = setup.ExecContext(ctx, "GRANT UPDATE(live_entries,live_stored_bytes,live_raw_bytes,max_live_stored_len,max_live_raw_len) ON documents.blob_packs TO "+role)
	require.NoError(t, err)
	committed, err := s.CommitPlacement(ctx, operation.ID, plan.Request, plan.Hashes[0], moved.Destination)
	require.NoError(t, err)
	require.True(t, committed.SourceRevoked)
	usage, err = catalog.ListPackUsage(ctx)
	require.NoError(t, err)
	require.Zero(t, usage[0].LiveEntries)
	require.Zero(t, usage[0].LiveRawBytes)
	locations, err = s.ResolveBlobLocations(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, []packstore.ReadLocation{moved.Destination}, locations.Candidates)
	stream, _, err := target.OpenLoose(ctx, hash, *locations.Candidates[0].Loose)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
	require.Equal(t, content, got)
	require.NoError(t, s.FinishStorageOperation(ctx, operation.ID, StorageOperationCompleted, `{"copied":true}`, "", time.Now().Add(24*time.Hour)))
	inbound, err := s.PlanPlacement(ctx, PlacementRequest{TargetNodeID: s.RootID(), SourceStoreID: secondary.ID, DestinationStoreID: s.primaryStoreID, RetireSource: true, Evacuate: true})
	require.NoError(t, err)
	require.NoError(t, s.BeginBlobStoreEvacuation(ctx, secondary.ID))
	evacuation := multiStoreOperation(t, s, inbound, storageOperationKindEvacuate)
	returned, err := packstore.Move(ctx, target, primary, packstore.MoveRequest{Source: inbound.Hashes[0].Source, Destination: packstore.StoreID(s.primaryStoreID), Identity: packstore.BlobIdentity{Hash: hash, Size: int64(len(content))}})
	require.NoError(t, err)
	_, err = s.CommitPlacement(ctx, evacuation.ID, inbound.Request, inbound.Hashes[0], returned.Destination)
	require.NoError(t, err)
	final, err := s.FinalizeBlobStoreEvacuation(ctx, evacuation.ID, secondary.ID, s.primaryStoreID)
	require.NoError(t, err)
	require.False(t, final.Detached)
	pending, err := s.StorageOperationCleanups(ctx, evacuation.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	safe, err := s.PrepareStorageOperationCleanup(ctx, evacuation.ID, pending[0])
	require.NoError(t, err)
	require.True(t, safe)
	require.NoError(t, target.Retire(ctx, pending[0].Ref))
	require.NoError(t, s.CompleteStorageOperationCleanup(ctx, evacuation.ID, pending[0]))
	final, err = s.FinalizeBlobStoreEvacuation(ctx, evacuation.ID, secondary.ID, s.primaryStoreID)
	require.NoError(t, err)
	require.True(t, final.Detached)
	require.NoError(t, s.FinishStorageOperation(ctx, evacuation.ID, StorageOperationCompleted, `{"evacuated":true}`, "", time.Now().Add(24*time.Hour)))
	require.NoError(t, s.UnregisterBlobStore(ctx, secondary.ID))
	got, _, err = maintainer.Store().ReadBounded(ctx, hash, 1024)
	require.NoError(t, err)
	require.Equal(t, content, got)
	checkRetentionMetadata(t, s)
}

func checkMultiStoreReadSnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"placement", "repair", "salvage"} {
		t.Run(operation, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), "snapshot.txt", fakeHash("f2"), 12, "text/plain")
			require.NoError(t, err)
			secondary, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive")
			require.NoError(t, err)
			require.NoError(t, s.RegisterBlobStore(ctx, secondary))
			request := PlacementRequest{TargetNodeID: node.ID, SourceStoreID: s.primaryStoreID, DestinationStoreID: secondary.ID}
			before, err := s.PlanPlacement(ctx, request)
			require.NoError(t, err)
			destination := before.Hashes[0].Source
			destination.StoreID = packstore.StoreID(secondary.ID)
			destination.Generation = "50000000-0000-4000-8000-000000000001"
			op := multiStoreOperation(t, s, before, storageOperationKindPlace)
			var mutationErr error
			if operation == "placement" {
				reader := sealedDeliveryReaderAfterSelect(t, s, func() { _, mutationErr = s.CommitPlacement(ctx, op.ID, request, before.Hashes[0], destination) })
				got, err := reader.PlanPlacement(ctx, request)
				require.NoError(t, err)
				require.Equal(t, before, got)
				next, err := s.PlanPlacement(ctx, request)
				require.NoError(t, err)
				require.NotEqual(t, before.Digest, next.Digest)
				require.NotNil(t, next.Hashes[0].Destination)
			} else {
				_, err = s.CommitPlacement(ctx, op.ID, request, before.Hashes[0], destination)
				require.NoError(t, err)
				original, err := s.PlanStorageRecovery(ctx, operation, node.BlobHash, secondary.ID)
				require.NoError(t, err)
				raw, err := json.Marshal(original)
				require.NoError(t, err)
				recovery, err := s.CreateStorageOperation(ctx, StorageOperationCreate{Kind: operation, RequestDigest: original.Digest, RequestJSON: string(raw), PlanJSON: string(raw), TotalObjects: 1})
				require.NoError(t, err)
				_, err = s.ClaimStorageOperation(ctx, recovery.ID)
				require.NoError(t, err)
				require.NoError(t, s.BeginStorageRecoveryPublication(ctx, recovery.ID, original))
				replacement := destination
				replacement.StoreID = packstore.StoreID(original.Destination)
				replacement.Generation = "50000000-0000-4000-8000-000000000002"
				reader := sealedDeliveryReaderAfterSelect(t, s, func() {
					mutationErr = s.CommitStorageRecovery(ctx, recovery.ID, original, replacement, `{"repaired":true}`, time.Now().Add(24*time.Hour))
				})
				got, err := reader.PlanStorageRecovery(ctx, operation, node.BlobHash, secondary.ID)
				require.NoError(t, err)
				require.Equal(t, original, got)
				next, err := s.PlanStorageRecovery(ctx, operation, node.BlobHash, secondary.ID)
				require.NoError(t, err)
				require.Equal(t, &replacement, next.Prior)
				require.NotEqual(t, original.Digest, next.Digest)
			}
			require.NoError(t, mutationErr)
		})
	}
}

func checkStorageRepairRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "repair.txt", fakeHash("f3"), 12, "text/plain")
	require.NoError(t, err)
	catalog := NewPackCatalog(s)
	hash := packstore.Hash(node.BlobHash)
	packID := pack.NewPackID()
	require.NoError(t, catalog.RecordPack(ctx, packstore.PackRecord{PackID: packID, EntryCount: 1, StoredBytes: 19, CreatedAt: time.Now()}, []packstore.Adoption{{Entry: packstore.IndexEntry{Hash: hash, PackID: packID, Offset: pack.MinEntryOffset, StoredLen: 19, RawLen: 12}}}))
	secondary, err := s.PrepareSecondaryBlobStore("backup", "filesystem", "backup")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	copyPlan, err := s.PlanPlacement(ctx, PlacementRequest{TargetNodeID: node.ID, SourceStoreID: s.primaryStoreID, DestinationStoreID: secondary.ID})
	require.NoError(t, err)
	copyOp := multiStoreOperation(t, s, copyPlan, storageOperationKindPlace)
	backup := packstore.ReadLocation{StoreID: packstore.StoreID(secondary.ID), Generation: "50000000-0000-4000-8000-000000000003", Loose: &packstore.LooseLocation{Encoding: packstore.LooseEncodingRaw, LogicalSize: 12, StoredSize: 12}}
	_, err = s.CommitPlacement(ctx, copyOp.ID, copyPlan.Request, copyPlan.Hashes[0], backup)
	require.NoError(t, err)
	plan, err := s.PlanStorageRecovery(ctx, "repair", hash.String(), s.primaryStoreID)
	require.NoError(t, err)
	require.NotNil(t, plan.Prior.Pack)
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	op, err := s.CreateStorageOperation(ctx, StorageOperationCreate{Kind: "repair", RequestDigest: plan.Digest, RequestJSON: string(raw), PlanJSON: string(raw), TotalObjects: 1})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, op.ID)
	require.NoError(t, err)
	require.NoError(t, s.BeginStorageRecoveryPublication(ctx, op.ID, plan))
	before, err := s.ResolveBlobLocations(ctx, hash)
	require.NoError(t, err)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "REVOKE UPDATE(receipt_json) ON documents.storage_operations FROM "+role)
	require.NoError(t, err)
	repaired := backup
	repaired.StoreID = packstore.StoreID(s.primaryStoreID)
	repaired.Generation = "50000000-0000-4000-8000-000000000004"
	deadline := time.Now().Add(24 * time.Hour)
	require.ErrorContains(t, s.CommitStorageRecovery(ctx, op.ID, plan, repaired, `{"repaired":true}`, deadline), "permission denied")
	after, err := s.ResolveBlobLocations(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, before, after)
	usage, err := catalog.ListPackUsage(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, usage[0].LiveEntries)
	stillRunning, err := s.StorageOperation(ctx, op.ID)
	require.NoError(t, err)
	require.Equal(t, StorageOperationRunning, stillRunning.State)
	_, err = setup.ExecContext(ctx, "GRANT UPDATE(receipt_json) ON documents.storage_operations TO "+role)
	require.NoError(t, err)
	require.NoError(t, s.CommitStorageRecovery(ctx, op.ID, plan, repaired, `{"repaired":true}`, deadline))
	finished, err := s.StorageOperation(ctx, op.ID)
	require.NoError(t, err)
	require.Equal(t, StorageOperationCompleted, finished.State)
	require.Equal(t, `{"repaired":true}`, finished.ReceiptJSON)
	usage, err = catalog.ListPackUsage(ctx)
	require.NoError(t, err)
	require.Zero(t, usage[0].LiveEntries)
	checkRetentionMetadata(t, s)
}
