package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/storenamespace"
)

func registerStorageRegistryRoutes(api huma.API, d Deps, g *gate) {
	previews := newStoragePreviewRegistry()

	type previewOutput struct{ Body BlobStorePreview }
	huma.Register(api, huma.Operation{
		OperationID: "previewBlobStoreRegistration", Method: http.MethodPost,
		Path:    "/api/v1/storage/stores/preview",
		Summary: "Preview attaching one configured secondary blob store",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name     string `json:"name" minLength:"1" maxLength:"200"`
			Binding  string `json:"binding" minLength:"1" maxLength:"63"`
			Takeover bool   `json:"takeover,omitzero"`
		}
	}) (*previewOutput, error) {
		if d.BlobRegistry == nil {
			return nil, NewError(http.StatusConflict, "storage_configuration_stale",
				"the running daemon has no storage binding registry; restart it after updating config.toml")
		}
		var output *previewOutput
		err := g.mutate(func() error {
			plan, err := previewStorageRegistration(
				ctx, d, in.Body.Name, in.Body.Binding, in.Body.Takeover,
			)
			if err != nil {
				return err
			}
			token, expiresAt, err := previews.issue(plan)
			if err != nil {
				return NewError(http.StatusInternalServerError, "internal",
					fmt.Sprintf("issuing storage preview token: %v", err))
			}
			output = &previewOutput{Body: BlobStorePreview{
				Store: blobStoreAPI(
					plan.store, store.BlobStoreStats{},
					blob.StoreObservation{State: blob.StoreUnbound}, 0,
				),
				MarkerAction: plan.markerAction, Takeover: plan.takeover,
				PreviewToken: token, ExpiresAt: expiresAt.Format(time.RFC3339Nano),
			}}
			return nil
		})
		return output, err
	})

	type storeOutput struct{ Body BlobStore }
	huma.Register(api, huma.Operation{
		OperationID: "registerBlobStore", Method: http.MethodPost,
		Path:    "/api/v1/storage/stores",
		Summary: "Attach the exact secondary store reviewed by a preview",
	}, func(ctx context.Context, in *struct {
		Body struct {
			PreviewToken string `json:"preview_token" minLength:"43" maxLength:"43"`
		}
	}) (*storeOutput, error) {
		var output *storeOutput
		err := g.mutate(func() error {
			plan, err := previews.take(in.Body.PreviewToken)
			if err != nil {
				return NewError(http.StatusConflict, "storage_preview_stale", err.Error())
			}
			result, err := applyStorageRegistration(ctx, d, plan)
			if err != nil {
				return err
			}
			output = &storeOutput{Body: result}
			return nil
		})
		return output, err
	})

	type storesOutput struct{ Body []BlobStore }
	huma.Register(api, huma.Operation{
		OperationID: "listBlobStores", Method: http.MethodGet,
		Path:    "/api/v1/storage/stores",
		Summary: "List cataloged blob stores and their deployment state",
	}, func(ctx context.Context, in *struct {
		Refresh bool `query:"refresh"`
	}) (*storesOutput, error) {
		stores, err := readBlobStores(ctx, d, in.Refresh)
		if err != nil {
			return nil, FromStoreError(err)
		}
		result := make([]BlobStore, 0, len(stores))
		for _, item := range stores {
			result = append(result, blobStoreAPI(
				item.store, item.stats, item.observation, item.unreadable,
			))
		}
		return &storesOutput{Body: result}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "detachBlobStore", Method: http.MethodPost,
		Path:    "/api/v1/storage/stores/{store_id}/detach",
		Summary: "Detach one empty secondary store from runtime use",
	}, func(ctx context.Context, in *struct {
		StoreID string `path:"store_id"`
	}) (*storeOutput, error) {
		var output *storeOutput
		err := g.mutate(func() error {
			if err := d.Store.DetachBlobStore(ctx, in.StoreID); err != nil {
				return FromStoreError(err)
			}
			item, err := d.Store.BlobStoreBySelector(ctx, in.StoreID)
			if err != nil {
				return FromStoreError(err)
			}
			observation := blob.StoreObservation{State: blob.StoreDetached}
			if d.BlobRegistry != nil {
				observation = d.BlobRegistry.AttachSpec(ctx, blobStoreSpec(item))
			}
			output = &storeOutput{Body: blobStoreAPI(
				item, store.BlobStoreStats{}, observation, 0,
			)}
			return nil
		})
		return output, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "unregisterBlobStore", Method: http.MethodDelete,
		Path:          "/api/v1/storage/stores/{store_id}",
		Summary:       "Forget one detached and empty secondary store identity",
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		StoreID string `path:"store_id"`
	}) (*struct{}, error) {
		err := g.mutate(func() error {
			item, err := d.Store.BlobStoreBySelector(ctx, in.StoreID)
			if err != nil {
				return FromStoreError(err)
			}
			if err := d.Store.UnregisterBlobStore(ctx, item.ID); err != nil {
				return FromStoreError(err)
			}
			if d.BlobRegistry != nil {
				d.BlobRegistry.RemoveSpec(item.ID)
			}
			return nil
		})
		return &struct{}{}, err
	})
}

func previewStorageRegistration(
	ctx context.Context, d Deps, name, bindingName string, takeover bool,
) (storageRegistrationPlan, error) {
	binding, ok := d.BlobRegistry.Binding(bindingName)
	if !ok {
		return storageRegistrationPlan{}, NewError(http.StatusConflict,
			"storage_configuration_stale",
			fmt.Sprintf("binding %q is not loaded; update config.toml and restart the daemon", bindingName))
	}
	existingStores, err := d.Store.BlobStores(ctx)
	if err != nil {
		return storageRegistrationPlan{}, FromStoreError(err)
	}
	for _, existing := range existingStores {
		if existing.Name == name {
			return storageRegistrationPlan{}, FromStoreError(
				fmt.Errorf("blob store name %q: %w", name, store.ErrExists))
		}
	}
	candidate, err := d.Store.PrepareSecondaryBlobStore(name, binding.Kind, bindingName)
	if err != nil {
		return storageRegistrationPlan{}, NewError(
			http.StatusUnprocessableEntity, "validation", err.Error())
	}
	plan := storageRegistrationPlan{
		store: candidate, binding: binding, markerAction: "create", takeover: takeover,
	}
	if err := validateStorageNamespace(
		d, bindingName, binding, existingStores,
	); err != nil {
		return storageRegistrationPlan{}, err
	}
	backend, err := blob.NewInspectionBackend(ctx, binding)
	if err != nil {
		return storageRegistrationPlan{}, NewError(http.StatusUnprocessableEntity,
			"storage_binding_invalid", err.Error())
	}
	defer func() { _ = closeStorageBackend(backend) }()
	current, markerErr := backend.Ownership(ctx)
	if errors.Is(markerErr, fs.ErrNotExist) ||
		errors.Is(markerErr, packstore.ErrPhysicalMissing) {
		if err := requireEmptyUnmarkedNamespace(ctx, backend); err != nil {
			return storageRegistrationPlan{}, err
		}
		return plan, nil
	}
	if markerErr != nil {
		return storageRegistrationPlan{}, NewError(http.StatusServiceUnavailable,
			"store_unavailable", markerErr.Error())
	}
	plan.expected = &current
	if current.Vault == d.Store.VaultID() {
		if _, err := d.Store.BlobStoreBySelector(ctx, string(current.Store)); err == nil {
			return storageRegistrationPlan{}, NewError(http.StatusConflict,
				"storage_store_exists", "this namespace is already registered")
		} else if !errors.Is(err, store.ErrNotFound) {
			return storageRegistrationPlan{}, FromStoreError(err)
		}
		plan.store.ID = string(current.Store)
		plan.store.OwnershipEpoch = current.Epoch
		plan.markerAction = "reattach"
		return plan, nil
	}
	if !takeover {
		return storageRegistrationPlan{}, NewError(http.StatusConflict,
			"storage_ownership_mismatch",
			fmt.Sprintf("namespace belongs to vault %s store %s; preview again with takeover",
				current.Vault, current.Store))
	}
	plan.markerAction = "takeover"
	return plan, nil
}

func applyStorageRegistration(
	ctx context.Context, d Deps, plan storageRegistrationPlan,
) (BlobStore, error) {
	stores, err := d.Store.BlobStores(ctx)
	if err != nil {
		return BlobStore{}, FromStoreError(err)
	}
	for _, existing := range stores {
		if existing.ID == plan.store.ID || existing.Name == plan.store.Name {
			return BlobStore{}, NewError(http.StatusConflict, "storage_preview_stale",
				"the catalog changed after preview; preview the registration again")
		}
	}
	if err := validateStorageNamespace(
		d, plan.store.Binding, plan.binding, stores,
	); err != nil {
		return BlobStore{}, err
	}
	if err := revalidateStorageRegistrationTarget(ctx, plan); err != nil {
		return BlobStore{}, err
	}
	if plan.binding.Kind == "filesystem" {
		if err := blob.EnsureFilesystemNamespace(plan.binding.Path); err != nil {
			return BlobStore{}, NewError(http.StatusServiceUnavailable,
				"store_unavailable", fmt.Sprintf("preparing storage namespace: %v", err))
		}
	}
	backend, err := blob.NewConfiguredBackend(ctx, plan.binding, nil)
	if err != nil {
		return BlobStore{}, NewError(http.StatusUnprocessableEntity,
			"storage_binding_invalid", err.Error())
	}
	defer func() { _ = closeStorageBackend(backend) }()
	if plan.markerAction == "create" {
		if err := requireEmptyUnmarkedNamespace(ctx, backend); err != nil {
			return BlobStore{}, err
		}
	}
	next := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: d.Store.VaultID(),
		Store: packstore.StoreID(plan.store.ID), Epoch: plan.store.OwnershipEpoch,
	}
	if err := backend.ReplaceOwnership(ctx, next, plan.expected); err != nil {
		code := "storage_preview_stale"
		if errors.Is(err, packstore.ErrStoreFenced) {
			code = "store_fenced"
		}
		return BlobStore{}, NewError(http.StatusConflict, code, err.Error())
	}
	actual, err := backend.Ownership(ctx)
	if err != nil || actual != next {
		return BlobStore{}, NewError(http.StatusServiceUnavailable, "store_unavailable",
			fmt.Sprintf("ownership marker read-back failed: %v", err))
	}
	// Capability probes are mutating operations against an owned temporary
	// key. Run them only after the marker handoff, while catalog authority is
	// still absent. A failed probe leaves a fenced, authority-free namespace
	// that the same registration workflow can safely reclaim.
	if err := blob.ProbeConfiguredBackend(ctx, backend); err != nil {
		return BlobStore{}, NewError(
			http.StatusUnprocessableEntity, "storage_capability_missing", err.Error(),
		)
	}
	if err := d.Store.RegisterBlobStore(ctx, plan.store); err != nil {
		return BlobStore{}, FromStoreError(err)
	}
	observation := d.BlobRegistry.AttachSpec(ctx, blobStoreSpec(plan.store))
	return blobStoreAPI(plan.store, store.BlobStoreStats{}, observation, 0), nil
}

func revalidateStorageRegistrationTarget(
	ctx context.Context, plan storageRegistrationPlan,
) error {
	backend, err := blob.NewInspectionBackend(ctx, plan.binding)
	if err != nil {
		return NewError(http.StatusUnprocessableEntity,
			"storage_binding_invalid", err.Error())
	}
	defer func() { _ = closeStorageBackend(backend) }()
	current, markerErr := backend.Ownership(ctx)
	if plan.expected == nil {
		if errors.Is(markerErr, fs.ErrNotExist) ||
			errors.Is(markerErr, packstore.ErrPhysicalMissing) {
			return requireEmptyUnmarkedNamespace(ctx, backend)
		}
		if markerErr != nil {
			return NewError(http.StatusServiceUnavailable,
				"store_unavailable", markerErr.Error())
		}
		return NewError(http.StatusConflict, "storage_preview_stale",
			"the storage ownership marker changed after preview")
	}
	if markerErr != nil || current != *plan.expected {
		return NewError(http.StatusConflict, "storage_preview_stale",
			"the storage ownership marker changed after preview")
	}
	return nil
}

func requireEmptyUnmarkedNamespace(
	ctx context.Context, backend packstore.Backend,
) error {
	inspector, ok := backend.(packstore.NamespaceInspector)
	if !ok {
		return NewError(
			http.StatusUnprocessableEntity, "storage_capability_missing",
			"storage backend cannot prove an unmarked namespace is empty",
		)
	}
	empty, err := inspector.NamespaceEmpty(ctx)
	if err != nil {
		return NewError(http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
	if !empty {
		return NewError(
			http.StatusConflict, "storage_namespace_not_empty",
			"unmarked storage namespace contains data; use an empty namespace or explicitly recover it",
		)
	}
	return nil
}

func validateStorageNamespace(
	d Deps,
	bindingName string,
	binding config.StoreBindingConfig,
	stores []store.BlobStore,
) error {
	if binding.Kind == "s3" {
		if _, err := storenamespace.CanonicalS3(storageS3Binding(binding)); err != nil {
			return NewError(
				http.StatusUnprocessableEntity, "storage_binding_invalid", err.Error(),
			)
		}
	}
	if binding.Kind == "filesystem" {
		overlap, err := ingest.PathsOverlap(binding.Path, d.VaultRoot)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return NewError(
				http.StatusUnprocessableEntity, "storage_binding_invalid", err.Error(),
			)
		}
		if overlap {
			return NewError(
				http.StatusConflict, "storage_namespace_overlap",
				fmt.Sprintf("binding %q overlaps the live vault root", bindingName),
			)
		}
		watchName, watchOverlap, err := ingest.WatchBindingOverlap(
			d.Cfg.Watches, binding,
		)
		if err != nil {
			return NewError(
				http.StatusUnprocessableEntity, "storage_binding_invalid", err.Error(),
			)
		}
		if watchOverlap {
			return NewError(
				http.StatusConflict, "storage_namespace_overlap",
				fmt.Sprintf(
					"binding %q overlaps configured watch %q", bindingName, watchName,
				),
			)
		}
	}
	for _, existing := range stores {
		if existing.Role == "primary" || existing.Kind != binding.Kind ||
			existing.Lifecycle == "detached" {
			continue
		}
		existingBinding, ok := d.BlobRegistry.Binding(existing.Binding)
		if !ok {
			return NewError(
				http.StatusConflict, "storage_configuration_stale",
				fmt.Sprintf(
					"cannot prove namespace separation while binding %q is not loaded; restore it or detach store %s",
					existing.Binding, existing.ID,
				),
			)
		}
		var overlap bool
		var err error
		if binding.Kind == "filesystem" {
			overlap, err = ingest.PathsOverlap(binding.Path, existingBinding.Path)
		} else {
			overlap, err = storenamespace.S3Overlaps(
				storageS3Binding(binding), storageS3Binding(existingBinding),
			)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return NewError(
				http.StatusUnprocessableEntity, "storage_binding_invalid", err.Error(),
			)
		}
		if overlap {
			return NewError(
				http.StatusConflict, "storage_namespace_overlap",
				fmt.Sprintf(
					"binding %q overlaps registered store %q",
					bindingName, existing.Name,
				),
			)
		}
	}
	return nil
}

func storageS3Binding(binding config.StoreBindingConfig) storenamespace.S3Binding {
	return storenamespace.S3Binding{
		Endpoint: binding.Endpoint,
		Region:   binding.Region,
		Bucket:   binding.Bucket,
		Prefix:   binding.Prefix,
	}
}

func closeStorageBackend(backend packstore.Backend) error {
	if closer, ok := backend.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func storageStoreStatuses(
	ctx context.Context, d Deps, refresh bool,
) ([]StorageStoreStatus, error) {
	stores, err := readBlobStores(ctx, d, refresh)
	if err != nil {
		return nil, err
	}
	result := make([]StorageStoreStatus, 0, len(stores))
	for _, item := range stores {
		result = append(result, blobStoreAPI(
			item.store, item.stats, item.observation, item.unreadable,
		).StorageStoreStatus)
	}
	return result, nil
}

type blobStoreSnapshot struct {
	store       store.BlobStore
	stats       store.BlobStoreStats
	observation blob.StoreObservation
	unreadable  int64
}

func readBlobStores(
	ctx context.Context, d Deps, refresh bool,
) ([]blobStoreSnapshot, error) {
	stores, err := d.Store.BlobStores(ctx)
	if err != nil {
		return nil, err
	}
	inventory, err := d.Store.BlobStoreInventory(ctx)
	if err != nil {
		return nil, err
	}
	observations := make(map[string]blob.StoreObservation, len(stores))
	online := make(map[string]bool, len(stores))
	refreshIDs := make([]string, 0, len(stores))
	for _, item := range stores {
		observations[item.ID] = storageObservation(d, item)
		if refresh && d.BlobRegistry != nil && item.Role != "primary" {
			refreshIDs = append(refreshIDs, item.ID)
		}
	}
	if len(refreshIDs) > 0 {
		maps.Copy(observations, d.BlobRegistry.RefreshStores(ctx, refreshIDs))
	}
	for _, item := range stores {
		observation := observations[item.ID]
		online[item.ID] = observation.State == blob.StoreOnline
	}
	unreadable, err := d.Store.BlobStoreUnreadableObjects(ctx, online)
	if err != nil {
		return nil, err
	}
	result := make([]blobStoreSnapshot, 0, len(stores))
	for _, item := range stores {
		result = append(result, blobStoreSnapshot{
			store: item, stats: inventory[item.ID], observation: observations[item.ID],
			unreadable: unreadable[item.ID],
		})
	}
	return result, nil
}

func storageObservation(d Deps, item store.BlobStore) blob.StoreObservation {
	if item.Role == "primary" {
		return blob.StoreObservation{State: blob.StoreOnline}
	}
	if d.BlobRegistry == nil {
		return blob.StoreObservation{State: blob.StoreUnbound}
	}
	return d.BlobRegistry.Observation(item.ID)
}

func blobStoreAPI(
	item store.BlobStore,
	stats store.BlobStoreStats,
	observation blob.StoreObservation,
	unreadableObjects int64,
) BlobStore {
	observedAt := ""
	if !observation.ObservedAt.IsZero() {
		observedAt = observation.ObservedAt.Format(time.RFC3339Nano)
	}
	return BlobStore{
		ID: item.ID, Name: item.Name, Kind: item.Kind, Role: item.Role,
		Lifecycle: item.Lifecycle, State: string(observation.State),
		Priority:             observation.Priority,
		AuthoritativeObjects: stats.AuthoritativeObjects,
		LogicalBytes:         stats.LogicalBytes, StoredBytes: stats.StoredBytes,
		PackCount: stats.PackCount, DeadPackedBytes: stats.DeadPackedBytes,
		SoleAuthorityObjects: stats.SoleAuthorityObjects,
		AffectedDocuments:    stats.AffectedDocuments,
		UnreadableObjects:    unreadableObjects,
		ObservedAt:           observedAt,
		Binding:              item.Binding, OwnershipEpoch: item.OwnershipEpoch,
		Detail: observation.Detail, CreatedAt: item.CreatedAt.Format(time.RFC3339Nano),
	}
}

func blobStoreSpec(item store.BlobStore) blob.StoreSpec {
	return blob.StoreSpec{
		ID: item.ID, Kind: item.Kind, Role: item.Role, Lifecycle: item.Lifecycle,
		Binding: item.Binding, OwnershipEpoch: item.OwnershipEpoch,
	}
}
