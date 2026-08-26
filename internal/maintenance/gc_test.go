package maintenance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestPurgeDerivativesWaitsForConcurrentPublication(t *testing.T) {
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobRoot := filepath.Join(root, "blobs")
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	ctx := t.Context()
	written, err := blobs.WriteDetailedContext(
		ctx, bytes.NewReader([]byte("concurrently published synthetic derivative")))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(ctx, written.Hash, written.Size,
		store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize,
			PackEligible: written.PackEligible, Created: written.Created}))

	mutationAcquired := make(chan struct{})
	publish := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- blobs.WithMutation(ctx, func() error {
			close(mutationAcquired)
			<-publish
			_, createErr := metadata.CreateFile(ctx, metadata.RootID(), "published.txt",
				written.Hash, written.Size, "text/plain")
			return createErr
		})
	}()
	<-mutationAcquired

	purgeDone := make(chan error, 1)
	go func() {
		_, purgeErr := PurgeDerivatives(ctx, metadata, blobs, store.PurgeRequest{})
		purgeDone <- purgeErr
	}()
	maintenanceQueued := false
	deadline := time.After(time.Second)
	for !maintenanceQueued {
		select {
		case purgeErr := <-purgeDone:
			close(publish)
			<-mutationDone
			require.NoError(t, purgeErr)
			t.Fatal("derivative purge did not wait behind the active publication")
		case <-deadline:
			close(publish)
			<-mutationDone
			t.Fatal("derivative purge did not queue a maintenance lease")
		default:
		}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		probeErr := blobs.WithMutation(probeCtx, func() error { return nil })
		cancel()
		maintenanceQueued = errors.Is(probeErr, context.DeadlineExceeded)
	}
	close(publish)
	require.NoError(t, <-mutationDone)
	require.NoError(t, <-purgeDone)

	recorded, err := metadata.HasBlob(ctx, written.Hash)
	require.NoError(t, err)
	assert.True(t, recorded)
	reader, err := blobs.Open(written.Hash)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
}

func TestRetireLooseCandidatesContinuesAfterMissingLocation(t *testing.T) {
	locations := []packstore.ReadLocation{
		{
			StoreID: "missing", Loose: &packstore.LooseLocation{
				Encoding: packstore.LooseEncodingRaw,
			},
		},
		{
			StoreID: "present", Loose: &packstore.LooseLocation{
				Encoding: packstore.LooseEncodingZstd,
			},
		},
	}
	var retired []packstore.StoreID
	count, err := retireLooseCandidates(locations, func(location packstore.ReadLocation) error {
		retired = append(retired, location.StoreID)
		if location.StoreID == "missing" {
			return fs.ErrNotExist
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, []packstore.StoreID{"missing", "present"}, retired)
}

func TestPurgeDerivativesRetiresPackedDerivativeBytes(t *testing.T) {
	// Mutation caught: catalog GC alone marks a packed derivative dead while its
	// sensitive bytes remain recoverable from the live immutable pack file.
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobRoot := filepath.Join(root, "blobs")
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	written, err := blobs.WriteDetailedContext(
		t.Context(), bytes.NewReader([]byte("packed synthetic sensitive derivative payload")))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(t.Context(), written.Hash, written.Size,
		store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize,
			PackEligible: written.PackEligible, Created: written.Created}))
	packed, err := blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.PacksSealed)
	records, err := store.NewPackCatalog(metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	oldPackPath := filepath.Join(blobRoot, "packs", records[0].PackID[:2],
		records[0].PackID+packstore.PackExt)
	require.FileExists(t, oldPackPath)

	report, err := PurgeDerivatives(t.Context(), metadata, blobs, store.PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Physical.PendingPackedBlobs)
	assert.Equal(t, 1, report.Repack.PacksRemoved)
	assert.NoFileExists(t, oldPackPath)
	records, err = store.NewPackCatalog(metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	assert.Empty(t, records)
	_, err = blobs.Open(written.Hash)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.True(t, report.Purge.ImmutableBackupCopiesUntouched)
}

func TestPurgeDerivativesRetiresSecondaryOnlySharedPack(t *testing.T) {
	metadata, blobs, secondary, root := derivativePurgeVaultWithSecondary(t)
	ctx := t.Context()
	targetPayload := []byte("secondary packed synthetic sensitive derivative")
	target, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(targetPayload))
	require.NoError(t, err)
	targetEncoding, err := target.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(ctx, target.Hash, target.Size,
		store.BlobPhysical{Encoding: targetEncoding, StoredBytes: target.StoredSize,
			PackEligible: target.PackEligible, Created: target.Created}))
	livePayload := []byte("ordinary live neighbor in a shared secondary pack")
	live, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(livePayload))
	require.NoError(t, err)
	liveEncoding, err := live.EncodingName()
	require.NoError(t, err)
	_, err = metadata.CreateFile(ctx, metadata.RootID(), "live.txt", live.Hash, live.Size,
		"text/plain", store.BlobPhysical{Encoding: liveEncoding, StoredBytes: live.StoredSize,
			PackEligible: live.PackEligible, Created: live.Created})
	require.NoError(t, err)
	packed, err := blobs.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, packed.BlobsPacked)

	catalog := store.NewPackCatalog(metadata)
	records, err := catalog.ListPackRecords(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
	entries, err := catalog.ListPackEntries(ctx, records[0].PackID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	secondaryBackend, ok := blobs.WritableBackend(packstore.StoreID(secondary.ID))
	require.True(t, ok)
	primaryPackPath := filepath.Join(root, "blobs", "packs", records[0].PackID[:2],
		records[0].PackID+packstore.PackExt)
	source, err := os.Open(primaryPackPath)
	require.NoError(t, err)
	packInfo, err := source.Stat()
	require.NoError(t, err)
	published, publishErr := secondaryBackend.PublishPack(ctx, records[0].PackID, source,
		packstore.PublishOptions{ExpectedSize: packInfo.Size(), SizeKnown: true,
			Durability: packstore.DurablePublication})
	require.NoError(t, publishErr)
	require.NoError(t, source.Close())

	db, err := metadata.SQLiteDriver().Open(filepath.Join(root, "metadata.db"),
		docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting,
			TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(ctx, `INSERT INTO blob_packs(
		store_id,pack_id,entry_count,stored_bytes,created_at) VALUES(?,?,?,?,?)`,
		secondary.ID, records[0].PackID, records[0].EntryCount, records[0].StoredBytes,
		records[0].CreatedAt.UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)
	for _, entry := range entries {
		_, err = db.ExecContext(ctx, `INSERT INTO blob_pack_entries(
			blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c
		) VALUES(?,?,?,?,?,?,?,?)`, entry.Hash.String(), secondary.ID, entry.PackID,
			entry.Offset, entry.StoredLen, entry.RawLen, entry.Flags, entry.CRC32C)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'packed',NULL,?,1)`, entry.Hash.String(), secondary.ID,
			published.Generation, entry.StoredLen)
		require.NoError(t, err)
	}
	primaryBackend, ok := blobs.WritableBackend(packstore.StoreID(metadata.PrimaryBlobStoreID()))
	require.True(t, ok)
	require.NoError(t, primaryBackend.Retire(ctx, packstore.ObjectRef{PackID: records[0].PackID}))
	require.NoError(t, catalog.DeletePackRecord(ctx, records[0].PackID))
	secondaryPackPath := filepath.Join(root, "archive", "packs", records[0].PackID[:2],
		records[0].PackID+packstore.PackExt)
	require.FileExists(t, secondaryPackPath)

	report, err := PurgeDerivatives(ctx, metadata, blobs, store.PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Repack.PacksSelected)
	assert.Equal(t, 1, report.Repack.PacksRewritten)
	assert.Equal(t, 1, report.Repack.PacksRemoved)
	assert.NoFileExists(t, secondaryPackPath)
	reader, err := blobs.Open(live.Hash)
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, livePayload, got)
	_, err = blobs.Open(target.Hash)
	require.ErrorIs(t, err, os.ErrNotExist)
	var pending int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM derivative_pack_purge_pending`).Scan(&pending))
	assert.Zero(t, pending)
}

func TestPurgeDerivativesLeavesUnrelatedDeadPackUntouched(t *testing.T) {
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobRoot := filepath.Join(root, "blobs")
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	ctx := t.Context()
	target, err := blobs.WriteDetailedContext(ctx, bytes.NewReader([]byte("target derivative pack")))
	require.NoError(t, err)
	targetEncoding, err := target.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(ctx, target.Hash, target.Size,
		store.BlobPhysical{Encoding: targetEncoding, StoredBytes: target.StoredSize,
			PackEligible: target.PackEligible, Created: target.Created}))
	_, err = blobs.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)

	unrelated, err := blobs.WriteDetailedContext(ctx, bytes.NewReader([]byte("unrelated dead packed bytes")))
	require.NoError(t, err)
	unrelatedEncoding, err := unrelated.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(ctx, unrelated.Hash, unrelated.Size,
		store.BlobPhysical{Encoding: unrelatedEncoding, StoredBytes: unrelated.StoredSize,
			PackEligible: unrelated.PackEligible, Created: unrelated.Created}))
	_, err = blobs.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	catalog := store.NewPackCatalog(metadata)
	records, err := catalog.ListPackRecords(ctx)
	require.NoError(t, err)
	require.Len(t, records, 2)
	var unrelatedPackID string
	for _, record := range records {
		entries, entriesErr := catalog.ListPackEntries(ctx, record.PackID)
		require.NoError(t, entriesErr)
		if len(entries) == 1 && entries[0].Hash.String() == unrelated.Hash {
			unrelatedPackID = record.PackID
		}
	}
	require.NotEmpty(t, unrelatedPackID)
	unrelatedPackPath := filepath.Join(blobRoot, "packs", unrelatedPackID[:2],
		unrelatedPackID+packstore.PackExt)
	require.FileExists(t, unrelatedPackPath)
	require.NoError(t, metadata.DeleteBlobRows(ctx, []string{unrelated.Hash}))

	report, err := PurgeDerivatives(ctx, metadata, blobs, store.PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Repack.PacksSelected)
	assert.Equal(t, 1, report.Repack.PacksRemoved)
	assert.FileExists(t, unrelatedPackPath,
		"derivative purge must not run a vault-wide repack over unrelated packs")
	records, err = catalog.ListPackRecords(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, unrelatedPackID, records[0].PackID)
}

func derivativePurgeVaultWithSecondary(
	t *testing.T,
) (*store.Store, *blob.Store, store.BlobStore, string) {
	t.Helper()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	secondary, err := metadata.PrepareSecondaryBlobStore("archive", "filesystem", "archive")
	require.NoError(t, err)
	secondaryRoot := filepath.Join(root, "archive")
	require.NoError(t, blob.EnsureFilesystemNamespace(secondaryRoot))
	unattached, err := blob.NewFilesystemBackend(secondaryRoot, nil)
	require.NoError(t, err)
	require.NoError(t, unattached.ReplaceOwnership(t.Context(), packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: metadata.VaultID(),
		Store: packstore.StoreID(secondary.ID), Epoch: secondary.OwnershipEpoch,
	}, nil))
	require.NoError(t, unattached.Close())
	require.NoError(t, metadata.RegisterBlobStore(t.Context(), secondary))
	registry := blob.NewRegistry(t.Context(), metadata.VaultID(),
		map[string]config.StoreBindingConfig{"archive": {
			Kind: "filesystem", Path: secondaryRoot, Priority: 20,
		}}, []blob.StoreSpec{{
			ID: secondary.ID, Kind: secondary.Kind, Role: secondary.Role,
			Lifecycle: secondary.Lifecycle, Binding: secondary.Binding,
			OwnershipEpoch: secondary.OwnershipEpoch,
		}})
	blobs, err := blob.NewWithOptions(store.NewPackCatalog(metadata), filepath.Join(root, "blobs"),
		blob.Options{Registry: registry})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	return metadata, blobs, secondary, root
}

func TestPurgeDerivativesRunsLocationAwarePhysicalGC(t *testing.T) {
	// Mutation caught: stopping after catalog-manifest deletion leaves sensitive
	// loose provider output in the live vault instead of completing physical GC.
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobRoot := filepath.Join(root, "blobs")
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	original, err := blobs.WriteDetailedContext(
		t.Context(), bytes.NewReader([]byte("ordinary pruned original in regret window")))
	require.NoError(t, err)
	node, err := metadata.CreateFile(t.Context(), metadata.RootID(), "ordinary.txt",
		original.Hash, original.Size, "text/plain")
	require.NoError(t, err)
	replacement, err := blobs.WriteDetailedContext(
		t.Context(), bytes.NewReader([]byte("current ordinary replacement")))
	require.NoError(t, err)
	node, _, err = metadata.ReplaceContent(t.Context(), node.ID, node.Revision,
		replacement.Hash, replacement.Size, "text/plain")
	require.NoError(t, err)
	_, err = metadata.PruneContentVersions(t.Context(), node.ID, node.Revision,
		store.VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	originalPath := filepath.Join(blobRoot, original.Hash[:2], original.Hash)
	require.FileExists(t, originalPath)
	written, err := blobs.WriteDetailedContext(
		t.Context(), bytes.NewReader([]byte("abandoned synthetic provider output")),
	)
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(t.Context(), written.Hash, written.Size,
		store.BlobPhysical{
			Encoding: encoding, StoredBytes: written.StoredSize,
			PackEligible: written.PackEligible, Created: written.Created,
		}))
	path := filepath.Join(blobRoot, written.Hash[:2], written.Hash)
	require.FileExists(t, path)
	logical, err := metadata.PurgeDerivatives(t.Context(), store.PurgeRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{written.Hash}, logical.PhysicalDerivativeBlobsPendingGC,
		"the logical transaction durably records its physical erasure target")

	report, err := PurgeDerivatives(t.Context(), metadata, blobs, store.PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Physical.CandidateBlobs)
	assert.Equal(t, 1, report.Physical.ReclaimedFiles)
	assert.Equal(t, 1, report.Physical.RemovedBlobs)
	assert.True(t, report.Purge.ImmutableBackupCopiesUntouched)
	assert.NoFileExists(t, path)
	recorded, err := metadata.HasBlob(t.Context(), written.Hash)
	require.NoError(t, err)
	assert.False(t, recorded)
	assert.FileExists(t, originalPath,
		"derivative purge must not run vault-wide GC over ordinary regret-window blobs")
	recorded, err = metadata.HasBlob(t.Context(), original.Hash)
	require.NoError(t, err)
	assert.True(t, recorded)
}
