package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// registerPhotoOwnerRoutes enrolls people as photo owners. Renaming and
// retiring stay on the person routes; all three routes are master-only.
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
		OperationID: "enrollPhotoOwner", Method: http.MethodPost,
		Path: "/api/v1/photos/owners", Summary: "Enroll a person as a photo owner",
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct {
		IfMatch string `header:"If-Match" doc:"The person's current revision"`
		Body    EnrollPhotoOwnerRequest
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
			owner, err = d.Store.EnrollPhotoOwner(ctx, in.Body.PersonID, revision)
			return FromStoreError(err)
		})
		if err != nil {
			return nil, err
		}
		return &photoOwnerOutput{Body: fromStorePhotoOwner(owner)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "removePhotoOwner", Method: http.MethodDelete,
		Path: "/api/v1/photos/owners/{person_id}", Summary: "Remove a photo owner enrollment",
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id"`
		IfMatch  string `header:"If-Match" doc:"The person's current revision"`
	}) (*struct{}, error) {
		if browserSessionRequest(ctx) {
			return nil, FromStoreError(store.ErrNotFound)
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		return nil, g.mutate(func() error {
			return FromStoreError(d.Store.RemovePhotoOwner(ctx, in.PersonID, revision))
		})
	})
}
