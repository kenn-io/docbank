package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestFreshBlobCatalogHasOneFixedPrimary(t *testing.T) {
	s := newTestStore(t)
	primary, err := s.PrimaryBlobStore(t.Context())
	require.NoError(t, err)
	assert.Equal(t, s.PrimaryBlobStoreID(), primary.ID)
	assert.Equal(t, BlobStore{
		ID: primary.ID, Name: primaryBlobStoreName, Kind: blobStoreKindFilesystem,
		Role: blobStoreRolePrimary, Lifecycle: blobStoreLifecycleActive,
		Binding: primaryBlobStoreBinding, OwnershipEpoch: primary.OwnershipEpoch,
		CreatedAt: primary.CreatedAt,
	}, primary)

	blobColumns, err := tableColumns(s.db, "blobs")
	require.NoError(t, err)
	assert.Equal(t, []string{"created_at", "hash", "size"}, blobColumns)

	_, err = s.db.Exec(`
		INSERT INTO blob_stores(
			store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at
		) VALUES(?, 'other-primary', 'filesystem', 'primary', 'active', 'other',
		         ?, ?)`,
		"40000000-0000-4000-8000-000000000001",
		"40000000-0000-4000-8000-000000000002", nowRFC3339(),
	)
	require.Error(t, err)
}

func TestResolveBlobLocationsUsesStoreScopedAuthority(t *testing.T) {
	s := newTestStore(t)
	hash, err := packstore.ParseHash(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(
		t.Context(), s.RootID(), "report.txt", hash.String(), 20, "text/plain",
		BlobPhysical{Encoding: looseEncodingZstd, StoredBytes: 9, PackEligible: true},
	)
	require.NoError(t, err)

	resolution, err := s.ResolveBlobLocations(t.Context(), hash)
	require.NoError(t, err)
	require.True(t, resolution.Member)
	require.Len(t, resolution.Candidates, 1)
	assert.Equal(t, packstore.StoreID(s.primaryStoreID), resolution.Candidates[0].StoreID)
	require.NotNil(t, resolution.Candidates[0].Loose)
	assert.Equal(t, packstore.LooseEncodingZstd, resolution.Candidates[0].Loose.Encoding)

	packID := pack.NewPackID()
	require.NoError(t, NewPackCatalog(s).RecordPack(
		t.Context(),
		packstore.PackRecord{
			PackID: packID, EntryCount: 1, StoredBytes: 32,
			CreatedAt: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
		},
		[]packstore.Adoption{{Entry: packstore.IndexEntry{
			Hash: hash, PackID: packID, Offset: pack.MinEntryOffset,
			StoredLen: 9, RawLen: 20,
		}}},
	))
	resolution, err = s.ResolveBlobLocations(t.Context(), hash)
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 1)
	assert.Nil(t, resolution.Candidates[0].Loose)
	require.NotNil(t, resolution.Candidates[0].Pack)
	assert.Equal(t, packID, resolution.Candidates[0].Pack.PackID)
}
