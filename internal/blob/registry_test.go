package blob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/safefileio"

	"go.kenn.io/docbank/internal/config"
)

func TestConfiguredS3BackendRejectsPlainHTTP(t *testing.T) {
	_, err := NewConfiguredBackend(t.Context(), config.StoreBindingConfig{
		Kind: "s3", Endpoint: "http://127.0.0.1:9000", Region: "us-east-1",
		Bucket: "documents", CredentialProfile: "test", ForcePathStyle: true,
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTPS")
}

func TestRegistryClassifiesBindingsAndOwnership(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	root := t.TempDir()
	require.NoError(t, EnsureFilesystemNamespace(root))
	expected := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: vaultID,
		Store: storeID, Epoch: epoch,
	}
	unattached, err := NewFilesystemBackend(root, nil)
	require.NoError(t, err)
	require.NoError(t, unattached.ReplaceOwnership(t.Context(), expected, nil))
	require.NoError(t, unattached.Close())

	spec := StoreSpec{
		ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
		Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
	}
	registry := NewRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem, Path: root, Priority: 25},
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

func TestRegistryKeepsAcquiredBackendUsableAcrossConcurrentRefresh(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	root := t.TempDir()
	require.NoError(t, EnsureFilesystemNamespace(root))
	expected := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: vaultID,
		Store: storeID, Epoch: epoch,
	}
	unattached, err := NewFilesystemBackend(root, nil)
	require.NoError(t, err)
	require.NoError(t, unattached.ReplaceOwnership(t.Context(), expected, nil))
	require.NoError(t, unattached.Close())
	var opened []*closeObservedBackend
	registry := newRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem, Path: root, Priority: 25},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}}, func(
			ctx context.Context,
			binding config.StoreBindingConfig,
			ownership *packstore.Ownership,
		) (packstore.Backend, error) {
			backend, err := NewConfiguredBackend(ctx, binding, ownership)
			if err != nil {
				return nil, err
			}
			observed := &closeObservedBackend{Backend: backend}
			opened = append(opened, observed)
			return observed, nil
		})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })
	backend, ok := registry.WritableBackend(storeID)
	require.True(t, ok)
	ready := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(ready)
		<-release
		_, err := backend.Ownership(t.Context())
		result <- err
	}()
	<-ready
	assert.Equal(t, StoreOnline, registry.Refresh(t.Context(), storeID).State)
	close(release)
	require.NoError(t, <-result)
	require.Len(t, opened, 1)
	assert.False(t, opened[0].closed.Load())
}

func TestRegistrySecuresOwnedFilesystemScaffoldingOnAttachment(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	root := filepath.Join(t.TempDir(), "archive")
	require.NoError(t, EnsureFilesystemNamespace(root))
	backend, err := NewFilesystemBackend(root, nil)
	require.NoError(t, err)
	require.NoError(t, backend.ReplaceOwnership(t.Context(), packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  vaultID,
		Store:  storeID,
		Epoch:  epoch,
	}, nil))
	require.NoError(t, backend.Close())
	shard := filepath.Join(root, "aa")
	require.NoError(t, os.MkdirAll(shard, 0o755))
	require.NoError(t, os.Chmod(shard, 0o755))

	registry := NewRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem, Path: root},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })
	assert.Equal(t, StoreOnline, registry.Observation(storeID).State)
	require.NoError(t, safefileio.ValidatePrivateDir(shard))
}

func TestRegistryDoesNotRepairMismatchedFilesystemNamespace(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	root := filepath.Join(t.TempDir(), "archive")
	require.NoError(t, EnsureFilesystemNamespace(root))
	backend, err := NewFilesystemBackend(root, nil)
	require.NoError(t, err)
	require.NoError(t, backend.ReplaceOwnership(t.Context(), packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "10000000-0000-4000-8000-000000000099",
		Store:  storeID,
		Epoch:  epoch,
	}, nil))
	require.NoError(t, backend.Close())
	shard := filepath.Join(root, "aa")
	require.NoError(t, os.Mkdir(shard, 0o700))
	makeFilesystemNamespaceInsecure(t, shard)

	registry := NewRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem, Path: root},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })
	assert.Equal(t, StoreFenced, registry.Observation(storeID).State)
	require.Error(t, safefileio.ValidatePrivateDir(shard))
}

func TestRegistryClassifiesMissingOwnershipMarkerAsUnavailable(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	root := filepath.Join(t.TempDir(), "archive")
	require.NoError(t, EnsureFilesystemNamespace(root))

	registry := NewRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem, Path: root},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })
	assert.Equal(t, StoreUnavailable, registry.Observation(storeID).State)
}

func TestRegistryKeepsUnboundStoreDegraded(t *testing.T) {
	spec := StoreSpec{
		ID:   "20000000-0000-4000-8000-000000000001",
		Kind: storeKindFilesystem, Role: "secondary", Lifecycle: "active",
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

type closeObservedBackend struct {
	packstore.Backend

	closed atomic.Bool
}

func (b *closeObservedBackend) Ownership(ctx context.Context) (packstore.Ownership, error) {
	if b.closed.Load() {
		return packstore.Ownership{}, errors.New("backend was closed")
	}
	ownership, err := b.Backend.Ownership(ctx)
	if err != nil {
		return packstore.Ownership{}, fmt.Errorf("reading observed backend ownership: %w", err)
	}
	return ownership, nil
}

func (b *closeObservedBackend) Close() error {
	b.closed.Store(true)
	return closeBackend(b.Backend)
}

func (r staticLocationResolver) ResolveLocations(
	context.Context, packstore.Hash,
) (packstore.Resolution, error) {
	return r.resolution, nil
}
