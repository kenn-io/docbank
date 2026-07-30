package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecondaryBlobStoreLifecycle(t *testing.T) {
	s := newTestStore(t)
	secondary, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive_nas")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), secondary))

	stores, err := s.BlobStores(t.Context())
	require.NoError(t, err)
	require.Len(t, stores, 2)
	assert.Equal(t, "primary", stores[0].Role)
	assert.Equal(t, secondary, stores[1])

	resolved, err := s.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, secondary, resolved)
	resolved, err = s.BlobStoreBySelector(t.Context(), "archive")
	require.NoError(t, err)
	assert.Equal(t, secondary, resolved)

	require.NoError(t, s.DetachBlobStore(t.Context(), secondary.ID))
	detached, err := s.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", detached.Lifecycle)
	require.NoError(t, s.UnregisterBlobStore(t.Context(), secondary.ID))
	_, err = s.BlobStoreBySelector(t.Context(), secondary.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestBlobStoreRemovalRequiresEmptyDetachedSecondary(t *testing.T) {
	s := newTestStore(t)
	primary, err := s.PrimaryBlobStore(t.Context())
	require.NoError(t, err)
	require.ErrorIs(t, s.DetachBlobStore(t.Context(), primary.ID), ErrBlobStorePrimary)

	secondary, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive_nas")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), secondary))
	require.ErrorIs(t, s.UnregisterBlobStore(t.Context(), secondary.ID), ErrBlobStoreState)

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err = s.db.Exec(
		`INSERT INTO blobs(hash, size, created_at) VALUES(?, 1, ?)`,
		hash, nowRFC3339(),
	)
	require.NoError(t, err)
	_, err = s.db.Exec(`
		INSERT INTO blob_locations(
			blob_hash, store_id, generation, kind, encoding, stored_size, pack_eligible
		) VALUES(?, ?, ?, 'loose', 'raw', 1, 1)`,
		hash,
		secondary.ID, "30000000-0000-4000-8000-000000000001",
	)
	require.NoError(t, err)
	require.ErrorIs(t, s.DetachBlobStore(t.Context(), secondary.ID), ErrBlobStoreNotEmpty)
}

func TestBlobStoreRegistrationRejectsConflicts(t *testing.T) {
	s := newTestStore(t)
	first, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive_nas")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), first))

	for _, candidate := range []BlobStore{
		{
			ID: first.ID, Name: "other", Kind: "filesystem", Role: "secondary",
			Lifecycle: "active", Binding: "other", OwnershipEpoch: first.OwnershipEpoch,
			CreatedAt: first.CreatedAt,
		},
		{
			ID:   "30000000-0000-4000-8000-000000000002",
			Name: first.Name, Kind: "filesystem", Role: "secondary",
			Lifecycle: "active", Binding: "other",
			OwnershipEpoch: "30000000-0000-4000-8000-000000000003",
			CreatedAt:      first.CreatedAt,
		},
	} {
		require.ErrorIs(t, s.RegisterBlobStore(t.Context(), candidate), ErrExists)
	}
}
