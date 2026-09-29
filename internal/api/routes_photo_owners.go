package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

func registerPhotoOwnerRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "listPhotoOwners", Method: http.MethodGet,
		Path: "/api/v1/photos/owners", Summary: "List photo owners",
	}, func(ctx context.Context, _ *struct{}) (*photoOwnersOutput, error) {
		if browserSessionRequest(ctx) {
			return nil, FromStoreError(store.ErrNotFound)
		}
		owners, err := d.Store.PhotoOwners(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		result := make([]PhotoOwner, 0, len(owners))
		for _, owner := range owners {
			result = append(result, fromStorePhotoOwner(owner))
		}
		return &photoOwnersOutput{Body: result}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "createPhotoOwner", Method: http.MethodPost,
		Path: "/api/v1/photos/owners", Summary: "Create a photo owner",
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct{ Body PhotoOwnerRequest }) (*photoOwnerOutput, error) {
		if browserSessionRequest(ctx) {
			return nil, FromStoreError(store.ErrNotFound)
		}
		var owner store.PhotoOwner
		err := g.mutate(func() error {
			var err error
			owner, err = d.Store.CreatePhotoOwner(ctx, in.Body.Name)
			return FromStoreError(err)
		})
		if err != nil {
			return nil, err
		}
		return &photoOwnerOutput{ETag: revisionETag(owner.Revision), Body: fromStorePhotoOwner(owner)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "renamePhotoOwner", Method: http.MethodPatch,
		Path: "/api/v1/photos/owners/{owner_id}", Summary: "Rename a photo owner",
	}, func(ctx context.Context, in *struct {
		OwnerID string `path:"owner_id"`
		IfMatch string `header:"If-Match"`
		Body    PhotoOwnerRequest
	}) (*photoOwnerOutput, error) {
		if browserSessionRequest(ctx) {
			return nil, FromStoreError(store.ErrNotFound)
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var owner store.PhotoOwner
		err = g.mutate(func() error {
			var err error
			owner, err = d.Store.RenamePhotoOwner(ctx, in.OwnerID, revision, in.Body.Name)
			return FromStoreError(err)
		})
		if err != nil {
			return nil, err
		}
		return &photoOwnerOutput{ETag: revisionETag(owner.Revision), Body: fromStorePhotoOwner(owner)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "removePhotoOwner", Method: http.MethodDelete,
		Path: "/api/v1/photos/owners/{owner_id}", Summary: "Remove a photo owner",
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		OwnerID string `path:"owner_id"`
		IfMatch string `header:"If-Match"`
	}) (*struct{}, error) {
		if browserSessionRequest(ctx) {
			return nil, FromStoreError(store.ErrNotFound)
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		err = g.mutate(func() error {
			return FromStoreError(d.Store.RemovePhotoOwner(ctx, in.OwnerID, revision))
		})
		return nil, err
	})
}
