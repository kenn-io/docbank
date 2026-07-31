package blob

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/config"
)

func TestInsecureS3TransportIsLimitedToLoopback(t *testing.T) {
	assert.True(t, allowInsecureLoopbackEndpoint("http://127.0.0.1:9000"))
	assert.True(t, allowInsecureLoopbackEndpoint("http://[::1]:9000"))
	assert.True(t, allowInsecureLoopbackEndpoint("http://localhost:9000"))
	assert.False(t, allowInsecureLoopbackEndpoint("http://objects.example:9000"))
	assert.False(t, allowInsecureLoopbackEndpoint("https://127.0.0.1:9000"))
}

func TestRegistryClassifiesBindingsAndOwnership(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	root := t.TempDir()
	expected := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: vaultID,
		Store: storeID, Epoch: epoch,
	}
	unattached, err := NewFilesystemBackend(root, nil)
	require.NoError(t, err)
	require.NoError(t, unattached.ReplaceOwnership(t.Context(), expected, nil))
	require.NoError(t, unattached.Close())

	spec := StoreSpec{
		ID: storeID, Kind: "filesystem", Role: "secondary",
		Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
	}
	registry := NewRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: "filesystem", Path: root, Priority: 25},
		}, []StoreSpec{spec})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })
	assert.Equal(t, StoreOnline, registry.Observation(storeID).State)
	_, ok := registry.Backend(storeID)
	assert.True(t, ok)

	takenOver := expected
	takenOver.Epoch = "30000000-0000-4000-8000-000000000002"
	require.NoError(t, unattached.ReplaceOwnership(t.Context(), takenOver, &expected))
	assert.Equal(t, StoreFenced, registry.Refresh(t.Context(), storeID).State)
	_, ok = registry.Backend(storeID)
	assert.False(t, ok)
}

func TestRegistryKeepsUnboundStoreDegraded(t *testing.T) {
	spec := StoreSpec{
		ID:   "20000000-0000-4000-8000-000000000001",
		Kind: "filesystem", Role: "secondary", Lifecycle: "active",
		Binding: "missing", OwnershipEpoch: "30000000-0000-4000-8000-000000000001",
	}
	registry := NewRegistry(t.Context(),
		"10000000-0000-4000-8000-000000000001", nil, []StoreSpec{spec})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })

	observation := registry.Observation(spec.ID)
	assert.Equal(t, StoreUnbound, observation.State)
	assert.Contains(t, observation.Detail, "restart")
	_, ok := registry.Backend(packstore.StoreID(spec.ID))
	assert.False(t, ok)
}

func TestUnboundSecondaryReadReportsStoreUnavailable(t *testing.T) {
	hash, err := packstore.ParseHash(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	)
	require.NoError(t, err)
	const storeID = "20000000-0000-4000-8000-000000000001"
	resolver := staticLocationResolver{resolution: packstore.Resolution{
		Member: true,
		Candidates: []packstore.ReadLocation{{
			StoreID: storeID, Generation: "30000000-0000-4000-8000-000000000001",
			Loose: &packstore.LooseLocation{
				Encoding: packstore.LooseEncodingRaw, LogicalSize: 1, StoredSize: 1,
			},
		}},
	}}
	registry := NewRegistry(t.Context(),
		"10000000-0000-4000-8000-000000000001", nil, nil)
	t.Cleanup(func() { require.NoError(t, registry.Close()) })
	reader, err := packstore.NewMultiStore(resolver, registry, packstore.MultiStoreOptions{
		Limits: StorageLimits(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	_, _, err = reader.OpenStream(t.Context(), hash)
	require.ErrorIs(t, err, packstore.ErrStoreUnavailable)
	require.NotErrorIs(t, err, packstore.ErrPhysicalMissing)
}

type staticLocationResolver struct {
	resolution packstore.Resolution
}

func (r staticLocationResolver) ResolveLocations(
	context.Context, packstore.Hash,
) (packstore.Resolution, error) {
	return r.resolution, nil
}
