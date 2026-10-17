package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

func registerPhotoSuggestionRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "listPhotoSuggestions", Method: http.MethodGet, Path: "/api/v1/photos/assets/{asset_id}/suggestions", Summary: "List proposals for the current display photo"}, func(ctx context.Context, in *struct {
		AssetID string `path:"asset_id" format:"uuid"`
	}) (*struct{ Body []store.PhotoSuggestion }, error) {
		result, err := d.Store.PhotoSuggestions(ctx, in.AssetID)
		return &struct{ Body []store.PhotoSuggestion }{result}, FromStoreError(err)
	})
	huma.Register(api, huma.Operation{OperationID: "respondPhotoSuggestion", Method: http.MethodPost, Path: "/api/v1/photos/suggestions/{suggestion_id}/response", Summary: "Accept selected fields or dismiss a photo proposal"}, func(ctx context.Context, in *struct {
		SuggestionID string `path:"suggestion_id" format:"uuid"`
		Body         store.PhotoSuggestionResponse
	}) (*struct{ Body store.PhotoSuggestion }, error) {
		var result store.PhotoSuggestion
		err := g.mutate(func() error {
			var err error
			result, err = d.Store.RespondPhotoSuggestion(ctx, in.SuggestionID, in.Body)
			if errors.Is(err, store.ErrPhotoSuggestionConflict) {
				return huma.Error409Conflict(err.Error())
			}
			return FromStoreError(err)
		})
		return &struct{ Body store.PhotoSuggestion }{result}, err
	})
}
