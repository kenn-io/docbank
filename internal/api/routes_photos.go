package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

func registerPhotoRoutes(api huma.API, d Deps, g *gate) {
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
			out = &photoAssetOutput{ETag: photoETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
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
		return &photoAssetOutput{ETag: photoETag(asset.Revision), Body: fromStorePhotoAsset(asset)}, nil
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
		return &photoAssetOutput{ETag: photoETag(asset.Revision), Body: fromStorePhotoAsset(asset)}, nil
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
			out = &photoAssetOutput{ETag: photoETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
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
			out = &photoAssetOutput{ETag: photoETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
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
			out = &photoAssetOutput{ETag: photoETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
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
			out = &photoAssetOutput{ETag: photoETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
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
			out = &photoAssetOutput{ETag: photoETag(asset.Revision), Body: fromStorePhotoAsset(asset)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "getPhotoSettings", Method: http.MethodGet,
		Path: "/api/v1/photos/settings", Summary: "Inspect the photo display preference",
	}, func(ctx context.Context, _ *struct{}) (*photoSettingsOutput, error) {
		settings, err := d.Store.PhotoSettings(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &photoSettingsOutput{ETag: photoETag(settings.Revision), Body: fromStorePhotoSettings(settings)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "setPhotoSettings", Method: http.MethodPut,
		Path: "/api/v1/photos/settings", Summary: "Set or reset the photo display preference",
	}, func(ctx context.Context, in *struct {
		IfMatch string `header:"If-Match"`
		Body    SetPhotoSettingsRequest
	}) (*photoSettingsOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *photoSettingsOutput
		err = g.mutate(func() error {
			settings, callErr := d.Store.SetPhotoSettings(ctx, revision, in.Body.Preference)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &photoSettingsOutput{ETag: photoETag(settings.Revision), Body: fromStorePhotoSettings(settings)}
			return nil
		})
		return out, err
	})
}
