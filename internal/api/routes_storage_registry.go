package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
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
			Takeover bool   `json:"takeover,omitempty"`
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
					blob.StoreObservation{State: blob.StoreUnbound},
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
		stores, inventory, err := readBlobStores(ctx, d)
		if err != nil {
			return nil, FromStoreError(err)
		}
		result := make([]BlobStore, 0, len(stores))
		for _, item := range stores {
			observation := storageObservation(d, item)
			if in.Refresh && d.BlobRegistry != nil && item.Role != "primary" {
				observation = d.BlobRegistry.Refresh(ctx, item.ID)
			}
			result = append(result, blobStoreAPI(item, inventory[item.ID], observation))
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
			output = &storeOutput{Body: blobStoreAPI(item, store.BlobStoreStats{}, observation)}
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
	if binding.Kind != "filesystem" {
		return storageRegistrationPlan{}, NewError(http.StatusNotImplemented,
			"storage_backend_unavailable", "S3 registration is not active in this build")
	}
	overlap, err := ingest.PathsOverlap(binding.Path, d.VaultRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return storageRegistrationPlan{}, NewError(http.StatusUnprocessableEntity,
			"storage_binding_invalid", err.Error())
	}
	if overlap {
		return storageRegistrationPlan{}, NewError(http.StatusConflict,
			"storage_namespace_overlap",
			fmt.Sprintf("binding %q overlaps the live vault root", bindingName))
	}
	backend, err := blob.NewFilesystemBackend(binding.Path, nil)
	if err != nil {
		return storageRegistrationPlan{}, NewError(http.StatusUnprocessableEntity,
			"storage_binding_invalid", err.Error())
	}
	defer func() { _ = backend.Close() }()
	current, markerErr := backend.Ownership(ctx)
	if errors.Is(markerErr, fs.ErrNotExist) {
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
	if plan.binding.Kind != "filesystem" {
		return BlobStore{}, NewError(http.StatusNotImplemented,
			"storage_backend_unavailable", "S3 registration is not active in this build")
	}
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
	if err := os.MkdirAll(plan.binding.Path, 0o700); err != nil {
		return BlobStore{}, NewError(http.StatusServiceUnavailable,
			"store_unavailable", fmt.Sprintf("preparing storage namespace: %v", err))
	}
	overlap, err := ingest.PathsOverlap(plan.binding.Path, d.VaultRoot)
	if err != nil {
		return BlobStore{}, NewError(http.StatusUnprocessableEntity,
			"storage_binding_invalid", err.Error())
	}
	if overlap {
		return BlobStore{}, NewError(http.StatusConflict, "storage_namespace_overlap",
			"storage namespace now overlaps the live vault root; preview again")
	}
	backend, err := blob.NewFilesystemBackend(plan.binding.Path, nil)
	if err != nil {
		return BlobStore{}, NewError(http.StatusUnprocessableEntity,
			"storage_binding_invalid", err.Error())
	}
	defer func() { _ = backend.Close() }()
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
	if err := d.Store.RegisterBlobStore(ctx, plan.store); err != nil {
		return BlobStore{}, FromStoreError(err)
	}
	observation := d.BlobRegistry.AttachSpec(ctx, blobStoreSpec(plan.store))
	return blobStoreAPI(plan.store, store.BlobStoreStats{}, observation), nil
}

func storageStoreStatuses(ctx context.Context, d Deps) ([]StorageStoreStatus, error) {
	stores, inventory, err := readBlobStores(ctx, d)
	if err != nil {
		return nil, err
	}
	result := make([]StorageStoreStatus, 0, len(stores))
	for _, item := range stores {
		result = append(result,
			blobStoreAPI(item, inventory[item.ID], storageObservation(d, item)).StorageStoreStatus)
	}
	return result, nil
}

func readBlobStores(
	ctx context.Context, d Deps,
) ([]store.BlobStore, map[string]store.BlobStoreStats, error) {
	stores, err := d.Store.BlobStores(ctx)
	if err != nil {
		return nil, nil, err
	}
	inventory, err := d.Store.BlobStoreInventory(ctx)
	return stores, inventory, err
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
) BlobStore {
	observedAt := ""
	if !observation.ObservedAt.IsZero() {
		observedAt = observation.ObservedAt.Format(time.RFC3339Nano)
	}
	return BlobStore{
		StorageStoreStatus: StorageStoreStatus{
			ID: item.ID, Name: item.Name, Kind: item.Kind, Role: item.Role,
			Lifecycle: item.Lifecycle, State: string(observation.State),
			Priority:             observation.Priority,
			AuthoritativeObjects: stats.AuthoritativeObjects,
			LogicalBytes:         stats.LogicalBytes, StoredBytes: stats.StoredBytes,
			PackCount: stats.PackCount, DeadPackedBytes: stats.DeadPackedBytes,
			ObservedAt: observedAt,
		},
		Binding: item.Binding, OwnershipEpoch: item.OwnershipEpoch,
		Detail: observation.Detail, CreatedAt: item.CreatedAt.Format(time.RFC3339Nano),
	}
}

func blobStoreSpec(item store.BlobStore) blob.StoreSpec {
	return blob.StoreSpec{
		ID: item.ID, Kind: item.Kind, Role: item.Role, Lifecycle: item.Lifecycle,
		Binding: item.Binding, OwnershipEpoch: item.OwnershipEpoch,
	}
}
