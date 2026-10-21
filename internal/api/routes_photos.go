package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func registerPhotoRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "trashPhotoAsset", Method: http.MethodPost,
		Path: "/api/v1/photos/assets/{asset_id}/trash", Summary: "Move every photo asset member to recoverable trash",
	}, func(ctx context.Context, in *struct {
		AssetID string `path:"asset_id"`
		IfMatch string `header:"If-Match"`
	}) (*photoAssetOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAssetOutput
		err = g.mutate(func() error {
			asset, err := d.Store.TrashPhotoAsset(ctx, in.AssetID, revision)
			if err != nil {
				return FromStoreError(err)
			}
			out = &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "createPhotoAsset", Method: http.MethodPost,
		Path: "/api/v1/photos/assets", Summary: "Create a photo asset for one file",
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct{ Body CreatePhotoAssetRequest }) (*photoAssetOutput, error) {
		var out *photoAssetOutput
		err := g.mutate(func() error {
			asset, err := d.Store.CreatePhotoAsset(ctx, in.Body.NodeID, in.Body.Role, in.Body.Kind)
			if err != nil {
				return FromStoreError(err)
			}
			out = &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "getPhotoAsset", Method: http.MethodGet,
		Path: "/api/v1/photos/assets/{asset_id}", Summary: "Inspect one photo asset",
	}, func(ctx context.Context, in *struct {
		AssetID string `path:"asset_id"`
	}) (*photoAssetOutput, error) {
		asset, err := d.Store.PhotoAssetByID(ctx, in.AssetID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getPhotoAssetByNode", Method: http.MethodGet,
		Path: "/api/v1/photos/nodes/{node_id}/asset", Summary: "Inspect the photo asset containing one node",
	}, func(ctx context.Context, in *struct {
		NodeID int64 `path:"node_id"`
	}) (*photoAssetOutput, error) {
		asset, err := d.Store.PhotoAssetForNode(ctx, in.NodeID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "attachPhotoFile", Method: http.MethodPost,
		Path: "/api/v1/photos/assets/{asset_id}/files", Summary: "Attach one node to a photo asset",
	}, func(ctx context.Context, in *struct {
		AssetID string `path:"asset_id"`
		IfMatch string `header:"If-Match"`
		Body    AttachPhotoFileRequest
	}) (*photoAssetOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAssetOutput
		err = g.mutate(func() error {
			asset, callErr := d.Store.AttachPhotoFile(ctx, in.AssetID, revision, in.Body.NodeID, in.Body.Role, in.Body.SidecarOfID)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "detachPhotoFile", Method: http.MethodDelete,
		Path: "/api/v1/photos/assets/{asset_id}/files/{file_id}", Summary: "Detach one file from a photo asset",
	}, func(ctx context.Context, in *struct {
		AssetID                string `path:"asset_id"`
		FileID                 string `path:"file_id"`
		IfMatch                string `header:"If-Match"`
		ClearDependentSidecars bool   `query:"clear_dependent_sidecars"`
	}) (*photoAssetOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAssetOutput
		err = g.mutate(func() error {
			asset, callErr := d.Store.DetachPhotoFile(ctx, in.AssetID, revision, in.FileID, store.PhotoDetachOptions{ClearDependentSidecars: in.ClearDependentSidecars})
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "excludePhotoAsset", Method: http.MethodPost,
		Path: "/api/v1/photos/assets/{asset_id}/exclude", Summary: "Exclude or include a photo asset",
	}, func(ctx context.Context, in *struct {
		AssetID string `path:"asset_id"`
		IfMatch string `header:"If-Match"`
		Body    SetPhotoExcludedRequest
	}) (*photoAssetOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAssetOutput
		err = g.mutate(func() error {
			asset, callErr := d.Store.SetPhotoAssetExcluded(ctx, in.AssetID, revision, in.Body.Excluded)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "promotePhotoNode", Method: http.MethodPost,
		Path: "/api/v1/photos/nodes/{node_id}/promote", Summary: "Promote one node into a photo asset",
	}, func(ctx context.Context, in *struct {
		NodeID  int64  `path:"node_id"`
		IfMatch string `header:"If-Match"`
		Body    struct {
			Role string `json:"role,omitzero" enum:"raw,image,video,sidecar"`
			Kind string `json:"kind,omitzero" enum:"photo,video"`
		}
	}) (*photoAssetOutput, error) {
		var expectedRevision *int64
		if in.IfMatch != "" {
			revision, parseErr := parseIfMatch(in.IfMatch)
			if parseErr != nil {
				return nil, parseErr
			}
			expectedRevision = &revision
		}
		var out *photoAssetOutput
		err := g.mutate(func() error {
			asset, callErr := d.Store.PromotePhotoNode(ctx, in.NodeID, expectedRevision, in.Body.Role, in.Body.Kind)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "setPhotoDisplay", Method: http.MethodPut,
		Path: "/api/v1/photos/assets/{asset_id}/display", Summary: "Set or reset a photo display override",
	}, func(ctx context.Context, in *struct {
		AssetID string `path:"asset_id"`
		IfMatch string `header:"If-Match"`
		Body    SetPhotoDisplayRequest
	}) (*photoAssetOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoAssetOutput
		err = g.mutate(func() error {
			asset, callErr := d.Store.SetPhotoDisplay(ctx, in.AssetID, revision, in.Body.FileID)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &photoAssetOutput{ETag: revisionETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "getPhotoSettings", Method: http.MethodGet,
		Path: "/api/v1/photos/settings", Summary: "Inspect photo settings",
	}, func(ctx context.Context, _ *struct{}) (*photoSettingsOutput, error) {
		settings, err := d.Store.PhotoSettings(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &photoSettingsOutput{ETag: revisionETag(settings.Revision), Body: fromStorePhotoSettings(settings)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "setPhotoSettings", Method: http.MethodPut,
		Path: "/api/v1/photos/settings", Summary: "Set photo settings",
	}, func(ctx context.Context, in *struct {
		IfMatch string `header:"If-Match"`
		Body    SetPhotoSettingsRequest
	}) (*photoSettingsOutput, error) {
		revision, update, err := in.Body.update(ctx, in.IfMatch)
		if err != nil {
			return nil, err
		}
		update.ValidateSchedule = func(next store.PhotoSettings) error {
			if err := effectiveBackupConfig(d.Cfg.Backup, next).ValidateSchedule(); err != nil {
				return fmt.Errorf("%w: %w", store.ErrInvalidPhotoAsset, err)
			}
			return nil
		}
		var out *photoSettingsOutput
		err = g.mutate(func() error {
			settings, callErr := d.Store.SetPhotoSettings(ctx, revision, update)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			if effectiveBackupConfig(d.Cfg.Backup, settings).Interval.Std() > 0 {
				if err := startBackupSchedule(d); err != nil {
					return err
				}
			}
			out = &photoSettingsOutput{ETag: revisionETag(settings.Revision), Body: fromStorePhotoSettings(settings)}
			return nil
		})
		return out, err
	})
	// A Cull run can hold 250,000 photos. The body limit applies before
	// parsing; the response stays below it because each submitted ID appears once.
	huma.Register(api, huma.Operation{
		OperationID: "groupPhotoStacks", Method: http.MethodPost, Path: "/api/v1/photos/stacks/group",
		Summary: "Group explicit photos into bursts", MaxBodyBytes: 16 << 20,
	}, func(ctx context.Context, in *struct{ Body GroupPhotoStacksRequest }) (*struct{ Body GroupPhotoStacksResponse }, error) {
		groups, unavailable, gap, incomplete, err := d.Store.PhotoStackGroups(ctx, in.Body.AssetIDs, in.Body.Hidden, processing.SourceMetadataExtractorFingerprint)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body GroupPhotoStacksResponse }{GroupPhotoStacksResponse{GroupingIncomplete: incomplete, Groups: groups, UnavailableIDs: unavailable, BurstGapSeconds: gap}}, nil
	})
}

// update validates request intent and preserves the legacy empty-body reset.
func (in SetPhotoSettingsRequest) update(ctx context.Context, ifMatch string) (int64, store.PhotoSettingsUpdate, error) {
	update := store.PhotoSettingsUpdate{Preference: in.Preference, ResetPreference: in.ResetPreference, BurstGapSeconds: in.BurstGapSeconds, BackupIntervalSeconds: in.BackupIntervalSeconds, ResetBackupInterval: in.ResetBackupInterval, BackupKeepLast: in.BackupKeepLast, ResetBackupKeepLast: in.ResetBackupKeepLast, HiddenUnlockLifetimeSeconds: in.HiddenUnlockLifetimeSeconds, ResetHiddenUnlockLifetime: in.ResetHiddenUnlockLifetime}
	if in.preferenceNull || in.Preference == nil && in.BurstGapSeconds == nil && in.BackupIntervalSeconds == nil && in.BackupKeepLast == nil && in.HiddenUnlockLifetimeSeconds == nil && !in.ResetBackupInterval && !in.ResetBackupKeepLast && !in.ResetHiddenUnlockLifetime {
		update.ResetPreference = true
	}
	overrides := in.BackupIntervalSeconds != nil || in.BackupKeepLast != nil || in.HiddenUnlockLifetimeSeconds != nil || in.ResetBackupInterval || in.ResetBackupKeepLast || in.ResetHiddenUnlockLifetime

	revision, err := parseIfMatch(ifMatch)
	if err != nil {
		return 0, update, err
	}
	if in.runtimeNull {
		return 0, update, huma.Error422UnprocessableEntity("Use reset flags to reset runtime overrides")
	}
	if in.ResetPreference && in.Preference != nil {
		return 0, update, huma.Error422UnprocessableEntity("Set preference or reset_preference, rather than both")
	}
	if in.preferenceNull && (in.BurstGapSeconds != nil || overrides) {
		return 0, update, huma.Error422UnprocessableEntity("Omit preference to keep it, or use reset_preference to reset it with another setting")
	}
	if browserSessionRequest(ctx) && (update.Preference != nil || update.ResetPreference || overrides) {
		return 0, update, huma.Error403Forbidden("Changing the display preference or runtime overrides requires the API key")
	}
	return revision, update, nil
}
