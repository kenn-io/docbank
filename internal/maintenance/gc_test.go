package maintenance

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

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

func TestExactDerivativeErasureRejectsSecondaryPackedLocations(t *testing.T) {
	err := validateUnreachablePackLocations("primary", packstore.Resolution{
		Member: true,
		Candidates: []packstore.ReadLocation{
			{StoreID: "primary", Pack: &packstore.IndexEntry{}},
			{StoreID: "secondary", Pack: &packstore.IndexEntry{}},
		},
	})
	require.ErrorIs(t, err, packstore.ErrStoreUnavailable)
	require.ErrorContains(t, err, "secondary")
}

func TestDerivativePackSelectionDetectsConcurrentRetirement(t *testing.T) {
	selected := []packstore.PackUsage{{
		PackID: "pack-a", EntryCount: 3,
		LiveEntries: 2,
	}}
	assert.False(t, derivativePackSelectionChanged(selected, append([]packstore.PackUsage(nil), selected...)))
	changed := append([]packstore.PackUsage(nil), selected...)
	changed[0].LiveEntries = 0
	assert.True(t, derivativePackSelectionChanged(selected, changed))
	assert.True(t, derivativePackSelectionChanged(selected, nil))
}

func TestPurgeDerivativesRetiresPackedDerivativeBytes(t *testing.T) {
	// Mutation caught: catalog GC alone marks a packed derivative dead while its
	// sensitive bytes remain recoverable from a dense live immutable pack file.
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobRoot := filepath.Join(root, "blobs")
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	var originals []blob.WriteReceipt
	for index, payload := range []string{
		"first retained synthetic original",
		"second retained synthetic original",
	} {
		receipt, writeErr := blobs.WriteDetailedContext(
			t.Context(), bytes.NewReader([]byte(payload)))
		require.NoError(t, writeErr)
		encoding, encodingErr := receipt.EncodingName()
		require.NoError(t, encodingErr)
		_, createErr := metadata.CreateFile(t.Context(), metadata.RootID(),
			fmt.Sprintf("original-%d.txt", index), receipt.Hash, receipt.Size, "text/plain",
			store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize,
				PackEligible: receipt.PackEligible, Created: receipt.Created})
		require.NoError(t, createErr)
		originals = append(originals, receipt)
	}
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
	assert.Equal(t, 1, report.Repack.PacksRewritten)
	assert.Equal(t, 1, report.Repack.PacksRemoved)
	assert.NoFileExists(t, oldPackPath)
	records, err = store.NewPackCatalog(metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, int64(2), records[0].EntryCount)
	_, err = blobs.Open(written.Hash)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, original := range originals {
		reader, openErr := blobs.Open(original.Hash)
		require.NoError(t, openErr)
		require.NoError(t, reader.Close())
	}
	assert.True(t, report.Purge.ImmutableBackupCopiesUntouched)
}

func TestPurgeDerivativesRetriesDurablePackedErasureAfterInterruption(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "metadata.db")
	blobRoot := filepath.Join(root, "blobs")
	metadata, err := store.Open(databasePath)
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)

	original, err := blobs.WriteDetailedContext(
		t.Context(), bytes.NewReader([]byte("retained synthetic original")))
	require.NoError(t, err)
	originalEncoding, err := original.EncodingName()
	require.NoError(t, err)
	_, err = metadata.CreateFile(t.Context(), metadata.RootID(), "original.txt",
		original.Hash, original.Size, "text/plain", store.BlobPhysical{
			Encoding: originalEncoding, StoredBytes: original.StoredSize,
			PackEligible: original.PackEligible, Created: original.Created,
		})
	require.NoError(t, err)
	derivative, err := blobs.WriteDetailedContext(
		t.Context(), bytes.NewReader([]byte("purged synthetic derivative")))
	require.NoError(t, err)
	derivativeEncoding, err := derivative.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(t.Context(), derivative.Hash, derivative.Size,
		store.BlobPhysical{Encoding: derivativeEncoding, StoredBytes: derivative.StoredSize,
			PackEligible: derivative.PackEligible, Created: derivative.Created}))
	packed, err := blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.PacksSealed)
	records, err := store.NewPackCatalog(metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	oldPackPath := filepath.Join(blobRoot, "packs", records[0].PackID[:2],
		records[0].PackID+packstore.PackExt)

	logical, err := metadata.PurgeDerivatives(t.Context(), store.PurgeRequest{})
	require.NoError(t, err)
	ordinaryCandidates, err := metadata.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ordinaryCandidates,
		"the unpaged ordinary GC inventory must also preserve exact purge receipts")
	ordinary, err := GarbageCollect(t.Context(), metadata, blobs, GCOptions{})
	require.NoError(t, err)
	assert.Zero(t, ordinary.RemovedBlobs,
		"ordinary GC must leave exact derivative erasure receipts for the purge retry")
	hasDerivative, err := metadata.HasBlob(t.Context(), derivative.Hash)
	require.NoError(t, err)
	assert.True(t, hasDerivative)
	physical, err := collectExactUnreachableBlobs(
		t.Context(), metadata, blobs, logical.PhysicalDerivativeBlobsPendingGC)
	require.NoError(t, err)
	require.Equal(t, 1, physical.PendingPackedBlobs)
	require.FileExists(t, oldPackPath,
		"the interruption occurs after durable logical deletion but before pack rewrite")
	pending, err := metadata.PendingDerivativePackRetirements(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, blobs.Close())
	require.NoError(t, metadata.Close())

	metadata, err = store.Open(databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobs, err = blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	report, err := PurgeDerivatives(t.Context(), metadata, blobs, store.PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Repack.PacksRewritten)
	assert.NoFileExists(t, oldPackPath)
	pending, err = metadata.PendingDerivativePackRetirements(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestPurgeDerivativesRetriesDurableLooseErasureAfterInterruption(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "metadata.db")
	blobRoot := filepath.Join(root, "blobs")
	metadata, err := store.Open(databasePath)
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)

	derivative, err := blobs.WriteDetailedContext(
		t.Context(), bytes.NewReader([]byte("purged synthetic loose derivative")))
	require.NoError(t, err)
	derivativeEncoding, err := derivative.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(t.Context(), derivative.Hash, derivative.Size,
		store.BlobPhysical{Encoding: derivativeEncoding, StoredBytes: derivative.StoredSize,
			PackEligible: derivative.PackEligible, Created: derivative.Created}))
	path := filepath.Join(blobRoot, derivative.Hash[:2], derivative.Hash)
	require.FileExists(t, path)

	logical, err := metadata.PurgeDerivatives(t.Context(), store.PurgeRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{derivative.Hash}, logical.PhysicalDerivativeBlobsPendingGC)
	parsed, err := packstore.ParseHash(derivative.Hash)
	require.NoError(t, err)
	require.NoError(t, metadata.DeleteBlobRowsWithGCRetirements(
		t.Context(), logical.PhysicalDerivativeBlobsPendingGC,
		[]store.GCLooseRetirement{{
			StoreID: metadata.PrimaryBlobStoreID(), Hash: parsed,
			Encoding: derivative.Encoding,
		}}, nil,
	))
	require.FileExists(t, path,
		"the interruption occurs after durable logical deletion but before loose-file removal")
	pending, err := metadata.PendingGCLooseRetirements(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, blobs.Close())
	require.NoError(t, metadata.Close())

	metadata, err = store.Open(databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobs, err = blob.New(store.NewPackCatalog(metadata), blobRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	report, err := PurgeDerivatives(t.Context(), metadata, blobs, store.PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Physical.ReclaimedFiles)
	assert.NoFileExists(t, path)
	pending, err = metadata.PendingGCLooseRetirements(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
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
