package blob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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

func TestRegistryReopensFencedStoreAgainstObservedOwnershipForSalvage(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	taken := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "10000000-0000-4000-8000-000000000099",
		Store:  storeID,
		Epoch:  "30000000-0000-4000-8000-000000000099",
	}
	var openings []*packstore.Ownership
	registry := newRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem, Path: t.TempDir()},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}}, func(
			_ context.Context,
			_ config.StoreBindingConfig,
			expected *packstore.Ownership,
		) (packstore.Backend, error) {
			if expected == nil {
				openings = append(openings, nil)
			} else {
				expectedOwnership := *expected
				openings = append(openings, &expectedOwnership)
			}
			return &staticOwnershipBackend{ownership: taken}, nil
		})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })
	require.Equal(t, StoreFenced, registry.Observation(storeID).State)

	salvage, err := registry.SalvageBackend(t.Context(), storeID)
	require.NoError(t, err)
	_, exposesWrites := salvage.(packstore.Backend)
	assert.False(t, exposesWrites)
	require.Len(t, openings, 3)
	assert.Nil(t, openings[1])
	require.NotNil(t, openings[2])
	assert.Equal(t, taken, *openings[2])
	closer, ok := salvage.(interface{ Close() error })
	require.True(t, ok)
	require.NoError(t, closer.Close())
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

func TestRegistryRefreshDoesNotBlockBackendLookup(t *testing.T) {
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

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	registry := newRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem, Path: root},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}}, func(
			ctx context.Context,
			binding config.StoreBindingConfig,
			ownership *packstore.Ownership,
		) (packstore.Backend, error) {
			backend, openErr := NewConfiguredBackend(ctx, binding, ownership)
			if openErr != nil {
				return nil, openErr
			}
			return &blockingOwnershipBackend{
				Backend: backend, probeStarted: probeStarted, releaseProbe: releaseProbe,
			}, nil
		})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })

	refreshed := make(chan StoreObservation, 1)
	go func() { refreshed <- registry.Refresh(t.Context(), storeID) }()
	<-probeStarted
	lookup := make(chan bool, 1)
	go func() {
		_, ok := registry.Backend(storeID)
		lookup <- ok
	}()
	select {
	case ok := <-lookup:
		assert.True(t, ok)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("backend lookup blocked behind remote ownership probe")
	}
	close(releaseProbe)
	assert.Equal(t, StoreOnline, (<-refreshed).State)
}

func TestRegistryRefreshCallerCancellationPreservesHealthyBackend(t *testing.T) {
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
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	registry := newRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem, Path: root},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}}, func(
			ctx context.Context,
			binding config.StoreBindingConfig,
			ownership *packstore.Ownership,
		) (packstore.Backend, error) {
			backend, openErr := NewConfiguredBackend(ctx, binding, ownership)
			if openErr != nil {
				return nil, openErr
			}
			return &blockingOwnershipBackend{
				Backend: backend, probeStarted: probeStarted, releaseProbe: releaseProbe,
			}, nil
		})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })

	ctx, cancel := context.WithCancel(t.Context())
	refreshed := make(chan StoreObservation, 1)
	go func() { refreshed <- registry.Refresh(ctx, storeID) }()
	<-probeStarted
	cancel()
	observation := <-refreshed
	assert.Equal(t, StoreOnline, observation.State)
	_, admitted := registry.Backend(storeID)
	assert.True(t, admitted)
	close(releaseProbe)
}

func TestRegistryRefreshReturnsWhenOwnershipProbeIgnoresContext(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	expected := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: vaultID,
		Store: storeID, Epoch: epoch,
	}
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	backend := &contextIgnoringOwnershipBackend{
		ownership: expected, probeStarted: probeStarted, releaseProbe: releaseProbe,
	}
	registry := newRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}}, func(
			context.Context,
			config.StoreBindingConfig,
			*packstore.Ownership,
		) (packstore.Backend, error) {
			return backend, nil
		})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	refreshed := make(chan StoreObservation, 1)
	go func() { refreshed <- registry.Refresh(ctx, storeID) }()
	<-probeStarted
	select {
	case observation := <-refreshed:
		assert.Equal(t, StoreOnline, observation.State)
	case <-time.After(500 * time.Millisecond):
		close(releaseProbe)
		<-refreshed
		t.Fatal("refresh did not return after its context deadline")
	}
	close(releaseProbe)
}

func TestRegistryCoalescesRetriesWhileProbeRemainsBlocked(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	expected := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: vaultID,
		Store: storeID, Epoch: epoch,
	}
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	backend := &contextIgnoringOwnershipBackend{
		ownership: expected, probeStarted: probeStarted, releaseProbe: releaseProbe,
	}
	registry := newRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem},
		}, []StoreSpec{{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
		}}, func(
			context.Context,
			config.StoreBindingConfig,
			*packstore.Ownership,
		) (packstore.Backend, error) {
			return backend, nil
		})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	first := registry.Refresh(ctx, storeID)
	cancel()
	assert.Equal(t, StoreOnline, first.State)
	assert.Equal(t, int64(2), backend.calls.Load())

	retryCtx, cancelRetry := context.WithTimeout(t.Context(), 25*time.Millisecond)
	retry := registry.Refresh(retryCtx, storeID)
	cancelRetry()
	assert.Equal(t, StoreOnline, retry.State)
	assert.Equal(t, int64(2), backend.calls.Load(), "retry started a duplicate blocked probe")
	close(releaseProbe)
	require.Eventually(t, func() bool {
		registry.mu.RLock()
		defer registry.mu.RUnlock()
		_, probing := registry.probes[storeID]
		return !probing
	}, time.Second, time.Millisecond)
	assert.Equal(t, StoreOnline, registry.Refresh(t.Context(), storeID).State)
	assert.Equal(t, int64(3), backend.calls.Load())
}

func TestRegistryDetachSupersedesBlockedProbe(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	expected := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: vaultID,
		Store: storeID, Epoch: epoch,
	}
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	backend := &contextIgnoringOwnershipBackend{
		ownership: expected, probeStarted: probeStarted, releaseProbe: releaseProbe,
	}
	spec := StoreSpec{
		ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
		Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
	}
	registry := newRegistry(t.Context(), vaultID,
		map[string]config.StoreBindingConfig{
			"archive": {Kind: storeKindFilesystem},
		}, []StoreSpec{spec}, func(
			context.Context,
			config.StoreBindingConfig,
			*packstore.Ownership,
		) (packstore.Backend, error) {
			return backend, nil
		})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })

	refreshResult := make(chan StoreObservation, 1)
	go func() { refreshResult <- registry.Refresh(t.Context(), storeID) }()
	<-probeStarted
	spec.Lifecycle = "detached"
	observation := registry.AttachSpec(t.Context(), spec)
	assert.Equal(t, StoreDetached, observation.State)
	_, admitted := registry.Backend(storeID)
	assert.False(t, admitted)
	close(releaseProbe)
	assert.Equal(t, StoreDetached, (<-refreshResult).State)
}

func TestRegistryClosesBackendOpenedAfterProbeDeadline(t *testing.T) {
	const (
		vaultID = "10000000-0000-4000-8000-000000000001"
		storeID = "20000000-0000-4000-8000-000000000001"
		epoch   = "30000000-0000-4000-8000-000000000001"
	)
	expected := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: vaultID,
		Store: storeID, Epoch: epoch,
	}
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	backend := &closeObservedBackend{Backend: &staticOwnershipBackend{ownership: expected}}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	registryReady := make(chan *Registry, 1)
	go func() {
		registryReady <- newRegistry(ctx, vaultID,
			map[string]config.StoreBindingConfig{
				"archive": {Kind: storeKindFilesystem},
			}, []StoreSpec{{
				ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
				Lifecycle: "active", Binding: "archive", OwnershipEpoch: epoch,
			}}, func(
				context.Context,
				config.StoreBindingConfig,
				*packstore.Ownership,
			) (packstore.Backend, error) {
				close(probeStarted)
				<-releaseProbe
				return backend, nil
			})
	}()
	<-probeStarted
	var registry *Registry
	select {
	case registry = <-registryReady:
		assert.Equal(t, StoreUnavailable, registry.Observation(storeID).State)
	case <-time.After(500 * time.Millisecond):
		close(releaseProbe)
		registry = <-registryReady
		require.NoError(t, registry.Close())
		t.Fatal("registry construction did not return after its context deadline")
	}
	t.Cleanup(func() { require.NoError(t, registry.Close()) })
	close(releaseProbe)
	require.Eventually(t, backend.closed.Load, time.Second, time.Millisecond)
}

func TestRegistryRefreshStoresRunsContextIgnoringProbesConcurrently(t *testing.T) {
	const vaultID = "10000000-0000-4000-8000-000000000001"
	storeIDs := []string{
		"20000000-0000-4000-8000-000000000001",
		"20000000-0000-4000-8000-000000000002",
	}
	epochs := []string{
		"30000000-0000-4000-8000-000000000001",
		"30000000-0000-4000-8000-000000000002",
	}
	bindings := make(map[string]config.StoreBindingConfig, len(storeIDs))
	backends := make(map[string]*contextIgnoringOwnershipBackend, len(storeIDs))
	stores := make([]StoreSpec, 0, len(storeIDs))
	releaseProbes := make(chan struct{})
	for i, storeID := range storeIDs {
		bindingName := fmt.Sprintf("archive-%d", i)
		bindings[bindingName] = config.StoreBindingConfig{
			Kind: storeKindFilesystem, Path: storeID,
		}
		backends[storeID] = &contextIgnoringOwnershipBackend{
			ownership: packstore.Ownership{
				Format: packstore.OwnershipFormatV1, Vault: vaultID,
				Store: packstore.StoreID(storeID), Epoch: epochs[i],
			},
			probeStarted: make(chan struct{}), releaseProbe: releaseProbes,
		}
		stores = append(stores, StoreSpec{
			ID: storeID, Kind: storeKindFilesystem, Role: "secondary",
			Lifecycle: "active", Binding: bindingName, OwnershipEpoch: epochs[i],
		})
	}
	registry := newRegistry(t.Context(), vaultID, bindings, stores, func(
		_ context.Context,
		binding config.StoreBindingConfig,
		_ *packstore.Ownership,
	) (packstore.Backend, error) {
		return backends[binding.Path], nil
	})
	t.Cleanup(func() { require.NoError(t, registry.Close()) })

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	refreshed := make(chan map[string]StoreObservation, 1)
	go func() { refreshed <- registry.RefreshStores(ctx, storeIDs) }()
	for _, storeID := range storeIDs {
		select {
		case <-backends[storeID].probeStarted:
		case <-time.After(250 * time.Millisecond):
			close(releaseProbes)
			<-refreshed
			t.Fatalf("probe for store %s did not start concurrently", storeID)
		}
	}
	close(releaseProbes)
	observations := <-refreshed
	require.Len(t, observations, len(storeIDs))
	for _, storeID := range storeIDs {
		assert.Equal(t, StoreOnline, observations[storeID].State)
	}
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

type staticOwnershipBackend struct {
	packstore.Backend

	ownership packstore.Ownership
}

func (b *staticOwnershipBackend) Ownership(context.Context) (packstore.Ownership, error) {
	return b.ownership, nil
}

type contextIgnoringOwnershipBackend struct {
	packstore.Backend

	ownership    packstore.Ownership
	probeStarted chan struct{}
	releaseProbe chan struct{}
	calls        atomic.Int64
}

func (b *contextIgnoringOwnershipBackend) Ownership(context.Context) (packstore.Ownership, error) {
	call := b.calls.Add(1)
	if call > 1 {
		if call == 2 {
			close(b.probeStarted)
		}
		<-b.releaseProbe
	}
	return b.ownership, nil
}

type blockingOwnershipBackend struct {
	packstore.Backend

	probeStarted chan struct{}
	releaseProbe chan struct{}
	calls        atomic.Int64
}

func (b *blockingOwnershipBackend) Ownership(ctx context.Context) (packstore.Ownership, error) {
	if b.calls.Add(1) > 1 {
		close(b.probeStarted)
		select {
		case <-ctx.Done():
			return packstore.Ownership{}, ctx.Err()
		case <-b.releaseProbe:
		}
	}
	ownership, err := b.Backend.Ownership(ctx)
	if err != nil {
		return packstore.Ownership{}, fmt.Errorf("reading blocking backend ownership: %w", err)
	}
	return ownership, nil
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
